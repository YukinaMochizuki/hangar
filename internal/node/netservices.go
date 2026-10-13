package node

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/dhcp"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/metadata"
	"github.com/YukinaMochizuki/hangar/internal/node/netsvc"
	"github.com/YukinaMochizuki/hangar/internal/node/resolver"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// resolverPort is where a network's resolver listens, on its gateway.
const resolverPort = 53

// dhcpAddr is where a network's DHCP server listens: any address of its
// bridge, as requests come from clients without one, to the broadcast.
var dhcpAddr = netip.AddrPortFrom(netip.IPv4Unspecified(), 67)

// netServices are the services the node serves the networks on its host:
// each network's resolver, its DHCP and its metadata service.
type netServices struct {
	resolver *resolver.Resolver
	dhcp     *dhcp.Server
	metadata *metadata.Server
	services *netsvc.Manager

	mu sync.Mutex
	// named is, by machine uid, the networks whose records name the
	// machine, so a machine leaving a network has that network's records
	// made again too.
	named map[string]map[controller.Key]bool
}

func newNetServices(n driver.Network, upstreams []netip.AddrPort, log *slog.Logger) *netServices {
	r := resolver.New(resolver.Config{Upstreams: upstreams, Log: log})
	dns := netsvc.Service{
		Name: "resolver",
		Addr: func(n netsvc.Network) netip.AddrPort { return netip.AddrPortFrom(n.Gateway, resolverPort) },
		ServeUDP: func(ctx context.Context, n netsvc.Network, c *net.UDPConn) {
			if err := r.ServeUDP(ctx, n.UID, c); err != nil {
				log.Error("a network's resolver stopped", "network_uid", n.UID, "error", err)
			}
		},
		ServeTCP: func(ctx context.Context, n netsvc.Network, l *net.TCPListener) {
			if err := r.ServeTCP(ctx, n.UID, l); err != nil {
				log.Error("a network's resolver stopped", "network_uid", n.UID, "error", err)
			}
		},
	}
	d := dhcp.New(log)
	dhcpService := netsvc.Service{
		Name: "dhcp",
		Addr: func(netsvc.Network) netip.AddrPort { return dhcpAddr },
		ServeUDP: func(ctx context.Context, n netsvc.Network, c *net.UDPConn) {
			if err := d.Serve(ctx, n.UID, c); err != nil {
				log.Error("a network's DHCP stopped", "network_uid", n.UID, "error", err)
			}
		},
	}
	md := metadata.New(metadata.Config{Log: log})
	mdService := netsvc.Service{
		Name: "metadata",
		Addr: func(netsvc.Network) netip.AddrPort { return netip.AddrPortFrom(driver.MetadataAddr, 80) },
		ServeTCP: func(ctx context.Context, n netsvc.Network, l *net.TCPListener) {
			if err := md.Serve(ctx, n.UID, l); err != nil {
				log.Error("a network's metadata service stopped", "network_uid", n.UID, "error", err)
			}
		},
	}
	return &netServices{
		resolver: r, dhcp: d, metadata: md, services: netsvc.New(n, log, dns, dhcpService, mdService),
		named: map[string]map[controller.Key]bool{},
	}
}

// networkController keeps each network on the host served: its records made
// from every machine on it, whichever node runs them, and its services
// listening in its namespace as it is now.
func (a *Agent) networkController() *controller.Controller {
	return &controller.Controller{
		Name:       "network services",
		Source:     controller.APISource{Client: a.api},
		Reconciler: controller.ReconcilerFunc(a.reconcileNetworkServices),
		Watches: []controller.Watch{
			{Kind: registry.Network.Kind, Map: controller.Self},
			{Kind: registry.Machine.Kind, Map: a.networksOfMachine},
		},
		// The network driver's changes, its own included, are when a
		// namespace comes, goes or is made again.
		Trigger: a.netTrigger,
		Clock:   a.cfg.Clock,
		Log:     a.cfg.Log,
	}
}

// networksOfMachine maps a machine to the networks it has an interface on,
// and those whose records still name it.
func (a *Agent) networksOfMachine(_ context.Context, o store.Object) []controller.Key {
	var m computev1alpha1.Machine
	if proto.Unmarshal(o.Data, &m) != nil {
		return nil
	}
	keys := map[controller.Key]bool{}
	for _, nic := range m.GetSpec().GetInterfaces() {
		keys[controller.Key{Project: o.Project, Name: nic.GetNetwork()}] = true
	}
	s := a.netsvc
	s.mu.Lock()
	for k := range s.named[m.GetMetadata().GetUid()] {
		keys[k] = true
	}
	s.mu.Unlock()
	out := make([]controller.Key, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	return out
}

// reconcileNetworkServices has a network's services listen, if the network
// is on the host, then makes its records and leases from the API; one not on
// the host is neither served nor named. Listening first is what makes a
// guest asking before the records are read get SERVFAIL rather than
// nothing. A network gone from the API is the sweep's to stop serving, as
// it is the sweep's to remove.
func (a *Agent) reconcileNetworkServices(ctx context.Context, k controller.Key) error {
	n, err := a.getNetwork(ctx, k.Project, k.Name)
	if err != nil {
		var s *stuck
		if errors.As(err, &s) { // not found
			a.netsvc.name(k, nil)
			return nil
		}
		return err
	}
	uid := n.GetMetadata().GetUid()
	// Admission refuses a network without a range; one read without a uid
	// is not stored yet. Neither has anything to serve.
	cidr, ok := parsePrefix(n.GetSpec().GetIpv4Cidr())
	if uid == "" || !ok {
		return nil
	}
	s := a.netsvc
	if err := s.services.Ensure(ctx, netsvc.Network{UID: uid, Gateway: cidr.Addr().Next()}); err != nil {
		return err
	}
	if !slices.Contains(s.services.Serving(), uid) {
		s.forget(uid)
		s.name(k, nil)
		return nil
	}
	objs, _, err := a.api.ListAll(ctx, registry.Machine, client.ListOptions{Project: k.Project})
	if err != nil {
		return err
	}
	names := map[string][]netip.Addr{}
	leases := dhcp.Leases{Gateway: cidr.Addr().Next(), Search: []string{resolver.Search(k.Project)}, ByMAC: map[string]dhcp.Lease{}}
	instances := map[netip.Addr]metadata.Instance{}
	var on []string
	for _, o := range objs {
		m := o.(*computev1alpha1.Machine)
		for _, nic := range m.GetSpec().GetInterfaces() {
			addr, err := netip.ParsePrefix(nic.GetIpv4())
			if nic.GetNetwork() != k.Name || err != nil {
				continue
			}
			name := machineDNSName(m)
			if !slices.Contains(names[name], addr.Addr()) {
				names[name] = append(names[name], addr.Addr())
			}
			leases.ByMAC[mac(m.GetMetadata().GetUid(), nic.GetName())] = dhcp.Lease{Addr: addr, Hostname: name}
			instances[addr.Addr()] = metadata.Instance{UID: m.GetMetadata().GetUid(), Hostname: name}
			on = append(on, m.GetMetadata().GetUid())
		}
	}
	s.name(k, on)
	s.resolver.Set(uid, resolver.Records{Project: k.Project, Names: names})
	s.dhcp.Set(uid, leases)
	s.metadata.Set(uid, instances)
	return nil
}

// name records that the network's records name exactly the machines on.
func (s *netServices) name(network controller.Key, on []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for uid, nets := range s.named {
		if delete(nets, network); len(nets) == 0 {
			delete(s.named, uid)
		}
	}
	for _, uid := range on {
		if s.named[uid] == nil {
			s.named[uid] = map[controller.Key]bool{}
		}
		s.named[uid][network] = true
	}
}

// keep stops serving every network not on the host, and forgets its
// records and leases.
func (s *netServices) keep(onHost map[string]bool) {
	s.services.Keep(func(uid string) bool {
		if !onHost[uid] {
			s.forget(uid)
		}
		return onHost[uid]
	})
}

func (s *netServices) forget(networkUID string) {
	s.resolver.Forget(networkUID)
	s.dhcp.Forget(networkUID)
	s.metadata.Forget(networkUID)
}

// machineDNSName is the name a machine answers to in its project: that of the
// object it was made from, such as its Container, or its own.
func machineDNSName(m *computev1alpha1.Machine) string {
	for _, ref := range m.GetMetadata().GetOwnerReferences() {
		if ref.GetController() {
			return ref.GetName()
		}
	}
	return m.GetMetadata().GetName()
}

// guestDNS is what a machine's guest resolves with: the resolver of its first
// interface's network, and its project's search domain. A resolver knows
// only its own network, and a guest does not ask the next one after a
// NXDOMAIN, so listing the others would answer nothing more.
func guestDNS(project string, ifaces []driver.Interface) (nameservers, search []string) {
	if len(ifaces) > 0 && ifaces[0].Gateway.IsValid() {
		nameservers = []string{ifaces[0].Gateway.String()}
	}
	return nameservers, []string{resolver.Search(project)}
}

func parsePrefix(s string) (netip.Prefix, bool) {
	p, err := netip.ParsePrefix(s)
	return p, err == nil
}

func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
