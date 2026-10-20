//go:build testbed

package network

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/dhcp"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// sniffer records what a guest's eth0 receives of unicast for another MAC:
// what its tap was flooded with, or sent of another's. It also notes the
// markers it receives, which take waits for.
type sniffer struct {
	g, via  *guest
	mu      sync.Mutex
	foreign []string
	markers map[uint16]bool
	next    uint16
}

// sniff starts recording on g's eth0 until the test ends; via is the guest
// take sends its markers from.
func sniff(t *testing.T, g, via *guest) *sniffer {
	t.Helper()
	ns, err := netns.GetFromName(g.ns)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ns.Close() }()
	all := int(htons(unix.ETH_P_ALL))
	fd := -1
	if err := inNS(ns, func() error {
		ifi, err := net.InterfaceByName("eth0")
		if err != nil {
			return err
		}
		if fd, err = unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, all); err != nil {
			return err
		}
		return unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: ifi.Index})
	}); err != nil {
		t.Fatal(err)
	}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Usec: 100_000}); err != nil {
		t.Fatal(err)
	}
	s := &sniffer{g: g, via: via, markers: map[uint16]bool{}}
	from := via.nic.Address.Addr().As4()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 65536)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, sa, err := unix.Recvfrom(fd, buf, 0)
			if err != nil || n < 14 {
				continue
			}
			ll, _ := sa.(*unix.SockaddrLinklayer)
			if ll == nil {
				continue
			}
			s.mu.Lock()
			switch ip := buf[14:n]; {
			// The kernel tells a frame for another MAC as for another host.
			case ll.Pkttype == unix.PACKET_OTHERHOST:
				s.foreign = append(s.foreign, fmt.Sprintf("%s to %s, type %#04x", net.HardwareAddr(buf[6:12]), net.HardwareAddr(buf[:6]), uint16(buf[12])<<8|uint16(buf[13])))
			case ll.Pkttype == unix.PACKET_HOST && buf[12] == 0x08 && buf[13] == 0 && len(ip) >= 20 && ip[9] == 1 &&
				binary.BigEndian.Uint16(ip[2:]) == markerLength && [4]byte(ip[12:16]) == from:
				s.markers[binary.BigEndian.Uint16(ip[4:])] = true
			}
			s.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
		_ = unix.Close(fd)
	})
	return s
}

// markerLength is the IP length of the markers, a marked ping's.
const markerLength = 1139

// take returns what was recorded since it was last asked. It first sends a
// marker from via to the sniffing guest, a marked ping with an IP id of its
// own, and waits until it arrives: what was flooded to the guest's tap
// before it is queued ahead of it.
func (s *sniffer) take(t *testing.T) []string {
	t.Helper()
	s.mu.Lock()
	s.next++
	id := s.next
	s.mu.Unlock()
	p := markedPing(s.via.nic.Address.Addr(), s.g.nic.Address.Addr())
	binary.BigEndian.PutUint16(p[4:], id)
	binary.BigEndian.PutUint16(p[10:], 0)
	binary.BigEndian.PutUint16(p[10:], checksum(p[:20]))
	sendFrame(t, s.via, ethernet(mustMAC(s.g.nic.MAC), mustMAC(s.via.nic.MAC), 0x0800, p))
	testutil.Eventually(t, fmt.Sprintf("marker %d at %s", id, s.g.ns), func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.markers[id]
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	got := s.foreign
	s.foreign = nil
	return got
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// serveDHCP runs a network's DHCP on d's socket, as the agent does,
// leasing each interface its address.
func serveDHCP(t *testing.T, d *hostnet.Driver, n driver.NetworkSpec, nics ...driver.Interface) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c, _, err := d.ListenUDP(ctx, n.UID, netip.AddrPortFrom(netip.IPv4Unspecified(), 67))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	leases := dhcp.Leases{Gateway: n.CIDR.Addr().Next(), ByMAC: map[string]dhcp.Lease{}}
	for _, i := range nics {
		leases.ByMAC[strings.ToLower(i.MAC)] = dhcp.Lease{Addr: i.Address, Hostname: i.MachineUID}
	}
	srv := dhcp.New(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	srv.Set(n.UID, leases)
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, n.UID, c); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = c.Close()
	})
}

// dhcp4 has a real DHCP client, dhcpcd, ask for the guest's IPv4 address
// from no address, by broadcast, as a cloud image does, and returns the
// address it was leased.
func (g *guest) dhcp4(t *testing.T) netip.Addr {
	t.Helper()
	// -A: no ARP probe of an address the guest holds already.
	out, err := exec.Command("ip", "netns", "exec", g.ns, "timeout", "60",
		"dhcpcd", "-f", "/dev/null", "-c", "/bin/true", "-4", "-A", "-B", "-1", "-p", "-t", "30", "eth0").CombinedOutput()
	if err != nil {
		t.Fatalf("dhcpcd in %s: %v: %s", g.ns, err, out)
	}
	m := regexp.MustCompile(`leased (\S+)`).FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("dhcpcd in %s leased nothing: %s", g.ns, out)
	}
	return netip.MustParseAddr(m[1])
}

// portWhole says what is not as the driver makes it of an interface's tap
// as a bridge port: no learning, no unicast flooding, broadcast and
// multicast flooding, and a static entry for its MAC.
func portWhole(t *testing.T, i driver.Interface) error {
	t.Helper()
	ns := hostnet.NamespaceOf(i.NetworkUID)
	link := run(t, "ip", "-n", ns, "-d", "link", "show", "dev", i.Device)
	for _, want := range []string{"learning off", "flood off", "mcast_flood on", "bcast_flood on"} {
		if !regexp.MustCompile(`\b` + want + `\b`).MatchString(link) {
			return fmt.Errorf("tap %s is not %q:\n%s", i.Device, want, link)
		}
	}
	fdb := run(t, "ip", "netns", "exec", ns, "bridge", "fdb", "show", "dev", i.Device)
	if !strings.Contains(fdb, strings.ToLower(i.MAC)+" master hn-br static") {
		return fmt.Errorf("tap %s has no static entry for %s:\n%s", i.Device, i.MAC, fdb)
	}
	return nil
}

// TestATapReceivesOnlyItsOwnUnicast runs three guests on one network, the
// third sniffing its eth0: none of the others' unicast reaches it, not even
// while the bridge's forwarding database is flushed, and ARP, DHCP,
// neighbour discovery and DHCPv6, which need broadcast and multicast,
// still work.
func TestATapReceivesOnlyItsOwnUnicast(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr), IPv6Prefix: prefix6, PublicIPv6: netip.MustParseAddr(hostAddr6)})
	nets := []driver.NetworkSpec{netA}
	nics := []driver.Interface{nic6("a1", netA, 2), nic6("a2", netA, 3), nic6("a3", netA, 4)}
	made := ensure(t, d, nets, nics...)
	var policies []driver.InterfacePolicy
	for _, i := range made {
		policies = append(policies, policy6(i, "::/0"))
	}
	if err := d.EnsureFirewall(t.Context(), netA.UID, policies); err != nil {
		t.Fatal(err)
	}
	serveIPv6(t, d, netA, nics...)
	serveDHCP(t, d, netA, nics...)
	a1, a2, a3 := newGuest(t, made[0]), newGuest(t, made[1]), newGuest(t, made[2])
	ns := hostnet.NamespaceOf(netA.UID)
	gateway := netA.CIDR.Addr().Next().String()
	s := sniff(t, a3, a1)
	noneSeen := func(t *testing.T, while string) {
		t.Helper()
		if got := s.take(t); len(got) > 0 {
			t.Errorf("%s, a3 received %d frames of unicast for others: %q", while, len(got), got)
			logHost(t)
		}
	}
	// flushAndTalk flushes the bridge's forwarding database, puts back a3's
	// entry alone, so the sniffer's markers reach it, and has the others
	// send what the bridge would flood to every port that takes unknown
	// unicast: a1's frames for a2, a2's for the gateway, the namespace's
	// for a2, and a frame for a MAC no tap has.
	flushAndTalk := func(t *testing.T) {
		t.Helper()
		run(t, "ip", "netns", "exec", ns, "bridge", "fdb", "flush", "dev", "hn-br")
		if fdb := run(t, "ip", "netns", "exec", ns, "bridge", "fdb", "show", "br", "hn-br"); strings.Contains(fdb, strings.ToLower(a2.nic.MAC)) {
			t.Fatalf("a2's entry outlived the flush; it tests nothing:\n%s", fdb)
		}
		run(t, "ip", "netns", "exec", ns, "bridge", "fdb", "add", strings.ToLower(a3.nic.MAC), "dev", a3.nic.Device, "master", "static")
		_ = exec.Command("ip", "netns", "exec", a1.ns, "ping", "-c", "3", "-i", "0.2", "-W", "1", a2.nic.Address.Addr().String()).Run()
		_ = exec.Command("ip", "netns", "exec", a2.ns, "ping", "-c", "3", "-i", "0.2", "-W", "1", gateway).Run()
		_ = exec.Command("ip", "netns", "exec", ns, "ping", "-c", "3", "-i", "0.2", "-W", "1", a2.nic.Address.Addr().String()).Run()
		sendFrame(t, a1, ethernet(mustMAC("02:00:00:00:99:99"), mustMAC(a1.nic.MAC), 0x0800, markedPing(a1.nic.Address.Addr(), netip.MustParseAddr("10.0.0.99"))))
	}
	// whole ensures the network again, as the agent does on the signal, and
	// checks every tap is a port as the driver makes it.
	whole := func(t *testing.T) {
		t.Helper()
		ensure(t, d, nets, nics...)
		if err := d.EnsureFirewall(t.Context(), netA.UID, policies); err != nil {
			t.Fatal(err)
		}
		for _, i := range made {
			if err := portWhole(t, i); err != nil {
				t.Errorf("after ensuring again: %v", err)
			}
		}
	}

	t.Run("each tap is a port receiving only its own", func(t *testing.T) {
		for _, i := range made {
			if err := portWhole(t, i); err != nil {
				t.Error(err)
			}
		}
	})
	t.Run("DHCP", func(t *testing.T) {
		for _, g := range []*guest{a1, a2} {
			if got := g.dhcp4(t); got != g.nic.Address.Addr() {
				t.Errorf("%s was leased %s by DHCP, want %s", g.ns, got, g.nic.Address.Addr())
			}
		}
	})
	t.Run("RA and DHCPv6", func(t *testing.T) {
		for _, g := range []*guest{a1, a2} {
			if got := g.dhcp6(t); got != g.nic.IPv6 {
				t.Errorf("%s got %s by DHCPv6, want %s", g.ns, got, g.nic.IPv6)
			}
		}
	})
	t.Run("ARP and neighbour discovery", func(t *testing.T) {
		for _, n := range []string{a1.ns, a2.ns, ns} {
			run(t, "ip", "-n", n, "neigh", "flush", "all")
		}
		if !a1.reaches(a2.nic.Address.Addr().String()) {
			t.Error("a1 does not reach a2 over IPv4")
		}
		if !a1.reaches6(a2.nic.IPv6.String()) {
			t.Error("a1 does not reach a2 over IPv6")
		}
		if !a2.reaches(gateway) {
			t.Error("a2 does not reach the gateway")
		}
		if got := run(t, "ip", "-n", a1.ns, "neigh", "show", "dev", "eth0"); !strings.Contains(got, strings.ToLower(a2.nic.MAC)) {
			t.Errorf("a1 did not resolve a2's MAC %s: %s", a2.nic.MAC, got)
		}
	})
	noneSeen(t, "while the others leased and talked")

	t.Run("nor right after the forwarding database is flushed", func(t *testing.T) {
		settle(d)
		flushAndTalk(t)
		noneSeen(t, "with the forwarding database flushed")
		changed(t, d, "the flush")
		whole(t)
		if !a1.reaches(a2.nic.Address.Addr().String()) {
			t.Error("after ensuring again, a1 does not reach a2")
		}
		if !a2.reaches(gateway) {
			t.Error("after ensuring again, a2 does not reach the gateway")
		}
		noneSeen(t, "after ensuring again")
	})

	// The control: with a3's tap taking flooded unknown unicast, the same
	// traffic reaches it, so the sniffer would see a leak.
	t.Run("a tap taking flooded unicast sees the others'", func(t *testing.T) {
		settle(d)
		run(t, "ip", "-n", ns, "link", "set", a3.nic.Device, "type", "bridge_slave", "flood", "on")
		flushAndTalk(t)
		if got := s.take(t); len(got) == 0 {
			t.Error("a3's tap took flooded unicast, yet a3 recorded none of the others'; the sniffer sees nothing")
		} else {
			t.Logf("with flooding on, a3 received %d frames of unicast for others: %q", len(got), got)
		}
		whole(t)
		if !a2.reaches(gateway) {
			t.Error("after ensuring again, a2 does not reach the gateway")
		}
		noneSeen(t, "with flooding off again")
	})

	// One MAC's frames go to one tap: neither another interface's MAC nor
	// the gateway's is taken.
	t.Run("no interface takes another's MAC or the gateway's", func(t *testing.T) {
		for _, mac := range []string{a2.nic.MAC, macOf(t, ns, "hn-br")} {
			twin := nic6("a4", netA, 5)
			twin.MAC = mac
			if _, err := d.EnsureInterface(t.Context(), twin); !errors.Is(err, driver.ErrMACInUse) {
				t.Errorf("an interface given %s: %v, want ErrMACInUse", mac, err)
			}
		}
		if ifs, err := d.Interfaces(t.Context()); err != nil || len(ifs) != 3 {
			t.Errorf("after refusing MACs, interfaces %+v %v", ifs, err)
		}
	})

	// The binding drops a frame from another's MAC before the bridge could
	// learn from it, and the bridge learns nothing on a tap anyway.
	t.Run("a guest sending as another moves no entry", func(t *testing.T) {
		gw := mustMAC(macOf(t, ns, "hn-br"))
		a2MAC := mustMAC(a2.nic.MAC)
		for range 3 {
			sendFrame(t, a1, ethernet(gw, a2MAC, 0x0806, arpReply(a2MAC, a2.nic.Address.Addr(), netA.CIDR.Addr().Next())))
		}
		if err := portWhole(t, made[1]); err != nil {
			t.Error(err)
		}
		if fdb := run(t, "ip", "netns", "exec", ns, "bridge", "fdb", "show", "dev", a1.nic.Device); strings.Contains(fdb, strings.ToLower(a2.nic.MAC)) {
			t.Errorf("a2's MAC moved to a1's tap:\n%s", fdb)
		}
		if !a3.reaches(a2.nic.Address.Addr().String()) {
			t.Error("a3 does not reach a2")
		}
		noneSeen(t, "after a1 sent as a2")
	})
}
