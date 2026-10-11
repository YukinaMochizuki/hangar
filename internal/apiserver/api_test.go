package apiserver_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
)

const (
	alice = "alice@example.com"
	bob   = "bob@example.com"
)

func TestLifecycle(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	a := e.as(alice)

	created := must[tenancyv1alpha1.ApplyTenantResponse](t)(a.tenants.ApplyTenant(ctx, connect.NewRequest(tenant("acme"))))
	if !created.GetCreated() {
		t.Error("first apply of a tenant did not report created")
	}
	meta := created.GetTenant().GetMetadata()
	if meta.GetUid() == "" || meta.GetGeneration() != 1 || meta.GetResourceVersion() == 0 || meta.GetCreateTime() == nil {
		t.Errorf("system fields not set on create: %v", meta)
	}
	if got := created.GetTenant().GetApiVersion(); got != "tenancy.hangar.dev/v1alpha1" {
		t.Errorf("apiVersion %q", got)
	}
	must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, connect.NewRequest(project("web", "acme"))))

	c := container("web", "api", "nginx:1")
	first := must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(c)))
	got := must[computev1alpha1.GetContainerResponse](t)(a.containers.GetContainer(ctx, getContainer("web", "api")))
	if !proto.Equal(got.GetContainer(), first.GetContainer()) {
		t.Errorf("Get returned %v, Apply returned %v", got.GetContainer(), first.GetContainer())
	}

	// Applying the same object again changes nothing.
	again := must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(c)))
	if again.GetCreated() || again.GetContainer().GetMetadata().GetResourceVersion() != first.GetContainer().GetMetadata().GetResourceVersion() {
		t.Errorf("a no-op apply wrote: %v", again.GetContainer().GetMetadata())
	}

	// A label change is a write but not a new generation; a spec change is.
	c.Metadata.Labels = map[string]string{"tier": "front"}
	labelled := must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(c)))
	if labelled.GetContainer().GetMetadata().GetGeneration() != 1 {
		t.Errorf("a label change bumped the generation to %d", labelled.GetContainer().GetMetadata().GetGeneration())
	}
	c.Spec.Image = "nginx:2"
	changed := must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(c)))
	cm := changed.GetContainer().GetMetadata()
	if cm.GetGeneration() != 2 || cm.GetUid() != first.GetContainer().GetMetadata().GetUid() {
		t.Errorf("after a spec change: %v", cm)
	}

	list := must[computev1alpha1.ListContainersResponse](t)(a.containers.ListContainers(ctx, connect.NewRequest(&computev1alpha1.ListContainersRequest{Project: "web"})))
	if len(list.GetItems()) != 1 || list.GetRevision() < cm.GetResourceVersion() {
		t.Errorf("List returned %d items at revision %d", len(list.GetItems()), list.GetRevision())
	}
	selected := must[computev1alpha1.ListContainersResponse](t)(a.containers.ListContainers(ctx, connect.NewRequest(&computev1alpha1.ListContainersRequest{Project: "web", LabelSelector: "tier=back"})))
	if len(selected.GetItems()) != 0 {
		t.Errorf("label selector tier=back matched %d items", len(selected.GetItems()))
	}

	must[computev1alpha1.DeleteContainerResponse](t)(a.containers.DeleteContainer(ctx, connect.NewRequest(&computev1alpha1.DeleteContainerRequest{Project: "web", Name: "api"})))
	_, err := a.containers.GetContainer(ctx, getContainer("web", "api"))
	wantCode(t, err, connect.CodeNotFound, "Get after Delete")
}

func TestAuthorization(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a, b := e.as(alice), e.as(bob)
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "api", "nginx"))))

	_, err := e.as("").tenants.GetTenant(ctx, connect.NewRequest(&tenancyv1alpha1.GetTenantRequest{Name: "acme"}))
	wantCode(t, err, connect.CodeUnauthenticated, "a call without credentials")

	_, err = b.tenants.GetTenant(ctx, connect.NewRequest(&tenancyv1alpha1.GetTenantRequest{Name: "acme"}))
	wantCode(t, err, connect.CodePermissionDenied, "bob reading alice's tenant")
	_, err = b.containers.GetContainer(ctx, getContainer("web", "api"))
	wantCode(t, err, connect.CodePermissionDenied, "bob reading alice's container")
	_, err = b.containers.GetContainer(ctx, getContainer("web", "missing"))
	wantCode(t, err, connect.CodePermissionDenied, "bob probing for a container that does not exist")
	_, err = b.containers.ApplyContainer(ctx, applyContainer(container("web", "evil", "x")))
	wantCode(t, err, connect.CodePermissionDenied, "bob writing into alice's project")
	_, err = b.projects.ApplyProject(ctx, connect.NewRequest(project("intruder", "acme")))
	wantCode(t, err, connect.CodePermissionDenied, "bob creating a project in alice's tenant")
	_, err = b.tenants.ApplyTenant(ctx, connect.NewRequest(tenant("acme")))
	wantCode(t, err, connect.CodePermissionDenied, "bob overwriting alice's tenant")

	// Bob's own tenant is his; listing shows each user only their own.
	e.setupTenant(bob, "bobco", "bobweb")
	names := func(email string) []string {
		res := must[tenancyv1alpha1.ListTenantsResponse](t)(e.as(email).tenants.ListTenants(ctx, connect.NewRequest(&tenancyv1alpha1.ListTenantsRequest{})))
		var out []string
		for _, tn := range res.GetItems() {
			out = append(out, tn.GetMetadata().GetName())
		}
		return out
	}
	if got := names(bob); !slices.Equal(got, []string{"bobco"}) {
		t.Errorf("bob lists tenants %v", got)
	}
	if got := names(root); !slices.Equal(got, []string{"acme", "bobco"}) {
		t.Errorf("root lists tenants %v", got)
	}
	all := must[computev1alpha1.ListContainersResponse](t)(b.containers.ListContainers(ctx, connect.NewRequest(&computev1alpha1.ListContainersRequest{})))
	if len(all.GetItems()) != 0 {
		t.Errorf("bob listing containers across projects sees %d of alice's", len(all.GetItems()))
	}

	// Status belongs to the system: no user, not even root, may write it, and
	// the node may.
	status := &computev1alpha1.UpdateContainerStatusRequest{Project: "web", Name: "api", Status: &computev1alpha1.ContainerStatus{ObservedGeneration: 1}}
	_, err = a.containers.UpdateContainerStatus(ctx, connect.NewRequest(status))
	wantCode(t, err, connect.CodePermissionDenied, "the owner writing status")
	_, err = e.as(root).containers.UpdateContainerStatus(ctx, connect.NewRequest(status))
	wantCode(t, err, connect.CodePermissionDenied, "root writing status")
	updated := must[computev1alpha1.UpdateContainerStatusResponse](t)(e.asNode().containers.UpdateContainerStatus(ctx, connect.NewRequest(status)))
	if updated.GetContainer().GetStatus().GetObservedGeneration() != 1 {
		t.Errorf("node status write returned %v", updated.GetContainer().GetStatus())
	}
	_, err = e.asNode().containers.ApplyContainer(ctx, applyContainer(container("web", "api", "other")))
	wantCode(t, err, connect.CodePermissionDenied, "the node writing spec")

	// A sponsor sees no tenant's resources.
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(e.as(root).bindings.ApplyRoleBinding(ctx, binding("sponsor", "sponsor@example.com", "platform.sponsor", platformScope())))
	_, err = e.as("sponsor@example.com").containers.GetContainer(ctx, getContainer("web", "api"))
	wantCode(t, err, connect.CodePermissionDenied, "a sponsor reading a container")

	// Escalation: alice cannot grant herself anything at platform level, nor
	// bind a role at a scope it does not belong to.
	_, err = a.bindings.ApplyRoleBinding(ctx, binding("grab", alice, "platform.root", platformScope()))
	wantCode(t, err, connect.CodePermissionDenied, "alice binding platform.root")
	_, err = a.bindings.ApplyRoleBinding(ctx, binding("grab", alice, "platform.root", tenantScope("acme")))
	wantCode(t, err, connect.CodeInvalidArgument, "alice binding a platform role at tenant scope")
	_, err = a.bindings.ApplyRoleBinding(ctx, binding("grab", alice, "tenant.owner", tenantScope("bobco")))
	wantCode(t, err, connect.CodePermissionDenied, "alice making herself owner of bob's tenant")

	// Alice shares her tenant with bob; it takes effect on his very next call.
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("acme-bob", bob, "tenant.owner", tenantScope("acme"))))
	must[computev1alpha1.GetContainerResponse](t)(b.containers.GetContainer(ctx, getContainer("web", "api")))
}

func TestApplyRules(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)

	c := container("web", "api", "nginx")
	c.Metadata.Labels = map[string]string{"hangar.dev/managed": "yes"}
	_, err := a.containers.ApplyContainer(ctx, applyContainer(c))
	wantCode(t, err, connect.CodeInvalidArgument, "a user setting a reserved label")
	c.Metadata.Labels = map[string]string{"x.hangar.dev/managed": "yes"}
	_, err = a.containers.ApplyContainer(ctx, applyContainer(c))
	wantCode(t, err, connect.CodeInvalidArgument, "a user setting a label under a reserved subdomain")

	_, err = a.containers.ApplyContainer(ctx, applyContainer(container("web", "Bad_Name", "nginx")))
	wantCode(t, err, connect.CodeInvalidArgument, "an invalid name")
	_, err = a.containers.ApplyContainer(ctx, applyContainer(container("web", "noimage", "")))
	wantCode(t, err, connect.CodeInvalidArgument, "a container without an image")
	_, err = a.containers.ApplyContainer(ctx, applyContainer(container("", "api", "nginx")))
	wantCode(t, err, connect.CodeInvalidArgument, "a project-scoped object without a project")
	_, err = e.as(root).containers.ApplyContainer(ctx, applyContainer(container("ghost", "api", "nginx")))
	wantCode(t, err, connect.CodeFailedPrecondition, "a container in a project that does not exist")
	wrongKind := container("web", "api", "nginx")
	wrongKind.Kind = "VirtualMachine"
	_, err = a.containers.ApplyContainer(ctx, applyContainer(wrongKind))
	wantCode(t, err, connect.CodeInvalidArgument, "a container claiming another kind")

	// Fields the system owns are ignored on input.
	sneaky := container("web", "api", "nginx")
	sneaky.Metadata.Finalizers = []string{"user/finalizer"}
	sneaky.Metadata.Uid = "forged"
	sneaky.Status = &computev1alpha1.ContainerStatus{ObservedGeneration: 99}
	stored := must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(sneaky))).GetContainer()
	if len(stored.GetMetadata().GetFinalizers()) != 0 || stored.GetMetadata().GetUid() == "forged" || stored.GetStatus().GetObservedGeneration() != 0 {
		t.Errorf("system-owned fields taken from input: %v / %v", stored.GetMetadata(), stored.GetStatus())
	}

	// Optimistic concurrency.
	stale := container("web", "api", "nginx:2")
	stale.Metadata.ResourceVersion = stored.GetMetadata().GetResourceVersion() - 1
	_, err = a.containers.ApplyContainer(ctx, applyContainer(stale))
	wantCode(t, err, connect.CodeAborted, "an apply with a stale resource version")
	fresh := container("web", "api", "nginx:2")
	fresh.Metadata.ResourceVersion = stored.GetMetadata().GetResourceVersion()
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(fresh)))
	rv := stored.GetMetadata().GetResourceVersion()
	_, err = a.containers.DeleteContainer(ctx, connect.NewRequest(&computev1alpha1.DeleteContainerRequest{Project: "web", Name: "api", ExpectedResourceVersion: &rv}))
	wantCode(t, err, connect.CodeAborted, "a delete with a stale resource version")

	// Immutable fields.
	must[tenancyv1alpha1.ApplyTenantResponse](t)(a.tenants.ApplyTenant(ctx, connect.NewRequest(tenant("acme2"))))
	_, err = a.projects.ApplyProject(ctx, connect.NewRequest(project("web", "acme2")))
	wantCode(t, err, connect.CodeInvalidArgument, "moving a project to another tenant")
}

func TestWatch(t *testing.T) {
	e := newEnv(t, func(c *apiserver.Config) { c.BookmarkInterval = 30 * time.Second })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)

	list := must[computev1alpha1.ListContainersResponse](t)(a.containers.ListContainers(ctx, connect.NewRequest(&computev1alpha1.ListContainersRequest{Project: "web"})))
	stream, err := a.containers.WatchContainers(ctx, connect.NewRequest(&computev1alpha1.WatchContainersRequest{Project: "web", SinceRevision: list.GetRevision()}))
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan *computev1alpha1.WatchContainersResponse, 16)
	go func() {
		defer close(events)
		for stream.Receive() {
			events <- stream.Msg()
		}
	}()
	next := func() *computev1alpha1.WatchContainersResponse {
		t.Helper()
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("watch ended: %v", stream.Err())
			}
			return ev
		case <-time.After(10 * time.Second):
			t.Fatal("no watch event")
		}
		return nil
	}

	if ev := next(); ev.GetType() != metav1.EventType_EVENT_TYPE_BOOKMARK || ev.GetRevision() != list.GetRevision() {
		t.Fatalf("want an opening bookmark at %d, got %v at %d", list.GetRevision(), ev.GetType(), ev.GetRevision())
	}
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "api", "nginx"))))
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "api", "nginx:2"))))
	must[computev1alpha1.DeleteContainerResponse](t)(a.containers.DeleteContainer(ctx, connect.NewRequest(&computev1alpha1.DeleteContainerRequest{Project: "web", Name: "api"})))
	for _, want := range []metav1.EventType{metav1.EventType_EVENT_TYPE_ADDED, metav1.EventType_EVENT_TYPE_MODIFIED, metav1.EventType_EVENT_TYPE_DELETED} {
		ev := next()
		if ev.GetType() != want || ev.GetContainer().GetMetadata().GetName() != "api" {
			t.Fatalf("got %v %v, want %v", ev.GetType(), ev.GetContainer().GetMetadata().GetName(), want)
		}
	}
	last, err := e.store.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// An idle watch sends a bookmark on the clock, at the revision it has
	// reached. Three tickers run: compaction, the garbage collection sweep,
	// and this watch.
	if err := e.clock.BlockUntilContext(ctx, 3); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(30 * time.Second)
	if ev := next(); ev.GetType() != metav1.EventType_EVENT_TYPE_BOOKMARK || ev.GetRevision() != last || ev.GetContainer() != nil {
		t.Errorf("want a bookmark at %d, got %v at %d", last, ev.GetType(), ev.GetRevision())
	}
}

func TestWatchReportsItsDeadline(t *testing.T) {
	e := newEnv(t)
	e.setupTenant(alice, "acme", "web")
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	stream, err := e.as(alice).containers.WatchContainers(ctx, connect.NewRequest(&computev1alpha1.WatchContainersRequest{Project: "web"}))
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
	}
	wantCode(t, stream.Err(), connect.CodeDeadlineExceeded, "a watch that ran out of time")
}

func TestWatchFromCompactedRevision(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	rev, err := e.store.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.Compact(ctx, rev); err != nil {
		t.Fatal(err)
	}
	stream, err := e.as(alice).containers.WatchContainers(ctx, connect.NewRequest(&computev1alpha1.WatchContainersRequest{Project: "web", SinceRevision: 1}))
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
		if stream.Msg().GetType() != metav1.EventType_EVENT_TYPE_BOOKMARK {
			t.Fatalf("unexpected event %v", stream.Msg())
		}
	}
	wantCode(t, stream.Err(), connect.CodeOutOfRange, "watching from a compacted revision")
}

func TestTenantDeletionCollectsItsBindings(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	must[tenancyv1alpha1.ApplyTenantResponse](t)(e.as(alice).tenants.ApplyTenant(ctx, connect.NewRequest(tenant("acme"))))
	owners := func() int {
		res := must[iamv1alpha1.ListRoleBindingsResponse](t)(e.as(root).bindings.ListRoleBindings(ctx, connect.NewRequest(&iamv1alpha1.ListRoleBindingsRequest{})))
		n := 0
		for _, rb := range res.GetItems() {
			if rb.GetSpec().GetRole() == "tenant.owner" {
				n++
			}
		}
		return n
	}
	if owners() != 1 {
		t.Fatalf("creating a tenant made %d owner bindings", owners())
	}
	must[tenancyv1alpha1.DeleteTenantResponse](t)(e.as(alice).tenants.DeleteTenant(ctx, connect.NewRequest(&tenancyv1alpha1.DeleteTenantRequest{Name: "acme"})))
	deadline := time.Now().Add(10 * time.Second)
	for owners() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the owner binding outlived its tenant")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBootstrapIsOnce(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	// A second server over the same store must not add another root.
	srv := apiserver.New(apiserver.Config{Store: e.store, Clock: e.clock, BootstrapAdmin: "someone-else@example.com"})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.Run(runCtx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	res := must[iamv1alpha1.ListRoleBindingsResponse](t)(e.as(root).bindings.ListRoleBindings(ctx, connect.NewRequest(&iamv1alpha1.ListRoleBindingsRequest{})))
	roots := 0
	for _, rb := range res.GetItems() {
		if rb.GetSpec().GetRole() == "platform.root" {
			roots++
		}
	}
	if roots != 1 {
		t.Errorf("%d root bindings after a second bootstrap, want 1", roots)
	}
}

func binding(name, user, role string, scope *iamv1alpha1.BindingScope) *connect.Request[iamv1alpha1.ApplyRoleBindingRequest] {
	return connect.NewRequest(&iamv1alpha1.ApplyRoleBindingRequest{RoleBinding: &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: user}},
			Role:    role,
			Scope:   scope,
		},
	}})
}

func platformScope() *iamv1alpha1.BindingScope {
	return &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}}
}

func tenantScope(t string) *iamv1alpha1.BindingScope {
	return &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: t}}
}
