package apiserver

import (
	"context"
	"sync"
	"testing"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
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
// the other, which holds it too, and the claim stays.
func TestASameHolderRaceKeepsTheHostname(t *testing.T) {
	var raced *racedCreates
	s, _, user := runningWith(t, func(c *Config) {
		raced = &racedCreates{Store: c.Store, kind: registry.PublicEndpoint.Kind, arrived: make(chan struct{}), first: make(chan struct{})}
		c.Store = raced
	})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		ctx := context.WithValue(user, creatorKey{}, i)
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
