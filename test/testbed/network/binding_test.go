//go:build testbed

package network

import (
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
)

// TestBinding: a guest sends only as its own interface, even inside its own
// network: not from another address of the range, not as its neighbour,
// not from another MAC; and an interface deleted takes its binding along.
func TestBinding(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	made := ensure(t, d, []driver.NetworkSpec{netA}, nic("b1", netA, 2), nic("b2", netA, 3))
	b1, b2 := newGuest(t, made[0]), newGuest(t, made[1])
	nsA := hostnet.NamespaceOf(netA.UID)
	for _, ns := range []string{b2.ns, nsA} {
		if err := addCounters(ns); err != nil {
			t.Fatal(err)
		}
		count(t, ns, "marked-request")
	}
	b2Addr, gw := b2.nic.Address.Addr().String(), b1.nic.Gateway.String()

	pingAs := func(dst string) {
		t.Helper()
		_ = exec.Command("ip", "netns", "exec", b1.ns, "ping", "-c", "3", "-i", "0.2", "-W", "1", "-s", markSize, dst).Run()
		time.Sleep(200 * time.Millisecond)
	}
	pingAs(b2Addr)
	if n := count(t, b2.ns, "marked-request"); n == 0 {
		t.Fatal("b1 as itself does not reach b2: the test sees nothing")
	}

	b1.sendAs(t, "10.0.0.9", b2Addr)
	if n := count(t, b2.ns, "marked-request"); n > 0 {
		t.Errorf("b1 as 10.0.0.9, an address of its range but not its own, reached b2 (%d pings)", n)
	}
	b1.sendAs(t, b2Addr, gw)
	if n := count(t, nsA, "marked-request"); n > 0 {
		t.Errorf("b1 as b2 reached the gateway (%d pings)", n)
	}

	run(t, "ip", "-n", b1.ns, "link", "set", "eth0", "address", "02:00:00:00:99:99")
	run(t, "ip", "-n", b1.ns, "neigh", "flush", "dev", "eth0")
	pingAs(b2Addr)
	if n := count(t, b2.ns, "marked-request"); n > 0 {
		t.Errorf("b1 from another MAC reached b2 (%d pings)", n)
	}
	run(t, "ip", "-n", b1.ns, "link", "set", "eth0", "address", b1.nic.MAC)

	// b1 saying by ARP that b2's address is at its own MAC poisons nothing;
	// without the binding it would.
	mac1, _ := net.ParseMAC(b1.nic.MAC)
	b2IP := b2.nic.Address.Addr()
	claim := ethernet(net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, mac1, 0x0806, arpReply(mac1, b2IP, b2IP))
	neighbour := func() string {
		run(t, "ip", "netns", "exec", nsA, "ping", "-c", "1", "-W", "1", b2Addr)
		return run(t, "ip", "-n", nsA, "neigh", "show", b2Addr)
	}
	poisoned := func() bool {
		// The kernel ignores ARP about an entry it learnt within its
		// locktime, a second by default.
		time.Sleep(1500 * time.Millisecond)
		sendFrame(t, b1, claim)
		time.Sleep(200 * time.Millisecond)
		return strings.Contains(run(t, "ip", "-n", nsA, "neigh", "show", b2Addr), b1.nic.MAC)
	}
	if n := neighbour(); !strings.Contains(n, b2.nic.MAC) {
		t.Fatalf("the namespace does not know b2 at its MAC: %s", n)
	}
	if poisoned() {
		t.Error("b1's ARP claiming b2's address poisoned the namespace")
	}
	run(t, "ip", "netns", "exec", nsA, "nft", "flush", "chain", "bridge", hostnet.Table, "prerouting")
	if !poisoned() {
		t.Error("without the binding, b1's ARP did not poison the namespace; the test tests nothing")
	}
	ensure(t, d, []driver.NetworkSpec{netA}, made...)
	run(t, "ip", "-n", nsA, "neigh", "flush", "dev", "hn-br")

	// A frame to an 802.1D group address is passed up the tap's own stack,
	// past the bridge's hooks: as b2 it reaches nothing of the namespace.
	group := ethernet(net.HardwareAddr{0x01, 0x80, 0xc2, 0, 0, 0x0e}, mac1, 0x0800, markedPing(b2IP, b1.nic.Gateway))
	sendFrame(t, b1, group)
	time.Sleep(200 * time.Millisecond)
	if n := count(t, nsA, "marked-request"); n > 0 {
		t.Errorf("b1 as b2, to a group address, reached the namespace's stack (%d pings)", n)
	}
	run(t, "ip", "netns", "exec", nsA, "nft", "flush", "chain", "ip", hostnet.Table, "prerouting")
	sendFrame(t, b1, group)
	time.Sleep(200 * time.Millisecond)
	if n := count(t, nsA, "marked-request"); n == 0 {
		t.Error("without the namespace's filter, the frame to a group address did not arrive; the test tests nothing")
	}
	ensure(t, d, []driver.NetworkSpec{netA}, made...)

	b1.unplug()
	if err := d.DeleteInterfaces(t.Context(), "b1"); err != nil {
		t.Fatal(err)
	}
	if rules := run(t, "ip", "netns", "exec", nsA, "nft", "list", "table", "bridge", "hangar"); strings.Contains(rules, made[0].Device) {
		t.Errorf("a deleted interface's binding is left:\n%s", rules)
	}
}
