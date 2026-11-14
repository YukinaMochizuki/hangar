package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/cli"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// limitsRow is the row of `hangar project limits` for one limit, its
// columns joined by single spaces.
func limitsRow(out, limit string) string {
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == limit {
			return strings.Join(f, " ")
		}
	}
	return ""
}

// TestProjectLimits: a tenant owner sets some of a project's own limits,
// changes one while keeping the rest, unsets them, and sees them beside
// the plan's, those in effect and the usage; a limit above the plan's, one
// only the plan sets, and a change by someone with a role in the project
// alone are refused.
func TestProjectLimits(t *testing.T) {
	w := newWorld(t, func(c *apiserver.Config) {
		c.DefaultPlanLimits = &tenancyv1alpha1.Limits{Cpu: proto.String("8"), Memory: proto.String("16Gi"), Machines: proto.Int32(5)}
	})
	w.as("alice@example.com")
	w.must(memberStack, "apply", "-f", "-")

	out := w.must("", "project", "set-limits", "web", "--cpu", "4", "--memory", "8Gi", "--machines", "3")
	if out != "project/web: limits cpu 4, memory 8Gi, machines 3; the rest follow its tenant's plan\n" {
		t.Errorf("set-limits: %q", out)
	}
	// The context's project, web, when no name is given; cpu and memory stay.
	if out := w.must("", "project", "set-limits", "--machines", "2", "--disk", "20Gi"); out != "project/web: limits cpu 4, memory 8Gi, disk 20Gi, machines 2; the rest follow its tenant's plan\n" {
		t.Errorf("set-limits keeping the rest: %q", out)
	}
	if out := w.must("", "project", "set-limits", "--unset", "memory,disk", "--unset", "machines"); out != "project/web: limits cpu 4; the rest follow its tenant's plan\n" {
		t.Errorf("set-limits --unset: %q", out)
	}

	testutil.Eventually(t, "the limits in effect written", func() (bool, string) {
		out := w.must("", "project", "limits", "web")
		return limitsRow(out, "cpu") == "cpu 4 8 4 0" && limitsRow(out, "memory") == "memory - 16Gi 16Gi 0" &&
			limitsRow(out, "machines") == "machines - 5 5 0" && limitsRow(out, "disk") == "disk - - - 0", out
	})
	var doc struct {
		Project  string         `json:"project"`
		Own      map[string]any `json:"own"`
		Plan     map[string]any `json:"plan"`
		InEffect map[string]any `json:"inEffect"`
		Usage    map[string]any `json:"usage"`
	}
	if err := json.Unmarshal([]byte(w.must("", "project", "limits", "-o", "json")), &doc); err != nil {
		t.Fatal(err)
	}
	// A count of zero is written, not left out.
	if doc.Project != "web" || doc.Own["cpu"] != "4" || len(doc.Own) != 1 || doc.Plan["cpu"] != "8" || doc.InEffect["cpu"] != "4" || doc.Usage["machines"] != 0.0 {
		t.Errorf("limits -o json: %+v", doc)
	}

	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"--cpu", "16"}, cli.ExitError, `spec.limits.cpu 16 is above tenant "acme"'s limit of 8`},
		{[]string{"--machines", "two"}, cli.ExitUsage, `--machines "two" is not a whole number`},
		{[]string{"--cpu", "2", "--unset", "cpu"}, cli.ExitUsage, "--cpu is both set and unset"},
		{[]string{"--unset", "projects"}, cli.ExitUsage, `--unset "projects" is no limit a project sets`},
		{[]string{"--unset", "all,disk"}, cli.ExitUsage, "--unset all unsets every limit; name no other"},
		{[]string{"--projects", "2"}, cli.ExitUsage, "unknown flag: --projects"},
		{nil, cli.ExitUsage, "set or unset at least one limit"},
	} {
		r := w.run("", append([]string{"project", "set-limits", "web"}, c.args...)...)
		if r.code != c.code || !strings.Contains(r.err, c.want) {
			t.Errorf("set-limits %v: exit %d %q, want exit %d and %q", c.args, r.code, r.err, c.code, c.want)
		}
	}

	// Bob owns the project but not the tenant: he may read the project's
	// limits, not the plan's, and may not raise his own budget.
	w.must("", "member", "add", "bob@example.com", "--role", "project.owner", "--project", "web")
	w.as("bob@example.com")
	out = w.must("", "project", "limits")
	if limitsRow(out, "cpu") != "cpu 4 ? 4 0" || !strings.Contains(out, `may not read tenant "acme"`) {
		t.Errorf("limits as a project owner:\n%s", out)
	}
	if r := w.run("", "project", "set-limits", "--cpu", "6"); r.code != cli.ExitError || !strings.Contains(r.err, "needs apply on projects at tenant") {
		t.Errorf("set-limits as a project owner: exit %d %q", r.code, r.err)
	}

	w.as("alice@example.com")
	if out := w.must("", "project", "set-limits", "--unset", "all"); out != "project/web: no limits of its own; its tenant's plan holds\n" {
		t.Errorf("set-limits --unset all: %q", out)
	}
}
