package apiserver_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/auth"
)

func signUpReq(username, displayName string) *connect.Request[iamv1alpha1.SignUpRequest] {
	return connect.NewRequest(&iamv1alpha1.SignUpRequest{Username: username, DisplayName: displayName})
}

func whoAmI(t *testing.T, c clients) *iamv1alpha1.WhoAmIResponse {
	t.Helper()
	return must[iamv1alpha1.WhoAmIResponse](t)(c.accounts.WhoAmI(context.Background(), connect.NewRequest(&iamv1alpha1.WhoAmIRequest{})))
}

func checkUsername(t *testing.T, c clients, name string) bool {
	t.Helper()
	return must[tenancyv1alpha1.CheckNameResponse](t)(c.names.CheckName(context.Background(),
		connect.NewRequest(&tenancyv1alpha1.CheckNameRequest{Kind: "User", Name: name}))).GetAvailable()
}

// TestSigningUp: an invited person is refused everywhere, with the way out
// named, until they sign up; WhoAmI, CheckName for a username and SignUp
// itself work before, and the User carries their verified email.
func TestSigningUp(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	const carol = "carol@example.com"
	c := e.as(carol)

	// Not invited: WhoAmI answers, and nothing else does.
	if me := whoAmI(t, c); me.GetType() != "user" || me.GetId() != carol || me.GetInvited() || me.GetMember() || me.GetUser() != nil {
		t.Errorf("a stranger's WhoAmI: %v", me)
	}
	_, err := c.accounts.SignUp(ctx, signUpReq("carol", ""))
	wantCode(t, err, connect.CodePermissionDenied, "signing up without an invitation")
	if err != nil && !strings.Contains(err.Error(), "not a platform member") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	_, err = c.names.CheckName(ctx, connect.NewRequest(&tenancyv1alpha1.CheckNameRequest{Kind: "User", Name: "carol"}))
	wantCode(t, err, connect.CodePermissionDenied, "a stranger checking a username")

	// Invited, not signed up: refused everywhere else, told to sign up.
	e.invite(carol, "platform.member")
	if me := whoAmI(t, c); !me.GetInvited() || me.GetMember() || me.GetUser() != nil {
		t.Errorf("an invited person's WhoAmI: %v", me)
	}
	for what, call := range map[string]func() error{
		"listing tenants": func() error {
			_, err := c.tenants.ListTenants(ctx, connect.NewRequest(&tenancyv1alpha1.ListTenantsRequest{}))
			return err
		},
		"opening a tenant": func() error {
			_, err := c.tenants.ApplyTenant(ctx, connect.NewRequest(tenant("carolco")))
			return err
		},
		"checking a tenant name": func() error {
			_, err := c.names.CheckName(ctx, connect.NewRequest(&tenancyv1alpha1.CheckNameRequest{Kind: "Tenant", Name: "carolco"}))
			return err
		},
		"watching tenants": func() error {
			stream, err := c.tenants.WatchTenants(ctx, connect.NewRequest(&tenancyv1alpha1.WatchTenantsRequest{}))
			if err != nil {
				return err
			}
			defer func() { _ = stream.Close() }()
			if stream.Receive() {
				return fmt.Errorf("watch sent %v", stream.Msg())
			}
			return stream.Err()
		},
		"reading a container's logs": func() error {
			logs := computev1alpha1connect.NewContainerLogsServiceClient(http.DefaultClient, e.url,
				connect.WithInterceptors(headerInterceptor{auth.DevUserHeader, carol}))
			stream, err := logs.ContainerLogs(ctx, connect.NewRequest(&computev1alpha1.ContainerLogsRequest{Project: "web", Name: "api"}))
			if err != nil {
				return err
			}
			defer func() { _ = stream.Close() }()
			if stream.Receive() {
				return fmt.Errorf("logs sent %v", stream.Msg())
			}
			return stream.Err()
		},
		"changing a profile": func() error {
			_, err := c.accounts.UpdateProfile(ctx, connect.NewRequest(&iamv1alpha1.UpdateProfileRequest{DisplayName: "C"}))
			return err
		},
	} {
		err := call()
		if connect.CodeOf(err) != connect.CodePermissionDenied && connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("%s before signing up: %v", what, err)
		}
		if err == nil || !strings.Contains(err.Error(), "hangar signup") {
			t.Errorf("%s before signing up does not name hangar signup: %v", what, err)
		}
	}
	if !checkUsername(t, c, "carol") {
		t.Error("an unused username is not available")
	}

	u := must[iamv1alpha1.SignUpResponse](t)(c.accounts.SignUp(ctx, signUpReq("carol", "Carol"))).GetUser()
	if u.GetMetadata().GetName() != "carol" || u.GetSpec().GetEmail() != carol || u.GetSpec().GetDisplayName() != "Carol" || u.GetSpec().MaxOwnedTenants != nil {
		t.Errorf("signed up as %v", u)
	}
	if me := whoAmI(t, c); !me.GetInvited() || !me.GetMember() || me.GetUser().GetMetadata().GetName() != "carol" {
		t.Errorf("a member's WhoAmI: %v", me)
	}
	must[tenancyv1alpha1.ApplyTenantResponse](t)(c.tenants.ApplyTenant(ctx, connect.NewRequest(tenant("carolco"))))
	if checkUsername(t, e.as(alice), "carol") {
		t.Error("a taken username is available")
	}

	// Once is enough: a second sign-up, under any name, is refused.
	_, err = c.accounts.SignUp(ctx, signUpReq("carol2", ""))
	wantCode(t, err, connect.CodeAlreadyExists, "signing up twice")
}

// TestUsernames: a username is a DNS label, not reserved, not taken and never
// used before; CheckName answers the same.
func TestUsernames(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	const carol, dave = "carol@example.com", "dave@example.com"
	e.invite(carol, "platform.member")
	e.invite(dave, "platform.member")
	c := e.as(carol)

	for _, name := range []string{"admin", "root", "hangar", "system", "support", "api", "console", "www"} {
		if checkUsername(t, c, name) {
			t.Errorf("reserved %q is available", name)
		}
		_, err := c.accounts.SignUp(ctx, signUpReq(name, ""))
		wantCode(t, err, connect.CodeInvalidArgument, "signing up as reserved "+name)
	}
	_, err := c.accounts.SignUp(ctx, signUpReq("Carol_1", ""))
	wantCode(t, err, connect.CodeInvalidArgument, "a username that is no DNS label")
	_, err = c.names.CheckName(ctx, connect.NewRequest(&tenancyv1alpha1.CheckNameRequest{Kind: "User", Name: "Carol_1"}))
	wantCode(t, err, connect.CodeInvalidArgument, "checking a username that is no DNS label")

	// Taken by another person now.
	_, err = c.accounts.SignUp(ctx, signUpReq(usernameOf(alice), ""))
	wantCode(t, err, connect.CodeFailedPrecondition, "signing up as a taken username")
	if err != nil && strings.Contains(err.Error(), alice) {
		t.Errorf("the refusal names who holds it: %v", err)
	}

	// No one deletes a User, root or its person, so its name stays taken
	// and its email keeps it. (A name used before and gone from the store is
	// TestAUsernameIsNeverReused's.)
	must[iamv1alpha1.SignUpResponse](t)(c.accounts.SignUp(ctx, signUpReq("carol", "")))
	for who, cl := range map[string]clients{"root": e.as(root), "carol": c} {
		_, err := cl.users.DeleteUser(ctx, connect.NewRequest(&iamv1alpha1.DeleteUserRequest{Name: "carol"}))
		wantCode(t, err, connect.CodePermissionDenied, who+" deleting a user")
		if err != nil && !strings.Contains(err.Error(), "never deleted") {
			t.Errorf("%s deleting a user: the refusal does not say why: %v", who, err)
		}
	}
	must[iamv1alpha1.GetUserResponse](t)(e.as(root).users.GetUser(ctx, connect.NewRequest(&iamv1alpha1.GetUserRequest{Name: "carol"})))
	if checkUsername(t, e.as(dave), "carol") {
		t.Error("a user's name is available")
	}
}

// TestOneUserPerEmail: of two sign-ups racing for one email, one wins.
func TestOneUserPerEmail(t *testing.T) {
	e := newEnv(t)
	const carol = "carol@example.com"
	e.invite(carol, "platform.member")
	c := e.as(carol)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			_, errs[i] = c.accounts.SignUp(context.Background(), signUpReq("carol-"+string(rune('a'+i)), ""))
		})
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case connect.CodeOf(err) == connect.CodeAlreadyExists, connect.CodeOf(err) == connect.CodeFailedPrecondition:
		default:
			t.Errorf("a losing sign-up: %v", err)
		}
	}
	users := must[iamv1alpha1.ListUsersResponse](t)(e.as(root).users.ListUsers(t.Context(), connect.NewRequest(&iamv1alpha1.ListUsersRequest{})))
	mine := 0
	for _, u := range users.GetItems() {
		if u.GetSpec().GetEmail() == carol {
			mine++
		}
	}
	if won != 1 || mine != 1 {
		t.Errorf("%d sign-ups won, %d users have the email", won, mine)
	}
}

// TestUsersAreRoots: only platform.root reads, lists or changes Users, and
// nobody, root included, creates one for someone else or changes its email;
// a person reads their own through WhoAmI and changes only its display name.
func TestUsersAreRoots(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a, r := e.as(alice), e.as(root)
	aliceName := usernameOf(alice)

	// A forged email: a person, and root, making a User for an email.
	for who, c := range map[string]clients{"alice": a, "root": r} {
		_, err := c.users.ApplyUser(ctx, connect.NewRequest(&iamv1alpha1.ApplyUserRequest{User: &iamv1alpha1.User{
			Metadata: &metav1.ObjectMeta{Name: "mallory"}, Spec: &iamv1alpha1.UserSpec{Email: "mallory@example.com"},
		}}))
		wantCode(t, err, connect.CodePermissionDenied, who+" creating a user")
	}
	_, err := r.users.GetUser(ctx, connect.NewRequest(&iamv1alpha1.GetUserRequest{Name: "mallory"}))
	wantCode(t, err, connect.CodeNotFound, "the forged user")

	// A tenant owner's grants do not reach Users, not even their own.
	_, err = a.users.GetUser(ctx, connect.NewRequest(&iamv1alpha1.GetUserRequest{Name: aliceName}))
	wantCode(t, err, connect.CodePermissionDenied, "alice getting her user")
	if got := must[iamv1alpha1.ListUsersResponse](t)(a.users.ListUsers(ctx, connect.NewRequest(&iamv1alpha1.ListUsersRequest{}))); len(got.GetItems()) != 0 {
		t.Errorf("alice lists %d users", len(got.GetItems()))
	}
	if me := whoAmI(t, a); me.GetUser().GetSpec().GetEmail() != alice {
		t.Errorf("alice's WhoAmI: %v", me)
	}
	if got := must[iamv1alpha1.ListUsersResponse](t)(r.users.ListUsers(ctx, connect.NewRequest(&iamv1alpha1.ListUsersRequest{}))); len(got.GetItems()) != 3 {
		t.Errorf("root lists %d users, want root, alice and bob", len(got.GetItems()))
	}

	// Only root sets the tenant cap.
	u := must[iamv1alpha1.GetUserResponse](t)(r.users.GetUser(ctx, connect.NewRequest(&iamv1alpha1.GetUserRequest{Name: aliceName}))).GetUser()
	capped := proto.Clone(u).(*iamv1alpha1.User)
	capped.Spec.MaxOwnedTenants = proto.Int32(5)
	_, err = a.users.ApplyUser(ctx, connect.NewRequest(&iamv1alpha1.ApplyUserRequest{User: capped}))
	wantCode(t, err, connect.CodePermissionDenied, "alice setting her own tenant cap")
	must[iamv1alpha1.ApplyUserResponse](t)(r.users.ApplyUser(ctx, connect.NewRequest(&iamv1alpha1.ApplyUserRequest{User: capped})))
	negative := proto.Clone(capped).(*iamv1alpha1.User)
	negative.Spec.MaxOwnedTenants = proto.Int32(-1)
	negative.Metadata.ResourceVersion = 0
	_, err = r.users.ApplyUser(ctx, connect.NewRequest(&iamv1alpha1.ApplyUserRequest{User: negative}))
	wantCode(t, err, connect.CodeInvalidArgument, "a negative tenant cap")

	// Not even root changes the email.
	moved := proto.Clone(capped).(*iamv1alpha1.User)
	moved.Spec.Email = "mallory@example.com"
	moved.Metadata.ResourceVersion = 0
	_, err = r.users.ApplyUser(ctx, connect.NewRequest(&iamv1alpha1.ApplyUserRequest{User: moved}))
	wantCode(t, err, connect.CodeInvalidArgument, "root changing a user's email")

	// The person changes their display name, and the cap stays.
	got := must[iamv1alpha1.UpdateProfileResponse](t)(a.accounts.UpdateProfile(ctx, connect.NewRequest(&iamv1alpha1.UpdateProfileRequest{DisplayName: "Alice L."}))).GetUser()
	if got.GetSpec().GetDisplayName() != "Alice L." || got.GetSpec().GetMaxOwnedTenants() != 5 || got.GetSpec().GetEmail() != alice {
		t.Errorf("after alice's profile change: %v", got)
	}

	// The display name is hers: root's apply leaving it out keeps it, and
	// one setting another is refused.
	quiet := proto.Clone(got).(*iamv1alpha1.User)
	quiet.Spec.DisplayName = ""
	quiet.Spec.MaxOwnedTenants = proto.Int32(7)
	quiet.Metadata.ResourceVersion = 0
	kept := must[iamv1alpha1.ApplyUserResponse](t)(r.users.ApplyUser(ctx, connect.NewRequest(&iamv1alpha1.ApplyUserRequest{User: quiet}))).GetUser()
	if kept.GetSpec().GetDisplayName() != "Alice L." || kept.GetSpec().GetMaxOwnedTenants() != 7 {
		t.Errorf("after root's apply without a display name: %v", kept)
	}
	renamed := proto.Clone(quiet).(*iamv1alpha1.User)
	renamed.Spec.DisplayName = "Mallory"
	_, err = r.users.ApplyUser(ctx, connect.NewRequest(&iamv1alpha1.ApplyUserRequest{User: renamed}))
	wantCode(t, err, connect.CodePermissionDenied, "root changing a user's display name")
	if err != nil && !strings.Contains(err.Error(), "UpdateProfile") {
		t.Errorf("the refusal does not point at UpdateProfile: %v", err)
	}
	if me := whoAmI(t, a); me.GetUser().GetSpec().GetDisplayName() != "Alice L." {
		t.Errorf("alice's display name is now %q", me.GetUser().GetSpec().GetDisplayName())
	}
}

// TestRemovingAMember: deleting someone's platform binding ends their
// membership and leaves their User; inviting them again restores it.
func TestRemovingAMember(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	b := e.as(bob)
	must[iamv1alpha1.DeleteRoleBindingResponse](t)(e.as(root).bindings.DeleteRoleBinding(ctx,
		connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: "member-" + usernameOf(bob)})))
	_, err := b.tenants.ListTenants(ctx, connect.NewRequest(&tenancyv1alpha1.ListTenantsRequest{}))
	wantCode(t, err, connect.CodePermissionDenied, "a removed member listing")
	if err != nil && !strings.Contains(err.Error(), "not a platform member") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	if me := whoAmI(t, b); me.GetInvited() || me.GetMember() || me.GetUser().GetMetadata().GetName() != usernameOf(bob) {
		t.Errorf("a removed member's WhoAmI: %v", me)
	}
	_, err = b.accounts.UpdateProfile(ctx, connect.NewRequest(&iamv1alpha1.UpdateProfileRequest{DisplayName: "B"}))
	wantCode(t, err, connect.CodePermissionDenied, "a removed member changing their profile")
	must[iamv1alpha1.GetUserResponse](t)(e.as(root).users.GetUser(ctx, connect.NewRequest(&iamv1alpha1.GetUserRequest{Name: usernameOf(bob)})))

	e.invite(bob, "platform.member")
	must[tenancyv1alpha1.ListTenantsResponse](t)(b.tenants.ListTenants(ctx, connect.NewRequest(&tenancyv1alpha1.ListTenantsRequest{})))
}
