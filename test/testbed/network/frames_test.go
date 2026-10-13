//go:build testbed

package network

import (
	"encoding/binary"
	"net"
	"net/netip"
	"testing"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// sendFrame sends a raw Ethernet frame out of a guest's eth0, as a guest
// can send whatever it likes.
func sendFrame(t *testing.T, g *guest, frame []byte) {
	t.Helper()
	ns, err := netns.GetFromName(g.ns)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ns.Close() }()
	if err := inNS(ns, func() error {
		ifi, err := net.InterfaceByName("eth0")
		if err != nil {
			return err
		}
		fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, 0)
		if err != nil {
			return err
		}
		defer func() { _ = unix.Close(fd) }()
		return unix.Sendto(fd, frame, 0, &unix.SockaddrLinklayer{Ifindex: ifi.Index, Halen: 6})
	}); err != nil {
		t.Fatal(err)
	}
}

func ethernet(dst, src net.HardwareAddr, typ uint16, payload []byte) []byte {
	b := append(append(append([]byte{}, dst...), src...), byte(typ>>8), byte(typ))
	return append(b, payload...)
}

// arpReply says sha has spa, to tpa.
func arpReply(sha net.HardwareAddr, spa, tpa netip.Addr) []byte {
	b := []byte{0, 1, 8, 0, 6, 4, 0, 2}
	b = append(b, sha...)
	b = append(b, spa.AsSlice()...)
	b = append(b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
	return append(b, tpa.AsSlice()...)
}

// markedPing is an IPv4 echo request from src to dst of the length the
// tests' counters count as marked.
func markedPing(src, dst netip.Addr) []byte {
	const total = 1139 // markLength
	b := make([]byte, total)
	b[0], b[8], b[9] = 0x45, 64, 1
	binary.BigEndian.PutUint16(b[2:], total)
	copy(b[12:], src.AsSlice())
	copy(b[16:], dst.AsSlice())
	binary.BigEndian.PutUint16(b[10:], checksum(b[:20]))
	b[20] = 8 // echo request
	binary.BigEndian.PutUint16(b[22:], checksum(b[20:]))
	return b
}

func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
