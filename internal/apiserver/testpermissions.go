package apiserver

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// maxTestedPermissions caps the permissions one TestPermissions call names.
const maxTestedPermissions = 100

// testedScope is where a TestPermissions call tests, resolved to the
// targets the calls themselves check.
type testedScope struct {
	// target is the scope's own: where an object living in it is checked.
	target authz.Target
	// tenant is where a Tenant at the scope is checked: the scope's tenant,
	// the project's tenant, or the platform.
	tenant authz.Target
}

// TestPermissions answers which of the permissions named the caller holds at
// the scope, each by the check its call makes. A scope the caller may not get
// answers as one that does not exist, with none, so the answer tells nothing
// of which tenants and projects there are. It writes nothing.
func (a account) TestPermissions(ctx context.Context, req *connect.Request[iamv1alpha1.TestPermissionsRequest]) (*connect.Response[iamv1alpha1.TestPermissionsResponse], error) {
	p, err := a.s.begin(ctx, false)
	if err != nil {
		return nil, err
	}
	perms := req.Msg.GetPermissions()
	switch {
	case len(perms) == 0:
		return nil, invalid("permissions: name at least one")
	case len(perms) > maxTestedPermissions:
		return nil, invalid("permissions: %d named, at most %d", len(perms), maxTestedPermissions)
	}
	for i, perm := range perms {
		if !authz.KnownPermission(perm) {
			return nil, invalid("permissions[%d]: %q is no permission", i, perm)
		}
	}
	sc, visible, err := a.s.testedScope(ctx, p, req.Msg.GetScope())
	if err != nil {
		return nil, err
	}
	res := &iamv1alpha1.TestPermissionsResponse{}
	if !visible {
		return connect.NewResponse(res), nil
	}
	seen := map[string]bool{}
	for _, perm := range perms {
		if seen[perm] {
			continue
		}
		seen[perm] = true
		if a.s.holds(p, perm, sc) {
			res.Permissions = append(res.Permissions, perm)
		}
	}
	return connect.NewResponse(res), nil
}

// testedScope resolves a TestPermissions scope, and whether the caller sees
// it: the platform always, a tenant or project when the caller may get it
// and it exists. Existence is read only after the get check passes.
func (s *Server) testedScope(ctx context.Context, p auth.Principal, scope *iamv1alpha1.BindingScope) (testedScope, bool, error) {
	switch t := scope.GetTarget().(type) {
	case *iamv1alpha1.BindingScope_Platform:
		return testedScope{}, true, nil
	case *iamv1alpha1.BindingScope_Tenant:
		if err := ValidName(t.Tenant); err != nil {
			return testedScope{}, false, invalid("scope.tenant: %v", err)
		}
		target := authz.Target{Tenant: t.Tenant}
		b := s.backend(registry.Tenant)
		if !b.allowed(p, authz.Get, target) {
			return testedScope{}, false, nil
		}
		ok, err := s.exists(ctx, b, t.Tenant)
		return testedScope{target: target, tenant: target}, ok, err
	case *iamv1alpha1.BindingScope_Project:
		if err := ValidName(t.Project); err != nil {
			return testedScope{}, false, invalid("scope.project: %v", err)
		}
		b := s.backend(registry.Project)
		if !b.allowed(p, authz.Get, authz.Target{Project: t.Project}) {
			return testedScope{}, false, nil
		}
		o, err := s.store.Get(ctx, b.storeKey(resource.Key{Name: t.Project}))
		if errors.Is(err, store.ErrNotFound) {
			return testedScope{}, false, nil
		}
		if err != nil {
			return testedScope{}, false, apiError(err)
		}
		m, err := b.decode(o)
		if err != nil {
			return testedScope{}, false, apiError(err)
		}
		tenant := m.(*tenancyv1alpha1.Project).GetSpec().GetTenant()
		return testedScope{
			target: authz.Target{Project: t.Project, Tenant: tenant},
			tenant: authz.Target{Tenant: tenant},
		}, true, nil
	}
	return testedScope{}, false, invalid("scope is required: the platform, a tenant or a project")
}

func (s *Server) exists(ctx context.Context, b *backend, name string) (bool, error) {
	_, err := s.store.Get(ctx, b.storeKey(resource.Key{Name: name}))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, apiError(err)
	}
	return true, nil
}

// holds reports whether p holds a known permission at sc, by the check its
// call makes: the machine capabilities and endpoint access by the
// authorizer, as their handlers ask it, the platform permissions by the
// authorizer at the platform, whatever the scope, and everything else by its
// kind's backend, with the kind's own rules and target.
func (s *Server) holds(p auth.Principal, perm string, sc testedScope) bool {
	r, verb, _ := authz.LookupPermission(perm)
	if authz.IsPlatform(perm) {
		return s.authz.Allowed(p, perm, authz.Target{})
	}
	if authz.IsReach(perm) {
		return s.authz.Allowed(p, perm, sc.target)
	}
	b := s.backend(r)
	var target authz.Target
	switch {
	case verb == authz.Create:
		// Opening a tenant is checked at the platform, wherever it is asked.
		target = authz.Target{}
	case r.Kind == registry.Tenant.Kind:
		target = sc.tenant
	case r.ProjectScoped(), r.Kind == registry.Project.Kind, r.Kind == registry.RoleBinding.Kind:
		target = sc.target
	default:
		// Every other global kind lives at the platform, and its calls check
		// there whatever the scope.
		target = b.target(resource.Key{}, nil)
	}
	return b.allowed(p, verb, target)
}
