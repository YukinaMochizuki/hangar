package fake

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"syscall"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// A network's sockets are real loopback ones standing for their addresses
// in its namespace (see ServiceAddr), each kept to the instance it opened in.

// socket is one socket handed out for a network.
type socket struct {
	network  string
	proto    string // udp or tcp
	addr     netip.AddrPort
	instance string
	udp      *net.UDPConn
	tcp      *net.TCPListener
}

// open says whether the socket has not been closed.
func (s *socket) open() bool {
	var sc syscall.Conn = s.tcp
	if s.udp != nil {
		sc = s.udp
	}
	rc, err := sc.SyscallConn()
	return err == nil && rc.Control(func(uintptr) {}) == nil
}

func (s *socket) String() string { return s.proto + " " + s.addr.String() }

func (d network) ListenUDP(_ context.Context, networkUID string, addr netip.AddrPort) (*net.UDPConn, string, error) {
	s, err := d.listen(networkUID, "udp", addr, func() (*socket, error) {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		return &socket{udp: c}, err
	})
	if err != nil {
		return nil, "", err
	}
	return s.udp, s.instance, nil
}

func (d network) ListenTCP(_ context.Context, networkUID string, addr netip.AddrPort) (*net.TCPListener, string, error) {
	s, err := d.listen(networkUID, "tcp", addr, func() (*socket, error) {
		l, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
		return &socket{tcp: l}, err
	})
	if err != nil {
		return nil, "", err
	}
	return s.tcp, s.instance, nil
}

func (d network) listen(networkUID, proto string, addr netip.AddrPort, open func() (*socket, error)) (*socket, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.netErr != nil {
		return nil, d.h.netErr
	}
	n, ok := d.h.networks[networkUID]
	if !ok {
		return nil, fmt.Errorf("network %s has no namespace on the host", networkUID)
	}
	if a := addr.Addr(); a != n.CIDR.Addr().Next() && a != driver.MetadataAddr && a != netip.IPv4Unspecified() {
		return nil, fmt.Errorf("network %s has no address %s to listen on", networkUID, a)
	}
	instance := d.h.instances[networkUID]
	d.h.sockets = slices.DeleteFunc(d.h.sockets, func(s *socket) bool { return !s.open() })
	if slices.ContainsFunc(d.h.sockets, func(s *socket) bool {
		return s.network == networkUID && s.instance == instance && s.proto == proto && s.addr == addr
	}) {
		return nil, fmt.Errorf("network %s: %s %s: address already in use", networkUID, proto, addr)
	}
	s, err := open()
	if err != nil {
		return nil, err
	}
	s.network, s.proto, s.addr, s.instance = networkUID, proto, addr, instance
	d.h.sockets = append(d.h.sockets, s)
	return s, nil
}

func (d network) Instance(_ context.Context, networkUID string) (string, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.netErr != nil {
		return "", d.h.netErr
	}
	return d.h.instances[networkUID], nil
}

// newInstance gives a network made anew its instance. Callers hold h.mu.
func (h *Host) newInstance(networkUID string) {
	h.instanceCount++
	h.instances[networkUID] = strconv.Itoa(h.instanceCount)
}

// RemakeNamespace makes a network's namespace again behind the agent's back,
// as `ip netns del` and the next ensure do, and says so on Changes: the
// network's sockets serve nothing from then on.
func (h *Host) RemakeNamespace(networkUID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.networks[networkUID]; ok {
		h.newInstance(networkUID)
	}
	signal(h.netChange)
}

// ServiceAddr returns where a test reaches what listens on proto ("udp" or
// "tcp") and addr in the network's current namespace.
func (h *Host) ServiceAddr(networkUID, proto string, addr netip.AddrPort) (net.Addr, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sockets {
		if s.network == networkUID && s.instance == h.instances[networkUID] && s.proto == proto && s.addr == addr && s.open() {
			if s.udp != nil {
				return s.udp.LocalAddr(), true
			}
			return s.tcp.Addr(), true
		}
	}
	return nil, false
}

// listening returns the sockets still open, by network, whatever their
// instance. Callers hold h.mu.
func (h *Host) listening() map[string][]string {
	out := map[string][]string{}
	for _, s := range h.sockets {
		if s.open() {
			out[s.network] = append(out[s.network], s.String())
		}
	}
	for _, l := range out {
		slices.Sort(l)
	}
	return out
}
