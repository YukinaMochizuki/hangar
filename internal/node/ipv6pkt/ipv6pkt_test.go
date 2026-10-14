package ipv6pkt

import (
	"bytes"
	"net/netip"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	src, dst := netip.MustParseAddr("fe80::1"), netip.MustParseAddr("fe80::5054:ff:fe12:3456")
	for _, p := range []Packet{
		{Src: src, Dst: dst, Next: UDP, HopLimit: 64, Payload: UDPDatagram(547, 546, []byte("advertise"))},
		{Src: src, Dst: netip.MustParseAddr("ff02::1"), Next: ICMPv6, HopLimit: 255, Payload: []byte{134, 0, 0, 0, 64, 0x80, 0, 0}},
		{Src: src, Dst: dst, Next: UDP, HopLimit: 1, Payload: UDPDatagram(1, 2, []byte("odd"))},
	} {
		b := p.Marshal()
		got, err := Parse(b)
		if err != nil {
			t.Fatalf("%+v: %v", p, err)
		}
		if got.Src != p.Src || got.Dst != p.Dst || got.Next != p.Next || got.HopLimit != p.HopLimit || len(got.Payload) != len(p.Payload) {
			t.Errorf("read back %+v, wrote %+v", got, p)
		}
		if !bytes.Equal(got.Payload[8:], p.Payload[8:]) {
			t.Errorf("payload %q, wrote %q", got.Payload, p.Payload)
		}
		for i := range b {
			bad := bytes.Clone(b)
			bad[i] ^= 0x10
			if q, err := Parse(bad); err == nil && i >= 8 && i != 7 {
				t.Errorf("byte %d flipped, still read as %+v", i, q)
			}
		}
	}
}

// TestCompleteChecksum: a packet whose checksum a guest left to its NIC,
// with only the pseudo header summed, reads once completed.
func TestCompleteChecksum(t *testing.T) {
	b := Packet{Src: netip.MustParseAddr("fe80::5054:ff:fe00:1"), Dst: netip.MustParseAddr("ff02::1:2"), Next: UDP, HopLimit: 1,
		Payload: UDPDatagram(546, 547, []byte("solicit"))}.Marshal()
	b[HeaderLen+6], b[HeaderLen+7] = 0x12, 0x34 // as offloaded: not the checksum
	if _, err := Parse(b); err == nil {
		t.Fatal("a packet with a partial checksum read")
	}
	CompleteChecksum(b)
	if _, err := Parse(b); err != nil {
		t.Errorf("completed, the packet does not read: %v", err)
	}
}

func TestParseRefuses(t *testing.T) {
	ok := Packet{Src: netip.MustParseAddr("fe80::1"), Dst: netip.MustParseAddr("ff02::1:2"), Next: UDP, HopLimit: 1,
		Payload: UDPDatagram(546, 547, []byte("solicit"))}.Marshal()
	for name, b := range map[string][]byte{
		"empty":            nil,
		"short":            ok[:39],
		"IPv4":             append([]byte{0x45}, ok[1:]...),
		"truncated":        ok[:len(ok)-1],
		"extension header": func() []byte { b := bytes.Clone(ok); b[6] = 0; return b }(),
	} {
		if _, err := Parse(b); err == nil {
			t.Errorf("%s read", name)
		}
	}
}
