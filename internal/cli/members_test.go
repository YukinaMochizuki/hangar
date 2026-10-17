package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const memberStack = `apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Tenant
metadata: {name: acme}
spec: {displayName: Acme}
---
apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Project
metadata: {name: web}
spec: {tenant: acme}
---
apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Project
metadata: {name: db}
spec: {tenant: acme, displayName: Database}
`

// TestMemberCommands: a tenant owner adds, lists, changes and removes a
// member of a project and of the tenant, and is told when the person is
// not a platform member yet.
func TestMemberCommands(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	w.must(memberStack, "apply", "-f", "-")

	r := w.run("", "member", "add", "bob@example.com", "--role", "project.editor", "--project", "web")
	if r.code != 0 || r.out != "added bob@example.com as project.editor in project/web\n" || r.err != "" {
		t.Errorf("member add bob: exit %d %q %q", r.code, r.out, r.err)
	}
	r = w.run("", "member", "add", "carol@example.com", "--role", "project.viewer", "--project", "web")
	if r.code != 0 || !strings.Contains(r.err, "warning: carol@example.com is not a platform member yet") {
		t.Errorf("member add carol, not a platform member: exit %d %q %q", r.code, r.out, r.err)
	}

	out := w.must("", "member", "list", "--project", "web")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 ||
		strings.Join(strings.Fields(lines[1]), " ") != "bob@example.com bob-example-com project.editor project/web yes" ||
		strings.Join(strings.Fields(lines[2]), " ") != "carol@example.com - project.viewer project/web no" {
		t.Errorf("member list:\n%s", out)
	}

	if out := w.must("", "member", "set-role", "bob@example.com", "--role", "project.viewer", "--project", "web"); out != "set bob@example.com as project.viewer in project/web\n" {
		t.Errorf("member set-role: %q", out)
	}
	if out := w.must("", "member", "remove", "carol@example.com", "--project", "web"); out != "removed carol@example.com as project.viewer from project/web\n" {
		t.Errorf("member remove: %q", out)
	}
	if out := w.must("", "member", "list", "--project", "web"); strings.Contains(out, "carol") || !strings.Contains(out, "project.viewer") {
		t.Errorf("member list after the changes:\n%s", out)
	}

	w.must("", "member", "add", "bob@example.com", "--role", "tenant.viewer", "--tenant", "acme")
	if out := w.must("", "member", "list", "--tenant", "acme"); !strings.Contains(out, "tenant.owner") || !strings.Contains(out, "tenant.viewer") {
		t.Errorf("member list --tenant:\n%s", out)
	}

	// Bob, a viewer, may not change who may.
	w.as("bob@example.com")
	if r := w.run("", "member", "add", "dave@example.com", "--role", "project.owner", "--project", "web"); r.code == 0 || !strings.Contains(r.err, "may not") {
		t.Errorf("a viewer adding a member: exit %d %q", r.code, r.err)
	}
	if r := w.run("", "member", "list", "--tenant", "acme", "--project", "web"); r.code == 0 {
		t.Errorf("member list with both --tenant and --project: exit 0")
	}
}

// TestProjectCommands: a person lists the projects they reach and switches
// the context to one of them, and only to one they reach.
func TestProjectCommands(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	w.must(memberStack, "apply", "-f", "-")
	w.must("", "member", "add", "bob@example.com", "--role", "project.viewer", "--project", "db")

	w.as("bob@example.com")
	// rows are the project list's rows, but its header, by project name.
	rows := func(out string) map[string][]string {
		t.Helper()
		byName := map[string][]string{}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
			f := strings.Fields(line)
			name := f[0]
			if name == "*" {
				name = f[1]
			}
			byName[name] = f
		}
		return byName
	}
	// Bob reaches db, which he was granted, and the project signing up
	// opened for him, named after him and in the tenant of its name; not
	// web.
	out := w.must("", "project", "list")
	listed := rows(out)
	var own string
	for name, f := range listed {
		if strings.HasPrefix(name, "bob-") && len(f) >= 2 && f[1] == name {
			own = name
		}
	}
	if len(listed) != 2 || listed["db"] == nil || !strings.Contains(out, "Database") || own == "" {
		t.Errorf("bob's project list, want db and his own project only:\n%s", out)
	}
	if r := w.run("", "project", "use", "web"); r.code == 0 {
		t.Errorf("switching to a project bob does not reach: exit 0, %q", r.out)
	}
	if r := w.run("", "--context", "nope", "project", "use", "db"); r.code == 0 || !strings.Contains(r.err, `no context named "nope"`) {
		t.Errorf("switching the project of a context that does not exist: exit %d %q", r.code, r.err)
	}
	// project use writes the config, so it looks after the directory as
	// every command that writes it does.
	if err := os.Chmod(filepath.Dir(w.config), 0o755); err != nil {
		t.Fatal(err)
	}
	if r := w.run("", "project", "use", "db"); r.code != 0 || !strings.Contains(r.err, "warning: ") || !strings.Contains(r.err, "others can see") {
		t.Errorf("project use with a config directory others may read: exit %d %q", r.code, r.err)
	}
	out = w.must("", "project", "list")
	listed = rows(out)
	if f := listed["db"]; len(f) < 2 || f[0] != "*" {
		t.Errorf("the project list does not mark db as the context's:\n%s", out)
	}
	if f := listed[own]; len(f) == 0 || f[0] == "*" {
		t.Errorf("the project list marks bob's own project, not db, or leaves it out:\n%s", out)
	}
	if view := w.must("", "config", "view", "-o", "json"); !strings.Contains(view, `"project":"db"`) && !strings.Contains(view, `"project": "db"`) {
		t.Errorf("the context does not use db:\n%s", view)
	}
}
