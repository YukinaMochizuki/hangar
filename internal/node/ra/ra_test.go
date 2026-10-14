package ra

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/ipv6pkt"
)

// pipe is a PacketConn whose reads come from in and writes go to out.
type pipe struct {
	in       chan []byte
	out      chan []byte
	deadline chan struct{}
}

func (p *pipe) ReadPacket(b []byte) (int, net.HardwareAddr, error) {
	select {
	case f := <-p.in:
		return copy(b, f), net.HardwareAddr{2, 0, 0, 0, 0, 1}, nil
	case <-p.deadline:
		return 0, nil, os.ErrDeadlineExceeded
	}
}

func (p *pipe) WritePacket(b []byte, to net.HardwareAddr) error {
	if to.String() != "33:33:00:00:00:01" {
		panic("an advertisement to " + to.String())
	}
	p.out <- append([]byte(nil), b...)
	return nil
}

func (p *pipe) SetReadDeadline(time.Time) error { close(p.deadline); return nil }
func (p *pipe) Close() error                    { return nil }

var _ driver.PacketConn = (*pipe)(nil)

func solicitation(hopLimit uint8) []byte {
	return ipv6pkt.Packet{
		Src: netip.MustParseAddr("fe80::5054:ff:fe00:1"), Dst: netip.MustParseAddr("ff02::2"),
		Next: ipv6pkt.ICMPv6, HopLimit: hopLimit, Payload: []byte{133, 0, 0, 0, 0, 0, 0, 0},
	}.Marshal()
}

func TestTheAdvertisement(t *testing.T) {
	p, err := ipv6pkt.Parse(Packet())
	if err != nil {
		t.Fatal(err)
	}
	m := p.Payload
	if p.Src != driver.GatewayIPv6 || p.Dst != netip.MustParseAddr("ff02::1") || p.HopLimit != 255 || m[0] != 134 || m[1] != 0 {
		t.Errorf("advertisement from %s to %s, hop limit %d, type %d", p.Src, p.Dst, p.HopLimit, m[0])
	}
	if m[5]&0x80 == 0 || binary.BigEndian.Uint16(m[6:8]) != 1800 {
		t.Errorf("flags %#x, router lifetime %d: want managed and 1800 s", m[5], binary.BigEndian.Uint16(m[6:8]))
	}
	if len(m) != 16 {
		t.Errorf("the advertisement carries options (%d bytes): no prefix may be offered", len(m)-16)
	}
}

func TestSolicitationsAreAnsweredAtMostEveryMinInterval(t *testing.T) {
	if !IsSolicitation(solicitation(255)) || IsSolicitation(solicitation(64)) {
		t.Fatal("a solicitation is one with hop limit 255 only")
	}
	p := &pipe{in: make(chan []byte, 256), out: make(chan []byte, 256), deadline: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, p, slog.New(slog.DiscardHandler)) }()
	<-p.out // the first, sent at once
	for range 100 {
		p.in <- solicitation(255)
	}
	time.Sleep(MinInterval / 2)
	if n := len(p.out); n != 0 {
		t.Errorf("%d advertisements within %v of the last", n, MinInterval/2)
	}
	time.Sleep(MinInterval/2 + maxResponseDelay + 200*time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := len(p.out); n != 1 {
		t.Errorf("%d advertisements for 100 solicitations, want 1", n)
	}
}
