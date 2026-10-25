package node

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protodelim"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm/serialmux"
)

// Console relays a running firmware machine's serial console, as its
// console helper serves it, for as long as the call lasts.
func (s nodeService) Console(ctx context.Context, stream *connect.BidiStream[nodev1.ConsoleRequest, nodev1.ConsoleResponse]) error {
	if s.a.cfg.Drivers == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("this node runs no machines"))
	}
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	start := first.GetStart()
	uid := start.GetMachineUid()
	if uid == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("the first request is a start naming the machine"))
	}
	vm, err := s.a.vmOf(ctx, uid)
	if err != nil {
		return connect.NewError(connect.CodeUnknown, err)
	}
	if vm == nil || vm.State != driver.VMRunning {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("machine %s's VM is not running", uid))
	}
	conn, err := s.a.cfg.Drivers.Hypervisor.AttachConsole(ctx, uid)
	switch {
	case errors.Is(err, driver.ErrNoConsole):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case err != nil:
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("machine %s's console: %w", uid, err))
	}
	defer func() { _ = conn.Close() }()
	// The helper is told only how the client attaches: the machine is its.
	helperStart := &nodev1.ConsoleRequest{Message: &nodev1.ConsoleRequest_Start{Start: &nodev1.ConsoleStart{
		ReadOnly: start.GetReadOnly(), Force: start.GetForce(),
	}}}
	if _, err := protodelim.MarshalTo(conn, helperStart); err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("machine %s's console: %w", uid, err))
	}
	// What the caller sends goes on to the helper until it ends its side.
	go relayToConsole(stream, conn)
	r := bufio.NewReader(conn)
	for {
		res := &nodev1.ConsoleResponse{}
		err := protodelim.UnmarshalOptions{MaxSize: serialmux.MaxMessage}.UnmarshalFrom(r, res)
		if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
			return nil // the VM, or its console, ended
		}
		if err != nil {
			return connect.NewError(connect.CodeUnavailable, fmt.Errorf("machine %s's console: %w", uid, err))
		}
		if why := res.GetEnded(); why != "" {
			return connect.NewError(connect.CodeResourceExhausted, errors.New(why))
		}
		if err := stream.Send(res); err != nil {
			return err
		}
	}
}

// relayToConsole passes what the caller types and its resizes to the
// console, and closes it once the caller is done.
func relayToConsole(stream *connect.BidiStream[nodev1.ConsoleRequest, nodev1.ConsoleResponse], conn net.Conn) {
	defer func() { _ = conn.Close() }()
	for {
		req, err := stream.Receive()
		if err != nil {
			return
		}
		if req.GetStart() != nil {
			return // only the first is a start
		}
		if _, err := protodelim.MarshalTo(conn, req); err != nil {
			return
		}
	}
}
