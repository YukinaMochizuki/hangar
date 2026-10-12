package apiserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1/nodev1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/tunnel"
)

// scriptedNode serves NodeService.Exec as a node would, running scripted
// programs named by the first argument: echo prints its arguments, fail
// fails the call as a VM that is not running does, flood writes output
// until it is stopped, and anything else echoes stdin, ends with stdin's
// close or a signal, and records resizes.
type scriptedNode struct {
	nodev1connect.UnimplementedNodeServiceHandler
	starts  chan *nodev1.ExecStart
	resizes chan *nodev1.WindowSize
	// ended gets the context error of each call as it ends.
	ended   chan error
	flooded atomic.Int64
}

func newScriptedNode() *scriptedNode {
	return &scriptedNode{starts: make(chan *nodev1.ExecStart, 100), resizes: make(chan *nodev1.WindowSize, 100), ended: make(chan error, 100)}
}

func (n *scriptedNode) Exec(ctx context.Context, s *connect.BidiStream[nodev1.ExecRequest, nodev1.ExecResponse]) error {
	defer func() { n.ended <- ctx.Err() }()
	first, err := s.Receive()
	if err != nil {
		return err
	}
	start := first.GetStart()
	n.starts <- start
	exit := func(code, sig int32) error {
		if err := s.Send(&nodev1.ExecResponse{Message: &nodev1.ExecResponse_Exit{Exit: &nodev1.ExitStatus{Code: code, Signal: sig}}}); err != nil {
			return err
		}
		drain := func() {
			for {
				if _, err := s.Receive(); err != nil {
					return
				}
			}
		}
		drain()
		return nil
	}
	switch start.GetArgs()[0] {
	case "echo":
		if err := s.Send(&nodev1.ExecResponse{Message: &nodev1.ExecResponse_Stdout{Stdout: []byte(strings.Join(start.GetArgs()[1:], " "))}}); err != nil {
			return err
		}
		if err := s.Send(&nodev1.ExecResponse{Message: &nodev1.ExecResponse_Stderr{Stderr: []byte("to stderr")}}); err != nil {
			return err
		}
		return exit(3, 0)
	case "fail":
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("machine m1's VM is not running"))
	case "flood":
		// Random, as compression on the tunnel would make anything else
		// small on the wire.
		chunk := make([]byte, 64<<10)
		_, _ = rand.Read(chunk)
		for {
			if err := s.Send(&nodev1.ExecResponse{Message: &nodev1.ExecResponse_Stdout{Stdout: chunk}}); err != nil {
				return err
			}
			n.flooded.Add(int64(len(chunk)))
		}
	}
	for {
		req, err := s.Receive()
		if err != nil {
			return err
		}
		switch m := req.GetMessage().(type) {
		case *nodev1.ExecRequest_Stdin:
			if err := s.Send(&nodev1.ExecResponse{Message: &nodev1.ExecResponse_Stdout{Stdout: m.Stdin}}); err != nil {
				return err
			}
		case *nodev1.ExecRequest_StdinClose:
			return exit(0, 0)
		case *nodev1.ExecRequest_Signal:
			return exit(128+m.Signal, m.Signal)
		case *nodev1.ExecRequest_Resize:
			n.resizes <- m.Resize
		}
	}
}

// execWorld is an apiserver with alice's container api running on node
// n1, whose agent is node.
type execWorld struct {
	s    *Server
	url  string
	node *scriptedNode
	log  *syncBuffer
	// uid is the machine's.
	uid string
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// readerRole reads containers and their logs, and nothing more.
const readerRole = "test.reader"

func newExecWorld(t *testing.T, configure func(*Config)) *execWorld {
	t.Helper()
	// Registered before the server runs and removed after it stops.
	authz.Roles[readerRole] = authz.Role{Level: authz.TenantLevel, Permissions: []string{
		authz.Permission(registry.Container, authz.Get), authz.ContainerLogs,
	}}
	t.Cleanup(func() { delete(authz.Roles, readerRole) })
	w := &execWorld{node: newScriptedNode(), log: &syncBuffer{}}
	s, system, user := runningWith(t, func(c *Config) {
		c.Nodes = []string{"n1"}
		c.Logger = slog.New(slog.NewJSONHandler(w.log, nil))
		if configure != nil {
			configure(c)
		}
	})
	w.s = s
	for _, rb := range []*iamv1alpha1.RoleBinding{
		binding("member-bob", "bob@example.com", "platform.member", &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}}),
		binding("member-carol", "carol@example.com", "platform.member", &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}}),
		binding("reader-carol", "carol@example.com", readerRole, &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: "acme"}}),
	} {
		if _, err := s.backend(registry.RoleBinding).Apply(system, rb); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.backend(registry.Container).Apply(user, &computev1alpha1.Container{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: "api"},
		Spec:     &computev1alpha1.ContainerSpec{Image: "nginx"},
	}); err != nil {
		t.Fatal(err)
	}
	c, err := s.backend(registry.Container).Get(user, resource.Key{Project: "web", Name: "api"})
	if err != nil {
		t.Fatal(err)
	}
	m := machine(machineName("api"), "n1")
	m.Metadata.OwnerReferences = []*metav1.OwnerReference{{
		ApiVersion: "compute.hangar.dev/v1alpha1", Kind: "Container", Name: "api",
		Uid: c.(*computev1alpha1.Container).GetMetadata().GetUid(), Controller: true,
	}}
	applied, err := s.backend(registry.Machine).Apply(system, m)
	if err != nil {
		t.Fatal(err)
	}
	w.uid = applied.Object.(*computev1alpha1.Machine).GetMetadata().GetUid()
	reportMachine(t, s, system, applied.Object.(*computev1alpha1.Machine), computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, 1)

	srv := httptest.NewServer(s.Handler(auth.Chain{auth.Dev{}}))
	t.Cleanup(srv.Close)
	w.url = srv.URL
	return w
}

func binding(name, user, role string, scope *iamv1alpha1.BindingScope) *iamv1alpha1.RoleBinding {
	return &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: user}},
			Role:    role, Scope: scope,
		},
	}
}

// connectNode opens n1's tunnel, serving the scripted node, until the test
// ends.
func (w *execWorld) connectNode(t *testing.T) {
	t.Helper()
	sock := listen(t, w.s, auth.Chain{auth.PeerCred{NodeUIDs: []uint32{uint32(os.Getuid())}, NodeName: "n1"}})
	mux := http.NewServeMux()
	mux.Handle(nodev1connect.NewNodeServiceHandler(w.node))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&tunnel.Agent{Dial: func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}, Handler: mux}).Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := w.s.tunnels.Node("n1"); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("n1's tunnel never opened")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// dial opens an exec as user, failing as the server refuses it.
func (w *execWorld) dial(t *testing.T, user, name string) (*websocket.Conn, error) {
	t.Helper()
	c, resp, err := websocket.Dial(t.Context(), w.url+execws.Path("web", name), &websocket.DialOptions{
		HTTPHeader:   http.Header{auth.DevUserHeader: []string{user}},
		Subprotocols: []string{execws.Subprotocol},
	})
	if err != nil {
		if resp == nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusSwitchingProtocols {
			return nil, execws.ReadError(resp)
		}
		return nil, err
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c, nil
}

func send(t *testing.T, c *websocket.Conn, req *computev1alpha1.ContainerExecRequest) {
	t.Helper()
	b, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(t.Context(), websocket.MessageBinary, b); err != nil {
		t.Fatal(err)
	}
}

func startReq(start *computev1alpha1.ContainerExecStart) *computev1alpha1.ContainerExecRequest {
	return &computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_Start{Start: start}}
}

type wsResult struct {
	stdout, stderr string
	exit           *computev1alpha1.ContainerExecExit
	// err is what the WebSocket closed with: nil for a normal closure.
	err error
}

// readAll reads an exec to its close, within 10 s.
func readAll(t *testing.T, c *websocket.Conn) (r wsResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var out, errb strings.Builder
	defer func() { r.stdout, r.stderr = out.String(), errb.String() }()
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatal("the exec did not end")
			}
			r.err = execws.ErrorOf(err)
			return r
		}
		res := &computev1alpha1.ContainerExecResponse{}
		if err := proto.Unmarshal(b, res); err != nil {
			t.Fatal(err)
		}
		switch m := res.GetMessage().(type) {
		case *computev1alpha1.ContainerExecResponse_Stdout:
			out.Write(m.Stdout)
		case *computev1alpha1.ContainerExecResponse_Stderr:
			errb.Write(m.Stderr)
		case *computev1alpha1.ContainerExecResponse_Exit:
			r.exit = m.Exit
		}
	}
}

func (n *scriptedNode) waitEnded(t *testing.T) error {
	t.Helper()
	select {
	case err := <-n.ended:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the node's call did not end")
		return nil
	}
}

func TestAnExecRelaysAProcess(t *testing.T) {
	w := newExecWorld(t, nil)
	w.connectNode(t)
	c, err := w.dial(t, "alice@example.com", "api")
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, startReq(&computev1alpha1.ContainerExecStart{
		Command: []string{"echo", "hello", "there"}, Env: []*computev1alpha1.EnvVar{{Name: "A", Value: "b=c"}},
		User: "1000", WorkingDir: "/srv", Tty: true, Size: &computev1alpha1.ContainerExecWindowSize{Rows: 24, Cols: 80},
	}))
	r := readAll(t, c)
	if r.err != nil || r.stdout != "hello there" || r.stderr != "to stderr" || r.exit.GetCode() != 3 {
		t.Errorf("%+v", r)
	}
	start := <-w.node.starts
	want := &nodev1.ExecStart{
		MachineUid: w.uid, Args: []string{"echo", "hello", "there"}, Env: []string{"A=b=c"},
		User: "1000", WorkingDir: "/srv", Tty: true, Size: &nodev1.WindowSize{Rows: 24, Cols: 80},
	}
	if !proto.Equal(start, want) {
		t.Errorf("the node was asked %v, want %v", start, want)
	}
	// Ended, by the close of this side or the reset that follows it.
	_ = w.node.waitEnded(t)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(w.log.String(), `"msg":"exec"`) {
		if time.Now().After(deadline) {
			t.Fatalf("no audit line in %s", w.log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, want := range []string{
		`"principal":"user:alice@example.com"`, `"project":"web"`, `"container":"api"`, `"machine":"` + w.uid + `"`,
		`"argv":["echo","hello","there"]`, `"tty":true`, `"exit_code":3`, `"duration":`,
	} {
		if !strings.Contains(w.log.String(), want) {
			t.Errorf("the audit line has no %s: %s", want, w.log.String())
		}
	}
}

func TestAnExecCarriesStdinResizesAndSignals(t *testing.T) {
	w := newExecWorld(t, nil)
	w.connectNode(t)
	c, err := w.dial(t, "alice@example.com", "api")
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, startReq(&computev1alpha1.ContainerExecStart{Command: []string{"cat"}, Stdin: true}))
	send(t, c, &computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_Stdin{Stdin: []byte("secret input")}})
	send(t, c, &computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_StdinClose{StdinClose: &computev1alpha1.ContainerExecStdinClose{}}})
	if r := readAll(t, c); r.err != nil || r.stdout != "secret input" || r.exit.GetCode() != 0 {
		t.Errorf("stdin: %+v", r)
	}
	if strings.Contains(w.log.String(), "secret") {
		t.Errorf("the log holds stdin: %s", w.log.String())
	}

	c, err = w.dial(t, "alice@example.com", "api")
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, startReq(&computev1alpha1.ContainerExecStart{Command: []string{"cat"}, Tty: true}))
	send(t, c, &computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_Resize{Resize: &computev1alpha1.ContainerExecWindowSize{Rows: 30, Cols: 100}}})
	select {
	case size := <-w.node.resizes:
		if size.GetRows() != 30 || size.GetCols() != 100 {
			t.Errorf("resized to %v", size)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the resize never reached the node")
	}
	send(t, c, &computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_Signal{Signal: 2}})
	if r := readAll(t, c); r.err != nil || r.exit.GetCode() != 130 || r.exit.GetSignal() != 2 {
		t.Errorf("signal: %+v", r)
	}

	// Stdin to an exec started without it ends the exec.
	c, err = w.dial(t, "alice@example.com", "api")
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, startReq(&computev1alpha1.ContainerExecStart{Command: []string{"cat"}}))
	send(t, c, &computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_Stdin{Stdin: []byte("x")}})
	wantCodeOf(t, readAll(t, c).err, connect.CodeInvalidArgument, "stdin without asking for it")
}

// TestAnExecIsAuthorizedBeforeAnythingIsTold: a caller who may not exec
// learns nothing of the container, whether it exists included, and no
// node is called for them.
func TestAnExecIsAuthorizedBeforeAnythingIsTold(t *testing.T) {
	w := newExecWorld(t, nil)
	w.connectNode(t)
	for _, tc := range []struct {
		what, user, name string
		code             connect.Code
	}{
		{"one who may get and read logs, not exec", "carol@example.com", "api", connect.CodePermissionDenied},
		{"another tenant's member", "bob@example.com", "api", connect.CodePermissionDenied},
		{"another tenant's member, of a container that is not there", "bob@example.com", "nope", connect.CodePermissionDenied},
		{"one who may not exec, of a container that is not there", "carol@example.com", "nope", connect.CodePermissionDenied},
		{"no one", "", "api", connect.CodeUnauthenticated},
		{"the owner, of a container that is not there", "alice@example.com", "nope", connect.CodeNotFound},
	} {
		_, err := w.dial(t, tc.user, tc.name)
		wantCodeOf(t, err, tc.code, tc.what)
		if err != nil && (strings.Contains(err.Error(), "n1") || strings.Contains(err.Error(), w.uid)) {
			t.Errorf("%s: told %v", tc.what, err)
		}
	}
	// Carol may read the logs she is denied exec on.
	if _, err := w.s.backend(registry.Container).Get(auth.WithPrincipal(t.Context(), auth.Principal{Type: auth.User, ID: "carol@example.com"}),
		resource.Key{Project: "web", Name: "api"}); err != nil {
		t.Errorf("carol getting the container: %v", err)
	}
	select {
	case start := <-w.node.starts:
		t.Errorf("the node was called: %v", start)
	default:
	}
}

func TestAnExecIsRefusedBeforeTheUpgrade(t *testing.T) {
	w := newExecWorld(t, func(c *Config) { c.MaxNodeCallsPerPrincipal = 1 })
	// No node yet.
	_, err := w.dial(t, "alice@example.com", "api")
	wantCodeOf(t, err, connect.CodeUnavailable, "a node not connected")

	w.connectNode(t)
	c, err := w.dial(t, "alice@example.com", "api")
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, startReq(&computev1alpha1.ContainerExecStart{Command: []string{"cat"}}))
	<-w.node.starts
	_, err = w.dial(t, "alice@example.com", "api")
	wantCodeOf(t, err, connect.CodeResourceExhausted, "a second exec over the cap")
	_ = c.CloseNow()
	_ = w.node.waitEnded(t)

	// Not running.
	system := auth.WithPrincipal(t.Context(), auth.SystemPrincipal)
	m, err := w.s.backend(registry.Machine).Get(system, resource.Key{Project: "web", Name: machineName("api")})
	if err != nil {
		t.Fatal(err)
	}
	reportMachine(t, w.s, system, m.(*computev1alpha1.Machine), computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, 1)
	_, err = w.dial(t, "alice@example.com", "api")
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a container not running")
	if err != nil && !strings.Contains(err.Error(), "not running") {
		t.Errorf("not running: %v", err)
	}

	// No subprotocol.
	reportMachine(t, w.s, system, m.(*computev1alpha1.Machine), computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, 1)
	_, resp, err := websocket.Dial(t.Context(), w.url+execws.Path("web", "api"), &websocket.DialOptions{
		HTTPHeader: http.Header{auth.DevUserHeader: []string{"alice@example.com"}},
	})
	if err == nil || resp == nil {
		t.Fatalf("an exec without the subprotocol: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	wantCodeOf(t, execws.ReadError(resp), connect.CodeInvalidArgument, "an exec without the subprotocol")
}

func TestAnExecFailsAfterTheUpgradeWithItsCode(t *testing.T) {
	w := newExecWorld(t, func(c *Config) { c.ExecStart = 200 * time.Millisecond })
	w.connectNode(t)
	c, err := w.dial(t, "alice@example.com", "api")
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, startReq(&computev1alpha1.ContainerExecStart{Command: []string{"fail"}}))
	r := readAll(t, c)
	wantCodeOf(t, r.err, connect.CodeFailedPrecondition, "the node failing the exec")
	if r.err != nil && !strings.Contains(r.err.Error(), "not running") {
		t.Errorf("the node's failure: %v", r.err)
	}

	c, err = w.dial(t, "alice@example.com", "api")
	if err != nil {
		t.Fatal(err)
	}
	wantCodeOf(t, readAll(t, c).err, connect.CodeInvalidArgument, "no start")

	c, err = w.dial(t, "alice@example.com", "api")
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, startReq(&computev1alpha1.ContainerExecStart{}))
	wantCodeOf(t, readAll(t, c).err, connect.CodeInvalidArgument, "no command")
}

// TestAClientGoingAwayEndsTheNodesCall: the node's call, and with it the
// process, ends with the client, whether it closes, drops, or stops
// answering pings.
func TestAClientGoingAwayEndsTheNodesCall(t *testing.T) {
	w := newExecWorld(t, func(c *Config) { c.ExecPing, c.ExecPong = 50*time.Millisecond, 100*time.Millisecond })
	w.connectNode(t)
	for _, end := range []func(c *websocket.Conn){
		func(c *websocket.Conn) { _ = c.CloseNow() },
		func(c *websocket.Conn) { _ = c.Close(websocket.StatusNormalClosure, "") },
		// It never reads, so it never answers a ping.
		func(*websocket.Conn) {},
	} {
		c, err := w.dial(t, "alice@example.com", "api")
		if err != nil {
			t.Fatal(err)
		}
		send(t, c, startReq(&computev1alpha1.ContainerExecStart{Command: []string{"cat"}, Stdin: true}))
		<-w.node.starts
		end(c)
		if err := w.node.waitEnded(t); !errors.Is(err, context.Canceled) {
			t.Errorf("the node's call ended with %v, want cancelled", err)
		}
	}
}

// TestAClientThatAnswersPingsStaysConnected: pings keep an idle exec up.
func TestAClientThatAnswersPingsStaysConnected(t *testing.T) {
	w := newExecWorld(t, func(c *Config) { c.ExecPing, c.ExecPong = 20*time.Millisecond, 200*time.Millisecond })
	w.connectNode(t)
	c, err := w.dial(t, "alice@example.com", "api")
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, startReq(&computev1alpha1.ContainerExecStart{Command: []string{"cat"}, Stdin: true}))
	done := make(chan wsResult, 1)
	go func() { done <- readAll(t, c) }()
	time.Sleep(500 * time.Millisecond) // many pings
	select {
	case r := <-done:
		t.Fatalf("the exec ended while idle: %+v", r)
	case err := <-w.node.ended:
		t.Fatalf("the node's call ended while idle: %v", err)
	default:
	}
	send(t, c, &computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_StdinClose{StdinClose: &computev1alpha1.ContainerExecStdinClose{}}})
	if r := <-done; r.err != nil || r.exit.GetCode() != 0 {
		t.Errorf("%+v", r)
	}
}

// TestAClientThatStopsReadingHoldsUpItsNode: output a client does not read
// is not kept by the apiserver; it holds up the node's call, which stops
// sending.
func TestAClientThatStopsReadingHoldsUpItsNode(t *testing.T) {
	w := newExecWorld(t, nil)
	w.connectNode(t)
	c, err := w.dial(t, "alice@example.com", "api")
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, startReq(&computev1alpha1.ContainerExecStart{Command: []string{"flood"}}))
	var last int64
	for stable := 0; stable < 10; time.Sleep(50 * time.Millisecond) {
		n := w.node.flooded.Load()
		if n == last && n > 0 {
			stable++
		} else {
			stable = 0
		}
		last = n
	}
	// The stream's window, the WebSocket's buffers and the sockets' on
	// loopback; the apiserver holds a message at a time.
	if last > 32<<20 {
		t.Errorf("the node got %d bytes out to a client reading nothing", last)
	}
	_ = c.CloseNow()
	_ = w.node.waitEnded(t)
}

// TestExecsLeaveNothingRunning: execs that end every way leave no
// goroutine in the apiserver.
func TestExecsLeaveNothingRunning(t *testing.T) {
	w := newExecWorld(t, nil)
	w.connectNode(t)
	once := func(i int) {
		c, err := w.dial(t, "alice@example.com", "api")
		if err != nil {
			t.Fatal(err)
		}
		switch i % 3 {
		case 0:
			send(t, c, startReq(&computev1alpha1.ContainerExecStart{Command: []string{"echo"}}))
			readAll(t, c)
		case 1:
			send(t, c, startReq(&computev1alpha1.ContainerExecStart{Command: []string{"cat"}}))
			<-w.node.starts
			_ = c.CloseNow()
		case 2:
			send(t, c, startReq(&computev1alpha1.ContainerExecStart{Command: []string{"fail"}}))
			readAll(t, c)
		}
		_ = w.node.waitEnded(t)
		select {
		case <-w.node.starts:
		default:
		}
	}
	once(0)
	before := runtime.NumGoroutine()
	for i := range 30 {
		once(i)
	}
	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > before+5 {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("%d goroutines after 30 execs, %d before:\n%s", runtime.NumGoroutine(), before, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The last exec's handler may still be closing its WebSocket.
	calls := func() int {
		w.s.nodeCalls.mu.Lock()
		defer w.s.nodeCalls.mu.Unlock()
		return w.s.nodeCalls.perNode["n1"]
	}
	for deadline := time.Now().Add(10 * time.Second); calls() != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d calls to n1 still counted", calls())
		}
	}
}
