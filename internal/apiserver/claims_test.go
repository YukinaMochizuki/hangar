package apiserver

import (
	"context"
	"sync"
	"testing"

	"connectrpc.com/connect"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// creatorKey tags the context of one of two racing creators.
type creatorKey struct{}

// racedCreates is a store that, for objects of kind, holds both creators at
// the object's create until both are there, then lets the one that did not
// take a claim store the object first: the claim's taker then finds the
// object made, the case where a claim must be kept.
type racedCreates struct {
	store.Store
	kind string

	mu      sync.Mutex
	claimer any
	arrived chan struct{}
	first   chan struct{}
	n       int
}

func (r *racedCreates) Create(ctx context.Context, key store.Key, data []byte) (int64, error) {
	who := ctx.Value(creatorKey{})
	if key.Kind == nameClaimKind {
		rev, err := r.Store.Create(ctx, key, data)
		if err == nil && who != nil {
			r.mu.Lock()
			if r.claimer == nil {
				r.claimer = who
			}
			r.mu.Unlock()
		}
		return rev, err
	}
	if key.Kind != r.kind || who == nil {
		return r.Store.Create(ctx, key, data)
	}
	r.mu.Lock()
	r.n++
	if r.n == 2 {
		close(r.arrived)
	}
	claimer := r.claimer
	r.mu.Unlock()
	<-r.arrived
	if who == claimer {
		<-r.first
		return r.Store.Create(ctx, key, data)
	}
	defer close(r.first)
	return r.Store.Create(ctx, key, data)
}

// TestASameHolderRaceKeepsTheHostname: two creators of one endpoint at
// once; the one that took the hostname's claim finds the endpoint made by
// the other, which holds it too, and the claim stays. One creator is the
// apiserver, as its container endpoint controller is: two callers' creates
// are serialized by the quota lock, which the apiserver's own do not take.
func TestASameHolderRaceKeepsTheHostname(t *testing.T) {
	t.Parallel()
	var raced *racedCreates
	s, system, user := runningWith(t, func(c *Config) {
		raced = &racedCreates{Store: c.Store, kind: registry.PublicEndpoint.Kind, arrived: make(chan struct{}), first: make(chan struct{})}
		c.Store = raced
	})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, who := range []context.Context{system, user} {
		ctx := context.WithValue(who, creatorKey{}, i)
		wg.Go(func() {
			_, errs[i] = s.backend(registry.PublicEndpoint).Apply(ctx, endpoint("site", "api", 80, private))
		})
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("applies: %v", errs)
	}
	if raced.claimer == nil {
		t.Fatal("no creator took the claim; the race did not happen")
	}
	claim := endpointClaim(resource.Key{Project: "web", Name: "site"}).key
	if _, err := s.store.Get(user, claim); err != nil {
		t.Fatalf("the hostname's claim after a same-holder race: %v", err)
	}
}

// TestASameNameRaceKeepsTheName: two creators of one tenant at once; the
// name's claim stays, as the generic never-reused path keeps it.
func TestASameNameRaceKeepsTheName(t *testing.T) {
	t.Parallel()
	s, _, user := running(t)
	errs := make([]error, 4)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = s.backend(registry.Tenant).Apply(user, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "racy"}})
		})
	}
	wg.Wait()
	if _, err := s.store.Get(user, nameClaim(registry.Tenant, resource.Key{Name: "racy"})); err != nil {
		t.Fatalf("the name's claim after a race: %v (applies %v)", err, errs)
	}
	if _, err := s.store.Get(user, store.Key{Kind: registry.Tenant.Kind, Name: "racy"}); err != nil {
		t.Fatalf("the tenant after a race: %v (applies %v)", err, errs)
	}
}

// missedOnce is a store whose first Get of key, once armed, answers NotFound,
// as a read made just before another writer created the object would; the
// create that follows finds the object there.
type missedOnce struct {
	store.Store
	key store.Key

	mu      sync.Mutex
	armed   bool
	creates int
}

func (m *missedOnce) arm() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.armed, m.creates = true, 0
}

func (m *missedOnce) Get(ctx context.Context, key store.Key) (store.Object, error) {
	m.mu.Lock()
	missed := m.armed && key == m.key
	if missed {
		m.armed = false
	}
	m.mu.Unlock()
	if missed {
		return store.Object{}, store.ErrNotFound
	}
	return m.Store.Get(ctx, key)
}

func (m *missedOnce) Create(ctx context.Context, key store.Key, data []byte) (int64, error) {
	if key == m.key {
		m.mu.Lock()
		m.creates++
		m.mu.Unlock()
	}
	return m.Store.Create(ctx, key, data)
}

// TestARacedCreateIsAuthorizedAgainstTheObjectFound: bob creates a binding
// under the name of alice's, scoped to his own tenant, where he may create
// one; his read misses hers, so his create finds it. He is refused as for
// her binding, create-only or not, and never told that it exists.
func TestARacedCreateIsAuthorizedAgainstTheObjectFound(t *testing.T) {
	t.Parallel()
	name := "member-" + usernameOf("alice@example.com")
	var missed *missedOnce
	s, _, _ := runningWith(t, func(c *Config) {
		missed = &missedOnce{Store: c.Store, key: store.Key{Kind: registry.RoleBinding.Kind, Name: name}}
		c.Store = missed
	})
	addMember(t, s, "bob@example.com")
	bob := auth.WithPrincipal(t.Context(), auth.Principal{Type: auth.User, ID: "bob@example.com"})
	if _, err := s.backend(registry.Tenant).Apply(bob, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "bobco"}}); err != nil {
		t.Fatal(err)
	}
	rb := &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: "bob@example.com"}},
			Role:    "tenant.viewer",
			Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: "bobco"}},
		},
	}
	b := s.backend(registry.RoleBinding)
	for _, createOnly := range []bool{false, true} {
		missed.arm()
		var err error
		if createOnly {
			_, err = b.Create(bob, rb)
		} else {
			_, err = b.Apply(bob, rb)
		}
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("createOnly=%t: got %v, want permission_denied", createOnly, err)
		}
		missed.mu.Lock()
		creates := missed.creates
		missed.mu.Unlock()
		if creates == 0 {
			t.Errorf("createOnly=%t: no create of the binding was tried; the race did not happen", createOnly)
		}
	}
}
