package apiserver

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// container makes a Container in project "web" as the system and returns
// it as stored.
func container(t *testing.T, s *Server, system context.Context, name string) *computev1alpha1.Container {
	t.Helper()
	res, err := s.backend(registry.Container).Apply(system, ctr(name))
	if err != nil {
		t.Fatal(err)
	}
	return res.Object.(*computev1alpha1.Container)
}

// ownBy makes the container name owned by its project and by owners, and
// nothing else, as the system can.
func ownBy(t *testing.T, s *Server, system context.Context, name string, owners ...*computev1alpha1.Container) {
	t.Helper()
	b := s.backend(registry.Container)
	got, err := b.Get(system, resource.Key{Project: "web", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	c := got.(*computev1alpha1.Container)
	refs := slices.DeleteFunc(c.GetMetadata().GetOwnerReferences(), func(r *metav1.OwnerReference) bool {
		return r.GetKind() == registry.Container.Kind
	})
	for _, o := range owners {
		refs = append(refs, &metav1.OwnerReference{
			ApiVersion: registry.Container.APIVersion(), Kind: registry.Container.Kind,
			Name: o.GetMetadata().GetName(), Uid: o.GetMetadata().GetUid(),
		})
	}
	c.Metadata.OwnerReferences = refs
	if _, err := b.Apply(system, c); err != nil {
		t.Fatal(err)
	}
}

// indexed returns the names of the containers the owner index holds as
// owned by c, once the garbage collector has followed every write.
func indexed(t *testing.T, s *Server, c *computev1alpha1.Container) []string {
	t.Helper()
	waitSettled(t, s)
	keys, _, ok := s.ownerIdx.lookup(c.GetMetadata().GetUid())
	if !ok {
		t.Fatal("the owner index is not built")
	}
	var names []string
	for _, k := range keys {
		names = append(names, k.Name)
	}
	return names
}

func ctrKey(name string) resource.Key { return resource.Key{Project: "web", Name: name} }

func TestOwnerIndexFollowsReferencesAddedAndRemoved(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	b := s.backend(registry.Container)
	owner := container(t, s, system, "owner")
	container(t, s, system, "dep")
	if got := indexed(t, s, owner); len(got) != 0 {
		t.Fatalf("owned before any reference: %v", got)
	}

	ownBy(t, s, system, "dep", owner)
	if got := indexed(t, s, owner); !slices.Equal(got, []string{"dep"}) {
		t.Errorf("after the reference was added: %v, want [dep]", got)
	}

	ownBy(t, s, system, "dep")
	if got := indexed(t, s, owner); len(got) != 0 {
		t.Errorf("after the reference was removed: %v, want none", got)
	}

	if _, err := b.Delete(user, ctrKey("owner"), nil); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "owner gone", deleted(b, user, ctrKey("owner")))
	waitSettled(t, s)
	if _, err := b.Get(user, ctrKey("dep")); err != nil {
		t.Errorf("deleting its former owner collected dep: %v", err)
	}
}

func TestOwnerIndexDropsADependentDeletedFirst(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	b := s.backend(registry.Container)
	owner := container(t, s, system, "owner")
	container(t, s, system, "dep")
	ownBy(t, s, system, "dep", owner)
	if got := indexed(t, s, owner); !slices.Equal(got, []string{"dep"}) {
		t.Fatalf("owned: %v, want [dep]", got)
	}

	if _, err := b.Delete(user, ctrKey("dep"), nil); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "dep gone", deleted(b, user, ctrKey("dep")))
	if got := indexed(t, s, owner); len(got) != 0 {
		t.Errorf("after the dependent went: %v, want none", got)
	}

	if _, err := b.Delete(user, ctrKey("owner"), nil); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "owner gone", deleted(b, user, ctrKey("owner")))
	waitSettled(t, s)
	s.ownerIdx.mu.Lock()
	defer s.ownerIdx.mu.Unlock()
	if deps, ok := s.ownerIdx.m.dependents[owner.GetMetadata().GetUid()]; ok {
		t.Errorf("the index still holds the owner, with %v", deps)
	}
	if uids, ok := s.ownerIdx.m.owners[store.Key{Kind: registry.Container.Kind, Project: "web", Name: "dep"}]; ok {
		t.Errorf("the index still holds the deleted dependent, naming %v", uids)
	}
}

func TestOwnerIndexFollowsAMoveBetweenOwners(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	b := s.backend(registry.Container)
	first := container(t, s, system, "first")
	second := container(t, s, system, "second")
	container(t, s, system, "dep")
	ownBy(t, s, system, "dep", first)
	ownBy(t, s, system, "dep", second)
	if got := indexed(t, s, first); len(got) != 0 {
		t.Errorf("the former owner still owns %v", got)
	}
	if got := indexed(t, s, second); !slices.Equal(got, []string{"dep"}) {
		t.Errorf("the new owner owns %v, want [dep]", got)
	}

	if _, err := b.Delete(user, ctrKey("first"), nil); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "first gone", deleted(b, user, ctrKey("first")))
	waitSettled(t, s)
	if _, err := b.Get(user, ctrKey("dep")); err != nil {
		t.Fatalf("deleting its former owner collected dep: %v", err)
	}
	if _, err := b.Delete(user, ctrKey("second"), nil); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "deleting its owner collects dep", deleted(b, user, ctrKey("dep")))
}

func TestOwnerIndexRebuildsOnRestart(t *testing.T) {
	t.Parallel()
	db := filepath.Join(t.TempDir(), "hangar.db")
	s, system, user, stop := stoppableAt(t, db, func(*Config) {})
	b := s.backend(registry.Container)
	owner := container(t, s, system, "owner")
	gone := container(t, s, system, "gone")
	for _, name := range []string{"dep", "moved"} {
		container(t, s, system, name)
		ownBy(t, s, system, name, owner)
	}
	ownBy(t, s, system, "moved", gone)
	if _, err := b.Delete(user, ctrKey("gone"), nil); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "gone gone", deleted(b, user, ctrKey("gone")))
	waitSettled(t, s)
	stop()

	s, ctx, _ := serve(t, db, func(*Config) {})
	user = auth.WithPrincipal(ctx, auth.Principal{Type: auth.User, ID: "alice@example.com"})
	if got := indexed(t, s, owner); !slices.Equal(got, []string{"dep"}) {
		t.Errorf("after a restart the owner owns %v, want [dep]", got)
	}
	if got := indexed(t, s, gone); len(got) != 0 {
		t.Errorf("after a restart a deleted owner owns %v", got)
	}
	b = s.backend(registry.Container)
	if _, err := b.Delete(user, ctrKey("owner"), nil); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "deleting the owner collects dep", deleted(b, user, ctrKey("dep")))
}

// TestAReferenceAddedToAGoneOwnerIsCollected: an owner's delete takes only
// what named it then, so a reference added later to an owner already gone
// is caught when it is written, not left to the periodic sweep.
func TestAReferenceAddedToAGoneOwnerIsCollected(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	b := s.backend(registry.Container)
	owner := container(t, s, system, "owner")
	container(t, s, system, "dep")
	if _, err := b.Delete(user, ctrKey("owner"), nil); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "owner gone", deleted(b, user, ctrKey("owner")))
	waitSettled(t, s)

	ownBy(t, s, system, "dep", owner)
	testutil.Eventually(t, "dep, naming a gone owner, collected", deleted(b, user, ctrKey("dep")))
}

// TestCascadeSeesWritesNotYetFollowed checks what a cascade relies on to
// conclude nothing is left: the dependents the index does not hold yet,
// written after the revision it is as of.
func TestCascadeSeesWritesNotYetFollowed(t *testing.T) {
	t.Parallel()
	s, system, _ := running(t)
	owner := container(t, s, system, "owner")
	waitSettled(t, s)
	rev, err := s.store.Revision(system)
	if err != nil {
		t.Fatal(err)
	}
	container(t, s, system, "dep")
	ownBy(t, s, system, "dep", owner)
	container(t, s, system, "other")

	keys, ok, err := s.ownedSince(system, owner.GetMetadata().GetUid(), rev, nil)
	if err != nil || !ok {
		t.Fatalf("ownedSince: %v, %v", ok, err)
	}
	want := []store.Key{{Kind: registry.Container.Kind, Project: "web", Name: "dep"}}
	if !slices.Equal(keys, want) {
		t.Errorf("written since the index: %v, want %v", keys, want)
	}
}

// TestOwnerIndexReplaysWhatItsListingSaw checks the start: the listing
// that builds the index may already show writes the watch then delivers
// again, which must leave it as the store is.
func TestOwnerIndexReplaysWhatItsListingSaw(t *testing.T) {
	t.Parallel()
	key := func(name string) store.Key { return store.Key{Kind: "Container", Project: "web", Name: name} }
	m := newOwnerMap()
	// As listed: a owned by o2 (it was owned by o1 first), b owned by o1,
	// c already deleted.
	m.set(key("a"), []string{"o2"})
	m.set(key("b"), []string{"o1"})
	var x ownerIndex
	x.install(10, m)
	ev := func(typ store.EventType, rev int64, name string) store.Event {
		return store.Event{Type: typ, Revision: rev, Object: store.Object{Key: key(name)}}
	}
	x.apply(ev(store.EventCreated, 11, "a"), []string{"o1"})
	x.apply(ev(store.EventCreated, 12, "c"), []string{"o1"})
	x.apply(ev(store.EventCreated, 13, "b"), []string{"o1"})
	x.apply(ev(store.EventUpdated, 14, "a"), []string{"o2"})
	x.apply(ev(store.EventDeleted, 15, "c"), []string{"o1"})

	for owner, want := range map[string][]store.Key{"o1": {key("b")}, "o2": {key("a")}} {
		got, rev, ok := x.lookup(owner)
		if !ok || rev != 15 || !slices.Equal(got, want) {
			t.Errorf("%s owns %v as of %d (built: %v), want %v as of 15", owner, got, rev, ok, want)
		}
	}
	if n := len(x.m.owners); n != 2 {
		t.Errorf("the index holds %d objects, want 2: %v", n, fmt.Sprint(x.m.owners))
	}
}
