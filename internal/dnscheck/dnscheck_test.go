package dnscheck_test

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/YukinaMochizuki/hangar/internal/dnscheck"
	"github.com/YukinaMochizuki/hangar/internal/dnscheck/dnstest"
)

const (
	challenge = "_acme-challenge.example.com."
	target    = "k3x9.acme.hangarlab.dev."
)

var challengeKey = dnstest.Key(challenge, dns.TypeCNAME)

var rr = dnstest.RR

// stale is a recursive resolver as one is after it was asked for the
// challenge record before the record was made: it says there is none, as
// it remembers. It knows example.com's nameserver and its address, a
// public one.
func stale(t *testing.T) *dnstest.Server {
	return dnstest.Serve(t, func(q dns.Question, m *dns.Msg) {
		m.RecursionAvailable = true
		switch {
		case q.Qtype == dns.TypeNS && q.Name == "example.com.":
			m.Answer = append(m.Answer, rr(t, "example.com. 300 IN NS ns1.example.com."))
		case q.Qtype == dns.TypeA && q.Name == "ns1.example.com.":
			m.Answer = append(m.Answer, rr(t, "ns1.example.com. 300 IN A 192.0.2.53"))
		default:
			m.Rcode = dns.RcodeNameError
			m.Ns = append(m.Ns, rr(t, "example.com. 1800 IN SOA ns1.example.com. h.example.com. 1 1 1 1 1800"))
		}
	})
}

// authority is example.com's nameserver, with the challenge CNAME of
// cname if it is set.
func authority(t *testing.T, cname string) *dnstest.Server {
	return dnstest.Serve(t, func(q dns.Question, m *dns.Msg) {
		m.Authoritative = true
		if q.Name == challenge && cname != "" {
			m.Answer = append(m.Answer, rr(t, challenge+" 300 IN CNAME "+cname))
			return
		}
		m.Rcode = dns.RcodeNameError
		m.Ns = append(m.Ns, rr(t, "example.com. 1800 IN SOA ns1.example.com. h.example.com. 1 1 1 1 1800"))
	})
}

// at asks each nameserver, by name, at the server given for it; asked
// records the addresses the lookup meant to ask them at.
type at struct {
	servers map[string]string
	mu      sync.Mutex
	asked   []netip.Addr
}

func (a *at) Nameserver(host string, addr netip.Addr) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, addr)
	if s, ok := a.servers[host]; ok {
		return s
	}
	return a.servers["*"]
}

func (a *at) addrs() []netip.Addr {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]netip.Addr(nil), a.asked...)
}

func servers(m map[string]string) *at { return &at{servers: m} }

func lookup(t *testing.T, r *dnscheck.Resolver) dnscheck.Answer {
	t.Helper()
	ans, err := lookupErr(r)
	if err != nil {
		t.Fatal(err)
	}
	return ans
}

func lookupErr(r *dnscheck.Resolver) (dnscheck.Answer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return r.CNAME(ctx, "_acme-challenge.Example.com")
}

// TestARecordJustMadeIsSeenThoughTheResolverRemembersNone: the resolver
// still answers that the challenge record does not exist; the zone's own
// nameserver, asked without recursion over TCP at the address the
// resolver gave, has it.
func TestARecordJustMadeIsSeenThoughTheResolverRemembersNone(t *testing.T) {
	rec, auth := stale(t), authority(t, target)
	ns := servers(map[string]string{"ns1.example.com.": auth.Addr})
	ans := lookup(t, &dnscheck.Resolver{Recursive: []string{rec.Addr}, Nameserver: ns.Nameserver})
	if ans.Target != strings.TrimSuffix(target, ".") || ans.From != "ns1.example.com" {
		t.Errorf("got %+v; want %s from ns1.example.com", ans, target)
	}
	if n := rec.Asked(challengeKey); n != 0 {
		t.Errorf("the resolver was asked for the record itself %d times", n)
	}
	// A nameserver that also resolves for others would answer a question
	// that asks for recursion from its own cache; one over UDP, anyone on
	// the internet can answer in its place.
	if n, udp := auth.Recursed(challengeKey), auth.OverUDP(challengeKey); n != 0 || udp != 0 || auth.Asked(challengeKey) == 0 {
		t.Errorf("the nameserver was asked %d times, %d of them for recursion, %d over UDP", auth.Asked(challengeKey), n, udp)
	}
	if got := ns.addrs(); len(got) != 1 || got[0] != netip.MustParseAddr("192.0.2.53") {
		t.Errorf("the nameserver was asked at %v; want its address 192.0.2.53", got)
	}
}

// TestNoRecordIsNoneFromTheNameserver: the nameserver says there is none;
// that is the answer, and the resolver is not asked instead.
func TestNoRecordIsNoneFromTheNameserver(t *testing.T) {
	rec, auth := stale(t), authority(t, "")
	ans := lookup(t, &dnscheck.Resolver{Recursive: []string{rec.Addr}, Nameserver: servers(map[string]string{"ns1.example.com.": auth.Addr}).Nameserver})
	if ans.Target != "" || ans.From != "ns1.example.com" {
		t.Errorf("got %+v; want none from ns1.example.com", ans)
	}
}

// TestACNAMEOfAnotherNameIsNotTheRecord: an answer carrying a CNAME owned
// by another name, as one further along a chain, is no CNAME of the
// challenge name.
func TestACNAMEOfAnotherNameIsNotTheRecord(t *testing.T) {
	rec := stale(t)
	auth := dnstest.Serve(t, func(q dns.Question, m *dns.Msg) {
		m.Authoritative = true
		m.Answer = append(m.Answer, rr(t, "other.example.com. 300 IN CNAME "+target))
	})
	ans := lookup(t, &dnscheck.Resolver{Recursive: []string{rec.Addr}, Nameserver: servers(map[string]string{"ns1.example.com.": auth.Addr}).Nameserver})
	if ans.Target != "" {
		t.Errorf("got %+v; want none", ans)
	}
}

// TestACutAnswerIsAskedAgainOverTCP: a resolver whose answer over UDP is
// cut short is asked again over TCP, and its full answer taken.
func TestACutAnswerIsAskedAgainOverTCP(t *testing.T) {
	var udp sync.Map
	rec := dnstest.Serve(t, func(q dns.Question, m *dns.Msg) {
		m.RecursionAvailable = true
		if q.Qtype == dns.TypeNS {
			m.Rcode = dns.RcodeServerFailure
			return
		}
		// The first question of each name, over UDP, is cut; the second,
		// over TCP, answered.
		if _, again := udp.LoadOrStore(q.Name, true); !again {
			m.Truncated = true
			return
		}
		m.Answer = append(m.Answer, rr(t, challenge+" 300 IN CNAME "+target))
	})
	ans := lookup(t, &dnscheck.Resolver{Recursive: []string{rec.Addr}})
	if ans.Target != strings.TrimSuffix(target, ".") || ans.From != "a resolver" {
		t.Errorf("got %+v; want %s from a resolver", ans, target)
	}
	if n := rec.OverUDP(challengeKey); n != 1 || rec.Asked(challengeKey) != 2 {
		t.Errorf("the resolver was asked %d times, %d over UDP; want once over each", rec.Asked(challengeKey), n)
	}
}

// TestAZoneBelowTheDomainIsFollowed: example.com delegates
// _acme-challenge.example.com to a nameserver of its own, which holds the
// record, and is asked at the address its glue gives.
func TestAZoneBelowTheDomainIsFollowed(t *testing.T) {
	rec := stale(t)
	sub := dnstest.Serve(t, func(q dns.Question, m *dns.Msg) {
		m.Authoritative = true
		m.Answer = append(m.Answer, rr(t, challenge+" 300 IN CNAME "+target))
	})
	parent := dnstest.Serve(t, func(q dns.Question, m *dns.Msg) {
		m.Ns = append(m.Ns, rr(t, challenge+" 300 IN NS ns.sub.example.net."))
		m.Extra = append(m.Extra, rr(t, "ns.sub.example.net. 300 IN A 198.51.100.7"))
	})
	ns := servers(map[string]string{"ns1.example.com.": parent.Addr, "ns.sub.example.net.": sub.Addr})
	ans := lookup(t, &dnscheck.Resolver{Recursive: []string{rec.Addr}, Nameserver: ns.Nameserver})
	if ans.Target != strings.TrimSuffix(target, ".") || ans.From != "ns.sub.example.net" {
		t.Errorf("got %+v; want %s from ns.sub.example.net", ans, target)
	}
	if got := ns.addrs(); len(got) != 2 || got[1] != netip.MustParseAddr("198.51.100.7") {
		t.Errorf("asked at %v; want the glue's 198.51.100.7 last", got)
	}
}

// TestAReferralElsewhereIsNotFollowed: a referral to a zone that is not
// the challenge name nor above it, or not below example.com, leads
// nowhere; its glue, for a name no referral lists, is not used.
func TestAReferralElsewhereIsNotFollowed(t *testing.T) {
	for what, ns := range map[string]string{
		"a sibling":           "other.example.com.",
		"above the zone":      "com.",
		"another zone":        "example.org.",
		"the zone itself":     "example.com.",
		"below the challenge": "x." + challenge,
	} {
		t.Run(what, func(t *testing.T) {
			rec := stale(t)
			elsewhere := authority(t, target)
			parent := dnstest.Serve(t, func(q dns.Question, m *dns.Msg) {
				m.Ns = append(m.Ns, rr(t, ns+" 300 IN NS ns.elsewhere.net."))
				m.Extra = append(m.Extra, rr(t, "ns.elsewhere.net. 300 IN A 198.51.100.9"))
			})
			r := &dnscheck.Resolver{Recursive: []string{rec.Addr}, Timeout: time.Second,
				Nameserver: servers(map[string]string{"ns1.example.com.": parent.Addr, "ns.elsewhere.net.": elsewhere.Addr}).Nameserver}
			ans, err := lookupErr(r)
			if err != nil {
				t.Fatal(err)
			}
			if elsewhere.Asked(challengeKey) != 0 || ans.From != "a resolver" {
				t.Errorf("got %+v, ns.elsewhere.net asked %d times; want the resolver's answer", ans, elsewhere.Asked(challengeKey))
			}
		})
	}
}

// TestReferralsAreFollowedAFewTimes: nameservers that refer on and on,
// each to a zone one label lower towards the challenge name, are followed
// three times, then the resolver answers.
func TestReferralsAreFollowedAFewTimes(t *testing.T) {
	rec := stale(t)
	const deep = "a.b.c.d.e." + challenge
	var referred atomic.Int32
	refer := dnstest.Serve(t, func(q dns.Question, m *dns.Msg) {
		// Each answer refers one label further down than the last, from
		// _acme-challenge.example.com on.
		n := int(referred.Add(1))
		labels := dns.SplitDomainName(deep)
		owner := dns.Fqdn(strings.Join(labels[len(labels)-3-n:], "."))
		m.Ns = append(m.Ns, rr(t, owner+" 300 IN NS ns1.example.com."))
	})
	r := &dnscheck.Resolver{Recursive: []string{rec.Addr}, Nameserver: servers(map[string]string{"*": refer.Addr}).Nameserver}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ans, err := r.CNAME(ctx, deep)
	if err != nil {
		t.Fatal(err)
	}
	if n := refer.Asked(dnstest.Key(deep, dns.TypeCNAME)); n != 4 || ans.From != "a resolver" {
		t.Errorf("got %+v after %d questions; want the resolver's answer after 4", ans, n)
	}
}

// TestNameserversAreAskedAtPublicAddressesAlone: a zone whose nameservers
// are at loopback, private, link-local, shared and unique-local addresses,
// as a tenant could set to probe the networks the apiserver is on, is
// asked at none of them; the error names no address.
func TestNameserversAreAskedAtPublicAddressesAlone(t *testing.T) {
	rec := dnstest.Serve(t, func(q dns.Question, m *dns.Msg) {
		switch {
		case q.Qtype == dns.TypeNS && q.Name == "example.com.":
			m.Answer = append(m.Answer, rr(t, "example.com. 300 IN NS ns1.example.com."))
		case q.Qtype == dns.TypeA && q.Name == "ns1.example.com.":
			for _, a := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0"} {
				m.Answer = append(m.Answer, rr(t, "ns1.example.com. 300 IN A "+a))
			}
		case q.Qtype == dns.TypeAAAA && q.Name == "ns1.example.com.":
			for _, a := range []string{"::1", "fd00::1", "fe80::1", "::ffff:127.0.0.1"} {
				m.Answer = append(m.Answer, rr(t, "ns1.example.com. 300 IN AAAA "+a))
			}
		default:
			m.Rcode = dns.RcodeServerFailure
		}
	})
	ns := servers(map[string]string{"*": rec.Addr})
	_, err := lookupErr(&dnscheck.Resolver{Recursive: []string{rec.Addr}, Nameserver: ns.Nameserver})
	if got := ns.addrs(); len(got) != 0 {
		t.Errorf("asked at %v", got)
	}
	if err == nil || !strings.Contains(err.Error(), "no public address of example.com's nameservers ns1.example.com") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error %v", err)
	}
}

// TestALookupAsksAFewNameserversForAWhile: a zone of many nameservers of
// many addresses, none answering, is asked at a few of them, and the
// lookup ends by its deadline, saying so in words naming no address.
func TestALookupAsksAFewNameserversForAWhile(t *testing.T) {
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })
	rec := dnstest.Serve(t, func(q dns.Question, m *dns.Msg) {
		switch {
		case q.Qtype == dns.TypeNS && q.Name == "example.com.":
			for i := range 10 {
				m.Answer = append(m.Answer, rr(t, "example.com. 300 IN NS ns"+string(rune('a'+i))+".example.com."))
			}
		case q.Qtype == dns.TypeA:
			for i := range 10 {
				m.Answer = append(m.Answer, rr(t, q.Name+" 300 IN A 192.0.2."+string(rune('1'+i%9))))
			}
		default:
			m.Rcode = dns.RcodeServerFailure
		}
	})
	ns := servers(map[string]string{"*": silent.Addr().String()})
	start := time.Now()
	_, err = lookupErr(&dnscheck.Resolver{Recursive: []string{rec.Addr}, Nameserver: ns.Nameserver, Timeout: 200 * time.Millisecond, Total: 500 * time.Millisecond})
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the lookup took %s", took)
	}
	if n := len(ns.addrs()); n == 0 || n > 6 {
		t.Errorf("%d addresses asked; want at most 6", n)
	}
	if n := rec.Asked(dnstest.Key("nse.example.com.", dns.TypeA)); n != 0 {
		t.Errorf("a fifth nameserver's address was looked up %d times", n)
	}
	if err == nil || strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "192.0.2") {
		t.Errorf("error %v", err)
	}
}

// TestTheResolverAnswersWhenNoNameserverDoes: with the zone's nameserver
// unreachable, the resolver's answer is taken, and says it is a
// resolver's.
func TestTheResolverAnswersWhenNoNameserverDoes(t *testing.T) {
	rec := stale(t)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone := closed.Addr().String()
	_ = closed.Close()
	ans := lookup(t, &dnscheck.Resolver{Recursive: []string{rec.Addr}, Timeout: 300 * time.Millisecond,
		Nameserver: servers(map[string]string{"ns1.example.com.": gone}).Nameserver})
	if ans.Target != "" || ans.From != "a resolver" {
		t.Errorf("got %+v; want none from a resolver", ans)
	}
}

// TestTheZoneIsFoundAboveACNAME: a resolver that answers the NS question
// for the challenge name with its CNAME, as a fresh one does, still leads
// to example.com's nameserver.
func TestTheZoneIsFoundAboveACNAME(t *testing.T) {
	rec := dnstest.Serve(t, func(q dns.Question, m *dns.Msg) {
		switch {
		case q.Name == challenge:
			m.Answer = append(m.Answer, rr(t, challenge+" 300 IN CNAME "+target))
		case q.Qtype == dns.TypeNS && q.Name == "example.com.":
			m.Answer = append(m.Answer, rr(t, "example.com. 300 IN NS ns1.example.com."))
		case q.Qtype == dns.TypeA && q.Name == "ns1.example.com.":
			m.Answer = append(m.Answer, rr(t, "ns1.example.com. 300 IN A 192.0.2.53"))
		}
	})
	auth := authority(t, target)
	ans := lookup(t, &dnscheck.Resolver{Recursive: []string{rec.Addr}, Nameserver: servers(map[string]string{"ns1.example.com.": auth.Addr}).Nameserver})
	if ans.From != "ns1.example.com" {
		t.Errorf("got %+v; want an answer from ns1.example.com", ans)
	}
}

// TestPublic: the addresses a nameserver may be asked at.
func TestPublic(t *testing.T) {
	for a, want := range map[string]bool{
		"192.0.2.1": true, "8.8.8.8": true, "2001:4860:4860::8888": true,
		"127.0.0.1": false, "10.0.0.1": false, "172.31.0.1": false, "192.168.0.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "100.127.255.254": false, "0.0.0.0": false,
		"224.0.0.1": false, "255.255.255.255": false, "::1": false, "::": false, "fd12::1": false,
		"fe80::1": false, "ff02::1": false, "::ffff:10.0.0.1": false, "::ffff:8.8.8.8": true,
	} {
		if got := dnscheck.Public(netip.MustParseAddr(a)); got != want {
			t.Errorf("Public(%s) = %v; want %v", a, got, want)
		}
	}
}
