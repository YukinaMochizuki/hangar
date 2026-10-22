package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/edge/front/acmetest"
)

const acmeDomain = `apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Tenant
metadata: {name: acme}
---
apiVersion: network.hangarlab.cloud/v1alpha1
kind: CustomDomain
metadata: {name: example-com}
spec: {tenant: acme, domain: example.com}
`

// checkWorld is a world whose apiserver looks the challenge record up at a
// DNS server of the test's, holding custom domain example-com of tenant
// acme, which alice owns, not yet verified.
func checkWorld(t *testing.T) (*world, *acmetest.Server) {
	t.Helper()
	dns, err := acmetest.Start("127.0.0.1:0", "example.com")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dns.Close)
	// Each test checks a domain again at once.
	w := newWorld(t, func(c *apiserver.Config) {
		c.PublicEndpoints.Resolvers = []string{dns.Addr}
		c.PublicEndpoints.CheckInterval = time.Nanosecond
	})
	w.as("alice@example.com")
	w.must(acmeDomain, "apply", "-f", "-")
	return w, dns
}

// TestCheckSaysWhyADomainIsNotVerifiedThenThatItIs: before the record,
// check prints the reason and exits 1; once it is in place, it prints
// verified and exits 0, and the domain as stored is verified.
func TestCheckSaysWhyADomainIsNotVerifiedThenThatItIs(t *testing.T) {
	w, dns := checkWorld(t)
	r := w.run("", "check", "customdomain", "example-com")
	if r.code != 1 || !strings.Contains(r.out, "customdomain/example-com not verified (ChallengeRecordMissing): _acme-challenge.example.com must be a CNAME of ") {
		t.Fatalf("before the record: exit %d, %q, %q", r.code, r.out, r.err)
	}
	var d struct {
		Status struct {
			ChallengeTarget string `json:"challengeTarget"`
		} `json:"status"`
	}
	r = w.run("", "check", "cdom", "example-com", "-o", "json")
	if err := json.Unmarshal([]byte(r.out), &d); r.code != 1 || err != nil || d.Status.ChallengeTarget == "" {
		t.Fatalf("-o json before the record: exit %d, %q, %v", r.code, r.out, err)
	}
	dns.SetCNAME("_acme-challenge.example.com", d.Status.ChallengeTarget)
	r = w.run("", "check", "cdom", "example-com")
	if r.code != 0 || !strings.HasPrefix(r.out, "customdomain/example-com verified: the challenge CNAME is in place") {
		t.Fatalf("with the record: exit %d, %q, %q", r.code, r.out, r.err)
	}
	if out := w.must("", "get", "cdom", "example-com", "-o", "json"); !strings.Contains(out, `"reason": "Verified"`) {
		t.Errorf("the domain as stored: %s", out)
	}
}

// TestCheckWaitReturnsOnceVerified: --wait asks until the domain is
// verified, here at once, and a short timeout on one that never is ends
// with exit 5, saying what it last found.
func TestCheckWaitReturnsOnceVerified(t *testing.T) {
	w, dns := checkWorld(t)
	r := w.run("", "check", "cdom", "example-com", "--wait", "--timeout=1s")
	if r.code != 5 || !strings.Contains(r.out, "not verified (ChallengeRecordMissing)") {
		t.Fatalf("--wait on a domain never verified: exit %d, %q, %q", r.code, r.out, r.err)
	}
	var d struct {
		Status struct {
			ChallengeTarget string `json:"challengeTarget"`
		} `json:"status"`
	}
	_ = json.Unmarshal([]byte(w.run("", "get", "cdom", "example-com", "-o", "json").out), &d)
	dns.SetCNAME("_acme-challenge.example.com", d.Status.ChallengeTarget)
	if r := w.run("", "check", "cdom", "example-com", "--wait", "-o", "name"); r.code != 0 || r.out != "customdomain/example-com\n" {
		t.Errorf("--wait with the record: exit %d, %q, %q", r.code, r.out, r.err)
	}
}

// TestCheckRefusals: check takes custom domains alone, no project, and
// what changing the domain takes.
func TestCheckRefusals(t *testing.T) {
	w, _ := checkWorld(t)
	for _, c := range []struct {
		args []string
		code int
		says string
	}{
		{[]string{"check", "container", "api"}, 2, "check works on custom domains, not containers"},
		{[]string{"check", "cdom", "example-com", "-p", "web"}, 2, "are not in projects"},
		{[]string{"check", "cdom", "example-com", "-o", "yaml"}, 2, "check prints name or json"},
		{[]string{"check", "cdom"}, 2, "usage"},
		{[]string{"check", "cdom", "example-com", "--timeout=1m"}, 2, "--timeout is how long --wait waits"},
	} {
		if r := w.run("", c.args...); r.code != c.code || !strings.Contains(r.err, c.says) {
			t.Errorf("hangar %s: exit %d, %q; want %d saying %q", strings.Join(c.args, " "), r.code, r.err, c.code, c.says)
		}
	}
	w.as("bob@example.com")
	if r := w.run("", "check", "cdom", "example-com"); r.code == 0 || !strings.Contains(r.err, "may not apply customdomains") {
		t.Errorf("someone of another tenant: exit %d, %q", r.code, r.err)
	}
}

// TestCheckWaitStopsWhenAnotherTenantHoldsTheName: bob's tenant verifies
// example.com first; alice's --wait, which no record of hers can help,
// stops at once with exit 1 and says why.
func TestCheckWaitStopsWhenAnotherTenantHoldsTheName(t *testing.T) {
	w, dns := checkWorld(t)
	w.as("bob@example.com")
	w.must(`apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Tenant
metadata: {name: bobco}
---
apiVersion: network.hangarlab.cloud/v1alpha1
kind: CustomDomain
metadata: {name: bob-example-com}
spec: {tenant: bobco, domain: example.com}
`, "apply", "-f", "-")
	var d struct {
		Status struct {
			ChallengeTarget string `json:"challengeTarget"`
		} `json:"status"`
	}
	r := w.run("", "check", "cdom", "bob-example-com", "-o", "json")
	if err := json.Unmarshal([]byte(r.out), &d); err != nil || d.Status.ChallengeTarget == "" {
		t.Fatalf("bob's check: exit %d, %q, %v", r.code, r.out, err)
	}
	dns.SetCNAME("_acme-challenge.example.com", d.Status.ChallengeTarget)
	if r := w.run("", "check", "cdom", "bob-example-com"); r.code != 0 {
		t.Fatalf("bob's check with his record: exit %d, %q, %q", r.code, r.out, r.err)
	}
	w.as("alice@example.com")
	start := time.Now()
	r = w.run("", "check", "cdom", "example-com", "--wait", "--timeout=3s")
	if r.code != 1 || !strings.Contains(r.out, "not verified (HeldByAnotherTenant)") {
		t.Errorf("alice's --wait: exit %d, %q, %q", r.code, r.out, r.err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("alice's --wait took %s", took)
	}
}
