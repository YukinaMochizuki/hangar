package apiserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/execws"
)

// Defaults of an exec's timing.
const (
	// DefaultExecPing is how often an exec's WebSocket is pinged: Cloudflare
	// closes one idle for 100 s.
	DefaultExecPing = 30 * time.Second
	// DefaultExecPong is how much longer than DefaultExecPing a client may
	// stay silent, with nothing pending, before it is taken to be gone.
	DefaultExecPong = 30 * time.Second
	// DefaultExecStart is how long a client has to send the start.
	DefaultExecStart = 10 * time.Second
)

// errClientGone ends an exec whose client has gone, or stopped answering.
var errClientGone = connect.NewError(connect.CodeCanceled, errors.New("the client went away"))

// execTarget is what an exec runs in, and the call to its node.
type execTarget struct {
	principal auth.Principal
	key       resource.Key
	machine   string // uid
	call      *nodeCall
}

// containerExec serves a container's exec, a WebSocket on the public
// listener. Everything that can refuse it does so before the upgrade, and
// nothing about the container is told before the caller is authorized.
func (s *Server) containerExec(chain auth.Chain) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, ok := execws.NameOf(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		key := resource.Key{Project: r.PathValue("project"), Name: name}
		refuse := func(p auth.Principal, err error) {
			s.auditRefused(p, key, err)
			execws.WriteError(w, r, err)
		}
		ctx, err := chain.Authenticate(r.Context(), r.Header)
		if err != nil {
			refuse(auth.Principal{}, connect.NewError(connect.CodeUnauthenticated, err))
			return
		}
		p, _ := auth.FromContext(ctx)
		x, err := s.execTarget(ctx, key)
		if err != nil {
			refuse(p, err)
			return
		}
		defer x.call.release()
		if !slices.Contains(subprotocols(r), execws.Subprotocol) {
			refuse(p, invalid("an exec is a WebSocket with the subprotocol %s", execws.Subprotocol))
			return
		}
		timing := s.execTiming()
		live := execws.NewLiveness(timing.ping, timing.pong)
		// The Origin check stays on: see docs/decisions/streaming-transport.md.
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols:   []string{execws.Subprotocol},
			OnPingReceived: live.OnPing, OnPongReceived: live.OnPong,
		})
		if err != nil {
			// Accept answered.
			code := connect.CodeInvalidArgument
			if r.Header.Get("Origin") != "" && strings.Contains(err.Error(), "Origin") {
				code = connect.CodePermissionDenied
			}
			s.auditRefused(p, key, connect.NewError(code, err))
			return
		}
		s.runExec(ctx, c, x, live)
	}
}

func subprotocols(r *http.Request) []string {
	var out []string
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		for p := range strings.SplitSeq(h, ",") {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return out
}

// execTarget authorizes an exec in the container at key and begins the
// call to its node, as ContainerLogs does.
func (s *Server) execTarget(ctx context.Context, key resource.Key) (*execTarget, error) {
	b := s.backend(registry.Container)
	p, err := b.begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	_, obj, err := b.read(ctx, p, authz.Exec, key)
	if err != nil {
		return nil, err
	}
	c := obj.(*computev1alpha1.Container)
	mobj, err := s.get(ctx, registry.Machine, resource.Key{Project: key.Project, Name: machineName(key.Name)})
	if err != nil {
		return nil, apiError(err)
	}
	machine, _ := mobj.(*computev1alpha1.Machine)
	if machine == nil || !ownedBy(machine.GetMetadata(), c.GetMetadata().GetUid()) {
		return nil, failedPrecondition("container %q has no machine yet", key.Name)
	}
	node := machine.GetSpec().GetNode()
	if node == "" {
		return nil, failedPrecondition("container %q is not placed on a node yet", key.Name)
	}
	if phase := machine.GetStatus().GetPhase(); phase != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING {
		return nil, failedPrecondition("container %q is not running: its machine is %s", key.Name,
			strings.ToLower(strings.TrimPrefix(phase.String(), "MACHINE_PHASE_")))
	}
	project, err := s.get(ctx, registry.Project, resource.Key{Name: key.Project})
	if err != nil {
		return nil, apiError(err)
	}
	if project == nil {
		return nil, failedPrecondition("project %q is gone", key.Project)
	}
	call, err := s.callNode(p, project.(*tenancyv1alpha1.Project).GetSpec().GetTenant(), node, fmt.Sprintf("container %q", key.Name))
	if err != nil {
		return nil, err
	}
	return &execTarget{principal: p, key: key, machine: machine.GetMetadata().GetUid(), call: call}, nil
}

// execAudit is what the audit lines of an exec say.
type execAudit struct {
	start *computev1alpha1.ContainerExecStart
	exit  *nodev1.ExitStatus
	began time.Time
}

// runExec runs an exec on an upgraded WebSocket until the process ends,
// the client goes, or the node does, and closes the WebSocket.
func (s *Server) runExec(ctx context.Context, c *websocket.Conn, x *execTarget, live *execws.Liveness) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	c.SetReadLimit(execws.MaxMessage)
	var once sync.Once
	// end ends the exec once, closing the WebSocket with why: what reads
	// or writes it is ended by that, not by ctx.
	end := func(err error) {
		once.Do(func() {
			cancel(err)
			switch {
			case err == nil:
				_ = c.Close(websocket.StatusNormalClosure, "")
			case errors.Is(err, errClientGone):
				_ = c.CloseNow()
			default:
				code, reason := execws.CloseStatus(err)
				_ = c.Close(code, reason)
			}
		})
	}
	audit := &execAudit{began: time.Now()}
	err := s.exec(ctx, end, c, x, live, audit)
	if audit.start == nil {
		s.auditRefused(x.principal, x.key, err)
	} else {
		s.auditEnded(x, audit, err)
	}
	end(err)
}

// auditRefused logs an exec refused before its process started.
func (s *Server) auditRefused(p auth.Principal, key resource.Key, err error) {
	principal := ""
	if p != (auth.Principal{}) {
		principal = p.String()
	}
	s.log.Info("exec refused", "principal", principal, "project", key.Project, "container", key.Name,
		"code", connect.CodeOf(err).String(), "error", err.Error())
}

// auditStarted logs an exec whose process is asked of the node: never its
// input or output, nor its environment's values.
func (s *Server) auditStarted(x *execTarget, start *computev1alpha1.ContainerExecStart) {
	env := make([]string, 0, len(start.GetEnv()))
	for _, e := range start.GetEnv() {
		env = append(env, e.GetName())
	}
	s.log.Info("exec started", "principal", x.principal.String(), "project", x.key.Project, "container", x.key.Name,
		"machine", x.machine, "argv", start.GetCommand(), "tty", start.GetTty(), "stdin", start.GetStdin(),
		"user", start.GetUser(), "workdir", start.GetWorkingDir(), "env_names", env)
}

func (s *Server) auditEnded(x *execTarget, a *execAudit, err error) {
	attrs := []any{
		"principal", x.principal.String(), "project", x.key.Project, "container", x.key.Name, "machine", x.machine,
		"duration", time.Since(a.began).Round(time.Millisecond).String(),
	}
	if a.exit != nil {
		attrs = append(attrs, "exit_code", a.exit.GetCode(), "signal", a.exit.GetSignal())
	}
	if err != nil {
		attrs = append(attrs, "code", connect.CodeOf(err).String(), "error", err.Error())
	}
	s.log.Info("exec ended", attrs...)
}

// exec relays between the client and the node, one message at a time each
// way, until end. It reads and writes the WebSocket with ws, which never
// ends: coder/websocket closes a connection whose I/O is cancelled.
func (s *Server) exec(ctx context.Context, end func(error), c *websocket.Conn, x *execTarget, live *execws.Liveness, audit *execAudit) error {
	ws := context.WithoutCancel(ctx)
	timing := s.execTiming()
	noStart := time.AfterFunc(timing.start, func() { end(invalid("no start came in %s", timing.start)) })
	first, err := readExec(ws, c)
	if !noStart.Stop() {
		return context.Cause(ctx)
	}
	if err != nil {
		return clientFailed(ctx, err)
	}
	live.Heard()
	start := first.GetStart()
	if err := checkExecStart(start); err != nil {
		return err
	}
	audit.start = start
	s.auditStarted(x, start)

	// The node kills the process once the stream is reset on the tunnel.
	// ctx ending resets it while the response is being read, and closing
	// the response once nothing reads it.
	stream := x.call.nodes.Exec(ctx)
	context.AfterFunc(ctx, func() { _ = stream.CloseResponse() })
	node := &nodeSender{stream: stream, live: live}
	if err := node.send(&nodev1.ExecRequest{Message: &nodev1.ExecRequest_Start{Start: nodeExecStart(x.machine, start)}}); err != nil {
		return s.nodeFailed(ctx, x, stream.Receive)
	}

	go func() {
		if live.Run(ctx, c) != nil {
			end(errClientGone)
		}
	}()
	go func() {
		// Once the exec has ended, end does nothing.
		end(relayInput(ctx, ws, c, node, start.GetStdin()))
	}()

	for {
		res, err := stream.Receive()
		if err != nil {
			if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
				return cause
			}
			return x.call.failed(err)
		}
		out := &computev1alpha1.ContainerExecResponse{}
		switch m := res.GetMessage().(type) {
		case *nodev1.ExecResponse_Stdout:
			out.Message = &computev1alpha1.ContainerExecResponse_Stdout{Stdout: m.Stdout}
		case *nodev1.ExecResponse_Stderr:
			out.Message = &computev1alpha1.ContainerExecResponse_Stderr{Stderr: m.Stderr}
		case *nodev1.ExecResponse_Exit:
			audit.exit = m.Exit
			out.Message = &computev1alpha1.ContainerExecResponse_Exit{Exit: &computev1alpha1.ContainerExecExit{
				Code: m.Exit.GetCode(), Signal: m.Exit.GetSignal(),
			}}
		default:
			continue
		}
		done := live.Pending()
		err = writeExec(ws, c, out)
		done()
		if err != nil {
			return clientFailed(ctx, err)
		}
		if audit.exit != nil {
			// The node ends the call once this side is closed.
			node.close()
			return nil
		}
	}
}

// nodeFailed is what an exec fails with when the node's call failed: the
// call says why on receive.
func (s *Server) nodeFailed(ctx context.Context, x *execTarget, receive func() (*nodev1.ExecResponse, error)) error {
	_, err := receive()
	if err == nil || ctx.Err() != nil {
		return errClientGone
	}
	return x.call.failed(err)
}

// clientFailed is what an exec fails with when reading from or writing to
// its client failed with err: why the exec ended, if it has, a message
// that is not one, or the client gone.
func clientFailed(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	if ce := new(connect.Error); errors.As(err, &ce) {
		return err
	}
	return errClientGone
}

// nodeSender sends to the node's call from the client's reader and closes
// it from the relay, one at a time, as the call needs. A send held up by
// the process not reading is pending to live.
type nodeSender struct {
	mu     sync.Mutex
	stream interface {
		Send(*nodev1.ExecRequest) error
		CloseRequest() error
	}
	live   *execws.Liveness
	closed bool
}

func (n *nodeSender) send(req *nodev1.ExecRequest) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil // the process has ended; what comes after is dropped
	}
	done := n.live.Pending()
	defer done()
	return n.stream.Send(req)
}

func (n *nodeSender) close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.closed {
		n.closed = true
		_ = n.stream.CloseRequest()
	}
}

// relayInput passes on what the client sends after the start until it
// goes, reading with ws. It returns why the exec must end: errClientGone,
// what the client sent that it may not, or, once the exec has ended, why
// it did.
func relayInput(ctx, ws context.Context, c *websocket.Conn, node *nodeSender, stdin bool) error {
	for {
		req, err := readExec(ws, c)
		if err != nil {
			return clientFailed(ctx, err)
		}
		node.live.Heard()
		out := &nodev1.ExecRequest{}
		switch m := req.GetMessage().(type) {
		case *computev1alpha1.ContainerExecRequest_Stdin:
			if !stdin {
				return invalid("stdin for an exec started without it")
			}
			out.Message = &nodev1.ExecRequest_Stdin{Stdin: m.Stdin}
		case *computev1alpha1.ContainerExecRequest_StdinClose:
			if !stdin {
				return invalid("stdin for an exec started without it")
			}
			out.Message = &nodev1.ExecRequest_StdinClose{StdinClose: &nodev1.ExecStdinClose{}}
		case *computev1alpha1.ContainerExecRequest_Resize:
			out.Message = &nodev1.ExecRequest_Resize{Resize: &nodev1.WindowSize{Rows: m.Resize.GetRows(), Cols: m.Resize.GetCols()}}
		case *computev1alpha1.ContainerExecRequest_Signal:
			out.Message = &nodev1.ExecRequest_Signal{Signal: m.Signal}
		case *computev1alpha1.ContainerExecRequest_Start:
			return invalid("an exec starts once")
		default:
			continue
		}
		// A failed send means the call is over; the relay says why.
		_ = node.send(out)
	}
}

type execTiming struct{ ping, pong, start time.Duration }

func (s *Server) execTiming() execTiming {
	t := execTiming{ping: s.cfg.ExecPing, pong: s.cfg.ExecPong, start: s.cfg.ExecStart}
	if t.ping <= 0 {
		t.ping = DefaultExecPing
	}
	if t.pong <= 0 {
		t.pong = DefaultExecPong
	}
	if t.start <= 0 {
		t.start = DefaultExecStart
	}
	return t
}

// readExec reads the client's next message. A message that is not one is
// InvalidArgument.
func readExec(ctx context.Context, c *websocket.Conn) (*computev1alpha1.ContainerExecRequest, error) {
	typ, b, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageBinary {
		return nil, invalid("an exec's messages are binary")
	}
	req := &computev1alpha1.ContainerExecRequest{}
	if err := proto.Unmarshal(b, req); err != nil {
		return nil, invalid("a message that is not a ContainerExecRequest: %v", err)
	}
	return req, nil
}

func writeExec(ctx context.Context, c *websocket.Conn, res *computev1alpha1.ContainerExecResponse) error {
	b, err := proto.Marshal(res)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageBinary, b)
}

func checkExecStart(start *computev1alpha1.ContainerExecStart) error {
	switch {
	case start == nil:
		return invalid("the first message must start the process")
	case len(start.GetCommand()) == 0 || start.GetCommand()[0] == "":
		return invalid("a command is required")
	}
	for _, e := range start.GetEnv() {
		if e.GetName() == "" || strings.Contains(e.GetName(), "=") {
			return invalid("env name %q is empty or holds =", e.GetName())
		}
	}
	return nil
}

func nodeExecStart(machineUID string, start *computev1alpha1.ContainerExecStart) *nodev1.ExecStart {
	out := &nodev1.ExecStart{
		MachineUid: machineUID, Args: start.GetCommand(), WorkingDir: start.GetWorkingDir(), User: start.GetUser(),
		Tty: start.GetTty(), Stdin: start.GetStdin(),
		Size: &nodev1.WindowSize{Rows: start.GetSize().GetRows(), Cols: start.GetSize().GetCols()},
	}
	for _, e := range start.GetEnv() {
		out.Env = append(out.Env, e.GetName()+"="+e.GetValue())
	}
	return out
}
