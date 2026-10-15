package apiserver

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// names serves NameService.
type names struct{ s *Server }

// CheckName says whether a never-reused name is still free: no object of the
// kind has it now, and none ever had it.
func (n names) CheckName(ctx context.Context, req *connect.Request[tenancyv1alpha1.CheckNameRequest]) (*connect.Response[tenancyv1alpha1.CheckNameResponse], error) {
	m := req.Msg
	r, ok := registry.Find(m.GetKind())
	if !ok || r.Kind != m.GetKind() || !hooksFor(r).neverReuseNames {
		return nil, invalid("kind %q: names are checked only for kinds whose names are never reused, Tenant and Project", m.GetKind())
	}
	b := n.s.backend(r)
	if _, err := b.begin(ctx); err != nil {
		return nil, err
	}
	key := resource.Key{Name: m.GetName()}
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	// Every object is created after its claim, and the claim outlives it, so
	// the claim alone answers.
	_, err := n.s.store.Get(ctx, nameClaim(r, key))
	switch {
	case errors.Is(err, store.ErrNotFound):
		return connect.NewResponse(&tenancyv1alpha1.CheckNameResponse{Available: true}), nil
	case err != nil:
		return nil, apiError(err)
	}
	return connect.NewResponse(&tenancyv1alpha1.CheckNameResponse{Available: false}), nil
}
