//go:build testbed

package network

import (
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/internal/node/natlog"
)

// dualStackGuest runs a guest with an IPv6 address on network n, which it
// takes by DHCPv6 as a cloud image does.
func dualStackGuest(t *testing.T, d *hostnet.Driver, n driver.NetworkSpec, machine string) *guest {
	t.Helper()
	i := nic6(machine, n, 2)
	made := ensure(t, d, []driver.NetworkSpec{n}, i)
	serveIPv6(t, d, n, i)
	g := newGuest(t, made[0])
	if got := g.dhcp6(t); got != i.IPv6 {
		t.Fatalf("%s got %s by DHCPv6, want %s", g.ns, got, i.IPv6)
	}
	return g
}

// TestNoMail: TCP from a network to ports 25 and 465 never leaves the host,
// over IPv4 or IPv6; 587, submission, does.
func TestNoMail(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr), IPv6Prefix: prefix6, PublicIPv6: netip.MustParseAddr(hostAddr6)})
	g := dualStackGuest(t, d, netA, "a1")
	for _, host := range []string{upstreamAddr, "[" + upstreamAddr6 + "]"} {
		for _, c := range []struct {
			port string
			open bool
		}{{"8080", true}, {"587", true}, {"25", false}, {"465", false}} {
			out, err := exec.Command("ip", "netns", "exec", g.ns, "curl", "-sS", "-m", "3", "http://"+host+":"+c.port+"/").CombinedOutput()
			switch {
			case c.open && err != nil:
				t.Errorf("a1 does not reach %s:%s: %v: %s", host, c.port, err, out)
			case !c.open && err == nil:
				t.Errorf("a1 reached %s:%s: %s", host, c.port, out)
			}
		}
	}
	noneForeign(t)
}

// probeCounters count, in the upstream, the UDP probes of
// TestNewConnectionsAreLimited by family.
const probeCounters = `
table inet tbprobe {
	counter v4 {}
	counter v6 {}
	chain pre {
		type filter hook prerouting priority 0; policy accept;
		meta nfproto ipv4 udp dport 9999 counter name v4
		meta nfproto ipv6 udp dport 9999 counter name v6
	}
}
`

// floodScript opens a TCP connection, waits for the bucket to refill, sends
// 600 UDP probes, each a new connection, IPv4 and IPv6 in turn, then uses
// the first connection. It prints how long the probes took and the answer.
const floodScript = `import socket,time
c=socket.create_connection((%[1]q,8080),timeout=3)
time.sleep(4)
start=time.monotonic()
for i in range(600):
    fam,addr=(socket.AF_INET,%[1]q) if i%%2==0 else (socket.AF_INET6,%[2]q)
    s=socket.socket(fam,socket.SOCK_DGRAM); s.sendto(b"probe",(addr,9999)); s.close()
    time.sleep(0.001)
took=time.monotonic()-start
c.sendall(b"GET / HTTP/1.0\r\n\r\n")
print(took, c.recv(64).split(b"\r\n")[0].decode())
`

// TestNewConnectionsAreLimited: a network opens at most 300 new connections
// at once, then 100 a second, IPv4 and IPv6 from one bucket; past it they
// are dropped, while a connection already open carries on, and another
// network is not held back.
func TestNewConnectionsAreLimited(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr), IPv6Prefix: prefix6, PublicIPv6: netip.MustParseAddr(hostAddr6)})
	a1 := dualStackGuest(t, d, netA, "a1")
	b1 := dualStackGuest(t, d, netB, "b1")
	c := exec.Command("ip", "netns", "exec", upstreamNS, "nft", "-f", "-")
	c.Stdin = strings.NewReader(probeCounters)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("probe counters: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("ip", "netns", "exec", upstreamNS, "nft", "delete", "table", "inet", "tbprobe").Run()
	})
	probes := func(family string) int {
		out := run(t, "ip", "netns", "exec", upstreamNS, "nft", "reset", "counter", "inet", "tbprobe", family)
		f := strings.Fields(out[strings.Index(out, "packets"):])
		n, _ := strconv.Atoi(f[1])
		return n
	}

	out, err := exec.Command("ip", "netns", "exec", a1.ns, "python3", "-c", fmt.Sprintf(floodScript, upstreamAddr, upstreamAddr6)).CombinedOutput()
	if err != nil {
		t.Fatalf("the flood: %v: %s", err, out)
	}
	// Established, the connection opened first carries on with the bucket
	// empty.
	var took float64
	var status string
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%g %s", &took, &status); err != nil || status != "HTTP/1.0" {
		t.Errorf("the connection open before the flood got %q after it (%v), want an answer", out, err)
	}
	// b1, on another network, opens one at once.
	if got, err := b1.seenAs(); err != nil || got != hostAddr {
		t.Errorf("b1 on another network is held back after a1's flood: %q, %v", got, err)
	}
	v4, v6 := probes("v4"), probes("v6")
	most := 300 + int(100*took) + 5
	t.Logf("600 probes in %.3fs: %d over IPv4 and %d over IPv6 arrived, at most %d allowed", took, v4, v6, most)
	if v4+v6 > most {
		t.Errorf("%d new connections passed in %.3fs, more than the burst of 300 and 100 a second allow (%d): the families do not share a bucket, or there is none", v4+v6, took, most)
	}
	// Sent no faster than the bucket fills, they would all pass: the burst
	// itself must.
	if v4+v6 < 290 {
		t.Errorf("only %d new connections passed, short of the burst of 300", v4+v6)
	}
	if v4 == 0 || v6 == 0 {
		t.Errorf("IPv4 %d, IPv6 %d: a family got nothing through", v4, v6)
	}
	noneForeign(t)
}

// TestNATLog: each new IPv4 connection out of a network is reported with
// the public port the upstream sees, joined with the machine address it
// came from, even between networks whose ranges, and machines' addresses,
// are the same; the NAT log then finds the machine by that port.
func TestNATLog(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	made := ensure(t, d, []driver.NetworkSpec{netA, netC}, nic("a1", netA, 2), nic("c1", netC, 2))
	if made[0].Address != made[1].Address {
		t.Fatalf("a1 has %s and c1 %s: the test needs them the same", made[0].Address, made[1].Address)
	}
	flows := make(chan driver.NATFlow, 1024)
	lost := make(chan driver.NATLost, 16)
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- d.FollowNAT(ctx, driver.NATHandler{
			Started: func(time.Time) { close(started) },
			Flow:    func(f driver.NATFlow) { flows <- f },
			Lost:    func(l driver.NATLost) { lost <- l },
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-errc; err != nil {
			t.Errorf("following the host's connections: %v", err)
		}
	})
	select {
	case <-started:
	case err := <-errc:
		t.Fatalf("following did not start: %v", err)
	}

	// next is the next flow reported that match takes.
	next := func(what string, match func(driver.NATFlow) bool) driver.NATFlow {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case f := <-flows:
				if f.NetworkUID == "" {
					t.Errorf("a connection of no network is reported: %+v", f)
				}
				if match(f) {
					return f
				}
			case l := <-lost:
				t.Errorf("connections lost: %+v", l)
			case err := <-errc:
				t.Fatalf("following stopped: %v", err)
			case <-deadline:
				t.Fatalf("no %s reported", what)
			}
		}
	}
	dir := t.TempDir()
	l, err := natlog.Open(dir, natlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// The agent's names, each machine by its uid, from the interfaces.
	names := natlog.NewNames()
	ifaces, err := d.Interfaces(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []driver.NetworkSpec{netA, netC} {
		net := natlog.Network{Project: "p", Name: n.UID, Machines: map[netip.Addr]natlog.Machine{}}
		for _, i := range ifaces {
			if i.NetworkUID == n.UID {
				net.Machines[i.Address.Addr()] = natlog.Machine{Name: i.MachineUID, UID: i.MachineUID}
			}
		}
		names.Set(n.UID, net)
	}
	upstream := netip.MustParseAddr(upstreamAddr)
	// The host's own connections are never reported.
	run(t, "curl", "-sS", "-m", "3", upstreamURL)
	for _, m := range made {
		g := newGuest(t, m)
		// The same source port from both, so only the join tells them apart.
		out, err := exec.Command("ip", "netns", "exec", g.ns, "curl", "-sS", "-m", "3", "--local-port", "41000",
			"http://"+upstreamAddr+":"+sourcePort+"/").CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", g.ns, err, out)
		}
		seen, err := netip.ParseAddrPort(strings.TrimSpace(string(out)))
		if err != nil || seen.Addr() != netip.MustParseAddr(hostAddr) {
			t.Fatalf("%s is seen upstream as %q (%v), want the host's address", g.ns, out, err)
		}
		f := next("TCP flow from "+g.ns, func(f driver.NATFlow) bool {
			return f.Protocol == unix.IPPROTO_TCP && f.Destination.Addr() == upstream && f.Public == seen
		})
		if f.NetworkUID != m.NetworkUID || f.Inner != netip.AddrPortFrom(m.Address.Addr(), 41000) {
			t.Errorf("%s's connection, seen upstream from %s, is reported from network %q at %s, want %s at %s:41000",
				g.ns, seen, f.NetworkUID, f.Inner, m.NetworkUID, m.Address.Addr())
		}
		if err := l.Write(names.Record(f)); err != nil {
			t.Fatal(err)
		}
		recs, err := natlog.Lookup(dir, natlog.Query{Port: seen.Port(), IP: hostAddr, From: time.Now().Add(-time.Minute), To: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		var found []string
		for _, r := range recs {
			if r.Event == natlog.EventNew {
				found = append(found, r.Machine+" of "+r.Network)
			}
		}
		if want := m.MachineUID + " of " + m.NetworkUID; len(found) == 0 || found[len(found)-1] != want {
			t.Errorf("the NAT log names %q for %s, want %s last", found, seen, want)
		}

		// UDP and a ping are reported too, from the machine's address.
		run(t, "ip", "netns", "exec", g.ns, "python3", "-c", fmt.Sprintf(
			`import socket; s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(("0.0.0.0",41001)); s.sendto(b"x",(%q,9998))`, upstreamAddr))
		u := next("UDP flow from "+g.ns, func(f driver.NATFlow) bool {
			return f.Protocol == unix.IPPROTO_UDP && f.Destination == netip.AddrPortFrom(upstream, 9998) && f.NetworkUID == m.NetworkUID
		})
		if u.Inner != netip.AddrPortFrom(m.Address.Addr(), 41001) || u.Public.Addr() != netip.MustParseAddr(hostAddr) || u.Public.Port() == 0 {
			t.Errorf("%s's UDP is reported from %s as %s, want from %s:41001 as the host's address and a port", g.ns, u.Inner, u.Public, m.Address.Addr())
		}
		if !g.reaches(upstreamAddr) {
			t.Fatalf("%s does not reach the upstream", g.ns)
		}
		p := next("ping from "+g.ns, func(f driver.NATFlow) bool {
			return f.Protocol == unix.IPPROTO_ICMP && f.Destination.Addr() == upstream && f.NetworkUID == m.NetworkUID
		})
		if p.Inner != netip.AddrPortFrom(m.Address.Addr(), 0) {
			t.Errorf("%s's ping is reported from %s, want %s", g.ns, p.Inner, m.Address.Addr())
		}
	}
	_ = l.Close("test")
}
