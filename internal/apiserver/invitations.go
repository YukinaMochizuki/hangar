package apiserver

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
)

// validEmail checks an email as bindings and invitations name people: in
// lower case, with one "@" between a local part and a domain, and nothing
// that cannot be in a store key or a page token.
func validEmail(email string) error {
	local, domain, ok := strings.Cut(email, "@")
	switch {
	case len(email) > 254:
		return fmt.Errorf("%q is longer than 254 characters", email)
	case !ok || local == "" || domain == "" || strings.Contains(domain, "@"):
		return fmt.Errorf("%q is not an email address", email)
	case email != strings.ToLower(email):
		return fmt.Errorf("%q must be in lower case", email)
	case strings.ContainsAny(email, "/ \t\r\n"):
		return fmt.Errorf("%q may not hold a slash or white space", email)
	}
	return nil
}

// validateInvitation keeps an Invitation one for a person who has not
// signed up yet, naming a plan that exists and a tenant cap that leaves
// room for the tenant signing up opens. It runs under the users and plans
// locks.
func validateInvitation(ctx context.Context, s *Server, _ auth.Principal, old, obj proto.Message) error {
	inv := obj.(*iamv1alpha1.Invitation)
	email := inv.GetMetadata().GetName()
	if name, err := s.userWithEmail(ctx, email); err != nil {
		return err
	} else if name != "" {
		return failedPrecondition("%s has already signed up, as %q; set spec.plan or spec.max_owned_tenants on their User instead", email, name)
	}
	spec := inv.GetSpec()
	if spec.MaxOwnedTenants != nil && spec.GetMaxOwnedTenants() < 1 {
		return invalid("spec.max_owned_tenants %d is below 1: signing up opens a tenant", spec.GetMaxOwnedTenants())
	}
	var was string
	if old != nil {
		was = old.(*iamv1alpha1.Invitation).GetSpec().GetPlan()
	}
	return s.requirePlan(ctx, was, spec.GetPlan())
}

// requirePlan checks that plan, if set and not was, the plan named before,
// exists. Its callers hold the plans lock, which deleting a plan holds too.
func (s *Server) requirePlan(ctx context.Context, was, plan string) error {
	if plan == "" || plan == was {
		return nil
	}
	o, err := s.get(ctx, registry.Plan, resource.Key{Name: plan})
	if err != nil {
		return err
	}
	if o == nil {
		return failedPrecondition("spec.plan: plan %q does not exist", plan)
	}
	return nil
}

// invitationOf returns the Invitation of email, or nil if there is none.
func (s *Server) invitationOf(ctx context.Context, email string) (*iamv1alpha1.Invitation, error) {
	o, err := s.get(ctx, registry.Invitation, resource.Key{Name: email})
	if err != nil || o == nil {
		return nil, err
	}
	return o.(*iamv1alpha1.Invitation), nil
}
