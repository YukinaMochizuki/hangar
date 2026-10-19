package apiserver

import (
	"context"
	"errors"
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
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/sshca"
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

// serveExec serves an exec in the machine of the object route names, a
// WebSocket on the public listener. Everything that can refuse it does so
// before the upgrade, and nothing about the object is told before the
// caller is authorized. It is held as a Connect stream is (auth.Chain.Hold).
func (s *Server) serveExec(chain auth.Chain, w http.ResponseWriter, r *http.Request, route execws.Route) {
	key := resource.Key{Project: route.Project, Name: route.Name}
	refuse := func(p auth.Principal, err error) {
		s.auditRefused("exec refused", p, clientIP(r.Context()), route, err)
		execws.WriteError(w, r, err)
	}
	ctx, release, err := chain.Hold(r.Context(), r.Header, s.clock)
	if err != nil {
		refuse(auth.Principal{}, connect.NewError(connect.CodeUnauthenticated, err))
		return
	}
	defer release()
	p, _ := auth.FromContext(ctx)
	kind, err := kindOfPlural(route.Plural)
	if err != nil {
		refuse(p, err)
		return
	}
	// A VM's exec is an SSH login (vm-ssh.md): it needs the ssh
	// permission, and a machine with an sshd.
	verb, fits := authz.Exec, (func(*reached, *computev1alpha1.Machine) error)(nil)
	overSSH := machineKinds[kind.Kind].execOverSSH
	if overSSH {
		verb, fits = authz.SSH, sshFits
	}
	x, err := s.machineTarget(ctx, kind, key, verb, fits)
	if err != nil {
		refuse(p, err)
		return
	}
	defer x.call.release()
	if overSSH && s.cfg.SSHCA == nil {
		refuse(p, connect.NewError(connect.CodeFailedPrecondition, sshca.ErrNoCA))
		return
	}
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
		s.auditRefused("exec refused", p, clientIP(r.Context()), route, connect.NewError(code, err))
		return
	}
	s.runExec(ctx, c, x, live, route)
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

// execAudit is what the audit lines of an exec say.
type execAudit struct {
	start *computev1alpha1.MachineExecStart
	// user is whom the process runs as: as the start says, or, in a VM,
	// whom the exec logged in as.
	user string
	// key is the fingerprint of the key a VM's exec logged in with.
	key   string
	exit  *computev1alpha1.MachineExecExit
	began time.Time
}

// runExec runs an exec on an upgraded WebSocket until the process ends,
// the client goes, or the node does, and closes the WebSocket.
func (s *Server) runExec(ctx context.Context, c *websocket.Conn, x *machineTarget, live *execws.Liveness, route execws.Route) {
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
	// A recheck that refuses the caller ends the exec at once, even one
	// blocked writing to a client that reads nothing.
	defer context.AfterFunc(ctx, func() {
		if err := auth.Ended(ctx); err != nil {
			end(err)
		}
	})()
	audit := &execAudit{began: time.Now()}
	run := s.exec
	if machineKinds[x.kind.Kind].execOverSSH {
		run = s.vmExec
	}
	err := run(ctx, end, c, x, live, audit)
	if audit.start == nil {
		s.auditRefused("exec refused", x.principal, x.client, route, err)
	} else {
		s.auditEnded(x, audit, err)
	}
	end(err)
}

// auditRefused logs, as msg, a capability's WebSocket refused before it
// began: an exec before its process started, an SSH tunnel before it
// opened, and the caller's address. It says what the route names, not the
// kind it resolves to, as a refusal may come before that.
func (s *Server) auditRefused(msg string, p auth.Principal, client string, route execws.Route, err error) {
	principal := ""
	if p != (auth.Principal{}) {
		principal = p.String()
	}
	s.log.Info(msg, "principal", principal, "client_ip", client, "project", route.Project, "plural", route.Plural, "name", route.Name,
		"code", connect.CodeOf(err).String(), "error", err.Error())
}

// auditStarted logs an exec whose process is asked of the node, or, in a
// VM, of its sshd: never its input or output, nor its environment's
// values.
func (s *Server) auditStarted(x *machineTarget, a *execAudit) {
	start := a.start
	env := make([]string, 0, len(start.GetEnv()))
	for _, e := range start.GetEnv() {
		env = append(env, e.GetName())
	}
	attrs := append(x.auditAttrs(), "argv", start.GetCommand(), "tty", start.GetTty(), "stdin", start.GetStdin(),
		"user", a.user, "workdir", start.GetWorkingDir(), "env_names", env)
	if a.key != "" {
		attrs = append(attrs, "key", a.key)
	}
	s.log.Info("exec started", attrs...)
}

func (s *Server) auditEnded(x *machineTarget, a *execAudit, err error) {
	attrs := append(x.auditAttrs(), "duration", time.Since(a.began).Round(time.Millisecond).String())
	if a.key != "" {
		attrs = append(attrs, "user", a.user, "key", a.key)
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
func (s *Server) exec(ctx context.Context, end func(error), c *websocket.Conn, x *machineTarget, live *execws.Liveness, audit *execAudit) error {
	ws := context.WithoutCancel(ctx)
	start, err := s.readStart(ctx, end, c, live)
	if err != nil {
		return err
	}
	if err := checkExecStart(start); err != nil {
		return err
	}
	audit.start, audit.user = start, start.GetUser()
	s.auditStarted(x, audit)

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
		out := &computev1alpha1.MachineExecResponse{}
		switch m := res.GetMessage().(type) {
		case *nodev1.ExecResponse_Stdout:
			out.Message = &computev1alpha1.MachineExecResponse_Stdout{Stdout: m.Stdout}
		case *nodev1.ExecResponse_Stderr:
			out.Message = &computev1alpha1.MachineExecResponse_Stderr{Stderr: m.Stderr}
		case *nodev1.ExecResponse_Exit:
			audit.exit = &computev1alpha1.MachineExecExit{Code: m.Exit.GetCode(), Signal: m.Exit.GetSignal()}
			out.Message = &computev1alpha1.MachineExecResponse_Exit{Exit: audit.exit}
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

// readStart reads an exec's first message, which comes within the start
// timing or ends the exec, as its start, unchecked.
func (s *Server) readStart(ctx context.Context, end func(error), c *websocket.Conn, live *execws.Liveness) (*computev1alpha1.MachineExecStart, error) {
	timing := s.execTiming()
	noStart := time.AfterFunc(timing.start, func() { end(invalid("no start came in %s", timing.start)) })
	first, err := readExec(context.WithoutCancel(ctx), c)
	if !noStart.Stop() {
		return nil, context.Cause(ctx)
	}
	if err != nil {
		return nil, clientFailed(ctx, err)
	}
	live.Heard()
	return first.GetStart(), nil
}

// nodeFailed is what an exec fails with when the node's call failed: the
// call says why on receive.
func (s *Server) nodeFailed(ctx context.Context, x *machineTarget, receive func() (*nodev1.ExecResponse, error)) error {
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
		case *computev1alpha1.MachineExecRequest_Stdin:
			if !stdin {
				return invalid("stdin for an exec started without it")
			}
			out.Message = &nodev1.ExecRequest_Stdin{Stdin: m.Stdin}
		case *computev1alpha1.MachineExecRequest_StdinClose:
			if !stdin {
				return invalid("stdin for an exec started without it")
			}
			out.Message = &nodev1.ExecRequest_StdinClose{StdinClose: &nodev1.ExecStdinClose{}}
		case *computev1alpha1.MachineExecRequest_Resize:
			out.Message = &nodev1.ExecRequest_Resize{Resize: &nodev1.WindowSize{Rows: m.Resize.GetRows(), Cols: m.Resize.GetCols()}}
		case *computev1alpha1.MachineExecRequest_Signal:
			out.Message = &nodev1.ExecRequest_Signal{Signal: m.Signal}
		case *computev1alpha1.MachineExecRequest_Start:
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
func readExec(ctx context.Context, c *websocket.Conn) (*computev1alpha1.MachineExecRequest, error) {
	typ, b, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageBinary {
		return nil, invalid("an exec's messages are binary")
	}
	req := &computev1alpha1.MachineExecRequest{}
	if err := proto.Unmarshal(b, req); err != nil {
		return nil, invalid("a message that is not a MachineExecRequest: %v", err)
	}
	return req, nil
}

func writeExec(ctx context.Context, c *websocket.Conn, res *computev1alpha1.MachineExecResponse) error {
	b, err := proto.Marshal(res)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageBinary, b)
}

func checkExecStart(start *computev1alpha1.MachineExecStart) error {
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

func nodeExecStart(machineUID string, start *computev1alpha1.MachineExecStart) *nodev1.ExecStart {
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
