package dhcp

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

var (
	known   = net.HardwareAddr{0x02, 0, 0, 0, 0, 0x01}
	unknown = net.HardwareAddr{0x02, 0, 0, 0, 0, 0x99}
	leases  = Leases{
		Gateway: netip.MustParseAddr("10.0.0.1"),
		Search:  []string{"web.hangar.internal"},
		ByMAC:   map[string]Lease{"02:00:00:00:00:01": {Addr: netip.MustParsePrefix("10.0.0.5/24"), Hostname: "api"}},
	}
)

func newServer(t *testing.T) *Server {
	t.Helper()
	s := New(slog.New(slog.DiscardHandler))
	s.Set("n1", leases)
	return s
}

func request(t *testing.T, mac net.HardwareAddr, typ dhcpv4.MessageType, mods ...dhcpv4.Modifier) *dhcpv4.DHCPv4 {
	t.Helper()
	m, err := dhcpv4.New(append([]dhcpv4.Modifier{dhcpv4.WithHwAddr(mac), dhcpv4.WithMessageType(typ)}, mods...)...)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAKnownMachineGetsItsAddress(t *testing.T) {
	s := newServer(t)
	offer := s.Reply("n1", request(t, known, dhcpv4.MessageTypeDiscover))
	if offer == nil || offer.MessageType() != dhcpv4.MessageTypeOffer {
		t.Fatalf("discover answered with %v", offer)
	}
	if !offer.YourIPAddr.Equal(net.IPv4(10, 0, 0, 5)) || offer.SubnetMask().String() != "ffffff00" {
		t.Errorf("offered %s/%s", offer.YourIPAddr, offer.SubnetMask())
	}
	gw := net.IPv4(10, 0, 0, 1).To4()
	if r := offer.Router(); len(r) != 1 || !r[0].Equal(gw) {
		t.Errorf("router %v", r)
	}
	if d := offer.DNS(); len(d) != 1 || !d[0].Equal(gw) {
		t.Errorf("DNS %v", d)
	}
	if !offer.ServerIdentifier().Equal(gw) || offer.HostName() != "api" || offer.IPAddressLeaseTime(0) != LeaseTime {
		t.Errorf("server %s, hostname %q, lease %s", offer.ServerIdentifier(), offer.HostName(), offer.IPAddressLeaseTime(0))
	}
	if search := offer.DomainSearch(); search == nil || !slices.Equal(search.Labels, []string{"web.hangar.internal"}) {
		t.Errorf("search %v", search)
	}

	ack := s.Reply("n1", request(t, known, dhcpv4.MessageTypeRequest, dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.IPv4(10, 0, 0, 5)))))
	if ack == nil || ack.MessageType() != dhcpv4.MessageTypeAck || !ack.YourIPAddr.Equal(net.IPv4(10, 0, 0, 5)) {
		t.Errorf("request answered with %v", ack)
	}
	// A renewal says the address it has.
	renew := request(t, known, dhcpv4.MessageTypeRequest)
	renew.ClientIPAddr = net.IPv4(10, 0, 0, 5)
	if ack := s.Reply("n1", renew); ack == nil || ack.MessageType() != dhcpv4.MessageTypeAck {
		t.Errorf("renewal answered with %v", ack)
	}
}

func TestAnotherAddressIsRefused(t *testing.T) {
	s := newServer(t)
	nak := s.Reply("n1", request(t, known, dhcpv4.MessageTypeRequest, dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.IPv4(10, 0, 0, 9)))))
	if nak == nil || nak.MessageType() != dhcpv4.MessageTypeNak || !nak.YourIPAddr.IsUnspecified() {
		t.Errorf("a request for another address answered with %v", nak)
	}
}

func TestWhatIsNotAnswered(t *testing.T) {
	s := newServer(t)
	for name, tc := range map[string]struct {
		network string
		req     *dhcpv4.DHCPv4
	}{
		"an unknown MAC":      {"n1", request(t, unknown, dhcpv4.MessageTypeDiscover)},
		"a network not known": {"n2", request(t, known, dhcpv4.MessageTypeDiscover)},
		"a release":           {"n1", request(t, known, dhcpv4.MessageTypeRelease)},
		"a decline":           {"n1", request(t, known, dhcpv4.MessageTypeDecline)},
		"a reply from a guest": {"n1", func() *dhcpv4.DHCPv4 {
			m := request(t, known, dhcpv4.MessageTypeOffer)
			m.OpCode = dhcpv4.OpcodeBootReply
			return m
		}()},
	} {
		if r := s.Reply(tc.network, tc.req); r != nil {
			t.Errorf("%s answered with %v", name, r)
		}
	}
	s.Forget("n1")
	if r := s.Reply("n1", request(t, known, dhcpv4.MessageTypeDiscover)); r != nil {
		t.Errorf("a forgotten network answered with %v", r)
	}
}

// TestOverASocket: a client with an address is answered at it; Serve ends
// with its ctx.
func TestOverASocket(t *testing.T) {
	s := newServer(t)
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, "n1", c) }()

	client, err := net.DialUDP("udp4", nil, c.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Write([]byte("not dhcp")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(request(t, known, dhcpv4.MessageTypeDiscover).ToBytes()); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	offer, err := dhcpv4.FromBytes(buf[:n])
	if err != nil || offer.MessageType() != dhcpv4.MessageTypeOffer {
		t.Fatalf("answered %v %v", offer, err)
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve outlived its ctx")
	}
}
