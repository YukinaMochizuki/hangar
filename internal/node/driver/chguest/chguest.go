// Package chguest is the Guest driver for Cloud Hypervisor: it reaches
// hangar-init over the VM's hybrid vsock, the unix socket on which Cloud
// Hypervisor connects the host to a vsock port in the guest.
package chguest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1/guestv1connect"
	"github.com/YukinaMochizuki/hangar/internal/guest/agent"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// Guest reaches each machine's guest agent.
type Guest struct {
	// Socket is the hybrid vsock socket of the machine's VM, as the
	// hypervisor driver started it.
	Socket func(machineUID string) string
}

var _ driver.Guest = Guest{}

// Ready pings the guest agent.
func (g Guest) Ready(ctx context.Context, machineUID string) error {
	c, done := g.Client(machineUID)
	defer done()
	_, err := c.Ping(ctx, connect.NewRequest(&guestv1.PingRequest{}))
	return err
}

// Stop asks the guest agent to stop the container's process.
func (g Guest) Stop(ctx context.Context, machineUID string, grace time.Duration) error {
	c, done := g.Client(machineUID)
	defer done()
	_, err := c.Stop(ctx, connect.NewRequest(&guestv1.StopRequest{Grace: durationpb.New(grace)}))
	return err
}

// Client is a client of the machine's guest agent, bounding what it takes
// from the guest; done closes its connections once idle. It dials when first
// used, and again whenever the connection is lost, as when the VM restarts.
func (g Guest) Client(machineUID string) (c guestv1connect.GuestAgentServiceClient, done func()) {
	socket := g.Socket(machineUID)
	protocols := &http.Protocols{}
	protocols.SetUnencryptedHTTP2(true)
	tr := &http.Transport{
		Protocols: protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return Dial(ctx, socket, agent.Port)
		},
		MaxResponseHeaderBytes: 64 << 10,
		IdleConnTimeout:        time.Minute,
	}
	inner := guestv1connect.NewGuestAgentServiceClient(&http.Client{Transport: tr}, "http://guest",
		connect.WithReadMaxBytes(agent.MaxMessage))
	return client{inner}, tr.CloseIdleConnections
}

type client struct {
	guestv1connect.GuestAgentServiceClient
}

// Exec ends the call in the guest, and with it the process, once ctx ends.
// Cancelling alone would not tell the guest: HTTP/2 resets the stream only
// when the response is closed. Pass the exec's own ctx, not a long-lived one.
func (c client) Exec(ctx context.Context) *connect.BidiStreamForClient[guestv1.ExecRequest, guestv1.ExecResponse] {
	s := c.GuestAgentServiceClient.Exec(ctx)
	context.AfterFunc(ctx, func() { _ = s.CloseResponse() })
	return s
}

// Dial connects to vsock port in the guest through Cloud Hypervisor's
// hybrid vsock socket: the host writes "CONNECT <port>" and Cloud
// Hypervisor answers "OK <host port>" once the guest accepts.
func Dial(ctx context.Context, socket string, port uint32) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// Cancelling ctx ends a handshake the guest never answers.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// Read the answer a byte at a time: whatever follows it is the guest's.
	line, err := readLine(conn)
	if err != nil {
		_ = conn.Close()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("vsock port %d: %w", port, err)
	}
	if !strings.HasPrefix(line, "OK ") {
		_ = conn.Close()
		return nil, fmt.Errorf("vsock port %d: refused: %q", port, line)
	}
	if !stop() {
		_ = conn.Close()
		return nil, ctx.Err()
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func readLine(conn net.Conn) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for b.Len() < 64 {
		if _, err := conn.Read(buf); err != nil {
			return "", err
		}
		if buf[0] == '\n' {
			return b.String(), nil
		}
		b.WriteByte(buf[0])
	}
	return "", fmt.Errorf("no answer line: %q", b.String())
}
