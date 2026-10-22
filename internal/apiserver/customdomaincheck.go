package apiserver

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/authz"
)

// customDomainCheck serves CustomDomainCheckService: a domain's challenge
// CNAME looked for now, as the controller looks every minute
// (docs/decisions/custom-domains.md).
type customDomainCheck struct{ s *Server }

func (h customDomainCheck) CheckCustomDomain(ctx context.Context, req *connect.Request[networkv1alpha1.CheckCustomDomainRequest]) (*connect.Response[networkv1alpha1.CheckCustomDomainResponse], error) {
	b := h.s.backend(registry.CustomDomain)
	p, err := b.begin(ctx)
	if err != nil {
		return nil, err
	}
	key := resource.Key{Name: req.Msg.GetName()}
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	// As changing the domain: a check writes its status, and asks the
	// domain's nameservers on the caller's behalf. read refuses one the
	// caller may not, missing or not, alike.
	if _, _, err := b.read(ctx, p, authz.Apply, key); err != nil {
		return nil, err
	}
	d, err := h.s.checkCustomDomain(ctx, key.Name)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", b.asked(key)))
	}
	b.answer(p, d)
	return connect.NewResponse(&networkv1alpha1.CheckCustomDomainResponse{CustomDomain: d}), nil
}
