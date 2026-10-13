//go:build testbed

package network

import (
	"io"
	"net"
	"net/netip"
	"os/exec"
	"testing"
	"time"

	"github.com/vishvananda/netns"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
)

// TestServiceSockets: a socket the driver opens for a network's service
// answers the network's machines and nothing else: not a machine of another
// network, even one whose range overlaps, and not the host, whether at the
// namespace's transit address or routed to the gateway by its uplink. A
// process inside the namespace itself is answered, as the kernel delivers
// what is sent there to a socket bound to any of its links; only root on
// the host can run one.
func TestServiceSockets(t *testing.T) {
	ctx := t.Context()
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	nics := ensure(t, d, []driver.NetworkSpec{netA, netC}, nic("sa", netA, 2), nic("sc", netC, 2))
	inA, inC := newGuest(t, nics[0]), newGuest(t, nics[1])
	at := netip.AddrPortFrom(netA.CIDR.Addr().Next(), 53)

	udp, _, err := d.ListenUDP(ctx, netA.UID, at)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = udp.Close() }()
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = udp.WriteTo(buf[:n], from)
		}
	}()
	tcp, _, err := d.ListenTCP(ctx, netA.UID, at)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tcp.Close() }()
	go func() {
		for {
			c, err := tcp.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("hi"))
			_ = c.Close()
		}
	}()

	guestNS := func(g *guest) netns.NsHandle {
		ns, err := netns.GetFromName(g.ns)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ns.Close() })
		return ns
	}
	if !answers(t, guestNS(inA), "udp", at) || !answers(t, guestNS(inA), "tcp", at) {
		t.Error("a machine of the network gets no answer")
	}
	if answers(t, guestNS(inC), "udp", at) || answers(t, guestNS(inC), "tcp", at) {
		t.Error("a machine of an overlapping network is answered")
	}
	// The host, reaching the namespace by its uplink: at its transit
	// address, and at the gateway through a route it is given.
	root, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	nsTransit := netA.Transit.Addr().Next()
	if answers(t, root, "udp", netip.AddrPortFrom(nsTransit, 53)) {
		t.Error("the host is answered at the namespace's transit address")
	}
	route := []string{"ip", "route", "add", at.Addr().String() + "/32", "via", nsTransit.String()}
	run(t, route...)
	defer func() { _ = exec.Command("ip", "route", "del", at.Addr().String()+"/32").Run() }()
	if answers(t, root, "udp", at) || answers(t, root, "tcp", at) {
		t.Error("the host is answered at the gateway, by the namespace's uplink")
	}

	// A bridge made again is another instance: the sockets bound to the old
	// one serve nothing, and new ones serve the network.
	before, err := d.Instance(ctx, netA.UID)
	if err != nil {
		t.Fatal(err)
	}
	run(t, "ip", "-n", hostnet.NamespaceOf(netA.UID), "link", "del", "hn-br")
	if got, err := d.Instance(ctx, netA.UID); err != nil || got != "" {
		t.Errorf("a network without its bridge has instance %q %v", got, err)
	}
	ensure(t, d, []driver.NetworkSpec{netA}, nics[0])
	after, err := d.Instance(ctx, netA.UID)
	if err != nil || after == "" || after == before {
		t.Fatalf("a bridge made again: instance %q, before %q, %v", after, before, err)
	}
	if answers(t, guestNS(inA), "udp", at) {
		t.Error("a socket bound to the old bridge still answers")
	}
	_ = udp.Close()
	again, in, err := d.ListenUDP(ctx, netA.UID, at)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if in != after {
		t.Errorf("a socket opened in instance %q, want %q", in, after)
	}
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := again.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = again.WriteTo(buf[:n], from)
		}
	}()
	if !answers(t, guestNS(inA), "udp", at) {
		t.Error("a socket opened on the bridge made again does not answer")
	}
}

// answers says whether a client in ns gets an answer from proto addr.
func answers(t *testing.T, ns netns.NsHandle, proto string, addr netip.AddrPort) bool {
	t.Helper()
	got := false
	err := inNS(ns, func() error {
		c, err := net.DialTimeout(proto, addr.String(), time.Second)
		if err != nil {
			return nil // refused, or nothing on the way
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(time.Second))
		if proto == "udp" {
			if _, err := c.Write([]byte("ping")); err != nil {
				return nil
			}
		}
		buf := make([]byte, 16)
		n, err := c.Read(buf)
		got = n > 0 && (err == nil || err == io.EOF)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}
