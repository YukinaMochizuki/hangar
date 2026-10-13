package netsvc_test

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
	"github.com/YukinaMochizuki/hangar/internal/node/netsvc"
)

var (
	spec = driver.NetworkSpec{
		UID: "n1", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.0/31"),
	}
	n1   = netsvc.Network{UID: "n1", Gateway: netip.MustParseAddr("10.0.0.1")}
	echo = netip.AddrPortFrom(n1.Gateway, 7)
)

// echoService answers each UDP datagram with itself, and each TCP
// connection with "hi".
func echoService(addr func(netsvc.Network) netip.AddrPort) netsvc.Service {
	return netsvc.Service{
		Name: "echo",
		Addr: addr,
		ServeUDP: func(_ context.Context, _ netsvc.Network, c *net.UDPConn) {
			buf := make([]byte, 512)
			for {
				n, from, err := c.ReadFrom(buf)
				if err != nil {
					return
				}
				_, _ = c.WriteTo(buf[:n], from)
			}
		},
		ServeTCP: func(_ context.Context, _ netsvc.Network, l *net.TCPListener) {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				_, _ = c.Write([]byte("hi"))
				_ = c.Close()
			}
		},
	}
}

func setup(t *testing.T, services ...netsvc.Service) (*fake.Host, *netsvc.Manager) {
	t.Helper()
	h := fake.New()
	if err := h.Network().EnsureNetwork(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if len(services) == 0 {
		services = []netsvc.Service{echoService(func(n netsvc.Network) netip.AddrPort { return netip.AddrPortFrom(n.Gateway, 7) })}
	}
	m := netsvc.New(h.Network(), slog.New(slog.DiscardHandler), services...)
	t.Cleanup(m.CloseAll)
	return h, m
}

func answersUDP(t *testing.T, h *fake.Host) bool {
	t.Helper()
	addr, ok := h.ServiceAddr("n1", "udp", echo)
	if !ok {
		return false
	}
	c, err := net.Dial("udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		return false
	}
	buf := make([]byte, 16)
	n, err := c.Read(buf)
	return err == nil && string(buf[:n]) == "ping"
}

func TestEnsureServes(t *testing.T) {
	h, m := setup(t)
	if err := m.Ensure(t.Context(), n1); err != nil {
		t.Fatal(err)
	}
	if !answersUDP(t, h) {
		t.Error("the service does not answer over UDP")
	}
	addr, ok := h.ServiceAddr("n1", "tcp", echo)
	if !ok {
		t.Fatal("nothing listens on TCP")
	}
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if n, _ := c.Read(buf); string(buf[:n]) != "hi" {
		t.Errorf("TCP answered %q", buf[:n])
	}
	_ = c.Close()

	// Ensuring again changes nothing.
	before := h.Snapshot().Listening
	if err := m.Ensure(t.Context(), n1); err != nil {
		t.Fatal(err)
	}
	if after := h.Snapshot().Listening; !slices.Equal(before["n1"], after["n1"]) || len(after["n1"]) != 2 {
		t.Errorf("listening %v, then %v after ensuring again", before, after)
	}
	if got := m.Serving(); !slices.Equal(got, []string{"n1"}) {
		t.Errorf("serving %v", got)
	}
}

func TestANamespaceMadeAgainIsServedAgain(t *testing.T) {
	h, m := setup(t)
	if err := m.Ensure(t.Context(), n1); err != nil {
		t.Fatal(err)
	}
	h.RemakeNamespace("n1")
	if answersUDP(t, h) {
		t.Fatal("a socket of the old namespace serves the new one")
	}
	if err := m.Ensure(t.Context(), n1); err != nil {
		t.Fatal(err)
	}
	if !answersUDP(t, h) {
		t.Error("the service does not answer in the namespace made again")
	}
	if l := h.Snapshot().Listening["n1"]; len(l) != 2 {
		t.Errorf("listening %v: the old namespace's sockets were left open", l)
	}
}

func TestANetworkLeavingTheHostIsNoLongerServed(t *testing.T) {
	h, m := setup(t)
	if err := m.Ensure(t.Context(), netsvc.Network{UID: "absent", Gateway: n1.Gateway}); err != nil {
		t.Errorf("a network not on the host: %v", err)
	}
	if err := m.Ensure(t.Context(), n1); err != nil {
		t.Fatal(err)
	}
	h.LoseNetwork("n1")
	if err := m.Ensure(t.Context(), n1); err != nil {
		t.Fatal(err)
	}
	if s := h.Snapshot(); len(s.Listening) != 0 || len(m.Serving()) != 0 {
		t.Errorf("listening %v, serving %v, for a network gone from the host", s.Listening, m.Serving())
	}

	if err := h.Network().EnsureNetwork(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := m.Ensure(t.Context(), n1); err != nil {
		t.Fatal(err)
	}
	m.Keep(func(uid string) bool { return uid != "n1" })
	if s := h.Snapshot(); len(s.Listening) != 0 || len(m.Serving()) != 0 {
		t.Errorf("listening %v, serving %v, after keeping none", s.Listening, m.Serving())
	}
}

func TestAFailedOpenLeavesNothing(t *testing.T) {
	ok := echoService(func(n netsvc.Network) netip.AddrPort { return netip.AddrPortFrom(n.Gateway, 7) })
	bad := echoService(func(netsvc.Network) netip.AddrPort { return netip.MustParseAddrPort("192.0.2.1:9") })
	bad.Name = "bad"
	h, m := setup(t, ok, bad)
	if err := m.Ensure(t.Context(), n1); err == nil {
		t.Fatal("a service on an address the network lacks was opened")
	}
	if s := h.Snapshot(); len(s.Listening) != 0 || len(m.Serving()) != 0 {
		t.Errorf("listening %v, serving %v, after a failed open", s.Listening, m.Serving())
	}
}

func TestAnotherGatewayIsServedThere(t *testing.T) {
	h, m := setup(t)
	if err := m.Ensure(t.Context(), n1); err != nil {
		t.Fatal(err)
	}
	other := spec
	other.CIDR = netip.MustParsePrefix("10.9.0.0/24")
	if err := h.Network().EnsureNetwork(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	moved := netsvc.Network{UID: "n1", Gateway: netip.MustParseAddr("10.9.0.1")}
	if err := m.Ensure(t.Context(), moved); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.ServiceAddr("n1", "udp", echo); ok {
		t.Error("still listening on the old address")
	}
	if _, ok := h.ServiceAddr("n1", "udp", netip.AddrPortFrom(moved.Gateway, 7)); !ok {
		t.Error("not listening on the new address")
	}
}

// TestStoppingWaitsForNoClient: a service told to stop by its ctx, as the
// resolver is, ends its network's serving at once, whatever a client holds.
func TestStoppingWaitsForNoClient(t *testing.T) {
	stuck := make(chan struct{})
	defer close(stuck)
	svc := netsvc.Service{
		Name: "holder",
		Addr: func(n netsvc.Network) netip.AddrPort { return netip.AddrPortFrom(n.Gateway, 7) },
		ServeTCP: func(ctx context.Context, _ netsvc.Network, l *net.TCPListener) {
			go func() {
				if c, err := l.Accept(); err == nil {
					<-stuck // a client keeping its connection busy
					_ = c.Close()
				}
			}()
			<-ctx.Done()
		},
	}
	h, m := setup(t, svc)
	if err := m.Ensure(t.Context(), n1); err != nil {
		t.Fatal(err)
	}
	addr, _ := h.ServiceAddr("n1", "tcp", echo)
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	closed := make(chan struct{})
	go func() { m.Close("n1"); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("closing a network waits on a client's connection")
	}
}
