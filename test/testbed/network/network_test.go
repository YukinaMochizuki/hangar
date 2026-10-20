//go:build testbed

package network

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/drivertest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

// newDriver returns a driver on a host with nothing of the driver's on it,
// and leaves the host that way after the test.
func newDriver(t *testing.T, cfg hostnet.Config) *hostnet.Driver {
	t.Helper()
	wipe(t)
	d, err := hostnet.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The driver itself first: only one with the prefix removes the
		// proxy entries it made.
		nets, _ := d.Networks(context.Background())
		for _, n := range nets {
			_ = d.DeleteNetwork(context.Background(), n.UID)
		}
		_ = d.Close()
		wipe(t)
	})
	return d
}

func wipe(t *testing.T) {
	t.Helper()
	d, err := hostnet.New(hostnet.Config{Uplink: "lo"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	nets, err := d.Networks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nets {
		if err := d.DeleteNetwork(context.Background(), n.UID); err != nil {
			t.Fatal(err)
		}
	}
}

// TestConformance runs the suite's network parts; the rest it skips.
func TestConformance(t *testing.T) {
	drivertest.Run(t, func(t *testing.T) drivertest.Host {
		guests := map[string]*guest{}
		return drivertest.Host{
			Network: newDriver(t, hostnet.Config{Uplink: upstreamLink}),
			ServeGuest: func(t *testing.T, nic driver.Interface, ports ...uint16) {
				ints := make([]int, len(ports))
				for i, p := range ports {
					ints[i] = int(p)
				}
				echoServer(t, newGuest(t, nic), ints...)
			},
			// The guest holds its tap only while it connects, so the suite
			// can delete the interface after.
			Connect: func(t *testing.T, nic driver.Interface, srcPort uint16) (netip.AddrPort, netip.AddrPort) {
				g := guests[nic.MachineUID]
				if g == nil {
					g = newGuest(t, nic)
					guests[nic.MachineUID] = g
				} else {
					g.plug(t)
				}
				defer g.unplug()
				out, err := exec.Command("ip", "netns", "exec", g.ns, "curl", "-sS", "-m", "3", "--local-port", strconv.Itoa(int(srcPort)),
					"http://"+upstreamAddr+":"+sourcePort+"/").CombinedOutput()
				seen, perr := netip.ParseAddrPort(strings.TrimSpace(string(out)))
				if err != nil || perr != nil {
					t.Fatalf("%s: %v %v: %s", g.ns, err, perr, out)
				}
				return netip.MustParseAddrPort(upstreamAddr + ":" + sourcePort), seen
			},
		}
	})
}

// TestConformanceWithIPv6 runs them on a host with an IPv6 prefix.
func TestConformanceWithIPv6(t *testing.T) {
	prefix := netip.MustParsePrefix(hostPrefix)
	drivertest.Run(t, func(t *testing.T) drivertest.Host {
		return drivertest.Host{Network: newDriver(t, hostnet.Config{Uplink: upstreamLink, IPv6Prefix: prefix}), IPv6Prefix: prefix}
	})
}

var (
	netA = driver.NetworkSpec{UID: "net-a", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.0/31")}
	netB = driver.NetworkSpec{UID: "net-b", CIDR: netip.MustParsePrefix("10.1.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.2/31")}
	// netC overlaps netA, as networks of two projects may.
	netC = driver.NetworkSpec{UID: "net-c", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.4/31")}
)

// nic is interface eth0 of machine on n at the host-th address.
func nic(machine string, n driver.NetworkSpec, host int) driver.Interface {
	a := n.CIDR.Addr()
	for range host {
		a = a.Next()
	}
	return driver.Interface{
		MachineUID: machine, Name: "eth0", NetworkUID: n.UID,
		MAC: fmt.Sprintf("02:00:00:00:%02x:%02x", n.Transit.Addr().As4()[3], host), Address: netip.PrefixFrom(a, n.CIDR.Bits()),
		Gateway: n.CIDR.Addr().Next(),
	}
}

// ensure ensures the networks and interfaces, as the agent does on every
// reconcile, and returns the interfaces as made.
func ensure(t *testing.T, d *hostnet.Driver, nets []driver.NetworkSpec, nics ...driver.Interface) []driver.Interface {
	t.Helper()
	for _, n := range nets {
		if err := d.EnsureNetwork(t.Context(), n); err != nil {
			t.Fatal(err)
		}
	}
	var made []driver.Interface
	for _, i := range nics {
		m, err := d.EnsureInterface(t.Context(), i)
		if err != nil {
			t.Fatal(err)
		}
		made = append(made, m)
	}
	openFirewalls(t, d, nets)
	return made
}

// openFirewalls lets every interface of the networks receive and send
// anything, so these tests see the networks' own behaviour: the driver
// lets nothing through a tap it has no policy for.
func openFirewalls(t *testing.T, d *hostnet.Driver, nets []driver.NetworkSpec) {
	t.Helper()
	all, err := d.Interfaces(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	open := secgroup.Policy{Ingress: []secgroup.Rule{{Protocol: secgroup.All, Peers: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}}, EgressAll: true}
	for _, n := range nets {
		var policies []driver.InterfacePolicy
		for _, i := range all {
			if i.NetworkUID == n.UID {
				policies = append(policies, driver.InterfacePolicy{MachineUID: i.MachineUID, Name: i.Name, Policy: open})
			}
		}
		if err := d.EnsureFirewall(t.Context(), n.UID, policies); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTraffic runs stand-in guests on two networks, two each, and one on a
// third whose range overlaps the first's.
func TestTraffic(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	made := ensure(t, d, []driver.NetworkSpec{netA, netB, netC},
		nic("a1", netA, 2), nic("a2", netA, 3), nic("b1", netB, 2), nic("b2", netB, 3), nic("c1", netC, 2))
	logHost(t)
	gs := map[string]*guest{}
	for _, m := range made {
		gs[m.MachineUID] = newGuest(t, m)
	}
	a1, a2, b1, b2, c1 := gs["a1"], gs["a2"], gs["b1"], gs["b2"], gs["c1"]

	t.Run("within a network", func(t *testing.T) {
		for _, p := range [][2]*guest{{a1, a2}, {a2, a1}, {b1, b2}, {b2, b1}} {
			if !p[0].reaches(p[1].nic.Address.Addr().String()) {
				t.Errorf("%s does not reach %s on its network", p[0].ns, p[1].ns)
			}
			if !p[0].reaches(p[0].nic.Gateway.String()) {
				t.Errorf("%s does not reach its gateway", p[0].ns)
			}
		}
	})
	count(t, upstreamNS, "foreign")
	t.Run("out through both NATs, as the host", func(t *testing.T) {
		for _, g := range []*guest{a1, a2, b1, b2, c1} {
			if got, err := g.seenAs(); err != nil || got != hostAddr {
				t.Errorf("%s is seen upstream as %q (%v), want the host's %s", g.ns, got, err, hostAddr)
			}
		}
		noneForeign(t)
	})
	t.Run("networks are isolated", func(t *testing.T) {
		for _, c := range []struct {
			g    *guest
			addr string
			what string
		}{
			{a1, "10.1.0.2", "a guest on another network"},
			{b1, "10.0.0.2", "a guest on another network"},
			{a1, "100.64.0.3", "another network's end of its transit link"},
			{a1, "100.64.0.2", "the host's end of another network's transit link"},
			{a1, "100.64.0.0", "the host's end of its own transit link"},
			{a1, hostAddr, "the host's uplink address"},
		} {
			if c.g.reaches(c.addr) {
				t.Errorf("%s reaches %s, %s", c.g.ns, c.addr, c.what)
			}
		}
		// c1 has a1's address on a network with a1's range; the upstream
		// answers each, and a2 reaches a1 only.
		if !a2.reaches("10.0.0.2") {
			t.Error("a2 lost a1 to c1, which shares its address")
		}
		noneForeign(t)
	})
	t.Run("a tap is held while open and opens again", func(t *testing.T) {
		if err := d.DeleteInterfaces(t.Context(), "a1"); err == nil {
			t.Fatal("deleted the interfaces of a machine whose tap is open")
		}
		run(t, "ip", "-n", hostnet.NamespaceOf(netA.UID), "link", "set", a1.nic.Device, "down")
		if err := d.DeleteInterfaces(t.Context(), "a1"); err == nil {
			t.Fatal("deleted the interfaces of a machine whose tap is open but down")
		}
		a1.unplug()
		if a1.reaches(a2.nic.Address.Addr().String()) {
			t.Error("a1 reaches a2 with its tap closed")
		}
		a1.plug(t)
		if !a1.reaches(a2.nic.Address.Addr().String()) {
			t.Error("a1 does not reach a2 after its tap was opened again")
		}
	})
	t.Run("deleting a network leaves the others working", func(t *testing.T) {
		c1.unplug()
		if err := d.DeleteInterfaces(t.Context(), "c1"); err != nil {
			t.Fatal(err)
		}
		if err := d.DeleteNetwork(t.Context(), netC.UID); err != nil {
			t.Fatal(err)
		}
		for _, g := range []*guest{a1, b1} {
			if got, err := g.seenAs(); err != nil || got != hostAddr {
				t.Errorf("%s is seen upstream as %q (%v) after another network went", g.ns, got, err)
			}
		}
		noneForeign(t)
	})
}

// TestInternet sends the stand-in guests' traffic out of the testbed's real
// uplink: what the internet sees must be what it sees of the host.
func TestInternet(t *testing.T) {
	r, err := netlink.RouteGet([]byte{1, 1, 1, 1})
	if err != nil || len(r) == 0 {
		t.Fatalf("no route to the internet: %v", err)
	}
	up, err := netlink.LinkByIndex(r[0].LinkIndex)
	if err != nil {
		t.Fatal(err)
	}
	d := newDriver(t, hostnet.Config{Uplink: up.Attrs().Name})
	made := ensure(t, d, []driver.NetworkSpec{netA, netB},
		nic("a1", netA, 2), nic("a2", netA, 3), nic("b1", netB, 2), nic("b2", netB, 3))
	host, err := traceIP("")
	if err != nil {
		t.Fatalf("the host itself does not reach the internet: %v", err)
	}
	t.Logf("uplink %s; the internet sees the host as %s", up.Attrs().Name, host)
	var gs []*guest
	for _, m := range made {
		g := newGuest(t, m)
		gs = append(gs, g)
		if got, err := traceIP(g.ns); err != nil || got != host {
			t.Errorf("the internet sees %s as %q (%v), want the host's %s", g.ns, got, err, host)
		}
	}

	// networkd reconfiguring the uplink, as netplan apply does, drops its
	// addresses and routes for a moment.
	t.Run("networkd reconfigures the uplink", func(t *testing.T) {
		if out, err := exec.Command("networkctl", "status", up.Attrs().Name).CombinedOutput(); err != nil ||
			!regexp.MustCompile(`State: routable \(configured`).Match(out) {
			t.Skipf("networkd does not manage %s: %v %s", up.Attrs().Name, err, out)
		}
		settle(d)
		run(t, "networkctl", "reconfigure", up.Attrs().Name)
		select {
		case <-d.Changes():
		case <-time.After(10 * time.Second):
			t.Error("the reconfiguration was never signalled")
		}
		deadline := time.Now().Add(60 * time.Second)
		for {
			if _, err := traceIP(""); err == nil {
				break
			} else if time.Now().After(deadline) {
				t.Fatalf("the host never got back to the internet: %v", err)
			}
			time.Sleep(time.Second)
		}
		ensure(t, d, []driver.NetworkSpec{netA, netB}, made...)
		for _, g := range gs {
			if got, err := traceIP(g.ns); err != nil || got != host {
				t.Errorf("after networkd reconfigured the uplink, the internet sees %s as %q (%v)", g.ns, got, err)
			}
		}
	})
}

// traceIP returns the address Cloudflare sees a request from, made in
// namespace ns or the host's.
func traceIP(ns string) (string, error) {
	args := []string{"curl", "-sS", "-m", "10", "https://1.1.1.1/cdn-cgi/trace"}
	if ns != "" {
		args = append([]string{"ip", "netns", "exec", ns}, args...)
	}
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, out)
	}
	m := regexp.MustCompile(`(?m)^ip=(\S+)$`).FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("no ip= in %s", out)
	}
	return string(m[1]), nil
}

// TestDrift breaks what the driver made behind its back: each break must be
// signalled, and the next ensure must make things work again.
func TestDrift(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	nets := []driver.NetworkSpec{netA}
	nics := []driver.Interface{nic("a1", netA, 2), nic("a2", netA, 3)}
	made := ensure(t, d, nets, nics...)
	a1, a2 := newGuest(t, made[0]), newGuest(t, made[1])
	ns := hostnet.NamespaceOf(netA.UID)
	works := func() error {
		if got, err := a1.seenAs(); err != nil {
			return fmt.Errorf("a1 does not reach the upstream: %w: %s", err, got)
		} else if got != hostAddr {
			return fmt.Errorf("a1 is seen upstream as %s", got)
		}
		if !a1.reaches(a2.nic.Address.Addr().String()) {
			return fmt.Errorf("a1 does not reach a2")
		}
		return nil
	}
	if err := works(); err != nil {
		t.Fatal(err)
	}
	settle(d)
	// Links coming and going in the root namespace that are neither the
	// driver's nor the uplink are not its business.
	run(t, "ip", "link", "add", decoyPrefix+"6", "type", "dummy")
	run(t, "ip", "addr", "add", "192.0.2.1/24", "dev", decoyPrefix+"6")
	run(t, "ip", "link", "set", decoyPrefix+"6", "up")
	run(t, "ip", "link", "del", decoyPrefix+"6")
	select {
	case <-d.Changes():
		t.Error("another link in the root namespace was signalled as a change")
	case <-time.After(time.Second):
	}
	// Nor is a VM starting again, which the agent does: its tap's carrier
	// goes and comes back.
	a1.unplug()
	a1.plug(t)
	select {
	case <-d.Changes():
		t.Error("a tap opened again was signalled as a change")
	case <-time.After(time.Second):
	}

	for _, c := range []struct {
		name string
		do   [][]string
		// breaks says the drift alone stops traffic.
		breaks bool
	}{
		{"veth deleted", [][]string{{"ip", "link", "del", rootVeth(t)}}, true},
		{"veth ends replaced by ends of other pairs", [][]string{
			{"ip", "link", "del", rootVeth(t)},
			{"ip", "link", "add", rootVeth(t), "type", "veth", "peer", "name", decoyPrefix + "4"},
			{"ip", "link", "set", rootVeth(t), "alias", `{"network":"` + netA.UID + `"}`},
			{"ip", "-n", ns, "link", "add", "hn-up", "type", "veth", "peer", "name", decoyPrefix + "5"},
		}, true},
		{"bridge deleted", [][]string{{"ip", "-n", ns, "link", "del", "hn-br"}}, true},
		{"gateway MAC changed", [][]string{{"ip", "-n", ns, "link", "set", "hn-br", "address", "02:00:00:00:ff:ff"}}, true},
		// The namespace keeps the old MAC of the root end, and its frames to it
		// are dropped. The other way, the namespace's next ARP request heals it.
		{"root end of the veth's MAC changed", [][]string{{"ip", "link", "set", rootVeth(t), "address", "02:00:00:00:ff:01"}}, true},
		{"namespace end of the veth's MAC changed", [][]string{{"ip", "-n", ns, "link", "set", "hn-up", "address", "02:00:00:00:ff:02"}}, false},
		{"tap taken off the bridge", [][]string{{"ip", "-n", ns, "link", "set", made[0].Device, "nomaster"}}, true},
		// a1's frames for a2 are then dropped, not flooded.
		{"a tap's forwarding entry deleted", [][]string{{"ip", "netns", "exec", ns, "bridge", "fdb", "del", strings.ToLower(made[1].MAC), "dev", made[1].Device, "master"}}, true},
		{"a tap's MAC moved to another tap", [][]string{{"ip", "netns", "exec", ns, "bridge", "fdb", "replace", strings.ToLower(made[1].MAC), "dev", made[0].Device, "master", "static"}}, true},
		// The bridge's own entry goes too, and with it the gateway.
		{"forwarding database flushed", [][]string{{"ip", "netns", "exec", ns, "bridge", "fdb", "flush", "dev", "hn-br"}}, true},
		// a2's frames then go to the namespace.
		{"a tap's MAC made the bridge's own", [][]string{{"ip", "netns", "exec", ns, "bridge", "fdb", "replace", strings.ToLower(made[1].MAC), "dev", "hn-br", "self", "permanent"}}, true},
		{"broadcast flooding off on a tap", [][]string{{"ip", "-n", ns, "link", "set", made[0].Device, "type", "bridge_slave", "bcast_flood", "off"}}, false},
		{"multicast flooding off on a tap", [][]string{{"ip", "-n", ns, "link", "set", made[0].Device, "type", "bridge_slave", "mcast_flood", "off"}}, false},
		{"learning on a tap", [][]string{{"ip", "-n", ns, "link", "set", made[0].Device, "type", "bridge_slave", "learning", "on"}}, false},
		{"unicast flooding to a tap", [][]string{{"ip", "-n", ns, "link", "set", made[0].Device, "type", "bridge_slave", "flood", "on"}}, false},
		{"default route deleted", [][]string{{"ip", "-n", ns, "route", "del", "default"}}, true},
		{"routes flushed", [][]string{{"ip", "-n", ns, "route", "flush", "table", "main"}}, true},
		{"root routes flushed", [][]string{{"ip", "route", "flush", "dev", rootVeth(t)}}, true},
		{"address deleted", [][]string{{"ip", "-n", ns, "addr", "flush", "dev", "hn-up"}}, true},
		{"root nft table deleted", [][]string{{"nft", "delete", "table", "ip", "hangar"}}, true},
		{"namespace nft table deleted", [][]string{{"ip", "netns", "exec", ns, "nft", "delete", "table", "ip", "hangar"}}, true},
		{"binding table deleted", [][]string{{"ip", "netns", "exec", ns, "nft", "delete", "table", "bridge", "hangar"}}, false},
		{"binding rules flushed", [][]string{{"ip", "netns", "exec", ns, "nft", "flush", "chain", "bridge", "hangar", "prerouting"}}, false},
		{"metadata address deleted", [][]string{{"ip", "-n", ns, "addr", "del", "169.254.169.254/32", "dev", "hn-br"}}, false},
		// Without its firewall a tap is open; made again, the firewall is
		// closed until its policies are ensured again, as ensure does.
		{"firewall table deleted", [][]string{{"ip", "netns", "exec", ns, "nft", "delete", "table", "bridge", "hangar_sg"}}, false},
		// Flushed, a firewall lets everything through; the agent's ensure
		// on the signal writes it whole again.
		{"firewall rules flushed", [][]string{{"ip", "netns", "exec", ns, "nft", "flush", "chain", "bridge", "hangar_sg", "prerouting"}}, false},
		{"root nft rule deleted", [][]string{{"nft", "flush", "chain", "ip", "hangar", "postrouting"}}, true},
		{"namespace end of the veth down", [][]string{{"ip", "-n", ns, "link", "set", "hn-up", "down"}}, true},
		{"root end of the veth down and up", [][]string{{"ip", "link", "set", rootVeth(t), "down"}, {"ip", "link", "set", rootVeth(t), "up"}}, false},
		{"uplink down and up", [][]string{{"ip", "link", "set", upstreamLink, "down"}, {"ip", "link", "set", upstreamLink, "up"}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, args := range c.do {
				run(t, args...)
			}
			select {
			case <-d.Changes():
			case <-time.After(5 * time.Second):
				t.Error("the drift was never signalled")
			}
			if c.breaks {
				if err := works(); err == nil {
					t.Error("traffic works before the driver ensured again; the drift tests nothing")
				}
			}
			ensure(t, d, nets, nics...)
			if err := vethMACs(rootVeth(t), netA); err != nil {
				t.Errorf("after ensuring again: %v", err)
			}
			if rules := run(t, "ip", "netns", "exec", ns, "nft", "list", "table", "bridge", "hangar"); !strings.Contains(rules, a1.nic.Device) ||
				!strings.Contains(rules, a2.nic.Device) {
				t.Errorf("after ensuring again, the taps are not bound:\n%s", rules)
			}
			if rules := run(t, "ip", "netns", "exec", ns, "nft", "list", "chain", "bridge", "hangar_sg", "prerouting"); !strings.Contains(rules, "nothing else") ||
				!strings.Contains(rules, a1.nic.Device) {
				t.Errorf("after ensuring again, the firewall is not whole:\n%s", rules)
			}
			for _, i := range made {
				if err := portWhole(t, i); err != nil {
					t.Errorf("after ensuring again: %v", err)
				}
			}
			if addrs := run(t, "ip", "-n", ns, "-4", "addr", "show", "dev", "hn-br"); !strings.Contains(addrs, "169.254.169.254/32") {
				t.Errorf("after ensuring again, the bridge lacks the metadata address:\n%s", addrs)
			}
			if err := works(); err != nil {
				t.Errorf("after ensuring again: %v", err)
				logHost(t)
				t.Logf("a1's neighbours:\n%s\nthe namespace's:\n%s\nthe root end of the veth:\n%s",
					run(t, "ip", "-n", a1.ns, "neigh"), run(t, "ip", "-n", ns, "neigh"), run(t, "ip", "-o", "link", "show", "dev", rootVeth(t)))
			}
			// Ensuring what is whole changes nothing, so it signals nothing:
			// the agent's ensure on a signal does not signal again.
			settle(d)
			ensure(t, d, nets, nics...)
			select {
			case <-d.Changes():
				t.Error("ensuring what is whole was signalled as a change")
			case <-time.After(time.Second):
			}
		})
	}

	// ip netns del unmounts the namespace and signals nothing. The guests'
	// taps are in the old one, which lives on while they hold it.
	t.Run("namespace deleted", func(t *testing.T) {
		a1.unplug()
		a2.unplug()
		run(t, "ip", "netns", "del", ns)
		made := ensure(t, d, nets, nics...)
		a1.nic, a2.nic = made[0], made[1]
		a1.plug(t)
		a2.plug(t)
		if err := works(); err != nil {
			logHost(t)
			t.Fatalf("in the namespace made again: %v", err)
		}
		// The namespace made again is the one watched: quiet once whole,
		// and a drift in it is signalled.
		settle(d)
		ensure(t, d, nets, nics...)
		select {
		case <-d.Changes():
			t.Fatal("ensuring what is whole was signalled as a change")
		case <-time.After(time.Second):
		}
		run(t, "ip", "-n", ns, "link", "del", "hn-br")
		select {
		case <-d.Changes():
		case <-time.After(5 * time.Second):
			t.Error("a drift in the namespace made again was never signalled")
		}
		ensure(t, d, nets, nics...)
		if err := works(); err != nil {
			t.Error(err)
		}
	})

	// A sysctl changing is not signalled, but the next ensure puts it back.
	t.Run("namespace forwarding off", func(t *testing.T) {
		run(t, "ip", "netns", "exec", ns, "sysctl", "-qw", "net.ipv4.ip_forward=0")
		if err := works(); err == nil {
			t.Error("traffic works with forwarding off; the drift tests nothing")
		}
		ensure(t, d, nets, nics...)
		if err := works(); err != nil {
			t.Error(err)
		}
	})
}

// rootVeth returns the root end of the only network's veth.
func rootVeth(t *testing.T) string {
	t.Helper()
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		if strings.HasPrefix(l.Attrs().Name, "hnv") && l.Type() == "veth" {
			return l.Attrs().Name
		}
	}
	t.Fatal("no veth of the driver's in the root namespace")
	return ""
}

// settle waits for the signals of what just happened, and drops them.
func settle(d *hostnet.Driver) {
	time.Sleep(500 * time.Millisecond)
	select {
	case <-d.Changes():
	default:
	}
}

// firstName is the name the driver gives an object of key when no other
// link has it.
func firstName(prefix, key string) string {
	sum := sha256.Sum256([]byte(key))
	return prefix + hex.EncodeToString(sum[:])[:12]
}

// TestReadBack lists, from a new driver, what an earlier one made.
func TestReadBack(t *testing.T) {
	first := newDriver(t, hostnet.Config{Uplink: upstreamLink})
	made := ensure(t, first, []driver.NetworkSpec{netA, netB},
		nic("a1", netA, 2), nic("a2", netA, 3), nic("b1", netB, 2))
	for _, m := range made {
		if want := firstName("hnt", m.MachineUID+"/"+m.Name); m.Device != want {
			t.Errorf("machine %s's tap is %s, want %s", m.MachineUID, m.Device, want)
		}
	}
	// The veth's ends keep the MACs they were made with, which udev's
	// MACAddressPolicy would otherwise change a moment later.
	time.Sleep(time.Second)
	for _, n := range []driver.NetworkSpec{netA, netB} {
		if err := vethMACs(firstName("hnv", n.UID), n); err != nil {
			t.Error(err)
		}
	}
	_ = first.Close()
	second, err := hostnet.New(hostnet.Config{Uplink: upstreamLink})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	nets, err := second.Networks(t.Context())
	if err != nil || !slices.Equal(nets, []driver.NetworkSpec{netA, netB}) {
		t.Errorf("a new driver lists networks %v %v, want %v", nets, err, []driver.NetworkSpec{netA, netB})
	}
	ifs, err := second.Interfaces(t.Context())
	cmp := func(a, b driver.Interface) int { return strings.Compare(a.MachineUID, b.MachineUID) }
	slices.SortFunc(ifs, cmp)
	slices.SortFunc(made, cmp)
	if err != nil || !slices.Equal(ifs, made) {
		t.Errorf("a new driver lists interfaces %+v %v, want %+v", ifs, err, made)
	}
	// And ensures them as they are, under the same names.
	again := ensure(t, second, nil, nic("a1", netA, 2), nic("a2", netA, 3), nic("b1", netB, 2))
	slices.SortFunc(again, cmp)
	if !slices.Equal(again, made) {
		t.Errorf("a new driver ensured %+v, the first made %+v", again, made)
	}

	// A driver configured otherwise rewrites the root table to its config.
	third, err := hostnet.New(hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = third.Close() }()
	ensure(t, third, []driver.NetworkSpec{netA})
	if table := run(t, "nft", "list", "table", "ip", "hangar"); !strings.Contains(table, "snat to "+hostAddr) {
		t.Errorf("the root table was not rewritten for a public address:\n%s", table)
	}
}

// TestForwardingOff makes no network on a host that would not forward its
// traffic, which deploy/host is to prevent.
func TestForwardingOff(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink})
	run(t, "sysctl", "-qw", "net.ipv4.ip_forward=0")
	defer run(t, "sysctl", "-qw", "net.ipv4.ip_forward=1")
	if err := d.EnsureNetwork(t.Context(), netA); err == nil {
		t.Error("made a network with forwarding off")
	}
}

// TestHalfMade leaves what a driver killed between two steps leaves: veths
// and taps with the prefix but no alias. The next ensure removes them,
// unless held, and links of other types with the prefix are not its.
func TestHalfMade(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink})
	ensure(t, d, []driver.NetworkSpec{netA})
	ns := hostnet.NamespaceOf(netA.UID)
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", "hnvpn0").Run() })
	run(t, "ip", "link", "add", "hnv000000000000", "type", "veth", "peer", "name", decoyPrefix+"3")
	run(t, "ip", "link", "add", "hnvpn0", "type", "dummy")
	// A dummy has carrier once up, as a tap held open; a veth whose peer is
	// down has none, as a tap let go.
	run(t, "ip", "-n", ns, "link", "add", "hnt0dummy0", "type", "dummy")
	run(t, "ip", "-n", ns, "link", "add", "hnt0veth0", "type", "veth", "peer", "name", "hn-x")
	// Nor are taps without the prefix, even there.
	run(t, "ip", "-n", ns, "tuntap", "add", "dev", decoyPrefix+"tap", "mode", "tap")
	run(t, "ip", "-n", ns, "tuntap", "add", "dev", "hnt000000000000", "mode", "tap")
	var helds []*guest
	for i, dev := range []string{"hnt000000000001", "hnt000000000002"} {
		run(t, "ip", "-n", ns, "tuntap", "add", "dev", dev, "mode", "tap")
		run(t, "ip", "-n", ns, "link", "set", dev, "up")
		helds = append(helds, newGuest(t, driver.Interface{
			MachineUID: fmt.Sprintf("held%d", i), Name: "eth0", NetworkUID: netA.UID, MAC: fmt.Sprintf("02:00:00:00:00:%02x", 0x90+i),
			Address: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 0, 0, byte(90 + i)}), 24), Gateway: netip.MustParseAddr("10.0.0.1"), Device: dev,
		}))
	}
	// Held, but down, so it shows no carrier.
	run(t, "ip", "-n", ns, "link", "set", "hnt000000000002", "down")
	ensure(t, d, []driver.NetworkSpec{netA})
	if _, err := netlink.LinkByName("hnv000000000000"); err == nil {
		t.Error("a veth without an alias is left in the root namespace")
	}
	if out, _ := exec.Command("ip", "-n", ns, "link", "show", "hnt000000000000").CombinedOutput(); !strings.Contains(string(out), "does not exist") {
		t.Errorf("a tap without an alias is left: %s", out)
	}
	for _, dev := range []string{"hnt000000000001", "hnt000000000002"} {
		if _, err := exec.Command("ip", "-n", ns, "link", "show", dev).CombinedOutput(); err != nil {
			t.Errorf("tap %s without an alias, which something holds open, was removed", dev)
		}
	}
	if _, err := netlink.LinkByName("hnvpn0"); err != nil {
		t.Error("a dummy link with the veth prefix was removed from the root namespace")
	}
	for _, l := range []string{"hnt0dummy0", "hnt0veth0", decoyPrefix + "tap"} {
		if _, err := exec.Command("ip", "-n", ns, "link", "show", l).CombinedOutput(); err != nil {
			t.Errorf("%s, not a tap of the driver's, was removed", l)
		}
	}
	for _, g := range helds {
		g.unplug()
	}
}

// TestHeldTaps keeps a VM's tap from going when its interface is ensured
// on another network, or with a duplicate the VM holds.
func TestHeldTaps(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink})
	nets := []driver.NetworkSpec{netA, netB}
	made := ensure(t, d, nets, nic("a1", netA, 2), nic("a2", netA, 3))
	a1, a2 := newGuest(t, made[0]), newGuest(t, made[1])
	ns := hostnet.NamespaceOf(netA.UID)

	t.Run("moved to another network", func(t *testing.T) {
		moved := nic("a1", netB, 2)
		if _, err := d.EnsureInterface(t.Context(), moved); err == nil {
			t.Error("moved the interface of a machine whose tap is held open")
		}
		if !a1.reaches(a2.nic.Address.Addr().String()) {
			t.Error("a1 lost its tap")
		}
		a1.unplug()
		now := ensure(t, d, nil, moved)[0]
		ifs, err := d.Interfaces(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range ifs {
			if i.MachineUID == "a1" && i != now {
				t.Errorf("a1's interface on %s is left after it moved", i.NetworkUID)
			}
		}
	})
	t.Run("a duplicate held", func(t *testing.T) {
		dup := "hnt0000000000dd"
		l, err := linkIn(ns, a2.nic.Device)
		if err != nil {
			t.Fatal(err)
		}
		run(t, "ip", "-n", ns, "tuntap", "add", "dev", dup, "mode", "tap")
		run(t, "ip", "-n", ns, "link", "set", dup, "alias", l.Attrs().Alias, "up")
		f, err := hostnet.OpenTap(driver.Interface{NetworkUID: netA.UID, Device: dup})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.EnsureInterface(t.Context(), a2.nic); err == nil {
			t.Error("ensured an interface by removing a duplicate tap held open")
		}
		if _, err := linkIn(ns, dup); err != nil {
			t.Errorf("the duplicate tap held open is gone: %v", err)
		}
		_ = f.Close()
		ensure(t, d, nil, a2.nic)
		if _, err := linkIn(ns, dup); err == nil {
			t.Error("the duplicate tap is left once let go")
		}
		if !a2.reaches(a2.nic.Gateway.String()) {
			t.Error("a2 lost its own tap")
		}
	})
}

// vethMACs returns an error unless the ends of network n's veth, root end
// root, have the MACs the driver derives from n's uid.
func vethMACs(root string, n driver.NetworkSpec) error {
	for _, c := range []struct{ ns, link, key string }{
		{"", root, "veth/host/"}, {hostnet.NamespaceOf(n.UID), "hn-up", "veth/ns/"},
	} {
		l, err := linkIn(c.ns, c.link)
		if err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(c.key + n.UID))
		want := net.HardwareAddr{0x02, sum[0], sum[1], sum[2], sum[3], sum[4]}
		if got := l.Attrs().HardwareAddr; got.String() != want.String() {
			return fmt.Errorf("%s's %s has MAC %s, want %s", n.UID, c.link, got, want)
		}
	}
	return nil
}

// linkIn returns the link named name in namespace ns, or the root one for "".
func linkIn(ns, name string) (netlink.Link, error) {
	if ns == "" {
		return netlink.LinkByName(name)
	}
	h, err := netns.GetFromName(ns)
	if err != nil {
		return nil, err
	}
	defer func() { _ = h.Close() }()
	nh, err := netlink.NewHandleAt(h)
	if err != nil {
		return nil, err
	}
	defer nh.Close()
	return nh.LinkByName(name)
}

// TestRuleExpressions leaves a table of other rules under the same words,
// as a driver of other rules wrote it: the next ensure must write it again.
func TestRuleExpressions(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink})
	nets := []driver.NetworkSpec{netA}
	ensure(t, d, nets)
	body := regexp.MustCompile(`(?m)^(\s+)\S.* (comment "hangar: .*) \[[0-9a-f]+\]"$`)
	for _, in := range [][]string{nil, {"ip", "netns", "exec", hostnet.NamespaceOf(netA.UID)}} {
		nft := func(stdin string, args ...string) string {
			c := exec.Command("nft", args...)
			if in != nil {
				c = exec.Command(in[0], append(append(in[1:], "nft"), args...)...)
			}
			c.Stdin = strings.NewReader(stdin)
			out, err := c.CombinedOutput()
			if err != nil {
				t.Fatalf("%v nft %v: %v: %s", in, args, err, out)
			}
			return string(out)
		}
		want := nft("", "list", "table", "ip", hostnet.Table)
		forged := body.ReplaceAllString(want, `${1}meta l4proto 99 accept ${2}"`)
		if forged == want {
			t.Fatalf("no rule to change in:\n%s", want)
		}
		nft("", "delete", "table", "ip", hostnet.Table)
		nft(forged, "-f", "-")
		ensure(t, d, nets)
		if got := nft("", "list", "table", "ip", hostnet.Table); got != want {
			t.Errorf("%v: rules changed under their comments were left:\n%s\nwant:\n%s", in, got, want)
		}
	}
}

// TestNameCollision gives the first names the driver would pick to others:
// it must name its objects otherwise and still find them.
func TestNameCollision(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink})
	// Network net-b's veth under the name net-a's would have.
	squatter := firstName("hnv", netA.UID)
	run(t, "ip", "link", "add", squatter, "type", "veth", "peer", "name", decoyPrefix+"2")
	run(t, "ip", "link", "set", squatter, "alias", `{"network":"`+netB.UID+`"}`)
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", squatter).Run() })
	// Machine m2's tap under the name m1's would have.
	m2 := ensure(t, d, []driver.NetworkSpec{netA}, nic("m2", netA, 3))[0]
	tapSquatter := firstName("hnt", "m1/eth0")
	ns := hostnet.NamespaceOf(netA.UID)
	run(t, "ip", "-n", ns, "link", "set", m2.Device, "down")
	run(t, "ip", "-n", ns, "link", "set", m2.Device, "name", tapSquatter)

	made := ensure(t, d, []driver.NetworkSpec{netA}, nic("m1", netA, 2))
	if made[0].Device == tapSquatter {
		t.Errorf("m1's tap took the name of m2's, %s", tapSquatter)
	}
	if _, err := netlink.LinkByName(squatter); err != nil {
		t.Errorf("the veth under net-a's first name is gone: %v", err)
	}
	nets, err := d.Networks(t.Context())
	if err != nil || !slices.Contains(nets, netA) {
		t.Errorf("networks %v %v, want net-a under another name", nets, err)
	}
	ifs, err := d.Interfaces(t.Context())
	if err != nil || !slices.Contains(ifs, made[0]) {
		t.Errorf("interfaces %+v %v, want %+v", ifs, err, made[0])
	}
	if again := ensure(t, d, nil, nic("m1", netA, 2)); again[0] != made[0] {
		t.Errorf("ensured again as %+v, made as %+v", again[0], made[0])
	}
	logHost(t)
	// The squatter is net-b's veth without a namespace; deleting net-b
	// takes it.
	if err := d.DeleteNetwork(t.Context(), netB.UID); err != nil {
		t.Fatal(err)
	}
	if _, err := netlink.LinkByName(squatter); err == nil {
		t.Error("deleting net-b left its veth")
	}
}

// decoyPrefix names what the tests make that the driver must leave alone.
const decoyPrefix = "tbdecoy"

// TestLeavesOthersAndNothing runs the driver among decoys that look like
// its objects, then deletes everything: the decoys must be as they were.
func TestLeavesOthersAndNothing(t *testing.T) {
	alias := `{"network":"net-a"}`
	// Of another type than the driver's veths, under their prefix.
	const prefixed = "hnvpn0"
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", prefixed).Run() })
	for _, args := range [][]string{
		{"ip", "link", "add", prefixed, "type", "dummy"},
		{"ip", "netns", "add", decoyPrefix},
		{"ip", "-n", decoyPrefix, "link", "add", "hn-br", "type", "bridge"},
		{"ip", "-n", decoyPrefix, "link", "add", "hnt000000000000", "type", "dummy"},
		{"ip", "link", "add", decoyPrefix + "0", "type", "dummy"},
		{"ip", "link", "set", decoyPrefix + "0", "alias", alias},
		{"ip", "link", "add", decoyPrefix + "1", "type", "veth", "peer", "name", "hn-up", "netns", decoyPrefix},
		{"nft", "add", "table", "ip", decoyPrefix},
		{"nft", "add", "chain", "ip", decoyPrefix, "post", "{ type nat hook postrouting priority srcnat; }"},
		{"nft", "add", "rule", "ip", decoyPrefix, "post", "ip", "saddr", "100.64.0.0/10", "masquerade"},
		{"nft", "add", "table", "inet", decoyPrefix},
		{"ip", "netns", "exec", decoyPrefix, "nft", "add", "table", "ip", "hangar"},
	} {
		run(t, args...)
	}
	decoys := func() string {
		return run(t, "ip", "-d", "-o", "link", "show", "dev", decoyPrefix+"0") +
			run(t, "ip", "-d", "-o", "link", "show", "dev", prefixed) +
			run(t, "ip", "-n", decoyPrefix, "-d", "-o", "link", "show") +
			run(t, "nft", "list", "table", "ip", decoyPrefix) +
			run(t, "nft", "list", "table", "inet", decoyPrefix) +
			run(t, "ip", "netns", "exec", decoyPrefix, "nft", "list", "ruleset")
	}
	before := decoys()
	t.Logf("host before:\n%s", hostListing(t))

	d := newDriver(t, hostnet.Config{Uplink: upstreamLink})
	nets := []driver.NetworkSpec{netA, netB}
	nics := []driver.Interface{nic("a1", netA, 2), nic("b1", netB, 2)}
	// What the driver holds for a network, it lets go of with it; the
	// handle below is the one more file open at the end.
	fdsBefore := openFDs(t) + 1
	ensure(t, d, nets, nics...)
	// Something holding a namespace keeps it after it is deleted: it must
	// be empty by then.
	held, err := netns.GetFromName(hostnet.NamespaceOf(netA.UID))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	run(t, "nft", "delete", "table", "ip", "hangar")
	run(t, "ip", "link", "del", rootVeth(t))
	ensure(t, d, nets, nics...)
	t.Logf("host with two networks:\n%s", hostListing(t))
	for _, i := range nics {
		if err := d.DeleteInterfaces(t.Context(), i.MachineUID); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range nets {
		if err := d.DeleteNetwork(t.Context(), n.UID); err != nil {
			t.Fatal(err)
		}
	}

	after := hostListing(t)
	t.Logf("host after:\n%s", after)
	if nets, err := d.Networks(t.Context()); err != nil || len(nets) != 0 {
		t.Errorf("networks listed after deleting them all: %v %v", nets, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for n := openFDs(t); n > fdsBefore; n = openFDs(t) {
		if time.Now().After(deadline) {
			t.Errorf("%d files open after deleting the networks, %d with them", n, fdsBefore)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if after := decoys(); after != before {
		t.Errorf("the decoys changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if left := ours(t); !slices.Equal(left, []string{"link " + prefixed}) {
		t.Errorf("left on the host: %v, want only the decoy %s", left, prefixed)
	}
	h, err := netlink.NewHandleAt(held)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if links, err := h.LinkList(); err != nil || len(links) != 1 {
		t.Errorf("a deleted namespace still held has links %v %v, want only lo", links, err)
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(fds)
}

// ours lists the driver's in the root namespace and the namespace
// directory: links and namespaces with its prefix, and its nft table.
func ours(t *testing.T) []string {
	t.Helper()
	var left []string
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		if strings.HasPrefix(l.Attrs().Name, hostnet.Prefix) {
			left = append(left, "link "+l.Attrs().Name)
		}
	}
	entries, _ := os.ReadDir("/run/netns")
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), hostnet.Prefix) {
			left = append(left, "netns "+e.Name())
		}
	}
	for _, line := range strings.Split(run(t, "nft", "list", "tables"), "\n") {
		if line == "table ip "+hostnet.Table {
			left = append(left, line)
		}
	}
	return left
}

// hostListing is what an operator would look at.
func hostListing(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, args := range [][]string{
		{"ip", "netns", "list"},
		{"ip", "-br", "link"},
		{"nft", "list", "tables"},
	} {
		fmt.Fprintf(&b, "$ %s\n%s", strings.Join(args, " "), run(t, args...))
	}
	return b.String()
}

func logHost(t *testing.T) {
	t.Helper()
	var b strings.Builder
	b.WriteString(hostListing(t))
	if _, err := os.Stat("/run/netns/" + hostnet.NamespaceOf(netA.UID)); err == nil {
		ns := hostnet.NamespaceOf(netA.UID)
		for _, args := range [][]string{
			{"ip", "-n", ns, "-br", "addr"},
			{"ip", "-n", ns, "route"},
			{"ip", "-n", ns, "-d", "link", "show", "master", "hn-br"},
			{"ip", "netns", "exec", ns, "nft", "list", "ruleset"},
		} {
			fmt.Fprintf(&b, "$ %s\n%s", strings.Join(args, " "), run(t, args...))
		}
	}
	fmt.Fprintf(&b, "$ nft list table ip hangar\n%s", run(t, "nft", "list", "table", "ip", "hangar"))
	t.Logf("host:\n%s", b.String())
}
