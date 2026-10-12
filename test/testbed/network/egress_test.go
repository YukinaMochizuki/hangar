//go:build testbed

package network

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"testing"

	"github.com/vishvananda/netns"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
)

// TestSpoofing has a guest forge its source address: only addresses of its
// network's range leave it, NATed, and each of the two layers holds alone.
func TestSpoofing(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	nets := []driver.NetworkSpec{netA, netB, netC}
	// c2 has a2's address, on a network with net-a's range.
	made := ensure(t, d, nets, nic("a1", netA, 2), nic("a2", netA, 3), nic("c2", netC, 3))
	a1, a2, c2 := newGuest(t, made[0]), newGuest(t, made[1]), newGuest(t, made[2])
	for _, g := range []*guest{a1, a2, c2} {
		if err := addCounters(g.ns); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []string{"foreign", "marked-request", "marked-reply"} {
		count(t, upstreamNS, c)
	}

	outside := []struct{ src, what string }{
		{"203.0.113.7", "an address of no network"},
		{"100.64.0.3", "net-b's end of its transit link"},
		{"100.64.0.1", "its namespace's end of the transit link"},
		{"100.64.0.0", "the host's end of its transit link"},
		{"10.1.0.2", "an address of another network's range"},
	}
	forged := func(t *testing.T) {
		t.Helper()
		for _, c := range outside {
			a1.sendAs(t, c.src, upstreamAddr)
			if n := count(t, upstreamNS, "marked-request"); n > 0 {
				t.Errorf("a1 as %s, %s, reached the upstream (%d pings)", c.src, c.what, n)
			}
		}
		noneForeign(t)
	}
	// The gateway answers a ping from anywhere out through both NATs, so a
	// forged source would make the host send to it.
	reflected := func(t *testing.T) {
		t.Helper()
		a1.sendAs(t, upstreamAddr, a1.nic.Gateway.String())
		if n := count(t, upstreamNS, "marked-reply"); n > 0 {
			t.Errorf("the gateway answered a1 as the upstream, out to the upstream (%d replies)", n)
		}
	}
	restore := func() {
		ensure(t, d, nets)
		settle(d)
	}

	t.Run("outside its range, nothing leaves", func(t *testing.T) {
		forged(t)
		reflected(t)
	})
	t.Run("as another guest of its network, it stays there", func(t *testing.T) {
		a1.sendAs(t, a2.nic.Address.Addr().String(), upstreamAddr)
		if n := count(t, upstreamNS, "marked-request"); n == 0 {
			t.Error("a1 as a2 did not reach the upstream; the test tests nothing")
		}
		// a1 and a2 both answer ARP for the address; either may get them.
		if n := count(t, a1.ns, "marked-reply") + count(t, a2.ns, "marked-reply"); n == 0 {
			t.Error("the replies to a1 as a2 did not come back to net-a")
		}
		if n := count(t, c2.ns, "marked-reply"); n > 0 {
			t.Errorf("%d replies to a1 as a2 reached c2, which has a2's address on net-c", n)
		}
		noneForeign(t)
	})
	t.Run("without the namespace's filter, the host's holds", func(t *testing.T) {
		defer restore()
		run(t, "ip", "netns", "exec", hostnet.NamespaceOf(netA.UID), "nft", "flush", "chain", "ip", hostnet.Table, "prerouting")
		forged(t)
	})
	t.Run("without the host's filter, the namespace's holds", func(t *testing.T) {
		defer restore()
		run(t, "nft", "flush", "chain", "ip", hostnet.Table, "prerouting")
		forged(t)
		reflected(t)
	})
}

// TestOnlyTheUplink puts another network on the host, as docker's or a VPN
// would be, with a port of the host's address forwarded into it: a guest
// reaches neither, and the uplink as before.
func TestOnlyTheUplink(t *testing.T) {
	lan, link, table := decoyPrefix+"-lan", decoyPrefix+"7", decoyPrefix+"_dnat"
	t.Cleanup(func() {
		_ = exec.Command("ip", "netns", "del", lan).Run()
		_ = exec.Command("ip", "link", "del", link).Run()
		_ = exec.Command("nft", "delete", "table", "ip", table).Run()
	})
	for _, args := range [][]string{
		{"ip", "netns", "add", lan},
		{"ip", "link", "add", link, "type", "veth", "peer", "name", "eth0", "netns", lan},
		{"ip", "addr", "add", "172.31.0.1/24", "dev", link},
		{"ip", "link", "set", link, "up"},
		{"ip", "-n", lan, "addr", "add", "172.31.0.2/24", "dev", "eth0"},
		{"ip", "-n", lan, "link", "set", "eth0", "up"},
		{"ip", "-n", lan, "link", "set", "lo", "up"},
		{"ip", "-n", lan, "route", "add", "default", "via", "172.31.0.1"},
		{"nft", "add", "table", "ip", table},
		{"nft", "add", "chain", "ip", table, "pre", "{ type nat hook prerouting priority dstnat; }"},
		{"nft", "add", "rule", "ip", table, "pre", "ip", "daddr", hostAddr, "tcp", "dport", "9999", "dnat", "to", "172.31.0.2:8080"},
	} {
		run(t, args...)
	}
	ns, err := netns.GetFromName(lan)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ns.Close() }()
	var ln net.Listener
	if err := inNS(ns, func() (err error) {
		ln, err = net.Listen("tcp4", "172.31.0.2:8080")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, "decoy") }))
	}()
	forwarded := "http://" + hostAddr + ":9999/"

	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	a1 := newGuest(t, ensure(t, d, []driver.NetworkSpec{netA}, nic("a1", netA, 2))[0])
	// The host reaches the decoy, and the upstream it through the forward.
	run(t, "ping", "-c", "1", "-W", "1", "172.31.0.2")
	run(t, "ip", "netns", "exec", upstreamNS, "curl", "-sS", "-m", "3", forwarded)

	if a1.reaches("172.31.0.2") {
		t.Error("a1 reaches a network of the host's other than the uplink")
	}
	if out, err := exec.Command("ip", "netns", "exec", a1.ns, "curl", "-sS", "-m", "3", forwarded).CombinedOutput(); err == nil {
		t.Errorf("a1 reaches the host's forward into another network: %s", out)
	}
	if got, err := a1.seenAs(); err != nil || got != hostAddr {
		t.Errorf("a1 is seen upstream as %q (%v), want the host's %s", got, err, hostAddr)
	}
	noneForeign(t)
}
