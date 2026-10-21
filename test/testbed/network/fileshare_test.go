//go:build testbed

package network

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"os/exec"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

// listenEcho serves TCP echo at addr in the network's namespace, as the
// agent's services listen there, until the test ends.
func listenEcho(t *testing.T, d *hostnet.Driver, networkUID string, addr netip.AddrPort) {
	t.Helper()
	l, _, err := d.ListenTCP(t.Context(), networkUID, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer c.Close(); _, _ = io.Copy(c, c) }(c)
		}
	}()
}

// listenUDP has the network's namespace take UDP at addr, as its
// resolver does, and says from where each datagram came, until the test
// ends.
func listenUDP(t *testing.T, d *hostnet.Driver, networkUID string, addr netip.AddrPort) <-chan netip.Addr {
	t.Helper()
	c, _, err := d.ListenUDP(t.Context(), networkUID, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	from := make(chan netip.Addr, 64)
	go func() {
		b := make([]byte, 64)
		for {
			_, a, err := c.ReadFromUDPAddrPort(b)
			if err != nil {
				return
			}
			select {
			case from <- a.Addr().Unmap():
			default:
			}
		}
	}()
	return from
}

// sendUDP has a guest send a few datagrams to addr.
func sendUDP(t *testing.T, g *guest, addr netip.AddrPort) {
	t.Helper()
	if b, err := exec.Command("ip", "netns", "exec", g.ns, "python3", "-c", fmt.Sprintf(`import socket,time
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)
for i in range(5): s.sendto(b"probe",(%q,%d)); time.sleep(0.1)`, addr.Addr(), addr.Port())).CombinedOutput(); err != nil {
		t.Fatalf("sending UDP: %v: %s", err, b)
	}
}

// arrived says whether a datagram from addr came within a second.
func arrived(from <-chan netip.Addr, addr netip.Addr) bool {
	timeout := time.After(time.Second)
	for {
		select {
		case a := <-from:
			if a == addr {
				return true
			}
		case <-timeout:
			return false
		}
	}
}

// TestAFileShareServerReachesNothing: a file share's server reaches none of
// what every other tap does, its network's services on the gateway among
// them, nor any guest, while its clients reach its 2049; a guest beside it
// reaches what it does not, the test's control.
func TestAFileShareServerReachesNothing(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	srvNIC := nic("srv", netA, 9)
	srvNIC.FileShareServer = true
	made := ensure(t, d, []driver.NetworkSpec{netA}, srvNIC, nic("plain", netA, 3), nic("peer", netA, 4))
	srv, plain, peer := newGuest(t, made[0]), newGuest(t, made[1]), newGuest(t, made[2])
	open := secgroup.Policy{Ingress: []secgroup.Rule{{Protocol: secgroup.All, Peers: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}}, EgressAll: true}
	if err := d.EnsureFirewall(t.Context(), netA.UID, []driver.InterfacePolicy{
		{MachineUID: "srv", Name: "eth0", Policy: open, ShareClients: []netip.Addr{made[1].Address.Addr()}},
		{MachineUID: "plain", Name: "eth0", Policy: open, Shares: []netip.Addr{made[0].Address.Addr()}},
		{MachineUID: "peer", Name: "eth0", Policy: open},
	}); err != nil {
		t.Fatal(err)
	}
	gateway := netA.CIDR.Addr().Next()
	listenEcho(t, d, netA.UID, netip.AddrPortFrom(gateway, 53))
	resolver := listenUDP(t, d, netA.UID, netip.AddrPortFrom(gateway, 53))
	listenEcho(t, d, netA.UID, netip.AddrPortFrom(driver.MetadataAddr, 80))
	echoServer(t, srv, driver.ShareServerPort, 22)
	echoServer(t, plain, 22)
	for _, c := range []struct {
		what string
		from *guest
		to   netip.Addr
		port int
		want bool
	}{
		{"the control: a guest to the resolver over TCP", plain, gateway, 53, true},
		{"the control: a guest to the metadata service", plain, driver.MetadataAddr, 80, true},
		{"a client to its share", plain, made[0].Address.Addr(), driver.ShareServerPort, true},
		{"the server to the resolver over TCP", srv, gateway, 53, false},
		{"the server to the metadata service", srv, driver.MetadataAddr, 80, false},
		{"the server to a guest", srv, made[1].Address.Addr(), 22, false},
		{"a client to the server's 22", plain, made[0].Address.Addr(), 22, false},
	} {
		if got := connects(c.from, c.to.String(), c.port); got != c.want {
			t.Errorf("%s: connected %t, want %t", c.what, got, c.want)
		}
	}
	// What needs no reply: UDP, one way, to the resolver and to a guest
	// open to all.
	sendUDP(t, plain, netip.AddrPortFrom(gateway, 53))
	if !arrived(resolver, made[1].Address.Addr()) {
		t.Error("the control: a guest's UDP does not reach the resolver")
	}
	sendUDP(t, srv, netip.AddrPortFrom(gateway, 53))
	if arrived(resolver, made[0].Address.Addr()) {
		t.Error("the server's UDP reaches the resolver")
	}
	if !udpArrives(t, plain, peer, 40000, 9999) {
		t.Error("the control: a guest's UDP does not reach a guest open to all")
	}
	if udpArrives(t, srv, peer, driver.ShareServerPort, 9999) {
		t.Error("the server's UDP reaches a guest")
	}
	if _, err := plain.seenAs(); err != nil {
		t.Errorf("the control: a guest does not reach the upstream: %v", err)
	}
	if seen, err := srv.seenAs(); err == nil {
		t.Errorf("the server reaches the upstream, seen as %s", seen)
	}
	if !plain.reaches(gateway.String()) {
		t.Error("the control: a guest does not ping its gateway")
	}
	if srv.reaches(gateway.String()) {
		t.Error("the server pings its gateway")
	}
}

// TestAServerTapWaitsForItsFirewall: on a network whose firewall an older
// driver wrote, which would give a server's tap what every tap has, a
// server's interface is refused until the firewall is written again;
// another's is not.
func TestAServerTapWaitsForItsFirewall(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink})
	ensure(t, d, []driver.NetworkSpec{netA})
	if err := hostnet.WriteFirewallOfOld(netA.UID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnsureInterface(t.Context(), nic("plain", netA, 3)); err != nil {
		t.Errorf("an interface of no server refused: %v", err)
	}
	srv := nic("srv", netA, 9)
	srv.FileShareServer = true
	if _, err := d.EnsureInterface(t.Context(), srv); err == nil {
		t.Error("a server's interface made behind a firewall with no chains for it")
	}
	if err := d.EnsureFirewall(t.Context(), netA.UID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnsureInterface(t.Context(), srv); err != nil {
		t.Errorf("a server's interface refused once the firewall is written again: %v", err)
	}
}
