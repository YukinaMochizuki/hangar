package apiserver

import (
	"context"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/login"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// endpoint is a PublicEndpoint of project web reaching container ctr's port.
func endpoint(name, ctr string, port int32, access networkv1alpha1.PublicEndpointAccess) *networkv1alpha1.PublicEndpoint {
	return endpointIn("web", name, ctr, port, access)
}

func endpointIn(project, name, ctr string, port int32, access networkv1alpha1.PublicEndpointAccess) *networkv1alpha1.PublicEndpoint {
	return &networkv1alpha1.PublicEndpoint{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec: &networkv1alpha1.PublicEndpointSpec{
			Target: &networkv1alpha1.PublicEndpointTarget{Object: &networkv1alpha1.PublicEndpointTarget_Container{Container: ctr}},
			Port:   port, Access: access,
		},
	}
}

const (
	private = networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PRIVATE
	public  = networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PUBLIC
)

func waitEndpoint(t *testing.T, s *Server, system context.Context, name, what string, ok func(*networkv1alpha1.PublicEndpoint) bool) *networkv1alpha1.PublicEndpoint {
	t.Helper()
	return waitObject(t, s, system, registry.PublicEndpoint, resource.Key{Project: "web", Name: name}, "public endpoint "+name+" "+what, ok)
}

func endpointReady(ep *networkv1alpha1.PublicEndpoint) *metav1.Condition {
	return conditions.Find(ep.GetStatus().GetConditions(), PublicEndpointReady)
}

// project makes another project of tenant acme as user.
func project(t *testing.T, s *Server, user context.Context, name string) {
	t.Helper()
	if _, err := s.backend(registry.Project).Apply(user, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: name}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "acme"},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPublicEndpointAdmission(t *testing.T) {
	t.Parallel()
	s, _, user := running(t)
	b := s.backend(registry.PublicEndpoint)
	if _, err := b.Apply(user, endpoint("site", "api", 8080, 0)); err != nil {
		t.Fatalf("an endpoint of a container not made yet: %v", err)
	}
	noTarget := endpoint("none", "api", 80, private)
	noTarget.Spec.Target = nil
	badAccess := endpoint("access", "api", 80, private)
	badAccess.Spec.Access = 7
	for what, ep := range map[string]*networkv1alpha1.PublicEndpoint{
		"no target":                                noTarget,
		"a target's name not a name":               endpoint("bad", "Not_A_Name", 80, private),
		"port 0":                                   endpoint("p0", "api", 0, private),
		"port 65536":                               endpoint("p1", "api", 65536, private),
		"an access that is neither":                badAccess,
		"a hostname label longer than 63":          endpoint(strings.Repeat("a", 60), "api", 80, private),
		"a label of an internationalized name":     endpoint("xn--abc", "api", 80, private),
		"a label the platform keeps":               endpoint("hangar-status", "api", 80, private),
		"a label with -- in its 3rd and 4th place": endpoint("ab--x", "api", 80, private),
	} {
		_, err := b.Apply(user, ep)
		wantCodeOf(t, err, connect.CodeInvalidArgument, what)
	}
	// 59 characters and "-web" make 63, the most a label holds.
	if _, err := b.Apply(user, endpoint(strings.Repeat("a", 59), "api", 80, private)); err != nil {
		t.Errorf("a label of 63 characters: %v", err)
	}
	got, err := b.Get(user, resource.Key{Project: "web", Name: "site"})
	if err != nil {
		t.Fatal(err)
	}
	if fs := got.(*networkv1alpha1.PublicEndpoint).GetMetadata().GetFinalizers(); len(fs) != 0 {
		t.Errorf("a new endpoint's finalizers: %v", fs)
	}
}

// TestAHostnameIsClaimedForGood: endpoint "a-b" of project web and endpoint
// "a" of project b-web have one hostname; whichever is made first holds it
// for good, against the other even after it is deleted, while the endpoint
// itself may be made again.
func TestAHostnameIsClaimedForGood(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	project(t, s, user, "b-web")
	b := s.backend(registry.PublicEndpoint)
	if _, err := b.Apply(user, endpoint("a-b", "api", 80, private)); err != nil {
		t.Fatal(err)
	}
	shadow := endpointIn("b-web", "a", "api", 80, private)
	_, err := b.Apply(user, shadow)
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "an endpoint of a hostname another holds")

	key := resource.Key{Project: "web", Name: "a-b"}
	if _, err := b.Delete(user, key, nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.PublicEndpoint, "a-b")
	_, err = b.Apply(user, shadow)
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "an endpoint of a hostname once held by another")
	if _, err := b.Apply(user, endpoint("a-b", "api", 80, private)); err != nil {
		t.Errorf("the endpoint holding the hostname, made again: %v", err)
	}
}

// TestAHostnameIsClaimedOnce: endpoints of one hostname in several projects
// made at once, only one is.
func TestAHostnameIsClaimedOnce(t *testing.T) {
	t.Parallel()
	s, _, user := running(t)
	// Every endpoint below has the label "x-y-z-web".
	cands := []*networkv1alpha1.PublicEndpoint{
		endpointIn("web", "x-y-z", "api", 80, private),
		endpointIn("z-web", "x-y", "api", 80, private),
		endpointIn("y-z-web", "x", "api", 80, private),
	}
	for _, p := range []string{"z-web", "y-z-web"} {
		project(t, s, user, p)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(cands))
	for i, ep := range cands {
		wg.Go(func() { _, errs[i] = s.backend(registry.PublicEndpoint).Apply(user, ep) })
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		if err == nil {
			won++
		} else {
			wantCodeOf(t, err, connect.CodeFailedPrecondition, "an endpoint that lost the hostname")
		}
	}
	if won != 1 {
		t.Errorf("%d endpoints of one hostname made; errors %v", won, errs)
	}
}

// TestTheEdgeReadsOnlyWhatItNeeds: the Edge principal reads endpoints,
// bindings, projects and users everywhere, and does nothing else.
func TestTheEdgeReadsOnlyWhatItNeeds(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	project(t, s, user, "other")
	for _, ep := range []*networkv1alpha1.PublicEndpoint{endpoint("site", "api", 80, private), endpointIn("other", "site", "api", 80, public)} {
		if _, err := s.backend(registry.PublicEndpoint).Apply(user, ep); err != nil {
			t.Fatal(err)
		}
	}
	applyCtr(t, s, user, ctr("api"))
	edge := auth.WithPrincipal(system, auth.Principal{Type: auth.Edge, ID: "h1"})
	eps, err := s.backend(registry.PublicEndpoint).List(edge, resource.ListOptions{})
	if err != nil || len(eps.Items) != 2 {
		t.Errorf("the edge listing every project's endpoints: %d, %v", len(eps.Items), err)
	}
	if rbs, err := s.backend(registry.RoleBinding).List(edge, resource.ListOptions{}); err != nil || len(rbs.Items) == 0 {
		t.Errorf("the edge listing role bindings: %d, %v", len(rbs.Items), err)
	}
	if _, err := s.backend(registry.Project).Get(edge, resource.Key{Name: "web"}); err != nil {
		t.Errorf("the edge reading a project: %v", err)
	}
	if users, err := s.backend(registry.User).List(edge, resource.ListOptions{}); err != nil || len(users.Items) == 0 {
		t.Errorf("the edge listing users: %d, %v", len(users.Items), err)
	}
	_, err = s.backend(registry.Container).Get(edge, resource.Key{Project: "web", Name: "api"})
	wantCodeOf(t, err, connect.CodePermissionDenied, "the edge reading a container")
	_, err = s.backend(registry.Machine).List(edge, resource.ListOptions{Project: "web"})
	wantCodeOf(t, err, connect.CodePermissionDenied, "the edge listing machines")
	_, err = s.backend(registry.PublicEndpoint).Apply(edge, endpoint("mine", "api", 80, public))
	wantCodeOf(t, err, connect.CodePermissionDenied, "the edge making an endpoint")
	_, err = s.backend(registry.PublicEndpoint).Delete(edge, resource.Key{Project: "web", Name: "site"}, nil)
	wantCodeOf(t, err, connect.CodePermissionDenied, "the edge deleting an endpoint")
	_, err = s.backend(registry.PublicEndpoint).UpdateStatus(edge, resource.Key{Project: "web", Name: "site"}, &networkv1alpha1.PublicEndpointStatus{Machine: "x"}, nil)
	wantCodeOf(t, err, connect.CodePermissionDenied, "the edge writing an endpoint's status")
}

// runningMachine has container name's machine placed on n1 and reported
// running.
func runningMachine(t *testing.T, s *Server, system context.Context, name string) *computev1alpha1.Machine {
	t.Helper()
	m := waitMachine(t, s, system, machineName(name), "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration())
	return m
}

func TestAnEndpointFollowsItsTarget(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	if _, err := s.backend(registry.PublicEndpoint).Apply(user, endpoint("site", "api", 8080, private)); err != nil {
		t.Fatal(err)
	}
	ep := waitEndpoint(t, s, system, "site", "waiting for its target", func(ep *networkv1alpha1.PublicEndpoint) bool {
		return endpointReady(ep).GetReason() == ReasonTargetNotFound
	})
	if st := ep.GetStatus(); st.GetHostname() != "site-web.hangarlab.dev" || st.GetUrl() != "https://site-web.hangarlab.dev" {
		t.Errorf("hostname %q url %q", st.GetHostname(), st.GetUrl())
	}
	applyCtr(t, s, user, ctr("api"))
	waitEndpoint(t, s, system, "site", "waiting for the machine", func(ep *networkv1alpha1.PublicEndpoint) bool {
		return endpointReady(ep).GetReason() == ReasonMachineNotReady && ep.GetStatus().GetMachine() == "ct-api"
	})
	runningMachine(t, s, system, "api")
	ep = waitEndpoint(t, s, system, "site", "ready", func(ep *networkv1alpha1.PublicEndpoint) bool {
		return conditions.IsTrue(ep.GetStatus().GetConditions(), PublicEndpointReady)
	})
	if ep.GetStatus().GetNode() != "n1" || len(ep.GetStatus().GetConditions()) != 1 {
		t.Errorf("a ready private endpoint: %v", ep.GetStatus())
	}
	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "api"}, nil); err != nil {
		t.Fatal(err)
	}
	waitEndpoint(t, s, system, "site", "without its target", func(ep *networkv1alpha1.PublicEndpoint) bool {
		return endpointReady(ep).GetReason() == ReasonTargetNotFound && ep.GetStatus().GetMachine() == ""
	})
}

// TestAPublicEndpointNeedsNothingMore: a PUBLIC endpoint is ready once its
// machine runs, as a PRIVATE one is, and goes at once when deleted.
func TestAPublicEndpointNeedsNothingMore(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyCtr(t, s, user, ctr("api"))
	runningMachine(t, s, system, "api")
	if _, err := s.backend(registry.PublicEndpoint).Apply(user, endpoint("site", "api", 8080, public)); err != nil {
		t.Fatal(err)
	}
	waitEndpoint(t, s, system, "site", "ready", func(ep *networkv1alpha1.PublicEndpoint) bool {
		return conditions.IsTrue(ep.GetStatus().GetConditions(), PublicEndpointReady)
	})
	if _, err := s.backend(registry.PublicEndpoint).Delete(user, resource.Key{Project: "web", Name: "site"}, nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.PublicEndpoint, "site")
}

// TestAnEndpointLosesWhatAccessLeft: an endpoint stored while PUBLIC ones
// had a Cloudflare Access application, with its finalizer and condition,
// loses both at its next reconcile, and so is not held on delete.
func TestAnEndpointLosesWhatAccessLeft(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	b := s.backend(registry.PublicEndpoint)
	if _, err := b.Apply(user, endpoint("site", "api", 8080, public)); err != nil {
		t.Fatal(err)
	}
	key := resource.Key{Project: "web", Name: "site"}
	skey := store.Key{Kind: registry.PublicEndpoint.Kind, Project: "web", Name: "site"}
	o, err := s.store.Get(system, skey)
	if err != nil {
		t.Fatal(err)
	}
	var ep networkv1alpha1.PublicEndpoint
	if err := proto.Unmarshal(o.Data, &ep); err != nil {
		t.Fatal(err)
	}
	ep.Metadata.Finalizers = append(ep.Metadata.Finalizers, oldAccessFinalizer)
	ep.Status = &networkv1alpha1.PublicEndpointStatus{Conditions: []*metav1.Condition{{
		Type: oldAccessCondition, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Created",
	}}}
	data, err := proto.Marshal(&ep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Update(system, skey, data, o.Revision); err != nil {
		t.Fatal(err)
	}
	reconcile := func() {
		t.Helper()
		if err := s.reconcilePublicEndpoint(system, controller.Key{Project: "web", Name: "site"}); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	reconcile()
	obj, err := s.get(system, registry.PublicEndpoint, key)
	if err != nil {
		t.Fatal(err)
	}
	got := obj.(*networkv1alpha1.PublicEndpoint)
	if fs := got.GetMetadata().GetFinalizers(); len(fs) != 0 {
		t.Errorf("finalizers left: %v", fs)
	}
	if c := conditions.Find(got.GetStatus().GetConditions(), oldAccessCondition); c != nil {
		t.Errorf("the Access condition left: %v", c)
	}
	if _, err := b.Delete(user, key, nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.PublicEndpoint, "site")
}

// TestAContainersPublicPortsGetEndpoints: each public port gets a private
// endpoint the container owns, which follows the port and goes with it; a
// name another endpoint holds is reported, and taken once it is free.
func TestAContainersPublicPortsGetEndpoints(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	if _, err := s.backend(registry.PublicEndpoint).Apply(user, endpoint("api-admin", "other", 9000, public)); err != nil {
		t.Fatal(err)
	}
	c := ctr("api")
	c.Spec.Ports = []*computev1alpha1.ContainerPort{
		{Name: "http", Port: 8080, Public: true},
		{Name: "admin", Port: 9090, Public: true},
		{Name: "metrics", Port: 9100},
	}
	applyCtr(t, s, user, c)
	ep := waitEndpoint(t, s, system, "api-http", "made", func(*networkv1alpha1.PublicEndpoint) bool { return true })
	want := containerEndpointSpec("api", 8080)
	if !proto.Equal(ep.GetSpec(), want) || !ownedBy(ep.GetMetadata(), waitContainer(t, s, system, "api", "made", func(*computev1alpha1.Container) bool { return true }).GetMetadata().GetUid()) {
		t.Errorf("the endpoint of port http: %v", ep)
	}
	waitContainer(t, s, system, "api", "told the name is taken", func(c *computev1alpha1.Container) bool {
		cond := conditions.Find(c.GetStatus().GetConditions(), ContainerPublicEndpoints)
		return cond.GetReason() == "NameTaken" && strings.Contains(cond.GetMessage(), "api-admin")
	})
	if obj, _ := s.get(system, registry.PublicEndpoint, resource.Key{Project: "web", Name: "api-metrics"}); obj != nil {
		t.Error("a port not public got an endpoint")
	}

	// The other endpoint gone, the container takes the name.
	if _, err := s.backend(registry.PublicEndpoint).Delete(user, resource.Key{Project: "web", Name: "api-admin"}, nil); err != nil {
		t.Fatal(err)
	}
	waitEndpoint(t, s, system, "api-admin", "the container's", func(ep *networkv1alpha1.PublicEndpoint) bool {
		return ep.GetSpec().GetPort() == 9090 && ep.GetSpec().GetAccess() == private
	})
	waitContainer(t, s, system, "api", "with its endpoints", func(c *computev1alpha1.Container) bool {
		return conditions.IsTrue(c.GetStatus().GetConditions(), ContainerPublicEndpoints)
	})

	// A port moved, and one no longer public.
	c.Spec.Ports = []*computev1alpha1.ContainerPort{{Name: "http", Port: 8081, Public: true}, {Name: "admin", Port: 9090}}
	applyCtr(t, s, user, c)
	waitEndpoint(t, s, system, "api-http", "moved", func(ep *networkv1alpha1.PublicEndpoint) bool { return ep.GetSpec().GetPort() == 8081 })
	waitGone(t, s, system, registry.PublicEndpoint, "api-admin")

	// No public port at all: the condition goes too.
	c.Spec.Ports = nil
	applyCtr(t, s, user, c)
	waitGone(t, s, system, registry.PublicEndpoint, "api-http")
	waitContainer(t, s, system, "api", "without the condition", func(c *computev1alpha1.Container) bool {
		return conditions.Find(c.GetStatus().GetConditions(), ContainerPublicEndpoints) == nil
	})
}

func TestPublicPortAdmission(t *testing.T) {
	t.Parallel()
	s, _, user := running(t)
	for what, port := range map[string]*computev1alpha1.ContainerPort{
		"a public UDP port":                 {Name: "dns", Port: 53, Protocol: computev1alpha1.Protocol_PROTOCOL_UDP, Public: true},
		"a public port whose label is long": {Name: strings.Repeat("p", 56), Port: 80, Public: true},
	} {
		c := ctr("api")
		c.Spec.Ports = []*computev1alpha1.ContainerPort{port}
		_, err := s.backend(registry.Container).Apply(user, c)
		wantCodeOf(t, err, connect.CodeInvalidArgument, what)
	}
	c := ctr("api")
	c.Spec.Ports = []*computev1alpha1.ContainerPort{{Name: "dns", Port: 53, Protocol: computev1alpha1.Protocol_PROTOCOL_UDP}}
	if _, err := s.backend(registry.Container).Apply(user, c); err != nil {
		t.Errorf("a UDP port that is not public: %v", err)
	}
}

// TestEndpointLoginsVerdict: endpoint-login hands off to an endpoint that
// stands, by a host exactly one label below the domain, and to none being
// deleted, nor to a PUBLIC one. (No endpoint's label holds a dot, so a host
// of more labels could not name one anyway; the check stands in case.)
func TestEndpointLoginsVerdict(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	b := s.backend(registry.PublicEndpoint)
	for _, ep := range []*networkv1alpha1.PublicEndpoint{endpoint("site", "api", 8080, private), endpoint("open", "api", 8080, public)} {
		if _, err := b.Apply(user, ep); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := auth.FromContext(user)
	verdict := func(host string) login.EndpointVerdict {
		t.Helper()
		v, err := s.endpointVerdict(system, p, host)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := verdict("site-web.hangarlab.dev"); v != login.EndpointAllowed {
		t.Fatalf("a standing endpoint: %v", v)
	}
	if v := verdict("open-web.hangarlab.dev"); v != login.EndpointUnknown {
		t.Errorf("a public endpoint: %v", v)
	}
	// Exactly one label below the domain, and the domain itself, alone.
	for _, h := range []string{"a.site-web.hangarlab.dev", "site-web.a.hangarlab.dev", ".hangarlab.dev", "hangarlab.dev", "site-web.hangarlab.dev.evil", "site-webhangarlab.dev"} {
		if v := verdict(h); v != login.EndpointUnknown {
			t.Errorf("%s: %v", h, v)
		}
	}
	// Being deleted: held by a finalizer, with its delete time set.
	skey := store.Key{Kind: registry.PublicEndpoint.Kind, Project: "web", Name: "site"}
	o, err := s.store.Get(system, skey)
	if err != nil {
		t.Fatal(err)
	}
	var ep networkv1alpha1.PublicEndpoint
	if err := proto.Unmarshal(o.Data, &ep); err != nil {
		t.Fatal(err)
	}
	ep.Metadata.Finalizers = append(ep.Metadata.Finalizers, "example.com/hold")
	ep.Metadata.DeleteTime = timestamppb.Now()
	data, err := proto.Marshal(&ep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Update(system, skey, data, o.Revision); err != nil {
		t.Fatal(err)
	}
	if v := verdict("site-web.hangarlab.dev"); v != login.EndpointUnknown {
		t.Errorf("an endpoint being deleted: %v", v)
	}
}
