package cli_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/cli"
)

func TestCreateSuggestsAFreeName(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")

	out := w.must("", "create", "tenant", "--display-name", "Alice's Lab!", "-o", "name")
	tenant := strings.TrimPrefix(strings.TrimSpace(out), "tenant/")
	if !regexp.MustCompile(`^alice-s-lab-[0-9]{6}$`).MatchString(tenant) {
		t.Fatalf("suggested tenant name %q", tenant)
	}
	if got := w.must("", "get", "tenant", tenant, "-o", "json"); !strings.Contains(got, `"displayName": "Alice's Lab!"`) {
		t.Errorf("the display name was not kept: %s", got)
	}

	out = w.must("", "create", "project", "--tenant", tenant, "--display-name", "我的 demo")
	if !regexp.MustCompile(`^project/demo-[0-9]{6} created\n$`).MatchString(out) {
		t.Errorf("create project: %q", out)
	}
	// Nothing in the display name a name can hold: the kind stands in.
	out = w.must("", "create", "project", "--tenant", tenant, "--display-name", "我的", "-o", "name")
	if !regexp.MustCompile(`^project/project-[0-9]{6}\n$`).MatchString(out) {
		t.Errorf("create project from a display name with no ASCII: %q", out)
	}
}

func TestCreateRefusesATakenOrUsedName(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	w.must("", "create", "tenant", "lab", "--display-name", "Lab")
	w.must("", "create", "project", "gone", "--tenant", "lab", "--display-name", "Gone")
	w.must("", "delete", "project", "gone")

	w.as("bob@example.com")
	for _, args := range [][]string{
		{"create", "tenant", "lab", "--display-name", "Mine"},
		{"create", "project", "gone", "--tenant", "lab", "--display-name", "Mine"},
	} {
		r := w.run("", args...)
		if r.code == 0 || !strings.Contains(r.err, "never reused") {
			t.Errorf("hangar %s: exit %d: %s", strings.Join(args, " "), r.code, r.err)
		}
		// What is refused says nothing about who holds the name.
		if strings.Contains(r.err, "alice") {
			t.Errorf("the refusal names the holder: %s", r.err)
		}
	}
	if r := w.run("", "create", "tenant", "fresh", "--display-name", "Fresh"); r.code != 0 {
		t.Errorf("a free name: exit %d: %s", r.code, r.err)
	}
}

func TestCreateUsage(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	for _, args := range [][]string{
		{"create", "tenant"},
		{"create", "project", "--display-name", "No tenant"},
		{"create", "tenant", "--tenant", "x", "--display-name", "X"},
		{"create", "container", "web", "--display-name", "Web"},
	} {
		if r := w.run("", args...); r.code != cli.ExitUsage {
			t.Errorf("hangar %s: exit %d, want %d: %s", strings.Join(args, " "), r.code, cli.ExitUsage, r.err)
		}
	}
}

func TestCheckNameNeedsMembership(t *testing.T) {
	w := newWorld(t)
	w.as("stranger@example.com")
	r := w.run("", "create", "tenant", "--display-name", "Mine")
	if r.code == 0 || !strings.Contains(r.err, "not a platform member") {
		t.Errorf("a non-member: exit %d: %s", r.code, r.err)
	}
}
