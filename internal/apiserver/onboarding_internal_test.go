package apiserver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// asUser is a context acting as the person with email.
func asUser(email string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: email})
}

// signUpAs signs an invited person up as username and returns the answer.
func signUpAs(s *Server, email, username string) (*iamv1alpha1.SignUpResponse, error) {
	res, err := (account{s}).SignUp(asUser(email), connect.NewRequest(&iamv1alpha1.SignUpRequest{Username: username}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// makePlan makes a plan with no limits, as the system.
func makePlan(t *testing.T, s *Server, system context.Context, name string) {
	t.Helper()
	if _, err := s.backend(registry.Plan).Apply(system, &tenancyv1alpha1.Plan{
		Metadata: &metav1.ObjectMeta{Name: name}, Spec: &tenancyv1alpha1.PlanSpec{Limits: &tenancyv1alpha1.Limits{}},
	}); err != nil {
		t.Fatal(err)
	}
}

// makeInvitation makes the Invitation of email, as the system.
func makeInvitation(t *testing.T, s *Server, system context.Context, email string, spec *iamv1alpha1.InvitationSpec) {
	t.Helper()
	if _, err := s.backend(registry.Invitation).Apply(system, &iamv1alpha1.Invitation{
		Metadata: &metav1.ObjectMeta{Name: email}, Spec: spec,
	}); err != nil {
		t.Fatal(err)
	}
}

// mustGet reads an object as the system, failing the test if it is not
// there.
func mustGet[T proto.Message](t *testing.T, s *Server, system context.Context, r registry.Resource, key resource.Key) T {
	t.Helper()
	obj, err := s.get(system, r, key)
	if err != nil {
		t.Fatal(err)
	}
	if obj == nil {
		t.Fatalf("%s %v does not exist", r.Kind, key)
	}
	return obj.(T)
}

var onboardingNamePattern = regexp.MustCompile(`^carol-example-com-[` + nameAlphabet + `]{5}$`)

// TestSignUpOpensATenantAndProject: signing up makes the User from the
// invitation, which it deletes, and opens a tenant the person owns, with
// the User's plan and counting toward their cap, holding a project of the
// same name with its default network.
func TestSignUpOpensATenantAndProject(t *testing.T) {
	s, system, _ := running(t)
	makePlan(t, s, system, "sponsor")
	invite(t, s, "carol@example.com", "platform.member")
	makeInvitation(t, s, system, "carol@example.com", &iamv1alpha1.InvitationSpec{Plan: "sponsor", MaxOwnedTenants: proto.Int32(2)})

	res, err := signUpAs(s, "carol@example.com", "carol-example-com")
	if err != nil {
		t.Fatal(err)
	}
	name := res.GetTenant()
	if !onboardingNamePattern.MatchString(name) || res.GetProject() != name {
		t.Fatalf("signing up opened tenant %q and project %q", name, res.GetProject())
	}
	u := res.GetUser().GetSpec()
	if u.GetEmail() != "carol@example.com" || u.GetPlan() != "sponsor" || u.MaxOwnedTenants == nil || u.GetMaxOwnedTenants() != 2 {
		t.Errorf("the User did not start with the invitation: %v", u)
	}
	if inv, err := s.invitationOf(system, "carol@example.com"); err != nil || inv != nil {
		t.Errorf("the invitation outlived signing up: %v, %v", inv, err)
	}

	tenant := mustGet[*tenancyv1alpha1.Tenant](t, s, system, registry.Tenant, resource.Key{Name: name})
	if tenant.GetSpec().GetDisplayName() != "carol-example-com" || tenant.GetSpec().GetPlan() != "sponsor" ||
		tenant.GetMetadata().GetAnnotations()[OpenedBy] != "user:carol@example.com" {
		t.Errorf("the tenant: %v %v", tenant.GetMetadata().GetAnnotations(), tenant.GetSpec())
	}
	project := mustGet[*tenancyv1alpha1.Project](t, s, system, registry.Project, resource.Key{Name: name})
	if project.GetSpec().GetTenant() != name || project.GetSpec().GetDisplayName() != DefaultProjectDisplayName ||
		!ownedBy(project.GetMetadata(), tenant.GetMetadata().GetUid()) {
		t.Errorf("the project: %v, owners %v", project.GetSpec(), project.GetMetadata().GetOwnerReferences())
	}
	network := mustGet[*networkv1alpha1.Network](t, s, system, registry.Network, resource.Key{Project: name, Name: DefaultNetwork})
	if network.GetSpec().GetIpv4Cidr() != DefaultNetworkCIDR {
		t.Errorf("the default network: %v", network.GetSpec())
	}
	owner := mustGet[*iamv1alpha1.RoleBinding](t, s, system, registry.RoleBinding,
		resource.Key{Name: ownerBindingName(name, tenant.GetMetadata().GetUid())})
	if owner.GetSpec().GetRole() != "tenant.owner" || owner.GetSpec().GetSubject().GetUser() != "carol@example.com" ||
		owner.GetSpec().GetScope().GetTenant() != name || !ownedBy(owner.GetMetadata(), tenant.GetMetadata().GetUid()) {
		t.Errorf("the owner binding: %v", owner.GetSpec())
	}

	// Carol can work in the project at once.
	carol := asUser("carol@example.com")
	if _, err := s.backend(registry.Network).Apply(carol, networkIn(name, "more", "10.7.0.0/24")); err != nil {
		t.Errorf("carol making a network in her project: %v", err)
	}
	// Her next tenant gets her plan too, and is the last her cap of 2 allows.
	tb := s.backend(registry.Tenant)
	if _, err := tb.Apply(carol, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "carol-two"}}); err != nil {
		t.Fatal(err)
	}
	if got := mustGet[*tenancyv1alpha1.Tenant](t, s, system, registry.Tenant, resource.Key{Name: "carol-two"}).GetSpec().GetPlan(); got != "sponsor" {
		t.Errorf("carol's second tenant has plan %q, want sponsor", got)
	}
	_, err = tb.Apply(carol, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "carol-three"}})
	wantExhausted(t, err, "owns 2 tenants and may own 2 (set on their User)", "a tenant past the cap the invitation set")

	// A plan given to her User later holds for tenants opened from then on,
	// and changes none she has.
	makePlan(t, s, system, "trial")
	user := mustGet[*iamv1alpha1.User](t, s, system, registry.User, resource.Key{Name: "carol-example-com"})
	user.Spec.Plan, user.Spec.MaxOwnedTenants = "trial", proto.Int32(3)
	if _, err := s.backend(registry.User).Apply(system, user); err != nil {
		t.Fatal(err)
	}
	if _, err := tb.Apply(carol, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "carol-three"}}); err != nil {
		t.Fatal(err)
	}
	for tenant, want := range map[string]string{name: "sponsor", "carol-two": "sponsor", "carol-three": "trial"} {
		if got := mustGet[*tenancyv1alpha1.Tenant](t, s, system, registry.Tenant, resource.Key{Name: tenant}).GetSpec().GetPlan(); got != want {
			t.Errorf("tenant %s has plan %q, want %q", tenant, got, want)
		}
	}
	// She may name her own plan, but no other.
	_, err = tb.Apply(carol, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "carol-four"}, Spec: &tenancyv1alpha1.TenantSpec{Plan: "sponsor"}})
	wantCodeOf(t, err, connect.CodePermissionDenied, "a person naming a plan not their User's")

	// Her own manifests, which name no plan, apply again: unchanged, and with
	// only the display name changed. The plan stays; another is refused.
	for _, tt := range []struct{ what, tenant, displayName string }{
		{"an unchanged manifest of the tenant signing up opened", name, "carol-example-com"},
		{"a new display name on the tenant signing up opened", name, "Carol's lab"},
		{"a new display name on a tenant she opened later", "carol-two", "Carol two"},
	} {
		if _, err := tb.Apply(carol, &tenancyv1alpha1.Tenant{
			Metadata: &metav1.ObjectMeta{Name: tt.tenant}, Spec: &tenancyv1alpha1.TenantSpec{DisplayName: tt.displayName},
		}); err != nil {
			t.Errorf("%s: %v", tt.what, err)
		}
		got := mustGet[*tenancyv1alpha1.Tenant](t, s, system, registry.Tenant, resource.Key{Name: tt.tenant}).GetSpec()
		if got.GetPlan() != "sponsor" || got.GetDisplayName() != tt.displayName {
			t.Errorf("after %s: %v", tt.what, got)
		}
	}
	_, err = tb.Apply(carol, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: name}, Spec: &tenancyv1alpha1.TenantSpec{Plan: "trial"}})
	wantCodeOf(t, err, connect.CodePermissionDenied, "a person changing an open tenant's plan, even to their User's")
	// Root's apply that names no plan keeps it too; naming the default plan
	// is how root moves a tenant back to it.
	root := asRoot(t, s, system)
	planOf := func(tenant string) string {
		t.Helper()
		return mustGet[*tenancyv1alpha1.Tenant](t, s, system, registry.Tenant, resource.Key{Name: tenant}).GetSpec().GetPlan()
	}
	if _, err := tb.Apply(root, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "carol-two"}, Spec: &tenancyv1alpha1.TenantSpec{DisplayName: "Carol two"}}); err != nil {
		t.Fatal(err)
	}
	if got := planOf("carol-two"); got != "sponsor" {
		t.Errorf("root applying a tenant without a plan left %q, want sponsor kept", got)
	}
	if _, err := tb.Apply(root, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "carol-two"}, Spec: &tenancyv1alpha1.TenantSpec{Plan: DefaultPlan}}); err != nil {
		t.Fatal(err)
	}
	if got := planOf("carol-two"); got != "" {
		t.Errorf("root naming the default plan left %q, want none", got)
	}
	// Carol's manifest applies again on the tenant now on the default plan,
	// whether it names no plan or the default one.
	for _, plan := range []string{"", DefaultPlan} {
		if _, err := tb.Apply(carol, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "carol-two"}, Spec: &tenancyv1alpha1.TenantSpec{Plan: plan}}); err != nil {
			t.Errorf("carol applying a tenant on the default plan with plan %q: %v", plan, err)
		}
		if got := planOf("carol-two"); got != "" {
			t.Errorf("after carol's apply with plan %q: %q", plan, got)
		}
	}
	// But she may not move one of hers to the default plan herself.
	_, err = tb.Apply(carol, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: name}, Spec: &tenancyv1alpha1.TenantSpec{Plan: DefaultPlan}})
	wantCodeOf(t, err, connect.CodePermissionDenied, "a person moving a tenant to the default plan")
}

// networkIn is a Network with a range, in project.
func networkIn(project, name, cidr string) *networkv1alpha1.Network {
	return &networkv1alpha1.Network{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec:     &networkv1alpha1.NetworkSpec{Ipv4Cidr: cidr},
	}
}

// TestSignUpWithoutAnInvitation: someone with no Invitation, such as the
// bootstrap admin, starts with the default plan and the platform's cap.
func TestSignUpWithoutAnInvitation(t *testing.T) {
	s, system, _ := running(t)
	// The setup, in which alice opens acme besides the tenant signing up
	// opened for her, runs under a higher cap.
	s.cfg.DefaultMaxOwnedTenants = DefaultMaxOwnedTenants
	invite(t, s, "dan@example.com", "platform.member")
	res, err := signUpAs(s, "dan@example.com", "dan")
	if err != nil {
		t.Fatal(err)
	}
	if u := res.GetUser().GetSpec(); u.GetPlan() != "" || u.MaxOwnedTenants != nil {
		t.Errorf("dan's User: %v", u)
	}
	if got := mustGet[*tenancyv1alpha1.Tenant](t, s, system, registry.Tenant, resource.Key{Name: res.GetTenant()}).GetSpec().GetPlan(); got != "" {
		t.Errorf("dan's tenant has plan %q, want the default", got)
	}
	_, err = s.backend(registry.Tenant).Apply(asUser("dan@example.com"), &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "dan-two"}})
	wantExhausted(t, err, "owns 1 tenants and may own 1 (the platform default)", "a second tenant at the default cap")
}

// TestOnboardingNames: the name is the username, cut to leave room for the
// suffix and a hostname label's prefix, and a valid label.
func TestOnboardingNames(t *testing.T) {
	for _, tt := range []struct{ username, want string }{
		{"carol", "carol-k3m9x"},
		{"a", "a-k3m9x"},
		{"abcdefghijklmnopqrstuvwxyz", "abcdefghijklmnopqrst-k3m9x"},
		// A '-' left at the end of the cut is dropped.
		{"abcdefghijklmnopqrs-tuvwxyz", "abcdefghijklmnopqrs-k3m9x"},
		{"abcdefghijklmnopqr--tuvwxyz", "abcdefghijklmnopqr-k3m9x"},
		{strings.Repeat("x", 63), strings.Repeat("x", 20) + "-k3m9x"},
	} {
		got := onboardingName(tt.username, "k3m9x")
		if got != tt.want {
			t.Errorf("onboardingName(%q) = %q, want %q", tt.username, got, tt.want)
		}
		if err := ValidName(got); err != nil || len(got) > 26 {
			t.Errorf("onboardingName(%q) = %q: %v, %d characters", tt.username, got, err, len(got))
		}
	}
	for range 100 {
		if sfx := randomNameSuffix(); !regexp.MustCompile(`^[a-hjkmnp-z2-9]{5}$`).MatchString(sfx) {
			t.Fatalf("suffix %q has a character it should not", sfx)
		}
	}
}

// TestSignUpAvoidsUsedNames: a name some tenant or some project has ever
// had is not given again; another suffix is drawn.
func TestSignUpAvoidsUsedNames(t *testing.T) {
	s, _, user := running(t)
	if _, err := s.backend(registry.Tenant).Apply(user, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "erin-aaaaa"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Project).Apply(user, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "erin-bbbbb"}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "acme"},
	}); err != nil {
		t.Fatal(err)
	}
	// A deleted tenant's name stays used.
	if _, err := s.backend(registry.Tenant).Apply(user, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "erin-ccccc"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Tenant).Delete(user, resource.Key{Name: "erin-ccccc"}, nil); err != nil {
		t.Fatal(err)
	}
	suffixes := []string{"aaaaa", "bbbbb", "ccccc", "ddddd"}
	var drawn atomic.Int32
	s.nameSuffix = func() string { return suffixes[drawn.Add(1)-1] }
	invite(t, s, "erin@example.com", "platform.member")
	res, err := signUpAs(s, "erin@example.com", "erin")
	if err != nil {
		t.Fatal(err)
	}
	if res.GetTenant() != "erin-ddddd" || res.GetProject() != "erin-ddddd" || drawn.Load() != 4 {
		t.Errorf("signing up opened %q and %q after %d draws, want erin-ddddd after 4", res.GetTenant(), res.GetProject(), drawn.Load())
	}

	// With every name it draws used, it gives up, and leaves nothing: the
	// username is still free.
	if _, err := s.backend(registry.Tenant).Apply(user, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "gus-zzzzz"}}); err != nil {
		t.Fatal(err)
	}
	drawn.Store(0)
	s.nameSuffix = func() string { drawn.Add(1); return "zzzzz" }
	invite(t, s, "gus@example.com", "platform.member")
	_, err = signUpAs(s, "gus@example.com", "gus")
	wantCodeOf(t, err, connect.CodeAborted, "signing up with every name drawn used")
	if drawn.Load() != nameAttempts {
		t.Errorf("gave up after %d draws, want %d", drawn.Load(), nameAttempts)
	}
	s.nameSuffix = func() string { return "yyyyy" }
	if res, err := signUpAs(s, "gus@example.com", "gus"); err != nil || res.GetTenant() != "gus-yyyyy" {
		t.Fatalf("signing up again: %v, %v", res, err)
	}
}

// failingTxn is a store whose transactions fail the creates of one kind,
// while fail is set, as a full disk would.
type failingTxn struct {
	store.Store
	kind string
	fail atomic.Bool
}

var errDiskFull = errors.New("disk full")

func (f *failingTxn) Txn(ctx context.Context, fn func(store.Tx) error) error {
	return f.Store.Txn(ctx, func(tx store.Tx) error { return fn(failingTx{tx, f}) })
}

type failingTx struct {
	store.Tx
	f *failingTxn
}

func (t failingTx) Create(ctx context.Context, key store.Key, data []byte) (int64, error) {
	if t.f.fail.Load() && key.Kind == t.f.kind {
		return 0, errDiskFull
	}
	return t.Tx.Create(ctx, key, data)
}

func (t failingTx) Delete(ctx context.Context, key store.Key, expected int64) (store.Object, int64, error) {
	if t.f.fail.Load() && key.Kind == t.f.kind {
		return store.Object{}, 0, errDiskFull
	}
	return t.Tx.Delete(ctx, key, expected)
}

// TestSignUpIsAllOrNothing: whichever write of signing up fails, nothing of
// it is left in the store: no User, no tenant, project, network or binding,
// no claimed name, and the invitation stays; signing up again then works.
func TestSignUpIsAllOrNothing(t *testing.T) {
	for _, kind := range []registry.Resource{registry.User, nameClaimResource, registry.Tenant, registry.RoleBinding, registry.Project, registry.Network, registry.Invitation} {
		t.Run(kind.Kind, func(t *testing.T) {
			var st *failingTxn
			s, system, _ := runningWith(t, func(c *Config) {
				st = &failingTxn{Store: c.Store, kind: kind.Kind}
				c.Store = st
			})
			makePlan(t, s, system, "sponsor")
			invite(t, s, "fay@example.com", "platform.member")
			makeInvitation(t, s, system, "fay@example.com", &iamv1alpha1.InvitationSpec{Plan: "sponsor"})
			s.nameSuffix = func() string { return "fffff" }
			before, err := s.store.Revision(system)
			if err != nil {
				t.Fatal(err)
			}

			st.fail.Store(true)
			_, err = signUpAs(s, "fay@example.com", "fay")
			if !errors.Is(err, errDiskFull) {
				t.Fatalf("signing up with %s creates failing: %v", kind.Kind, err)
			}
			if after, err := s.store.Revision(system); err != nil || after != before {
				t.Errorf("a failed sign-up moved the revision from %d to %d (%v)", before, after, err)
			}
			for _, key := range []store.Key{
				{Kind: registry.User.Kind, Name: "fay"},
				nameClaim(registry.User, resource.Key{Name: "fay"}),
				{Kind: registry.Tenant.Kind, Name: "fay-fffff"},
				nameClaim(registry.Tenant, resource.Key{Name: "fay-fffff"}),
				{Kind: registry.Project.Kind, Name: "fay-fffff"},
				nameClaim(registry.Project, resource.Key{Name: "fay-fffff"}),
				{Kind: registry.Network.Kind, Project: "fay-fffff", Name: DefaultNetwork},
			} {
				if _, err := s.store.Get(system, key); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("%v after a failed sign-up: %v", key, err)
				}
			}
			bindings, err := s.listWith(system, registry.RoleBinding, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, rb := range bindings {
				if spec := rb.(*iamv1alpha1.RoleBinding).GetSpec(); spec.GetSubject().GetUser() == "fay@example.com" && spec.GetRole() != "platform.member" {
					t.Errorf("binding %s after a failed sign-up", object.Meta(rb).GetName())
				}
			}
			if inv, err := s.invitationOf(system, "fay@example.com"); err != nil || inv == nil {
				t.Errorf("the invitation after a failed sign-up: %v, %v", inv, err)
			}
			if s.authz.Allowed(auth.Principal{Type: auth.User, ID: "fay@example.com"}, authz.Permission(registry.Network, authz.Get), authz.Target{Project: "fay-fffff"}) {
				t.Error("fay holds a grant on the project of a failed sign-up")
			}

			st.fail.Store(false)
			res, err := signUpAs(s, "fay@example.com", "fay")
			if err != nil {
				t.Fatalf("signing up again: %v", err)
			}
			if res.GetTenant() != "fay-fffff" || res.GetUser().GetSpec().GetPlan() != "sponsor" {
				t.Errorf("signing up again: %v", res)
			}
		})
	}
}

// nameClaimResource names the claims' kind for TestSignUpIsAllOrNothing.
var nameClaimResource = registry.Resource{Kind: nameClaimKind}

// TestInvitationRules: only platform.root reads and writes invitations; an
// invitation is keyed by a lowercase ASCII email, names a plan that exists
// and a cap of at least 1, and is for someone who has not signed up; a plan
// an invitation or a User names is not deleted; and a User's plan must
// exist.
func TestInvitationRules(t *testing.T) {
	s, system, user := running(t)
	invite(t, s, "root@example.com", "platform.root")
	signUp(t, s, "root@example.com")
	root := asUser("root@example.com")
	ib := s.backend(registry.Invitation)
	apply := func(ctx context.Context, name string, spec *iamv1alpha1.InvitationSpec) error {
		_, err := ib.Apply(ctx, &iamv1alpha1.Invitation{Metadata: &metav1.ObjectMeta{Name: name}, Spec: spec})
		return err
	}
	makePlan(t, s, system, "sponsor")

	wantCodeOf(t, apply(user, "gil@example.com", nil), connect.CodePermissionDenied, "a member inviting")
	if err := apply(root, "gil@example.com", &iamv1alpha1.InvitationSpec{Plan: "sponsor", MaxOwnedTenants: proto.Int32(1)}); err != nil {
		t.Fatalf("root inviting: %v", err)
	}
	_, err := ib.Get(user, resource.Key{Name: "gil@example.com"})
	wantCodeOf(t, err, connect.CodePermissionDenied, "a member reading an invitation")
	if res, err := ib.List(user, resource.ListOptions{}); err != nil || len(res.Items) != 0 {
		t.Errorf("a member lists %d invitations: %v", len(res.Items), err)
	}
	if res, err := ib.List(root, resource.ListOptions{}); err != nil || len(res.Items) != 1 {
		t.Errorf("root lists %d invitations: %v", len(res.Items), err)
	}

	for name, spec := range map[string]*iamv1alpha1.InvitationSpec{
		"Gil@example.com": nil, "gil": nil, "gil@": nil, "@example.com": nil, "gil@a@b": nil, "gil/x@example.com": nil,
		"\u212aate@example.com": nil, "gil@ex\u00e4mple.com": nil,
		"hal@example.com": {MaxOwnedTenants: proto.Int32(0)},
	} {
		wantCodeOf(t, apply(root, name, spec), connect.CodeInvalidArgument, "inviting "+name)
	}
	wantCodeOf(t, apply(root, "hal@example.com", &iamv1alpha1.InvitationSpec{Plan: "missing"}), connect.CodeFailedPrecondition, "a plan that does not exist")
	wantCodeOf(t, apply(root, "alice@example.com", nil), connect.CodeFailedPrecondition, "inviting someone who has signed up")

	// A plan an invitation names stays, and so does one a User names.
	pb := s.backend(registry.Plan)
	_, err = pb.Delete(root, resource.Key{Name: "sponsor"}, nil)
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "deleting a plan an invitation names")
	if _, err := ib.Delete(root, resource.Key{Name: "gil@example.com"}, nil); err != nil {
		t.Fatal(err)
	}
	alice := mustGet[*iamv1alpha1.User](t, s, system, registry.User, resource.Key{Name: usernameOf("alice@example.com")})
	alice.Spec.Plan = "missing"
	_, err = s.backend(registry.User).Apply(root, alice)
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "giving a User a plan that does not exist")
	alice.Spec.Plan = "sponsor"
	if _, err := s.backend(registry.User).Apply(root, alice); err != nil {
		t.Fatal(err)
	}
	_, err = pb.Delete(root, resource.Key{Name: "sponsor"}, nil)
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "deleting a plan a User names")
	// Only root gives a User a plan.
	alice.Spec.Plan = ""
	_, err = s.backend(registry.User).Apply(user, alice)
	wantCodeOf(t, err, connect.CodePermissionDenied, "a member changing their own plan")
}

// TestInvitingRacesSigningUp: an invitation made while its person signs up
// is either taken up by the sign-up or refused for an email with a User;
// never are both left.
func TestInvitingRacesSigningUp(t *testing.T) {
	s, system, _ := running(t)
	makePlan(t, s, system, "sponsor")
	for i := range 20 {
		email := fmt.Sprintf("p%d@example.com", i)
		invite(t, s, email, "platform.member")
		var wg sync.WaitGroup
		var signErr, invErr error
		wg.Go(func() { _, signErr = signUpAs(s, email, fmt.Sprintf("p%d", i)) })
		wg.Go(func() {
			_, invErr = s.backend(registry.Invitation).Apply(system, &iamv1alpha1.Invitation{
				Metadata: &metav1.ObjectMeta{Name: email}, Spec: &iamv1alpha1.InvitationSpec{Plan: "sponsor"},
			})
		})
		wg.Wait()
		if signErr != nil {
			t.Fatal(signErr)
		}
		inv, err := s.invitationOf(system, email)
		if err != nil {
			t.Fatal(err)
		}
		plan := mustGet[*iamv1alpha1.User](t, s, system, registry.User, resource.Key{Name: fmt.Sprintf("p%d", i)}).GetSpec().GetPlan()
		switch {
		case inv != nil:
			t.Fatalf("%s has a User and an Invitation", email)
		case invErr == nil && plan != "sponsor":
			t.Fatalf("%s's invitation was made and is gone, but their User has plan %q", email, plan)
		case invErr != nil && connect.CodeOf(invErr) != connect.CodeFailedPrecondition:
			t.Fatalf("inviting %s: %v", email, invErr)
		}
	}
}

// TestDeletingAPlanRacesNamingIt: a plan deleted while a User is given it
// either goes, and the User is refused it, or stays, as the User names it.
func TestDeletingAPlanRacesNamingIt(t *testing.T) {
	s, system, _ := running(t)
	name := usernameOf("alice@example.com")
	for i := range 20 {
		plan := fmt.Sprintf("plan%d", i)
		makePlan(t, s, system, plan)
		u := mustGet[*iamv1alpha1.User](t, s, system, registry.User, resource.Key{Name: name})
		u.Spec.Plan = plan
		u.Metadata.ResourceVersion = 0
		var wg sync.WaitGroup
		var applyErr, deleteErr error
		wg.Go(func() { _, applyErr = s.backend(registry.User).Apply(system, u) })
		wg.Go(func() { _, deleteErr = s.backend(registry.Plan).Delete(system, resource.Key{Name: plan}, nil) })
		wg.Wait()
		if (applyErr == nil) == (deleteErr == nil) {
			t.Fatalf("giving a User %s: %v; deleting it: %v; want exactly one to succeed", plan, applyErr, deleteErr)
		}
	}
}

// TestTwoSignUpsOfOnePerson: of two sign-ups at once by one person, one
// wins, and only it opens a tenant.
func TestTwoSignUpsOfOnePerson(t *testing.T) {
	s, system, _ := running(t)
	invite(t, s, "kim@example.com", "platform.member")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() { _, errs[i] = signUpAs(s, "kim@example.com", fmt.Sprintf("kim%d", i)) })
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("two sign-ups of one person: %v, %v; want exactly one to succeed", errs[0], errs[1])
	}
	tenants, err := s.listWith(system, registry.Tenant, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	opened := 0
	for _, o := range tenants {
		if object.Meta(o).GetAnnotations()[OpenedBy] == "user:kim@example.com" {
			opened++
		}
	}
	if opened != 1 {
		t.Errorf("two sign-ups of one person opened %d tenants", opened)
	}
}

// TestNodesCannotReadUsersOrInvitations: a node agent, which reads most
// kinds, gets no User and no Invitation, by get, list or watch, and writes
// neither.
func TestNodesCannotReadUsersOrInvitations(t *testing.T) {
	s, system, user := running(t)
	makeInvitation(t, s, system, "lee@example.com", &iamv1alpha1.InvitationSpec{})
	node := asNode(user, "n1")
	for _, tt := range []struct {
		r     registry.Resource
		name  string
		other proto.Message
	}{
		{registry.User, usernameOf("alice@example.com"), &iamv1alpha1.User{
			Metadata: &metav1.ObjectMeta{Name: "node-made"}, Spec: &iamv1alpha1.UserSpec{Email: "n@example.com"}}},
		{registry.Invitation, "lee@example.com", &iamv1alpha1.Invitation{Metadata: &metav1.ObjectMeta{Name: "node@example.com"}}},
	} {
		b := s.backend(tt.r)
		_, err := b.Get(node, resource.Key{Name: tt.name})
		wantCodeOf(t, err, connect.CodePermissionDenied, "a node getting a "+tt.r.Kind)
		if res, err := b.List(node, resource.ListOptions{}); err != nil || len(res.Items) != 0 {
			t.Errorf("a node listing %s: %d, %v", tt.r.Plural, len(res.Items), err)
		}
		_, err = b.Apply(node, tt.other)
		wantCodeOf(t, err, connect.CodePermissionDenied, "a node writing a "+tt.r.Kind)

		// A watch of a global kind judges each event as allowed does, with
		// the platform as target.
		nodeP := auth.Principal{Type: auth.Node, ID: "n1"}
		for _, verb := range []authz.Verb{authz.Get, authz.List, authz.Watch} {
			if b.allowed(nodeP, verb, authz.Target{}) {
				t.Errorf("a node may %s %s", verb, tt.r.Plural)
			}
		}
		// The apiserver itself still does.
		if !b.allowed(auth.SystemPrincipal, authz.Watch, authz.Target{}) {
			t.Errorf("the system may not watch %s", tt.r.Plural)
		}
	}
}

// TestOnlyTheSystemRunsATransaction: a transaction holds every other write
// back, so one acting as a person, whose writes wait for the authorization
// view and the quota lock, is refused before it starts.
func TestOnlyTheSystemRunsATransaction(t *testing.T) {
	s, system, user := running(t)
	for who, ctx := range map[string]context.Context{"a person": user, "no one": context.Background()} {
		ran := false
		err := s.atomically(ctx, func(context.Context) error {
			ran = true
			return nil
		})
		if err == nil || ran {
			t.Errorf("a transaction as %s: ran %v, %v", who, ran, err)
		}
	}
	if err := s.atomically(system, func(context.Context) error { return nil }); err != nil {
		t.Errorf("a transaction as the system: %v", err)
	}
}

// TestTheDefaultPlanAndCap are what a new store starts with.
func TestTheDefaultPlanAndCap(t *testing.T) {
	s, system, _ := runningWith(t, func(c *Config) { c.DefaultPlanLimits = nil })
	plan := mustGet[*tenancyv1alpha1.Plan](t, s, system, registry.Plan, resource.Key{Name: DefaultPlan})
	want := &tenancyv1alpha1.Limits{
		Cpu: proto.String("8"), Memory: proto.String("16Gi"), Disk: proto.String("100Gi"),
		Machines: proto.Int32(10), Projects: proto.Int32(5), Networks: proto.Int32(5), Addresses: proto.Int32(5),
		PublicEndpoints: proto.Int32(10), ServiceTokens: proto.Int32(10),
	}
	if !proto.Equal(plan.GetSpec().GetLimits(), want) {
		t.Errorf("the default plan's limits are %v, want %v", plan.GetSpec().GetLimits(), want)
	}
	if DefaultMaxOwnedTenants != 1 {
		t.Errorf("DefaultMaxOwnedTenants is %d, want 1", DefaultMaxOwnedTenants)
	}
}
