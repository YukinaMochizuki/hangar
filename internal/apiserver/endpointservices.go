package apiserver

import (
	"context"
	"errors"
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
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/labels"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/resolver"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// EndpointService and NetworkEndpoint condition types and reasons
// (docs/decisions/endpoint-services.md). The endpoint service and network
// endpoint controllers own Ready; the node in status.node owns Released,
// which it reports once it no longer carries an object being deleted.
const (
	EndpointReady    = "Ready"
	EndpointReleased = "Released"
	// Reasons for an EndpointService's Ready, besides ReasonTargetNotFound
	// and ReasonInterfaceNotFound.
	ReasonServing        = "Serving"
	ReasonServicePending = "ServicePending"
	// Reasons for a NetworkEndpoint's Ready, besides ReasonServicePending,
	// ReasonNetworkNotReady and ReasonNoAddressLeft.
	ReasonConnected       = "Connected"
	ReasonServiceNotFound = "ServiceNotFound"
	ReasonNotAllowed      = "NotAllowed"
	ReasonSameNetwork     = "SameNetwork"
)

func decodeEndpointService(o store.Object) (*networkv1alpha1.EndpointService, bool) {
	var es networkv1alpha1.EndpointService
	return &es, proto.Unmarshal(o.Data, &es) == nil
}

func decodeNetworkEndpoint(o store.Object) (*networkv1alpha1.NetworkEndpoint, bool) {
	var ne networkv1alpha1.NetworkEndpoint
	return &ne, proto.Unmarshal(o.Data, &ne) == nil
}

// endpointServices lists the EndpointServices of a project, or every
// project's for "".
func (s *Server) endpointServices(ctx context.Context, project string) ([]*networkv1alpha1.EndpointService, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.EndpointService.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	out := make([]*networkv1alpha1.EndpointService, 0, len(objs))
	for _, o := range objs {
		es, ok := decodeEndpointService(o)
		if !ok {
			return nil, fmt.Errorf("decode endpoint service %s/%s", o.Project, o.Name)
		}
		out = append(out, es)
	}
	return out, nil
}

// networkEndpoints lists the NetworkEndpoints of a project.
func (s *Server) networkEndpoints(ctx context.Context, project string) ([]*networkv1alpha1.NetworkEndpoint, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.NetworkEndpoint.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	out := make([]*networkv1alpha1.NetworkEndpoint, 0, len(objs))
	for _, o := range objs {
		ne, ok := decodeNetworkEndpoint(o)
		if !ok {
			return nil, fmt.Errorf("decode network endpoint %s/%s", o.Project, o.Name)
		}
		out = append(out, ne)
	}
	return out, nil
}

func endpointServiceLock(s *Server) *sync.Mutex { return &s.endpointServicePorts }

// endpointServiceTargetOf is the interface an EndpointService reaches, its
// target's first; ok is false for none.
func endpointServiceTargetOf(spec *networkv1alpha1.EndpointServiceSpec) (addressTarget, bool) {
	switch o := spec.GetTarget().GetObject().(type) {
	case *networkv1alpha1.EndpointServiceTarget_VirtualMachine:
		return addressTarget{registry.VirtualMachine.Kind, o.VirtualMachine, "eth0"}, true
	case *networkv1alpha1.EndpointServiceTarget_Container:
		return addressTarget{registry.Container.Kind, o.Container, "eth0"}, true
	}
	return addressTarget{}, false
}

// validateEndpointService checks an EndpointService's spec; the protocol's
// not changing is the field's immutability.
func validateEndpointService(_ context.Context, _ *Server, _ auth.Principal, _, obj proto.Message) error {
	spec := obj.(*networkv1alpha1.EndpointService).GetSpec()
	t, ok := endpointServiceTargetOf(spec)
	if !ok {
		return invalid("spec.target needs a virtualMachine or a container")
	}
	if err := ValidName(t.name); err != nil {
		return invalid("spec.target: %v", err)
	}
	switch spec.GetProtocol() {
	case networkv1alpha1.Protocol_PROTOCOL_TCP, networkv1alpha1.Protocol_PROTOCOL_UDP:
	default:
		return invalid("spec.protocol must be TCP or UDP")
	}
	if p := spec.GetPort(); p < 1 || p > 65535 {
		return invalid("spec.port %d is not a port from 1 to 65535", p)
	}
	if _, err := labels.Parse(spec.GetNetworkSelector()); err != nil {
		return invalid("spec.networkSelector: %v", err)
	}
	return nil
}

// drawEndpointService is a new EndpointService's first status: a transit
// port no other EndpointService of its protocol holds, those being deleted
// included, drawn at random. It is unique across the platform rather than
// within the service's network, which the target's machine may leave for
// another. Every apply and delete of the kind holds its write lock, so the
// draw and the write are one decision.
func drawEndpointService(ctx context.Context, s *Server, obj proto.Message) (proto.Message, error) {
	es := obj.(*networkv1alpha1.EndpointService)
	all, err := s.endpointServices(ctx, "")
	if err != nil {
		return nil, err
	}
	taken := map[int32]bool{}
	for _, other := range all {
		if other.GetSpec().GetProtocol() == es.GetSpec().GetProtocol() && other.GetStatus().GetTransitPort() != 0 {
			taken[other.GetStatus().GetTransitPort()] = true
		}
	}
	lo, hi := s.transitPorts()
	var free []int32
	for p := lo; p <= hi; p++ {
		if !taken[p] {
			free = append(free, p)
		}
	}
	if len(free) == 0 {
		return nil, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("every %s transit port from %d to %d is held by an endpoint service; none is left to draw", protocolName(es.GetSpec().GetProtocol()), lo, hi))
	}
	return &networkv1alpha1.EndpointServiceStatus{TransitPort: free[rand.IntN(len(free))]}, nil
}

// transitPorts is the range transit ports are drawn from: the platform's, or
// a test's smaller one.
func (s *Server) transitPorts() (lo, hi int32) {
	if s.testTransitPorts != [2]int32{} {
		return s.testTransitPorts[0], s.testTransitPorts[1]
	}
	return computeapi.TransitPortsFrom, computeapi.TransitPortsThrough
}

// releasedFromNode keeps, of a node's report on conditions, its Released
// condition alone, over the rest as stored.
func releasedFromNode(stored, reported []*metav1.Condition) []*metav1.Condition {
	out := slices.DeleteFunc(slices.Clone(stored), func(c *metav1.Condition) bool { return c.GetType() == EndpointReleased })
	if c := conditions.Find(reported, EndpointReleased); c != nil {
		out = append(out, c)
	}
	return out
}

func endpointServiceFromNode(stored, reported proto.Message) proto.Message {
	out := &networkv1alpha1.EndpointServiceStatus{}
	if st, ok := stored.(*networkv1alpha1.EndpointServiceStatus); ok && st != nil {
		out = proto.Clone(st).(*networkv1alpha1.EndpointServiceStatus)
	}
	out.Conditions = releasedFromNode(out.GetConditions(), reported.(*networkv1alpha1.EndpointServiceStatus).GetConditions())
	return out
}

func networkEndpointFromNode(stored, reported proto.Message) proto.Message {
	out := &networkv1alpha1.NetworkEndpointStatus{}
	if st, ok := stored.(*networkv1alpha1.NetworkEndpointStatus); ok && st != nil {
		out = proto.Clone(st).(*networkv1alpha1.NetworkEndpointStatus)
	}
	out.Conditions = releasedFromNode(out.GetConditions(), reported.(*networkv1alpha1.NetworkEndpointStatus).GetConditions())
	return out
}

// endpointServiceNode and networkEndpointNode are the node responsible for
// the object, the only one that reads it or reports on it.
func endpointServiceNode(obj proto.Message) string {
	return obj.(*networkv1alpha1.EndpointService).GetStatus().GetNode()
}

func networkEndpointNode(obj proto.Message) string {
	return obj.(*networkv1alpha1.NetworkEndpoint).GetStatus().GetNode()
}

// networkEndpointOwner makes a NetworkEndpoint its network's: it goes with
// the network.
func networkEndpointOwner(_ context.Context, _ *Server, obj proto.Message) (registry.Resource, resource.Key, bool) {
	ne := obj.(*networkv1alpha1.NetworkEndpoint)
	return registry.Network, resource.Key{Project: ne.GetMetadata().GetProject(), Name: ne.GetSpec().GetNetwork()}, true
}

func validateNetworkEndpoint(_ context.Context, _ *Server, _ auth.Principal, _, obj proto.Message) error {
	spec := obj.(*networkv1alpha1.NetworkEndpoint).GetSpec()
	if err := ValidName(spec.GetNetwork()); err != nil {
		return invalid("spec.network: %v", err)
	}
	if err := ValidName(spec.GetService()); err != nil {
		return invalid("spec.service: %v", err)
	}
	return nil
}

// releasedOrNowhere says whether a node that carried an object being
// deleted has let it go, or none ever carried it.
func releasedOrNowhere(node string, conds []*metav1.Condition) bool {
	return node == "" || conditions.IsTrue(conds, EndpointReleased)
}

// reconcileEndpointService reports where an EndpointService leads, and lets
// a deleted one go once the node that carried it no longer does.
func (s *Server) reconcileEndpointService(ctx context.Context, k controller.Key) error {
	key := resource.Key{Project: k.Project, Name: k.Name}
	obj, err := s.get(ctx, registry.EndpointService, key)
	if err != nil || obj == nil {
		return err
	}
	es := obj.(*networkv1alpha1.EndpointService)
	if es.GetMetadata().GetDeleteTime() != nil {
		if releasedOrNowhere(es.GetStatus().GetNode(), es.GetStatus().GetConditions()) {
			return s.RemoveFinalizer(ctx, registry.EndpointService, key, NodeCleanupFinalizer)
		}
		return nil
	}
	status := &networkv1alpha1.EndpointServiceStatus{}
	if es.GetStatus() != nil {
		status = proto.Clone(es.GetStatus()).(*networkv1alpha1.EndpointServiceStatus)
	}
	status.ObservedGeneration = es.GetMetadata().GetGeneration()
	reason, msg, err := s.serviceOf(ctx, es, status)
	if err != nil {
		return err
	}
	cond := &metav1.Condition{
		Type: EndpointReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reason, Message: msg,
		ObservedGeneration: es.GetMetadata().GetGeneration(),
	}
	if reason == ReasonServing {
		cond.Status = metav1.ConditionStatus_CONDITION_STATUS_TRUE
	}
	status.Conditions = conditions.Set(status.GetConditions(), cond, s.clock.Now())
	if proto.Equal(status, es.GetStatus()) {
		return nil
	}
	rv := es.GetMetadata().GetResourceVersion()
	_, err = s.backend(registry.EndpointService).UpdateStatus(s.system(ctx), key, status, &rv)
	return err
}

// serviceOf finds where an EndpointService leads, sets status's machine,
// network and node to it, and says why it is ready or not. The node stays
// when the machine goes, so a deleted service waits for the node that last
// carried it.
func (s *Server) serviceOf(ctx context.Context, es *networkv1alpha1.EndpointService, status *networkv1alpha1.EndpointServiceStatus) (reason, msg string, err error) {
	status.Machine, status.Network = "", ""
	t, ok := endpointServiceTargetOf(es.GetSpec())
	if !ok {
		return ReasonTargetNotFound, "the endpoint service names no target", nil
	}
	m, err := s.machineOfTarget(ctx, es.GetMetadata().GetProject(), t)
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
	nic := m.GetSpec().GetInterfaces()[i]
	status.Network = nic.GetNetwork()
	node := m.GetSpec().GetNode()
	if node == "" {
		return ReasonServicePending, fmt.Sprintf("%s %s is not on a node yet", t.kind, t.name), nil
	}
	status.Node = node
	if !interfaceIPv4(nic).Is4() {
		return ReasonServicePending, fmt.Sprintf("%s %s's %s has no IPv4 address yet", t.kind, t.name, t.iface), nil
	}
	return ReasonServing, fmt.Sprintf("%s port %d of %s %s, on network %s", protocolName(es.GetSpec().GetProtocol()), es.GetSpec().GetPort(), t.kind, t.name, nic.GetNetwork()), nil
}

// endpointServicesOfMachine maps a machine to the EndpointServices that name
// its owner or lead to it.
func (s *Server) endpointServicesOfMachine(ctx context.Context, o store.Object) []controller.Key {
	m, ok := decodeMachine(o)
	if !ok {
		return nil
	}
	owner := ownerOfMachine(m)
	all, err := s.endpointServices(ctx, o.Project)
	if err != nil {
		s.log.Error("listing endpoint services failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, es := range all {
		t, ok := endpointServiceTargetOf(es.GetSpec())
		if (ok && t.kind == owner.kind && t.name == owner.name) || es.GetStatus().GetMachine() == o.Name {
			keys = append(keys, controller.Key{Project: o.Project, Name: es.GetMetadata().GetName()})
		}
	}
	return keys
}

// reconcileNetworkEndpoint gives a NetworkEndpoint its address, reports
// whether its network may reach its service, and lets a deleted one go once
// the node that carried it no longer holds its address.
func (s *Server) reconcileNetworkEndpoint(ctx context.Context, k controller.Key) error {
	key := resource.Key{Project: k.Project, Name: k.Name}
	obj, err := s.get(ctx, registry.NetworkEndpoint, key)
	if err != nil || obj == nil {
		return err
	}
	ne := obj.(*networkv1alpha1.NetworkEndpoint)
	if ne.GetMetadata().GetDeleteTime() != nil {
		if releasedOrNowhere(ne.GetStatus().GetNode(), ne.GetStatus().GetConditions()) {
			return s.RemoveFinalizer(ctx, registry.NetworkEndpoint, key, NodeCleanupFinalizer)
		}
		return nil
	}
	if ne.GetStatus().GetIpv4() == "" {
		// The address is decided with every machine's of the network: it is
		// drawn and written under ipam, as theirs are.
		s.ipam.Lock()
		defer s.ipam.Unlock()
		if obj, err = s.get(ctx, registry.NetworkEndpoint, key); err != nil || obj == nil {
			return err
		}
		ne = obj.(*networkv1alpha1.NetworkEndpoint)
	}
	status := &networkv1alpha1.NetworkEndpointStatus{}
	if ne.GetStatus() != nil {
		status = proto.Clone(ne.GetStatus()).(*networkv1alpha1.NetworkEndpointStatus)
	}
	status.ObservedGeneration = ne.GetMetadata().GetGeneration()
	reason, msg, err := s.endpointOf(ctx, ne, status)
	if err != nil {
		return err
	}
	cond := &metav1.Condition{
		Type: EndpointReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reason, Message: msg,
		ObservedGeneration: ne.GetMetadata().GetGeneration(),
	}
	if reason == ReasonConnected {
		cond.Status = metav1.ConditionStatus_CONDITION_STATUS_TRUE
	}
	status.Conditions = conditions.Set(status.GetConditions(), cond, s.clock.Now())
	if proto.Equal(status, ne.GetStatus()) {
		return nil
	}
	rv := ne.GetMetadata().GetResourceVersion()
	_, err = s.backend(registry.NetworkEndpoint).UpdateStatus(s.system(ctx), key, status, &rv)
	return err
}

// endpointOf gives status the endpoint's address if it has none, which the
// caller holds ipam for, and its port, node and hostname from its service,
// and says why it is ready or not.
func (s *Server) endpointOf(ctx context.Context, ne *networkv1alpha1.NetworkEndpoint, status *networkv1alpha1.NetworkEndpointStatus) (reason, msg string, err error) {
	project, network := ne.GetMetadata().GetProject(), ne.GetSpec().GetNetwork()
	if status.GetIpv4() == "" {
		p, err := s.freeAddress(ctx, project, network)
		var b *blocked
		if errors.As(err, &b) {
			return b.reason, b.message, nil
		}
		if err != nil {
			return "", "", err
		}
		status.Ipv4 = netip.MustParsePrefix(p).Addr().String()
	}
	hostname, err := s.networkEndpointHostname(ctx, ne)
	if err != nil {
		return "", "", err
	}
	status.Hostname = hostname
	svcObj, err := s.get(ctx, registry.EndpointService, resource.Key{Project: project, Name: ne.GetSpec().GetService()})
	if err != nil {
		return "", "", err
	}
	if svcObj == nil || svcObj.(*networkv1alpha1.EndpointService).GetMetadata().GetDeleteTime() != nil {
		return ReasonServiceNotFound, fmt.Sprintf("endpoint service %s does not exist", ne.GetSpec().GetService()), nil
	}
	svc := svcObj.(*networkv1alpha1.EndpointService)
	if node := svc.GetStatus().GetNode(); node != "" {
		status.Node = node
	}
	status.Port = svc.GetSpec().GetPort()
	netObj, err := s.get(ctx, registry.Network, resource.Key{Project: project, Name: network})
	if err != nil {
		return "", "", err
	}
	if netObj == nil {
		return ReasonNetworkNotReady, fmt.Sprintf("network %s does not exist", network), nil
	}
	if !selects(svc.GetSpec().GetNetworkSelector(), netObj.(*networkv1alpha1.Network).GetMetadata().GetLabels()) {
		return ReasonNotAllowed, fmt.Sprintf("network %s's labels do not match endpoint service %s's networkSelector %q",
			network, svc.GetMetadata().GetName(), svc.GetSpec().GetNetworkSelector()), nil
	}
	if svc.GetStatus().GetNetwork() == network {
		return ReasonSameNetwork, fmt.Sprintf("endpoint service %s is on network %s itself; its machines reach it there", svc.GetMetadata().GetName(), network), nil
	}
	if !conditions.IsTrue(svc.GetStatus().GetConditions(), EndpointReady) {
		return ReasonServicePending, fmt.Sprintf("endpoint service %s leads nowhere yet", svc.GetMetadata().GetName()), nil
	}
	return ReasonConnected, fmt.Sprintf("%s:%d on network %s leads to endpoint service %s",
		status.GetIpv4(), status.GetPort(), network, svc.GetMetadata().GetName()), nil
}

// selects says whether a service's network selector matches a network's
// labels. Admission refuses a selector that does not parse, so one read back
// as such matches nothing rather than everything.
func selects(selector string, l map[string]string) bool {
	sel, err := labels.Parse(selector)
	return err == nil && sel.Matches(l)
}

// networkEndpointHostname is the name a NetworkEndpoint's network resolves
// to its address: none while a machine on the network goes by the same
// name, or has it as an alias there, which keeps it.
func (s *Server) networkEndpointHostname(ctx context.Context, ne *networkv1alpha1.NetworkEndpoint) (string, error) {
	project, name := ne.GetMetadata().GetProject(), ne.GetMetadata().GetName()
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind, Project: project})
	if err != nil {
		return "", err
	}
	for _, o := range objs {
		m, ok := decodeMachine(o)
		if !ok {
			return "", fmt.Errorf("decode machine %s/%s", o.Project, o.Name)
		}
		served := ownerOfMachine(m).name
		if served == "" {
			served = m.GetMetadata().GetName()
		}
		if m.GetSpec().GetFileShareServer() != nil {
			continue
		}
		if slices.ContainsFunc(m.GetSpec().GetInterfaces(), func(nic *computev1alpha1.MachineInterface) bool {
			return nic.GetNetwork() == ne.GetSpec().GetNetwork() && (served == name || slices.Contains(nic.GetAliases(), name))
		}) {
			return "", nil
		}
	}
	return name + "." + resolver.Search(project), nil
}

// networkEndpointsOfService maps an EndpointService to the NetworkEndpoints
// naming it.
func (s *Server) networkEndpointsOfService(ctx context.Context, o store.Object) []controller.Key {
	return s.networkEndpointsWhere(ctx, o.Project, func(ne *networkv1alpha1.NetworkEndpoint) bool {
		return ne.GetSpec().GetService() == o.Name
	})
}

// networkEndpointsOfNetwork maps a Network, whose labels decide which
// services it may reach, to its NetworkEndpoints.
func (s *Server) networkEndpointsOfNetwork(ctx context.Context, o store.Object) []controller.Key {
	return s.networkEndpointsWhere(ctx, o.Project, func(ne *networkv1alpha1.NetworkEndpoint) bool {
		return ne.GetSpec().GetNetwork() == o.Name
	})
}

// networkEndpointsOfMachine maps a machine, whose name may take an
// endpoint's, to the NetworkEndpoints of its networks.
func (s *Server) networkEndpointsOfMachine(ctx context.Context, o store.Object) []controller.Key {
	m, ok := decodeMachine(o)
	if !ok {
		return nil
	}
	return s.networkEndpointsWhere(ctx, o.Project, func(ne *networkv1alpha1.NetworkEndpoint) bool {
		return slices.ContainsFunc(m.GetSpec().GetInterfaces(), func(nic *computev1alpha1.MachineInterface) bool {
			return nic.GetNetwork() == ne.GetSpec().GetNetwork()
		})
	})
}

func (s *Server) networkEndpointsWhere(ctx context.Context, project string, match func(*networkv1alpha1.NetworkEndpoint) bool) []controller.Key {
	all, err := s.networkEndpoints(ctx, project)
	if err != nil {
		s.log.Error("listing network endpoints failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, ne := range all {
		if match(ne) {
			keys = append(keys, controller.Key{Project: project, Name: ne.GetMetadata().GetName()})
		}
	}
	return keys
}

// endpointAddresses are the addresses NetworkEndpoints hold on a network,
// those being deleted included, which keep theirs until their node lets go.
func (s *Server) endpointAddresses(ctx context.Context, project, network string) ([]netip.Addr, error) {
	all, err := s.networkEndpoints(ctx, project)
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, ne := range all {
		if ne.GetSpec().GetNetwork() != network {
			continue
		}
		if a, err := netip.ParseAddr(ne.GetStatus().GetIpv4()); err == nil {
			out = append(out, a)
		}
	}
	return out, nil
}
