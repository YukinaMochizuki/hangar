package apiserver

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/cloudflare"
	"github.com/YukinaMochizuki/hangar/internal/cloudflare/cftest"
	"github.com/YukinaMochizuki/hangar/internal/controller"
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
	var last *networkv1alpha1.PublicEndpoint
	deadline := time.Now().Add(10 * time.Second)
	for {
		obj, err := s.get(system, registry.PublicEndpoint, resource.Key{Project: "web", Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if obj != nil {
			last = obj.(*networkv1alpha1.PublicEndpoint)
			if ok(last) {
				return last
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("public endpoint %s never %s; last seen: %v", name, what, last)
		}
		time.Sleep(5 * time.Millisecond)
	}
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
	if fs := got.(*networkv1alpha1.PublicEndpoint).GetMetadata().GetFinalizers(); len(fs) != 1 || fs[0] != AccessApplicationFinalizer {
		t.Errorf("a new endpoint's finalizers: %v", fs)
	}
}

// TestAHostnameIsClaimedForGood: endpoint "a-b" of project web and endpoint
// "a" of project b-web have one hostname; whichever is made first holds it
// for good, against the other even after it is deleted, while the endpoint
// itself may be made again.
func TestAHostnameIsClaimedForGood(t *testing.T) {
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
	if err := s.RemoveFinalizer(system, registry.PublicEndpoint, key, AccessApplicationFinalizer); err != nil {
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
	if ep.GetStatus().GetNode() != "n1" || conditions.Find(ep.GetStatus().GetConditions(), PublicEndpointAccessApplication) != nil {
		t.Errorf("a ready private endpoint: %v", ep.GetStatus())
	}
	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "api"}, nil); err != nil {
		t.Fatal(err)
	}
	waitEndpoint(t, s, system, "site", "without its target", func(ep *networkv1alpha1.PublicEndpoint) bool {
		return endpointReady(ep).GetReason() == ReasonTargetNotFound && ep.GetStatus().GetMachine() == ""
	})
}

func TestAPublicEndpointNeedsCloudflare(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyCtr(t, s, user, ctr("api"))
	runningMachine(t, s, system, "api")
	if _, err := s.backend(registry.PublicEndpoint).Apply(user, endpoint("site", "api", 8080, public)); err != nil {
		t.Fatal(err)
	}
	waitEndpoint(t, s, system, "site", "not configured", func(ep *networkv1alpha1.PublicEndpoint) bool {
		return endpointReady(ep).GetReason() == ReasonCloudflareNotConfigured
	})
	// Without an application to delete, it goes at once.
	if _, err := s.backend(registry.PublicEndpoint).Delete(user, resource.Key{Project: "web", Name: "site"}, nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.PublicEndpoint, "site")
}

const (
	cfToken   = "cf-token-for-tests"
	cfAccount = "0123456789abcdef0123456789abcdef"
)

// withCloudflare points the server's endpoints at a fake Cloudflare with a
// bypass policy, as hangar-cloudflare leaves it.
func withCloudflare(t *testing.T) (*cftest.Fake, func(*Config)) {
	t.Helper()
	f := cftest.New(cfToken, cfAccount, "hangarlab.dev")
	t.Cleanup(f.Close)
	c, err := cloudflare.New(cfToken, cloudflare.WithBaseURL(f.URL()), cloudflare.WithRetry(1, time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.CreateAccessPolicy(t.Context(), cfAccount, cloudflare.AccessPolicy{
		Name: "hangarlab.dev: public endpoints, everyone", Decision: "bypass", Include: []json.RawMessage{cloudflare.EveryoneRule()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, func(cfg *Config) {
		cfg.PublicEndpoints = PublicEndpointsConfig{Cloudflare: c, AccountID: cfAccount, BypassPolicyID: p.ID}
	}
}

// apps returns the fake's applications, by hostname.
func apps(f *cftest.Fake) map[string][]*cftest.App {
	out := map[string][]*cftest.App{}
	f.Mutate(func(s *cftest.State) {
		for _, a := range s.Apps {
			c := *a
			out[a.Domain] = append(out[a.Domain], &c)
		}
	})
	return out
}

func TestAPublicEndpointOpensThroughAnAccessApplication(t *testing.T) {
	f, cf := withCloudflare(t)
	s, system, user := runningWith(t, func(c *Config) {
		c.DisableControllers = false
		c.Nodes = []string{"n1"}
		c.NodeMonitorInterval = 1000 * time.Hour
		cf(c)
	})
	waitNode(t, s, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyCtr(t, s, user, ctr("api"))
	runningMachine(t, s, system, "api")
	b := s.backend(registry.PublicEndpoint)
	if _, err := b.Apply(user, endpoint("site", "api", 8080, public)); err != nil {
		t.Fatal(err)
	}
	const host = "site-web.hangarlab.dev"
	ep := waitEndpoint(t, s, system, "site", "ready", func(ep *networkv1alpha1.PublicEndpoint) bool {
		return conditions.IsTrue(ep.GetStatus().GetConditions(), PublicEndpointReady)
	})
	got := apps(f)[host]
	if len(got) != 1 || got[0].ID != ep.GetStatus().GetAccessApplicationId() || got[0].Name != cloudflare.EndpointAppPrefix+host ||
		len(got[0].Links) != 1 || got[0].Links[0].ID != s.cfg.PublicEndpoints.BypassPolicyID {
		t.Fatalf("the endpoint's application: %+v; status %v", got, ep.GetStatus())
	}

	// Private again: the application goes.
	if _, err := b.Apply(user, endpoint("site", "api", 8080, private)); err != nil {
		t.Fatal(err)
	}
	waitEndpoint(t, s, system, "site", "private", func(ep *networkv1alpha1.PublicEndpoint) bool {
		return ep.GetStatus().GetAccessApplicationId() == "" && conditions.IsTrue(ep.GetStatus().GetConditions(), PublicEndpointReady) &&
			ep.GetStatus().GetObservedGeneration() == 2
	})
	if n := len(apps(f)[host]); n != 0 {
		t.Errorf("%d applications left for a private endpoint", n)
	}

	// Public, then deleted: the application goes, then the endpoint.
	if _, err := b.Apply(user, endpoint("site", "api", 8080, public)); err != nil {
		t.Fatal(err)
	}
	waitEndpoint(t, s, system, "site", "public again", func(ep *networkv1alpha1.PublicEndpoint) bool {
		return ep.GetStatus().GetAccessApplicationId() != ""
	})
	if _, err := b.Delete(user, resource.Key{Project: "web", Name: "site"}, nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.PublicEndpoint, "site")
	if n := len(apps(f)[host]); n != 0 {
		t.Errorf("%d applications left after the endpoint went", n)
	}
}

// TestAnAccessApplicationMadeButNotRecordedIsFound: a reconcile that made
// the application and stopped before recording it left its intent; the
// next one adopts it rather than make another, and deletes it when it is
// no longer wanted. Reconciles run by hand, each as after a restart.
func TestAnAccessApplicationMadeButNotRecordedIsFound(t *testing.T) {
	f, cf := withCloudflare(t)
	s, system, user := runningWith(t, cf)
	b := s.backend(registry.PublicEndpoint)
	key := resource.Key{Project: "web", Name: "site"}
	const host = "site-web.hangarlab.dev"
	reconcile := func() {
		t.Helper()
		if err := s.reconcilePublicEndpoint(system, controller.Key{Project: "web", Name: "site"}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() *networkv1alpha1.PublicEndpoint {
		t.Helper()
		obj, err := s.get(system, registry.PublicEndpoint, key)
		if err != nil {
			t.Fatal(err)
		}
		if obj == nil {
			return nil
		}
		return obj.(*networkv1alpha1.PublicEndpoint)
	}
	if _, err := b.Apply(user, endpoint("site", "api", 8080, public)); err != nil {
		t.Fatal(err)
	}
	reconcile() // the intent
	if r := conditions.Find(read().GetStatus().GetConditions(), PublicEndpointAccessApplication).GetReason(); r != ReasonAccessCreating {
		t.Fatalf("after the first reconcile: %s", r)
	}
	if n := len(apps(f)); n != 0 {
		t.Fatalf("an application made before its intent was stored: %d", n)
	}
	// As if the reconcile had made it and stopped.
	made, err := s.cfg.PublicEndpoints.Cloudflare.CreateAccessApp(system, cfAccount, s.bypassApp(host))
	if err != nil {
		t.Fatal(err)
	}
	reconcile()
	if id := read().GetStatus().GetAccessApplicationId(); id != made.ID || len(apps(f)[host]) != 1 {
		t.Fatalf("recorded %q, made %q; applications %d", id, made.ID, len(apps(f)[host]))
	}

	// Recorded, then lost again with a crash: the status says Creating and
	// names none. Deleting the endpoint still finds it.
	ep := read()
	st := proto.Clone(ep.GetStatus()).(*networkv1alpha1.PublicEndpointStatus)
	st.AccessApplicationId = ""
	st.Conditions = conditions.Set(st.GetConditions(), &metav1.Condition{
		Type: PublicEndpointAccessApplication, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: ReasonAccessCreating,
	}, time.Now())
	if _, err := b.UpdateStatus(system, key, st, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Delete(user, key, nil); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if read() != nil || len(apps(f)[host]) != 0 {
		t.Errorf("after deleting: endpoint %v, applications %d", read(), len(apps(f)[host]))
	}
}

// TestAContainersPublicPortsGetEndpoints: each public port gets a private
// endpoint the container owns, which follows the port and goes with it; a
// name another endpoint holds is reported, and taken once it is free.
func TestAContainersPublicPortsGetEndpoints(t *testing.T) {
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
