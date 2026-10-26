package fake

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"syscall"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

func (d network) EnsureFirewall(_ context.Context, networkUID string, policies []driver.InterfacePolicy) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.netErr != nil {
		return d.h.netErr
	}
	if _, ok := d.h.networks[networkUID]; !ok {
		return fmt.Errorf("network %s has no namespace on the host", networkUID)
	}
	set := map[string]driver.InterfacePolicy{}
	size := 0
	for _, p := range policies {
		i, ok := d.h.interfaces[p.MachineUID][p.Name]
		if !ok || i.NetworkUID != networkUID {
			continue
		}
		if i.FileShareServer {
			// A server's interface takes nothing of a policy but its clients.
			p = driver.InterfacePolicy{MachineUID: p.MachineUID, Name: p.Name, ShareClients: slices.Clone(p.ShareClients)}
			size += len(p.ShareClients)
		} else {
			p.EdgePorts, p.Shares, p.ShareClients = slices.Clone(p.EdgePorts), slices.Clone(p.Shares), nil
			size += p.Policy.Size() + len(p.EdgePorts) + len(p.Shares)
		}
		set[p.MachineUID+"/"+p.Name] = p
	}
	if limit := cmp.Or(d.h.firewallLimit, driver.MaxFirewallRules); size > limit {
		return fmt.Errorf("network %s: security groups make %d rules, more than %d", networkUID, size, limit)
	}
	d.h.firewalls[networkUID] = set
	return nil
}

// EdgePorts returns the ports a network's interfaces let the edge reach,
// by machine uid and interface name, such as "uid/eth0".
func (h *Host) EdgePorts(networkUID string) map[string][]uint16 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string][]uint16{}
	for k, p := range h.firewalls[networkUID] {
		if len(p.EdgePorts) > 0 {
			out[k] = slices.Clone(p.EdgePorts)
		}
	}
	return out
}

// interfaceAt returns the interface of the network holding addr. Callers
// hold h.mu.
func (h *Host) interfaceAt(networkUID string, addr netip.Addr) (driver.Interface, bool) {
	for _, ifs := range h.interfaces {
		for _, i := range ifs {
			if i.NetworkUID == networkUID && i.Address.Addr() == addr {
				return i, true
			}
		}
	}
	return driver.Interface{}, false
}

// dialOpen reports whether the firewall lets a dial reach addr: port 22 of
// any interface, a port of an interface's edge ports, or with anyPort, a
// port forward's, any port; but nothing of a file share's server. Callers
// hold h.mu.
func (h *Host) dialOpen(networkUID string, addr netip.AddrPort, anyPort bool) bool {
	if i, ok := h.interfaceAt(networkUID, addr.Addr()); ok && i.FileShareServer {
		return false
	}
	if anyPort || addr.Port() == 22 {
		return true
	}
	for key, p := range h.firewalls[networkUID] {
		machine, name, _ := strings.Cut(key, "/")
		if i, ok := h.interfaces[machine][name]; ok && i.Address.Addr() == addr.Addr() && slices.Contains(p.EdgePorts, addr.Port()) {
			return true
		}
	}
	return false
}

// Firewall returns the policies a network's interfaces hold, by machine uid
// and interface name, such as "uid/eth0"; a file share's server's holds
// none.
func (h *Host) Firewall(networkUID string) map[string]secgroup.Policy {
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.firewalls[networkUID]
	if !ok {
		return nil
	}
	out := map[string]secgroup.Policy{}
	for k, p := range set {
		out[k] = p.Policy
	}
	return out
}

func (d network) ShareRules(_ context.Context, networkUID string) (map[string]driver.ShareRules, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.netErr != nil {
		return nil, d.h.netErr
	}
	if _, ok := d.h.networks[networkUID]; !ok {
		return nil, fmt.Errorf("network %s has no namespace on the host", networkUID)
	}
	out := map[string]driver.ShareRules{}
	for k, p := range d.h.firewalls[networkUID] {
		machine, name, _ := strings.Cut(k, "/")
		i, ok := d.h.interfaces[machine][name]
		switch {
		case !ok:
		case i.FileShareServer && len(p.ShareClients) > 0:
			out[k] = driver.ShareRules{Clients: slices.Clone(p.ShareClients)}
		case !i.FileShareServer && len(p.Shares) > 0:
			out[k] = driver.ShareRules{Servers: slices.Clone(p.Shares)}
		}
	}
	return out, nil
}

// GuestConnect has the guest on from open a TCP connection to an address
// of its network, as the firewall lets it: a file share's server takes
// only its clients, on driver.ShareServerPort, and opens none; any other
// sends what its groups let out, and the file share servers it names on
// their port, and takes what its groups let in. What nothing serves is
// refused, once let through.
func (h *Host) GuestConnect(from driver.Interface, to netip.AddrPort) (net.Conn, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	src, ok := h.interfaces[from.MachineUID][from.Name]
	if !ok {
		return nil, fmt.Errorf("machine %s has no interface %s", from.MachineUID, from.Name)
	}
	dst, ok := h.interfaceAt(src.NetworkUID, to.Addr())
	dropped := fmt.Errorf("%s to %s: the firewall drops it: %w", src.Address.Addr(), to, syscall.ETIMEDOUT)
	if !ok {
		return nil, dropped
	}
	pols := h.firewalls[src.NetworkUID]
	out, hasOut := pols[src.MachineUID+"/"+src.Name]
	in, hasIn := pols[dst.MachineUID+"/"+dst.Name]
	switch {
	case src.FileShareServer || !hasOut || !hasIn:
		return nil, dropped
	case dst.FileShareServer:
		if to.Port() != driver.ShareServerPort || !slices.Contains(in.ShareClients, src.Address.Addr()) {
			return nil, dropped
		}
	case !admits(in.Policy.Ingress, src.Address.Addr(), to.Port()):
		return nil, dropped
	}
	toShare := to.Port() == driver.ShareServerPort && slices.Contains(out.Shares, to.Addr())
	if !toShare && !out.Policy.EgressAll && !admits(out.Policy.Egress, to.Addr(), to.Port()) {
		return nil, dropped
	}
	serve, ok := h.guests[guestKey{src.NetworkUID, to}]
	if !ok {
		return nil, fmt.Errorf("%s to %s: %w", src.Address.Addr(), to, syscall.ECONNREFUSED)
	}
	client, server := net.Pipe()
	go serve(server)
	return client, nil
}

// admits says whether a rule lets TCP to port pass with peer, its source
// for ingress and its destination for egress.
func admits(rules []secgroup.Rule, peer netip.Addr, port uint16) bool {
	for _, r := range rules {
		if r.Protocol != secgroup.TCP && r.Protocol != secgroup.All {
			continue
		}
		if len(r.Ports) > 0 && !slices.ContainsFunc(r.Ports, func(p secgroup.PortRange) bool { return port >= p.From && port <= p.To }) {
			continue
		}
		if slices.ContainsFunc(r.Peers, func(p netip.Prefix) bool { return p.Contains(peer) }) {
			return true
		}
	}
	return false
}

// LimitFirewall bounds each firewall at n rules in place of
// driver.MaxFirewallRules, so a test can pass the bound with a few; 0
// clears it.
func (h *Host) LimitFirewall(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.firewallLimit = n
}
