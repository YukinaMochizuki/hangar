//go:build testbed

package network

import (
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/vishvananda/netns"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
)

// sharedAddr is the testbed's shared IPv4, held on the uplink as
// deploy/host holds it: a /32 the host never sends from.
const sharedAddr = "198.51.100.3"

// TestTheMachinesLeaveAsTheSharedAddress: with a shared address, a
// machine's TCP and UDP reach the upstream from it, from a port at or
// above hostnet.SharedPortsFrom, and its ping from it too, while the
// host's own traffic still leaves as the host's address.
func TestTheMachinesLeaveAsTheSharedAddress(t *testing.T) {
	run(t, "ip", "addr", "add", sharedAddr+"/32", "dev", upstreamLink, "preferred_lft", "0")
	t.Cleanup(func() {
		_ = exec.Command("ip", "addr", "del", sharedAddr+"/32", "dev", upstreamLink).Run()
		// The upstream counts the shared address as foreign; the tests
		// after this one start from none.
		count(t, upstreamNS, "foreign")
	})
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr), SharedIPv4: netip.MustParseAddr(sharedAddr)})
	g := newGuest(t, ensure(t, d, []driver.NetworkSpec{netA}, nic("a1", netA, 2))[0])

	// TCP, from the upstream's answer naming the source it saw.
	out, err := exec.Command("ip", "netns", "exec", g.ns, "curl", "-sS", "-m", "3", "http://"+upstreamAddr+":"+sourcePort+"/").CombinedOutput()
	if err != nil {
		t.Fatalf("the guest's TCP does not reach the upstream: %v: %s", err, out)
	}
	expectShared(t, "TCP", strings.TrimSpace(string(out)))

	// UDP, from a responder in the upstream answering with the source.
	ns, err := netns.GetFromName(upstreamNS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ns.Close() }()
	var pc net.PacketConn
	if err := inNS(ns, func() (err error) {
		pc, err = net.ListenPacket("udp", upstreamAddr+":0")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	go func() {
		buf := make([]byte, 64)
		for {
			_, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo([]byte(from.String()), from)
		}
	}()
	port := strconv.Itoa(pc.LocalAddr().(*net.UDPAddr).Port)
	script := `import socket, sys
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(3)
s.sendto(b"?", (sys.argv[1], int(sys.argv[2])))
print(s.recv(64).decode())`
	out, err = exec.Command("ip", "netns", "exec", g.ns, "python3", "-c", script, upstreamAddr, port).CombinedOutput()
	if err != nil {
		t.Fatalf("the guest's UDP does not reach the upstream: %v: %s", err, out)
	}
	expectShared(t, "UDP", strings.TrimSpace(string(out)))

	// What has no ports leaves as the shared address too.
	if !g.reaches(upstreamAddr) {
		t.Error("the guest's ping does not reach the upstream")
	}

	// The host's own traffic still leaves as the host's address: the shared
	// one is deprecated, so never picked as a source.
	out, err = exec.Command("curl", "-sS", "-m", "3", upstreamURL).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != hostAddr {
		t.Errorf("the host is seen as %q (%v), want %s", out, err, hostAddr)
	}
}

// expectShared fails t unless seen, an address and port the upstream saw,
// is the shared address at a port of the machines'.
func expectShared(t *testing.T, what, seen string) {
	t.Helper()
	ap, err := netip.ParseAddrPort(seen)
	if err != nil {
		t.Fatalf("%s: the upstream answered %q: %v", what, seen, err)
	}
	if ap.Addr().String() != sharedAddr || ap.Port() < hostnet.SharedPortsFrom {
		t.Errorf("%s from the guest is seen from %s, want %s at a port from %d", what, ap, sharedAddr, hostnet.SharedPortsFrom)
	}
}

// Without a shared address the machines leave as the host's own, as they
// always have.
func TestWithoutASharedAddressTheMachinesLeaveAsTheHost(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	g := newGuest(t, ensure(t, d, []driver.NetworkSpec{netA}, nic("a1", netA, 2))[0])
	if seen, err := g.seenAs(); err != nil || seen != hostAddr {
		t.Errorf("the guest is seen as %q (%v), want %s", seen, err, hostAddr)
	}
	noneForeign(t)
}
