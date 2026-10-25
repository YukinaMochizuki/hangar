package apiserver

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"github.com/miekg/dns"

	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/customdomain"
	"github.com/YukinaMochizuki/hangar/internal/dnscheck/dnstest"
)

// challengeKey is the question for example.com's challenge record.
var challengeKey = dnstest.Key("_acme-challenge.example.com.", dns.TypeCNAME)

// dnsWorld is example.com's DNS as the apiserver meets it: a resolver that
// remembers the challenge record missing, as one asked before the record
// was made does for the zone's negative TTL, and example.com's nameserver,
// at a public address, which has the record once target is set.
type dnsWorld struct {
	resolver, nameserver *dnstest.Server
	mu                   sync.Mutex
	target               string
	// answering, if set, is called as the nameserver is asked, before it
	// answers.
	answering func()
}

func (w *dnsWorld) set(target string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.target = target
}

func newDNSWorld(t *testing.T) *dnsWorld {
	t.Helper()
	w := &dnsWorld{}
	w.resolver = dnstest.Serve(t, func(q dns.Question, m *dns.Msg) {
		m.RecursionAvailable = true
		switch {
		case q.Qtype == dns.TypeNS && q.Name == "example.com.":
			m.Answer = append(m.Answer, dnstest.RR(t, "example.com. 300 IN NS ns1.example.com."))
		case q.Qtype == dns.TypeA && q.Name == "ns1.example.com.":
			m.Answer = append(m.Answer, dnstest.RR(t, "ns1.example.com. 300 IN A 192.0.2.53"))
		default:
			m.Rcode = dns.RcodeNameError
		}
	})
	w.nameserver = dnstest.Serve(t, func(q dns.Question, m *dns.Msg) {
		m.Authoritative = true
		w.mu.Lock()
		answering := w.answering
		w.mu.Unlock()
		if answering != nil {
			answering()
		}
		w.mu.Lock()
		target := w.target
		w.mu.Unlock()
		if strings.EqualFold(q.Name, "_acme-challenge.example.com.") && target != "" {
			m.Answer = append(m.Answer, dnstest.RR(t, "_acme-challenge.example.com. 300 IN CNAME "+target+"."))
			return
		}
		m.Rcode = dns.RcodeNameError
	})
	return w
}

func (w *dnsWorld) configure(c *Config) {
	c.PublicEndpoints.Resolvers = []string{w.resolver.Addr}
	c.PublicEndpoints.Nameserver = func(string, netip.Addr) string { return w.nameserver.Addr }
}

func (w *dnsWorld) asked() int { return w.nameserver.Asked(challengeKey) }

func checkDomain(ctx context.Context, s *Server, name string) (*networkv1alpha1.CustomDomain, error) {
	res, err := customDomainCheck{s}.CheckCustomDomain(ctx, connect.NewRequest(&networkv1alpha1.CheckCustomDomainRequest{Name: name}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetCustomDomain(), nil
}

// TestCheckSeesARecordTheResolverRemembersMissing: the record is made
// after the domain, and the resolver still says there is none; a check
// asks example.com's nameserver, finds it, and the domain is verified, in
// the answer and as stored.
func TestCheckSeesARecordTheResolverRemembersMissing(t *testing.T) {
	t.Parallel()
	w := newDNSWorld(t)
	s, system, user := runningWith(t, w.configure)
	if _, err := s.backend(registry.CustomDomain).Apply(user, customDomain("example", "acme", "example.com")); err != nil {
		t.Fatal(err)
	}
	got, err := checkDomain(user, s, "example")
	if err != nil {
		t.Fatal(err)
	}
	if isVerified(got) || !strings.Contains(verifiedMessage(got), "ns1.example.com answers it is none") {
		t.Fatalf("before the record: %v", got.GetStatus().GetConditions())
	}
	w.set(got.GetStatus().GetChallengeTarget())
	s.clock.(*clockwork.FakeClock).Advance(domainCheckInterval)
	got, err = checkDomain(user, s, "example")
	if err != nil {
		t.Fatal(err)
	}
	if !isVerified(got) || !strings.Contains(verifiedMessage(got), "as ns1.example.com answers") {
		t.Errorf("after the record: %v", got.GetStatus().GetConditions())
	}
	if !isVerified(mustGet[*networkv1alpha1.CustomDomain](t, s, system, registry.CustomDomain, resource.Key{Name: "example"})) {
		t.Error("the check's verification was not stored")
	}
}

// TestAVerifiedDomainIsNotLookedUpAgain: checking a verified domain answers
// it as it is and asks no nameserver.
func TestAVerifiedDomainIsNotLookedUpAgain(t *testing.T) {
	t.Parallel()
	w := newDNSWorld(t)
	s, system, user := runningWith(t, w.configure)
	if _, err := s.backend(registry.CustomDomain).Apply(user, customDomain("example", "acme", "example.com")); err != nil {
		t.Fatal(err)
	}
	markVerified(t, s, system, "example")
	got, err := checkDomain(user, s, "example")
	if err != nil || !isVerified(got) {
		t.Fatalf("checking a verified domain: %v, %v", got, err)
	}
	if n := w.asked(); n != 0 {
		t.Errorf("the nameserver was asked %d times", n)
	}
}

// TestOnlyThoseWhoMayChangeADomainCheckIt: a check writes the status and
// asks the domain's nameservers, so it takes what changing the domain
// takes: the tenant's owner may, a project's people who read it may not,
// nor another tenant, and a domain that does not exist is refused alike.
func TestOnlyThoseWhoMayChangeADomainCheckIt(t *testing.T) {
	t.Parallel()
	w := newDNSWorld(t)
	s, system, user := runningWith(t, w.configure)
	if _, err := s.backend(registry.CustomDomain).Apply(user, customDomain("example", "acme", "example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := checkDomain(user, s, "example"); err != nil {
		t.Errorf("the tenant's owner: %v", err)
	}
	asked := w.asked()
	bob := otherTenant(t, s)
	for who, ctx := range map[string]context.Context{
		"a project's editor, who reads the domain": bindProjectRole(t, s, system, "editor@example.com", "project.editor"),
		"another tenant's owner":                   bob,
	} {
		_, err := checkDomain(ctx, s, "example")
		wantCodeOf(t, err, connect.CodePermissionDenied, who)
	}
	_, err := checkDomain(bob, s, "nosuch")
	wantCodeOf(t, err, connect.CodePermissionDenied, "a domain that does not exist")
	_, err = checkDomain(user, s, "Not_A_Name")
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a name that is no name")
	if n := w.asked(); n != asked {
		t.Errorf("refused checks asked the nameserver %d times", n-asked)
	}
}

func verifiedMessage(d *networkv1alpha1.CustomDomain) string {
	for _, c := range d.GetStatus().GetConditions() {
		if c.GetType() == CustomDomainVerified {
			return c.GetMessage()
		}
	}
	return ""
}

// TestADomainMadeAgainDuringItsLookupIsNotVerifiedByIt: a tenant checks
// its domain "example" for example.com, a zone it holds, and while
// example.com's nameserver is asked, deletes the domain and makes it again
// for victim.example.org, then answers the challenge with the new domain's
// target. That answer is example.com's, and verifies nothing.
func TestADomainMadeAgainDuringItsLookupIsNotVerifiedByIt(t *testing.T) {
	t.Parallel()
	w := newDNSWorld(t)
	s, system, user := runningWith(t, w.configure)
	b := s.backend(registry.CustomDomain)
	if _, err := b.Apply(user, customDomain("example", "acme", "example.com")); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	w.mu.Lock()
	w.answering = func() {
		once.Do(func() {
			if _, err := b.Delete(user, resource.Key{Name: "example"}, nil); err != nil {
				t.Error(err)
				return
			}
			if _, err := b.Apply(user, customDomain("example", "acme", "victim.example.org")); err != nil {
				t.Error(err)
				return
			}
			again := mustGet[*networkv1alpha1.CustomDomain](t, s, system, registry.CustomDomain, resource.Key{Name: "example"})
			w.set(customdomain.ChallengeTarget(again.GetMetadata().GetUid(), ChallengeZone(s.endpointDomain())))
		})
	}
	w.mu.Unlock()
	got, err := checkDomain(user, s, "example")
	if err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	set := w.target != ""
	w.mu.Unlock()
	if got.GetSpec().GetDomain() != "victim.example.org" || !set {
		t.Fatalf("the domain was not made again during the lookup: %v", got)
	}
	if isVerified(got) || isVerified(mustGet[*networkv1alpha1.CustomDomain](t, s, system, registry.CustomDomain, resource.Key{Name: "example"})) {
		t.Errorf("victim.example.org was verified by example.com's nameserver: %v", got.GetStatus().GetConditions())
	}
}

// TestADomainIsNotCheckedAgainAtOnce: a second check within the interval
// is refused, saying when to try again, and asks no nameserver; after it,
// a check is made.
func TestADomainIsNotCheckedAgainAtOnce(t *testing.T) {
	t.Parallel()
	w := newDNSWorld(t)
	s, _, user := runningWith(t, w.configure)
	if _, err := s.backend(registry.CustomDomain).Apply(user, customDomain("example", "acme", "example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := checkDomain(user, s, "example"); err != nil {
		t.Fatal(err)
	}
	asked := w.asked()
	_, err := checkDomain(user, s, "example")
	wantCodeOf(t, err, connect.CodeResourceExhausted, "a second check at once")
	if !strings.Contains(err.Error(), "try again in 5s") {
		t.Errorf("the refusal: %v", err)
	}
	if n := w.asked(); n != asked {
		t.Errorf("a refused check asked the nameserver %d times", n-asked)
	}
	s.clock.(*clockwork.FakeClock).Advance(domainCheckInterval)
	if _, err := checkDomain(user, s, "example"); err != nil {
		t.Errorf("a check after the interval: %v", err)
	}
}

// TestChecksRunAFewAtOnce: past the checks running at once, another
// domain's check is refused until one ends.
func TestChecksRunAFewAtOnce(t *testing.T) {
	t.Parallel()
	var l checkLimiter
	now := time.Now()
	var releases []func()
	for i := range domainChecksAtOnce {
		release, _ := l.take(fmt.Sprint("d", i), now, 0)
		if release == nil {
			t.Fatalf("check %d refused", i)
		}
		releases = append(releases, release)
	}
	if release, _ := l.take("another", now, 0); release != nil {
		t.Fatal("a check past those at once was let run")
	}
	releases[0]()
	if release, _ := l.take("another", now, 0); release == nil {
		t.Error("a check once another ended was refused")
	}
}

// TestNameserversAtInternalAddressesAreNotAsked: a zone whose nameserver
// is at loopback, private, shared and unique-local addresses, as a tenant
// could name to probe the apiserver's networks, is asked at none of them;
// the status says what a resolver answers instead.
func TestNameserversAtInternalAddressesAreNotAsked(t *testing.T) {
	t.Parallel()
	w := newDNSWorld(t)
	internal := dnstest.Serve(t, func(q dns.Question, m *dns.Msg) {
		m.RecursionAvailable = true
		switch {
		case q.Qtype == dns.TypeNS && q.Name == "example.com.":
			m.Answer = append(m.Answer, dnstest.RR(t, "example.com. 300 IN NS ns1.example.com."), dnstest.RR(t, "example.com. 300 IN NS ns2.example.com."))
		case q.Qtype == dns.TypeA && q.Name == "ns1.example.com.":
			m.Answer = append(m.Answer, dnstest.RR(t, "ns1.example.com. 300 IN A 127.0.0.1"), dnstest.RR(t, "ns1.example.com. 300 IN A 10.0.0.53"))
		case q.Qtype == dns.TypeAAAA && q.Name == "ns2.example.com.":
			m.Answer = append(m.Answer, dnstest.RR(t, "ns2.example.com. 300 IN AAAA fd00::53"))
		case q.Qtype == dns.TypeA && q.Name == "ns2.example.com.":
			m.Answer = append(m.Answer, dnstest.RR(t, "ns2.example.com. 300 IN A 100.64.0.53"))
		default:
			m.Rcode = dns.RcodeNameError
		}
	})
	s, _, user := runningWith(t, func(c *Config) {
		w.configure(c)
		c.PublicEndpoints.Resolvers = []string{internal.Addr}
	})
	if _, err := s.backend(registry.CustomDomain).Apply(user, customDomain("example", "acme", "example.com")); err != nil {
		t.Fatal(err)
	}
	got, err := checkDomain(user, s, "example")
	if err != nil {
		t.Fatal(err)
	}
	if n := w.nameserver.Asked(challengeKey); n != 0 {
		t.Errorf("a nameserver at an internal address was asked %d times", n)
	}
	if msg := verifiedMessage(got); !strings.Contains(msg, "a resolver answers it is none") {
		t.Errorf("the status: %q", msg)
	}
}
