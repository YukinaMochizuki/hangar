package hostnet

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"slices"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// ensurePort makes tap l, a port of bridge br, receive only the unicast for
// mac, its interface's: the port is set as tapPort, and mac is a static
// entry of the bridge's forwarding database on it. Learning is off as it
// would move a static entry to whichever port last sent from its MAC.
func ensurePort(h *netlink.Handle, ports *portSocket, br, l netlink.Link, mac net.HardwareAddr) error {
	name := l.Attrs().Name
	flags, err := ports.flags()
	if err != nil {
		return fmt.Errorf("port flags of %s: %w", name, err)
	}
	if flags[l.Attrs().Index] != tapPort {
		if err := ports.set(l.Attrs().Index, tapPort); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	have, err := dumped(func() ([]netlink.Neigh, error) { return h.NeighList(l.Attrs().Index, unix.AF_BRIDGE) })
	if err != nil {
		return fmt.Errorf("forwarding entries of %s: %w", name, err)
	}
	stale, add := fdbChanges(have, br.Attrs().Index, l.Attrs().HardwareAddr, mac)
	for _, n := range stale {
		del := netlink.Neigh{LinkIndex: n.LinkIndex, Family: unix.AF_BRIDGE, Flags: netlink.NTF_MASTER, HardwareAddr: n.HardwareAddr, Vlan: n.Vlan}
		if err := h.NeighDel(&del); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("forwarding entry for %s on %s: %w", n.HardwareAddr, name, err)
		}
	}
	if !add {
		return nil
	}
	// Static is NUD_NOARP: NUD_PERMANENT would make it the bridge's own
	// address, delivered to the namespace rather than the tap.
	if err := h.NeighSet(&netlink.Neigh{LinkIndex: l.Attrs().Index, Family: unix.AF_BRIDGE, Flags: netlink.NTF_MASTER,
		State: netlink.NUD_NOARP, HardwareAddr: mac}); err != nil {
		return fmt.Errorf("forwarding entry for %s on %s: %w", mac, name, err)
	}
	return nil
}

// macTaken refuses interface i a MAC that another interface of its
// network, among all, or its gateway has: the bridge sends one MAC's
// frames to one port.
func macTaken(all []tap, i driver.Interface) error {
	mac, err := net.ParseMAC(i.MAC)
	if err != nil {
		return err
	}
	if bytes.Equal(mac, gatewayMAC(i.NetworkUID)) {
		return fmt.Errorf("machine %s's %s: %s is network %s's gateway's: %w", i.MachineUID, i.Name, i.MAC, i.NetworkUID, driver.ErrMACInUse)
	}
	for _, t := range all {
		if !t.ok || t.network != i.NetworkUID || t.meta.Machine == i.MachineUID && t.meta.Name == i.Name {
			continue
		}
		if other, err := net.ParseMAC(t.meta.MAC); err == nil && bytes.Equal(other, mac) {
			return fmt.Errorf("machine %s's %s: %s is machine %s's %s's: %w",
				i.MachineUID, i.Name, i.MAC, t.meta.Machine, t.meta.Name, driver.ErrMACInUse)
		}
	}
	return nil
}

// ensureBridgeEntry makes mac, the gateway's, the bridge's own address in
// its forwarding database, as the kernel makes it with the bridge. A flush
// takes it too, and nothing on a tap reaches the gateway until it is back.
func ensureBridgeEntry(h *netlink.Handle, br netlink.Link, mac net.HardwareAddr) error {
	have, err := dumped(func() ([]netlink.Neigh, error) { return h.NeighList(br.Attrs().Index, unix.AF_BRIDGE) })
	if err != nil {
		return fmt.Errorf("forwarding entries of %s: %w", nsBridge, err)
	}
	if slices.ContainsFunc(have, func(n netlink.Neigh) bool { return isBridgeEntry(n, br.Attrs().Index, mac) }) {
		return nil
	}
	if err := h.NeighSet(&netlink.Neigh{LinkIndex: br.Attrs().Index, Family: unix.AF_BRIDGE, Flags: netlink.NTF_SELF,
		State: netlink.NUD_PERMANENT, HardwareAddr: mac}); err != nil {
		return fmt.Errorf("forwarding entry for %s on %s: %w", mac, nsBridge, err)
	}
	return nil
}

// isBridgeEntry says whether n is bridge br's entry for its own address,
// mac.
func isBridgeEntry(n netlink.Neigh, br int, mac net.HardwareAddr) bool {
	return n.LinkIndex == br && n.MasterIndex == br && n.State&netlink.NUD_PERMANENT != 0 && n.Vlan == 0 && bytes.Equal(n.HardwareAddr, mac)
}

// fdbChanges compares a tap's entries in the forwarding database of bridge
// br with the one wanted, a static entry for mac: it returns those to
// delete and whether to add the wanted one. Its copies on the port's VLANs,
// which the kernel adds with it, and the kernel's own entries for the
// tap's address, own, stay; nothing else does.
func fdbChanges(have []netlink.Neigh, br int, own, mac net.HardwareAddr) (stale []netlink.Neigh, add bool) {
	add = true
	for _, n := range have {
		if n.MasterIndex != br {
			continue // the tap's own addresses, not the bridge's database
		}
		switch {
		case isStatic(n) && bytes.Equal(n.HardwareAddr, mac):
			add = add && n.Vlan != 0
		case isKernels(n, own):
		default:
			stale = append(stale, n)
		}
	}
	return stale, add
}

func isStatic(n netlink.Neigh) bool { return n.State == netlink.NUD_NOARP }

// isKernels says whether n is the entry the bridge keeps for a port's own
// address, own, which delivers to the namespace.
func isKernels(n netlink.Neigh, own net.HardwareAddr) bool {
	return n.State&netlink.NUD_PERMANENT != 0 && bytes.Equal(n.HardwareAddr, own)
}
