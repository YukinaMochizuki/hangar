package fake

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// SetIPv6Prefix gives the host an IPv6 prefix, which interfaces' IPv6
// addresses must be of and without which a network has no packet sockets;
// an invalid one takes it away.
func (h *Host) SetIPv6Prefix(p netip.Prefix) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ipv6Prefix = p
}

// packetConn is a packet socket of a network's namespace: what a test sends
// it is read, and what is written is kept for the test to take.
type packetConn struct {
	network  string
	kind     driver.PacketKind
	instance string

	in   chan Packet
	mu   sync.Mutex
	sent []Packet
	// reads counts the reads begun, and sends the packets sent to be read;
	// guarded by mu.
	reads, sends int
	closed       chan struct{}
	once         sync.Once
	deadline     chan struct{}
	dlOnce       sync.Once
}

// Packet is an IPv6 packet a packet socket read or wrote, with the MAC it
// came from or went to.
type Packet struct {
	MAC  net.HardwareAddr
	Data []byte
}

func (c *packetConn) ReadPacket(b []byte) (int, net.HardwareAddr, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	select {
	case p := <-c.in:
		return copy(b, p.Data), p.MAC, nil
	case <-c.closed:
		return 0, nil, net.ErrClosed
	case <-c.deadline:
		return 0, nil, os.ErrDeadlineExceeded
	}
}

func (c *packetConn) WritePacket(b []byte, to net.HardwareAddr) error {
	select {
	case <-c.closed:
		return net.ErrClosed
	default:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, Packet{MAC: slices.Clone(to), Data: slices.Clone(b)})
	return nil
}

// SetReadDeadline takes only a deadline in the past, which ends reads, as
// the services set it.
func (c *packetConn) SetReadDeadline(t time.Time) error {
	if !t.IsZero() && !t.After(time.Now()) {
		c.dlOnce.Do(func() { close(c.deadline) })
	}
	return nil
}

func (c *packetConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *packetConn) open() bool {
	select {
	case <-c.closed:
		return false
	default:
		return true
	}
}

func (d network) ListenPackets(_ context.Context, networkUID string, kind driver.PacketKind) (driver.PacketConn, string, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.netErr != nil {
		return nil, "", d.h.netErr
	}
	if _, ok := d.h.networks[networkUID]; !ok {
		return nil, "", fmt.Errorf("network %s has no namespace on the host", networkUID)
	}
	if !d.h.ipv6Prefix.IsValid() {
		return nil, "", fmt.Errorf("network %s has no IPv6: the host has no prefix", networkUID)
	}
	instance := d.h.instances[networkUID]
	d.h.packetConns = slices.DeleteFunc(d.h.packetConns, func(c *packetConn) bool { return !c.open() })
	if slices.ContainsFunc(d.h.packetConns, func(c *packetConn) bool {
		return c.network == networkUID && c.instance == instance && c.kind == kind
	}) {
		return nil, "", fmt.Errorf("network %s: packets of kind %d: already in use", networkUID, kind)
	}
	c := &packetConn{
		network: networkUID, kind: kind, instance: instance,
		in: make(chan Packet, 64), closed: make(chan struct{}), deadline: make(chan struct{}),
	}
	d.h.packetConns = append(d.h.packetConns, c)
	return c, instance, nil
}

// packetConnOf is the open packet socket of a kind in a network's current
// namespace. Callers hold h.mu.
func (h *Host) packetConnOf(networkUID string, kind driver.PacketKind) *packetConn {
	for _, c := range h.packetConns {
		if c.network == networkUID && c.instance == h.instances[networkUID] && c.kind == kind && c.open() {
			return c
		}
	}
	return nil
}

// SendPacket has a machine with MAC from send packet b to what reads
// packets of the kind in the network; false if nothing does.
func (h *Host) SendPacket(networkUID string, kind driver.PacketKind, from net.HardwareAddr, b []byte) bool {
	h.mu.Lock()
	c := h.packetConnOf(networkUID, kind)
	h.mu.Unlock()
	if c == nil {
		return false
	}
	select {
	case c.in <- Packet{MAC: slices.Clone(from), Data: slices.Clone(b)}:
		// Only a packet delivered is counted, before the caller can ask
		// PacketsTaken.
		c.mu.Lock()
		c.sends++
		c.mu.Unlock()
		return true
	case <-c.closed:
		return false
	}
}

// PacketsTaken reports whether what reads packets of the kind in the
// network's current namespace has begun a read past every packet sent: for
// one reader that answers a packet before it reads the next, every packet
// sent has been answered or dropped.
func (h *Host) PacketsTaken(networkUID string, kind driver.PacketKind) bool {
	h.mu.Lock()
	c := h.packetConnOf(networkUID, kind)
	h.mu.Unlock()
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads > c.sends
}

// SentPackets returns, and forgets, what the packet socket of the kind in
// the network's current namespace has written; ok is false if it has none.
func (h *Host) SentPackets(networkUID string, kind driver.PacketKind) (sent []Packet, ok bool) {
	h.mu.Lock()
	c := h.packetConnOf(networkUID, kind)
	h.mu.Unlock()
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sent, c.sent = c.sent, nil
	return sent, true
}
