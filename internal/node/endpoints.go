package node

import (
	"context"
	"fmt"
	"net/netip"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// EndpointReady and EndpointReleased are the EndpointService and
// NetworkEndpoint conditions the agent reads and reports: the apiserver's
// Ready says a network endpoint may reach its service, and the node reports
// Released once it no longer carries an object being deleted
// (docs/decisions/endpoint-services.md).
const (
	EndpointReady    = "Ready"
	EndpointReleased = "Released"
)

// endpointsKey is the endpoint controller's one key: the host's network
// endpoints are ensured as one set.
var endpointsKey = controller.Key{Name: "endpoints"}

// endpointController keeps the host's network endpoints those of the
// NetworkEndpoints the apiserver gives this node, on changes to them, to
// their services, machines and networks, and when the network driver
// notices the host changed.
func (a *Agent) endpointController() *controller.Controller {
	all := func(context.Context, store.Object) []controller.Key { return []controller.Key{endpointsKey} }
	return &controller.Controller{
		Name:       "network endpoints",
		Source:     controller.APISource{Client: a.api},
		Reconciler: controller.ReconcilerFunc(a.reconcileEndpoints),
		Watches: []controller.Watch{
			{Kind: registry.NetworkEndpoint.Kind, Map: all},
			{Kind: registry.EndpointService.Kind, Map: all},
			// A service's machine moving, going or given its address; a
			// network getting its transit link.
			{Kind: registry.Machine.Kind, Map: all, SkipResync: true},
			{Kind: registry.Network.Kind, Map: all, SkipResync: true},
		},
		Trigger: a.endpointTrigger,
		Clock:   a.cfg.Clock,
		Log:     a.cfg.Log,
	}
}

// reconcileEndpoints hands the network driver the endpoint of every Ready
// NetworkEndpoint this node carries whose service leads to a machine on it,
// then reports on each NetworkEndpoint and EndpointService being deleted
// that it no longer carries it.
func (a *Agent) reconcileEndpoints(ctx context.Context, _ controller.Key) error {
	svcObjs, _, err := a.api.ListAll(ctx, registry.EndpointService, client.ListOptions{})
	if err != nil {
		return err
	}
	services := map[[2]string]*networkv1alpha1.EndpointService{}
	var releasedServices []*networkv1alpha1.EndpointService
	for _, obj := range svcObjs {
		es := obj.(*networkv1alpha1.EndpointService)
		if es.GetStatus().GetNode() != a.cfg.Name {
			continue
		}
		if es.GetMetadata().GetDeleteTime() != nil {
			releasedServices = append(releasedServices, es)
			continue
		}
		services[[2]string{es.GetMetadata().GetProject(), es.GetMetadata().GetName()}] = es
	}
	neObjs, _, err := a.api.ListAll(ctx, registry.NetworkEndpoint, client.ListOptions{})
	if err != nil {
		return err
	}
	var want []driver.Endpoint
	var released []*networkv1alpha1.NetworkEndpoint
	for _, obj := range neObjs {
		ne := obj.(*networkv1alpha1.NetworkEndpoint)
		if ne.GetStatus().GetNode() != a.cfg.Name {
			continue
		}
		if ne.GetMetadata().GetDeleteTime() != nil {
			released = append(released, ne)
			continue
		}
		es := services[[2]string{ne.GetMetadata().GetProject(), ne.GetSpec().GetService()}]
		if es == nil || !conditions.IsTrue(ne.GetStatus().GetConditions(), EndpointReady) {
			continue
		}
		e, ok, err := a.endpointOf(ctx, ne, es)
		if err != nil {
			return err
		}
		if ok {
			want = append(want, e)
		}
	}
	// Two endpoints the apiserver has not caught up with may contradict each
	// other, as a service's transit port drawn twice cannot: the set is
	// ensured without the later of each clash.
	want = consistentEndpoints(want)
	a.host.Lock()
	err = a.cfg.Drivers.Network.EnsureEndpoints(ctx, want)
	a.host.Unlock()
	if err != nil {
		return fmt.Errorf("carrying network endpoints: %w", err)
	}
	for _, ne := range released {
		if err := a.reportEndpointReleased(ctx, ne); err != nil {
			return err
		}
	}
	for _, es := range releasedServices {
		if err := a.reportServiceReleased(ctx, es); err != nil {
			return err
		}
	}
	return nil
}

// consistentEndpoints is es without each endpoint CheckEndpoints would
// refuse beside those before it.
func consistentEndpoints(es []driver.Endpoint) []driver.Endpoint {
	driver.SortEndpoints(es)
	var out []driver.Endpoint
	for _, e := range es {
		if driver.CheckEndpoints(append(slices.Clone(out), e)) == nil {
			out = append(out, e)
		}
	}
	return out
}

// endpointOf is the endpoint a NetworkEndpoint makes on this host: none
// while it has no address or port, or its service leads to no interface
// with an address of a machine here.
func (a *Agent) endpointOf(ctx context.Context, ne *networkv1alpha1.NetworkEndpoint, es *networkv1alpha1.EndpointService) (driver.Endpoint, bool, error) {
	project := ne.GetMetadata().GetProject()
	// An address that does not parse is the invalid one, which is no IPv4.
	addr, _ := netip.ParseAddr(ne.GetStatus().GetIpv4())
	port := ne.GetStatus().GetPort()
	transit := es.GetStatus().GetTransitPort()
	if !addr.Is4() || port <= 0 || port > 65535 || transit < driver.TransitPortsFrom || transit > driver.TransitPortsThrough {
		return driver.Endpoint{}, false, nil
	}
	var protocol driver.ForwardProtocol
	switch es.GetSpec().GetProtocol() {
	case networkv1alpha1.Protocol_PROTOCOL_TCP:
		protocol = driver.TCP
	case networkv1alpha1.Protocol_PROTOCOL_UDP:
		protocol = driver.UDP
	default:
		return driver.Endpoint{}, false, nil
	}
	m, err := a.getMachine(ctx, controller.Key{Project: project, Name: es.GetStatus().GetMachine()})
	if err != nil || m == nil || m.GetSpec().GetNode() != a.cfg.Name || m.GetMetadata().GetDeleteTime() != nil {
		return driver.Endpoint{}, false, err
	}
	// A service reaches its machine's first interface alone.
	i := slices.IndexFunc(m.GetSpec().GetInterfaces(), func(nic *computev1alpha1.MachineInterface) bool { return nic.GetName() == "eth0" })
	if i < 0 {
		return driver.Endpoint{}, false, nil
	}
	nic := m.GetSpec().GetInterfaces()[i]
	to, ok := parsePrefix(nic.GetIpv4())
	if !ok {
		return driver.Endpoint{}, false, nil
	}
	in, err := a.networkUID(ctx, project, ne.GetSpec().GetNetwork())
	if err != nil {
		return driver.Endpoint{}, false, err
	}
	svc, err := a.networkUID(ctx, project, nic.GetNetwork())
	if err != nil || in == "" || svc == "" || in == svc {
		return driver.Endpoint{}, false, err
	}
	return driver.Endpoint{
		Protocol: protocol, NetworkUID: in, Addr: addr, Port: uint16(port),
		ServiceNetworkUID: svc, TransitPort: uint16(transit), To: netip.AddrPortFrom(to.Addr(), uint16(es.GetSpec().GetPort())),
	}, es.GetSpec().GetPort() > 0 && es.GetSpec().GetPort() <= 65535, nil
}

// networkUID is the uid of a project's network; "" for none.
func (a *Agent) networkUID(ctx context.Context, project, name string) (string, error) {
	n, err := a.networks.GetNetwork(ctx, connect.NewRequest(&networkv1alpha1.GetNetworkRequest{Project: project, Name: name}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return n.Msg.GetNetwork().GetMetadata().GetUid(), nil
}

// released is conds with Released set, saying what the node let go of.
func (a *Agent) released(conds []*metav1.Condition, msg string) []*metav1.Condition {
	return conditions.Set(conds, &metav1.Condition{
		Type: EndpointReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "NoLongerCarried", Message: msg,
	}, a.cfg.Clock.Now())
}

// reportEndpointReleased writes on a NetworkEndpoint being deleted, whose
// address and traffic the host no longer carries, that it has let go of
// them.
func (a *Agent) reportEndpointReleased(ctx context.Context, ne *networkv1alpha1.NetworkEndpoint) error {
	if conditions.IsTrue(ne.GetStatus().GetConditions(), EndpointReleased) {
		return nil
	}
	status := &networkv1alpha1.NetworkEndpointStatus{}
	if ne.GetStatus() != nil {
		status = proto.Clone(ne.GetStatus()).(*networkv1alpha1.NetworkEndpointStatus)
	}
	status.Conditions = a.released(status.GetConditions(), fmt.Sprintf("node %s no longer holds %s", a.cfg.Name, ne.GetStatus().GetIpv4()))
	rv := ne.GetMetadata().GetResourceVersion()
	_, err := a.networkEndpoints.UpdateNetworkEndpointStatus(ctx, connect.NewRequest(&networkv1alpha1.UpdateNetworkEndpointStatusRequest{
		Project: ne.GetMetadata().GetProject(), Name: ne.GetMetadata().GetName(), Status: status, ExpectedResourceVersion: &rv,
	}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil
	}
	return err
}

// reportServiceReleased writes on an EndpointService being deleted, whose
// traffic the host no longer carries, that it has let go of it.
func (a *Agent) reportServiceReleased(ctx context.Context, es *networkv1alpha1.EndpointService) error {
	if conditions.IsTrue(es.GetStatus().GetConditions(), EndpointReleased) {
		return nil
	}
	status := &networkv1alpha1.EndpointServiceStatus{}
	if es.GetStatus() != nil {
		status = proto.Clone(es.GetStatus()).(*networkv1alpha1.EndpointServiceStatus)
	}
	status.Conditions = a.released(status.GetConditions(), fmt.Sprintf("node %s no longer carries transit port %d", a.cfg.Name, es.GetStatus().GetTransitPort()))
	rv := es.GetMetadata().GetResourceVersion()
	_, err := a.endpointServices.UpdateEndpointServiceStatus(ctx, connect.NewRequest(&networkv1alpha1.UpdateEndpointServiceStatusRequest{
		Project: es.GetMetadata().GetProject(), Name: es.GetMetadata().GetName(), Status: status, ExpectedResourceVersion: &rv,
	}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil
	}
	return err
}
