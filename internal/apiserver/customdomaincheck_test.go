package apiserver

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"github.com/miekg/dns"

	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
)

// dnsWorld is example.com's DNS as the apiserver meets it: a resolver that
// remembers the challenge record missing, as one asked before the record
// was made does for the zone's negative TTL, and example.com's nameserver,
// which has the record once target is set. asked counts the questions the
// nameserver had for the challenge record.
type dnsWorld struct {
	resolver, nameserver string
	mu                   sync.Mutex
	target               string
	asked                atomic.Int32
}

func (w *dnsWorld) set(target string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.target = target
}

func newDNSWorld(t *testing.T) *dnsWorld {
	t.Helper()
	w := &dnsWorld{}
	w.resolver = serveDNS(t, func(q dns.Question, m *dns.Msg) {
		m.RecursionAvailable = true
		switch {
		case q.Qtype == dns.TypeNS && q.Name == "example.com.":
			m.Answer = append(m.Answer, mustRR(t, "example.com. 300 IN NS ns1.example.com."))
		case q.Qtype == dns.TypeA && q.Name == "ns1.example.com.":
			m.Answer = append(m.Answer, mustRR(t, "ns1.example.com. 300 IN A 127.0.0.1"))
		default:
			m.Rcode = dns.RcodeNameError
		}
	})
	w.nameserver = serveDNS(t, func(q dns.Question, m *dns.Msg) {
		m.Authoritative = true
		w.mu.Lock()
		target := w.target
		w.mu.Unlock()
		if strings.EqualFold(q.Name, "_acme-challenge.example.com.") {
			w.asked.Add(1)
			if target != "" {
				m.Answer = append(m.Answer, mustRR(t, "_acme-challenge.example.com. 300 IN CNAME "+target+"."))
				return
			}
		}
		m.Rcode = dns.RcodeNameError
	})
	return w
}

func (w *dnsWorld) configure(c *Config) {
	c.PublicEndpoints.Resolvers = []string{w.resolver}
	c.PublicEndpoints.Nameserver = func(string, netip.Addr) string { return w.nameserver }
}

func serveDNS(t *testing.T, answer func(dns.Question, *dns.Msg)) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(rw dns.ResponseWriter, req *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(req)
		for _, q := range req.Question {
			answer(q, m)
		}
		_ = rw.WriteMsg(m)
	})}
	started := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(started) }
	go func() { _ = srv.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String()
}

func mustRR(t *testing.T, s string) dns.RR {
	t.Helper()
	r, err := dns.NewRR(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

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
	if n := w.asked.Load(); n != 0 {
		t.Errorf("the nameserver was asked %d times", n)
	}
}

// TestOnlyThoseWhoMayChangeADomainCheckIt: a check writes the status and
// asks the domain's nameservers, so it takes what changing the domain
// takes: the tenant's owner may, a project's people who read it may not,
// nor another tenant, and a domain that does not exist is refused alike.
func TestOnlyThoseWhoMayChangeADomainCheckIt(t *testing.T) {
	w := newDNSWorld(t)
	s, system, user := runningWith(t, w.configure)
	if _, err := s.backend(registry.CustomDomain).Apply(user, customDomain("example", "acme", "example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := checkDomain(user, s, "example"); err != nil {
		t.Errorf("the tenant's owner: %v", err)
	}
	asked := w.asked.Load()
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
	if n := w.asked.Load(); n != asked {
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
