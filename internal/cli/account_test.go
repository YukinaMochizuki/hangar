package cli_test

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/cli"
)

// TestSignUpThenCreate is the onboarding a new person goes through: invited,
// refused everywhere with the way out named, signed up, then at work.
func TestSignUpThenCreate(t *testing.T) {
	w := newWorld(t)
	w.as("carol@example.com")
	if out := w.must("", "whoami"); out != "user:carol@example.com\nnot a platform member yet: an administrator has to add you\n" {
		t.Errorf("whoami before the invitation: %q", out)
	}
	if r := w.run("", "signup", "--username", "carol"); r.code == 0 || !strings.Contains(r.err, "not a platform member") {
		t.Errorf("signup before the invitation: exit %d: %s", r.code, r.err)
	}

	w.invite("platform.member", "carol@example.com")
	w.as("carol@example.com")
	if out := w.must("", "whoami"); out != "user:carol@example.com\ninvited, not signed up yet: run hangar signup --username NAME\n" {
		t.Errorf("whoami before signing up: %q", out)
	}
	if r := w.run("", "create", "tenant", "--display-name", "Carol's lab"); r.code == 0 || !strings.Contains(r.err, "hangar signup --username NAME") {
		t.Errorf("create before signing up: exit %d: %s", r.code, r.err)
	}
	for _, name := range []string{"admin", usernameOf("alice@example.com")} {
		if r := w.run("", "signup", "--username", name); r.code == 0 || !strings.Contains(r.err, "choose another") {
			t.Errorf("signup as %s: exit %d: %s", name, r.code, r.err)
		}
	}
	if r := w.run("", "signup"); r.code != cli.ExitUsage {
		t.Errorf("signup without a username: exit %d: %s", r.code, r.err)
	}

	if out := w.must("", "signup", "--username", "carol", "--display-name", "Carol"); out != "signed up as carol\n" {
		t.Errorf("signup printed %q", out)
	}
	if out := w.must("", "whoami"); out != "user:carol@example.com\nusername: carol\ndisplay name: Carol\n" {
		t.Errorf("whoami after signing up: %q", out)
	}
	w.must("", "create", "tenant", "--display-name", "Carol's lab")
	if r := w.run("", "signup", "--username", "carol"); r.code == 0 {
		t.Error("signed up twice")
	}
}

// TestUsersForRoot: platform.root lists users and sets a tenant cap with
// get and apply, which leaves the person's display name as it was; a member
// reads none.
func TestUsersForRoot(t *testing.T) {
	w := newWorld(t)
	accounts := iamv1alpha1connect.NewAccountServiceClient(apiservertest.DevUser("alice@example.com"), w.url)
	if _, err := accounts.UpdateProfile(t.Context(), connect.NewRequest(&iamv1alpha1.UpdateProfileRequest{DisplayName: "Alice L."})); err != nil {
		t.Fatal(err)
	}
	w.as("root@example.com")
	out := w.must("", "get", "users", "-o", "name")
	for _, want := range []string{"user/root-example-com", "user/alice-example-com", "user/bob-example-com"} {
		if !strings.Contains(out, want) {
			t.Errorf("get users as root: %q lacks %s", out, want)
		}
	}
	w.must(`apiVersion: iam.hangarlab.cloud/v1alpha1
kind: User
metadata: {name: alice-example-com}
spec: {email: alice@example.com, maxOwnedTenants: 3}
`, "apply", "-f", "-")
	if got := w.must("", "get", "user", "alice-example-com", "-o", "json"); !strings.Contains(got, `"maxOwnedTenants": 3`) ||
		!strings.Contains(got, `"displayName": "Alice L."`) {
		t.Errorf("the cap was not set, or the display name not kept: %s", got)
	}
	renamed := w.run(`apiVersion: iam.hangarlab.cloud/v1alpha1
kind: User
metadata: {name: alice-example-com}
spec: {email: alice@example.com, displayName: Mallory, maxOwnedTenants: 3}
`, "apply", "-f", "-")
	if renamed.code == 0 || !strings.Contains(renamed.err, "UpdateProfile") {
		t.Errorf("root renaming alice: exit %d: %s", renamed.code, renamed.err)
	}
	w.as("alice@example.com")
	if r := w.run("", "get", "user", "alice-example-com"); r.code == 0 {
		t.Error("a member read a user")
	}
}
