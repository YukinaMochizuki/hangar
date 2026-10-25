package apiserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/consolews"
	"github.com/YukinaMochizuki/hangar/internal/execws"
)

// consoleFits refuses a machine with no firmware: only a virtual machine's
// serial console is interactive (docs/decisions/serial-console.md).
func consoleFits(r *reached, machine *computev1alpha1.Machine) error {
	if machine.GetSpec().GetFirmware() == nil {
		return failedPrecondition("%s has no interactive serial console: only a virtual machine's is", r)
	}
	return nil
}

// serveConsole attaches the client to the serial console of the machine of
// the object route names, a WebSocket refused, as an exec is, before the
// upgrade and telling nothing before the caller is authorized; it needs
// compute.machines.exec.
func (s *Server) serveConsole(chain auth.Chain, w http.ResponseWriter, r *http.Request, route execws.Route) {
	key := resource.Key{Project: route.Project, Name: route.Name}
	refuse := func(p auth.Principal, err error) {
		s.auditRefused("console refused", p, clientIP(r.Context()), route, err)
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
	x, err := s.machineTarget(ctx, kind, key, authz.Exec, consoleFits)
	if err != nil {
		refuse(p, err)
		return
	}
	defer x.call.release()
	if !slices.Contains(subprotocols(r), consolews.Subprotocol) {
		refuse(p, invalid("a serial console is a WebSocket with the subprotocol %s", consolews.Subprotocol))
		return
	}
	timing := s.execTiming()
	live := execws.NewLiveness(timing.ping, timing.pong)
	// The Origin check stays on, as an exec's.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:   []string{consolews.Subprotocol},
		OnPingReceived: live.OnPing, OnPongReceived: live.OnPong,
	})
	if err != nil {
		s.auditRefused("console refused", p, clientIP(r.Context()), route, connect.NewError(connect.CodeInvalidArgument, err))
		return
	}
	s.runConsole(ctx, c, x, live)
}

// runConsole relays the WebSocket and the node's call until either ends,
// and closes the WebSocket. The audit lines say who, which machine, for how
// long and how many bytes each way, never what.
func (s *Server) runConsole(ctx context.Context, c *websocket.Conn, x *machineTarget, live *execws.Liveness) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	c.SetReadLimit(consolews.MaxMessage)
	began := time.Now()
	var in, out atomic.Int64
	s.log.Info("console opened", x.auditAttrs()...)
	var once sync.Once
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
	// A recheck that refuses the caller ends the console at once.
	defer context.AfterFunc(ctx, func() {
		if err := auth.Ended(ctx); err != nil {
			end(err)
		}
	})()
	start, err := s.readConsoleStart(ctx, end, c, live)
	if err == nil {
		err = s.relayConsole(ctx, c, x, live, end, start, &in, &out)
	}
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		err = cause
	}
	attrs := append(x.auditAttrs(), "duration", time.Since(began).Round(time.Millisecond).String(), "bytes_in", in.Load(), "bytes_out", out.Load())
	if err != nil {
		attrs = append(attrs, "code", connect.CodeOf(err).String(), "error", err.Error())
	}
	s.log.Info("console closed", attrs...)
	end(err)
}

// relayConsole attaches to the node's console with the client's start and
// passes the rest both ways, until either side ends.
func (s *Server) relayConsole(ctx context.Context, c *websocket.Conn, x *machineTarget, live *execws.Liveness, end func(error),
	start *computev1alpha1.MachineConsoleStart, in, out *atomic.Int64) error {
	ws := context.WithoutCancel(ctx)
	stream := x.call.nodes.Console(ctx)
	context.AfterFunc(ctx, func() { _ = stream.CloseResponse() })
	var sendMu sync.Mutex
	send := func(req *nodev1.ConsoleRequest) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		done := live.Pending()
		defer done()
		return stream.Send(req)
	}
	err := send(&nodev1.ConsoleRequest{Message: &nodev1.ConsoleRequest_Start{Start: &nodev1.ConsoleStart{
		MachineUid: x.machine, ReadOnly: start.GetReadOnly(), Force: start.GetForce(),
	}}})
	if err != nil {
		return x.call.failed(err)
	}
	go func() {
		if live.Run(ctx, c) != nil {
			end(errClientGone)
		}
	}()
	go func() {
		for {
			req, err := readConsole(ws, c)
			if err != nil {
				sendMu.Lock()
				_ = stream.CloseRequest()
				sendMu.Unlock()
				end(clientFailed(ctx, err))
				return
			}
			live.Heard()
			var msg *nodev1.ConsoleRequest
			switch m := req.GetMessage().(type) {
			case nil:
				continue // a keepalive
			case *computev1alpha1.MachineConsoleRequest_Input:
				in.Add(int64(len(m.Input)))
				msg = &nodev1.ConsoleRequest{Message: &nodev1.ConsoleRequest_Input{Input: m.Input}}
			case *computev1alpha1.MachineConsoleRequest_Resize:
				msg = &nodev1.ConsoleRequest{Message: &nodev1.ConsoleRequest_Resize{Resize: &nodev1.ConsoleSize{
					Columns: m.Resize.GetColumns(), Rows: m.Resize.GetRows(),
				}}}
			default:
				end(invalid("only the first message is a start"))
				return
			}
			if err := send(msg); err != nil {
				return // the receive below says why
			}
		}
	}()
	err = s.relayConsoleOut(ctx, ws, c, stream.Receive, live, out)
	if err != nil && context.Cause(ctx) == nil {
		err = x.call.failed(err)
	}
	return err
}

// readConsoleStart reads the client's first message, which must be a
// start, within the exec's start timeout, else ending the console. It
// reads as an exec's start is read, never cancelled: coder/websocket
// closes a connection whose read is, before end's close says why.
func (s *Server) readConsoleStart(ctx context.Context, end func(error), c *websocket.Conn, live *execws.Liveness) (*computev1alpha1.MachineConsoleStart, error) {
	timing := s.execTiming()
	noStart := time.AfterFunc(timing.start, func() { end(invalid("no start came in %s", timing.start)) })
	defer noStart.Stop()
	for {
		req, err := readConsole(context.WithoutCancel(ctx), c)
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		if err != nil {
			return nil, clientFailed(ctx, err)
		}
		live.Heard()
		if req.GetMessage() == nil {
			continue // a keepalive
		}
		if req.GetStart() == nil {
			return nil, invalid("the first message is a start")
		}
		return req.GetStart(), nil
	}
}

// relayConsoleOut passes what the node sends to the client until the
// node's side ends: nil when the VM, or its console, ended.
func (s *Server) relayConsoleOut(ctx, ws context.Context, c *websocket.Conn, receive func() (*nodev1.ConsoleResponse, error), live *execws.Liveness, out *atomic.Int64) error {
	for {
		res, err := receive()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil && ctx.Err() != nil:
			return ctx.Err() // why is the client's, which runConsole says
		case err != nil:
			return err
		}
		var msg *computev1alpha1.MachineConsoleResponse
		switch m := res.GetMessage().(type) {
		case *nodev1.ConsoleResponse_Output:
			out.Add(int64(len(m.Output)))
			msg = &computev1alpha1.MachineConsoleResponse{Message: &computev1alpha1.MachineConsoleResponse_Output{Output: m.Output}}
		case *nodev1.ConsoleResponse_State:
			msg = &computev1alpha1.MachineConsoleResponse{Message: &computev1alpha1.MachineConsoleResponse_State{State: &computev1alpha1.MachineConsoleState{
				Input: m.State.GetInput(),
				Size:  &computev1alpha1.MachineConsoleSize{Columns: m.State.GetSize().GetColumns(), Rows: m.State.GetSize().GetRows()},
			}}}
		default:
			continue
		}
		b, err := proto.Marshal(msg)
		if err != nil {
			return err
		}
		done := live.Pending()
		err = c.Write(ws, websocket.MessageBinary, b)
		done()
		if err != nil {
			return clientFailed(ctx, err)
		}
	}
}

// readConsole reads one message of the client's.
func readConsole(ctx context.Context, c *websocket.Conn) (*computev1alpha1.MachineConsoleRequest, error) {
	typ, b, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageBinary {
		return nil, invalid("a serial console's messages are binary")
	}
	req := &computev1alpha1.MachineConsoleRequest{}
	if err := proto.Unmarshal(b, req); err != nil {
		return nil, invalid("a serial console's message: %v", err)
	}
	return req, nil
}
