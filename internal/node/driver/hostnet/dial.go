package hostnet

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Dial connects in the network's namespace, from its gateway's address and
// by its bridge, so the connection is the one the firewall lets through to
// a tap: to port 22 (platformSSH) or one of its edge ports (edgePorts).
func (d *Driver) Dial(ctx context.Context, networkUID string, addr netip.AddrPort) (net.Conn, error) {
	return d.dial(ctx, networkUID, addr, 0)
}

// DialAnyPort connects as Dial does, its socket marked anyPortMark, which
// the firewall lets through to any TCP port of a tap (platformAnyPort).
func (d *Driver) DialAnyPort(ctx context.Context, networkUID string, addr netip.AddrPort) (net.Conn, error) {
	return d.dial(ctx, networkUID, addr, anyPortMark)
}

// dial connects from the network's gateway by its bridge, its socket
// marked mark unless it is 0.
func (d *Driver) dial(ctx context.Context, networkUID string, addr netip.AddrPort, mark int) (net.Conn, error) {
	if err := checkUID(networkUID); err != nil {
		return nil, err
	}
	d.mu.Lock()
	ns, ok, err := openNS(networkUID)
	if err != nil || !ok {
		d.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("network %s has no namespace on the host", networkUID)
		}
		return nil, err
	}
	defer func() { _ = ns.Close() }()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		d.mu.Unlock()
		return nil, err
	}
	gateway, err := gatewayOf(h)
	h.Close()
	d.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("network %s: %w", networkUID, err)
	}
	dialer := &net.Dialer{
		LocalAddr: &net.TCPAddr{IP: gateway.AsSlice()},
		Control: func(_, _ string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, nsBridge)
				if serr == nil && mark != 0 {
					serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, mark)
				}
			}); err != nil {
				return err
			}
			return serr
		},
	}
	var conn net.Conn
	err = inNS(ns, func() error {
		var err error
		conn, err = dialer.DialContext(ctx, "tcp4", addr.String())
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("network %s: %w", networkUID, err)
	}
	return conn, nil
}
