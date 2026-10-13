package apiserver

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
)

// running starts a server without controllers and returns it with contexts
// acting as the system and as a user who owns tenant "acme" with project
// "web".
func running(t *testing.T) (s *Server, system, user context.Context) {
	t.Helper()
	return runningWith(t, func(*Config) {})
}

// runningWith is running with a config adjusted by configure.
func runningWith(t *testing.T, configure func(*Config)) (s *Server, system, user context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hangar.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Store: st, Clock: clockwork.NewFakeClock(), DisableControllers: true}
	configure(&cfg)
	s = New(cfg)
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = st.Close()
	})
	system = auth.WithPrincipal(ctx, auth.SystemPrincipal)
	user = auth.WithPrincipal(ctx, auth.Principal{Type: auth.User, ID: "alice@example.com"})
	if _, err := s.backend(registry.RoleBinding).Apply(system, &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: "member-alice"},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: "alice@example.com"}},
			Role:    "platform.member",
			Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Tenant).Apply(user, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "acme"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Project).Apply(user, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "web"}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "acme"},
	}); err != nil {
		t.Fatal(err)
	}
	// What the project controller does, for tests that run without it.
	if cfg.DisableControllers {
		if _, err := s.backend(registry.Network).Apply(system, &networkv1alpha1.Network{
			Metadata: &metav1.ObjectMeta{Project: "web", Name: DefaultNetwork},
			Spec:     &networkv1alpha1.NetworkSpec{Ipv4Cidr: DefaultNetworkCIDR},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return s, system, user
}

func ctr(name string) *computev1alpha1.Container {
	return &computev1alpha1.Container{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
		Spec:     &computev1alpha1.ContainerSpec{Image: "nginx"},
	}
}

func TestFailedSetupRollsBackAndReleasesTheName(t *testing.T) {
	s, _, user := running(t)
	b := s.backend(registry.Tenant)
	setup := b.hooks.afterCreate
	b.hooks.afterCreate = func(context.Context, *Server, auth.Principal, proto.Message) error {
		return errors.New("injected")
	}
	if _, err := b.Apply(user, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "flaky"}}); err == nil {
		t.Fatal("a create whose setup failed succeeded")
	}
	if _, err := s.store.Get(user, store.Key{Kind: "Tenant", Name: "flaky"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the half-created tenant was left behind: %v", err)
	}
	b.hooks.afterCreate = setup
	if _, err := b.Apply(user, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "flaky"}}); err != nil {
		t.Errorf("the name was not released after the rollback: %v", err)
	}
}

func TestFinalizers(t *testing.T) {
	s, system, user := running(t)
	b := s.backend(registry.Container)
	key := resource.Key{Project: "web", Name: "api"}

	c := ctr("api")
	c.Metadata.Finalizers = []string{"hangarlab.cloud/machine"}
	if _, err := b.Apply(system, c); err != nil {
		t.Fatal(err)
	}

	got, err := b.Delete(user, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.(*computev1alpha1.Container).GetMetadata().GetDeleteTime() == nil {
		t.Fatal("deleting an object with a finalizer did not mark it")
	}
	if _, err := b.Get(user, key); err != nil {
		t.Fatalf("an object with a finalizer disappeared on delete: %v", err)
	}
	_, err = b.Apply(user, ctr("api"))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("applying an object being deleted: %v", err)
	}

	// Containers also carry the cascade's finalizer, which garbage collection
	// removes once nothing they own is left.
	if err := s.RemoveFinalizer(system, registry.Container, key, "hangarlab.cloud/machine"); err != nil {
		t.Fatal(err)
	}
	eventuallyGone(t, b, user, key)
}

// eventuallyGone waits for an object to be deleted, as happens in the
// background once its finalizers are done.
func eventuallyGone(t *testing.T, b *backend, ctx context.Context, key resource.Key) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := b.Get(ctx, key)
		if connect.CodeOf(err) == connect.CodeNotFound {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s %s still there: %v", b.r.Kind, key.Name, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReservedLabelsSurviveUserApply(t *testing.T) {
	s, system, user := running(t)
	b := s.backend(registry.Container)

	c := ctr("api")
	c.Metadata.Labels = map[string]string{"hangarlab.cloud/machine": "m-1", "app": "web"}
	if _, err := b.Apply(system, c); err != nil {
		t.Fatal(err)
	}
	mine := ctr("api")
	mine.Metadata.Labels = map[string]string{"app": "api"}
	res, err := b.Apply(user, mine)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"hangarlab.cloud/machine": "m-1", "app": "api"}
	got := res.Object.(*computev1alpha1.Container).GetMetadata().GetLabels()
	if len(got) != len(want) || got["hangarlab.cloud/machine"] != "m-1" || got["app"] != "api" {
		t.Errorf("labels after a user apply: %v, want %v", got, want)
	}
}

func TestReservedAnnotationsSurviveAndEchoBack(t *testing.T) {
	s, system, user := running(t)
	b := s.backend(registry.Container)
	c := ctr("api")
	c.Metadata.Annotations = map[string]string{"hangarlab.cloud/machine": "m-1"}
	if _, err := b.Apply(system, c); err != nil {
		t.Fatal(err)
	}
	// Applying without the annotation keeps it.
	res, err := b.Apply(user, ctr("api"))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Object.(*computev1alpha1.Container).GetMetadata().GetAnnotations()["hangarlab.cloud/machine"]; got != "m-1" {
		t.Errorf("a user apply dropped a system annotation: %q", got)
	}
	// Sending back what was read is fine; changing it is not.
	read, err := b.Get(user, resource.Key{Project: "web", Name: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply(user, read); err != nil {
		t.Errorf("applying an object as read: %v", err)
	}
	forged := proto.Clone(read).(*computev1alpha1.Container)
	forged.Metadata.Annotations["hangarlab.cloud/machine"] = "m-2"
	if _, err := b.Apply(user, forged); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("changing a system annotation: %v", err)
	}
}

func TestSweepCollectsOrphans(t *testing.T) {
	s, system, user := running(t)
	b := s.backend(registry.Container)

	orphan := ctr("orphan")
	orphan.Metadata.OwnerReferences = []*metav1.OwnerReference{{
		ApiVersion: registry.Container.APIVersion(), Kind: "Container", Name: "gone", Uid: "no-such-uid",
	}}
	if _, err := b.Apply(system, orphan); err != nil {
		t.Fatal(err)
	}
	// Same name as a live owner, but the uid of an earlier object by that
	// name: still an orphan.
	impostor := ctr("impostor")
	impostor.Metadata.OwnerReferences = []*metav1.OwnerReference{{
		ApiVersion: registry.Container.APIVersion(), Kind: "Container", Name: "owner", Uid: "earlier-uid",
	}}
	if _, err := b.Apply(system, impostor); err != nil {
		t.Fatal(err)
	}
	owned := ctr("owned")
	if _, err := b.Apply(system, ctr("owner")); err != nil {
		t.Fatal(err)
	}
	owner, err := b.Get(user, resource.Key{Project: "web", Name: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	owned.Metadata.OwnerReferences = []*metav1.OwnerReference{{
		ApiVersion: registry.Container.APIVersion(), Kind: "Container", Name: "owner",
		Uid: owner.(*computev1alpha1.Container).GetMetadata().GetUid(),
	}}
	if _, err := b.Apply(system, owned); err != nil {
		t.Fatal(err)
	}

	s.sweep(system)
	eventuallyGone(t, b, user, resource.Key{Project: "web", Name: "orphan"})
	eventuallyGone(t, b, user, resource.Key{Project: "web", Name: "impostor"})
	if _, err := b.Get(user, resource.Key{Project: "web", Name: "owned"}); err != nil {
		t.Errorf("the sweep collected an object whose owner exists: %v", err)
	}

	// Deleting the owner collects what it owns.
	if _, err := b.Delete(user, resource.Key{Project: "web", Name: "owner"}, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := b.Get(user, resource.Key{Project: "web", Name: "owned"})
		if connect.CodeOf(err) == connect.CodeNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deleting the owner did not collect its dependent")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStoredBytesCarryNoResourceVersion(t *testing.T) {
	s, system, _ := running(t)
	if _, err := s.backend(registry.Container).Apply(system, ctr("api")); err != nil {
		t.Fatal(err)
	}
	o, err := s.store.Get(system, store.Key{Kind: "Container", Project: "web", Name: "api"})
	if err != nil {
		t.Fatal(err)
	}
	var c computev1alpha1.Container
	if err := proto.Unmarshal(o.Data, &c); err != nil {
		t.Fatal(err)
	}
	if c.GetMetadata().GetResourceVersion() != 0 {
		t.Errorf("stored bytes carry resource version %d; the store revision is the only source", c.GetMetadata().GetResourceVersion())
	}
}
