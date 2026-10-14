package fake

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// guestKey is where a guest serves: its network and address.
type guestKey struct {
	network string
	addr    netip.AddrPort
}

// ServeGuest has serve answer each connection made to addr in the network,
// as a guest's server would; nil stops it. DialSSH reaches port 22 alone.
func (h *Host) ServeGuest(networkUID string, addr netip.AddrPort, serve func(net.Conn)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.guests == nil {
		h.guests = map[guestKey]func(net.Conn){}
	}
	k := guestKey{networkUID, addr}
	if serve == nil {
		delete(h.guests, k)
		return
	}
	h.guests[k] = serve
}

// Dials lists what DialSSH reached for, as "network address:22".
func (h *Host) Dials() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.dials...)
}

func (d network) DialSSH(_ context.Context, networkUID string, to netip.Addr) (net.Conn, error) {
	addr := netip.AddrPortFrom(to, 22)
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.netErr != nil {
		return nil, d.h.netErr
	}
	if _, ok := d.h.networks[networkUID]; !ok {
		return nil, fmt.Errorf("network %s has no namespace on the host", networkUID)
	}
	d.h.dials = append(d.h.dials, networkUID+" "+addr.String())
	serve, ok := d.h.guests[guestKey{networkUID, addr}]
	if !ok {
		return nil, fmt.Errorf("network %s: dial %s: %w", networkUID, addr, syscall.ECONNREFUSED)
	}
	client, server := net.Pipe()
	go serve(server)
	return client, nil
}
