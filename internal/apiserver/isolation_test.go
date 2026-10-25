package apiserver_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

func TestDeletingATenantCascadesAndItsNameIsNeverReused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a, b, r := e.as(alice), e.as(bob), e.as(root)
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "api", "nginx"))))
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("keep", alice, "tenant.owner", tenantScope("acme"))))

	deleted := must[tenancyv1alpha1.DeleteTenantResponse](t)(a.tenants.DeleteTenant(ctx, connect.NewRequest(&tenancyv1alpha1.DeleteTenantRequest{Name: "acme"})))
	if deleted.GetTenant().GetMetadata().GetDeleteTime() == nil {
		t.Error("a tenant with contents was not marked as being deleted")
	}
	gone := func(err error) bool { return connect.CodeOf(err) == connect.CodeNotFound }
	testutil.Eventually(t, "the container is deleted", func() bool {
		_, err := r.containers.GetContainer(ctx, getContainer("web", "api"))
		return gone(err)
	})
	testutil.Eventually(t, "the project is deleted", func() bool {
		_, err := r.projects.GetProject(ctx, connect.NewRequest(&tenancyv1alpha1.GetProjectRequest{Name: "web"}))
		return gone(err)
	})
	testutil.Eventually(t, "the tenant is deleted", func() bool {
		_, err := r.tenants.GetTenant(ctx, connect.NewRequest(&tenancyv1alpha1.GetTenantRequest{Name: "acme"}))
		return gone(err)
	})
	testutil.Eventually(t, "the tenant's bindings are deleted", func() bool {
		res := must[iamv1alpha1.ListRoleBindingsResponse](t)(r.bindings.ListRoleBindings(ctx, connect.NewRequest(&iamv1alpha1.ListRoleBindingsRequest{})))
		for _, rb := range res.GetItems() {
			if rb.GetSpec().GetScope().GetTenant() == "acme" {
				return false
			}
		}
		return true
	})

	// Neither name can come to mean something else.
	_, err := b.tenants.ApplyTenant(ctx, connect.NewRequest(tenant("acme")))
	wantCode(t, err, connect.CodeFailedPrecondition, "reusing a deleted tenant's name")
	must[tenancyv1alpha1.ApplyTenantResponse](t)(b.tenants.ApplyTenant(ctx, connect.NewRequest(tenant("bobco"))))
	_, err = b.projects.ApplyProject(ctx, connect.NewRequest(project("web", "bobco")))
	wantCode(t, err, connect.CodeFailedPrecondition, "reusing a deleted project's name")
}

func TestNoExistenceLeaks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a, b := e.as(alice), e.as(bob)
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "api", "nginx"))))
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(e.as(root).bindings.ApplyRoleBinding(ctx, binding("ops", "ops@example.com", "platform.sponsor", platformScope())))

	for _, name := range []string{"api", "missing"} {
		_, err := b.containers.DeleteContainer(ctx, connect.NewRequest(&computev1alpha1.DeleteContainerRequest{Project: "web", Name: name}))
		wantCode(t, err, connect.CodePermissionDenied, "bob deleting container "+name)
		stale := container("web", name, "x")
		stale.Metadata.ResourceVersion = 1
		_, err = b.containers.ApplyContainer(ctx, applyContainer(stale))
		wantCode(t, err, connect.CodePermissionDenied, "bob applying container "+name+" at a resource version")
	}
	for _, name := range []string{"ops", "missing"} {
		_, err := b.bindings.GetRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.GetRoleBindingRequest{Name: name}))
		wantCode(t, err, connect.CodePermissionDenied, "bob reading binding "+name)
		_, err = b.bindings.DeleteRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: name}))
		wantCode(t, err, connect.CodePermissionDenied, "bob deleting binding "+name)
	}
	must[computev1alpha1.GetContainerResponse](t)(a.containers.GetContainer(ctx, getContainer("web", "api")))
}

// collect reads a watch in the background.
func collect[T any](t *testing.T, stream interface {
	Receive() bool
	Msg() *T
	Err() error
}) <-chan *T {
	ch := make(chan *T, 64)
	go func() {
		defer close(ch)
		for stream.Receive() {
			ch <- stream.Msg()
		}
	}()
	return ch
}

func TestWatchAuthorization(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.setupTenant(alice, "acme", "web")
	a, b := e.as(alice), e.as(bob)

	stream, err := b.containers.WatchContainers(ctx, connect.NewRequest(&computev1alpha1.WatchContainersRequest{Project: "web"}))
	if err == nil {
		for stream.Receive() {
		}
		err = stream.Err()
	}
	wantCode(t, err, connect.CodePermissionDenied, "bob watching alice's project")

	// Watching every project shows bob nothing of alice's.
	all, err := b.containers.WatchContainers(ctx, connect.NewRequest(&computev1alpha1.WatchContainersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, all)
	<-events // opening bookmark
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "api", "nginx"))))
	select {
	case ev := <-events:
		t.Errorf("bob saw an event from alice's project: %v", ev)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestOwnerSeesEveryEventOfOwnProjects(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	stream, err := a.projects.WatchProjects(ctx, connect.NewRequest(&tenancyv1alpha1.WatchProjectsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, stream)
	<-events
	for i := range 5 {
		name := "web-" + string(rune('a'+i))
		must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, connect.NewRequest(project(name, "acme"))))
		must[tenancyv1alpha1.DeleteProjectResponse](t)(a.projects.DeleteProject(ctx, connect.NewRequest(&tenancyv1alpha1.DeleteProjectRequest{Name: name})))
		var sawAdded, sawDeleted bool
		timeout := time.After(10 * time.Second)
		for !sawDeleted {
			select {
			case ev, ok := <-events:
				if !ok {
					t.Fatalf("watch ended: %v", stream.Err())
				}
				if ev.GetProject().GetMetadata().GetName() != name {
					continue
				}
				switch ev.GetType() {
				case metav1.EventType_EVENT_TYPE_ADDED:
					sawAdded = true
				case metav1.EventType_EVENT_TYPE_DELETED:
					sawDeleted = true
				}
			case <-timeout:
				t.Fatalf("round %d: the owner never saw %s deleted (added: %t)", i, name, sawAdded)
			}
		}
		if !sawAdded {
			t.Errorf("round %d: the owner never saw %s added", i, name)
		}
	}
}

func TestMoreDenials(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a, b := e.as(alice), e.as(bob)
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "api", "nginx"))))

	_, err := b.containers.DeleteContainer(ctx, connect.NewRequest(&computev1alpha1.DeleteContainerRequest{Project: "web", Name: "api"}))
	wantCode(t, err, connect.CodePermissionDenied, "bob deleting alice's container")
	must[computev1alpha1.GetContainerResponse](t)(a.containers.GetContainer(ctx, getContainer("web", "api")))

	// A binding's scope cannot move, so it cannot be carried elsewhere.
	must[tenancyv1alpha1.ApplyTenantResponse](t)(a.tenants.ApplyTenant(ctx, connect.NewRequest(tenant("acme2"))))
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("share", bob, "tenant.owner", tenantScope("acme"))))
	_, err = a.bindings.ApplyRoleBinding(ctx, binding("share", bob, "tenant.owner", tenantScope("acme2")))
	wantCode(t, err, connect.CodeInvalidArgument, "moving a binding to another tenant")

	// Quota is the platform's to give.
	planned := tenant("acme")
	planned.Tenant.Spec.Plan = "big"
	_, err = a.tenants.ApplyTenant(ctx, connect.NewRequest(planned))
	wantCode(t, err, connect.CodePermissionDenied, "a tenant owner choosing their plan")
	selfPlanned := tenant("planned")
	selfPlanned.Tenant.Spec.Plan = "big"
	_, err = b.tenants.ApplyTenant(ctx, connect.NewRequest(selfPlanned))
	wantCode(t, err, connect.CodePermissionDenied, "a new tenant choosing its plan")
	_, err = e.as(root).tenants.ApplyTenant(ctx, connect.NewRequest(planned))
	wantCode(t, err, connect.CodeFailedPrecondition, "a plan that does not exist")
	must[tenancyv1alpha1.ApplyPlanResponse](t)(e.as(root).plans.ApplyPlan(ctx, connect.NewRequest(&tenancyv1alpha1.ApplyPlanRequest{
		Plan: &tenancyv1alpha1.Plan{Metadata: &metav1.ObjectMeta{Name: "big"}},
	})))
	must[tenancyv1alpha1.ApplyTenantResponse](t)(e.as(root).tenants.ApplyTenant(ctx, connect.NewRequest(planned)))

	_, err = e.asNode().containers.UpdateContainerStatus(ctx, connect.NewRequest(&computev1alpha1.UpdateContainerStatusRequest{Project: "web", Name: "api"}))
	wantCode(t, err, connect.CodeInvalidArgument, "a status write without a status")

	// The node may read everything but delete nothing.
	must[computev1alpha1.GetContainerResponse](t)(e.asNode().containers.GetContainer(ctx, getContainer("web", "api")))
	_, err = e.asNode().containers.DeleteContainer(ctx, connect.NewRequest(&computev1alpha1.DeleteContainerRequest{Project: "web", Name: "api"}))
	wantCode(t, err, connect.CodePermissionDenied, "the node deleting a container")
}

// TestWatchSeesObjectsInNewProjects creates a project and at once a
// container in it, while watching every project: the container's event must
// be judged with the new project already known, or it is silently dropped.
func TestWatchSeesObjectsInNewProjects(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	stream, err := a.containers.WatchContainers(ctx, connect.NewRequest(&computev1alpha1.WatchContainersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, stream)
	<-events
	for i := range 30 {
		name := fmt.Sprintf("p%d", i)
		must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, connect.NewRequest(project(name, "acme"))))
		must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container(name, "api", "nginx"))))
	}
	seen := map[string]bool{}
	timeout := time.After(10 * time.Second)
	for len(seen) < 30 {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("watch ended: %v", stream.Err())
			}
			if ev.GetType() == metav1.EventType_EVENT_TYPE_ADDED {
				seen[ev.GetContainer().GetMetadata().GetProject()] = true
			}
		case <-timeout:
			t.Fatalf("saw containers created in %d of 30 new projects", len(seen))
		}
	}
}

// TestWatchSeesTheGrantThatAdmitsIt checks that an event which itself grants
// the watcher access is judged with that grant in effect.
func TestWatchSeesTheGrantThatAdmitsIt(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a := e.as(alice)
	stream, err := e.as(bob).bindings.WatchRoleBindings(ctx, connect.NewRequest(&iamv1alpha1.WatchRoleBindingsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, stream)
	<-events
	const rounds = 20
	for i := range rounds {
		name := fmt.Sprintf("t%d", i)
		must[tenancyv1alpha1.ApplyTenantResponse](t)(a.tenants.ApplyTenant(ctx, connect.NewRequest(tenant(name))))
		must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding(name+"-bob", bob, "tenant.owner", tenantScope(name))))
	}
	seen := map[string]bool{}
	timeout := time.After(10 * time.Second)
	for len(seen) < rounds {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("watch ended: %v", stream.Err())
			}
			if n := ev.GetRoleBinding().GetMetadata().GetName(); ev.GetType() == metav1.EventType_EVENT_TYPE_ADDED && strings.HasSuffix(n, "-bob") {
				seen[n] = true
			}
		case <-timeout:
			t.Fatalf("bob saw %d of the %d bindings that granted him access", len(seen), rounds)
		}
	}
}

func TestDenialsReadTheSameWhetherOrNotTheObjectExists(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	b := e.as(bob)
	message := func(err error, name string) string {
		t.Helper()
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("want permission_denied, got %v", err)
		}
		var ce *connect.Error
		if !errors.As(err, &ce) {
			t.Fatalf("not a connect error: %v", err)
		}
		return strings.ReplaceAll(ce.Message(), name, "NAME")
	}
	owner := ""
	res := must[iamv1alpha1.ListRoleBindingsResponse](t)(e.as(root).bindings.ListRoleBindings(ctx, connect.NewRequest(&iamv1alpha1.ListRoleBindingsRequest{})))
	for _, rb := range res.GetItems() {
		if rb.GetSpec().GetScope().GetTenant() == "acme" {
			owner = rb.GetMetadata().GetName()
		}
	}
	for _, existing := range []string{owner, "bootstrap-root"} {
		_, errExisting := b.bindings.GetRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.GetRoleBindingRequest{Name: existing}))
		_, errMissing := b.bindings.GetRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.GetRoleBindingRequest{Name: "no-such-binding"}))
		if a, m := message(errExisting, existing), message(errMissing, "no-such-binding"); a != m {
			t.Errorf("denials differ: %q for %s, %q for a missing one", a, existing, m)
		}
		_, errDelete := b.bindings.DeleteRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: existing}))
		if msg := message(errDelete, existing); strings.Contains(msg, "acme") || strings.Contains(msg, "platform") {
			t.Errorf("a denial reveals where the binding lives: %q", msg)
		}
	}
}

// TestOwnerSeesTheirTenantCascadeToTheEnd deletes a tenant while its owner
// watches: every deletion is visible to the owner, contents before the tenant.
func TestOwnerSeesTheirTenantCascadeToTheEnd(t *testing.T) {
	t.Parallel()
	testutil.Quarantine(t, "#210")
	e := newEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "api", "nginx"))))

	cs, err := a.containers.WatchContainers(ctx, connect.NewRequest(&computev1alpha1.WatchContainersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	ps, err := a.projects.WatchProjects(ctx, connect.NewRequest(&tenancyv1alpha1.WatchProjectsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	ts, err := a.tenants.WatchTenants(ctx, connect.NewRequest(&tenancyv1alpha1.WatchTenantsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	cEvents, pEvents, tEvents := collect(t, cs), collect(t, ps), collect(t, ts)
	<-cEvents
	<-pEvents
	<-tEvents
	must[tenancyv1alpha1.DeleteTenantResponse](t)(a.tenants.DeleteTenant(ctx, connect.NewRequest(&tenancyv1alpha1.DeleteTenantRequest{Name: "acme"})))

	deletedAt := func(name string, next func() (metav1.EventType, int64, bool)) int64 {
		t.Helper()
		for {
			typ, rev, ok := next()
			if !ok {
				t.Fatalf("the owner never saw %s deleted", name)
			}
			if typ == metav1.EventType_EVENT_TYPE_DELETED {
				return rev
			}
		}
	}
	timeout := time.After(10 * time.Second)
	from := func(ch <-chan *computev1alpha1.WatchContainersResponse) func() (metav1.EventType, int64, bool) {
		return func() (metav1.EventType, int64, bool) {
			select {
			case ev, ok := <-ch:
				return ev.GetType(), ev.GetRevision(), ok
			case <-timeout:
				return 0, 0, false
			}
		}
	}
	containerRev := deletedAt("the container", from(cEvents))
	project := deletedAt("the project", func() (metav1.EventType, int64, bool) {
		select {
		case ev, ok := <-pEvents:
			return ev.GetType(), ev.GetRevision(), ok
		case <-timeout:
			return 0, 0, false
		}
	})
	tenantRev := deletedAt("the tenant", func() (metav1.EventType, int64, bool) {
		select {
		case ev, ok := <-tEvents:
			return ev.GetType(), ev.GetRevision(), ok
		case <-timeout:
			return 0, 0, false
		}
	})
	if containerRev >= project || project >= tenantRev {
		t.Errorf("deleted out of order: container %d, project %d, tenant %d", containerRev, project, tenantRev)
	}
}

func TestPlatformMembership(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	const carol = "carol@example.com"
	c := e.as(carol)

	// Signed in but never added: nothing works, not even a grant inside a
	// tenant, and the error says why.
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(e.as(alice).bindings.ApplyRoleBinding(ctx, binding("acme-carol", carol, "tenant.owner", tenantScope("acme"))))
	_, err := c.tenants.GetTenant(ctx, connect.NewRequest(&tenancyv1alpha1.GetTenantRequest{Name: "acme"}))
	wantCode(t, err, connect.CodePermissionDenied, "a non-member using a tenant grant")
	if err != nil && !strings.Contains(err.Error(), "not a platform member") {
		t.Errorf("the denial does not say why: %v", err)
	}
	_, err = c.tenants.ApplyTenant(ctx, connect.NewRequest(tenant("carolco")))
	wantCode(t, err, connect.CodePermissionDenied, "a non-member opening a tenant")
	_, err = c.tenants.ListTenants(ctx, connect.NewRequest(&tenancyv1alpha1.ListTenantsRequest{}))
	wantCode(t, err, connect.CodePermissionDenied, "a non-member listing")

	// Members cannot make others members; that is the platform's call.
	_, err = e.as(alice).bindings.ApplyRoleBinding(ctx, binding("member-carol", carol, "platform.member", platformScope()))
	wantCode(t, err, connect.CodePermissionDenied, "a member adding a member")

	// A binding made before the person ever signs in works on their first
	// call.
	e.addMember(carol)
	must[tenancyv1alpha1.GetTenantResponse](t)(c.tenants.GetTenant(ctx, connect.NewRequest(&tenancyv1alpha1.GetTenantRequest{Name: "acme"})))
	must[tenancyv1alpha1.ApplyTenantResponse](t)(c.tenants.ApplyTenant(ctx, connect.NewRequest(tenant("carolco"))))

	// Sponsors are members too, once signed up.
	const dave = "dave@example.com"
	e.invite(dave, "platform.sponsor")
	e.signUp(dave)
	must[tenancyv1alpha1.ApplyTenantResponse](t)(e.as(dave).tenants.ApplyTenant(ctx, connect.NewRequest(tenant("daveco"))))
}

// TestASponsorReadsThePlatformAndNoTenant: a sponsor reads the Nodes and
// what is placed on them, counted, and nothing of any tenant's: no tenant,
// project or machine is listed or read. WhoAmI names the sponsor's
// platform role, and a member's.
func TestASponsorReadsThePlatformAndNoTenant(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	must[computev1alpha1.ApplyContainerResponse](t)(e.as(alice).containers.ApplyContainer(ctx, applyContainer(container("web", "api", "nginx"))))
	const sam = "sam@example.com"
	e.invite(sam, "platform.sponsor")
	e.signUp(sam)
	s := e.as(sam)

	nodes := must[computev1alpha1.ListNodesResponse](t)(s.nodes.ListNodes(ctx, connect.NewRequest(&computev1alpha1.ListNodesRequest{})))
	if len(nodes.GetItems()) != 1 || nodes.GetItems()[0].GetMetadata().GetName() != testNode {
		t.Errorf("a sponsor lists nodes %v", nodes.GetItems())
	}
	must[computev1alpha1.GetNodeResponse](t)(s.nodes.GetNode(ctx, connect.NewRequest(&computev1alpha1.GetNodeRequest{Name: testNode})))
	_, err := s.nodes.ApplyNode(ctx, connect.NewRequest(&computev1alpha1.ApplyNodeRequest{Node: nodes.GetItems()[0]}))
	wantCode(t, err, connect.CodePermissionDenied, "a sponsor applying a node")

	// Of the tenants, a sponsor sees only the one signing up opened for them.
	for _, tn := range must[tenancyv1alpha1.ListTenantsResponse](t)(s.tenants.ListTenants(ctx, connect.NewRequest(&tenancyv1alpha1.ListTenantsRequest{}))).GetItems() {
		if tn.GetMetadata().GetName() == "acme" {
			t.Error("a sponsor lists another's tenant")
		}
	}
	for _, p := range must[tenancyv1alpha1.ListProjectsResponse](t)(s.projects.ListProjects(ctx, connect.NewRequest(&tenancyv1alpha1.ListProjectsRequest{}))).GetItems() {
		if p.GetMetadata().GetName() == "web" {
			t.Error("a sponsor lists another's project")
		}
	}
	_, err = s.tenants.GetTenant(ctx, connect.NewRequest(&tenancyv1alpha1.GetTenantRequest{Name: "acme"}))
	wantCode(t, err, connect.CodePermissionDenied, "a sponsor reading another's tenant")
	_, err = s.machines.ListMachines(ctx, connect.NewRequest(&computev1alpha1.ListMachinesRequest{Project: "web"}))
	wantCode(t, err, connect.CodePermissionDenied, "a sponsor listing another's machines")
	// Across every project, a list answers only the projects the caller
	// reaches, which for a sponsor holds none of acme's.
	for _, m := range must[computev1alpha1.ListMachinesResponse](t)(s.machines.ListMachines(ctx, connect.NewRequest(&computev1alpha1.ListMachinesRequest{}))).GetItems() {
		if m.GetMetadata().GetProject() == "web" {
			t.Error("a sponsor lists another's machine across projects")
		}
	}

	for email, want := range map[string][]string{sam: {"platform.sponsor"}, alice: {"platform.member"}, root: {"platform.root"}} {
		me := must[iamv1alpha1.WhoAmIResponse](t)(e.as(email).accounts.WhoAmI(ctx, connect.NewRequest(&iamv1alpha1.WhoAmIRequest{})))
		if got := me.GetPlatformRoles(); !slices.Equal(got, want) {
			t.Errorf("%s's platform roles are %v, want %v", email, got, want)
		}
	}
}

func TestBootstrapOnlyOnAnEmptyStore(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	r := e.as(root)
	must[iamv1alpha1.DeleteRoleBindingResponse](t)(r.bindings.DeleteRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: "bootstrap-root"})))

	srv := apiserver.New(apiserver.Config{Store: e.store, Clock: e.clock, BootstrapAdmin: root})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.Run(runCtx) }()
	// Bootstrap runs once at start and reports nothing when it does nothing.
	testutil.Unchanged(t, "the revoked bootstrap binding, the server restarting", 200*time.Millisecond, e.storedVersion(bootstrapBinding))
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	res := must[iamv1alpha1.ListRoleBindingsResponse](t)(e.asNode().bindings.ListRoleBindings(ctx, connect.NewRequest(&iamv1alpha1.ListRoleBindingsRequest{})))
	for _, rb := range res.GetItems() {
		if rb.GetSpec().GetRole() == "platform.root" {
			t.Errorf("a restart re-granted root after it was revoked: %s", rb.GetMetadata().GetName())
		}
	}
}
