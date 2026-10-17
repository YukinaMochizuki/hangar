package apiserver

import (
	"context"
	"errors"
	"sync"

	"google.golang.org/protobuf/proto"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// tenantOwnerRole is the role a tenant always keeps a person in.
const tenantOwnerRole = "tenant.owner"

// ownersLock is RoleBinding's lock for writes by anyone but the system. The
// system is left out: its writes are exempt from checkOwnerLeft, and the
// token cascade deletes bindings as the system while it holds the token's
// lock, which a person's create of a binding takes after this one.
func ownersLock(s *Server) *sync.Mutex { return &s.owners }

// holdOwners takes Server.owners for a call that checks several writes as a
// whole, and returns the context to make its writes with, which then do not
// take it again (holding).
func (s *Server) holdOwners(ctx context.Context) (context.Context, func()) {
	s.owners.Lock()
	return holding(ctx, &s.owners), s.owners.Unlock
}

// ownerTenant returns the tenant a binding makes a person owner of, if it
// is a person's tenant.owner binding. A service token's does not count: it
// goes when the token or its project is deleted, which no check stops.
func ownerTenant(rb *iamv1alpha1.RoleBinding) (string, bool) {
	t := rb.GetSpec().GetScope().GetTenant()
	return t, t != "" && rb.GetSpec().GetRole() == tenantOwnerRole && rb.GetSpec().GetSubject().GetUser() != ""
}

// checkOwnerLeft refuses a write that would take the bindings named in
// removing from tenant and leave it no person as tenant.owner. Whoever may
// do verb on role bindings at the platform (platform.root, root on the host,
// the apiserver itself) is exempt, and so is a tenant being deleted, or gone,
// whose bindings go with it.
func (s *Server) checkOwnerLeft(ctx context.Context, p auth.Principal, verb authz.Verb, tenant string, removing map[string]bool) error {
	if len(removing) == 0 || s.authz.Allowed(p, authz.Permission(registry.RoleBinding, verb), authz.Target{}) {
		return nil
	}
	o, err := s.store.Get(ctx, store.Key{Kind: registry.Tenant.Kind, Name: tenant})
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var t tenancyv1alpha1.Tenant
	if err := proto.Unmarshal(o.Data, &t); err != nil {
		return err
	}
	if t.GetMetadata().GetDeleteTime() != nil {
		return nil
	}
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.RoleBinding.Kind})
	if err != nil {
		return err
	}
	for _, o := range objs {
		var rb iamv1alpha1.RoleBinding
		if err := proto.Unmarshal(o.Data, &rb); err != nil {
			return err
		}
		if owned, ok := ownerTenant(&rb); ok && owned == tenant && !removing[o.Name] && rb.GetMetadata().GetDeleteTime() == nil {
			return nil
		}
	}
	return failedPrecondition("tenant %q would be left with no person as %s; make someone else %s first", tenant, tenantOwnerRole, tenantOwnerRole)
}

// refuseRemovingLastOwner is RoleBinding's beforeDelete: deleting a
// tenant's last tenant.owner binding is refused.
func refuseRemovingLastOwner(ctx context.Context, s *Server, obj proto.Message) error {
	rb := obj.(*iamv1alpha1.RoleBinding)
	tenant, ok := ownerTenant(rb)
	if !ok {
		return nil
	}
	p, _ := auth.FromContext(ctx)
	return s.checkOwnerLeft(ctx, p, authz.Delete, tenant, map[string]bool{rb.GetMetadata().GetName(): true})
}
