package authz_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
)

// slowWatch delays every watch event, standing in for a view that lags the
// store.
type slowWatch struct {
	store.Store
	delay time.Duration
}

func (s slowWatch) Watch(ctx context.Context, opts store.WatchOptions, send func(store.Event) error) error {
	return s.Store.Watch(ctx, opts, func(ev store.Event) error {
		time.Sleep(s.delay)
		return send(ev)
	})
}

func setup(t *testing.T, delay time.Duration) (store.Store, *authz.Authorizer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hangar.db"))
	if err != nil {
		t.Fatal(err)
	}
	a := authz.New(slowWatch{st, delay}, nil)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = st.Close()
	})
	return st, a
}

func put(t *testing.T, st store.Store, r registry.Resource, name string, m proto.Message) {
	t.Helper()
	data, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(t.Context(), store.Key{Kind: r.Kind, Name: name}, data); err != nil {
		t.Fatal(err)
	}
}

func ownerBinding(user, tenant string) *iamv1alpha1.RoleBinding {
	return &iamv1alpha1.RoleBinding{Spec: &iamv1alpha1.RoleBindingSpec{
		Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: user}},
		Role:    "tenant.owner",
		Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: tenant}},
	}}
}

func memberBinding(user string) *iamv1alpha1.RoleBinding {
	return &iamv1alpha1.RoleBinding{Spec: &iamv1alpha1.RoleBindingSpec{
		Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: user}},
		Role:    "platform.member",
		Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}},
	}}
}

// signedUp is the User of a person who has signed up.
func signedUp(email string) *iamv1alpha1.User {
	return &iamv1alpha1.User{Spec: &iamv1alpha1.UserSpec{Email: email}}
}

var alice = auth.Principal{Type: auth.User, ID: "alice@example.com"}

func TestFreshSeesEarlierWrites(t *testing.T) {
	st, a := setup(t, 200*time.Millisecond)
	perm := authz.Permission(registry.Container, authz.Get)
	put(t, st, registry.Project, "web", &tenancyv1alpha1.Project{Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "acme"}})
	put(t, st, registry.RoleBinding, "b", ownerBinding(alice.ID, "acme"))
	put(t, st, registry.RoleBinding, "m", memberBinding(alice.ID))
	put(t, st, registry.User, "alice", signedUp(alice.ID))
	if err := a.Fresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !a.Allowed(alice, perm, authz.Target{Project: "web"}) {
		t.Error("a binding written before Fresh was not in effect after it")
	}
}

var localRoot = auth.Principal{Type: auth.Local, ID: "root@n1"}

func TestCoverage(t *testing.T) {
	st, a := setup(t, 0)
	put(t, st, registry.Project, "web", &tenancyv1alpha1.Project{Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "acme"}})
	put(t, st, registry.Project, "other", &tenancyv1alpha1.Project{Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "bobco"}})
	put(t, st, registry.RoleBinding, "b", ownerBinding(alice.ID, "acme"))
	put(t, st, registry.RoleBinding, "m", memberBinding(alice.ID))
	put(t, st, registry.User, "alice", signedUp(alice.ID))
	put(t, st, registry.RoleBinding, "carol", ownerBinding("carol@example.com", "acme"))
	if err := a.Fresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	get := authz.Permission(registry.Container, authz.Get)
	status := authz.Permission(registry.Container, authz.UpdateStatus)
	tests := []struct {
		name string
		p    auth.Principal
		perm string
		t    authz.Target
		want bool
	}{
		{"owner, own tenant", alice, get, authz.Target{Tenant: "acme"}, true},
		{"owner, project in own tenant", alice, get, authz.Target{Project: "web"}, true},
		{"owner, deleted project known by its tenant", alice, get, authz.Target{Project: "gone", Tenant: "acme"}, true},
		{"owner, other tenant's project", alice, get, authz.Target{Project: "other"}, false},
		{"owner, unknown project", alice, get, authz.Target{Project: "nowhere"}, false},
		{"owner, platform", alice, get, authz.Target{}, false},
		{"owner, status", alice, status, authz.Target{Project: "web"}, false},
		{"stranger", auth.Principal{Type: auth.User, ID: "bob@example.com"}, get, authz.Target{Project: "web"}, false},
		{"owner who is not a member", auth.Principal{Type: auth.User, ID: "carol@example.com"}, get, authz.Target{Project: "web"}, false},
		{"member opens a tenant", alice, authz.CreateTenant, authz.Target{}, true},
		{"non-member opens a tenant", auth.Principal{Type: auth.User, ID: "carol@example.com"}, authz.CreateTenant, authz.Target{}, false},
		{"service token with the owner's id", auth.Principal{Type: auth.ServiceToken, ID: alice.ID}, get, authz.Target{Project: "web"}, false},
		{"node reads", auth.Principal{Type: auth.Node, ID: "local"}, get, authz.Target{Project: "web"}, true},
		{"node writes status", auth.Principal{Type: auth.Node, ID: "local"}, status, authz.Target{Project: "web"}, true},
		{"node writes spec", auth.Principal{Type: auth.Node, ID: "local"}, authz.Permission(registry.Container, authz.Apply), authz.Target{Project: "web"}, false},
		{"root on the host, unbound, binds", localRoot, authz.Permission(registry.RoleBinding, authz.Apply), authz.Target{}, true},
		{"root on the host, other tenant's project", localRoot, get, authz.Target{Project: "other"}, true},
		{"root on the host execs", localRoot, authz.MachineExec, authz.Target{Project: "web"}, true},
		{"root on the host writes status", localRoot, status, authz.Target{Project: "web"}, false},
		{"a user named as root on the host", auth.Principal{Type: auth.User, ID: localRoot.ID}, get, authz.Target{Project: "web"}, false},
	}
	for _, tt := range tests {
		if got := a.Allowed(tt.p, tt.perm, tt.t); got != tt.want {
			t.Errorf("%s: allowed = %t, want %t", tt.name, got, tt.want)
		}
	}
}

func TestBindingDeletionRevokes(t *testing.T) {
	st, a := setup(t, 0)
	put(t, st, registry.RoleBinding, "b", ownerBinding(alice.ID, "acme"))
	put(t, st, registry.RoleBinding, "m", memberBinding(alice.ID))
	put(t, st, registry.User, "alice", signedUp(alice.ID))
	perm := authz.Permission(registry.Tenant, authz.Get)
	if err := a.Fresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !a.Allowed(alice, perm, authz.Target{Tenant: "acme"}) {
		t.Fatal("binding not in effect")
	}
	if _, _, err := st.Delete(t.Context(), store.Key{Kind: registry.RoleBinding.Kind, Name: "b"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := a.Fresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a.Allowed(alice, perm, authz.Target{Tenant: "acme"}) {
		t.Error("a deleted binding still grants")
	}
}

// TestMembershipNeedsAUser: a platform role invites a person, and only with
// their own User are they a member. Losing either ends it; service tokens and
// root on the host need neither.
func TestMembershipNeedsAUser(t *testing.T) {
	st, a := setup(t, 0)
	put(t, st, registry.RoleBinding, "b", ownerBinding(alice.ID, "acme"))
	put(t, st, registry.RoleBinding, "m", memberBinding(alice.ID))
	// Someone else's User proves nothing about alice.
	put(t, st, registry.User, "bob", signedUp("bob@example.com"))
	if err := a.Fresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	get := authz.Permission(registry.Tenant, authz.Get)
	if a.IsMember(alice) || !a.IsInvited(alice) || a.Allowed(alice, get, authz.Target{Tenant: "acme"}) || a.Allowed(alice, authz.CreateTenant, authz.Target{}) {
		t.Errorf("invited, not signed up: member %t, invited %t", a.IsMember(alice), a.IsInvited(alice))
	}
	if _, ok := a.Username(alice); ok {
		t.Error("a username for someone who never signed up")
	}
	bob := auth.Principal{Type: auth.User, ID: "bob@example.com"}
	if a.IsMember(bob) || a.IsInvited(bob) {
		t.Error("a User without a platform role makes a member")
	}

	put(t, st, registry.User, "alice", signedUp(alice.ID))
	if err := a.Fresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if name, ok := a.Username(alice); !a.IsMember(alice) || !ok || name != "alice" || !a.Allowed(alice, get, authz.Target{Tenant: "acme"}) {
		t.Errorf("signed up: member %t, username %q", a.IsMember(alice), name)
	}

	// Deleting the binding ends membership; the User stays.
	if _, _, err := st.Delete(t.Context(), store.Key{Kind: registry.RoleBinding.Kind, Name: "m"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := a.Fresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Username(alice); a.IsMember(alice) || a.IsInvited(alice) || !ok {
		t.Errorf("binding deleted: member %t, invited %t, username kept %t", a.IsMember(alice), a.IsInvited(alice), ok)
	}

	// Invited again but with the User deleted, they are not a member either.
	put(t, st, registry.RoleBinding, "m2", memberBinding(alice.ID))
	if _, _, err := st.Delete(t.Context(), store.Key{Kind: registry.User.Kind, Name: "alice"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := a.Fresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Username(alice); a.IsMember(alice) || ok {
		t.Error("a deleted User still makes a member")
	}

	for _, p := range []auth.Principal{{Type: auth.ServiceToken, ID: "agent.access"}, localRoot, {Type: auth.Node, ID: "n1"}, auth.SystemPrincipal} {
		if !a.IsMember(p) || a.IsInvited(p) {
			t.Errorf("%s: member %t, invited %t; want a member, not invited", p, a.IsMember(p), a.IsInvited(p))
		}
	}
	if !a.Allowed(localRoot, authz.Permission(registry.User, authz.Apply), authz.Target{}) {
		t.Error("root on the host may not change users")
	}
}
