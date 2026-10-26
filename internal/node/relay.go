package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"

	"connectrpc.com/connect"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// sshPort is the port the platform's SSH reaches.
const sshPort = 22

// dialTimeout bounds connecting to a guest that does not answer.
const dialTimeout = 10 * time.Second

// relayChunk bounds what one response carries.
const relayChunk = 32 << 10

// SSH relays a connection to port 22 of a running machine's eth0, which the
// node picks, from its network's gateway, for as long as the call lasts.
func (s nodeService) SSH(ctx context.Context, stream *connect.BidiStream[nodev1.SSHRequest, nodev1.SSHResponse]) error {
	if s.a.cfg.Drivers == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("this node runs no machines"))
	}
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	uid := first.GetMachineUid()
	if uid == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("the first request names the machine"))
	}
	return s.a.relay(ctx, uid, sshPort, s.a.cfg.Drivers.Network.Dial,
		func() ([]byte, error) {
			req, err := stream.Receive()
			return req.GetData(), err
		},
		func(b []byte) error { return stream.Send(&nodev1.SSHResponse{Data: b}) })
}

// PortForward relays a connection to a TCP port of a running machine's
// eth0, which the node picks, from its network's gateway, for as long as
// the call lasts.
func (s nodeService) PortForward(ctx context.Context, stream *connect.BidiStream[nodev1.PortForwardRequest, nodev1.PortForwardResponse]) error {
	if s.a.cfg.Drivers == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("this node runs no machines"))
	}
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start.GetMachineUid() == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("the first request names the machine"))
	}
	port := start.GetPort()
	if port < 1 || port > 65535 {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("port %d is not 1 to 65535", port))
	}
	return s.a.relay(ctx, start.GetMachineUid(), uint16(port), s.a.cfg.Drivers.Network.DialAnyPort,
		func() ([]byte, error) {
			req, err := stream.Receive()
			return req.GetData(), err
		},
		func(b []byte) error { return stream.Send(&nodev1.PortForwardResponse{Data: b}) })
}

// dialFunc is a way the network driver dials a machine from its network's
// gateway: Dial, or DialAnyPort.
type dialFunc func(ctx context.Context, networkUID string, addr netip.AddrPort) (net.Conn, error)

// relay connects by dial to port of the running machine uid's eth0 and
// relays bytes both ways: what receive gives goes to the guest until it
// fails, and what the guest sends goes to send until the guest closes its
// side. A guest that answers nothing there is Unavailable.
func (a *Agent) relay(ctx context.Context, uid string, port uint16, dial dialFunc, receive func() ([]byte, error), send func([]byte) error) error {
	vm, err := a.vmOf(ctx, uid)
	if err != nil {
		return connect.NewError(connect.CodeUnknown, err)
	}
	if vm == nil || vm.State != driver.VMRunning {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("machine %s's VM is not running", uid))
	}
	nic, err := a.eth0(ctx, uid)
	if err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	conn, err := dial(dctx, nic.NetworkUID, netip.AddrPortFrom(nic.Address.Addr(), port))
	cancel()
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("machine %s's port %d: %w", uid, port, err))
	}
	defer func() { _ = conn.Close() }()
	// What the caller sends goes on to the guest until it closes its side.
	go func() {
		for {
			b, err := receive()
			if err != nil {
				_ = conn.Close()
				return
			}
			if _, err := conn.Write(b); err != nil {
				return
			}
		}
	}()
	buf := make([]byte, relayChunk)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if serr := send(buf[:n]); serr != nil {
				return serr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return connect.NewError(connect.CodeCanceled, ctx.Err())
			}
			return nil // closed with the caller's side
		}
	}
}

// eth0 is the machine's first interface on the host.
func (a *Agent) eth0(ctx context.Context, uid string) (driver.Interface, error) {
	ifaces, err := a.cfg.Drivers.Network.Interfaces(ctx)
	if err != nil {
		return driver.Interface{}, connect.NewError(connect.CodeUnknown, err)
	}
	for _, i := range ifaces {
		if i.MachineUID == uid && i.Name == "eth0" && i.Address.IsValid() {
			return i, nil
		}
	}
	return driver.Interface{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("machine %s has no eth0 on the host", uid))
}
