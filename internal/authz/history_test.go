package authz_test

import (
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

func viewerBinding(user, project string) *iamv1alpha1.RoleBinding {
	return &iamv1alpha1.RoleBinding{Spec: &iamv1alpha1.RoleBindingSpec{
		Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: user}},
		Role:    "project.viewer",
		Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Project{Project: project}},
	}}
}

func webProject(tenant string) *tenancyv1alpha1.Project {
	return &tenancyv1alpha1.Project{Spec: &tenancyv1alpha1.ProjectSpec{Tenant: tenant}}
}

// del deletes an object and returns the revision of the deletion.
func del(t *testing.T, st store.Store, r registry.Resource, name string) int64 {
	t.Helper()
	_, rev, err := st.Delete(t.Context(), store.Key{Kind: r.Kind, Name: name}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rev
}

// fresh brings a's view up to every write so far, and prunes what it can.
func fresh(t *testing.T, a *authz.Authorizer) {
	t.Helper()
	if err := a.Fresh(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// allowedAt is a.AllowedAt at a revision the history must cover.
func allowedAt(t *testing.T, a *authz.Authorizer, p auth.Principal, perm string, target authz.Target, rev int64) bool {
	t.Helper()
	allowed, ok := a.AllowedAt(p, perm, target, rev)
	if !ok {
		t.Errorf("%s on %v at %d: the history does not reach back to it", perm, target, rev)
	}
	return allowed
}

// TestAllowedAtAnswersByTheViewAtARevision deletes, in turn, the project a
// tenant-scoped grant reaches, a binding, and the person's User. Each grant
// holds up to the revision that took it away, and not from it on.
func TestAllowedAtAnswersByTheViewAtARevision(t *testing.T) {
	st, a := setup(t, 0)
	put(t, st, registry.Project, "web", webProject("acme"))
	put(t, st, registry.RoleBinding, "owner", ownerBinding(alice.ID, "acme"))
	put(t, st, registry.RoleBinding, "viewer", viewerBinding(alice.ID, "api"))
	put(t, st, registry.RoleBinding, "m", memberBinding(alice.ID))
	put(t, st, registry.User, "alice", signedUp(alice.ID))
	fresh(t, a)

	get := authz.Permission(registry.Container, authz.Get)
	project := del(t, st, registry.Project, "web")
	binding := del(t, st, registry.RoleBinding, "owner")
	user := del(t, st, registry.User, "alice")
	fresh(t, a)

	tests := []struct {
		name   string
		target authz.Target
		gone   int64
	}{
		{"a project of the tenant, through the tenant's binding, across the project's delete", authz.Target{Project: "web"}, project},
		{"the tenant, across its binding's delete", authz.Target{Tenant: "acme"}, binding},
		{"a project bound alone, across the User's delete", authz.Target{Project: "api"}, user},
	}
	for _, tt := range tests {
		if !allowedAt(t, a, alice, get, tt.target, tt.gone-1) {
			t.Errorf("%s: denied just before the delete, at %d", tt.name, tt.gone-1)
		}
		if allowedAt(t, a, alice, get, tt.target, tt.gone) {
			t.Errorf("%s: allowed at the delete's own revision, %d", tt.name, tt.gone)
		}
		if a.Allowed(alice, get, tt.target) {
			t.Errorf("%s: allowed now", tt.name)
		}
	}
	before, ok1 := a.AllowedInProjectsOfAt(alice, get, "acme", project-1)
	after, ok2 := a.AllowedInProjectsOfAt(alice, get, "acme", binding)
	if !before || after || !ok1 || !ok2 {
		t.Errorf("AllowedInProjectsOfAt: %t (covered %t) before the project's delete, %t (covered %t) at the binding's", before, ok1, after, ok2)
	}
	if a.IsMember(alice) {
		t.Error("a deleted User still makes a member now")
	}
}

// TestTombstonesGoOnceOlderThanTheHistoryKept: with four revisions of
// history, a binding deleted within them is still answered for, a revision
// before them is answered for not at all, and churn keeps no more than
// they hold.
func TestTombstonesGoOnceOlderThanTheHistoryKept(t *testing.T) {
	st, a := setup(t, 0)
	a.RetainHistory(4)
	put(t, st, registry.RoleBinding, "m", memberBinding(alice.ID))
	put(t, st, registry.User, "alice", signedUp(alice.ID))
	put(t, st, registry.RoleBinding, "old", ownerBinding(alice.ID, "acme"))
	put(t, st, registry.RoleBinding, "recent", ownerBinding(alice.ID, "bobco"))
	get := authz.Permission(registry.Tenant, authz.Get)
	tombstones := func() int {
		t.Helper()
		kept, queued := a.Tombstones()
		if kept != queued {
			t.Errorf("%d tombstones kept, %d queued to drop", kept, queued)
		}
		return kept
	}

	// Every write moves the revision by one: recent goes four revisions
	// after old, which is then just out of the history. Old is bound again
	// in between.
	old := del(t, st, registry.RoleBinding, "old")
	put(t, st, registry.RoleBinding, "old", ownerBinding(alice.ID, "acme"))
	for i := range 2 {
		put(t, st, registry.Project, fmt.Sprintf("p%d", i), webProject("acme"))
	}
	recent := del(t, st, registry.RoleBinding, "recent")
	if recent != old+4 {
		t.Fatalf("deleted at %d and %d; want four revisions apart", old, recent)
	}
	fresh(t, a)
	if n := tombstones(); n != 1 {
		t.Errorf("%d tombstones kept, want the one within the history", n)
	}
	if !allowedAt(t, a, alice, get, authz.Target{Tenant: "bobco"}, recent-1) {
		t.Error("a binding deleted within the history is not answered for")
	}
	// Old is bound again now; old-1 is answered neither by that nor by
	// what is left of the history.
	if allowed, ok := a.AllowedAt(alice, get, authz.Target{Tenant: "acme"}, old-1); ok || allowed {
		t.Errorf("a revision before the history answered: allowed %t, covered %t", allowed, ok)
	}

	for i := range 20 {
		name := fmt.Sprintf("b%d", i)
		put(t, st, registry.RoleBinding, name, ownerBinding(alice.ID, "acme"))
		del(t, st, registry.RoleBinding, name)
	}
	fresh(t, a)
	// Of the deletes, every other revision, the last two are within four.
	if n := tombstones(); n != 2 {
		t.Errorf("after 20 bindings came and went, %d tombstones kept, want 2", n)
	}
}

// TestARevokedPeriodPastTheHistoryIsNotAnswered: alice loses her binding
// and gets it back. Once a later change has pushed the revocation out of
// the four revisions of history kept, a question about her revoked period
// is not answered, though she holds the grant now. The store may still
// let a watch resume from there until it next compacts.
func TestARevokedPeriodPastTheHistoryIsNotAnswered(t *testing.T) {
	st, a := setup(t, 0)
	a.RetainHistory(4)
	put(t, st, registry.RoleBinding, "m", memberBinding(alice.ID))
	put(t, st, registry.User, "alice", signedUp(alice.ID))
	put(t, st, registry.RoleBinding, "b", ownerBinding(alice.ID, "acme"))
	put(t, st, registry.RoleBinding, "other", ownerBinding("carol@example.com", "acme"))
	revoked := del(t, st, registry.RoleBinding, "b")
	put(t, st, registry.RoleBinding, "b", ownerBinding(alice.ID, "acme"))
	del(t, st, registry.RoleBinding, "other")
	for i := range 4 {
		put(t, st, registry.Project, fmt.Sprintf("p%d", i), webProject("acme"))
	}
	fresh(t, a)
	get := authz.Permission(registry.Tenant, authz.Get)
	if !a.Allowed(alice, get, authz.Target{Tenant: "acme"}) {
		t.Fatal("bound again, alice is not allowed now")
	}
	if allowed, ok := a.AllowedAt(alice, get, authz.Target{Tenant: "acme"}, revoked); ok || allowed {
		t.Errorf("her revoked period, past the history, answered: allowed %t, covered %t", allowed, ok)
	}
}

// TestARevisionBeforeTheListIsNotAnswered: an authorizer started over a
// store that holds alice's grant, revocation and new grant, as after an
// apiserver restart, knows nothing from before it listed the store. A
// question about her revoked period is not answered, though she holds the
// grant now.
func TestARevisionBeforeTheListIsNotAnswered(t *testing.T) {
	st := openStore(t)
	put(t, st, registry.RoleBinding, "m", memberBinding(alice.ID))
	put(t, st, registry.User, "alice", signedUp(alice.ID))
	put(t, st, registry.RoleBinding, "b", ownerBinding(alice.ID, "acme"))
	revoked := del(t, st, registry.RoleBinding, "b")
	put(t, st, registry.RoleBinding, "b", ownerBinding(alice.ID, "acme"))
	a := follow(t, st)
	fresh(t, a)
	get := authz.Permission(registry.Tenant, authz.Get)
	if !a.Allowed(alice, get, authz.Target{Tenant: "acme"}) {
		t.Fatal("bound again, alice is not allowed now")
	}
	if allowed, ok := a.AllowedAt(alice, get, authz.Target{Tenant: "acme"}, revoked); ok || allowed {
		t.Errorf("her revoked period, before the list, answered: allowed %t, covered %t", allowed, ok)
	}
	// From the list on, the history answers.
	put(t, st, registry.Project, "web", webProject("acme"))
	rev, err := st.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fresh(t, a)
	if !allowedAt(t, a, alice, get, authz.Target{Tenant: "acme"}, rev) {
		t.Error("after the list, alice's grant is not answered for")
	}
}

// TestRewritingTheSameGrantLeavesNoTombstone: a binding or project written
// again unchanged, as a status update does, is not a change of the view.
func TestRewritingTheSameGrantLeavesNoTombstone(t *testing.T) {
	st, a := setup(t, 0)
	put(t, st, registry.RoleBinding, "b", ownerBinding(alice.ID, "acme"))
	put(t, st, registry.Project, "web", webProject("acme"))
	put(t, st, registry.User, "alice", signedUp(alice.ID))
	fresh(t, a)
	for _, o := range []struct {
		r    registry.Resource
		name string
		m    proto.Message
	}{
		{registry.RoleBinding, "b", ownerBinding(alice.ID, "acme")},
		{registry.Project, "web", webProject("acme")},
		{registry.User, "alice", signedUp(alice.ID)},
	} {
		data, err := proto.Marshal(o.m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Update(t.Context(), store.Key{Kind: o.r.Kind, Name: o.name}, data, 0); err != nil {
			t.Fatal(err)
		}
	}
	fresh(t, a)
	if kept, _ := a.Tombstones(); kept != 0 {
		t.Errorf("%d tombstones from rewriting unchanged objects", kept)
	}
}
