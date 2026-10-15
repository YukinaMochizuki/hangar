package apiserver

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// reservedUsernames are the usernames no one may sign up as, because they
// would read as the platform itself speaking.
var reservedUsernames = map[string]bool{
	"admin": true, "administrator": true, "root": true, "hangar": true, "system": true,
	"support": true, "api": true, "console": true, "www": true,
	"abuse": true, "security": true, "postmaster": true, "noreply": true,
}

func userLock(s *Server) *sync.Mutex { return &s.users }

// validateUser keeps a User what signing up made it. Only the system creates
// one, for the person signing up and with their email, never a reserved
// name, and at most one per email; validateUser runs under userLock, so two
// sign-ups for one email cannot both pass. spec.email is immutable, and
// spec.display_name is the person's own: only the system, for
// AccountService.UpdateProfile, changes it. An update by anyone else keeps
// the stored one when it leaves the field empty, so an administrator's apply
// never wipes it, and is refused when it sets another. Who may change the
// rest is RBAC's: only platform.root holds iam.users.apply.
func validateUser(ctx context.Context, s *Server, p auth.Principal, old, obj proto.Message) error {
	u := obj.(*iamv1alpha1.User)
	spec := u.GetSpec()
	if old == nil {
		if p.Type != auth.System {
			return denied("%s may not create users: a person creates their own with hangar signup --username NAME", p)
		}
		if reservedUsernames[u.GetMetadata().GetName()] {
			return invalid("username %q is reserved", u.GetMetadata().GetName())
		}
		if spec.GetEmail() == "" {
			return invalid("spec.email is required")
		}
		if name, err := s.userWithEmail(ctx, spec.GetEmail()); err != nil {
			return err
		} else if name != "" {
			return failedPrecondition("%s has already signed up, as %q; one email has one user", spec.GetEmail(), name)
		}
	} else if p.Type != auth.System && spec != nil {
		stored := old.(*iamv1alpha1.User).GetSpec().GetDisplayName()
		switch spec.GetDisplayName() {
		case "":
			// Written into the incoming object, which the update stores.
			spec.DisplayName = stored
		case stored:
		default:
			return denied("spec.display_name is the user's own; they change it with AccountService.UpdateProfile")
		}
	}
	if spec.MaxOwnedTenants != nil && spec.GetMaxOwnedTenants() < 0 {
		return invalid("spec.max_owned_tenants %d is negative", spec.GetMaxOwnedTenants())
	}
	return nil
}

// userWithEmail returns the name of the User with the email, or "" if there
// is none. It reads the store, not the authorization view, so under userLock
// it sees every User written.
func (s *Server) userWithEmail(ctx context.Context, email string) (string, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.User.Kind})
	if err != nil {
		return "", err
	}
	for _, o := range objs {
		var u iamv1alpha1.User
		if err := proto.Unmarshal(o.Data, &u); err != nil {
			return "", err
		}
		if u.GetSpec().GetEmail() == email {
			return o.Name, nil
		}
	}
	return "", nil
}

// userOf returns p's User, or nil if they have not signed up.
func (s *Server) userOf(ctx context.Context, p auth.Principal) (*iamv1alpha1.User, error) {
	name, ok := s.authz.Username(p)
	if !ok {
		return nil, nil
	}
	b := s.backend(registry.User)
	o, err := s.store.Get(ctx, b.storeKey(resource.Key{Name: name}))
	if errors.Is(err, store.ErrNotFound) {
		// Deleted since the view was brought up to date.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u, err := b.decode(o)
	if err != nil {
		return nil, err
	}
	return u.(*iamv1alpha1.User), nil
}

// account serves AccountService: a person's own account. Its calls accept
// someone invited who has not signed up yet, whom every other call refuses.
type account struct{ s *Server }

// WhoAmI tells any authenticated caller who they are, whether they were
// invited, and their User.
func (a account) WhoAmI(ctx context.Context, _ *connect.Request[iamv1alpha1.WhoAmIRequest]) (*connect.Response[iamv1alpha1.WhoAmIResponse], error) {
	p, err := a.s.authenticated(ctx)
	if err != nil {
		return nil, err
	}
	u, err := a.s.userOf(ctx, p)
	if err != nil {
		return nil, apiError(err)
	}
	return connect.NewResponse(&iamv1alpha1.WhoAmIResponse{
		Type: p.Type.String(), Id: p.ID, Invited: a.s.authz.IsInvited(p), Member: a.s.authz.IsMember(p), User: u,
	}), nil
}

// SignUp creates the caller's User, as the system, with the caller's email.
func (a account) SignUp(ctx context.Context, req *connect.Request[iamv1alpha1.SignUpRequest]) (*connect.Response[iamv1alpha1.SignUpResponse], error) {
	p, err := a.s.begin(ctx, true)
	if err != nil {
		return nil, err
	}
	if p.Type != auth.User {
		return nil, failedPrecondition("only people sign up; %s needs no user", p)
	}
	if name, ok := a.s.authz.Username(p); ok {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s has already signed up, as %q", p, name))
	}
	name := req.Msg.GetUsername()
	if err := ValidName(name); err != nil {
		return nil, invalid("username: %v", err)
	}
	res, err := a.s.backend(registry.User).Create(a.s.system(ctx), &iamv1alpha1.User{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec:     &iamv1alpha1.UserSpec{Email: p.ID, DisplayName: req.Msg.GetDisplayName()},
	})
	if connect.CodeOf(err) == connect.CodeAlreadyExists {
		return nil, failedPrecondition("username %q is taken; choose another", name)
	}
	if err != nil {
		return nil, err
	}
	a.s.log.Info("signed up", "principal", p.String(), "user", name)
	return connect.NewResponse(&iamv1alpha1.SignUpResponse{User: res.Object.(*iamv1alpha1.User)}), nil
}

// UpdateProfile changes the caller's display name, and nothing else of
// their User.
func (a account) UpdateProfile(ctx context.Context, req *connect.Request[iamv1alpha1.UpdateProfileRequest]) (*connect.Response[iamv1alpha1.UpdateProfileResponse], error) {
	p, err := a.s.begin(ctx, true)
	if err != nil {
		return nil, err
	}
	for range maxRetries {
		u, err := a.s.userOf(ctx, p)
		if err != nil {
			return nil, apiError(err)
		}
		if u == nil {
			return nil, failedPrecondition("%s has not signed up yet; run hangar signup --username NAME first", p)
		}
		u.Spec.DisplayName = req.Msg.GetDisplayName()
		// The read's resource version makes the write fail, rather than undo
		// a change made since, such as an administrator's.
		res, err := a.s.backend(registry.User).Apply(a.s.system(ctx), u)
		if connect.CodeOf(err) == connect.CodeAborted {
			continue
		}
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&iamv1alpha1.UpdateProfileResponse{User: res.Object.(*iamv1alpha1.User)}), nil
	}
	return nil, connect.NewError(connect.CodeAborted, errors.New("too many concurrent writers; retry"))
}
