package apiserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/portforwardws"
	"github.com/YukinaMochizuki/hangar/internal/sshws"
)

// tunnelKind is a capability that relays one TCP connection to a port of
// a machine over a WebSocket: an SSH tunnel (vm-ssh.md) or a port forward
// (port-forward.md).
type tunnelKind struct {
	// audit names it in audit lines, such as "ssh opened".
	audit string
	// what is it in words, such as "an SSH tunnel".
	what        string
	verb        authz.Verb
	fits        func(*reached, *computev1alpha1.Machine) error
	subprotocol string
	maxMessage  int
	// auditPort puts the port in the audit lines: the caller chose it.
	auditPort bool
	// open begins the node's call for a connection to port of machine, its
	// first request sent.
	open func(ctx context.Context, x *machineTarget, port uint16) (nodeTunnel, error)
}

// nodeTunnel is the node's side of a tunnel.
type nodeTunnel interface {
	send(b []byte) error
	// receive is io.EOF once the guest closed the connection.
	receive() ([]byte, error)
	closeRequest() error
	closeResponse() error
}

var sshTunnel = tunnelKind{audit: "ssh", what: "an SSH tunnel", verb: authz.SSH, fits: sshFits,
	subprotocol: sshws.Subprotocol, maxMessage: sshws.MaxMessage,
	open: func(ctx context.Context, x *machineTarget, _ uint16) (nodeTunnel, error) {
		st := x.call.nodes.SSH(ctx)
		t := sshNodeTunnel{st}
		return t, st.Send(&nodev1.SSHRequest{Message: &nodev1.SSHRequest_MachineUid{MachineUid: x.machine}})
	}}

var portForwardTunnel = tunnelKind{audit: "portforward", what: "a port forward", verb: authz.PortForward,
	subprotocol: portforwardws.Subprotocol, maxMessage: portforwardws.MaxMessage, auditPort: true,
	open: func(ctx context.Context, x *machineTarget, port uint16) (nodeTunnel, error) {
		st := x.call.nodes.PortForward(ctx)
		t := portForwardNodeTunnel{st}
		return t, st.Send(&nodev1.PortForwardRequest{Message: &nodev1.PortForwardRequest_Start{
			Start: &nodev1.PortForwardStart{MachineUid: x.machine, Port: int32(port)}}})
	}}

type sshNodeTunnel struct {
	st interface {
		Send(*nodev1.SSHRequest) error
		Receive() (*nodev1.SSHResponse, error)
		CloseRequest() error
		CloseResponse() error
	}
}

func (t sshNodeTunnel) send(b []byte) error {
	return t.st.Send(&nodev1.SSHRequest{Message: &nodev1.SSHRequest_Data{Data: b}})
}

func (t sshNodeTunnel) receive() ([]byte, error) {
	res, err := t.st.Receive()
	return res.GetData(), err
}
func (t sshNodeTunnel) closeRequest() error  { return t.st.CloseRequest() }
func (t sshNodeTunnel) closeResponse() error { return t.st.CloseResponse() }

type portForwardNodeTunnel struct {
	st interface {
		Send(*nodev1.PortForwardRequest) error
		Receive() (*nodev1.PortForwardResponse, error)
		CloseRequest() error
		CloseResponse() error
	}
}

func (t portForwardNodeTunnel) send(b []byte) error {
	return t.st.Send(&nodev1.PortForwardRequest{Message: &nodev1.PortForwardRequest_Data{Data: b}})
}

func (t portForwardNodeTunnel) receive() ([]byte, error) {
	res, err := t.st.Receive()
	return res.GetData(), err
}
func (t portForwardNodeTunnel) closeRequest() error  { return t.st.CloseRequest() }
func (t portForwardNodeTunnel) closeResponse() error { return t.st.CloseResponse() }

// serveSSH serves an SSH tunnel to the machine of the object route names;
// its node picks the address, the machine's own, and the port, 22.
func (s *Server) serveSSH(chain auth.Chain, w http.ResponseWriter, r *http.Request, route execws.Route) {
	s.serveTunnel(chain, w, r, route, sshTunnel, func() (uint16, error) { return 22, nil })
}

// servePortForward serves a port forward to the port the query names of
// the machine of the object route names; its node picks the address, the
// machine's own.
func (s *Server) servePortForward(chain auth.Chain, w http.ResponseWriter, r *http.Request, route execws.Route) {
	s.serveTunnel(chain, w, r, route, portForwardTunnel, func() (uint16, error) {
		q := r.URL.Query()[portforwardws.PortParam]
		if len(q) != 1 {
			return 0, invalid("a port forward names one port, as ?%s=", portforwardws.PortParam)
		}
		port, err := strconv.ParseUint(q[0], 10, 16)
		if err != nil || port == 0 {
			return 0, invalid("port %q is not 1 to 65535", q[0])
		}
		return uint16(port), nil
	})
}

// serveTunnel serves a tunnel of kind k as an exec is served, refusing
// before the upgrade and telling nothing of the object before authorizing:
// port reads the request alone.
func (s *Server) serveTunnel(chain auth.Chain, w http.ResponseWriter, r *http.Request, route execws.Route, k tunnelKind, port func() (uint16, error)) {
	key := resource.Key{Project: route.Project, Name: route.Name}
	refuse := func(p auth.Principal, err error) {
		s.auditRefused(k.audit+" refused", p, clientIP(r.Context()), route, err)
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
	to, err := port()
	if err != nil {
		refuse(p, err)
		return
	}
	x, err := s.machineTarget(ctx, kind, key, k.verb, k.fits)
	if err != nil {
		refuse(p, err)
		return
	}
	defer x.call.release()
	if !slices.Contains(subprotocols(r), k.subprotocol) {
		refuse(p, invalid("%s is a WebSocket with the subprotocol %s", k.what, k.subprotocol))
		return
	}
	timing := s.execTiming()
	live := execws.NewLiveness(timing.ping, timing.pong)
	// The Origin check stays on, as an exec's.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:   []string{k.subprotocol},
		OnPingReceived: live.OnPing, OnPongReceived: live.OnPong,
	})
	if err != nil {
		s.auditRefused(k.audit+" refused", p, clientIP(r.Context()), route, connect.NewError(connect.CodeInvalidArgument, err))
		return
	}
	s.runTunnel(ctx, c, x, k, to, live)
}

// runTunnel relays the WebSocket and the node's call until either ends,
// and closes the WebSocket.
func (s *Server) runTunnel(ctx context.Context, c *websocket.Conn, x *machineTarget, k tunnelKind, port uint16, live *execws.Liveness) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	c.SetReadLimit(int64(k.maxMessage))
	began := time.Now()
	var in, out atomic.Int64
	attrs := x.auditAttrs()
	if k.auditPort {
		attrs = append(attrs, "port", port)
	}
	s.log.Info(k.audit+" opened", attrs...)
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
	// A recheck that refuses the caller ends the tunnel at once, as an
	// exec's.
	defer context.AfterFunc(ctx, func() {
		if err := auth.Ended(ctx); err != nil {
			end(err)
		}
	})()
	ws := context.WithoutCancel(ctx)
	var sendMu sync.Mutex
	sendMu.Lock()
	done := live.Pending()
	stream, err := k.open(ctx, x, port)
	done()
	sendMu.Unlock()
	if stream != nil {
		context.AfterFunc(ctx, func() { _ = stream.closeResponse() })
	}
	send := func(b []byte) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		done := live.Pending()
		defer done()
		return stream.send(b)
	}
	if err == nil {
		go func() {
			if live.Run(ctx, c) != nil {
				end(errClientGone)
			}
		}()
		go func() {
			for {
				typ, b, err := c.Read(ws)
				if err != nil {
					// The client closed, or went: the node's side ends with it.
					sendMu.Lock()
					_ = stream.closeRequest()
					sendMu.Unlock()
					end(clientFailed(ctx, err))
					return
				}
				live.Heard()
				if typ != websocket.MessageBinary {
					end(invalid("%s's messages are binary", k.what))
					return
				}
				in.Add(int64(len(b)))
				if err := send(b); err != nil {
					return // the receive below says why
				}
			}
		}()
		err = relayTunnelOut(ctx, ws, c, stream.receive, live, k.maxMessage, &out)
		if err != nil && context.Cause(ctx) == nil {
			err = x.call.failed(err)
		}
	}
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		err = cause
	}
	attrs = append(attrs, "duration", time.Since(began).Round(time.Millisecond).String(), "bytes_in", in.Load(), "bytes_out", out.Load())
	if err != nil {
		attrs = append(attrs, "code", connect.CodeOf(err).String(), "error", err.Error())
	}
	s.log.Info(k.audit+" closed", attrs...)
	end(err)
}

// relayTunnelOut passes what the guest sends to the client until the
// node's side ends: nil when the guest closed the connection.
func relayTunnelOut(ctx, ws context.Context, c *websocket.Conn, receive func() ([]byte, error), live *execws.Liveness, maxMessage int, out *atomic.Int64) error {
	for {
		data, err := receive()
		switch {
		case errors.Is(err, io.EOF):
			return nil // the guest closed the connection
		case err != nil && ctx.Err() != nil:
			return ctx.Err() // why is the client's, which runTunnel says
		case err != nil:
			return err
		}
		for len(data) > 0 {
			n := min(len(data), maxMessage)
			done := live.Pending()
			err := c.Write(ws, websocket.MessageBinary, data[:n])
			done()
			if err != nil {
				return clientFailed(ctx, err)
			}
			out.Add(int64(n))
			data = data[n:]
		}
	}
}
