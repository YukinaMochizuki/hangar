package apiserver

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/session"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/sshws"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// streamingNode is the scripted node, which also follows a log, writing one
// record and then nothing until the call ends, and relays an SSH
// connection to a guest that echoes what it is sent.
type streamingNode struct {
	*scriptedNode
}

func (streamingNode) Logs(ctx context.Context, _ *connect.Request[nodev1.LogsRequest], stream *connect.ServerStream[nodev1.LogsResponse]) error {
	if err := stream.Send(&nodev1.LogsResponse{Records: []*nodev1.LogRecord{{
		Stream: nodev1.LogStream_LOG_STREAM_STDOUT, Content: &nodev1.LogRecord_Data{Data: []byte("started")},
	}}}); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func (streamingNode) SSH(_ context.Context, stream *connect.BidiStream[nodev1.SSHRequest, nodev1.SSHResponse]) error {
	if _, err := stream.Receive(); err != nil { // the machine's uid
		return err
	}
	for {
		req, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&nodev1.SSHResponse{Data: req.GetData()}); err != nil {
			return err
		}
	}
}

// countingCredentials counts what is asked of the credentials store, so a
// test can tell a recheck has asked, and what it did not ask.
type countingCredentials struct {
	store.Credentials
	gets, touches, familyHolds atomic.Int64
}

func (c *countingCredentials) GetCredential(ctx context.Context, hash string) (store.Credential, error) {
	c.gets.Add(1)
	return c.Credentials.GetCredential(ctx, hash)
}

func (c *countingCredentials) TouchCredential(ctx context.Context, hash string, lastUsed, idleUntil time.Time) error {
	c.touches.Add(1)
	return c.Credentials.TouchCredential(ctx, hash, lastUsed, idleUntil)
}

func (c *countingCredentials) FamilyHolds(ctx context.Context, family, typ string, now time.Time) (bool, error) {
	c.familyHolds.Add(1)
	return c.Credentials.FamilyHolds(ctx, family, typ, now)
}

// bob is the member the stream tests open streams as, a project.editor in
// web by the binding bob-web.
var bob = auth.Principal{Type: auth.User, ID: "bob@example.com"}

// streamWorld is an execWorld, with alice's container api, that also runs
// her virtual machine web on n1. Its listener takes dev users, browser
// sessions, CLI access tokens and service tokens; its pipe serves the same
// over connections that hold nothing.
type streamWorld struct {
	*execWorld
	clock  *clockwork.FakeClock
	creds  *countingCredentials
	system context.Context
	user   context.Context
	url    string
	pipe   *pipeServer
}

func newStreamWorld(t *testing.T) *streamWorld {
	t.Helper()
	creds := &countingCredentials{}
	w := &streamWorld{creds: creds, execWorld: newExecWorld(t, func(c *Config) {
		creds.Credentials = c.Store.(store.Credentials)
		c.Credentials = creds
	})}
	s := w.s
	w.clock = s.clock.(*clockwork.FakeClock)
	w.system = auth.WithPrincipal(context.Background(), auth.SystemPrincipal)
	w.user = auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: "alice@example.com"})
	if _, err := s.backend(registry.Image).Apply(w.system, image("noble")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.VirtualMachine).Apply(w.user, vmFromImage("web", "noble")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Disk).Apply(w.user, disk("web-root", "20Gi")); err != nil {
		t.Fatal(err)
	}
	m := machine(vmMachineName("web"), "n1")
	m.Spec.Boot = &computev1alpha1.MachineSpec_Firmware{Firmware: &computev1alpha1.FirmwareBoot{RootDisk: "web-root"}}
	m.Metadata.OwnerReferences = []*metav1.OwnerReference{{
		ApiVersion: registry.VirtualMachine.APIVersion(), Kind: registry.VirtualMachine.Kind, Name: "web",
		Uid: getVM(t, s, w.system, "web").GetMetadata().GetUid(), Controller: true,
	}}
	applied, err := s.backend(registry.Machine).Apply(w.system, m)
	if err != nil {
		t.Fatal(err)
	}
	reportMachine(t, s, w.system, applied.Object.(*computev1alpha1.Machine), computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, 1)
	if _, err := s.backend(registry.RoleBinding).Apply(w.system, binding("bob-web", bob.ID, "project.editor",
		&iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Project{Project: "web"}})); err != nil {
		t.Fatal(err)
	}
	w.connectNodeServing(t, streamingNode{w.node})

	srv := httptest.NewUnstartedServer(nil)
	w.url = "http://" + srv.Listener.Addr().String()
	chain := auth.Chain{auth.Dev{}, session.Cookie{M: s.sessions, Origin: w.url}, session.Bearer{M: s.sessions}}
	srv.Config.Handler = s.Handler(chain)
	srv.Start()
	t.Cleanup(srv.Close)
	w.pipe = newPipeServer(t, s.Handler(chain))
	return w
}

// pipeServer serves a handler over in-memory pipes, which hold nothing: a
// write blocks until the other end reads it. frames gets a value each time
// the server begins writing a binary WebSocket frame, so a test knows the
// server is blocked on a client that reads nothing.
type pipeServer struct {
	conns  chan net.Conn
	done   chan struct{}
	once   sync.Once
	frames chan struct{}
}

func newPipeServer(t *testing.T, h http.Handler) *pipeServer {
	p := &pipeServer{conns: make(chan net.Conn), done: make(chan struct{}), frames: make(chan struct{}, 100)}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(p) }()
	t.Cleanup(func() { _ = srv.Close() })
	return p
}

func (p *pipeServer) Accept() (net.Conn, error) {
	select {
	case c := <-p.conns:
		return c, nil
	case <-p.done:
		return nil, net.ErrClosed
	}
}

func (p *pipeServer) Close() error {
	p.once.Do(func() { close(p.done) })
	return nil
}

func (p *pipeServer) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

func (p *pipeServer) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case p.conns <- &frameSignal{Conn: server, frames: p.frames}:
		return client, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.done:
		return nil, net.ErrClosed
	}
}

// frameSignal is a server's end of a pipe that tells when a binary
// WebSocket frame, final and unmasked, as a server sends it, begins.
type frameSignal struct {
	net.Conn
	frames chan struct{}
}

func (f *frameSignal) Write(b []byte) (int, error) {
	if len(b) > 0 && b[0] == 0x82 {
		select {
		case f.frames <- struct{}{}:
		default:
		}
	}
	return f.Conn.Write(b)
}

// dial opens the WebSocket at path with subprotocol and the credential
// header h, over the listener or, with pipe, over the pipe server.
func (w *streamWorld) dial(t *testing.T, h http.Header, path, subprotocol string, pipe bool) *websocket.Conn {
	t.Helper()
	opts := &websocket.DialOptions{HTTPHeader: h, Subprotocols: []string{subprotocol}}
	base := w.url
	if pipe {
		base = "http://pipe"
		opts.HTTPClient = &http.Client{Transport: &http.Transport{DialContext: w.pipe.dial}}
	}
	c, resp, err := websocket.Dial(t.Context(), base+path, opts)
	if err != nil {
		if resp != nil {
			err = execws.ReadError(resp)
			_ = resp.Body.Close()
		}
		t.Fatalf("opening %s: %v", path, err)
	}
	c.SetReadLimit(execws.MaxMessage)
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

// streams are an exec in api, a log of api followed and an SSH tunnel to
// web, all open, as one caller.
type streams struct {
	exec, ssh *websocket.Conn
	logs      <-chan error
}

// open opens the streams with the credential header h, and waits until
// each has begun.
func (w *streamWorld) open(t *testing.T, h http.Header) *streams {
	t.Helper()
	st := &streams{}
	st.exec = w.dial(t, h, execws.Path("web", "containers", "api"), execws.Subprotocol, false)
	send(t, st.exec, startReq(&computev1alpha1.MachineExecStart{Command: []string{"cat"}, Stdin: true}))
	testutil.Eventually(t, "the exec started", func() bool {
		select {
		case <-w.node.starts:
			return true
		default:
			return false
		}
	})
	st.ssh = w.dial(t, h, sshws.Path("web", "virtualmachines", "web"), sshws.Subprotocol, false)

	logsCtx, stopLogs := context.WithCancel(t.Context())
	t.Cleanup(stopLogs)
	client := computev1alpha1connect.NewMachineCapabilityServiceClient(http.DefaultClient, w.url,
		connect.WithInterceptors(streamHeaders(h)))
	logs, err := client.Logs(logsCtx, connect.NewRequest(&computev1alpha1.LogsRequest{
		Project: "web", Kind: registry.Container.Kind, Name: "api", Follow: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !logs.Receive() {
		t.Fatalf("the log did not begin: %v", logs.Err())
	}
	ended := make(chan error, 1)
	go func() {
		for logs.Receive() {
		}
		ended <- logs.Err()
	}()
	st.logs = ended
	st.alive(t, "just opened")
	return st
}

// alive checks that the exec and the SSH tunnel still relay, and the log is
// still followed.
func (st *streams) alive(t *testing.T, when string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testutil.Wait)
	defer cancel()
	send(t, st.exec, &computev1alpha1.MachineExecRequest{Message: &computev1alpha1.MachineExecRequest_Stdin{Stdin: []byte("ping")}})
	_, b, err := st.exec.Read(ctx)
	res := &computev1alpha1.MachineExecResponse{}
	if err == nil {
		err = proto.Unmarshal(b, res)
	}
	if err != nil || string(res.GetStdout()) != "ping" {
		t.Fatalf("the exec, %s, did not relay: %v, %v", when, res, execws.ErrorOf(err))
	}
	if err := st.ssh.Write(ctx, websocket.MessageBinary, []byte("SSH-2.0-test")); err != nil {
		t.Fatal(err)
	}
	if _, b, err := st.ssh.Read(ctx); err != nil || string(b) != "SSH-2.0-test" {
		t.Fatalf("the SSH tunnel, %s, did not relay: %q, %v", when, b, execws.ErrorOf(err))
	}
	select {
	case err := <-st.logs:
		t.Fatalf("the log followed, %s, ended: %v", when, err)
	default:
	}
}

// endWith checks that every stream ended with code, as its client reads it.
func (st *streams) endWith(t *testing.T, code connect.Code, why string) {
	t.Helper()
	wantCodeOf(t, readAll(t, st.exec).err, code, "the exec, after "+why)
	ctx, cancel := context.WithTimeout(t.Context(), testutil.Wait)
	defer cancel()
	for {
		_, _, err := st.ssh.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatalf("the SSH tunnel still open after %s", why)
			}
			wantCodeOf(t, execws.ErrorOf(err), code, "the SSH tunnel, after "+why)
			break
		}
	}
	var err error
	testutil.Eventually(t, "the log followed ended, after "+why, func() bool {
		select {
		case err = <-st.logs:
			return true
		default:
			return false
		}
	})
	wantCodeOf(t, err, code, "the log followed, after "+why)
}

// streamHeaders sets h on every call.
type streamHeaders http.Header

func (h streamHeaders) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc { return next }

func (h streamHeaders) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		for k, v := range h {
			conn.RequestHeader()[k] = v
		}
		return conn
	}
}

func (h streamHeaders) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// asBob, bobsBrowser, bobsCLI and serviceToken are the credentials the
// stream tests open streams with: bob through dev auth, a browser session
// and a CLI login of his, and a service token of web, an editor there.
func (w *streamWorld) asBob() http.Header { return http.Header{auth.DevUserHeader: {bob.ID}} }

func (w *streamWorld) bobsBrowser(t *testing.T) http.Header {
	t.Helper()
	b, err := w.s.sessions.StartBrowser(t.Context(), bob)
	if err != nil {
		t.Fatal(err)
	}
	return http.Header{"Cookie": {session.CookieName + "=" + b.Secret.Reveal()}, "Origin": {w.url}}
}

func (w *streamWorld) bobsCLI(t *testing.T) http.Header {
	t.Helper()
	tokens, err := w.s.sessions.StartCLI(t.Context(), bob)
	if err != nil {
		t.Fatal(err)
	}
	return http.Header{"Authorization": {"Bearer " + tokens.Access.Reveal()}}
}

func (w *streamWorld) serviceToken(t *testing.T) http.Header {
	t.Helper()
	res, err := (tokenIssuer{w.s}).CreateServiceToken(w.user, connect.NewRequest(&iamv1alpha1.CreateServiceTokenRequest{
		Project: "web", Name: "deployer", Role: "project.editor",
	}))
	if err != nil {
		t.Fatal(err)
	}
	return http.Header{"Authorization": {"Bearer " + res.Msg.GetSecret()}}
}

// TestMachineStreamsEndWithWhatLetThemIn: an exec, an SSH tunnel and a log
// followed end, at the next recheck, once what let them in is taken away:
// the credential they were opened with, the binding that granted them, or
// their caller's membership; the node's exec ends with them.
func TestMachineStreamsEndWithWhatLetThemIn(t *testing.T) {
	for _, tt := range []struct {
		name   string
		open   func(*streamWorld, *testing.T) http.Header
		revoke func(*streamWorld, *testing.T) error
		code   connect.Code
	}{
		{"the binding deleted", (*streamWorld).bobHeader, func(w *streamWorld, _ *testing.T) error {
			_, err := w.s.backend(registry.RoleBinding).Delete(w.system, resource.Key{Name: "bob-web"}, nil)
			return err
		}, connect.CodePermissionDenied},
		{"membership taken away", (*streamWorld).bobHeader, func(w *streamWorld, _ *testing.T) error {
			_, err := w.s.backend(registry.RoleBinding).Delete(w.system, resource.Key{Name: "member-" + usernameOf(bob.ID)}, nil)
			return err
		}, connect.CodePermissionDenied},
		{"the browser session revoked", (*streamWorld).bobsBrowser, revokeBob, connect.CodeUnauthenticated},
		{"the CLI login revoked", (*streamWorld).bobsCLI, revokeBob, connect.CodeUnauthenticated},
		{"the service token deleted", (*streamWorld).serviceToken, func(w *streamWorld, _ *testing.T) error {
			_, err := w.s.backend(registry.ServiceToken).Delete(w.user, resource.Key{Project: "web", Name: "deployer"}, nil)
			return err
		}, connect.CodeUnauthenticated},
		{"the service token rotated", (*streamWorld).serviceToken, func(w *streamWorld, _ *testing.T) error {
			_, err := (tokenIssuer{w.s}).RotateServiceToken(w.user, connect.NewRequest(&iamv1alpha1.RotateServiceTokenRequest{Project: "web", Name: "deployer"}))
			return err
		}, connect.CodeUnauthenticated},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newStreamWorld(t)
			st := w.open(t, tt.open(w, t))
			if err := tt.revoke(w, t); err != nil {
				t.Fatal(err)
			}
			w.clock.Advance(auth.RecheckInterval)
			st.endWith(t, tt.code, tt.name)
			if err := w.node.waitEnded(t); err == nil {
				t.Error("the node's exec went on")
			}
		})
	}
}

func (w *streamWorld) bobHeader(*testing.T) http.Header { return w.asBob() }

func revokeBob(w *streamWorld, t *testing.T) error {
	_, err := w.s.sessions.RevokePrincipal(t.Context(), bob)
	return err
}

// TestAnOpenStreamIsNoUseOfItsSession: rechecking streams opened with a
// browser session records no use of it, so they end at its idle limit,
// as a tab left open does, unless the person uses the session otherwise.
func TestAnOpenStreamIsNoUseOfItsSession(t *testing.T) {
	w := newStreamWorld(t)
	st := w.open(t, w.bobsBrowser(t))
	gets, touches := w.creds.gets.Load(), w.creds.touches.Load()
	w.clock.Advance(session.SessionIdle - time.Minute)
	// A round has read the session for each of the three streams.
	testutil.Eventually(t, "the streams rechecked", func() bool { return w.creds.gets.Load() >= gets+3 })
	if n := w.creds.touches.Load(); n != touches {
		t.Errorf("rechecks recorded %d uses of the session", n-touches)
	}
	st.alive(t, "within the session's idle limit")
	w.clock.Advance(2 * time.Minute)
	st.endWith(t, connect.CodeUnauthenticated, "the session went idle")
}

// TestAStreamOutlivesItsAccessToken: streams opened with a CLI access token
// are rechecked by the login it was issued in, so they outlive the token's
// hour, which the CLI renews without reopening them, and end with the login.
func TestAStreamOutlivesItsAccessToken(t *testing.T) {
	w := newStreamWorld(t)
	st := w.open(t, w.bobsCLI(t))
	asked := w.creds.familyHolds.Load()
	w.clock.Advance(session.AccessTTL + time.Minute)
	testutil.Eventually(t, "the streams' logins rechecked", func() bool { return w.creds.familyHolds.Load() >= asked+3 })
	st.alive(t, "past the access token's hour")
	if err := revokeBob(w, t); err != nil {
		t.Fatal(err)
	}
	w.clock.Advance(auth.RecheckInterval)
	st.endWith(t, connect.CodeUnauthenticated, "the login was revoked")
}

// TestARevokedStreamEndsThoughItsClientStoppedReading: an exec or an SSH
// tunnel blocked writing to a client that reads nothing, and so in no read
// or write that would see its end, still ends at the recheck that refuses
// its caller.
func TestARevokedStreamEndsThoughItsClientStoppedReading(t *testing.T) {
	for _, tt := range []struct {
		kind, path, subprotocol, ended string
		begin                          func(*testing.T, *websocket.Conn)
	}{
		{"exec", execws.Path("web", "containers", "api"), execws.Subprotocol, "exec ended", func(t *testing.T, c *websocket.Conn) {
			send(t, c, startReq(&computev1alpha1.MachineExecStart{Command: []string{"cat"}, Stdin: true}))
			send(t, c, &computev1alpha1.MachineExecRequest{Message: &computev1alpha1.MachineExecRequest_Stdin{Stdin: []byte("ping")}})
		}},
		{"ssh", sshws.Path("web", "virtualmachines", "web"), sshws.Subprotocol, "ssh closed", func(t *testing.T, c *websocket.Conn) {
			if err := c.Write(t.Context(), websocket.MessageBinary, []byte("SSH-2.0-test")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			w := newStreamWorld(t)
			c := w.dial(t, w.asBob(), tt.path, tt.subprotocol, true)
			tt.begin(t, c)
			// The echo the apiserver writes back blocks on the pipe, which
			// the client never reads.
			testutil.Eventually(t, "the apiserver writing to the client", func() bool {
				select {
				case <-w.pipe.frames:
					return true
				default:
					return false
				}
			})
			if _, err := w.s.backend(registry.RoleBinding).Delete(w.system, resource.Key{Name: "bob-web"}, nil); err != nil {
				t.Fatal(err)
			}
			w.clock.Advance(auth.RecheckInterval)
			// testutil.Eventually's bound covers the 5 s coder/websocket gives
			// the close frame to a client that reads nothing.
			testutil.Eventually(t, "the "+tt.kind+" ended", func() bool { return len(w.auditLines(tt.ended)) > 0 })
			wantFields(t, w.auditLines(tt.ended)[0], map[string]any{"principal": bob.String(), "code": "permission_denied"})
		})
	}
}
