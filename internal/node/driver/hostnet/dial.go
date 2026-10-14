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

// sshPort is the one port DialSSH reaches.
const sshPort = 22

// DialSSH connects in the network's namespace, from its gateway's address
// and by its bridge, so the connection is the one the firewall lets through
// to a tap (platformSSH).
func (d *Driver) DialSSH(ctx context.Context, networkUID string, addr netip.Addr) (net.Conn, error) {
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
			}); err != nil {
				return err
			}
			return serr
		},
	}
	var conn net.Conn
	err = inNS(ns, func() error {
		var err error
		conn, err = dialer.DialContext(ctx, "tcp4", netip.AddrPortFrom(addr, sshPort).String())
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("network %s: %w", networkUID, err)
	}
	return conn, nil
}
