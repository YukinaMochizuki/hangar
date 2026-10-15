package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"time"

	"connectrpc.com/connect"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// sshPort is the one port the platform's SSH reaches.
const sshPort = 22

// sshDialTimeout bounds connecting to a guest that does not answer.
const sshDialTimeout = 10 * time.Second

// sshChunk bounds what one response carries.
const sshChunk = 32 << 10

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
	vm, err := s.a.vmOf(ctx, uid)
	if err != nil {
		return connect.NewError(connect.CodeUnknown, err)
	}
	if vm == nil || vm.State != driver.VMRunning {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("machine %s's VM is not running", uid))
	}
	nic, err := s.a.eth0(ctx, uid)
	if err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, sshDialTimeout)
	conn, err := s.a.cfg.Drivers.Network.Dial(dctx, nic.NetworkUID, netip.AddrPortFrom(nic.Address.Addr(), sshPort))
	cancel()
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("machine %s's port %d: %w", uid, sshPort, err))
	}
	defer func() { _ = conn.Close() }()
	// What the caller sends goes on to the guest until it closes its side.
	go func() {
		for {
			req, err := stream.Receive()
			if err != nil {
				_ = conn.Close()
				return
			}
			if _, err := conn.Write(req.GetData()); err != nil {
				return
			}
		}
	}()
	buf := make([]byte, sshChunk)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if serr := stream.Send(&nodev1.SSHResponse{Data: buf[:n]}); serr != nil {
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
