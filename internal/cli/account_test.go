package cli_test

import (
	"encoding/json"
	"regexp"
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

	out := w.must("", "signup", "--username", "carol", "--display-name", "Carol")
	m := regexp.MustCompile(`^signed up as carol\nopened tenant (carol-[a-z2-9]{5}) with project (carol-[a-z2-9]{5})\nthe context now uses project (carol-[a-z2-9]{5})\n$`).FindStringSubmatch(out)
	if m == nil || m[1] != m[2] || m[2] != m[3] {
		t.Fatalf("signup printed %q", out)
	}
	first := m[1]
	// The context is at the new project, which is ready to use: its
	// default network is there, and the person owns the tenant.
	if got := w.must("", "get", "networks", "-o", "name"); got != "network/default\n" {
		t.Errorf("the networks of the context's project after signing up: %q", got)
	}
	if got := w.must("", "get", "tenant", first, "-o", "json"); !strings.Contains(got, `"displayName": "carol"`) {
		t.Errorf("the tenant signing up opened: %s", got)
	}
	if got := w.must("", "get", "project", first, "-o", "json"); !strings.Contains(got, `"displayName": "Default"`) {
		t.Errorf("the project signing up made: %s", got)
	}
	if out := w.must("", "whoami"); out != "user:carol@example.com\nusername: carol\ndisplay name: Carol\n" {
		t.Errorf("whoami after signing up: %q", out)
	}
	w.must("", "create", "tenant", "--display-name", "Carol's lab")
	if r := w.run("", "signup", "--username", "carol"); r.code == 0 {
		t.Error("signed up twice")
	}
}

// TestInvitations: platform.root invites a person with a plan and a tenant
// cap, sees the invitation until it is taken up, and the person's User
// starts with it; an invitation not taken up can be revoked, one taken up
// cannot; inviting someone who signed up before binds the role again and
// nothing else; and no one else invites.
func TestInvitations(t *testing.T) {
	w := newWorld(t)
	w.as("root@example.com")
	w.must(`apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Plan
metadata: {name: sponsor}
spec: {displayName: Sponsor, limits: {cpu: "32"}}
`, "apply", "-f", "-")
	if r := w.run("", "admin", "invite", "dave@example.com", "--plan", "missing"); r.code == 0 || !strings.Contains(r.err, `plan "missing" does not exist`) {
		t.Errorf("inviting with a plan that does not exist: exit %d: %s", r.code, r.err)
	}
	for _, cmd := range []string{"invite", "revoke"} {
		if r := w.run("", "admin", cmd, "\u212aate@example.com"); r.code != cli.ExitUsage || !strings.Contains(r.err, "must be ASCII") {
			t.Errorf("admin %s of a non-ASCII email: %d %s", cmd, r.code, r.err)
		}
	}
	if r := w.run("", "admin", "invite", "dave@example.com", "--max-tenants", "0"); r.code != cli.ExitUsage {
		t.Errorf("inviting with a cap of 0: exit %d: %s", r.code, r.err)
	}
	if out := w.must("", "admin", "invite", "Dave@Example.com", "--plan", "sponsor", "--max-tenants", "2"); !strings.Contains(out, "invited dave@example.com as platform.member, plan sponsor, tenants 2") {
		t.Errorf("invite printed %q", out)
	}
	w.must("", "admin", "invite", "erin@example.com")
	out := w.must("", "admin", "invitations")
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)^EMAIL +ROLE +PLAN +MAX TENANTS +AGE +NOTE$`),
		regexp.MustCompile(`(?m)^dave@example\.com +platform\.member +sponsor +2 +\S+ *$`),
		regexp.MustCompile(`(?m)^erin@example\.com +platform\.member +default +default +\S+ *$`),
	} {
		if !want.MatchString(out) {
			t.Errorf("admin invitations: %q does not match %s", out, want)
		}
	}

	w.as("dave@example.com")
	signedUp := w.must("", "signup", "--username", "dave")
	tenant := regexp.MustCompile(`opened tenant (dave-[a-z2-9]{5})`).FindStringSubmatch(signedUp)
	if tenant == nil {
		t.Fatalf("signup printed %q", signedUp)
	}
	// The tenant signing up opened has the invitation's plan, and counts
	// toward the cap of 2.
	if got := w.must("", "get", "tenant", tenant[1], "-o", "json"); !strings.Contains(got, `"plan": "sponsor"`) {
		t.Errorf("the tenant signing up opened has not the invited plan: %s", got)
	}
	w.must("", "create", "tenant", "--display-name", "Dave's second")
	if r := w.run("", "create", "tenant", "--display-name", "Dave's third"); r.code == 0 || !strings.Contains(r.err, "may own 2 (set on their User)") {
		t.Errorf("a third tenant under a cap of 2: exit %d: %s", r.code, r.err)
	}

	w.as("root@example.com")
	if got := w.must("", "get", "user", "dave", "-o", "json"); !strings.Contains(got, `"plan": "sponsor"`) || !strings.Contains(got, `"maxOwnedTenants": 2`) {
		t.Errorf("dave's User did not start with the invitation: %s", got)
	}
	if out := w.must("", "admin", "invitations", "-o", "name"); out != "invitation/erin@example.com\n" {
		t.Errorf("invitations once dave signed up: %q", out)
	}
	if r := w.run("", "admin", "revoke", "dave@example.com"); r.code == 0 || !strings.Contains(r.err, "has signed up, as dave") {
		t.Errorf("revoking an invitation taken up: exit %d: %s", r.code, r.err)
	}
	if r := w.run("", "admin", "invite", "dave@example.com", "--plan", "sponsor"); r.code == 0 || !strings.Contains(r.err, "signed up before, as dave") {
		t.Errorf("inviting with a plan someone who signed up: exit %d: %s", r.code, r.err)
	}
	out = w.must("", "admin", "invite", "dave@example.com")
	held := regexp.MustCompile(`^dave@example\.com already holds platform\.member through rolebinding (\S+)\n$`).FindStringSubmatch(out)
	if held == nil {
		t.Fatalf("inviting a member again: %q", out)
	}
	// Removed, then invited again.
	w.must("", "delete", "rolebinding", held[1])
	if out := w.must("", "admin", "invite", "dave@example.com"); out != "dave@example.com signed up before, as dave: bound platform.member again\n" {
		t.Errorf("inviting again someone who signed up and was removed: %q", out)
	}

	if out := w.must("", "admin", "revoke", "erin@example.com"); !regexp.MustCompile(`^deleted rolebinding invite-erin-example-com-[0-9a-f]{8} \(platform\.member\)\n` +
		`deleted invitation erin@example\.com\nrevoked the invitation of erin@example\.com\n$`).MatchString(out) {
		t.Errorf("revoke printed %q", out)
	}
	if r := w.run("", "admin", "revoke", "erin@example.com"); r.code != cli.ExitNotFound {
		t.Errorf("revoking again: exit %d: %s", r.code, r.err)
	}
	if out := w.must("", "admin", "invitations", "-o", "name"); out != "" {
		t.Errorf("invitations once erin's was revoked: %q", out)
	}
	w.as("erin@example.com")
	if r := w.run("", "signup", "--username", "erin"); r.code == 0 || !strings.Contains(r.err, "not a platform member") {
		t.Errorf("signing up once the invitation was revoked: exit %d: %s", r.code, r.err)
	}

	w.as("alice@example.com")
	if r := w.run("", "admin", "invite", "mallory@example.com"); r.code == 0 || !strings.Contains(r.err, "may not") {
		t.Errorf("a member inviting: exit %d: %s", r.code, r.err)
	}
	w.as("root@example.com")
	w.must("", "admin", "invite", "ivy@example.com")
	w.as("alice@example.com")
	// A list shows what the caller may read, which of invitations is none.
	if out := w.must("", "admin", "invitations", "-o", "name"); out != "" {
		t.Errorf("a member listing invitations: %q", out)
	}
}

// TestInvitationsOfHandBoundPeople: someone invited by a platform binding
// applied by hand shows as invited, starting with the defaults; inviting
// them gives them an Invitation but no second binding, and refuses another
// role; revoking deletes every platform binding naming them, whatever its
// name. An Invitation with no binding shows too.
func TestInvitationsOfHandBoundPeople(t *testing.T) {
	w := newWorld(t)
	w.invite("platform.member", "frank@example.com")
	w.as("root@example.com")
	w.must(`apiVersion: iam.hangarlab.cloud/v1alpha1
kind: RoleBinding
metadata: {name: frank-again}
spec: {subject: {user: frank@example.com}, role: platform.member, scope: {platform: {}}}
---
apiVersion: iam.hangarlab.cloud/v1alpha1
kind: Invitation
metadata: {name: gina@example.com}
spec: {maxOwnedTenants: 2}
`, "apply", "-f", "-")
	out := w.must("", "admin", "invitations")
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)^frank@example\.com +platform\.member,platform\.member +default +default +\S+ +no invitation: starts with the defaults$`),
		regexp.MustCompile(`(?m)^gina@example\.com +- +default +2 +\S+ +no platform role: cannot sign up$`),
	} {
		if !want.MatchString(out) {
			t.Errorf("admin invitations: %q does not match %s", out, want)
		}
	}
	// Members who signed up are not listed.
	if strings.Contains(out, "alice@example.com") {
		t.Errorf("admin invitations lists a member: %q", out)
	}

	platformBindingsOf := func(email string) int {
		t.Helper()
		var list struct {
			Items []struct {
				Spec struct {
					Subject struct{ User string }
					Scope   struct{ Platform *struct{} }
				}
			}
		}
		if err := json.Unmarshal([]byte(w.must("", "get", "rolebindings", "-o", "json")), &list); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, rb := range list.Items {
			if rb.Spec.Subject.User == email && rb.Spec.Scope.Platform != nil {
				n++
			}
		}
		return n
	}
	if out := w.must("", "admin", "invite", "frank@example.com", "--max-tenants", "3"); !strings.Contains(out, "frank@example.com already holds platform.member through rolebinding") {
		t.Errorf("inviting someone bound by hand printed %q", out)
	}
	if n := platformBindingsOf("frank@example.com"); n != 2 {
		t.Errorf("inviting someone bound by hand left %d platform bindings, want the 2 there were", n)
	}
	if r := w.run("", "admin", "invite", "frank@example.com", "--role", "platform.sponsor"); r.code == 0 || !strings.Contains(r.err, "already holds platform.member through rolebinding") {
		t.Errorf("inviting someone bound by hand to another role: exit %d: %s", r.code, r.err)
	}
	if out := w.must("", "admin", "invitations"); !regexp.MustCompile(`(?m)^frank@example\.com +platform\.member,platform\.member +default +3 +\S+ *$`).MatchString(out) {
		t.Errorf("admin invitations once frank has an Invitation: %q", out)
	}

	out = w.must("", "admin", "revoke", "frank@example.com")
	for _, want := range []string{
		"deleted rolebinding frank-again (platform.member)\n",
		"deleted rolebinding member-frank-example-com (platform.member)\n",
		"deleted invitation frank@example.com\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("revoke printed %q, without %q", out, want)
		}
	}
	if n := platformBindingsOf("frank@example.com"); n != 0 {
		t.Errorf("%d platform bindings of frank outlived revoking", n)
	}
	w.as("frank@example.com")
	if r := w.run("", "signup", "--username", "frank"); r.code == 0 || !strings.Contains(r.err, "not a platform member") {
		t.Errorf("frank signing up once revoked: exit %d: %s", r.code, r.err)
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
