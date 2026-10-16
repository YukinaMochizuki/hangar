package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/cloudflare/cftest"
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
	f.AddZone("hangar-user.example")
	t.Cleanup(f.Close)
	dir := t.TempDir()
	config := filepath.Join(dir, "cloudflare.yaml")
	err := os.WriteFile(config, []byte(`account_id: `+account+`
zone: hangar.example
addresses: {ipv4: 192.0.2.10, ipv6: "2001:db8:1::1"}
endpoints: {zone: hangar-user.example}
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

func TestPlanApplyVerify(t *testing.T) {
	h := newHarness(t)

	code, out, errOut := h.run("plan", nil)
	if code != exitOK || !strings.Contains(out, "Plan: 6 change(s).") || !strings.Contains(out, "+ create DNS AAAA record console.hangar.example") {
		t.Fatalf("plan: %d\n%s%s", code, out, errOut)
	}
	if len(h.f.Writes()) != 0 {
		t.Fatalf("plan wrote: %v", h.f.Writes())
	}
	if _, err := os.Stat(h.out); !os.IsNotExist(err) {
		t.Fatal("plan created the output directory")
	}
	if code, out, errOut = h.run("verify", nil); code != exitDrift {
		t.Fatalf("verify before apply: %d\n%s%s", code, out, errOut)
	}
	if code, out, errOut = h.run("apply", nil); code != exitOK || !strings.Contains(out, "Applied 6 change(s).") {
		t.Fatalf("apply: %d\n%s%s", code, out, errOut)
	}
	if code, out, errOut = h.run("apply", nil); code != exitOK || !strings.Contains(out, "No changes.") {
		t.Fatalf("second apply: %d\n%s%s", code, out, errOut)
	}
	if code, out, errOut = h.run("verify", nil); code != exitOK || !strings.Contains(out, "No changes.") {
		t.Fatalf("verify: %d\n%s%s", code, out, errOut)
	}

	// Someone turns the proxy on in the dashboard: verify fails and says so.
	h.f.Mutate(func(st *cftest.State) {
		for _, d := range st.DNS {
			if d.Name == "api.hangar.example" && d.Type == "A" {
				d.Proxied = true
			}
		}
	})
	code, out, errOut = h.run("verify", nil)
	if code != exitDrift || !strings.Contains(out, "~ update DNS A record api.hangar.example") || !strings.Contains(out, "proxied: true\n      -> false") {
		t.Fatalf("verify with drift: %d\n%s%s", code, out, errOut)
	}
	if code, out, _ = h.run("apply", nil); code != exitOK {
		t.Fatalf("apply: %d %s", code, out)
	}
	if code, out, _ = h.run("verify", nil); code != exitOK {
		t.Fatalf("verify after fix: %d %s", code, out)
	}
}

func TestRefusalIsClearAndChangesNothing(t *testing.T) {
	h := newHarness(t)
	var zone string
	h.f.Mutate(func(st *cftest.State) { zone = st.Zones[0].ID })
	theirs := h.f.AddRecord(zone, cftest.DNSRecord{Type: "A", Name: "api.hangar.example", Content: "198.51.100.1"})
	code, out, errOut := h.run("apply", nil)
	if code != exitError || !strings.Contains(errOut, "refusing to change records hangar-cloudflare did not create") ||
		!strings.Contains(errOut, "DNS A record api.hangar.example (id "+theirs.ID+")") {
		t.Fatalf("apply: %d\n%s%s", code, out, errOut)
	}
	if len(h.f.Writes()) != 0 {
		t.Fatalf("writes: %v", h.f.Writes())
	}
}

func TestOldConfigIsRefused(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(h.config, []byte("account_id: "+account+"\nzone: hangar.example\nhostname: api.hangar.example\ntunnel_name: hangar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := h.run("plan", nil)
	if code != exitError || !strings.Contains(errOut, "the tunnel and Access keys are gone") {
		t.Fatalf("plan: %d\n%s%s", code, out, errOut)
	}
	if len(h.f.Requests()) != 0 {
		t.Fatal("a refused config reached the API")
	}
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
