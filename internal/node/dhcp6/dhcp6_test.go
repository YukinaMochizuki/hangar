package dhcp6

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/iana"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/ipv6pkt"
)

var (
	clientMAC  = net.HardwareAddr{0x02, 0, 0, 0, 0, 1}
	clientLL   = netip.MustParseAddr("fe80::5054:ff:fe00:1")
	clientAddr = netip.MustParseAddr("2001:db8:0:1:8a3f:12c4:9e01:77b2")
	strangerM  = net.HardwareAddr{0x02, 0, 0, 0, 0, 9}
)

func server() *Server {
	s := New(slog.New(slog.DiscardHandler))
	s.Set("net", map[string]netip.Addr{clientMAC.String(): clientAddr})
	return s
}

func solicit(t *testing.T, mods ...dhcpv6.Modifier) *dhcpv6.Message {
	t.Helper()
	m, err := dhcpv6.NewSolicit(clientMAC, mods...)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// leased returns the address the reply's first IA_NA holds.
func leased(t *testing.T, m *dhcpv6.Message) netip.Addr {
	t.Helper()
	ia := m.Options.OneIANA()
	if ia == nil || ia.Options.OneAddress() == nil {
		t.Fatalf("no address in %s", m)
	}
	a, _ := netip.AddrFromSlice(ia.Options.OneAddress().IPv6Addr)
	return a
}

func TestAMachineIsLeasedItsAddress(t *testing.T) {
	s := server()
	sol := solicit(t)
	adv := s.Reply("net", clientMAC, sol)
	if adv == nil || adv.Type() != dhcpv6.MessageTypeAdvertise || leased(t, adv) != clientAddr {
		t.Fatalf("advertise: %v", adv)
	}
	if adv.Options.OneIANA().IaId != sol.Options.OneIANA().IaId || !adv.Options.ServerID().Equal(ServerID("net")) {
		t.Errorf("advertise does not answer the client's IA or name the server: %s", adv)
	}
	if len(adv.Options.DNS()) != 0 {
		t.Errorf("a lease carries DNS servers, which stay the network's IPv4 resolver: %v", adv.Options.DNS())
	}
	req, err := dhcpv6.NewRequestFromAdvertise(adv)
	if err != nil {
		t.Fatal(err)
	}
	if rep := s.Reply("net", clientMAC, req); rep == nil || rep.Type() != dhcpv6.MessageTypeReply || leased(t, rep) != clientAddr {
		t.Errorf("request answered with %v", rep)
	}
	if rc := s.Reply("net", clientMAC, solicit(t, dhcpv6.WithRapidCommit)); rc == nil || rc.Type() != dhcpv6.MessageTypeReply || leased(t, rc) != clientAddr {
		t.Errorf("rapid commit answered with %v", rc)
	}

	renew := *req
	renew.MessageType = dhcpv6.MessageTypeRenew
	if rep := s.Reply("net", clientMAC, &renew); rep == nil || leased(t, rep) != clientAddr {
		t.Errorf("renew answered with %v", rep)
	}
	other, _ := dhcpv6.NewRequestFromAdvertise(adv, dhcpv6.WithServerID(&dhcpv6.DUIDLL{HWType: iana.HWTypeEthernet, LinkLayerAddr: strangerM}))
	other.UpdateOption(dhcpv6.OptServerID(&dhcpv6.DUIDLL{HWType: iana.HWTypeEthernet, LinkLayerAddr: strangerM}))
	if rep := s.Reply("net", clientMAC, other); rep != nil {
		t.Errorf("a request for another server answered: %s", rep)
	}

	confirm, _ := dhcpv6.NewMessage()
	confirm.MessageType = dhcpv6.MessageTypeConfirm
	confirm.AddOption(dhcpv6.OptClientID(sol.Options.ClientID()))
	confirm.AddOption(&dhcpv6.OptIANA{IaId: [4]byte{1}, Options: dhcpv6.IdentityOptions{Options: dhcpv6.Options{
		&dhcpv6.OptIAAddress{IPv6Addr: net.ParseIP("2001:db8:0:1::99")},
	}}})
	if rep := s.Reply("net", clientMAC, confirm); rep == nil || rep.Options.Status() == nil || rep.Options.Status().StatusCode != iana.StatusNotOnLink {
		t.Errorf("confirming another address answered with %v", rep)
	}
}

// TestARenewMovesTheAddress: a client renewing an address no longer its
// own, as after an Address moved, is given its address now and the old one
// with lifetimes of 0, and renews again within RenewTime.
func TestARenewMovesTheAddress(t *testing.T) {
	s := server()
	old := netip.MustParseAddr("2001:db8:0:1:1:2:3:4")
	sol := solicit(t)
	renew, _ := dhcpv6.NewMessage()
	renew.MessageType = dhcpv6.MessageTypeRenew
	renew.AddOption(dhcpv6.OptClientID(sol.Options.ClientID()))
	renew.AddOption(dhcpv6.OptServerID(ServerID("net")))
	renew.AddOption(&dhcpv6.OptIANA{IaId: [4]byte{7}, Options: dhcpv6.IdentityOptions{Options: dhcpv6.Options{
		&dhcpv6.OptIAAddress{IPv6Addr: old.AsSlice(), PreferredLifetime: LeaseTime, ValidLifetime: LeaseTime},
	}}})
	rep := s.Reply("net", clientMAC, renew)
	if rep == nil {
		t.Fatal("no answer to a renew")
	}
	ia := rep.Options.OneIANA()
	if ia.T1 != RenewTime || ia.T2 != RebindTime || RenewTime > RebindTime || RebindTime > LeaseTime {
		t.Errorf("T1 %v, T2 %v", ia.T1, ia.T2)
	}
	got := map[netip.Addr][2]time.Duration{}
	for _, a := range ia.Options.Addresses() {
		addr, _ := netip.AddrFromSlice(a.IPv6Addr)
		got[addr] = [2]time.Duration{a.PreferredLifetime, a.ValidLifetime}
	}
	if got[clientAddr] != [2]time.Duration{LeaseTime, LeaseTime} {
		t.Errorf("the client's address: %v", got)
	}
	if l, ok := got[old]; !ok || l != [2]time.Duration{} {
		t.Errorf("the address it held is not given back with lifetimes of 0: %v", got)
	}
}

func TestAStrangerIsNotAnswered(t *testing.T) {
	s := server()
	if rep := s.Reply("net", strangerM, solicit(t)); rep != nil {
		t.Errorf("a MAC the network does not know was answered: %s", rep)
	}
	if rep := s.Reply("other-net", clientMAC, solicit(t)); rep != nil {
		t.Errorf("a network not known answered: %s", rep)
	}
	s.Forget("net")
	if rep := s.Reply("net", clientMAC, solicit(t)); rep != nil {
		t.Errorf("a forgotten network answered: %s", rep)
	}
}

// pipe is a PacketConn whose reads come from in and writes go to out.
type pipe struct {
	in       chan frame
	out      chan frame
	deadline chan struct{}
}

type frame struct {
	b   []byte
	mac net.HardwareAddr
}

func newPipe() *pipe {
	return &pipe{in: make(chan frame, 256), out: make(chan frame, 256), deadline: make(chan struct{})}
}

func (p *pipe) ReadPacket(b []byte) (int, net.HardwareAddr, error) {
	select {
	case f := <-p.in:
		return copy(b, f.b), f.mac, nil
	case <-p.deadline:
		return 0, nil, os.ErrDeadlineExceeded
	}
}

func (p *pipe) WritePacket(b []byte, to net.HardwareAddr) error {
	p.out <- frame{append([]byte(nil), b...), to}
	return nil
}

func (p *pipe) SetReadDeadline(time.Time) error { close(p.deadline); return nil }
func (p *pipe) Close() error                    { return nil }

var _ driver.PacketConn = (*pipe)(nil)

func packet(m *dhcpv6.Message) []byte {
	return ipv6pkt.Packet{
		Src: clientLL, Dst: netip.MustParseAddr("ff02::1:2"), Next: ipv6pkt.UDP, HopLimit: 1,
		Payload: ipv6pkt.UDPDatagram(546, 547, m.ToBytes()),
	}.Marshal()
}

func TestServeAnswersBoundedByMAC(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := server()
		at := time.Unix(1000, 0)
		s.now = func() time.Time { return at }
		p := newPipe()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- s.Serve(ctx, "net", p) }()
		sol := solicit(t)
		for range 3 * PerMAC {
			p.in <- frame{packet(sol), clientMAC}
		}
		p.in <- frame{packet(sol), strangerM}
		first := <-p.out
		got, err := ipv6pkt.Parse(first.b)
		if err != nil {
			t.Fatal(err)
		}
		if got.Src != driver.GatewayIPv6 || got.Dst != clientLL || port(got.Payload[0:2]) != 547 || port(got.Payload[2:4]) != 546 || first.mac.String() != clientMAC.String() {
			t.Errorf("answer from %s:%d to %s:%d at %s, want fe80::1:547 to the client's %s:546 at its MAC",
				got.Src, port(got.Payload[0:2]), got.Dst, port(got.Payload[2:4]), first.mac, clientLL)
		}
		// Every solicitation is read and answered or dropped.
		synctest.Wait()
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if n := 1 + len(p.out); n != PerMAC {
			t.Errorf("%d answers to %d solicitations of one MAC in a second, want %d", n, 3*PerMAC, PerMAC)
		}
	})
}
