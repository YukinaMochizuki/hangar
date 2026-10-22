package apiserver

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"slices"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// PortForward condition types and reasons. The port forward controller
// owns PortForwardReady; the node in status.node owns PortForwardReleased,
// which it reports once it has closed the port of a PortForward being
// deleted.
const (
	// PortForwardReady says whether the port leads to its target.
	PortForwardReady = "Ready"
	// PortForwardReleased is PortForwardReady's counterpart on the node.
	PortForwardReleased = "Released"
	// Reasons for PortForwardReady, besides ReasonTargetNotFound and
	// ReasonInterfaceNotFound.
	ReasonNoSharedIPv4 = "NoSharedIPv4"
	ReasonPortPending  = "PortPending"
	ReasonForwarding   = "Forwarding"
)

// The public ports a PortForward is drawn from: below hostnet's
// SharedPortsFrom, from which the machines' own connections leave the
// shared address (docs/decisions/networking.md, "Port forwards").
const (
	PortForwardPortMin = 10000
	PortForwardPortMax = 32767
)

func decodePortForward(o store.Object) (*networkv1alpha1.PortForward, bool) {
	var pf networkv1alpha1.PortForward
	return &pf, proto.Unmarshal(o.Data, &pf) == nil
}

// forwardTargetOf is the interface a PortForward names; ok is false for
// none.
func forwardTargetOf(spec *networkv1alpha1.PortForwardSpec) (addressTarget, bool) {
	t := spec.GetTarget()
	iface := t.GetInterface()
	if iface == "" {
		iface = "eth0"
	}
	switch o := t.GetObject().(type) {
	case *networkv1alpha1.PortForwardTarget_VirtualMachine:
		return addressTarget{registry.VirtualMachine.Kind, o.VirtualMachine, iface}, true
	case *networkv1alpha1.PortForwardTarget_Container:
		return addressTarget{registry.Container.Kind, o.Container, iface}, true
	}
	return addressTarget{}, false
}

// portForwards lists the PortForwards of a project, or every project's for
// "".
func (s *Server) portForwards(ctx context.Context, project string) ([]*networkv1alpha1.PortForward, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.PortForward.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	out := make([]*networkv1alpha1.PortForward, 0, len(objs))
	for _, o := range objs {
		pf, ok := decodePortForward(o)
		if !ok {
			return nil, fmt.Errorf("decode port forward %s/%s", o.Project, o.Name)
		}
		out = append(out, pf)
	}
	return out, nil
}

func portForwardLock(s *Server) *sync.Mutex { return &s.portForwardPorts }

// validatePortForward checks a PortForward's spec; the protocol's not
// changing is the field's immutability.
func validatePortForward(_ context.Context, _ *Server, _ auth.Principal, _, obj proto.Message) error {
	spec := obj.(*networkv1alpha1.PortForward).GetSpec()
	t, ok := forwardTargetOf(spec)
	if !ok {
		return invalid("spec.target needs a virtualMachine or a container")
	}
	if err := ValidName(t.name); err != nil {
		return invalid("spec.target: %v", err)
	}
	if err := ValidName(t.iface); err != nil {
		return invalid("spec.target.interface: %v", err)
	}
	switch spec.GetProtocol() {
	case networkv1alpha1.Protocol_PROTOCOL_TCP, networkv1alpha1.Protocol_PROTOCOL_UDP:
	default:
		return invalid("spec.protocol must be TCP or UDP")
	}
	if p := spec.GetTargetPort(); p < 1 || p > 65535 {
		return invalid("spec.targetPort %d is not a port from 1 to 65535", p)
	}
	return nil
}

// drawPortForward is a new PortForward's first status: a public port no
// other PortForward of its protocol holds, those being deleted included,
// drawn at random. Every apply and delete of the kind holds its write lock,
// so the draw and the write are one decision.
func drawPortForward(ctx context.Context, s *Server, obj proto.Message) (proto.Message, error) {
	pf := obj.(*networkv1alpha1.PortForward)
	all, err := s.portForwards(ctx, "")
	if err != nil {
		return nil, err
	}
	taken := map[int32]bool{}
	for _, other := range all {
		if other.GetSpec().GetProtocol() == pf.GetSpec().GetProtocol() && other.GetStatus().GetPort() != 0 {
			taken[other.GetStatus().GetPort()] = true
		}
	}
	lo, hi := s.forwardPorts()
	var free []int32
	for p := lo; p <= hi; p++ {
		if !taken[p] {
			free = append(free, p)
		}
	}
	if len(free) == 0 {
		return nil, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("every public %s port from %d to %d is held by a port forward; none is left to draw", protocolName(pf.GetSpec().GetProtocol()), lo, hi))
	}
	return &networkv1alpha1.PortForwardStatus{Port: free[rand.IntN(len(free))]}, nil
}

// forwardPorts is the range ports are drawn from: the platform's, or a
// test's smaller one.
func (s *Server) forwardPorts() (lo, hi int32) {
	if s.testForwardPorts != [2]int32{} {
		return s.testForwardPorts[0], s.testForwardPorts[1]
	}
	return PortForwardPortMin, PortForwardPortMax
}

// protocolName is a protocol as a refusal names it.
func protocolName(p networkv1alpha1.Protocol) string {
	switch p {
	case networkv1alpha1.Protocol_PROTOCOL_TCP:
		return "TCP"
	case networkv1alpha1.Protocol_PROTOCOL_UDP:
		return "UDP"
	}
	return p.String()
}

// portForwardFromNode is a node's report on a PortForward: its Released
// condition alone, over the rest as stored.
func portForwardFromNode(stored, reported proto.Message) proto.Message {
	out := &networkv1alpha1.PortForwardStatus{}
	if st, ok := stored.(*networkv1alpha1.PortForwardStatus); ok && st != nil {
		out = proto.Clone(st).(*networkv1alpha1.PortForwardStatus)
	}
	out.Conditions = slices.DeleteFunc(out.Conditions, func(c *metav1.Condition) bool { return c.GetType() == PortForwardReleased })
	if c := conditions.Find(reported.(*networkv1alpha1.PortForwardStatus).GetConditions(), PortForwardReleased); c != nil {
		out.Conditions = append(out.Conditions, c)
	}
	return out
}

// portForwardNode is the node responsible for a PortForward, the only one
// that reads it or reports on it.
func portForwardNode(obj proto.Message) string {
	return obj.(*networkv1alpha1.PortForward).GetStatus().GetNode()
}

// reconcilePortForward reports where a PortForward's port leads, and lets a
// deleted one go once the node that opened its port has closed it.
//
// status.node follows the target's machine to its node and stays when the
// machine goes, so a deleted PortForward waits for the node that last held
// its port. A target moving to another node moves status.node at once: the
// node it leaves closes the port as it next reads its forwards, and until
// then the port, still this PortForward's, leads nowhere else, at another
// node's shared address.
func (s *Server) reconcilePortForward(ctx context.Context, k controller.Key) error {
	key := resource.Key{Project: k.Project, Name: k.Name}
	obj, err := s.get(ctx, registry.PortForward, key)
	if err != nil || obj == nil {
		return err
	}
	pf := obj.(*networkv1alpha1.PortForward)
	if pf.GetMetadata().GetDeleteTime() != nil {
		if pf.GetStatus().GetNode() == "" || conditions.IsTrue(pf.GetStatus().GetConditions(), PortForwardReleased) {
			return s.RemoveFinalizer(ctx, registry.PortForward, key, NodeCleanupFinalizer)
		}
		return nil
	}
	status := &networkv1alpha1.PortForwardStatus{}
	if pf.GetStatus() != nil {
		status = proto.Clone(pf.GetStatus()).(*networkv1alpha1.PortForwardStatus)
	}
	status.ObservedGeneration = pf.GetMetadata().GetGeneration()
	reason, msg, err := s.forwardOf(ctx, pf, status)
	if err != nil {
		return err
	}
	cond := &metav1.Condition{
		Type: PortForwardReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reason, Message: msg,
		ObservedGeneration: pf.GetMetadata().GetGeneration(),
	}
	if reason == ReasonForwarding {
		cond.Status = metav1.ConditionStatus_CONDITION_STATUS_TRUE
	}
	status.Conditions = conditions.Set(status.GetConditions(), cond, s.clock.Now())
	if proto.Equal(status, pf.GetStatus()) {
		return nil
	}
	rv := pf.GetMetadata().GetResourceVersion()
	_, err = s.backend(registry.PortForward).UpdateStatus(s.system(ctx), key, status, &rv)
	return err
}

// forwardOf finds where a PortForward's port leads, sets status's machine,
// node and ipv4 to it, and says why it is ready or not.
func (s *Server) forwardOf(ctx context.Context, pf *networkv1alpha1.PortForward, status *networkv1alpha1.PortForwardStatus) (reason, msg string, err error) {
	status.Machine, status.Ipv4 = "", ""
	t, ok := forwardTargetOf(pf.GetSpec())
	if !ok {
		return ReasonTargetNotFound, "the port forward names no target", nil
	}
	m, err := s.machineOfTarget(ctx, pf.GetMetadata().GetProject(), t)
	if err != nil {
		return "", "", err
	}
	if m == nil {
		return ReasonTargetNotFound, fmt.Sprintf("%s %s does not exist, or has no machine yet", t.kind, t.name), nil
	}
	status.Machine = m.GetMetadata().GetName()
	i := slices.IndexFunc(m.GetSpec().GetInterfaces(), func(nic *computev1alpha1.MachineInterface) bool { return nic.GetName() == t.iface })
	if i < 0 {
		return ReasonInterfaceNotFound, fmt.Sprintf("%s %s has no interface %s", t.kind, t.name, t.iface), nil
	}
	node := m.GetSpec().GetNode()
	if node == "" {
		return ReasonPortPending, fmt.Sprintf("%s %s is not on a node yet", t.kind, t.name), nil
	}
	status.Node = node
	shared, err := s.nodeSharedIPv4(ctx, node)
	if err != nil {
		return "", "", err
	}
	if !shared.IsValid() {
		return ReasonNoSharedIPv4, fmt.Sprintf("node %s has no shared IPv4 address to open the port at", node), nil
	}
	status.Ipv4 = shared.String()
	if !interfaceIPv4(m.GetSpec().GetInterfaces()[i]).Is4() {
		return ReasonPortPending, fmt.Sprintf("%s %s's %s has no IPv4 address yet", t.kind, t.name, t.iface), nil
	}
	return ReasonForwarding, fmt.Sprintf("%s:%d leads to %s %s's %s, port %d", shared, status.GetPort(), t.kind, t.name, t.iface, pf.GetSpec().GetTargetPort()), nil
}

// machineOfTarget is the live machine made from the object t names in a
// project, if there is one.
func (s *Server) machineOfTarget(ctx context.Context, project string, t addressTarget) (*computev1alpha1.Machine, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		m, ok := decodeMachine(o)
		if !ok {
			return nil, fmt.Errorf("decode machine %s/%s", o.Project, o.Name)
		}
		owner := ownerOfMachine(m)
		if owner.kind == t.kind && owner.name == t.name && m.GetMetadata().GetDeleteTime() == nil {
			return m, nil
		}
	}
	return nil, nil
}

// nodeSharedIPv4 is the shared IPv4 address a node reports; invalid for
// none, or for no such node.
func (s *Server) nodeSharedIPv4(ctx context.Context, node string) (netip.Addr, error) {
	obj, err := s.get(ctx, registry.Node, resource.Key{Name: node})
	if err != nil || obj == nil {
		return netip.Addr{}, err
	}
	// Agents report only IPv4; a node reporting another has none.
	if a, err := netip.ParseAddr(obj.(*computev1alpha1.Node).GetStatus().GetSharedIpv4()); err == nil && a.Is4() {
		return a, nil
	}
	return netip.Addr{}, nil
}

// interfaceIPv4 is an interface's IPv4 address; invalid while it has none.
func interfaceIPv4(nic *computev1alpha1.MachineInterface) netip.Addr {
	p, _ := netip.ParsePrefix(nic.GetIpv4())
	return p.Addr()
}

// portForwardsOfMachine maps a machine to the PortForwards that name its
// owner or lead to it.
func (s *Server) portForwardsOfMachine(ctx context.Context, o store.Object) []controller.Key {
	m, ok := decodeMachine(o)
	if !ok {
		return nil
	}
	owner := ownerOfMachine(m)
	all, err := s.portForwards(ctx, o.Project)
	if err != nil {
		s.log.Error("listing port forwards failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, pf := range all {
		t, ok := forwardTargetOf(pf.GetSpec())
		if (ok && t.kind == owner.kind && t.name == owner.name) || pf.GetStatus().GetMachine() == o.Name {
			keys = append(keys, controller.Key{Project: o.Project, Name: pf.GetMetadata().GetName()})
		}
	}
	return keys
}

// portForwardsOnNode maps a node, whose shared address may have changed, to
// the PortForwards whose port it opens.
func (s *Server) portForwardsOnNode(ctx context.Context, o store.Object) []controller.Key {
	all, err := s.portForwards(ctx, "")
	if err != nil {
		s.log.Error("listing port forwards failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, pf := range all {
		if pf.GetStatus().GetNode() == o.Name {
			keys = append(keys, controller.Key{Project: pf.GetMetadata().GetProject(), Name: pf.GetMetadata().GetName()})
		}
	}
	return keys
}
