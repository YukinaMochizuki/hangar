package apiserver

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// TestNoUserForServiceTokensOrLocalRoot: a service token acts on what is
// bound to it and root on the host on everything, neither with a User, and
// neither can sign up; root on the host changes Users as platform.root does,
// but creates none.
func TestNoUserForServiceTokensOrLocalRoot(t *testing.T) {
	s, system, _ := running(t)
	token := serviceToken(t, s, system, "web", "agent")
	local := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.Local, ID: "root@n1"})
	if _, err := s.backend(registry.RoleBinding).Apply(system, &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: "agent"},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_ServiceToken{ServiceToken: "web/agent"}},
			Role:    "tenant.owner",
			Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: "acme"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Container).Apply(token, ctr("agent")); err != nil {
		t.Errorf("a service token without a User: %v", err)
	}
	if _, err := s.backend(registry.Tenant).List(local, resource.ListOptions{}); err != nil {
		t.Errorf("root on the host without a User: %v", err)
	}
	for who, ctx := range map[string]context.Context{"a service token": token, "root on the host": local} {
		me, err := (account{s}).WhoAmI(ctx, connect.NewRequest(&iamv1alpha1.WhoAmIRequest{}))
		if err != nil || !me.Msg.GetMember() || me.Msg.GetInvited() || me.Msg.GetUser() != nil {
			t.Errorf("%s's WhoAmI: %v, %v", who, me, err)
		}
		_, err = (account{s}).SignUp(ctx, connect.NewRequest(&iamv1alpha1.SignUpRequest{Username: "robot"}))
		wantCodeOf(t, err, connect.CodeFailedPrecondition, who+" signing up")
	}
	_, err := s.backend(registry.User).Apply(local, &iamv1alpha1.User{
		Metadata: &metav1.ObjectMeta{Name: "robot"}, Spec: &iamv1alpha1.UserSpec{Email: "robot@example.com"},
	})
	wantCodeOf(t, err, connect.CodePermissionDenied, "root on the host creating a user")

	obj, err := s.backend(registry.User).Get(local, resource.Key{Name: usernameOf("alice@example.com")})
	if err != nil {
		t.Fatal(err)
	}
	u := proto.Clone(obj).(*iamv1alpha1.User)
	u.Spec.MaxOwnedTenants = proto.Int32(0)
	if _, err := s.backend(registry.User).Apply(local, u); err != nil {
		t.Errorf("root on the host setting a tenant cap: %v", err)
	}
	// Not even root on the host deletes a User.
	_, err = s.backend(registry.User).Delete(local, resource.Key{Name: usernameOf("alice@example.com")}, nil)
	wantCodeOf(t, err, connect.CodePermissionDenied, "root on the host deleting a user")
	if _, err := s.backend(registry.User).Get(local, resource.Key{Name: usernameOf("alice@example.com")}); err != nil {
		t.Errorf("the user is gone: %v", err)
	}
}

// TestAUsernameIsNeverReused: a username whose User is gone, which only
// happens beneath the API, stays claimed, and the refusal is about
// usernames.
func TestAUsernameIsNeverReused(t *testing.T) {
	s, _, _ := running(t)
	invite(t, s, "bob@example.com", "platform.member")
	gone := usernameOf("alice@example.com")
	if _, _, err := s.store.Delete(context.Background(), store.Key{Kind: registry.User.Kind, Name: gone}, 0); err != nil {
		t.Fatal(err)
	}
	bob := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: "bob@example.com"})
	free, err := (names{s}).CheckName(bob, connect.NewRequest(&tenancyv1alpha1.CheckNameRequest{Kind: "User", Name: gone}))
	if err != nil || free.Msg.GetAvailable() {
		t.Errorf("a used username checked: %v, %v", free, err)
	}
	_, err = (account{s}).SignUp(bob, connect.NewRequest(&iamv1alpha1.SignUpRequest{Username: gone}))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "signing up as a used username")
	if err != nil && (!strings.Contains(err.Error(), "choose another username") || strings.Contains(err.Error(), "display_name")) {
		t.Errorf("the refusal does not fit a username: %v", err)
	}
}
