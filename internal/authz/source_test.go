package authz_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// waitAllowed waits until a says want for p's permission on the target.
func waitAllowed(t *testing.T, a *authz.Authorizer, p auth.Principal, perm string, target authz.Target, want bool) {
	t.Helper()
	testutil.EventuallyWithin(t, 5*time.Second, fmt.Sprintf("%v %s on %v: %t", p, perm, target, want), func() bool {
		return a.Allowed(p, perm, target) == want
	})
}

// TestAnAuthorizerFollowsASource: an authorizer outside the apiserver, as
// hangar-edge runs, answers from what its source lists and then from every
// change it follows.
func TestAnAuthorizerFollowsASource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hangar.db"))
	if err != nil {
		t.Fatal(err)
	}
	put(t, st, registry.Project, "web", &tenancyv1alpha1.Project{Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "acme"}})
	put(t, st, registry.RoleBinding, "m", memberBinding(alice.ID))
	put(t, st, registry.User, "alice", signedUp(alice.ID))
	a := authz.NewFromSource(controller.StoreSource{Store: st}, nil)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = st.Close()
	})
	select {
	case <-a.Synced():
	case <-time.After(5 * time.Second):
		t.Fatal("never synced")
	}
	use := authz.PublicEndpointAccess
	if a.Allowed(alice, use, authz.Target{Project: "web"}) {
		t.Fatal("a member without a grant may use a private endpoint")
	}
	put(t, st, registry.RoleBinding, "b", ownerBinding(alice.ID, "acme"))
	waitAllowed(t, a, alice, use, authz.Target{Project: "web"}, true)
	// A project of another tenant is not covered.
	put(t, st, registry.Project, "other", &tenancyv1alpha1.Project{Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "bobco"}})
	waitAllowed(t, a, alice, use, authz.Target{Project: "other"}, false)
	// Her User gone, she is no member; signed up again, she is.
	if _, _, err := st.Delete(t.Context(), store.Key{Kind: registry.User.Kind, Name: "alice"}, 0); err != nil {
		t.Fatal(err)
	}
	waitAllowed(t, a, alice, use, authz.Target{Project: "web"}, false)
	put(t, st, registry.User, "alice", signedUp(alice.ID))
	waitAllowed(t, a, alice, use, authz.Target{Project: "web"}, true)
	// Membership taken away, nothing is allowed.
	if _, _, err := st.Delete(t.Context(), store.Key{Kind: registry.RoleBinding.Kind, Name: "m"}, 0); err != nil {
		t.Fatal(err)
	}
	waitAllowed(t, a, alice, use, authz.Target{Project: "web"}, false)
	if err := a.Fresh(t.Context()); err == nil {
		t.Error("an authorizer following a source claimed to be fresh")
	}
}

// TestTheEdgeMayOnlyRead: the Edge principal reads public endpoints, role
// bindings, projects and users, and nothing else, bindings or not.
func TestTheEdgeMayOnlyRead(t *testing.T) {
	_, a := setup(t, 0)
	edge := auth.Principal{Type: auth.Edge, ID: "n1"}
	for _, r := range []registry.Resource{registry.PublicEndpoint, registry.CustomDomain, registry.RoleBinding, registry.Project, registry.User} {
		for _, v := range []authz.Verb{authz.Get, authz.List, authz.Watch} {
			if !a.Allowed(edge, authz.Permission(r, v), authz.Target{Project: "web"}) {
				t.Errorf("the edge may not %s %s", v, r.Plural)
			}
		}
		for _, v := range []authz.Verb{authz.Apply, authz.Delete, authz.UpdateStatus} {
			if a.Allowed(edge, authz.Permission(r, v), authz.Target{}) {
				t.Errorf("the edge may %s %s", v, r.Plural)
			}
		}
	}
	for _, perm := range []string{
		authz.Permission(registry.Container, authz.Get), authz.Permission(registry.Machine, authz.List),
		authz.PublicEndpointAccess, authz.MachineExec, authz.CreateTenant,
	} {
		if a.Allowed(edge, perm, authz.Target{Project: "web"}) {
			t.Errorf("the edge holds %s", perm)
		}
	}
}

// TestWhoMayUseAPrivateEndpoint: owners, editors and platform roots, not
// viewers, nor members or sponsors without a grant.
func TestWhoMayUseAPrivateEndpoint(t *testing.T) {
	for role, want := range map[string]bool{
		"tenant.owner": true, "project.owner": true, "project.editor": true, "platform.root": true,
		"tenant.viewer": false, "project.viewer": false, "platform.sponsor": false, "platform.member": false,
	} {
		if got := authz.Roles[role].Grants(authz.PublicEndpointAccess); got != want {
			t.Errorf("%s grants %s: %t, want %t", role, authz.PublicEndpointAccess, got, want)
		}
	}
	if authz.PublicEndpointAccess != "network.publicendpoints.access" {
		t.Errorf("the permission is %q", authz.PublicEndpointAccess)
	}
}
