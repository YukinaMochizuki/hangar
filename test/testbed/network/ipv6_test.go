//go:build testbed

package network

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/vishvananda/netns"
	"golang.org/x/net/ipv6"

	"github.com/YukinaMochizuki/hangar/internal/node/dhcp6"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/internal/node/ipv6pkt"
	"github.com/YukinaMochizuki/hangar/internal/node/ra"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

// counters6 counts, in a namespace, marked IPv6 pings by source, router
// advertisements, DHCPv6 server messages and neighbour advertisements for
// the gateway not from the gateway, which tests look for.
const counters6 = `
table ip6 tb6 {
	counter marked {}
	counter ra-forged {}
	counter dhcp6-forged {}
	counter na-gateway-forged {}
	counter internet-link {}
	chain pre {
		type filter hook prerouting priority 0; policy accept;
		icmpv6 type echo-request ip6 length ` + markLength6 + ` counter name marked
		icmpv6 type nd-router-advert ip6 saddr != fe80::1 counter name ra-forged
		udp sport 547 ip6 saddr != fe80::1 counter name dhcp6-forged
		udp sport != 547 udp dport 546 ip6 saddr != fe80::1 counter name dhcp6-forged
		icmpv6 type nd-neighbor-advert ip6 saddr != fe80::1 @th,64,128 0xfe800000000000000000000000000001 counter name na-gateway-forged
		icmpv6 type { 130, 131, 132, 133, 134, 135, 136, 143 } ip6 saddr `+upstreamAddr6+` counter name internet-link
	}
}
`

// markLength6 is the IPv6 payload length of a ping of markSize bytes.
const markLength6 = "1119"

func addCounters6(t *testing.T, ns string) {
	t.Helper()
	c := exec.Command("ip", "netns", "exec", ns, "nft", "-f", "-")
	c.Stdin = strings.NewReader(counters6)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("IPv6 counters in %s: %v: %s", ns, err, out)
	}
}

func count6(t *testing.T, ns, name string) int {
	t.Helper()
	out := run(t, "ip", "netns", "exec", ns, "nft", "reset", "counter", "ip6", "tb6", name)
	m := regexp.MustCompile(`packets (\d+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no packets in counter %s of %s: %s", name, ns, out)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

var prefix6 = netip.MustParsePrefix(hostPrefix)

// nic6 is nic with an IPv6 address of the prefix.
func nic6(machine string, n driver.NetworkSpec, host int) driver.Interface {
	i := nic(machine, n, host)
	b := prefix6.Addr().As16()
	b[8], b[9], b[14], b[15] = 0x8a, 0x3f, n.Transit.Addr().As4()[3], byte(host)
	i.IPv6 = netip.AddrFrom16(b)
	return i
}

// serveIPv6 runs a network's router advertisements and DHCPv6 on d's packet
// sockets, as the agent does, leasing each interface its address.
func serveIPv6(t *testing.T, d *hostnet.Driver, n driver.NetworkSpec, nics ...driver.Interface) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	rc, _, err := d.ListenPackets(ctx, n.UID, driver.RouterSolicitations)
	if err != nil {
		t.Fatal(err)
	}
	dc, _, err := d.ListenPackets(ctx, n.UID, driver.DHCPv6)
	if err != nil {
		t.Fatal(err)
	}
	leases := map[string]netip.Addr{}
	for _, i := range nics {
		leases[strings.ToLower(i.MAC)] = i.IPv6
	}
	srv := dhcp6.New(log)
	srv.Set(n.UID, leases)
	done := make(chan struct{}, 2)
	go func() { _ = ra.Serve(ctx, rc, log); done <- struct{}{} }()
	go func() { _ = srv.Serve(ctx, n.UID, dc); done <- struct{}{} }()
	t.Cleanup(func() {
		cancel()
		<-done
		<-done
		_ = rc.Close()
		_ = dc.Close()
	})
}

// dhcp6 has a real DHCPv6 client, dhcpcd, configure the guest's IPv6 as a
// cloud image does: it waits for a router advertisement, asks DHCPv6 for
// an address, and takes the default route from the advertisement. It
// returns the global address the guest got.
func (g *guest) dhcp6(t *testing.T) netip.Addr {
	t.Helper()
	// -c /bin/true runs no hook, which would write the host's resolv.conf.
	out, err := exec.Command("ip", "netns", "exec", g.ns, "timeout", "60",
		"dhcpcd", "-f", "/dev/null", "-c", "/bin/true", "-6", "-B", "-1", "-p", "-t", "45", "eth0").CombinedOutput()
	if err != nil {
		t.Fatalf("dhcpcd in %s: %v: %s", g.ns, err, out)
	}
	addrs := run(t, "ip", "-n", g.ns, "-6", "-o", "addr", "show", "dev", "eth0", "scope", "global")
	m := regexp.MustCompile(`inet6 (\S+)/128`).FindStringSubmatch(addrs)
	if m == nil {
		t.Fatalf("%s has no /128 after dhcpcd: %s\n%s", g.ns, addrs, out)
	}
	return netip.MustParseAddr(m[1])
}

// reaches6 says whether the guest gets an answer to a ping of addr.
func (g *guest) reaches6(addr string) bool {
	return reaches6From(g.ns, addr)
}

func reaches6From(ns, addr string) bool {
	for range 3 {
		if exec.Command("ip", "netns", "exec", ns, "ping", "-6", "-c", "1", "-W", "1", addr).Run() == nil {
			return true
		}
	}
	return false
}

// sendAs6 sends three marked pings to dst from the guest as src, an
// address it takes for them, as a VM can take any.
func (g *guest) sendAs6(t *testing.T, src, dst string) {
	t.Helper()
	if src != g.nic.IPv6.String() {
		run(t, "ip", "-n", g.ns, "-6", "addr", "add", src+"/128", "dev", "eth0", "nodad")
		defer run(t, "ip", "-n", g.ns, "-6", "addr", "del", src+"/128", "dev", "eth0")
	}
	out, _ := exec.Command("ip", "netns", "exec", g.ns, "ping", "-6", "-c", "3", "-i", "0.2", "-W", "1", "-s", markSize, "-I", src, dst).CombinedOutput()
	if !strings.Contains(string(out), "3 packets transmitted") {
		t.Fatalf("%s did not send as %s: %s", g.ns, src, out)
	}
	time.Sleep(200 * time.Millisecond)
}

// sendIPv6 sends an IPv6 packet from the guest's eth0 in a frame to dst.
func sendIPv6(t *testing.T, g *guest, dst net.HardwareAddr, p ipv6pkt.Packet) {
	t.Helper()
	mac, _ := net.ParseMAC(g.nic.MAC)
	sendFrame(t, g, ethernet(dst, mac, 0x86dd, p.Marshal()))
	time.Sleep(200 * time.Millisecond)
}

// sendICMPv6From sends an ICMPv6 message of type typ, as neighbour
// discovery's are long, from namespace ns to dst with the hop limit given.
func sendICMPv6From(t *testing.T, ns string, dst netip.Addr, typ byte, hopLimit int) {
	t.Helper()
	h, err := netns.GetFromName(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	if err := inNS(h, func() error {
		c, err := net.ListenPacket("ip6:ipv6-icmp", "::")
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		p := ipv6.NewPacketConn(c)
		if err := p.SetHopLimit(hopLimit); err != nil {
			return err
		}
		msg := make([]byte, 24)
		msg[0] = typ
		copy(msg[8:], dst.AsSlice())
		_, err = c.WriteTo(msg, &net.IPAddr{IP: dst.AsSlice()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func mustMAC(s string) net.HardwareAddr {
	m, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return m
}

// linkLocalOf is the guest's link-local address.
func linkLocalOf(t *testing.T, g *guest) netip.Addr {
	t.Helper()
	out := run(t, "ip", "-n", g.ns, "-6", "-o", "addr", "show", "dev", "eth0", "scope", "link")
	m := regexp.MustCompile(`inet6 (\S+)/64`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("%s has no link-local address: %s", g.ns, out)
	}
	return netip.MustParseAddr(m[1])
}

func macOf(t *testing.T, ns, link string) string {
	t.Helper()
	args := []string{"ip", "-o", "link", "show", link}
	if ns != "" {
		args = []string{"ip", "-n", ns, "-o", "link", "show", link}
	}
	out := run(t, args...)
	m := regexp.MustCompile(`link/ether (\S+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no MAC on %s: %s", link, out)
	}
	return m[1]
}

// policy6 opens a guest's interface to every IPv4 peer and to ingress from
// the given IPv6 peers, and lets it send anything.
func policy6(i driver.Interface, from ...string) driver.InterfacePolicy {
	p := secgroup.Policy{Ingress: []secgroup.Rule{{Protocol: secgroup.All, Peers: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}}, EgressAll: true}
	for _, f := range from {
		p.Ingress = append(p.Ingress, secgroup.Rule{Protocol: secgroup.ICMP, Peers: []netip.Prefix{netip.MustParsePrefix(f)}})
	}
	return driver.InterfacePolicy{MachineUID: i.MachineUID, Name: i.Name, Policy: p}
}

// TestIPv6 runs stand-in guests with public IPv6 addresses on two networks
// of a host with the OVH-style upstream: they configure themselves by RA
// and DHCPv6, reach the upstream as their own addresses, are reached only
// as their groups allow, from the upstream through proxy NDP or from
// another network, cannot pass as another or as a router or DHCPv6 server,
// and the host repairs its routes and proxy entries when they drift.
func TestIPv6(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr), IPv6Prefix: prefix6, PublicIPv6: netip.MustParseAddr(hostAddr6)})
	nics := []driver.Interface{nic6("a1", netA, 2), nic6("a2", netA, 3), nic6("b1", netB, 2)}
	made := ensure(t, d, []driver.NetworkSpec{netA, netB}, nics...)
	logHost(t)
	serveIPv6(t, d, netA, nics[0], nics[1])
	serveIPv6(t, d, netB, nics[2])
	gs := map[string]*guest{}
	for _, m := range made {
		gs[m.MachineUID] = newGuest(t, m)
	}
	a1, a2, b1 := gs["a1"], gs["a2"], gs["b1"]
	firewall := func(n driver.NetworkSpec, ps ...driver.InterfacePolicy) {
		t.Helper()
		if err := d.EnsureFirewall(t.Context(), n.UID, ps); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a guest gets its address by RA and DHCPv6", func(t *testing.T) {
		for _, g := range []*guest{a1, a2, b1} {
			if got := g.dhcp6(t); got != g.nic.IPv6 {
				t.Errorf("%s got %s by DHCPv6, want %s", g.ns, got, g.nic.IPv6)
			}
			if r := run(t, "ip", "-n", g.ns, "-6", "route", "show", "default"); !strings.Contains(r, "via fe80::1 dev eth0") {
				t.Errorf("%s's default route is not via the gateway: %s", g.ns, r)
			}
		}
	})
	t.Run("the network sends each address to its tap's MAC, never asking", func(t *testing.T) {
		for _, g := range []*guest{a1, a2} {
			n := run(t, "ip", "-n", hostnet.NamespaceOf(netA.UID), "-6", "neigh", "show", g.nic.IPv6.String(), "dev", "hn-br")
			if !strings.Contains(n, g.nic.MAC) || !strings.Contains(n, "PERMANENT") {
				t.Errorf("%s's address has no permanent neighbour entry at its MAC %s: %q", g.ns, g.nic.MAC, n)
			}
		}
	})
	t.Run("out to the upstream as its own address", func(t *testing.T) {
		for _, g := range []*guest{a1, b1} {
			out, err := exec.Command("ip", "netns", "exec", g.ns, "curl", "-sS", "-m", "5", upstreamURL6).CombinedOutput()
			if got := strings.TrimSpace(string(out)); err != nil || got != g.nic.IPv6.String() {
				t.Errorf("%s is seen upstream as %q (%v), want its own %s", g.ns, got, err, g.nic.IPv6)
			}
		}
	})
	t.Run("the host and private addresses stay out of reach", func(t *testing.T) {
		if a1.reaches6(hostAddr6) {
			t.Errorf("a1 reaches the host's own address %s", hostAddr6)
		}
		if a1.reaches("10.1.0.2") {
			t.Error("a1 reaches b1's private IPv4 address")
		}
	})
	t.Run("the front door alone of the host, at its public addresses", func(t *testing.T) {
		// The host listens on every address, as the edge's sockets do, on
		// the front door's ports and on one more.
		for _, port := range []string{"80", "443", "8443"} {
			ln, err := net.Listen("tcp", ":"+port)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			go func() {
				_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "host:"+port) }))
			}()
		}
		get := func(g *guest, url string) string {
			out, _ := exec.Command("ip", "netns", "exec", g.ns, "curl", "-sS", "-m", "3", url).CombinedOutput()
			return strings.TrimSpace(string(out))
		}
		for _, g := range []*guest{a1, b1} {
			for _, host := range []string{"[" + hostAddr6 + "]", hostAddr} {
				for _, port := range []string{"80", "443"} {
					if got := get(g, "http://"+host+":"+port+"/"); got != "host:"+port {
						t.Errorf("%s to the front door at %s:%s: %q", g.ns, host, port, got)
					}
				}
				if got := get(g, "http://"+host+":8443/"); strings.Contains(got, "host:") {
					t.Errorf("%s reaches the host's :8443 at %s", g.ns, host)
				}
			}
			// Not at the host's other addresses: its end of the transit link.
			if got := get(g, "http://100.64.0.0:443/"); strings.Contains(got, "host:") {
				t.Errorf("%s reaches the front door's port at the host's transit address", g.ns)
			}
		}
		// UDP to 443, as QUIC would be, is not the front door's.
		udp, err := net.ListenPacket("udp", ":443")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = udp.Close() }()
		for _, host := range []string{hostAddr6, hostAddr} {
			script := fmt.Sprintf("import socket\ns=socket.socket(socket.AF_INET6 if ':' in %q else socket.AF_INET, socket.SOCK_DGRAM)\nfor _ in range(3): s.sendto(b'quic', (%q, 443))", host, host)
			if out, err := exec.Command("ip", "netns", "exec", a1.ns, "python3", "-c", script).CombinedOutput(); err != nil {
				t.Fatalf("a1 sending UDP to %s: %v %s", host, err, out)
			}
		}
		_ = udp.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, from, err := udp.ReadFrom(make([]byte, 64)); err == nil {
			t.Errorf("a1 reaches the host's UDP 443, from %s", from)
		}
		// The host still answers no ping from a network.
		if a1.reaches6(hostAddr6) || a1.reaches(hostAddr) {
			t.Error("a1 pings the host")
		}
	})
	t.Run("another network reaches a guest only as its groups allow", func(t *testing.T) {
		firewall(netA, policy6(nics[0]), policy6(nics[1]))
		if b1.reaches6(a1.nic.IPv6.String()) {
			t.Error("b1 reaches a1, whose groups let in no IPv6, though 0.0.0.0/0")
		}
		firewall(netA, policy6(nics[0], netip.PrefixFrom(b1.nic.IPv6, 128).String()), policy6(nics[1]))
		if !b1.reaches6(a1.nic.IPv6.String()) {
			t.Error("b1 does not reach a1, whose groups let it in")
		}
		if b1.reaches6(a2.nic.IPv6.String()) {
			t.Error("b1 reaches a2, whose groups let it not in")
		}
	})
	t.Run("the upstream reaches a guest only as its groups allow", func(t *testing.T) {
		if reaches6From(upstreamNS, a1.nic.IPv6.String()) {
			t.Error("the upstream reaches a1, whose groups let it not in")
		}
		firewall(netA, policy6(nics[0], "::/0"), policy6(nics[1]))
		if !reaches6From(upstreamNS, a1.nic.IPv6.String()) {
			t.Fatal("the upstream does not reach a1, whose groups let in ::/0")
		}
		// The upstream delivers only to the MAC that answered its
		// solicitation: the host's, by proxy NDP.
		n := run(t, "ip", "-n", upstreamNS, "-6", "neigh", "show", a1.nic.IPv6.String())
		if want := macOf(t, "", upstreamLink); !strings.Contains(n, want) {
			t.Errorf("the upstream has a1's address at %q, want the host's uplink %s", n, want)
		}
	})

	addCounters6(t, upstreamNS)
	addCounters6(t, a2.ns)
	t.Run("a guest cannot pass as another", func(t *testing.T) {
		count6(t, upstreamNS, "marked")
		a1.sendAs6(t, a1.nic.IPv6.String(), upstreamAddr6)
		if n := count6(t, upstreamNS, "marked"); n == 0 {
			t.Fatal("a1 as itself does not reach the upstream: the test sees nothing")
		}
		for _, src := range []string{a2.nic.IPv6.String(), b1.nic.IPv6.String(), "2001:db8:0:1::99", "2001:db8:9::1"} {
			a1.sendAs6(t, src, upstreamAddr6)
			if n := count6(t, upstreamNS, "marked"); n > 0 {
				t.Errorf("a1 as %s reached the upstream (%d pings)", src, n)
			}
		}
	})
	// Each layer holds without the others: the binding, the namespace's
	// filter and the host's, as for IPv4.
	nsA := hostnet.NamespaceOf(netA.UID)
	forged := func(t *testing.T, srcs ...string) {
		t.Helper()
		for _, src := range srcs {
			a1.sendAs6(t, src, upstreamAddr6)
			if n := count6(t, upstreamNS, "marked"); n > 0 {
				t.Errorf("a1 as %s reached the upstream (%d pings)", src, n)
			}
		}
	}
	restore := func() {
		ensure(t, d, []driver.NetworkSpec{netA, netB}, nics...)
		firewall(netA, policy6(nics[0], "::/0"), policy6(nics[1]))
		settle(d)
	}
	unbind := func(t *testing.T) {
		t.Helper()
		run(t, "ip", "netns", "exec", nsA, "nft", "flush", "chain", "bridge", hostnet.Table, "prerouting")
	}
	t.Run("without the binding, the host's filter holds", func(t *testing.T) {
		defer restore()
		unbind(t)
		forged(t, b1.nic.IPv6.String(), "2001:db8:0:1::99", "2001:db8:9::1")
	})
	t.Run("without the binding and the host's filter, the namespace's holds outside the prefix", func(t *testing.T) {
		defer restore()
		unbind(t)
		run(t, "nft", "flush", "chain", "ip6", hostnet.Table, "prerouting")
		a1.sendAs6(t, b1.nic.IPv6.String(), upstreamAddr6)
		if n := count6(t, upstreamNS, "marked"); n == 0 {
			t.Error("a1 as b1 did not reach the upstream with only the namespace's filter; the test tests nothing")
		}
		forged(t, "2001:db8:9::1")
	})
	t.Run("without the namespace's and the host's filters, the binding holds", func(t *testing.T) {
		defer restore()
		run(t, "ip", "netns", "exec", nsA, "nft", "flush", "chain", "ip6", hostnet.Table, "prerouting")
		run(t, "nft", "flush", "chain", "ip6", hostnet.Table, "prerouting")
		forged(t, a2.nic.IPv6.String(), b1.nic.IPv6.String(), "2001:db8:0:1::99", "2001:db8:9::1")
	})
	t.Run("a guest is no router and no DHCPv6 server", func(t *testing.T) {
		// a2 takes in any IPv6, so that the binding alone stands between
		// it and a1.
		all := policy6(nics[1])
		all.Policy.Ingress = append(all.Policy.Ingress, secgroup.Rule{Protocol: secgroup.All, Peers: []netip.Prefix{netip.MustParsePrefix("::/0")}})
		firewall(netA, policy6(nics[0], "::/0"), all)
		defer firewall(netA, policy6(nics[0], "::/0"), policy6(nics[1]))
		for _, c := range []string{"ra-forged", "dhcp6-forged", "na-gateway-forged"} {
			count6(t, a2.ns, c)
		}
		ll := linkLocalOf(t, a1)
		allNodes := net.HardwareAddr{0x33, 0x33, 0, 0, 0, 1}
		sendIPv6(t, a1, allNodes, ipv6pkt.Packet{Src: ll, Dst: netip.MustParseAddr("ff02::1"), Next: ipv6pkt.ICMPv6, HopLimit: 255,
			Payload: ra.Message()})
		a2mac, _ := net.ParseMAC(a2.nic.MAC)
		reply, err := dhcpv6.NewMessage()
		if err != nil {
			t.Fatal(err)
		}
		reply.MessageType = dhcpv6.MessageTypeAdvertise
		// As a server answering a client, as a server answering a relay, and
		// to a client from another port: each is dropped by a rule of its
		// own.
		for _, ports := range [][2]uint16{{547, 546}, {547, 547}, {40000, 546}} {
			sendIPv6(t, a1, a2mac, ipv6pkt.Packet{Src: ll, Dst: linkLocalOf(t, a2), Next: ipv6pkt.UDP, HopLimit: 64,
				Payload: ipv6pkt.UDPDatagram(ports[0], ports[1], reply.ToBytes())})
		}
		na := make([]byte, 24)
		na[0], na[4] = 136, 0x20 // neighbour advertisement, override
		gw := driver.GatewayIPv6.As16()
		copy(na[8:], gw[:])
		sendIPv6(t, a1, allNodes, ipv6pkt.Packet{Src: ll, Dst: netip.MustParseAddr("ff02::1"), Next: ipv6pkt.ICMPv6, HopLimit: 255, Payload: na})
		for _, c := range []string{"ra-forged", "dhcp6-forged", "na-gateway-forged"} {
			if n := count6(t, a2.ns, c); n > 0 {
				t.Errorf("a2 got %d of a1's %s", n, strings.TrimSuffix(c, "-forged"))
			}
		}
	})

	t.Run("the internet's neighbour discovery and listener messages are the groups' to let in", func(t *testing.T) {
		addCounters6(t, a1.ns)
		defer run(t, "ip", "netns", "exec", a1.ns, "nft", "delete", "table", "ip6", "tb6")
		probe := func() int {
			t.Helper()
			count6(t, a1.ns, "internet-link")
			for _, typ := range []byte{130, 131, 132, 133, 134, 135, 136, 143} {
				sendICMPv6From(t, upstreamNS, a1.nic.IPv6, typ, 255)
			}
			time.Sleep(300 * time.Millisecond)
			return count6(t, a1.ns, "internet-link")
		}
		firewall(netA, policy6(nics[0]), policy6(nics[1]))
		if n := probe(); n > 0 {
			t.Errorf("%d of the upstream's ICMPv6 neighbour discovery and listener messages reached a1, whose groups let in no IPv6", n)
		}
		firewall(netA, policy6(nics[0], "::/0"), policy6(nics[1]))
		if n := probe(); n == 0 {
			t.Error("none reached a1 even once its groups let ICMPv6 in: the test sees nothing")
		}
	})
	t.Run("no mail out", func(t *testing.T) {
		curl := func(port string) (string, error) {
			out, err := exec.Command("ip", "netns", "exec", a1.ns, "curl", "-sS", "-m", "3", "http://["+upstreamAddr6+"]:"+port+"/").CombinedOutput()
			return strings.TrimSpace(string(out)), err
		}
		if got, err := curl("8080"); err != nil || got != a1.nic.IPv6.String() {
			t.Fatalf("a1 does not reach the upstream's port 8080 (%q, %v): the test sees nothing", got, err)
		}
		if got, err := curl("25"); err == nil {
			t.Errorf("a1 reached the upstream's port 25: %q", got)
		}
	})
	t.Run("a guest reaches one on another network only as its groups allow, both ways", func(t *testing.T) {
		if a1.reaches6(b1.nic.IPv6.String()) {
			t.Error("a1 reaches b1, whose groups let it not in")
		}
		firewall(netB, policy6(nics[2], netip.PrefixFrom(a1.nic.IPv6, 128).String()))
		defer firewall(netB, policy6(nics[2]))
		if !a1.reaches6(b1.nic.IPv6.String()) {
			t.Error("a1 does not reach b1, whose groups let it in")
		}
	})
	t.Run("a network reaches nothing on the host but the uplink and other networks", func(t *testing.T) {
		lan := decoyPrefix + "-lan6"
		link := decoyPrefix + "6"
		t.Cleanup(func() {
			_ = exec.Command("ip", "netns", "del", lan).Run()
			_ = exec.Command("ip", "link", "del", link).Run()
		})
		for _, args := range [][]string{
			{"ip", "netns", "add", lan},
			{"ip", "link", "add", link, "type", "veth", "peer", "name", "eth0", "netns", lan},
			{"ip", "-6", "addr", "add", "2001:db8:77::1/64", "dev", link, "nodad"},
			{"ip", "link", "set", link, "up"},
			{"ip", "-n", lan, "-6", "addr", "add", "2001:db8:77::2/64", "dev", "eth0", "nodad"},
			{"ip", "-n", lan, "link", "set", "eth0", "up"},
			{"ip", "-n", lan, "-6", "route", "add", "2001:db8::/32", "via", "2001:db8:77::1"},
		} {
			run(t, args...)
		}
		if !reaches6From(lan, hostAddr6) {
			t.Fatal("the decoy network does not reach the host: the test sees nothing")
		}
		if a1.reaches6("2001:db8:77::2") {
			t.Error("a1 reaches a network of the host's own, not the uplink")
		}
	})
	t.Run("only live addresses of the prefix come in", func(t *testing.T) {
		addCounters6(t, nsA)
		defer run(t, "ip", "netns", "exec", nsA, "nft", "delete", "table", "ip6", "tb6")
		// A route of the host's own, outside the prefix, into net-a: the
		// host lets nothing in by it.
		outside := "2001:db8:66::5"
		vethA := firstName("hnv", netA.UID)
		run(t, "ip", "-6", "route", "add", outside+"/128", "via", "fe80::2", "dev", vethA)
		defer run(t, "ip", "-6", "route", "del", outside+"/128", "via", "fe80::2", "dev", vethA)
		run(t, "ip", "-n", upstreamNS, "-6", "route", "add", "2001:db8:66::/64", "via", hostAddr6, "dev", "tbupx")
		defer run(t, "ip", "-n", upstreamNS, "-6", "route", "del", "2001:db8:66::/64", "via", hostAddr6, "dev", "tbupx")
		// The upstream sends to an address of the prefix no machine has,
		// at the host's MAC, as if the host had answered for it.
		dead := "2001:db8:0:1:8a3f::99"
		run(t, "ip", "-n", upstreamNS, "-6", "neigh", "replace", dead, "lladdr", macOf(t, "", upstreamLink), "dev", "tbupx")
		defer run(t, "ip", "-n", upstreamNS, "-6", "neigh", "del", dead, "dev", "tbupx")
		count6(t, nsA, "marked")
		for _, dst := range []string{outside, dead} {
			_ = exec.Command("ip", "netns", "exec", upstreamNS, "ping", "-6", "-c", "3", "-i", "0.2", "-W", "1", "-s", markSize, dst).Run()
			time.Sleep(200 * time.Millisecond)
			if n := count6(t, nsA, "marked"); n > 0 {
				t.Errorf("the upstream's pings to %s, which no machine has, reached net-a's namespace (%d)", dst, n)
			}
		}
		if !reaches6From(upstreamNS, a1.nic.IPv6.String()) {
			t.Error("the upstream does not reach a1, whose groups let it in: the test sees nothing")
		}
	})
	t.Run("a guest's neighbour advertisement says only its MAC, wherever the option is", func(t *testing.T) {
		a1Addr := a1.nic.IPv6.String()
		a2mac, _ := net.ParseMAC(a2.nic.MAC)
		at := func() string {
			return run(t, "ip", "-n", nsA, "-6", "neigh", "show", a1Addr)
		}
		run(t, "ip", "netns", "exec", nsA, "ping", "-6", "-c", "1", "-W", "1", a1Addr)
		if n := at(); !strings.Contains(n, a1.nic.MAC) {
			t.Fatalf("the namespace does not know a1 at its MAC: %s", n)
		}
		target := a1.nic.IPv6.As16()
		na := func(opts ...[]byte) []byte {
			b := make([]byte, 24)
			b[0], b[4] = 136, 0x20 // neighbour advertisement, override
			copy(b[8:], target[:])
			for _, o := range opts {
				b = append(b, o...)
			}
			return b
		}
		tlla := append([]byte{2, 1}, a2mac...)
		unknown := []byte{200, 1, 0, 0, 0, 0, 0, 0}
		for name, msg := range map[string][]byte{
			"as its only option":         na(tlla),
			"after an unknown option":    na(unknown, tlla),
			"after its own MAC's option": na(append([]byte{2, 1}, mustMAC(a1.nic.MAC)...), tlla),
		} {
			sendIPv6(t, a1, net.HardwareAddr{0x33, 0x33, 0, 0, 0, 1}, ipv6pkt.Packet{
				Src: a1.nic.IPv6, Dst: netip.MustParseAddr("ff02::1"), Next: ipv6pkt.ICMPv6, HopLimit: 255, Payload: msg,
			})
			if n := at(); strings.Contains(n, a2.nic.MAC) {
				t.Errorf("a1's advertisement with a2's MAC %s moved a1's address to it: %s", name, n)
				run(t, "ip", "-n", nsA, "-6", "neigh", "del", a1Addr, "dev", "hn-br")
				run(t, "ip", "netns", "exec", nsA, "ping", "-6", "-c", "1", "-W", "1", a1Addr)
			}
		}
	})

	a1Addr := a1.nic.IPv6.String()
	for _, c := range []struct {
		name  string
		drift []string
		wait  time.Duration
	}{
		{"the route into the network", []string{"ip", "-6", "route", "del", a1Addr + "/128"}, 5 * time.Second},
		{"the route in the namespace", []string{"ip", "-n", nsA, "-6", "route", "del", a1Addr + "/128"}, 5 * time.Second},
		// The kernel signals no change to proxy entries: the driver polls.
		{"the proxy entry", []string{"ip", "-6", "neigh", "del", "proxy", a1Addr, "dev", upstreamLink}, 15 * time.Second},
	} {
		t.Run("drift: "+c.name, func(t *testing.T) {
			settle(d)
			run(t, c.drift...)
			// Asked again, not answered from what the upstream learnt.
			run(t, "ip", "-n", upstreamNS, "-6", "neigh", "flush", "dev", "tbupx")
			if reaches6From(upstreamNS, a1Addr) {
				t.Fatalf("the upstream reaches a1 without %s: the test sees nothing", c.name)
			}
			select {
			case <-d.Changes():
			case <-time.After(c.wait):
				t.Fatalf("losing %s was never signalled", c.name)
			}
			ensure(t, d, []driver.NetworkSpec{netA, netB}, nics...)
			firewall(netA, policy6(nics[0], "::/0"), policy6(nics[1]))
			if !reaches6From(upstreamNS, a1Addr) {
				t.Errorf("the upstream does not reach a1 once %s is repaired", c.name)
			}
		})
	}
	// As an Address moves: the interface it leaves lets go first, then the
	// one on another network takes it.
	t.Run("an address moving to another network's interface moves its delivery", func(t *testing.T) {
		moving := a1.nic.IPv6.String()
		nsA, nsB := hostnet.NamespaceOf(netA.UID), hostnet.NamespaceOf(netB.UID)
		routeDev := func(addr string) string {
			f := strings.Fields(run(t, "ip", "-6", "route", "show", addr+"/128"))
			if i := slices.Index(f, "dev"); i >= 0 && i+1 < len(f) {
				return f[i+1]
			}
			return ""
		}
		intoB := routeDev(b1.nic.IPv6.String())
		freed := nics[0]
		freed.IPv6 = nic6("a1", netA, 9).IPv6
		taker := nics[2]
		taker.IPv6 = a1.nic.IPv6
		if _, err := d.EnsureInterface(t.Context(), taker); !errors.Is(err, driver.ErrIPv6InUse) {
			t.Errorf("b1 took the address a1 holds: %v", err)
		}
		for _, i := range []driver.Interface{freed, taker} {
			if _, err := d.EnsureInterface(t.Context(), i); err != nil {
				t.Fatal(err)
			}
		}
		// What the guests are told.
		run(t, "ip", "-n", a1.ns, "-6", "addr", "del", moving+"/128", "dev", "eth0")
		run(t, "ip", "-n", a1.ns, "-6", "addr", "add", freed.IPv6.String()+"/128", "dev", "eth0", "nodad")
		run(t, "ip", "-n", b1.ns, "-6", "addr", "add", moving+"/128", "dev", "eth0", "nodad")
		firewall(netA, policy6(freed, "::/0"), policy6(nics[1]))
		firewall(netB, policy6(taker, "::/0"))
		settle(d)

		if got := routeDev(moving); got != intoB || got == "" {
			t.Errorf("the address is routed by %q, not into b1's network by %q", got, intoB)
		}
		if n := run(t, "ip", "-n", nsB, "-6", "neigh", "show", moving, "dev", "hn-br"); !strings.Contains(n, b1.nic.MAC) || !strings.Contains(n, "PERMANENT") {
			t.Errorf("b1's network has no permanent neighbour entry for the address at b1's MAC: %q", n)
		}
		if n := run(t, "ip", "-n", nsA, "-6", "neigh", "show", moving, "dev", "hn-br"); strings.Contains(n, "PERMANENT") || strings.Contains(n, a1.nic.MAC) {
			t.Errorf("a1's network still sends the address to a1: %q", n)
		}
		if p := run(t, "ip", "-6", "neigh", "show", "proxy", moving); !strings.Contains(p, moving) {
			t.Error("the address moved has no proxy entry on the uplink")
		}
		run(t, "ip", "-n", upstreamNS, "-6", "neigh", "flush", "dev", "tbupx")
		if !reaches6From(upstreamNS, moving) {
			t.Error("the upstream does not reach b1 at the address it moved to")
		}
		if !reaches6From(upstreamNS, freed.IPv6.String()) {
			t.Error("the upstream does not reach a1 at its new address")
		}
	})
	t.Run("a guest's interface going takes its route and proxy entry", func(t *testing.T) {
		a2.unplug()
		if err := d.DeleteInterfaces(t.Context(), "a2"); err != nil {
			t.Fatal(err)
		}
		a2Addr := a2.nic.IPv6.String()
		if r := run(t, "ip", "-6", "route", "show", a2Addr+"/128"); r != "" {
			t.Errorf("a route to a2's address is left: %s", r)
		}
		if p := run(t, "ip", "-6", "neigh", "show", "proxy", a2Addr); strings.Contains(p, a2Addr) {
			t.Errorf("a proxy entry for a2's address is left: %s", p)
		}
		if n := run(t, "ip", "-n", hostnet.NamespaceOf(netA.UID), "-6", "neigh", "show", a2Addr, "dev", "hn-br"); strings.Contains(n, "PERMANENT") {
			t.Errorf("a neighbour entry for a2's address is left: %s", n)
		}
	})
}

// TestNoIPv6WithoutAPrefix: a host without a prefix has no IPv6 in its
// networks: none on their links, no ip6 table, no route or proxy entry.
func TestNoIPv6WithoutAPrefix(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	ensure(t, d, []driver.NetworkSpec{netA}, nic("a1", netA, 2))
	ns := hostnet.NamespaceOf(netA.UID)
	if out := run(t, "ip", "-n", ns, "-6", "addr"); strings.TrimSpace(out) != "" {
		t.Errorf("the namespace has IPv6 addresses:\n%s", out)
	}
	for _, args := range [][]string{
		{"nft", "list", "table", "ip6", hostnet.Table},
		{"ip", "netns", "exec", ns, "nft", "list", "table", "ip6", hostnet.Table},
	} {
		if exec.Command(args[0], args[1:]...).Run() == nil {
			t.Errorf("%s: there is an ip6 table", strings.Join(args, " "))
		}
	}
	if p := run(t, "ip", "-6", "neigh", "show", "proxy", "dev", upstreamLink); strings.TrimSpace(p) != "" {
		t.Errorf("proxy entries on the uplink: %s", p)
	}
}
