package hostnet

import (
	"fmt"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// OpenTap opens the tap of an interface the driver made, single-queue with
// virtio-net headers, for its VM; the file works from any namespace.
func OpenTap(i driver.Interface) (*os.File, error) {
	if err := checkUID(i.NetworkUID); err != nil {
		return nil, err
	}
	ns, ok, err := openNS(i.NetworkUID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("tap %s: network %s: %w", i.Device, i.NetworkUID, errNoNetwork)
	}
	defer func() { _ = ns.Close() }()
	fd := -1
	err = inNS(ns, func() error {
		// Opening a name no tap has would make a new tap, on no bridge.
		if l, err := netlink.LinkByName(i.Device); err != nil {
			return err
		} else if l.Type() != "tuntap" {
			return fmt.Errorf("%s is a %s, not a tap", i.Device, l.Type())
		}
		f, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		ifr, err := unix.NewIfreq(i.Device)
		if err == nil {
			ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR)
			err = unix.IoctlIfreq(f, unix.TUNSETIFF, ifr)
		}
		if err != nil {
			_ = unix.Close(f)
			return err
		}
		fd = f
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("opening tap %s: %w", i.Device, err)
	}
	return os.NewFile(uintptr(fd), "/dev/net/tun:"+i.Device), nil
}
