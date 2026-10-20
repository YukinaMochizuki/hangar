package hostnet

import (
	"errors"
	"net"
	"syscall"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// TestFDBChanges: a tap's forwarding entries end as the static one for its
// interface's MAC and the kernel's for the tap's own address, nothing else.
func TestFDBChanges(t *testing.T) {
	const br, tap = 7, 9
	own := net.HardwareAddr{0x5a, 0, 0, 0, 0, 1}
	mac := net.HardwareAddr{2, 0, 0, 0, 0, 2}
	other := net.HardwareAddr{2, 0, 0, 0, 0, 3}
	entry := func(m net.HardwareAddr, state, vlan, master int) netlink.Neigh {
		return netlink.Neigh{LinkIndex: tap, Family: unix.AF_BRIDGE, State: state, HardwareAddr: m, Vlan: vlan, MasterIndex: master}
	}
	kernels := entry(own, netlink.NUD_PERMANENT, 0, br)
	wanted := entry(mac, netlink.NUD_NOARP, 0, br)
	for _, c := range []struct {
		name  string
		have  []netlink.Neigh
		stale []netlink.Neigh
		add   bool
	}{
		{"none", nil, nil, true},
		{"the kernel's alone", []netlink.Neigh{kernels}, nil, true},
		{"as wanted", []netlink.Neigh{kernels, wanted}, nil, false},
		{"learnt rather than static", []netlink.Neigh{entry(mac, netlink.NUD_REACHABLE, 0, br)},
			[]netlink.Neigh{entry(mac, netlink.NUD_REACHABLE, 0, br)}, true},
		// Local, the bridge would deliver the interface's frames to the
		// namespace.
		{"the MAC as the bridge's own", []netlink.Neigh{entry(mac, netlink.NUD_PERMANENT, 0, br)},
			[]netlink.Neigh{entry(mac, netlink.NUD_PERMANENT, 0, br)}, true},
		{"another's MAC", []netlink.Neigh{wanted, entry(other, netlink.NUD_NOARP, 0, br)},
			[]netlink.Neigh{entry(other, netlink.NUD_NOARP, 0, br)}, false},
		{"another's MAC as the bridge's own", []netlink.Neigh{wanted, entry(other, netlink.NUD_PERMANENT, 0, br)},
			[]netlink.Neigh{entry(other, netlink.NUD_PERMANENT, 0, br)}, false},
		{"as wanted, with the kernel's copy on the port's VLAN", []netlink.Neigh{kernels, entry(own, netlink.NUD_PERMANENT, 1, br),
			wanted, entry(mac, netlink.NUD_NOARP, 1, br)}, nil, false},
		{"on a VLAN alone", []netlink.Neigh{entry(mac, netlink.NUD_NOARP, 1, br)}, nil, true},
		{"another's MAC on a VLAN", []netlink.Neigh{wanted, entry(other, netlink.NUD_NOARP, 1, br)},
			[]netlink.Neigh{entry(other, netlink.NUD_NOARP, 1, br)}, false},
		// The tap's own address list, which a dump shows beside the
		// bridge's entries.
		{"the tap's own", []netlink.Neigh{wanted, entry(other, netlink.NUD_PERMANENT, 0, 0)}, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			stale, add := fdbChanges(c.have, br, own, mac)
			if add != c.add {
				t.Errorf("add = %v, want %v", add, c.add)
			}
			if len(stale) != len(c.stale) {
				t.Fatalf("stale = %+v, want %+v", stale, c.stale)
			}
			for i := range stale {
				if stale[i].HardwareAddr.String() != c.stale[i].HardwareAddr.String() || stale[i].State != c.stale[i].State || stale[i].Vlan != c.stale[i].Vlan {
					t.Errorf("stale[%d] = %+v, want %+v", i, stale[i], c.stale[i])
				}
			}
		})
	}
}

// TestDriversEntries: the watch takes every forwarding entry on a tap as a
// change, but the kernel's own for the tap's address, and on the bridge the
// bridge's for its own address and any for a tap's interface's MAC.
func TestDriversEntries(t *testing.T) {
	tap := linkState{name: tapPrefix + "000000000001", mac: "5a:00:00:00:00:01"}
	br := linkState{name: nsBridge, mac: "02:00:00:00:00:ff"}
	n := func(mac string, state, master, family int) netlink.Neigh {
		m, _ := net.ParseMAC(mac)
		return netlink.Neigh{LinkIndex: 3, Family: family, State: state, HardwareAddr: m, MasterIndex: master}
	}
	isTapMAC := func(m net.HardwareAddr) bool { return m.String() == "02:00:00:00:00:03" }
	for _, c := range []struct {
		name  string
		n     netlink.Neigh
		l     linkState
		known bool
		want  bool
	}{
		{"static on a tap", n("02:00:00:00:00:02", netlink.NUD_NOARP, 3, unix.AF_BRIDGE), tap, true, true},
		{"learnt on a tap", n("02:00:00:00:00:02", netlink.NUD_REACHABLE, 3, unix.AF_BRIDGE), tap, true, true},
		{"the kernel's on a tap", n(tap.mac, netlink.NUD_PERMANENT, 3, unix.AF_BRIDGE), tap, true, false},
		{"another local one on a tap", n("02:00:00:00:00:02", netlink.NUD_PERMANENT, 3, unix.AF_BRIDGE), tap, true, true},
		{"on a link not known yet", n("02:00:00:00:00:02", netlink.NUD_NOARP, 3, unix.AF_BRIDGE), linkState{}, false, true},
		{"the bridge's own", n(br.mac, netlink.NUD_PERMANENT, 3, unix.AF_BRIDGE), br, true, true},
		{"another on the bridge", n("02:00:00:00:00:02", netlink.NUD_PERMANENT, 3, unix.AF_BRIDGE), br, true, false},
		{"a tap's interface's MAC on the bridge", n("02:00:00:00:00:03", netlink.NUD_PERMANENT, 3, unix.AF_BRIDGE), br, true, true},
		{"the tap's own list", n("33:33:00:00:00:01", netlink.NUD_PERMANENT, 0, unix.AF_BRIDGE), tap, true, false},
		{"a neighbour of the namespace", n("02:00:00:00:00:02", netlink.NUD_REACHABLE, 0, unix.AF_INET), br, true, false},
	} {
		if got := isDriversEntry(c.n, c.l, c.known, isTapMAC); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestMACTaken: an interface may not take the MAC of another interface of
// its network, in any case, nor its gateway's; another network's is free.
func TestMACTaken(t *testing.T) {
	all := []tap{
		{network: "n1", ok: true, meta: tapMeta{Machine: "m1", Name: "eth0", MAC: "02:00:00:00:00:0a"}},
		{network: "n1", meta: tapMeta{MAC: "02:00:00:00:00:09"}}, // not finished making
	}
	i := func(machine, network, mac string) driver.Interface {
		return driver.Interface{MachineUID: machine, Name: "eth0", NetworkUID: network, MAC: mac}
	}
	for _, c := range []struct {
		name string
		i    driver.Interface
		want bool
	}{
		{"another's", i("m2", "n1", "02:00:00:00:00:0a"), true},
		{"another's, in capitals", i("m2", "n1", "02:00:00:00:00:0A"), true},
		{"the gateway's", i("m2", "n1", gatewayMAC("n1").String()), true},
		{"its own, ensured again", i("m1", "n1", "02:00:00:00:00:0a"), false},
		{"another network's", i("m2", "n2", "02:00:00:00:00:0a"), false},
		{"another network's gateway's", i("m2", "n1", gatewayMAC("n2").String()), false},
		{"a tap's not finished making", i("m2", "n1", "02:00:00:00:00:09"), false},
	} {
		err := macTaken(all, c.i)
		if got := errors.Is(err, driver.ErrMACInUse); got != c.want {
			t.Errorf("%s: %v, want refused %v", c.name, err, c.want)
		}
	}
}

// TestPortFlags: the flags are read from the port's attributes, as the
// kernel sends them, and the flags a tap is set to are the ones wanted.
func TestPortFlags(t *testing.T) {
	attr := func(typ uint16, v byte) syscall.NetlinkRouteAttr {
		return syscall.NetlinkRouteAttr{Attr: syscall.RtAttr{Type: typ}, Value: []byte{v}}
	}
	got := portFlagsOf([]syscall.NetlinkRouteAttr{
		attr(unix.IFLA_BRPORT_LEARNING, 0), attr(unix.IFLA_BRPORT_UNICAST_FLOOD, 0),
		attr(unix.IFLA_BRPORT_MCAST_FLOOD, 1), attr(unix.IFLA_BRPORT_BCAST_FLOOD, 1), attr(unix.IFLA_BRPORT_PROXYARP, 1),
	})
	if got != tapPort {
		t.Errorf("read %+v, want %+v", got, tapPort)
	}
	if d := portFlagsOf([]syscall.NetlinkRouteAttr{attr(unix.IFLA_BRPORT_LEARNING, 1), attr(unix.IFLA_BRPORT_UNICAST_FLOOD, 1),
		attr(unix.IFLA_BRPORT_MCAST_FLOOD, 1), attr(unix.IFLA_BRPORT_BCAST_FLOOD, 1)}); d == tapPort {
		t.Error("a port as the kernel makes it reads as a tap's")
	}
	if tapPort.learning || tapPort.flood || !tapPort.mcastFlood || !tapPort.bcastFlood {
		t.Errorf("a tap is set to %+v", tapPort)
	}
}
