package apiserver

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"sync"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// Address condition types and reasons, the address controller's alone.
const (
	// AddressBound says whether the address's target interface holds it.
	AddressBound = "Bound"
	// Reasons for AddressBound.
	ReasonUnbound           = "Unbound"
	ReasonNoPrefix          = "NoPrefix"
	ReasonTargetNotFound    = "TargetNotFound"
	ReasonInterfaceNotFound = "InterfaceNotFound"
	ReasonOtherPrefix       = "OtherPrefix"
	ReasonBinding           = "Binding"
	ReasonBound             = "Bound"
)

// AddressUnbindFinalizer keeps a deleted Address, still reserving its
// address, until no interface holds it, so the interface it was bound to
// draws an automatic address of its own; the address controller removes it
// then.
const AddressUnbindFinalizer = registry.Domain + "/address-unbind"

// errAddressMoving is an interface whose Address another machine still
// holds: it takes it once that machine has let go, which the machine's
// change brings (addressTargets), or the wait.
var errAddressMoving = fmt.Errorf("%w: the address is still held by the interface it moves from", controller.ErrWaiting)

func decodeAddress(o store.Object) (*networkv1alpha1.Address, bool) {
	var a networkv1alpha1.Address
	return &a, proto.Unmarshal(o.Data, &a) == nil
}

// addressTarget is the interface an Address names: the kind and name of the
// object a machine is made from, and the interface's name.
type addressTarget struct {
	kind, name, iface string
}

// targetOf is the interface an Address names; ok is false for none.
func targetOf(spec *networkv1alpha1.AddressSpec) (addressTarget, bool) {
	t := spec.GetTarget()
	iface := t.GetInterface()
	if iface == "" {
		iface = "eth0"
	}
	switch o := t.GetObject().(type) {
	case *networkv1alpha1.AddressTarget_VirtualMachine:
		return addressTarget{registry.VirtualMachine.Kind, o.VirtualMachine, iface}, true
	case *networkv1alpha1.AddressTarget_Container:
		return addressTarget{registry.Container.Kind, o.Container, iface}, true
	}
	return addressTarget{}, false
}

// ipv6Owner is a machine, as its addresses are chosen: by the object it is
// made from, which Addresses name, and its own name.
type ipv6Owner struct {
	project, kind, name, machine string
}

// ownerOfMachine is the object a machine is made from.
func ownerOfMachine(m *computev1alpha1.Machine) ipv6Owner {
	o := ipv6Owner{project: m.GetMetadata().GetProject(), machine: m.GetMetadata().GetName()}
	for _, ref := range m.GetMetadata().GetOwnerReferences() {
		if ref.GetController() {
			o.kind, o.name = ref.GetKind(), ref.GetName()
		}
	}
	return o
}

// addresses lists the Addresses of a project, or every project's for "".
func (s *Server) addresses(ctx context.Context, project string) ([]*networkv1alpha1.Address, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Address.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	out := make([]*networkv1alpha1.Address, 0, len(objs))
	for _, o := range objs {
		a, ok := decodeAddress(o)
		if !ok {
			return nil, fmt.Errorf("decode address %s/%s", o.Project, o.Name)
		}
		out = append(out, a)
	}
	return out, nil
}

// reservedIPv6 are the addresses every Address reserves, which no interface
// draws as its automatic one.
func (s *Server) reservedIPv6(ctx context.Context) (map[netip.Addr]bool, error) {
	all, err := s.addresses(ctx, "")
	if err != nil {
		return nil, err
	}
	out := map[netip.Addr]bool{}
	for _, a := range all {
		if p, err := netip.ParsePrefix(a.GetStatus().GetIpv6()); err == nil {
			out[p.Addr()] = true
		}
	}
	return out, nil
}

// boundIPv6 are the addresses of the Addresses that name an interface of
// owner, by interface name. An Address being deleted binds nothing.
func (s *Server) boundIPv6(ctx context.Context, owner ipv6Owner) (map[string]netip.Addr, error) {
	if owner.kind == "" {
		return nil, nil
	}
	all, err := s.addresses(ctx, owner.project)
	if err != nil {
		return nil, err
	}
	out := map[string]netip.Addr{}
	for _, a := range all {
		t, ok := targetOf(a.GetSpec())
		p, err := netip.ParsePrefix(a.GetStatus().GetIpv6())
		if !ok || err != nil || a.GetMetadata().GetDeleteTime() != nil || t.kind != owner.kind || t.name != owner.name {
			continue
		}
		out[t.iface] = p.Addr()
	}
	return out, nil
}

// holders are, by IPv6 address, the machines whose interfaces hold one.
func (s *Server) holders(ctx context.Context) (map[netip.Addr]resource.Key, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind})
	if err != nil {
		return nil, err
	}
	out := map[netip.Addr]resource.Key{}
	for _, o := range objs {
		m, ok := decodeMachine(o)
		if !ok {
			return nil, fmt.Errorf("decode machine %s/%s", o.Project, o.Name)
		}
		for _, nic := range m.GetSpec().GetInterfaces() {
			if p, err := netip.ParsePrefix(nic.GetIpv6()); err == nil {
				out[p.Addr()] = resource.Key{Project: o.Project, Name: o.Name}
			}
		}
	}
	return out, nil
}

// reconcileAddress reserves an Address's address, once, from a node's
// prefix, and reports where it is bound. Binding is the machine's own
// reconcile (assignIPv6), which reads the Address.
func (s *Server) reconcileAddress(ctx context.Context, k controller.Key) error {
	key := resource.Key{Project: k.Project, Name: k.Name}
	obj, err := s.get(ctx, registry.Address, key)
	if err != nil || obj == nil {
		return err
	}
	a := obj.(*networkv1alpha1.Address)
	if a.GetMetadata().GetDeleteTime() != nil {
		// A machine being given an address is written under ipam; waiting
		// for it means seeing it.
		s.ipam.Lock()
		defer s.ipam.Unlock()
		if p, err := netip.ParsePrefix(a.GetStatus().GetIpv6()); err == nil {
			holders, err := s.holders(ctx)
			if err != nil {
				return err
			}
			if _, held := holders[p.Addr()]; held {
				return nil
			}
		}
		return s.RemoveFinalizer(ctx, registry.Address, key, AddressUnbindFinalizer)
	}
	status := &networkv1alpha1.AddressStatus{}
	if a.GetStatus() != nil {
		status = proto.Clone(a.GetStatus()).(*networkv1alpha1.AddressStatus)
	}
	status.ObservedGeneration = a.GetMetadata().GetGeneration()
	if status.GetIpv6() == "" {
		// Drawn and written as one decision with every other address.
		s.ipam.Lock()
		defer s.ipam.Unlock()
		addr, err := s.reserveIPv6(ctx)
		if err != nil {
			return err
		}
		if addr.IsValid() {
			status.Ipv6 = netip.PrefixFrom(addr, 128).String()
		}
	}
	reason, msg, machine, err := s.bindingOf(ctx, a, status.GetIpv6())
	if err != nil {
		return err
	}
	cond := &metav1.Condition{
		Type: AddressBound, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reason, Message: msg,
		ObservedGeneration: a.GetMetadata().GetGeneration(),
	}
	if reason == ReasonBound {
		cond.Status = metav1.ConditionStatus_CONDITION_STATUS_TRUE
	}
	status.Machine = machine
	status.Conditions = conditions.Set(status.GetConditions(), cond, s.clock.Now())
	if proto.Equal(status, a.GetStatus()) {
		return nil
	}
	rv := a.GetMetadata().GetResourceVersion()
	_, err = s.backend(registry.Address).UpdateStatus(s.system(ctx), key, status, &rv)
	return err
}

// reserveIPv6 draws an address no machine holds and no Address reserves
// from the first node, by name, that reports a prefix; invalid if none does.
// An Address binds only on a node of its prefix (ReasonOtherPrefix). The
// caller holds s.ipam until the address is written.
func (s *Server) reserveIPv6(ctx context.Context) (netip.Addr, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Node.Kind})
	if err != nil {
		return netip.Addr{}, err
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Name < objs[j].Name })
	var prefix netip.Prefix
	for _, o := range objs {
		var n computev1alpha1.Node
		if proto.Unmarshal(o.Data, &n) != nil {
			continue
		}
		if p, err := computeapi.ParseIPv6Prefix(n.GetStatus().GetIpv6Prefix()); err == nil {
			prefix = p
			break
		}
	}
	if !prefix.IsValid() {
		return netip.Addr{}, nil
	}
	taken, err := s.takenIPv6(ctx)
	if err != nil {
		return netip.Addr{}, err
	}
	return freeIPv6(prefix, taken, rand.Read)
}

// bindingOf says where an Address of address addr is: the reason for its
// Bound condition, a message, and the machine holding it.
func (s *Server) bindingOf(ctx context.Context, a *networkv1alpha1.Address, addr string) (reason, msg, machine string, err error) {
	if addr == "" {
		return ReasonNoPrefix, "no node has an IPv6 prefix to reserve the address from", "", nil
	}
	p, err := netip.ParsePrefix(addr)
	if err != nil {
		return "", "", "", fmt.Errorf("address %s: %w", addr, err)
	}
	t, ok := targetOf(a.GetSpec())
	if !ok {
		return ReasonUnbound, "the address is reserved and bound to nothing", "", nil
	}
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind, Project: a.GetMetadata().GetProject()})
	if err != nil {
		return "", "", "", err
	}
	for _, o := range objs {
		m, ok := decodeMachine(o)
		if !ok {
			return "", "", "", fmt.Errorf("decode machine %s/%s", o.Project, o.Name)
		}
		owner := ownerOfMachine(m)
		if owner.kind != t.kind || owner.name != t.name || m.GetMetadata().GetDeleteTime() != nil {
			continue
		}
		i := slices.IndexFunc(m.GetSpec().GetInterfaces(), func(nic *computev1alpha1.MachineInterface) bool { return nic.GetName() == t.iface })
		if i < 0 {
			return ReasonInterfaceNotFound, fmt.Sprintf("%s %s has no interface %s", t.kind, t.name, t.iface), "", nil
		}
		if got, err := netip.ParsePrefix(m.GetSpec().GetInterfaces()[i].GetIpv6()); err == nil && got.Addr() == p.Addr() {
			return ReasonBound, fmt.Sprintf("%s %s's %s holds the address", t.kind, t.name, t.iface), o.Name, nil
		}
		prefix, err := s.nodePrefix(ctx, m.GetSpec().GetNode())
		if err != nil {
			return "", "", "", err
		}
		if m.GetSpec().GetNode() != "" && !prefix.Contains(p.Addr()) {
			return ReasonOtherPrefix, fmt.Sprintf("%s %s is on node %s, whose prefix the address is not of", t.kind, t.name, m.GetSpec().GetNode()), "", nil
		}
		return ReasonBinding, fmt.Sprintf("%s %s's %s is being given the address", t.kind, t.name, t.iface), "", nil
	}
	return ReasonTargetNotFound, fmt.Sprintf("%s %s does not exist, or has no machine yet", t.kind, t.name), "", nil
}

// addressesOfMachine maps a machine to the Addresses that name its owner or
// that it holds, whose status follows it.
func (s *Server) addressesOfMachine(ctx context.Context, o store.Object) []controller.Key {
	m, ok := decodeMachine(o)
	if !ok {
		return nil
	}
	owner := ownerOfMachine(m)
	all, err := s.addresses(ctx, o.Project)
	if err != nil {
		s.log.Error("listing addresses failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, a := range all {
		t, ok := targetOf(a.GetSpec())
		if (ok && t.kind == owner.kind && t.name == owner.name) || a.GetStatus().GetMachine() == o.Name {
			keys = append(keys, controller.Key{Project: o.Project, Name: a.GetMetadata().GetName()})
		}
	}
	return keys
}

// addressesOnNode maps a node to the Addresses its prefix may change: those
// waiting for a prefix to reserve from, and those whose target is on a node
// of another prefix. The rest follow their machines.
func (s *Server) addressesOnNode(ctx context.Context, _ store.Object) []controller.Key {
	all, err := s.addresses(ctx, "")
	if err != nil {
		s.log.Error("listing addresses failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, a := range all {
		reason := conditions.Find(a.GetStatus().GetConditions(), AddressBound).GetReason()
		if a.GetStatus().GetIpv6() == "" || reason == ReasonOtherPrefix {
			keys = append(keys, controller.Key{Project: a.GetMetadata().GetProject(), Name: a.GetMetadata().GetName()})
		}
	}
	return keys
}

// addressTargets maps to the objects of kind r in a project that Addresses
// name: one waits for the machine an address moves from to let go of it.
func (s *Server) addressTargets(ctx context.Context, project string, r registry.Resource) []controller.Key {
	all, err := s.addresses(ctx, project)
	if err != nil {
		s.log.Error("listing addresses failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, a := range all {
		if t, ok := targetOf(a.GetSpec()); ok && t.kind == r.Kind {
			keys = append(keys, controller.Key{Project: project, Name: t.name})
		}
	}
	return keys
}

// targetsOfAddress maps an Address to the objects of kind r whose machines
// it binds or was bound to: its target, and the owner of the machine its
// status names, which lets go of it.
func (s *Server) targetsOfAddress(r registry.Resource) func(context.Context, store.Object) []controller.Key {
	return func(ctx context.Context, o store.Object) []controller.Key {
		a, ok := decodeAddress(o)
		if !ok {
			return nil
		}
		var keys []controller.Key
		if t, ok := targetOf(a.GetSpec()); ok && t.kind == r.Kind {
			keys = append(keys, controller.Key{Project: o.Project, Name: t.name})
		}
		if m := a.GetStatus().GetMachine(); m != "" {
			obj, err := s.get(ctx, registry.Machine, resource.Key{Project: o.Project, Name: m})
			if err == nil && obj != nil {
				keys = append(keys, ownerOf(o.Project, obj.(*computev1alpha1.Machine), r)...)
			}
		}
		// Every object of the kind that holds an address of a machine: a
		// moved Address releases the interface it left.
		keys = append(keys, s.ownersHoldingReserved(ctx, o.Project, r)...)
		return keys
	}
}

// ownersHoldingReserved maps to the objects of kind r in a project whose
// machines hold an address an Address reserves.
func (s *Server) ownersHoldingReserved(ctx context.Context, project string, r registry.Resource) []controller.Key {
	reserved, err := s.reservedIPv6(ctx)
	if err != nil {
		s.log.Error("listing addresses failed", "error", err)
		return nil
	}
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind, Project: project})
	if err != nil {
		s.log.Error("listing machines failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, o := range objs {
		m, ok := decodeMachine(o)
		if !ok {
			continue
		}
		for _, nic := range m.GetSpec().GetInterfaces() {
			if p, err := netip.ParsePrefix(nic.GetIpv6()); err == nil && reserved[p.Addr()] {
				keys = append(keys, ownerOf(project, m, r)...)
				break
			}
		}
	}
	return keys
}

// validateAddress checks an Address's target, and that no other Address of
// its project names the same interface: it holds the addresses' write lock
// from the check to the write.
func validateAddress(ctx context.Context, s *Server, _ auth.Principal, _, obj proto.Message) error {
	a := obj.(*networkv1alpha1.Address)
	if a.GetSpec().GetTarget() == nil {
		return nil
	}
	t, ok := targetOf(a.GetSpec())
	if !ok {
		return invalid("spec.target needs a virtualMachine or a container")
	}
	if err := ValidName(t.name); err != nil {
		return invalid("spec.target: %v", err)
	}
	if err := ValidName(t.iface); err != nil {
		return invalid("spec.target.interface: %v", err)
	}
	all, err := s.addresses(ctx, a.GetMetadata().GetProject())
	if err != nil {
		return err
	}
	for _, other := range all {
		if other.GetMetadata().GetName() == a.GetMetadata().GetName() {
			continue
		}
		if ot, ok := targetOf(other.GetSpec()); ok && ot == t {
			return failedPrecondition("%s %s's interface %s is already bound to address %q", t.kind, t.name, t.iface, other.GetMetadata().GetName())
		}
	}
	return nil
}

func addressLock(s *Server) *sync.Mutex { return &s.addressBinding }
