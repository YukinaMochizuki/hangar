package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

const cliCanary = "cli-canary-9Wd3"

// TestCreateRegistrySecret: a registry secret is made from a Docker config,
// from a file or stdin, never the command line, and no output of the CLI
// shows its value again.
func TestCreateRegistrySecret(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	w.must(stack, "apply", "-f", "-")

	cfg := `{"auths":{"https://index.docker.io/v1/":{"username":"bot","password":"` + cliCanary + `"},"ghcr.io":{"identitytoken":"` + cliCanary + `"}}}`
	file := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(file, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := w.must("", "create", "secret", "registry", "pull", "--from-docker-config", file); out != "secret/pull created, for 2 registries\n" {
		t.Errorf("create printed %q", out)
	}
	if out := w.must(cfg, "create", "secret", "registry", "pull", "--from-docker-config", "-"); out != "secret/pull applied, for 2 registries\n" {
		t.Errorf("create again from stdin printed %q", out)
	}
	var table string
	testutil.Eventually(t, "get secret listing both registries", func() (bool, string) {
		table = w.must("", "get", "secret")
		return strings.Contains(table, "ghcr.io,index.docker.io"), "\n" + table
	})
	if !strings.Contains(table, "REGISTRIES") || !strings.Contains(table, "ghcr.io,index.docker.io") {
		t.Errorf("get secret printed:\n%s", table)
	}
	var all strings.Builder
	all.WriteString(table)
	for _, args := range [][]string{
		{"get", "secret", "pull", "-o", "yaml"}, {"get", "secret", "pull", "-o", "json"}, {"get", "secrets", "-o", "yaml"},
	} {
		all.WriteString(w.must("", args...))
	}
	if strings.Contains(all.String(), cliCanary) || strings.Contains(all.String(), "sealed") {
		t.Errorf("the CLI showed the value or its sealed form:\n%s", all.String())
	}

	for _, c := range []struct {
		what  string
		stdin string
		args  []string
	}{
		{"no file", "", []string{"create", "secret", "registry", "x"}},
		{"not a Docker config", `{"auths":{"ghcr.io":{"password":"` + cliCanary + `"`, []string{"create", "secret", "registry", "x", "--from-docker-config", "-"}},
		{"a login without a username", `{"auths":{"ghcr.io":{"password":"` + cliCanary + `"}}}`, []string{"create", "secret", "registry", "x", "--from-docker-config", "-"}},
	} {
		r := w.run(c.stdin, c.args...)
		if r.code == 0 || strings.Contains(r.out+r.err, cliCanary) {
			t.Errorf("%s: exit %d, %q %q", c.what, r.code, r.out, r.err)
		}
	}
}
