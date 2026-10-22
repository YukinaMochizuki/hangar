package apiserver

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/customdomain"
	"github.com/YukinaMochizuki/hangar/internal/edge/front/acmetest"
)

func customDomain(name, tenant, domain string, projects ...string) *networkv1alpha1.CustomDomain {
	return &networkv1alpha1.CustomDomain{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec:     &networkv1alpha1.CustomDomainSpec{Tenant: tenant, Domain: domain, Projects: projects},
	}
}

// publicAt is a PUBLIC endpoint of project web at hostnames.
func publicAt(name string, hostnames ...string) *networkv1alpha1.PublicEndpoint {
	ep := endpoint(name, "api", 80, public)
	ep.Spec.Hostnames = hostnames
	return ep
}

// otherTenant makes bob a member who owns tenant "other" with project
// "shop", and returns a context acting as him.
func otherTenant(t *testing.T, s *Server) context.Context {
	t.Helper()
	addMember(t, s, "bob@example.com")
	bob := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: "bob@example.com"})
	if _, err := s.backend(registry.Tenant).Apply(bob, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "other"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Project).Apply(bob, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "shop"}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "other"},
	}); err != nil {
		t.Fatal(err)
	}
	return bob
}

func TestCustomDomainAdmission(t *testing.T) {
	s, system, user := runningWith(t, func(c *Config) { c.PublicEndpoints.ControlPlaneDomain = "console.hangarlab.cloud" })
	bob := otherTenant(t, s)
	b := s.backend(registry.CustomDomain)
	if _, err := b.Apply(user, customDomain("example", "acme", "example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply(user, customDomain("b-example", "acme", "b.example.com", "web")); err != nil {
		t.Errorf("a domain under the tenant's own: %v", err)
	}
	for what, d := range map[string]*networkv1alpha1.CustomDomain{
		"no domain":                customDomain("a", "acme", ""),
		"a public suffix":          customDomain("a", "acme", "co.uk"),
		"upper case":               customDomain("a", "acme", "Example.org"),
		"a trailing dot":           customDomain("a", "acme", "example.org."),
		"a wildcard":               customDomain("a", "acme", "*.example.org"),
		"the endpoints' domain":    customDomain("a", "acme", "x.hangarlab.dev"),
		"the control plane's site": customDomain("a", "acme", "hangarlab.cloud"),
		"a project named twice":    customDomain("a", "acme", "a.org", "web", "web"),
		"a project not a name":     customDomain("a", "acme", "a.org", "Web"),
		"a label not a DNS label":  customDomain("a", "acme", "a_b.org"),
		"a name longer than DNS's": customDomain("a", "acme", strings.Repeat("a.", 127)+"org"),
	} {
		_, err := b.Apply(user, d)
		wantCodeOf(t, err, connect.CodeInvalidArgument, what)
	}
	for what, d := range map[string]*networkv1alpha1.CustomDomain{
		"a project of another tenant":     customDomain("a", "acme", "a.org", "shop"),
		"a project that does not exist":   customDomain("a", "acme", "a.org", "nowhere"),
		"a domain the tenant has already": customDomain("again", "acme", "example.com"),
	} {
		_, err := b.Apply(user, d)
		wantCodeOf(t, err, connect.CodeFailedPrecondition, what)
	}
	// Only someone who may write at the platform names no tenant, or one
	// that is not there.
	_, err := b.Apply(system, customDomain("a", "", "a.org"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "no tenant")
	_, err = b.Apply(system, customDomain("a", "nobody", "a.org"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a tenant that does not exist")
	_, err = b.Apply(user, customDomain("a", "nobody", "a.org"))
	wantCodeOf(t, err, connect.CodePermissionDenied, "a tenant not the caller's")

	// A domain not verified keeps no one from a name: another tenant may
	// have it, one under it or one above it, too. (acme has example.com and
	// b.example.com, neither verified; example.com is above b.example.com.)
	for what, d := range map[string]*networkv1alpha1.CustomDomain{
		"the same as, and above, another tenant's domains, not verified": customDomain("same", "other", "example.com"),
		"under another tenant's domain, not verified":                    customDomain("under", "other", "shop.example.com"),
	} {
		if _, err := b.Apply(bob, d); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	// One verified keeps every other tenant from it, from under it and from
	// above it, made after.
	if _, err := b.Apply(user, customDomain("c-org", "acme", "c.example.org")); err != nil {
		t.Fatal(err)
	}
	markVerified(t, s, system, "b-example")
	markVerified(t, s, system, "c-org")
	for what, d := range map[string]*networkv1alpha1.CustomDomain{
		"a domain another tenant verified":       customDomain("t1", "other", "b.example.com"),
		"under a domain another tenant verified": customDomain("t2", "other", "x.b.example.com"),
		"above a domain another tenant verified": customDomain("t3", "other", "example.org"),
	} {
		_, err := b.Apply(bob, d)
		wantCodeOf(t, err, connect.CodeFailedPrecondition, what)
		if err != nil && !strings.Contains(err.Error(), "another tenant has verified") {
			t.Errorf("%s: refused for another reason: %v", what, err)
		}
	}
	// One made before stays its tenant's to change.
	if _, err := b.Apply(bob, customDomain("same", "other", "example.com", "shop")); err != nil {
		t.Errorf("changing a domain made before another tenant verified a name under it: %v", err)
	}
	if _, err := b.Apply(bob, customDomain("theirs", "other", "notexample.com")); err != nil {
		t.Errorf("a domain that only ends alike: %v", err)
	}
	// The domain and the tenant never change.
	moved := customDomain("example", "acme", "example.org")
	_, err = b.Apply(user, moved)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a domain changed")
}

// TestCustomDomainsAreTheTenants: only those who may write at the tenant
// write its domains; another tenant's owner neither reads nor writes them,
// nor names a tenant of someone else's.
func TestCustomDomainsAreTheTenants(t *testing.T) {
	s, _, user := running(t)
	bob := otherTenant(t, s)
	b := s.backend(registry.CustomDomain)
	if _, err := b.Apply(user, customDomain("example", "acme", "example.com")); err != nil {
		t.Fatal(err)
	}
	_, err := b.Apply(bob, customDomain("mine", "acme", "bob.org"))
	wantCodeOf(t, err, connect.CodePermissionDenied, "a domain for another's tenant")
	_, err = b.Get(bob, resource.Key{Name: "example"})
	if code := connect.CodeOf(err); code != connect.CodePermissionDenied && code != connect.CodeNotFound {
		t.Errorf("another tenant's domain read: %v", err)
	}
	list, err := b.List(bob, resource.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("another tenant's domains listed: %d", len(list.Items))
	}
	_, err = b.Delete(bob, resource.Key{Name: "example"}, nil)
	if code := connect.CodeOf(err); code != connect.CodePermissionDenied && code != connect.CodeNotFound {
		t.Errorf("another tenant's domain deleted: %v", err)
	}
	edge := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.Edge, ID: "h1"})
	if list, err := b.List(edge, resource.ListOptions{}); err != nil || len(list.Items) != 1 {
		t.Errorf("the edge lists %v, %v; want the one domain", list, err)
	}
}

func TestEndpointHostnames(t *testing.T) {
	s, _, user := running(t)
	bob := otherTenant(t, s)
	project(t, s, user, "blog")
	d := s.backend(registry.CustomDomain)
	b := s.backend(registry.PublicEndpoint)
	if _, err := d.Apply(user, customDomain("example", "acme", "example.com", "web")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Apply(bob, customDomain("theirs", "other", "theirs.org")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply(user, publicAt("site", "example.com", "www.example.com")); err != nil {
		t.Fatalf("an endpoint at the domain and one label below: %v", err)
	}
	privateAt := endpoint("priv", "api", 80, private)
	privateAt.Spec.Hostnames = []string{"priv.example.com"}
	for what, ep := range map[string]*networkv1alpha1.PublicEndpoint{
		"a PRIVATE endpoint": privateAt,
		"an endpoint unset": func() *networkv1alpha1.PublicEndpoint {
			e := endpoint("u", "api", 80, 0)
			e.Spec.Hostnames = []string{"u.example.com"}
			return e
		}(),
		"a hostname twice":     publicAt("twice", "a.example.com", "a.example.com"),
		"a hostname not one":   publicAt("bad", "A.example.com"),
		"a hostname with port": publicAt("port", "a.example.com:443"),
	} {
		_, err := b.Apply(user, ep)
		wantCodeOf(t, err, connect.CodeInvalidArgument, what)
	}
	for what, ep := range map[string]*networkv1alpha1.PublicEndpoint{
		"two labels below the domain":     publicAt("deep", "a.b.example.com"),
		"under no domain":                 publicAt("none", "example.org"),
		"under another tenant's domain":   publicAt("theft", "www.theirs.org"),
		"a hostname another endpoint has": publicAt("dup", "www.example.com"),
		"a project the domain leaves out": func() *networkv1alpha1.PublicEndpoint {
			e := publicAt("x", "blog.example.com")
			e.Metadata.Project = "blog"
			return e
		}(),
	} {
		_, err := b.Apply(user, ep)
		wantCodeOf(t, err, connect.CodeFailedPrecondition, what)
	}
	// While an endpoint serves under the domain, it stays, and keeps the
	// endpoint's project.
	_, err := d.Delete(user, resource.Key{Name: "example"}, nil)
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "deleting a domain in use")
	_, err = d.Apply(user, customDomain("example", "acme", "example.com", "blog"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "leaving out a project that uses the domain")
	// A hostname given up is another's to take, and the domain free to go.
	if _, err := b.Apply(user, publicAt("site", "example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply(user, publicAt("www", "www.example.com")); err != nil {
		t.Errorf("a hostname another endpoint gave up: %v", err)
	}
	for _, name := range []string{"site", "www"} {
		if _, err := b.Delete(user, resource.Key{Project: "web", Name: name}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Delete(user, resource.Key{Name: "example"}, nil); err != nil {
		t.Errorf("deleting a domain no endpoint uses: %v", err)
	}
	_, err = b.Apply(user, publicAt("late", "late.example.com"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a hostname under a deleted domain")
}

// TestCustomDomainVerification: a domain is verified once its challenge
// name is a CNAME of its own target, and stays so; an endpoint's hostnames
// serve once their domain is.
func TestCustomDomainVerification(t *testing.T) {
	dns, err := acmetest.Start("127.0.0.1:0", "example.com")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dns.Close)
	s, system, user := runningWith(t, func(c *Config) { c.PublicEndpoints.Resolvers = []string{dns.Addr} })
	if _, err := s.backend(registry.CustomDomain).Apply(user, customDomain("example", "acme", "example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.PublicEndpoint).Apply(user, publicAt("site", "www.example.com")); err != nil {
		t.Fatal(err)
	}
	key := controller.Key{Name: "example"}
	reconcile := func() *networkv1alpha1.CustomDomain {
		t.Helper()
		if err := s.reconcileCustomDomain(system, key); err != nil {
			t.Fatal(err)
		}
		if err := s.reconcilePublicEndpoint(system, controller.Key{Project: "web", Name: "site"}); err != nil {
			t.Fatal(err)
		}
		return mustGet[*networkv1alpha1.CustomDomain](t, s, system, registry.CustomDomain, resource.Key{Name: "example"})
	}
	site := func() *networkv1alpha1.PublicEndpoint {
		return mustGet[*networkv1alpha1.PublicEndpoint](t, s, system, registry.PublicEndpoint, resource.Key{Project: "web", Name: "site"})
	}

	got := reconcile()
	target := got.GetStatus().GetChallengeTarget()
	if target != customdomain.ChallengeTarget(got.GetMetadata().GetUid(), "acme.hangarlab.dev") {
		t.Fatalf("challenge target %q", target)
	}
	records := map[string]string{}
	for _, r := range got.GetStatus().GetRecords() {
		records[r.GetName()] = r.GetType() + " " + r.GetValue()
	}
	for name, want := range map[string]string{
		"_acme-challenge.example.com": "CNAME " + target,
		"*.example.com":               "CNAME cname.hangarlab.dev",
		"example.com":                 "ALIAS cname.hangarlab.dev",
	} {
		if records[name] != want {
			t.Errorf("record %s is %q, want %q", name, records[name], want)
		}
	}
	verified := func(d *networkv1alpha1.CustomDomain) *metav1.Condition {
		return conditions.Find(d.GetStatus().GetConditions(), CustomDomainVerified)
	}
	if c := verified(got); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || c.GetReason() != ReasonChallengeMissing {
		t.Errorf("with no record: %v", c)
	}
	if c := conditions.Find(site().GetStatus().GetConditions(), PublicEndpointHostnames); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || len(site().GetStatus().GetHostnameUrls()) != 0 {
		t.Errorf("an endpoint under a domain not verified: %v, %v", c, site().GetStatus().GetHostnameUrls())
	}

	// Another Domain's target, as a CNAME a deleted one left behind, proves
	// nothing.
	dns.SetCNAME("_acme-challenge.example.com", customdomain.ChallengeTarget("another-uid", "acme.hangarlab.dev"))
	if c := verified(reconcile()); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || !strings.Contains(c.GetMessage(), "another") && !strings.Contains(c.GetMessage(), "not ") {
		t.Errorf("with another domain's target: %v", c)
	}
	dns.SetCNAME("_acme-challenge.example.com", target)
	if c := verified(reconcile()); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Fatalf("with the record: %v", c)
	}
	if urls := site().GetStatus().GetHostnameUrls(); len(urls) != 1 || urls[0] != "https://www.example.com" {
		t.Errorf("an endpoint under a verified domain serves at %v", urls)
	}
	// Verified stays so when the record goes.
	dns.SetCNAME("_acme-challenge.example.com", "")
	if c := verified(reconcile()); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("after the record went: %v", c)
	}
}

func TestCustomDomainsCountTowardThePlan(t *testing.T) {
	s, _, user := runningWith(t, func(c *Config) { c.DefaultPlanLimits = &tenancyv1alpha1.Limits{Domains: ptr(int32(1))} })
	b := s.backend(registry.CustomDomain)
	if _, err := b.Apply(user, customDomain("one", "acme", "one.org")); err != nil {
		t.Fatal(err)
	}
	_, err := b.Apply(user, customDomain("two", "acme", "two.org"))
	wantCodeOf(t, err, connect.CodeResourceExhausted, "a domain past the plan")
	// A domain applied again counts as itself.
	if _, err := b.Apply(user, customDomain("one", "acme", "one.org", "web")); err != nil {
		t.Errorf("changing the one domain: %v", err)
	}
	_, err = s.backend(registry.Project).Apply(user, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "web"},
		Spec:     &tenancyv1alpha1.ProjectSpec{Tenant: "acme", Limits: &tenancyv1alpha1.Limits{Domains: ptr(int32(1))}},
	})
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a project's own limit on domains")
}

func ptr[T any](v T) *T { return &v }

// markVerified sets a CustomDomain's Verified condition, as the controller
// does on finding its challenge CNAME.
func markVerified(t *testing.T, s *Server, system context.Context, name string) {
	t.Helper()
	d := mustGet[*networkv1alpha1.CustomDomain](t, s, system, registry.CustomDomain, resource.Key{Name: name})
	status := &networkv1alpha1.CustomDomainStatus{Conditions: []*metav1.Condition{{
		Type: CustomDomainVerified, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: ReasonDomainVerified,
	}}}
	rv := d.GetMetadata().GetResourceVersion()
	if _, err := s.backend(registry.CustomDomain).UpdateStatus(system, resource.Key{Name: name}, status, &rv); err != nil {
		t.Fatal(err)
	}
}

// TestTheFirstTenantToVerifyANameHasIt: two tenants with one domain, both
// CNAMEs in place; the first verified has it, the other is told why it is
// not, and stays so with its record in place.
func TestTheFirstTenantToVerifyANameHasIt(t *testing.T) {
	dns, err := acmetest.Start("127.0.0.1:0", "example.com")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dns.Close)
	s, system, user := runningWith(t, func(c *Config) { c.PublicEndpoints.Resolvers = []string{dns.Addr} })
	bob := otherTenant(t, s)
	b := s.backend(registry.CustomDomain)
	if _, err := b.Apply(bob, customDomain("squat", "other", "example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply(user, customDomain("example", "acme", "example.com")); err != nil {
		t.Fatalf("a name another tenant has not verified: %v", err)
	}
	mine := mustGet[*networkv1alpha1.CustomDomain](t, s, system, registry.CustomDomain, resource.Key{Name: "example"})
	dns.SetCNAME("_acme-challenge.example.com", customdomain.ChallengeTarget(mine.GetMetadata().GetUid(), "acme.hangarlab.dev"))
	for _, name := range []string{"example", "squat"} {
		if err := s.reconcileCustomDomain(system, controller.Key{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if !isVerified(mustGet[*networkv1alpha1.CustomDomain](t, s, system, registry.CustomDomain, resource.Key{Name: "example"})) {
		t.Fatal("the holder's domain is not verified")
	}
	// Even with its own record in place, the other is not verified after.
	theirs := mustGet[*networkv1alpha1.CustomDomain](t, s, system, registry.CustomDomain, resource.Key{Name: "squat"})
	dns.SetCNAME("_acme-challenge.example.com", customdomain.ChallengeTarget(theirs.GetMetadata().GetUid(), "acme.hangarlab.dev"))
	if err := s.reconcileCustomDomain(system, controller.Key{Name: "squat"}); err != nil {
		t.Fatal(err)
	}
	theirs = mustGet[*networkv1alpha1.CustomDomain](t, s, system, registry.CustomDomain, resource.Key{Name: "squat"})
	if c := conditions.Find(theirs.GetStatus().GetConditions(), CustomDomainVerified); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || c.GetReason() != ReasonHeldByAnotherTenant {
		t.Errorf("the other tenant's domain: %v", c)
	}
}

// TestTwoTenantsVerifyingAtOnce: tenants whose domains overlap, each with
// its record in place as the other's verification runs: one is verified,
// never both.
func TestTwoTenantsVerifyingAtOnce(t *testing.T) {
	for range 10 {
		dns, err := acmetest.Start("127.0.0.1:0", "example.com")
		if err != nil {
			t.Fatal(err)
		}
		s, system, user := runningWith(t, func(c *Config) { c.PublicEndpoints.Resolvers = []string{dns.Addr} })
		bob := otherTenant(t, s)
		b := s.backend(registry.CustomDomain)
		if _, err := b.Apply(user, customDomain("a", "acme", "example.com")); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Apply(bob, customDomain("b", "other", "shop.example.com")); err != nil {
			t.Fatal(err)
		}
		for name, domain := range map[string]string{"a": "example.com", "b": "shop.example.com"} {
			d := mustGet[*networkv1alpha1.CustomDomain](t, s, system, registry.CustomDomain, resource.Key{Name: name})
			dns.SetCNAME("_acme-challenge."+domain, customdomain.ChallengeTarget(d.GetMetadata().GetUid(), "acme.hangarlab.dev"))
		}
		var wg sync.WaitGroup
		for _, name := range []string{"a", "b"} {
			wg.Go(func() {
				if err := s.reconcileCustomDomain(system, controller.Key{Name: name}); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		verified := 0
		for _, name := range []string{"a", "b"} {
			if isVerified(mustGet[*networkv1alpha1.CustomDomain](t, s, system, registry.CustomDomain, resource.Key{Name: name})) {
				verified++
			}
		}
		if verified != 1 {
			t.Fatalf("%d of two overlapping domains verified; want one", verified)
		}
		dns.Close()
	}
}

// TestOneHostnameOneEndpointUnderRacingApplies: endpoints applied at once,
// all asking for one hostname: exactly one has it. As the system, whose
// writes hold no quota lock, so the endpoints' own lock is all that keeps
// them apart.
func TestOneHostnameOneEndpointUnderRacingApplies(t *testing.T) {
	s, system, user := running(t)
	if _, err := s.backend(registry.CustomDomain).Apply(user, customDomain("example", "acme", "example.com")); err != nil {
		t.Fatal(err)
	}
	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, errs[i] = s.backend(registry.PublicEndpoint).Apply(system, publicAt(fmt.Sprintf("site%d", i), "www.example.com"))
		})
	}
	wg.Wait()
	var won int
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case connect.CodeOf(err) == connect.CodeFailedPrecondition:
		default:
			t.Errorf("an apply: %v", err)
		}
	}
	holders := 0
	eps, err := s.endpoints(system, "web")
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range eps {
		if slices.Contains(ep.GetSpec().GetHostnames(), "www.example.com") {
			holders++
		}
	}
	if won != 1 || holders != 1 {
		t.Errorf("%d applies won and %d endpoints hold the hostname; want one", won, holders)
	}
}

// TestANestedDomainTakesNoHostnameFromAProjectItLeavesOut: a domain made
// under another of the tenant's takes the hostnames under it, so it may
// not leave out a project that serves at one.
func TestANestedDomainTakesNoHostnameFromAProjectItLeavesOut(t *testing.T) {
	s, _, user := running(t)
	project(t, s, user, "blog")
	d := s.backend(registry.CustomDomain)
	if _, err := d.Apply(user, customDomain("example", "acme", "example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.PublicEndpoint).Apply(user, publicAt("site", "b.example.com")); err != nil {
		t.Fatal(err)
	}
	_, err := d.Apply(user, customDomain("b", "acme", "b.example.com", "blog"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a nested domain leaving out a project serving under it")
	if _, err := d.Apply(user, customDomain("b", "acme", "b.example.com", "web")); err != nil {
		t.Errorf("a nested domain letting the project: %v", err)
	}
}

// TestProjectRolesDoNotReachCustomDomains: a domain is the tenant's, so
// even a project's editor neither reads nor writes one; and no one but the
// apiserver writes a domain's status, its owner included.
func TestProjectRolesDoNotReachCustomDomains(t *testing.T) {
	s, system, user := running(t)
	b := s.backend(registry.CustomDomain)
	if _, err := b.Apply(user, customDomain("example", "acme", "example.com")); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"project.owner", "project.editor"} {
		who := bindProjectRole(t, s, system, strings.ReplaceAll(role, ".", "-")+"@example.com", role)
		_, err := b.Apply(who, customDomain("mine", "acme", "mine.org"))
		wantCodeOf(t, err, connect.CodePermissionDenied, role+" making a domain")
		_, err = b.Get(who, resource.Key{Name: "example"})
		wantCodeOf(t, err, connect.CodePermissionDenied, role+" reading a domain")
		_, err = b.Delete(who, resource.Key{Name: "example"}, nil)
		wantCodeOf(t, err, connect.CodePermissionDenied, role+" deleting a domain")
	}
	d := mustGet[*networkv1alpha1.CustomDomain](t, s, system, registry.CustomDomain, resource.Key{Name: "example"})
	rv := d.GetMetadata().GetResourceVersion()
	_, err := b.UpdateStatus(user, resource.Key{Name: "example"}, &networkv1alpha1.CustomDomainStatus{Conditions: []*metav1.Condition{{
		Type: CustomDomainVerified, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE,
	}}}, &rv)
	wantCodeOf(t, err, connect.CodePermissionDenied, "the tenant's owner verifying its domain itself")
}
