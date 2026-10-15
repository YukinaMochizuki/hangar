package apiserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/sshca"
	"github.com/YukinaMochizuki/hangar/internal/sshws"
)

// maxPublicKey bounds a key given to be signed.
const maxPublicKey = 16 << 10

// SignSSHKey certifies the caller's key for the machine of the named
// object; see vm-ssh.md.
func (h machineCapabilities) SignSSHKey(ctx context.Context, req *connect.Request[computev1alpha1.SignSSHKeyRequest]) (*connect.Response[computev1alpha1.SignSSHKeyResponse], error) {
	s := h.s
	m := req.Msg
	kind, err := kindNamed(m.GetKind())
	if err != nil {
		return nil, err
	}
	key := resource.Key{Project: m.GetProject(), Name: m.GetName()}
	r, err := s.reach(ctx, kind, key, authz.SSH, nil)
	if err != nil {
		return nil, err
	}
	p := r.principal
	if len(m.GetPublicKey()) > maxPublicKey {
		return nil, invalid("public_key is %d bytes, at most %d", len(m.GetPublicKey()), maxPublicKey)
	}
	if s.cfg.SSHCA == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, sshca.ErrNoCA)
	}
	uid := object.Meta(r.obj).GetUid()
	cert, err := s.cfg.SSHCA.Sign(m.GetPublicKey(), uid, p.String())
	if err != nil {
		if errors.Is(err, sshca.ErrNoCA) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		return nil, invalid("public_key: %v", err)
	}
	user, err := s.defaultUser(ctx, r.obj)
	if err != nil {
		return nil, err
	}
	validBefore := time.Unix(int64(cert.ValidBefore), 0)
	s.log.Info("ssh key signed", "principal", p.String(), "project", key.Project, "kind", kind.Kind, "name", key.Name, "uid", uid,
		"key", ssh.FingerprintSHA256(cert.Key), "valid_before", validBefore.UTC().Format(time.RFC3339))
	return connect.NewResponse(&computev1alpha1.SignSSHKeyResponse{
		Certificate: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(cert))),
		Principal:   uid, DefaultUser: user, ValidBefore: timestamppb.New(validBefore),
	}), nil
}

// defaultUser is the default user of the image a virtual machine's root
// disk was made from, if any says one; an object of another kind has none
// yet.
func (s *Server) defaultUser(ctx context.Context, obj proto.Message) (string, error) {
	vm, ok := obj.(*computev1alpha1.VirtualMachine)
	if !ok {
		return "", nil
	}
	image := vm.GetSpec().GetRootDisk().GetFromImage().GetImage().GetName()
	if name := vm.GetSpec().GetRootDisk().GetDisk(); name != "" {
		dobj, err := s.get(ctx, registry.Disk, resource.Key{Project: vm.GetMetadata().GetProject(), Name: name})
		if err != nil {
			return "", apiError(err)
		}
		if d, ok := dobj.(*storagev1alpha1.Disk); ok {
			image = d.GetSpec().GetSource().GetImage().GetName()
		}
	}
	if image == "" {
		return "", nil
	}
	iobj, err := s.get(ctx, registry.Image, resource.Key{Name: image})
	if err != nil {
		return "", apiError(err)
	}
	img, _ := iobj.(*storagev1alpha1.Image)
	return img.GetSpec().GetDefaultUser(), nil
}

// sshFits refuses a machine with no firmware: only a machine that boots a
// guest OS has an sshd to reach.
func sshFits(r *reached, machine *computev1alpha1.Machine) error {
	if machine.GetSpec().GetFirmware() == nil {
		return failedPrecondition("%s has no machine yet", r)
	}
	return nil
}

// serveSSH serves an SSH tunnel to the machine of the object route names
// as an exec is served, refusing before the upgrade and telling nothing
// before authorizing; its node picks the address, the machine's own, and
// the port, 22.
func (s *Server) serveSSH(chain auth.Chain, w http.ResponseWriter, r *http.Request, route execws.Route) {
	key := resource.Key{Project: route.Project, Name: route.Name}
	refuse := func(p auth.Principal, err error) {
		s.auditRefused("ssh refused", p, route, err)
		execws.WriteError(w, r, err)
	}
	ctx, err := chain.Authenticate(r.Context(), r.Header)
	if err != nil {
		refuse(auth.Principal{}, connect.NewError(connect.CodeUnauthenticated, err))
		return
	}
	p, _ := auth.FromContext(ctx)
	kind, err := kindOfPlural(route.Plural)
	if err != nil {
		refuse(p, err)
		return
	}
	x, err := s.machineTarget(ctx, kind, key, authz.SSH, sshFits)
	if err != nil {
		refuse(p, err)
		return
	}
	defer x.call.release()
	if !slices.Contains(subprotocols(r), sshws.Subprotocol) {
		refuse(p, invalid("an SSH tunnel is a WebSocket with the subprotocol %s", sshws.Subprotocol))
		return
	}
	timing := s.execTiming()
	live := execws.NewLiveness(timing.ping, timing.pong)
	// The Origin check stays on, as an exec's.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:   []string{sshws.Subprotocol},
		OnPingReceived: live.OnPing, OnPongReceived: live.OnPong,
	})
	if err != nil {
		s.auditRefused("ssh refused", p, route, connect.NewError(connect.CodeInvalidArgument, err))
		return
	}
	s.runSSH(ctx, c, x, live)
}

// runSSH relays the WebSocket and the node's call until either ends, and
// closes the WebSocket.
func (s *Server) runSSH(ctx context.Context, c *websocket.Conn, x *machineTarget, live *execws.Liveness) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	c.SetReadLimit(sshws.MaxMessage)
	began := time.Now()
	var in, out atomic.Int64
	s.log.Info("ssh opened", x.auditAttrs()...)
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
	ws := context.WithoutCancel(ctx)
	stream := x.call.nodes.SSH(ctx)
	context.AfterFunc(ctx, func() { _ = stream.CloseResponse() })
	var sendMu sync.Mutex
	send := func(req *nodev1.SSHRequest) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		done := live.Pending()
		defer done()
		return stream.Send(req)
	}
	err := send(&nodev1.SSHRequest{Message: &nodev1.SSHRequest_MachineUid{MachineUid: x.machine}})
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
					_ = stream.CloseRequest()
					sendMu.Unlock()
					end(clientFailed(ctx, err))
					return
				}
				live.Heard()
				if typ != websocket.MessageBinary {
					end(invalid("an SSH tunnel's messages are binary"))
					return
				}
				in.Add(int64(len(b)))
				if err := send(&nodev1.SSHRequest{Message: &nodev1.SSHRequest_Data{Data: b}}); err != nil {
					return // the receive below says why
				}
			}
		}()
		err = s.relaySSHOut(ctx, ws, c, stream.Receive, live, &out)
		if err != nil && context.Cause(ctx) == nil {
			err = x.call.failed(err)
		}
	}
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		err = cause
	}
	attrs := append(x.auditAttrs(), "duration", time.Since(began).Round(time.Millisecond).String(), "bytes_in", in.Load(), "bytes_out", out.Load())
	if err != nil {
		attrs = append(attrs, "code", connect.CodeOf(err).String(), "error", err.Error())
	}
	s.log.Info("ssh closed", attrs...)
	end(err)
}

// relaySSHOut passes what the guest sends to the client until the node's
// side ends: nil when the guest closed the connection.
func (s *Server) relaySSHOut(ctx, ws context.Context, c *websocket.Conn, receive func() (*nodev1.SSHResponse, error), live *execws.Liveness, out *atomic.Int64) error {
	for {
		res, err := receive()
		switch {
		case errors.Is(err, io.EOF):
			return nil // the guest closed the connection
		case err != nil && ctx.Err() != nil:
			return ctx.Err() // why is the client's, which runSSH says
		case err != nil:
			return err
		}
		data := res.GetData()
		for len(data) > 0 {
			n := min(len(data), sshws.MaxMessage)
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
