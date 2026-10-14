package hostnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"time"
	"unsafe"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/ipv6pkt"
)

// ethIPv6 is ETH_P_IPV6 in network byte order, as packet sockets take it.
const ethIPv6 = unix.ETH_P_IPV6<<8&0xff00 | unix.ETH_P_IPV6>>8

// packetFilter is the classic BPF program a packet socket of a kind takes,
// over the IPv6 header with no extension headers.
func packetFilter(kind driver.PacketKind) ([]bpf.Instruction, error) {
	drop, take := bpf.RetConstant{Val: 0}, bpf.RetConstant{Val: 0xffff}
	switch kind {
	case driver.RouterSolicitations:
		return []bpf.Instruction{
			bpf.LoadAbsolute{Off: 6, Size: 1},
			bpf.JumpIf{Cond: bpf.JumpEqual, Val: unix.IPPROTO_ICMPV6, SkipFalse: 3},
			bpf.LoadAbsolute{Off: 40, Size: 1},
			bpf.JumpIf{Cond: bpf.JumpEqual, Val: 133, SkipFalse: 1},
			take, drop,
		}, nil
	case driver.DHCPv6:
		return []bpf.Instruction{
			bpf.LoadAbsolute{Off: 6, Size: 1},
			bpf.JumpIf{Cond: bpf.JumpEqual, Val: unix.IPPROTO_UDP, SkipFalse: 3},
			bpf.LoadAbsolute{Off: 42, Size: 2},
			bpf.JumpIf{Cond: bpf.JumpEqual, Val: 547, SkipFalse: 1},
			take, drop,
		}, nil
	}
	return nil, fmt.Errorf("no packets of kind %d", kind)
}

func (d *Driver) ListenPackets(_ context.Context, networkUID string, kind driver.PacketKind) (driver.PacketConn, string, error) {
	if err := checkUID(networkUID); err != nil {
		return nil, "", err
	}
	if !d.cfg.IPv6Prefix.IsValid() {
		return nil, "", fmt.Errorf("network %s has no IPv6: the host has no prefix", networkUID)
	}
	prog, err := packetFilter(kind)
	if err != nil {
		return nil, "", err
	}
	raw, err := bpf.Assemble(prog)
	if err != nil {
		return nil, "", err
	}
	filter := make([]unix.SockFilter, len(raw))
	for i, r := range raw {
		filter[i] = unix.SockFilter{Code: r.Op, Jt: r.Jt, Jf: r.Jf, K: r.K}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	ns, ok, err := openNS(networkUID)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "", fmt.Errorf("network %s has no namespace on the host", networkUID)
	}
	defer func() { _ = ns.Close() }()
	instance, err := instanceOf(ns)
	if err != nil {
		return nil, "", err
	}
	if instance == "" {
		return nil, "", fmt.Errorf("network %s has no bridge", networkUID)
	}
	var c *packetConn
	err = inNS(ns, func() error {
		br, err := net.InterfaceByName(nsBridge)
		if err != nil {
			return err
		}
		// Unbound, the socket takes nothing: protocol 0 until it is bound
		// to the bridge, with its filter on.
		fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
		if err != nil {
			return err
		}
		fprog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
		err = unix.SetsockoptSockFprog(fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &fprog)
		if err == nil {
			// Says which packets' checksums a guest left to its NIC.
			err = unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_AUXDATA, 1)
		}
		if err == nil {
			err = unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: ethIPv6, Ifindex: br.Index})
		}
		if err != nil {
			_ = unix.Close(fd)
			return err
		}
		c = &packetConn{f: os.NewFile(uintptr(fd), "packet:"+nsBridge), ifindex: br.Index}
		return nil
	})
	if err != nil {
		return nil, "", fmt.Errorf("network %s: packet socket: %w", networkUID, err)
	}
	return c, instance, nil
}

// packetConn is a packet socket on a network's bridge, taking IPv6
// packets without their Ethernet header.
type packetConn struct {
	f       *os.File
	ifindex int
}

func (c *packetConn) ReadPacket(b []byte) (int, net.HardwareAddr, error) {
	rc, err := c.f.SyscallConn()
	if err != nil {
		return 0, nil, err
	}
	oob := make([]byte, unix.CmsgSpace(int(unsafe.Sizeof(unix.TpacketAuxdata{}))))
	for {
		var n, oobn int
		var from unix.Sockaddr
		var rerr error
		if err := rc.Read(func(fd uintptr) bool {
			n, oobn, _, from, rerr = unix.Recvmsg(int(fd), b, oob, 0)
			return rerr != unix.EAGAIN
		}); err != nil {
			if errors.Is(err, os.ErrClosed) {
				err = net.ErrClosed
			}
			return 0, nil, err
		}
		if rerr != nil {
			return 0, nil, rerr
		}
		ll, ok := from.(*unix.SockaddrLinklayer)
		// What the bridge sends, the services' own packets among them, is
		// seen too, and is not a machine's.
		if !ok || ll.Pkttype == unix.PACKET_OUTGOING || ll.Halen != 6 {
			continue
		}
		if csumNotReady(oob[:oobn]) {
			ipv6pkt.CompleteChecksum(b[:n])
		}
		return n, slices.Clone(net.HardwareAddr(ll.Addr[:6])), nil
	}
}

// csumNotReady says whether a packet's auxiliary data marks its checksum as
// left to be computed, as a guest offloading it to its virtio NIC leaves it.
func csumNotReady(oob []byte) bool {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return false
	}
	for _, m := range msgs {
		if m.Header.Level == unix.SOL_PACKET && m.Header.Type == unix.PACKET_AUXDATA && len(m.Data) >= int(unsafe.Sizeof(unix.TpacketAuxdata{})) {
			aux := (*unix.TpacketAuxdata)(unsafe.Pointer(&m.Data[0]))
			return aux.Status&unix.TP_STATUS_CSUMNOTREADY != 0
		}
	}
	return false
}

func (c *packetConn) WritePacket(b []byte, to net.HardwareAddr) error {
	if len(to) != 6 {
		return fmt.Errorf("MAC %s is not an Ethernet address", to)
	}
	rc, err := c.f.SyscallConn()
	if err != nil {
		return err
	}
	sa := &unix.SockaddrLinklayer{Protocol: ethIPv6, Ifindex: c.ifindex, Halen: 6}
	copy(sa.Addr[:], to)
	var werr error
	if err := rc.Write(func(fd uintptr) bool {
		werr = unix.Sendto(int(fd), b, 0, sa)
		return werr != unix.EAGAIN
	}); err != nil {
		return err
	}
	return werr
}

func (c *packetConn) SetReadDeadline(t time.Time) error { return c.f.SetReadDeadline(t) }
func (c *packetConn) Close() error                      { return c.f.Close() }
