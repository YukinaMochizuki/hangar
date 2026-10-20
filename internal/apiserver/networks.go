package apiserver

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

const (
	// NetworkInUseFinalizer keeps a network until no machine has an
	// interface on it; the network controller removes it then.
	NetworkInUseFinalizer = registry.Domain + "/network-in-use"
	// ReasonNetworkNotReady and ReasonNoAddressLeft are the reasons a
	// container's Ready condition gives when its machine can get no address.
	ReasonNetworkNotReady = "NetworkNotReady"
	ReasonNoAddressLeft   = "NoAddressLeft"
	// NetworkReady is the Network condition saying it has its transit link.
	NetworkReady = "Ready"
	// DefaultNetworkCIDR is the range of every project's default network.
	DefaultNetworkCIDR = "10.0.0.0/24"
)

// transitRange is where every network's link to the host comes from: the
// shared address space of RFC 6598, which no tenant range overlaps.
var transitRange = netip.MustParsePrefix("100.64.0.0/10")

func decodeNetwork(o store.Object) (*networkv1alpha1.Network, bool) {
	var n networkv1alpha1.Network
	return &n, proto.Unmarshal(o.Data, &n) == nil
}

// projectOfNetwork maps a network to its project, whose default it may be.
func projectOfNetwork(_ context.Context, o store.Object) []controller.Key {
	return []controller.Key{{Name: o.Project}}
}

// networksOfMachine maps a machine to the networks its interfaces are on: a
// machine going may let a network being deleted go too.
func networksOfMachine(_ context.Context, o store.Object) []controller.Key {
	m, ok := decodeMachine(o)
	if !ok {
		return nil
	}
	var keys []controller.Key
	for _, nic := range m.GetSpec().GetInterfaces() {
		keys = append(keys, controller.Key{Project: o.Project, Name: nic.GetNetwork()})
	}
	return keys
}

// reconcileProject gives every live project its default network. A deleted
// default network comes back.
func (s *Server) reconcileProject(ctx context.Context, k controller.Key) error {
	obj, err := s.get(ctx, registry.Project, resource.Key{Name: k.Name})
	if err != nil || obj == nil {
		return err
	}
	if obj.(*tenancyv1alpha1.Project).GetMetadata().GetDeleteTime() != nil {
		return nil
	}
	existing, err := s.get(ctx, registry.Network, resource.Key{Project: k.Name, Name: DefaultNetwork})
	if err != nil || existing != nil {
		return err
	}
	_, err = s.backend(registry.Network).Create(s.system(ctx), &networkv1alpha1.Network{
		Metadata: &metav1.ObjectMeta{Project: k.Name, Name: DefaultNetwork},
		Spec:     &networkv1alpha1.NetworkSpec{Ipv4Cidr: DefaultNetworkCIDR},
	})
	if connect.CodeOf(err) == connect.CodeAlreadyExists {
		// Written since it was read; that write is reconciled in turn.
		return nil
	}
	return err
}

// reconcileNetwork gives a network its transit link, and lets a deleted one
// go once no machine is on it.
func (s *Server) reconcileNetwork(ctx context.Context, k controller.Key) error {
	key := resource.Key{Project: k.Project, Name: k.Name}
	obj, err := s.get(ctx, registry.Network, key)
	if err != nil || obj == nil {
		return err
	}
	n := obj.(*networkv1alpha1.Network)
	if n.GetMetadata().GetDeleteTime() != nil {
		// A machine being given an address on the network is written under
		// ipam; waiting for it means seeing it.
		s.ipam.Lock()
		defer s.ipam.Unlock()
		machines, err := s.machinesOn(ctx, k.Project, k.Name)
		if err != nil || len(machines) > 0 {
			return err
		}
		return s.RemoveFinalizer(ctx, registry.Network, key, NetworkInUseFinalizer)
	}
	if n.GetStatus().GetTransitIpv4() != "" && conditions.IsTrue(n.GetStatus().GetConditions(), NetworkReady) &&
		n.GetStatus().GetObservedGeneration() == n.GetMetadata().GetGeneration() {
		return nil
	}
	return s.assignTransit(ctx, key, n)
}

// assignTransit records n's transit link, choosing one if it has none, as
// one decision with every other network's.
func (s *Server) assignTransit(ctx context.Context, key resource.Key, n *networkv1alpha1.Network) error {
	s.ipam.Lock()
	defer s.ipam.Unlock()
	transit := n.GetStatus().GetTransitIpv4()
	if transit == "" {
		objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Network.Kind})
		if err != nil {
			return err
		}
		used := map[netip.Prefix]bool{}
		for _, o := range objs {
			other, ok := decodeNetwork(o)
			if !ok {
				return fmt.Errorf("decode network %s/%s", o.Project, o.Name)
			}
			if p, err := netip.ParsePrefix(other.GetStatus().GetTransitIpv4()); err == nil {
				used[p] = true
			}
		}
		p, ok := firstFreeTransit(used)
		if !ok {
			return fmt.Errorf("no transit link left in %s", transitRange)
		}
		transit = p.String()
	}
	status := &networkv1alpha1.NetworkStatus{}
	if n.GetStatus() != nil {
		status = proto.Clone(n.GetStatus()).(*networkv1alpha1.NetworkStatus)
	}
	status.TransitIpv4 = transit
	status.ObservedGeneration = n.GetMetadata().GetGeneration()
	status.Conditions = conditions.Set(status.GetConditions(), &metav1.Condition{
		Type: NetworkReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "TransitAssigned",
		ObservedGeneration: n.GetMetadata().GetGeneration(),
	}, s.clock.Now())
	rv := n.GetMetadata().GetResourceVersion()
	_, err := s.backend(registry.Network).UpdateStatus(s.system(ctx), key, status, &rv)
	return err
}

// firstFreeTransit returns the lowest /31 in the transit range not in used.
func firstFreeTransit(used map[netip.Prefix]bool) (netip.Prefix, bool) {
	for a := transitRange.Addr(); transitRange.Contains(a); a = a.Next().Next() {
		p := netip.PrefixFrom(a, 31)
		if !used[p] {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

// machinesOn returns the machines, live or being deleted, with an interface
// on the network.
func (s *Server) machinesOn(ctx context.Context, project, network string) ([]string, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, o := range objs {
		m, ok := decodeMachine(o)
		if !ok {
			return nil, fmt.Errorf("decode machine %s/%s", o.Project, o.Name)
		}
		for _, nic := range m.GetSpec().GetInterfaces() {
			if nic.GetNetwork() == network {
				names = append(names, o.Name)
				break
			}
		}
	}
	return names, nil
}

// refuseDeletingInUse keeps users from deleting a network machines or file
// shares are on: one being deleted could not take new machines, and the
// deletion could not be taken back. A project being deleted takes its
// networks with it, and they wait for their machines under
// NetworkInUseFinalizer.
func refuseDeletingInUse(ctx context.Context, s *Server, obj proto.Message) error {
	n := obj.(*networkv1alpha1.Network)
	project, err := s.get(ctx, registry.Project, resource.Key{Name: n.GetMetadata().GetProject()})
	if err != nil {
		return err
	}
	if project == nil || project.(*tenancyv1alpha1.Project).GetMetadata().GetDeleteTime() != nil {
		return nil
	}
	machines, err := s.machinesOn(ctx, n.GetMetadata().GetProject(), n.GetMetadata().GetName())
	if err != nil {
		return err
	}
	if len(machines) > 0 {
		return failedPrecondition("network %q is in use by machines %s; delete their containers first",
			n.GetMetadata().GetName(), strings.Join(machines, ", "))
	}
	shares, err := s.sharesOn(ctx, n.GetMetadata().GetProject(), n.GetMetadata().GetName())
	if err != nil || len(shares) == 0 {
		return err
	}
	return failedPrecondition("network %q has file share %s on it; take the network out of their spec.networks first",
		n.GetMetadata().GetName(), strings.Join(shares, ", "))
}

// assignAddresses fills in the IPv4 address of every interface in want that
// has none, and, once want is on a node with an IPv6 prefix, its IPv6
// address, keeping the addresses the machine already has. The caller holds
// s.ipam until the machine is written, so two machines never get one
// address.
func (s *Server) assignAddresses(ctx context.Context, owner ipv6Owner, current, want *computev1alpha1.MachineSpec) error {
	project := owner.project
	have := map[string]*computev1alpha1.MachineInterface{}
	for _, nic := range current.GetInterfaces() {
		have[nic.GetName()] = nic
	}
	for _, nic := range want.GetInterfaces() {
		if old := have[nic.GetName()]; old != nil && old.GetNetwork() == nic.GetNetwork() && old.GetIpv4() != "" {
			nic.Ipv4 = old.GetIpv4()
			continue
		}
		addr, err := s.freeAddress(ctx, project, nic.GetNetwork())
		if err != nil {
			return err
		}
		nic.Ipv4 = addr
	}
	return s.assignIPv6(ctx, owner, current, want)
}

// assignIPv6 gives every interface of want a /128 of its node's prefix, or
// takes them away if the node has none; an unplaced machine has none. An
// interface an Address names takes the Address's, once no other machine
// holds it, which errAddressMoving says is still to come; any other keeps
// the one it has while it is of that prefix and no Address reserves it, and
// otherwise draws a new one. The caller holds s.ipam.
func (s *Server) assignIPv6(ctx context.Context, owner ipv6Owner, current, want *computev1alpha1.MachineSpec) error {
	prefix, err := s.nodePrefix(ctx, want.GetNode())
	if err != nil {
		return err
	}
	have := map[string]*computev1alpha1.MachineInterface{}
	for _, nic := range current.GetInterfaces() {
		have[nic.GetName()] = nic
	}
	var bound map[string]netip.Addr
	var reserved, taken map[netip.Addr]bool
	var holders map[netip.Addr]resource.Key
	if prefix.IsValid() {
		if bound, err = s.boundIPv6(ctx, owner); err != nil {
			return err
		}
		if reserved, err = s.reservedIPv6(ctx); err != nil {
			return err
		}
	}
	var moving bool
	for _, nic := range want.GetInterfaces() {
		old := have[nic.GetName()].GetIpv6()
		nic.Ipv6 = ""
		if !prefix.IsValid() {
			continue
		}
		if a, ok := bound[nic.GetName()]; ok && prefix.Contains(a) {
			if holders == nil {
				if holders, err = s.holders(ctx); err != nil {
					return err
				}
			}
			if h, held := holders[a]; held && h != (resource.Key{Project: owner.project, Name: owner.machine}) {
				// Until the machine it leaves lets go, the interface keeps
				// what it has.
				moving = true
				nic.Ipv6 = old
				continue
			}
			nic.Ipv6 = netip.PrefixFrom(a, 128).String()
			continue
		}
		if p, err := netip.ParsePrefix(old); err == nil && prefix.Contains(p.Addr()) && !reserved[p.Addr()] {
			nic.Ipv6 = old
			continue
		}
		if taken == nil {
			if taken, err = s.takenIPv6(ctx); err != nil {
				return err
			}
		}
		a, err := freeIPv6(prefix, taken, rand.Read)
		if err != nil {
			return err
		}
		taken[a] = true
		nic.Ipv6 = netip.PrefixFrom(a, 128).String()
	}
	if moving {
		return errAddressMoving
	}
	return nil
}

// nodePrefix is the IPv6 prefix a node reports, invalid if it reports none
// or there is no node.
func (s *Server) nodePrefix(ctx context.Context, node string) (netip.Prefix, error) {
	if node == "" {
		return netip.Prefix{}, nil
	}
	obj, err := s.get(ctx, registry.Node, resource.Key{Name: node})
	if err != nil || obj == nil {
		return netip.Prefix{}, err
	}
	// Agents report only valid prefixes; a node reporting another has none,
	// which is safe.
	p, _ := computeapi.ParseIPv6Prefix(obj.(*computev1alpha1.Node).GetStatus().GetIpv6Prefix())
	return p, nil
}

// takenIPv6 are the IPv6 addresses of every machine, live or being
// deleted, in any project, and those every Address reserves: a machine
// keeps its address until its node releases it.
func (s *Server) takenIPv6(ctx context.Context) (map[netip.Addr]bool, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind})
	if err != nil {
		return nil, err
	}
	taken, err := s.reservedIPv6(ctx)
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		m, ok := decodeMachine(o)
		if !ok {
			return nil, fmt.Errorf("decode machine %s/%s", o.Project, o.Name)
		}
		for _, nic := range m.GetSpec().GetInterfaces() {
			if p, err := netip.ParsePrefix(nic.GetIpv6()); err == nil {
				taken[p.Addr()] = true
			}
		}
	}
	return taken, nil
}

// freeIPv6 draws a random address of the /64 prefix that is not taken, so
// neighbours cannot be enumerated and a freed address is not handed straight
// to the next machine. Interface IDs below 2^32, the prefix's :: among them,
// are left to the host, which deploy/host puts on a low one, and the
// reserved anycast IDs of RFC 5453 are never drawn.
func freeIPv6(prefix netip.Prefix, taken map[netip.Addr]bool, read func([]byte) (int, error)) (netip.Addr, error) {
	for range 64 {
		var id [8]byte
		if _, err := read(id[:]); err != nil {
			return netip.Addr{}, err
		}
		if reservedInterfaceID(id) {
			continue
		}
		b := prefix.Addr().As16()
		copy(b[8:], id[:])
		if a := netip.AddrFrom16(b); !taken[a] {
			return a, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("no free IPv6 address found in %s", prefix)
}

// reservedInterfaceID says whether an interface ID is the host's (below
// 2^32) or a reserved subnet anycast one (fdff:ffff:ffff:ff80 and up).
func reservedInterfaceID(id [8]byte) bool {
	return binary.BigEndian.Uint32(id[:4]) == 0 || binary.BigEndian.Uint64(id[:]) >= 0xfdffffffffffff80
}

// freeAddress returns the lowest address of the network no other machine
// holds. The network address, the gateway (the first usable address) and the
// broadcast address are never handed out.
func (s *Server) freeAddress(ctx context.Context, project, network string) (string, error) {
	obj, err := s.get(ctx, registry.Network, resource.Key{Project: project, Name: network})
	if err != nil {
		return "", err
	}
	if obj == nil || obj.(*networkv1alpha1.Network).GetMetadata().GetDeleteTime() != nil {
		return "", &blocked{ReasonNetworkNotReady, fmt.Sprintf("network %s does not exist or is being deleted", network)}
	}
	cidr, err := netip.ParsePrefix(obj.(*networkv1alpha1.Network).GetSpec().GetIpv4Cidr())
	if err != nil {
		return "", err
	}
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind, Project: project})
	if err != nil {
		return "", err
	}
	taken := map[netip.Addr]bool{}
	for _, o := range objs {
		m, ok := decodeMachine(o)
		if !ok {
			return "", fmt.Errorf("decode machine %s/%s", o.Project, o.Name)
		}
		for _, nic := range m.GetSpec().GetInterfaces() {
			if nic.GetNetwork() != network {
				continue
			}
			if p, err := netip.ParsePrefix(nic.GetIpv4()); err == nil {
				taken[p.Addr()] = true
			}
		}
	}
	if a, ok := firstFreeHost(cidr, taken); ok {
		return netip.PrefixFrom(a, cidr.Bits()).String(), nil
	}
	return "", &blocked{ReasonNoAddressLeft, fmt.Sprintf("network %s (%s) has no address left", network, cidr)}
}

// firstFreeHost returns the lowest address in cidr after the gateway and
// before the broadcast address that is not taken.
func firstFreeHost(cidr netip.Prefix, taken map[netip.Addr]bool) (netip.Addr, bool) {
	gateway := cidr.Addr().Next()
	for a := gateway.Next(); cidr.Contains(a) && cidr.Contains(a.Next()); a = a.Next() {
		if !taken[a] {
			return a, true
		}
	}
	return netip.Addr{}, false
}
