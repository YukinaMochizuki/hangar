// Package chguest is the Guest driver for Cloud Hypervisor: it reaches
// hangar-init over the VM's hybrid vsock, the unix socket on which Cloud
// Hypervisor connects the host to a vsock port in the guest.
package chguest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sys/unix"
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

// SetIPv6 has the guest agent change the interfaces' public IPv6.
func (g Guest) SetIPv6(ctx context.Context, machineUID string, ifaces []driver.GuestIPv6) error {
	c, done := g.Client(machineUID)
	defer done()
	req := &guestv1.SetIPv6Request{}
	for _, i := range ifaces {
		nic := &guestv1.Interface{Mac: i.MAC}
		if i.Address.IsValid() {
			nic.Ipv6 = netip.PrefixFrom(i.Address, 128).String()
		}
		if i.Gateway.IsValid() {
			nic.Ipv6Gateway = i.Gateway.String()
		}
		req.Interfaces = append(req.Interfaces, nic)
	}
	_, err := c.SetIPv6(ctx, connect.NewRequest(req))
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return fmt.Errorf("%w: %w", driver.ErrGuestCannot, err)
	}
	return err
}

// RootUsage asks the guest agent how full the root filesystem is.
func (g Guest) RootUsage(ctx context.Context, machineUID string) (driver.FSUsage, error) {
	c, done := g.Client(machineUID)
	defer done()
	res, err := c.RootUsage(ctx, connect.NewRequest(&guestv1.RootUsageRequest{}))
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return driver.FSUsage{}, fmt.Errorf("%w: %w", driver.ErrGuestCannot, err)
	}
	if err != nil {
		return driver.FSUsage{}, err
	}
	return driver.FSUsage{Size: res.Msg.GetSize(), Used: res.Msg.GetUsed(), Files: res.Msg.GetFiles(), FilesUsed: res.Msg.GetFilesUsed()}, nil
}

// MemoryUsage asks the guest agent how much memory it has and how much is
// available.
func (g Guest) MemoryUsage(ctx context.Context, machineUID string) (driver.GuestMemory, error) {
	c, done := g.Client(machineUID)
	defer done()
	res, err := c.MemoryUsage(ctx, connect.NewRequest(&guestv1.MemoryUsageRequest{}))
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return driver.GuestMemory{}, fmt.Errorf("%w: %w", driver.ErrGuestCannot, err)
	}
	if err != nil {
		return driver.GuestMemory{}, err
	}
	return driver.GuestMemory{Total: res.Msg.GetTotal(), Available: res.Msg.GetAvailable()}, nil
}

// GrowRoot has the guest agent grow the root filesystem to its disk.
func (g Guest) GrowRoot(ctx context.Context, machineUID string) error {
	c, done := g.Client(machineUID)
	defer done()
	_, err := c.GrowRoot(ctx, connect.NewRequest(&guestv1.GrowRootRequest{}))
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return fmt.Errorf("%w: %w", driver.ErrGuestCannot, err)
	}
	return err
}

// VolumeUsage asks the guest agent how full the filesystem of the volume
// on device is.
func (g Guest) VolumeUsage(ctx context.Context, machineUID, device string) (driver.FSUsage, error) {
	c, done := g.Client(machineUID)
	defer done()
	res, err := c.VolumeUsage(ctx, connect.NewRequest(&guestv1.VolumeUsageRequest{Device: device}))
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return driver.FSUsage{}, fmt.Errorf("%w: %w", driver.ErrGuestCannot, err)
	}
	if err != nil {
		return driver.FSUsage{}, err
	}
	return driver.FSUsage{Size: res.Msg.GetSize(), Used: res.Msg.GetUsed(), Files: res.Msg.GetFiles(), FilesUsed: res.Msg.GetFilesUsed()}, nil
}

// GrowVolume has the guest agent grow the filesystem of the volume on
// device to its disk.
func (g Guest) GrowVolume(ctx context.Context, machineUID, device string) error {
	c, done := g.Client(machineUID)
	defer done()
	_, err := c.GrowVolume(ctx, connect.NewRequest(&guestv1.GrowVolumeRequest{Device: device}))
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return fmt.Errorf("%w: %w", driver.ErrGuestCannot, err)
	}
	return err
}

// SetTailnet has the guest agent join the tailnet as ts says, or leave it.
func (g Guest) SetTailnet(ctx context.Context, machineUID string, ts *driver.TailscaleSpec) error {
	c, done := g.Client(machineUID)
	defer done()
	req := &guestv1.SetTailnetRequest{}
	if ts != nil {
		req.Tailscale = &guestv1.Tailscale{Hostname: ts.Hostname, AcceptRoutes: ts.AcceptRoutes}
	}
	_, err := c.SetTailnet(ctx, connect.NewRequest(req))
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return fmt.Errorf("%w: %w", driver.ErrGuestCannot, err)
	}
	return err
}

// TailnetStatus asks the guest agent where its tailnet join stands.
func (g Guest) TailnetStatus(ctx context.Context, machineUID string) (driver.TailnetStatus, error) {
	c, done := g.Client(machineUID)
	defer done()
	res, err := c.TailnetStatus(ctx, connect.NewRequest(&guestv1.TailnetStatusRequest{}))
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return driver.TailnetStatus{}, fmt.Errorf("%w: %w", driver.ErrGuestCannot, err)
	}
	if err != nil {
		return driver.TailnetStatus{}, err
	}
	st := driver.TailnetStatus{Message: res.Msg.GetMessage(), WantsKey: res.Msg.GetWantsKey()}
	switch res.Msg.GetState() {
	case guestv1.TailnetState_TAILNET_STATE_NEEDS_KEY:
		st.State = driver.TailnetNeedsKey
	case guestv1.TailnetState_TAILNET_STATE_JOINED:
		st.State = driver.TailnetJoined
	case guestv1.TailnetState_TAILNET_STATE_KEY_REJECTED:
		st.State = driver.TailnetKeyRejected
	case guestv1.TailnetState_TAILNET_STATE_FAILED:
		st.State = driver.TailnetFailed
	case guestv1.TailnetState_TAILNET_STATE_OFF:
		st.State = driver.TailnetOff
	default:
		st.State = driver.TailnetStarting
	}
	return st, nil
}

// JoinTailnet gives the guest agent the auth key to join its tailnet with.
func (g Guest) JoinTailnet(ctx context.Context, machineUID, authKey string) error {
	c, done := g.Client(machineUID)
	defer done()
	_, err := c.JoinTailnet(ctx, connect.NewRequest(&guestv1.JoinTailnetRequest{AuthKey: authKey}))
	switch connect.CodeOf(err) {
	case connect.CodeUnimplemented:
		return fmt.Errorf("%w: %w", driver.ErrGuestCannot, err)
	case connect.CodeFailedPrecondition:
		return fmt.Errorf("%w: %w", driver.ErrTailnetHasState, err)
	case connect.CodeAborted:
		return fmt.Errorf("%w: %w", driver.ErrTailnetBusy, err)
	}
	return err
}

// SetExport replaces the export of a FileShare's server, which its
// hangar-init serves beside the guest agent.
func (g Guest) SetExport(ctx context.Context, machineUID string, export driver.ShareExport) error {
	socket := g.Socket(machineUID)
	tr := transport(func(ctx context.Context) (net.Conn, error) { return Dial(ctx, socket, agent.Port) })
	defer tr.CloseIdleConnections()
	c := guestv1connect.NewFileShareServerServiceClient(&http.Client{Transport: tr}, "http://guest", connect.WithReadMaxBytes(agent.MaxMessage))
	_, err := c.SetExport(ctx, connect.NewRequest(&guestv1.SetExportRequest{Export: ExportOf(export)}))
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return fmt.Errorf("%w: %w", driver.ErrGuestCannot, err)
	}
	return err
}

// ListDir lists a directory of the share a FileShare's server serves.
func (g Guest) ListDir(ctx context.Context, machineUID string, req driver.ShareListDir) (driver.ShareDirPage, error) {
	socket := g.Socket(machineUID)
	tr := transport(func(ctx context.Context) (net.Conn, error) { return Dial(ctx, socket, agent.Port) })
	defer tr.CloseIdleConnections()
	c := guestv1connect.NewFileShareServerServiceClient(&http.Client{Transport: tr}, "http://guest", connect.WithReadMaxBytes(agent.MaxMessage))
	res, err := c.ListDir(ctx, connect.NewRequest(&guestv1.ListDirRequest{Path: req.Path, After: req.After, Limit: req.Limit}))
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return driver.ShareDirPage{}, fmt.Errorf("%w: %w", driver.ErrGuestCannot, err)
	}
	if err != nil {
		return driver.ShareDirPage{}, err
	}
	page := driver.ShareDirPage{More: res.Msg.GetMore()}
	for _, e := range res.Msg.GetEntries() {
		page.Entries = append(page.Entries, driver.ShareDirEntry{Name: e.GetName(), Type: shareFileType(e.GetType()), Size: e.GetSize(), ModTime: e.GetModifyTime().AsTime()})
	}
	return page, nil
}

func shareFileType(t guestv1.FileType) driver.ShareFileType {
	switch t {
	case guestv1.FileType_FILE_TYPE_REGULAR:
		return driver.ShareFileRegular
	case guestv1.FileType_FILE_TYPE_DIRECTORY:
		return driver.ShareFileDirectory
	case guestv1.FileType_FILE_TYPE_SYMLINK:
		return driver.ShareFileSymlink
	}
	return driver.ShareFileOther
}

// ExportOf is export as a share's server takes it, on its boot disk or by
// SetExport.
func ExportOf(export driver.ShareExport) *guestv1.FileShareExport {
	out := &guestv1.FileShareExport{Squash: guestv1.FileShareSquash(export.Squash), AnonUid: export.AnonUID, AnonGid: export.AnonGID}
	for _, c := range export.Clients {
		out.Clients = append(out.Clients, &guestv1.FileShareClient{Address: c.Address.String(), ReadOnly: c.ReadOnly})
	}
	return out
}

// Client is a client of the machine's guest agent, bounding what it takes
// from the guest; done closes its connections once idle. It dials when first
// used, and again whenever the connection is lost, as when the VM restarts.
func (g Guest) Client(machineUID string) (c guestv1connect.GuestAgentServiceClient, done func()) {
	socket := g.Socket(machineUID)
	tr := transport(func(ctx context.Context) (net.Conn, error) { return Dial(ctx, socket, agent.Port) })
	return newClient(tr), tr.CloseIdleConnections
}

func transport(dial func(context.Context) (net.Conn, error)) *http.Transport {
	protocols := &http.Protocols{}
	protocols.SetUnencryptedHTTP2(true)
	return &http.Transport{
		Protocols:              protocols,
		DialContext:            func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		MaxResponseHeaderBytes: 64 << 10,
		IdleConnTimeout:        time.Minute,
	}
}

func newClient(tr *http.Transport) client {
	return client{guestv1connect.NewGuestAgentServiceClient(&http.Client{Transport: tr}, "http://guest",
		connect.WithReadMaxBytes(agent.MaxMessage))}
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
	conn, err := DialSocket(ctx, socket)
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

// ErrNotSocket is what Dial and DialSocket say of a socket path that holds
// something else.
var ErrNotSocket = errors.New("not a socket")

// DialSocket connects to the unix socket at path, and to nothing else: Cloud
// Hypervisor makes the socket in a directory it can write in, so a link
// there could lead its caller, root, to any socket of the host.
func DialSocket(ctx context.Context, path string) (net.Conn, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return nil, fmt.Errorf("%s: %w", path, ErrNotSocket)
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", fmt.Sprintf("/proc/self/fd/%d", fd))
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
