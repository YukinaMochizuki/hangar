package apiserver

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
)

// service is an EndpointService of project web to container ctr's port.
func service(name, ctr string, protocol networkv1alpha1.Protocol, port int32, selector string) *networkv1alpha1.EndpointService {
	return &networkv1alpha1.EndpointService{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
		Spec: &networkv1alpha1.EndpointServiceSpec{
			Target:   &networkv1alpha1.EndpointServiceTarget{Object: &networkv1alpha1.EndpointServiceTarget_Container{Container: ctr}},
			Protocol: protocol, Port: port, NetworkSelector: selector,
		},
	}
}

// netEndpoint is a NetworkEndpoint of project web on network to service.
func netEndpoint(name, network, svc string) *networkv1alpha1.NetworkEndpoint {
	return &networkv1alpha1.NetworkEndpoint{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
		Spec:     &networkv1alpha1.NetworkEndpointSpec{Network: network, Service: svc},
	}
}

// ctrOn is container name with one interface, on network.
func ctrOn(name, network string) *computev1alpha1.Container {
	c := ctr(name)
	c.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: network}}
	return c
}

func applyService(t *testing.T, s *Server, ctx context.Context, es *networkv1alpha1.EndpointService) *networkv1alpha1.EndpointService {
	t.Helper()
	res, err := s.backend(registry.EndpointService).Apply(ctx, es)
	if err != nil {
		t.Fatal(err)
	}
	return res.Object.(*networkv1alpha1.EndpointService)
}

func applyEndpoint(t *testing.T, s *Server, ctx context.Context, ne *networkv1alpha1.NetworkEndpoint) {
	t.Helper()
	if _, err := s.backend(registry.NetworkEndpoint).Apply(ctx, ne); err != nil {
		t.Fatal(err)
	}
}

func applyNet(t *testing.T, s *Server, ctx context.Context, n *networkv1alpha1.Network) {
	t.Helper()
	if _, err := s.backend(registry.Network).Apply(ctx, n); err != nil {
		t.Fatal(err)
	}
}

func waitNetEndpoint(t *testing.T, s *Server, system context.Context, name, what string, ok func(*networkv1alpha1.NetworkEndpoint) bool) *networkv1alpha1.NetworkEndpoint {
	t.Helper()
	return waitObject(t, s, system, registry.NetworkEndpoint, resource.Key{Project: "web", Name: name}, "network endpoint "+name+" "+what, ok)
}

func waitService(t *testing.T, s *Server, system context.Context, name, what string, ok func(*networkv1alpha1.EndpointService) bool) *networkv1alpha1.EndpointService {
	t.Helper()
	return waitObject(t, s, system, registry.EndpointService, resource.Key{Project: "web", Name: name}, "endpoint service "+name+" "+what, ok)
}

func endpointReason(conds []*metav1.Condition) string {
	return conditions.Find(conds, EndpointReady).GetReason()
}

func endpointIs(reason string) func(*networkv1alpha1.NetworkEndpoint) bool {
	return func(ne *networkv1alpha1.NetworkEndpoint) bool {
		return endpointReason(ne.GetStatus().GetConditions()) == reason
	}
}

// drawTransitFrom has s draw transit ports from lo to hi alone.
func drawTransitFrom(s *Server, lo, hi int32) {
	s.endpointServicePorts.Lock()
	defer s.endpointServicePorts.Unlock()
	s.testTransitPorts = [2]int32{lo, hi}
}

func TestEndpointServiceAdmission(t *testing.T) {
	t.Parallel()
	s, _, user := running(t)
	b := s.backend(registry.EndpointService)
	if _, err := b.Apply(user, service("waits", "not-made-yet", tcp, 22, "")); err != nil {
		t.Errorf("a service of a container not made yet: %v", err)
	}
	noTarget := service("bad", "x", tcp, 22, "")
	noTarget.Spec.Target = nil
	for what, es := range map[string]*networkv1alpha1.EndpointService{
		"no target":                    noTarget,
		"a target naming no object":    service("bad", "", tcp, 22, ""),
		"a target in another project":  service("bad", "other/git", tcp, 22, ""),
		"no protocol":                  service("bad", "git", networkv1alpha1.Protocol_PROTOCOL_UNSPECIFIED, 22, ""),
		"a protocol that is none":      service("bad", "git", networkv1alpha1.Protocol(7), 22, ""),
		"no port":                      service("bad", "git", tcp, 0, ""),
		"a port past 65535":            service("bad", "git", udp, 65536, ""),
		"a selector with an empty key": service("bad", "git", tcp, 22, "tier=ws,=x"),
	} {
		_, err := b.Apply(user, es)
		wantCodeOf(t, err, connect.CodeInvalidArgument, what)
	}
	_, err := b.Apply(user, service("waits", "not-made-yet", udp, 22, ""))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "changing the protocol")

	nb := s.backend(registry.NetworkEndpoint)
	_, err = nb.Apply(user, netEndpoint("git", "not-made-yet", "git"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "an endpoint on a network not made yet")
	_, err = nb.Apply(user, netEndpoint("git", "default", "Not A Name"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "an endpoint naming no service")
	if _, err := nb.Apply(user, netEndpoint("git", "default", "not-made-yet")); err != nil {
		t.Errorf("an endpoint of a service not made yet: %v", err)
	}
	moved := netEndpoint("git", "default", "other")
	_, err = nb.Apply(user, moved)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "changing an endpoint's service")
}

// TestATransitPortIsDrawnAndKept: a new service's transit port is of the
// range, unique per protocol across projects, and kept whatever is applied
// after; none left refuses the create.
func TestATransitPortIsDrawnAndKept(t *testing.T) {
	t.Parallel()
	s, _, user := running(t)
	es := applyService(t, s, user, service("git", "a", tcp, 22, ""))
	port := es.GetStatus().GetTransitPort()
	if port < computeapi.TransitPortsFrom || port > computeapi.TransitPortsThrough {
		t.Fatalf("drawn transit port %d, want one from %d to %d", port, computeapi.TransitPortsFrom, computeapi.TransitPortsThrough)
	}
	moved := service("git", "b", tcp, 2222, "tier=ws")
	moved.Status = &networkv1alpha1.EndpointServiceStatus{TransitPort: 1}
	if got := applyService(t, s, user, moved).GetStatus().GetTransitPort(); got != port {
		t.Errorf("retargeting changed the transit port from %d to %d", port, got)
	}
	otherProject(t, s, user)
	drawTransitFrom(s, 2000, 2000)
	if p := applyService(t, s, user, service("t", "a", tcp, 22, "")).GetStatus().GetTransitPort(); p != 2000 {
		t.Errorf("TCP drew %d from the one transit port 2000", p)
	}
	if p := applyService(t, s, user, service("u", "a", udp, 53, "")).GetStatus().GetTransitPort(); p != 2000 {
		t.Errorf("UDP drew %d from the one transit port 2000, which only TCP holds", p)
	}
	other := service("t", "a", tcp, 22, "")
	other.Metadata.Project = "other"
	_, err := s.backend(registry.EndpointService).Apply(user, other)
	wantExhausted(t, err, "every TCP transit port from 2000 to 2000 is held", "a service of another project drawing a held transit port")
}

// TestTransitDrawingIsOneDecision: creators racing for fewer transit ports
// than there are of them each get another, and the rest are refused.
func TestTransitDrawingIsOneDecision(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	drawTransitFrom(s, 2000, 2003)
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		ctx := user
		if i%2 == 0 {
			ctx = system
		}
		wg.Go(func() {
			_, errs[i] = s.backend(registry.EndpointService).Apply(ctx, service(fmt.Sprintf("s%d", i), "a", tcp, 22, ""))
		})
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case connect.CodeOf(err) != connect.CodeResourceExhausted:
			t.Errorf("a creator was refused with %v", err)
		}
	}
	all, err := s.endpointServices(system, "")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int32]bool{}
	for _, es := range all {
		if p := es.GetStatus().GetTransitPort(); seen[p] {
			t.Errorf("transit port %d drawn twice", p)
		}
		seen[es.GetStatus().GetTransitPort()] = true
	}
	if won != 4 || len(all) != 4 {
		t.Errorf("%d of 10 creators won 4 transit ports, %d stored", won, len(all))
	}
}

// TestANetworkEndpointReachesItsService: an endpoint on a network whose
// range overlaps the service's gets an address of its own network's range
// that no machine holds, waits for its service and its machine, then
// connects; the service reports where it leads.
func TestANetworkEndpointReachesItsService(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyNet(t, s, user, network("svc", "10.0.0.0/24"))
	applyNet(t, s, user, network("ws", "10.0.0.0/24"))
	applyCtr(t, s, user, ctrOn("a", "ws"))
	a := waitMachine(t, s, system, "ct-a", "addressed", func(m *computev1alpha1.Machine) bool { return ipv4Of(m) != "" })

	applyEndpoint(t, s, user, netEndpoint("git", "ws", "git"))
	ne := waitNetEndpoint(t, s, system, "git", "waiting for its service", endpointIs(ReasonServiceNotFound))
	addr := netip.MustParseAddr(ne.GetStatus().GetIpv4())
	if !netip.MustParsePrefix("10.0.0.0/24").Contains(addr) || netip.MustParsePrefix(ipv4Of(a)).Addr() == addr {
		t.Errorf("the endpoint's address %s is not a free one of ws (ct-a holds %s)", addr, ipv4Of(a))
	}
	if ne.GetStatus().GetHostname() != "git.web.hangar.internal" {
		t.Errorf("the endpoint's hostname is %q", ne.GetStatus().GetHostname())
	}

	applyService(t, s, user, service("git", "git", tcp, 22, ""))
	waitService(t, s, system, "git", "waiting for its target", func(es *networkv1alpha1.EndpointService) bool {
		return endpointReason(es.GetStatus().GetConditions()) == ReasonTargetNotFound
	})
	waitNetEndpoint(t, s, system, "git", "waiting for its service to lead somewhere", endpointIs(ReasonServicePending))
	applyCtr(t, s, user, ctrOn("git", "svc"))
	es := waitService(t, s, system, "git", "serving", func(es *networkv1alpha1.EndpointService) bool {
		return endpointReason(es.GetStatus().GetConditions()) == ReasonServing
	})
	if es.GetStatus().GetMachine() != "ct-git" || es.GetStatus().GetNetwork() != "svc" || es.GetStatus().GetNode() != "n1" {
		t.Errorf("the service leads to %q on %q at %q", es.GetStatus().GetMachine(), es.GetStatus().GetNetwork(), es.GetStatus().GetNode())
	}
	ne = waitNetEndpoint(t, s, system, "git", "connected", endpointIs(ReasonConnected))
	if ne.GetStatus().GetIpv4() != addr.String() || ne.GetStatus().GetPort() != 22 || ne.GetStatus().GetNode() != "n1" ||
		!conditions.IsTrue(ne.GetStatus().GetConditions(), EndpointReady) {
		t.Errorf("connected, the endpoint is %v", ne.GetStatus())
	}

	// A machine made after on the network does not take the address.
	applyCtr(t, s, user, ctrOn("b", "ws"))
	b := waitMachine(t, s, system, "ct-b", "addressed", func(m *computev1alpha1.Machine) bool { return ipv4Of(m) != "" })
	if netip.MustParsePrefix(ipv4Of(b)).Addr() == addr {
		t.Errorf("a machine was given the endpoint's address %s", addr)
	}

	// An endpoint on the service's own network leads nowhere.
	applyEndpoint(t, s, user, netEndpoint("self", "svc", "git"))
	waitNetEndpoint(t, s, system, "self", "on the service's network", endpointIs(ReasonSameNetwork))
}

// TestEndpointAddressesAreOneDecision: endpoints of one network given their
// addresses at once each get another, as the draw and the write are one
// decision under ipam, which a machine's address is drawn under too.
func TestEndpointAddressesAreOneDecision(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	applyNet(t, s, user, network("ws", "10.2.0.0/24"))
	const n = 24
	for i := range n {
		applyEndpoint(t, s, user, netEndpoint(fmt.Sprintf("e%d", i), "ws", "git"))
	}
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			// A write that lost to another's is retried, as the controller
			// would.
			for {
				err := s.reconcileNetworkEndpoint(system, controller.Key{Project: "web", Name: fmt.Sprintf("e%d", i)})
				if connect.CodeOf(err) != connect.CodeAborted {
					if err != nil {
						t.Error(err)
					}
					return
				}
			}
		})
	}
	wg.Wait()
	all, err := s.networkEndpoints(system, "web")
	if err != nil {
		t.Fatal(err)
	}
	held := map[string]string{}
	for _, ne := range all {
		a, name := ne.GetStatus().GetIpv4(), ne.GetMetadata().GetName()
		if a == "" {
			t.Errorf("%s was given no address", name)
			continue
		}
		if other, ok := held[a]; ok {
			t.Errorf("%s and %s both hold %s", other, name, a)
		}
		held[a] = name
	}
}

// TestTheSelectorDecides: a network the service's selector does not match
// is not allowed, and is once its labels match.
func TestTheSelectorDecides(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyNet(t, s, user, network("svc", "10.1.0.0/24"))
	applyNet(t, s, user, network("ws", "10.2.0.0/24"))
	applyCtr(t, s, user, ctrOn("git", "svc"))
	applyService(t, s, user, service("git", "git", tcp, 22, "tier=workstream"))
	applyEndpoint(t, s, user, netEndpoint("git", "ws", "git"))
	ne := waitNetEndpoint(t, s, system, "git", "not allowed", endpointIs(ReasonNotAllowed))
	if !strings.Contains(conditions.Find(ne.GetStatus().GetConditions(), EndpointReady).GetMessage(), "tier=workstream") {
		t.Errorf("not allowed says %q", conditions.Find(ne.GetStatus().GetConditions(), EndpointReady).GetMessage())
	}
	labelled := network("ws", "10.2.0.0/24")
	labelled.Metadata.Labels = map[string]string{"tier": "workstream"}
	applyNet(t, s, user, labelled)
	waitNetEndpoint(t, s, system, "git", "allowed once labelled", endpointIs(ReasonConnected))
	applyNet(t, s, user, network("ws", "10.2.0.0/24"))
	waitNetEndpoint(t, s, system, "git", "not allowed once the label is gone", endpointIs(ReasonNotAllowed))
}

// TestAMachineKeepsItsName: an endpoint named as a machine of its network
// has no hostname; the machine keeps the name.
func TestAMachineKeepsItsName(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyNet(t, s, user, network("ws", "10.2.0.0/24"))
	applyEndpoint(t, s, user, netEndpoint("git", "ws", "git"))
	waitNetEndpoint(t, s, system, "git", "named", func(ne *networkv1alpha1.NetworkEndpoint) bool { return ne.GetStatus().GetHostname() != "" })
	applyCtr(t, s, user, ctrOn("git", "ws"))
	waitNetEndpoint(t, s, system, "git", "unnamed beside container git", func(ne *networkv1alpha1.NetworkEndpoint) bool {
		return ne.GetStatus().GetHostname() == "" && ne.GetStatus().GetIpv4() != ""
	})
	// An alias on the network keeps the name as well, as the network's
	// resolver answers it for the machine.
	applyEndpoint(t, s, user, netEndpoint("mirror", "ws", "git"))
	waitNetEndpoint(t, s, system, "mirror", "named", func(ne *networkv1alpha1.NetworkEndpoint) bool { return ne.GetStatus().GetHostname() != "" })
	aliased := ctrOn("web", "ws")
	aliased.Spec.Interfaces[0].Aliases = []string{"mirror"}
	applyCtr(t, s, user, aliased)
	waitNetEndpoint(t, s, system, "mirror", "unnamed beside alias mirror", func(ne *networkv1alpha1.NetworkEndpoint) bool {
		return ne.GetStatus().GetHostname() == ""
	})
}

// TestADeletedEndpointHoldsItsAddressUntilItsNodeLetsGo: the address of an
// endpoint being deleted is given to no machine until the node that held
// it reports it let go; no other node can say so.
func TestADeletedEndpointHoldsItsAddressUntilItsNodeLetsGo(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1", "n2")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyNet(t, s, user, network("svc", "10.1.0.0/24"))
	applyNet(t, s, user, network("ws", "10.2.0.0/24"))
	applyCtr(t, s, user, ctrOn("git", "svc"))
	applyService(t, s, user, service("git", "git", tcp, 22, ""))
	applyEndpoint(t, s, user, netEndpoint("git", "ws", "git"))
	ne := waitNetEndpoint(t, s, system, "git", "connected", endpointIs(ReasonConnected))
	key := resource.Key{Project: "web", Name: "git"}
	b := s.backend(registry.NetworkEndpoint)
	if _, err := b.Delete(user, key, nil); err != nil {
		t.Fatal(err)
	}
	waitNetEndpoint(t, s, system, "git", "being deleted", func(ne *networkv1alpha1.NetworkEndpoint) bool { return ne.GetMetadata().GetDeleteTime() != nil })
	// The endpoint holds the lowest address, which a machine is given
	// first once it is free.
	applyCtr(t, s, user, ctrOn("m0", "ws"))
	m := waitMachine(t, s, system, "ct-m0", "addressed", func(m *computev1alpha1.Machine) bool { return ipv4Of(m) != "" })
	if netip.MustParsePrefix(ipv4Of(m)).Addr().String() == ne.GetStatus().GetIpv4() {
		t.Fatalf("a machine took the address %s of an endpoint being deleted", ne.GetStatus().GetIpv4())
	}
	released := func() *networkv1alpha1.NetworkEndpointStatus {
		st := proto.Clone(ne.GetStatus()).(*networkv1alpha1.NetworkEndpointStatus)
		st.Conditions = append(st.Conditions, &metav1.Condition{
			Type: EndpointReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "NoLongerCarried",
		})
		// What a node sends besides its condition is not taken.
		st.Ipv4, st.Node = "10.2.0.6", "n2"
		return st
	}
	_, err := b.UpdateStatus(asNode(system, "n2"), key, released(), nil)
	wantCodeOf(t, err, connect.CodePermissionDenied, "another node reporting the address let go")
	if _, err := b.UpdateStatus(asNode(system, "n1"), key, released(), nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.NetworkEndpoint, "git")
	applyCtr(t, s, user, ctrOn("m1", "ws"))
	waitMachine(t, s, system, "ct-m1", "given the address let go", func(m *computev1alpha1.Machine) bool {
		return ipv4Of(m) != "" && netip.MustParsePrefix(ipv4Of(m)).Addr().String() == ne.GetStatus().GetIpv4()
	})
}

// TestAnEndpointGoesWithItsNetwork: deleting a network deletes its
// endpoints; one its node never carried goes at once.
func TestAnEndpointGoesWithItsNetwork(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	applyNet(t, s, user, network("ws", "10.2.0.0/24"))
	applyEndpoint(t, s, user, netEndpoint("git", "ws", "git"))
	waitNetEndpoint(t, s, system, "git", "addressed", func(ne *networkv1alpha1.NetworkEndpoint) bool { return ne.GetStatus().GetIpv4() != "" })
	if _, err := s.backend(registry.Network).Delete(user, resource.Key{Project: "web", Name: "ws"}, nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.NetworkEndpoint, "git")
}

// TestANodeReadsOnlyItsEndpoints: a node reads the services and endpoints
// it carries, and no other.
func TestANodeReadsOnlyItsEndpoints(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1", "n2")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyNet(t, s, user, network("svc", "10.1.0.0/24"))
	applyNet(t, s, user, network("ws", "10.2.0.0/24"))
	applyCtr(t, s, user, ctrOn("git", "svc"))
	applyService(t, s, user, service("git", "git", tcp, 22, ""))
	applyEndpoint(t, s, user, netEndpoint("git", "ws", "git"))
	waitNetEndpoint(t, s, system, "git", "connected", endpointIs(ReasonConnected))
	key := resource.Key{Project: "web", Name: "git"}
	for _, r := range []registry.Resource{registry.EndpointService, registry.NetworkEndpoint} {
		b := s.backend(r)
		if _, err := b.Get(asNode(system, "n1"), key); err != nil {
			t.Errorf("its node reading %s git: %v", r.Kind, err)
		}
		_, err := b.Get(asNode(system, "n2"), key)
		wantCodeOf(t, err, connect.CodePermissionDenied, "another node reading "+r.Kind+" git")
	}
}

// TestEndpointPermissions: network.endpointservices.* and
// network.networkendpoints.* are the kinds' permissions; a viewer writes
// neither.
func TestEndpointPermissions(t *testing.T) {
	t.Parallel()
	for _, kind := range []registry.Resource{registry.EndpointService, registry.NetworkEndpoint} {
		for _, v := range []authz.Verb{authz.Get, authz.List, authz.Apply, authz.Delete} {
			perm := "network." + kind.Plural + "." + string(v)
			if r, verb, ok := authz.LookupPermission(perm); !ok || r.Kind != kind.Kind || verb != v {
				t.Errorf("%s is %v %v %t", perm, r.Kind, verb, ok)
			}
		}
	}
	s, system, _ := running(t)
	viewer := bindProjectRole(t, s, system, "carol@example.com", "project.viewer")
	_, err := s.backend(registry.EndpointService).Apply(viewer, service("git", "git", tcp, 22, ""))
	wantCodeOf(t, err, connect.CodePermissionDenied, "a viewer making a service")
	_, err = s.backend(registry.NetworkEndpoint).Apply(viewer, netEndpoint("git", "default", "git"))
	wantCodeOf(t, err, connect.CodePermissionDenied, "a viewer making an endpoint")
}

func TestNetworkEndpointsAreCounted(t *testing.T) {
	t.Parallel()
	s, _, user := running(t)
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{NetworkEndpoints: proto.Int32(1)})
	applyEndpoint(t, s, user, netEndpoint("one", "default", "git"))
	_, err := s.backend(registry.NetworkEndpoint).Apply(user, netEndpoint("two", "default", "git"))
	wantExhausted(t, err, "network_endpoints limit is 1", "a second endpoint past the plan")
}
