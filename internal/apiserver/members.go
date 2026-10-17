package apiserver

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// members serves MemberService. Every write is a RoleBinding apply or delete
// as the caller, so the rules are RoleBinding's; it adds the username and
// platform membership the caller could not read.
type members struct{ s *Server }

// memberScope is a tenant or project a member call names.
type memberScope struct {
	scope  *iamv1alpha1.BindingScope
	target authz.Target
	// key names it for good, such as "project/web".
	key string
	// what names it in the words of an answer, such as `project "web"`.
	what string
}

func parseMemberScope(scope *iamv1alpha1.BindingScope) (memberScope, error) {
	switch t := scope.GetTarget().(type) {
	case *iamv1alpha1.BindingScope_Tenant:
		if err := ValidName(t.Tenant); err != nil {
			return memberScope{}, invalid("scope.tenant: %v", err)
		}
		return memberScope{scope: scope, target: authz.Target{Tenant: t.Tenant}, key: "tenant/" + t.Tenant, what: fmt.Sprintf("tenant %q", t.Tenant)}, nil
	case *iamv1alpha1.BindingScope_Project:
		if err := ValidName(t.Project); err != nil {
			return memberScope{}, invalid("scope.project: %v", err)
		}
		return memberScope{scope: scope, target: authz.Target{Project: t.Project}, key: "project/" + t.Project, what: fmt.Sprintf("project %q", t.Project)}, nil
	case *iamv1alpha1.BindingScope_Platform:
		return memberScope{}, invalid("members are a tenant's or a project's; platform roles are granted by an administrator")
	}
	return memberScope{}, invalid("scope is required: a tenant or a project")
}

// memberEmail is the email a call names, as bindings hold it: in lower case.
func memberEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "", invalid("email is required")
	}
	return email, nil
}

// begin authenticates the call and checks that the caller may do verb on
// the role bindings at the scope, before anything about its members is read,
// so a refusal tells nothing of who they are.
func (m members) begin(ctx context.Context, verb authz.Verb, sc memberScope) error {
	b := m.s.backend(registry.RoleBinding)
	p, err := b.begin(ctx)
	if err != nil {
		return err
	}
	return b.authorize(p, verb, sc.target, fmt.Sprintf("rolebindings in %s", sc.what))
}

// bindingsAt returns the live bindings naming a person at exactly the scope,
// all of them when email is empty, ordered by email, role and name. It reads
// the store, which begin has authorized for the whole scope.
func (m members) bindingsAt(ctx context.Context, sc memberScope, email string) ([]*iamv1alpha1.RoleBinding, error) {
	b := m.s.backend(registry.RoleBinding)
	objs, _, err := m.s.store.List(ctx, store.ListOptions{Kind: registry.RoleBinding.Kind})
	if err != nil {
		return nil, apiError(err)
	}
	var out []*iamv1alpha1.RoleBinding
	for _, o := range objs {
		obj, err := b.decode(o)
		if err != nil {
			return nil, apiError(err)
		}
		rb := obj.(*iamv1alpha1.RoleBinding)
		user := rb.GetSpec().GetSubject().GetUser()
		if user == "" || email != "" && user != email || rb.GetMetadata().GetDeleteTime() != nil ||
			!proto.Equal(rb.GetSpec().GetScope(), sc.scope) {
			continue
		}
		out = append(out, rb)
	}
	slices.SortFunc(out, func(x, y *iamv1alpha1.RoleBinding) int {
		return cmp.Or(
			strings.Compare(x.GetSpec().GetSubject().GetUser(), y.GetSpec().GetSubject().GetUser()),
			strings.Compare(x.GetSpec().GetRole(), y.GetSpec().GetRole()),
			strings.Compare(x.GetMetadata().GetName(), y.GetMetadata().GetName()))
	})
	return out, nil
}

// member describes a binding, with what the authorizer knows of its person.
func (m members) member(rb *iamv1alpha1.RoleBinding) *iamv1alpha1.Member {
	email := rb.GetSpec().GetSubject().GetUser()
	p := auth.Principal{Type: auth.User, ID: email}
	username, _ := m.s.authz.Username(p)
	return &iamv1alpha1.Member{
		Email:          email,
		Role:           rb.GetSpec().GetRole(),
		Scope:          rb.GetSpec().GetScope(),
		Username:       username,
		PlatformMember: m.s.authz.IsMember(p),
		RoleBinding:    rb.GetMetadata().GetName(),
	}
}

// memberBindingName is the name AddMember gives a person's binding at a
// scope. It is the same for the same person and scope, so two adds of one
// person made at once cannot both create a binding.
func memberBindingName(sc memberScope, email string) string {
	sum := sha256.Sum256([]byte(sc.key + "\x00" + email))
	return "member-" + hex.EncodeToString(sum[:12])
}

func (m members) ListMembers(ctx context.Context, req *connect.Request[iamv1alpha1.ListMembersRequest]) (*connect.Response[iamv1alpha1.ListMembersResponse], error) {
	sc, err := parseMemberScope(req.Msg.GetScope())
	if err != nil {
		return nil, err
	}
	if err := m.begin(ctx, authz.List, sc); err != nil {
		return nil, err
	}
	rbs, err := m.bindingsAt(ctx, sc, "")
	if err != nil {
		return nil, err
	}
	res := &iamv1alpha1.ListMembersResponse{}
	for _, rb := range rbs {
		res.Members = append(res.Members, m.member(rb))
	}
	return connect.NewResponse(res), nil
}

func (m members) AddMember(ctx context.Context, req *connect.Request[iamv1alpha1.AddMemberRequest]) (*connect.Response[iamv1alpha1.AddMemberResponse], error) {
	sc, err := parseMemberScope(req.Msg.GetScope())
	if err != nil {
		return nil, err
	}
	email, err := memberEmail(req.Msg.GetEmail())
	if err != nil {
		return nil, err
	}
	if err := m.begin(ctx, authz.Apply, sc); err != nil {
		return nil, err
	}
	held, err := m.bindingsAt(ctx, sc, email)
	if err != nil {
		return nil, err
	}
	if len(held) > 0 {
		return nil, m.alreadyMember(sc, held[0])
	}
	res, err := m.s.backend(registry.RoleBinding).Create(ctx, &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: memberBindingName(sc, email)},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: email}},
			Role:    req.Msg.GetRole(),
			Scope:   sc.scope,
		},
	})
	if connect.CodeOf(err) == connect.CodeAlreadyExists {
		// Another add of the same person made it meanwhile.
		if held, lerr := m.bindingsAt(ctx, sc, email); lerr == nil && len(held) > 0 {
			return nil, m.alreadyMember(sc, held[0])
		}
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&iamv1alpha1.AddMemberResponse{
		Member: m.member(res.Object.(*iamv1alpha1.RoleBinding)), Warnings: res.Warnings,
	}), nil
}

func (m members) alreadyMember(sc memberScope, rb *iamv1alpha1.RoleBinding) error {
	return connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s already holds %s in %s; change it with SetMemberRole",
		rb.GetSpec().GetSubject().GetUser(), rb.GetSpec().GetRole(), sc.what))
}

func (m members) SetMemberRole(ctx context.Context, req *connect.Request[iamv1alpha1.SetMemberRoleRequest]) (*connect.Response[iamv1alpha1.SetMemberRoleResponse], error) {
	sc, err := parseMemberScope(req.Msg.GetScope())
	if err != nil {
		return nil, err
	}
	email, err := memberEmail(req.Msg.GetEmail())
	if err != nil {
		return nil, err
	}
	if err := m.begin(ctx, authz.Apply, sc); err != nil {
		return nil, err
	}
	// From the read to the last write, so the whole-call check holds.
	ctx, unlock := m.s.holdOwners(ctx)
	defer unlock()
	held, err := m.bindingsAt(ctx, sc, email)
	if err != nil {
		return nil, err
	}
	if len(held) == 0 {
		return nil, notMember(email, sc)
	}
	if len(held) > 1 {
		// The others are deleted: check what that needs up front, so the
		// call is not refused half-way.
		if err := m.begin(ctx, authz.Delete, sc); err != nil {
			return nil, err
		}
		if err := m.ownerLeft(ctx, authz.Apply, sc, held, req.Msg.GetRole()); err != nil {
			return nil, err
		}
	}
	// Keep a binding that already has the role, or else the first, and
	// change only its role: its scope and subject stay as they are.
	keep := held[0]
	if i := slices.IndexFunc(held, func(rb *iamv1alpha1.RoleBinding) bool { return rb.GetSpec().GetRole() == req.Msg.GetRole() }); i >= 0 {
		keep = held[i]
	}
	changed := proto.Clone(keep).(*iamv1alpha1.RoleBinding)
	changed.Spec.Role = req.Msg.GetRole()
	// The read's resource version makes the update fail, rather than undo a
	// change made since.
	b := m.s.backend(registry.RoleBinding)
	res, err := b.Apply(ctx, changed)
	if err != nil {
		return nil, err
	}
	for _, rb := range held {
		if rb == keep {
			continue
		}
		rv := rb.GetMetadata().GetResourceVersion()
		if _, err := b.Delete(ctx, resource.Key{Name: rb.GetMetadata().GetName()}, &rv); err != nil && connect.CodeOf(err) != connect.CodeNotFound {
			return nil, err
		}
	}
	return connect.NewResponse(&iamv1alpha1.SetMemberRoleResponse{
		Member: m.member(res.Object.(*iamv1alpha1.RoleBinding)), Warnings: res.Warnings,
	}), nil
}

func (m members) RemoveMember(ctx context.Context, req *connect.Request[iamv1alpha1.RemoveMemberRequest]) (*connect.Response[iamv1alpha1.RemoveMemberResponse], error) {
	sc, err := parseMemberScope(req.Msg.GetScope())
	if err != nil {
		return nil, err
	}
	email, err := memberEmail(req.Msg.GetEmail())
	if err != nil {
		return nil, err
	}
	if err := m.begin(ctx, authz.Delete, sc); err != nil {
		return nil, err
	}
	ctx, unlock := m.s.holdOwners(ctx)
	defer unlock()
	held, err := m.bindingsAt(ctx, sc, email)
	if err != nil {
		return nil, err
	}
	if len(held) == 0 {
		return nil, notMember(email, sc)
	}
	if len(held) > 1 {
		if err := m.ownerLeft(ctx, authz.Delete, sc, held, ""); err != nil {
			return nil, err
		}
	}
	b := m.s.backend(registry.RoleBinding)
	res := &iamv1alpha1.RemoveMemberResponse{}
	for _, rb := range held {
		rv := rb.GetMetadata().GetResourceVersion()
		if _, err := b.Delete(ctx, resource.Key{Name: rb.GetMetadata().GetName()}, &rv); err != nil && connect.CodeOf(err) != connect.CodeNotFound {
			return nil, err
		}
		res.Removed = append(res.Removed, m.member(rb))
	}
	return connect.NewResponse(res), nil
}

func notMember(email string, sc memberScope) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s holds no role in %s", email, sc.what))
}

// ownerLeft checks a call that writes several bindings as a whole, as each
// write is checked on its own: that a tenant scope keeps a tenant.owner
// binding once every binding in held has role, or is gone when role is "".
func (m members) ownerLeft(ctx context.Context, verb authz.Verb, sc memberScope, held []*iamv1alpha1.RoleBinding, role string) error {
	tenant := sc.scope.GetTenant()
	if tenant == "" || role == tenantOwnerRole {
		return nil
	}
	removing := map[string]bool{}
	for _, rb := range held {
		if _, ok := ownerTenant(rb); ok {
			removing[rb.GetMetadata().GetName()] = true
		}
	}
	p, _ := auth.FromContext(ctx)
	return m.s.checkOwnerLeft(ctx, p, verb, tenant, removing)
}
