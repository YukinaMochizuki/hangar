// Package ipv6pkt reads and writes the IPv6 packets the node's IPv6 services
// exchange with machines over a driver.PacketConn: an IPv6 header with no
// extension headers, then UDP or ICMPv6, checksums included. It is not a
// general packet library: it refuses what those services never need.
package ipv6pkt

import (
	"encoding/binary"
	"errors"
	"net/netip"
)

// Next-header values the services use.
const (
	UDP    = 17
	ICMPv6 = 58
)

// HeaderLen is the length of an IPv6 header.
const HeaderLen = 40

// Packet is an IPv6 packet with no extension headers.
type Packet struct {
	Src, Dst netip.Addr
	// Next is the payload's protocol, UDP or ICMPv6.
	Next     uint8
	HopLimit uint8
	// Payload is the UDP datagram or ICMPv6 message, header included.
	Payload []byte
}

// ErrMalformed is returned for a packet that is not what Parse reads.
var ErrMalformed = errors.New("not an IPv6 packet with a valid UDP or ICMPv6 payload")

// Parse reads an IPv6 packet carrying UDP or ICMPv6 directly, with a valid
// checksum; anything else is ErrMalformed. The payload shares b.
func Parse(b []byte) (Packet, error) {
	if len(b) < HeaderLen || b[0]>>4 != 6 {
		return Packet{}, ErrMalformed
	}
	n := int(binary.BigEndian.Uint16(b[4:6]))
	if HeaderLen+n > len(b) {
		return Packet{}, ErrMalformed
	}
	p := Packet{
		Src: netip.AddrFrom16([16]byte(b[8:24])), Dst: netip.AddrFrom16([16]byte(b[24:40])),
		Next: b[6], HopLimit: b[7], Payload: b[HeaderLen : HeaderLen+n],
	}
	switch {
	case p.Next == UDP && len(p.Payload) >= 8 && int(binary.BigEndian.Uint16(p.Payload[4:6])) == len(p.Payload):
	case p.Next == ICMPv6 && len(p.Payload) >= 4:
	default:
		return Packet{}, ErrMalformed
	}
	if checksum(p.Src, p.Dst, p.Next, p.Payload) != 0 {
		return Packet{}, ErrMalformed
	}
	return p, nil
}

// Marshal writes the packet, filling in the payload's checksum (a UDP
// datagram's length too), which it changes in p.Payload.
func (p Packet) Marshal() []byte {
	b := make([]byte, HeaderLen+len(p.Payload))
	b[0] = 6 << 4
	binary.BigEndian.PutUint16(b[4:6], uint16(len(p.Payload)))
	b[6], b[7] = p.Next, p.HopLimit
	src, dst := p.Src.As16(), p.Dst.As16()
	copy(b[8:24], src[:])
	copy(b[24:40], dst[:])
	pl := b[HeaderLen:]
	copy(pl, p.Payload)
	at := 2 // ICMPv6
	if p.Next == UDP {
		binary.BigEndian.PutUint16(pl[4:6], uint16(len(pl)))
		at = 6
	}
	pl[at], pl[at+1] = 0, 0
	sum := checksum(p.Src, p.Dst, p.Next, pl)
	if p.Next == UDP && sum == 0 {
		sum = 0xffff // a UDP checksum of 0 means none, which IPv6 forbids
	}
	binary.BigEndian.PutUint16(pl[at:at+2], sum)
	return b
}

// CompleteChecksum fills in the checksum of a UDP or ICMPv6 payload of
// packet b that a machine left to be computed by its NIC, as a virtio
// guest offloading it does: such a packet reaches the host with only the
// pseudo header summed, which Parse would refuse. Anything else it leaves.
func CompleteChecksum(b []byte) {
	if len(b) < HeaderLen || b[0]>>4 != 6 {
		return
	}
	n := int(binary.BigEndian.Uint16(b[4:6]))
	if HeaderLen+n > len(b) {
		return
	}
	p := Packet{Src: netip.AddrFrom16([16]byte(b[8:24])), Dst: netip.AddrFrom16([16]byte(b[24:40])), Next: b[6]}
	pl := b[HeaderLen : HeaderLen+n]
	at := -1
	switch {
	case p.Next == UDP && len(pl) >= 8:
		at = 6
	case p.Next == ICMPv6 && len(pl) >= 4:
		at = 2
	}
	if at < 0 {
		return
	}
	pl[at], pl[at+1] = 0, 0
	sum := checksum(p.Src, p.Dst, p.Next, pl)
	if p.Next == UDP && sum == 0 {
		sum = 0xffff
	}
	binary.BigEndian.PutUint16(pl[at:at+2], sum)
}

// UDPDatagram is a UDP header and payload, its length and checksum left for
// Marshal.
func UDPDatagram(src, dst uint16, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(b[0:2], src)
	binary.BigEndian.PutUint16(b[2:4], dst)
	copy(b[8:], payload)
	return b
}

// checksum is the Internet checksum of the payload under the IPv6 pseudo
// header; 0 over a payload holding a valid checksum.
func checksum(src, dst netip.Addr, next uint8, payload []byte) uint16 {
	var sum uint32
	add := func(b []byte) {
		for len(b) >= 2 {
			sum += uint32(binary.BigEndian.Uint16(b))
			b = b[2:]
		}
		if len(b) == 1 {
			sum += uint32(b[0]) << 8
		}
	}
	s, d := src.As16(), dst.As16()
	add(s[:])
	add(d[:])
	var l [8]byte
	binary.BigEndian.PutUint32(l[0:4], uint32(len(payload)))
	l[7] = next
	add(l[:])
	add(payload)
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
