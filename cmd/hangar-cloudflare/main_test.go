package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/cloudflare/cftest"
	"github.com/YukinaMochizuki/hangar/internal/cloudflare/converge"
)

const (
	apiToken = "cf-api-token-9d8c7b6a"
	account  = "0123456789abcdef0123456789abcdef"
)

type harness struct {
	t      *testing.T
	f      *cftest.Fake
	config string
	out    string
}

func newHarness(t *testing.T) *harness {
	f := cftest.New(apiToken, account, "hangar.example")
	t.Cleanup(f.Close)
	dir := t.TempDir()
	config := filepath.Join(dir, "cloudflare.yaml")
	err := os.WriteFile(config, []byte(`account_id: `+account+`
zone: hangar.example
hostname: api.hangar.example
tunnel_name: hangar
allowed_emails: [owner@example.com]
identity_providers:
  google: Google
  one_time_pin: One-time PIN
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, f: f, config: config, out: filepath.Join(dir, "out")}
}

func (h *harness) run(mode string, env map[string]string) (code int, stdout, stderr string) {
	var o, e bytes.Buffer
	if env == nil {
		env = map[string]string{"CLOUDFLARE_API_TOKEN": apiToken}
	}
	args := []string{mode, "--config", h.config, "--out", h.out, "--api-url", h.f.URL()}
	code = run(context.Background(), args, func(k string) string { return env[k] }, &o, &e)
	return code, o.String(), e.String()
}

// secrets returns every credential the fake holds or was given.
func (h *harness) secrets() []string {
	s := []string{apiToken}
	h.f.Mutate(func(st *cftest.State) {
		for _, t := range st.Tunnels {
			s = append(s, t.Token)
		}
		for _, t := range st.ServiceTokens {
			s = append(s, t.Secret)
		}
	})
	return s
}

func (h *harness) noSecrets(outputs ...string) {
	h.t.Helper()
	for _, s := range h.secrets() {
		for _, o := range outputs {
			if strings.Contains(o, s) {
				h.t.Fatalf("output contains a secret:\n%s", o)
			}
		}
	}
}

func TestPlanApplyVerify(t *testing.T) {
	h := newHarness(t)

	code, out, errOut := h.run("plan", nil)
	if code != exitOK || !strings.Contains(out, "Plan: 10 change(s).") {
		t.Fatalf("plan: %d\n%s%s", code, out, errOut)
	}
	if len(h.f.Writes()) != 0 {
		t.Fatalf("plan wrote: %v", h.f.Writes())
	}
	if _, err := os.Stat(h.out); !os.IsNotExist(err) {
		t.Fatal("plan created the output directory")
	}

	code, out, errOut = h.run("verify", nil)
	if code != exitDrift {
		t.Fatalf("verify before apply: %d\n%s%s", code, out, errOut)
	}

	code, out, errOut = h.run("apply", nil)
	if code != exitOK || !strings.Contains(out, "Applied 10 change(s).") {
		t.Fatalf("apply: %d\n%s%s", code, out, errOut)
	}
	h.noSecrets(out, errOut)
	b, err := os.ReadFile(filepath.Join(h.out, converge.SmokeTokenEnv))
	if err != nil || !strings.Contains(string(b), "HANGAR_CF_CLIENT_SECRET=") {
		t.Fatalf("smoke token file: %q %v", b, err)
	}

	code, out, errOut = h.run("apply", nil)
	if code != exitOK || !strings.Contains(out, "No changes.") {
		t.Fatalf("second apply: %d\n%s%s", code, out, errOut)
	}
	code, out, errOut = h.run("verify", nil)
	if code != exitOK || !strings.Contains(out, "No changes.") {
		t.Fatalf("verify: %d\n%s%s", code, out, errOut)
	}
	h.noSecrets(out, errOut)

	// Drift in the tunnel token output and the ingress: verify fails and
	// prints neither token.
	if err := os.WriteFile(filepath.Join(h.out, converge.CloudflaredEnv), []byte("TUNNEL_TOKEN=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.f.Mutate(func(st *cftest.State) {
		for _, c := range st.Configs {
			c.Config.Ingress[0].Service = "http://127.0.0.1:1"
		}
	})
	code, out, errOut = h.run("verify", nil)
	if code != exitDrift || !strings.Contains(out, "> write output cloudflared.env") || !strings.Contains(out, "~ update tunnel \"hangar\" configuration") {
		t.Fatalf("verify with drift: %d\n%s%s", code, out, errOut)
	}
	h.noSecrets(out, errOut)
	if code, out, _ = h.run("apply", nil); code != exitOK {
		t.Fatalf("apply: %d %s", code, out)
	}
	if code, out, _ = h.run("verify", nil); code != exitOK {
		t.Fatalf("verify after fix: %d %s", code, out)
	}
}

func TestRefusalIsClearAndChangesNothing(t *testing.T) {
	h := newHarness(t)
	theirs := h.f.AddTunnel("hangar", "cloudflare")
	code, out, errOut := h.run("apply", nil)
	if code != exitError || !strings.Contains(errOut, "refusing to change objects hangar-cloudflare did not create") ||
		!strings.Contains(errOut, `tunnel "hangar" (id `+theirs.ID+")") {
		t.Fatalf("apply: %d\n%s%s", code, out, errOut)
	}
	if len(h.f.Writes()) != 0 {
		t.Fatalf("writes: %v", h.f.Writes())
	}
	h.noSecrets(out, errOut)
}

func TestUsage(t *testing.T) {
	h := newHarness(t)
	if code, _, errOut := h.run("apply", map[string]string{}); code != exitUsage || !strings.Contains(errOut, "CLOUDFLARE_API_TOKEN") {
		t.Fatalf("no token: %d %s", code, errOut)
	}
	var o, e bytes.Buffer
	if code := run(context.Background(), []string{"destroy"}, func(string) string { return apiToken }, &o, &e); code != exitUsage {
		t.Fatalf("unknown mode: %d", code)
	}
	if code := run(context.Background(), []string{"plan", "--config", h.config}, func(string) string { return apiToken }, &o, &e); code != exitUsage {
		t.Fatalf("missing --out: %d", code)
	}
	if len(h.f.Requests()) != 0 {
		t.Fatal("usage errors reached the API")
	}
}

func TestWrongTokenFailsWithoutEchoingIt(t *testing.T) {
	h := newHarness(t)
	code, out, errOut := h.run("plan", map[string]string{"CLOUDFLARE_API_TOKEN": "not-the-token-77"})
	if code != exitError || !strings.Contains(errOut, "HTTP 401") || strings.Contains(out+errOut, "not-the-token-77") {
		t.Fatalf("plan: %d\n%s%s", code, out, errOut)
	}
}
