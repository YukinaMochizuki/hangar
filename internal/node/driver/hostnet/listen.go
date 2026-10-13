package hostnet

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func (d *Driver) ListenUDP(ctx context.Context, networkUID string, addr netip.AddrPort) (*net.UDPConn, string, error) {
	c, instance, err := d.listen(ctx, networkUID, func(ctx context.Context, lc *net.ListenConfig) (any, error) {
		return lc.ListenPacket(ctx, "udp4", addr.String())
	})
	if err != nil {
		return nil, "", err
	}
	return c.(*net.UDPConn), instance, nil
}

func (d *Driver) ListenTCP(ctx context.Context, networkUID string, addr netip.AddrPort) (*net.TCPListener, string, error) {
	l, instance, err := d.listen(ctx, networkUID, func(ctx context.Context, lc *net.ListenConfig) (any, error) {
		return lc.Listen(ctx, "tcp4", addr.String())
	})
	if err != nil {
		return nil, "", err
	}
	return l.(*net.TCPListener), instance, nil
}

// listen opens a socket on a thread in the network's namespace, bound to its
// bridge before the address, so that nothing arriving by another link of the
// namespace, such as its uplink, reaches the socket.
func (d *Driver) listen(ctx context.Context, uid string, open func(context.Context, *net.ListenConfig) (any, error)) (any, string, error) {
	if err := checkUID(uid); err != nil {
		return nil, "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	ns, ok, err := openNS(uid)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "", fmt.Errorf("network %s has no namespace on the host", uid)
	}
	defer func() { _ = ns.Close() }()
	instance, err := instanceOf(ns)
	if err != nil {
		return nil, "", err
	}
	if instance == "" {
		return nil, "", fmt.Errorf("network %s has no bridge", uid)
	}
	lc := &net.ListenConfig{Control: func(network, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, nsBridge)
			if serr == nil && strings.HasPrefix(network, "udp") {
				// Some answer clients that have no address yet, by
				// broadcast, as DHCP does.
				serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
			}
		})
		if err != nil {
			return err
		}
		return serr
	}}
	var sock any
	err = inNS(ns, func() error {
		var err error
		sock, err = open(ctx, lc)
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("network %s: %w", uid, err)
	}
	return sock, instance, nil
}

func (d *Driver) Instance(_ context.Context, networkUID string) (string, error) {
	if checkUID(networkUID) != nil {
		return "", nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	ns, ok, err := openNS(networkUID)
	if err != nil || !ok {
		return "", err
	}
	defer func() { _ = ns.Close() }()
	return instanceOf(ns)
}

// instanceOf names what a network's sockets are bound to: its namespace, by
// its nsfs inode, and its bridge, by its index, which a bridge made again
// does not keep. It is "" while the namespace has no bridge.
//
// The kernel may give a freed inode to a later namespace, but a socket holds
// the namespace it was opened in, so while a socket lives no namespace made
// since can share its instance; a namespace hands out link indexes in turn,
// without reuse until they wrap.
func instanceOf(ns netns.NsHandle) (string, error) {
	id, err := nsID(ns)
	if err != nil {
		return "", err
	}
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return "", err
	}
	defer h.Close()
	br, err := linkByName(h, nsBridge)
	if err != nil || br == nil {
		return "", err
	}
	return strconv.FormatUint(id, 10) + "/" + strconv.Itoa(br.Attrs().Index), nil
}
