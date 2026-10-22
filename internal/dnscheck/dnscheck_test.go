package dnscheck_test

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/YukinaMochizuki/hangar/internal/dnscheck"
)

const (
	challenge = "_acme-challenge.example.com."
	target    = "k3x9.acme.hangarlab.dev."
)

// fake is a DNS server answering from answer, on a port of its own.
type fake struct {
	addr string
	mu   sync.Mutex
	// asked counts the questions it was asked, by name and type, and
	// recursed those that asked for recursion.
	asked, recursed map[string]int
}

func serve(t *testing.T, answer func(q dns.Question, m *dns.Msg)) *fake {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{addr: pc.LocalAddr().String(), asked: map[string]int{}, recursed: map[string]int{}}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(req)
		for _, q := range req.Question {
			f.mu.Lock()
			key := strings.ToLower(q.Name) + "/" + dns.TypeToString[q.Qtype]
			f.asked[key]++
			if req.RecursionDesired {
				f.recursed[key]++
			}
			f.mu.Unlock()
			answer(q, m)
		}
		_ = w.WriteMsg(m)
	})}
	started := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(started) }
	go func() { _ = srv.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = srv.Shutdown() })
	return f
}

func (f *fake) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked[key]
}

func (f *fake) countRecursed(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recursed[key]
}

func rr(t *testing.T, s string) dns.RR {
	t.Helper()
	r, err := dns.NewRR(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// stale is a recursive resolver as one is after it was asked for the
// challenge record before the record was made: it says there is none, as
// it remembers. It knows example.com's nameserver and its address.
func stale(t *testing.T) *fake {
	return serve(t, func(q dns.Question, m *dns.Msg) {
		m.RecursionAvailable = true
		switch {
		case q.Qtype == dns.TypeNS && q.Name == "example.com.":
			m.Answer = append(m.Answer, rr(t, "example.com. 300 IN NS ns1.example.com."))
		case q.Qtype == dns.TypeA && q.Name == "ns1.example.com.":
			m.Answer = append(m.Answer, rr(t, "ns1.example.com. 300 IN A 127.0.0.1"))
		default:
			m.Rcode = dns.RcodeNameError
			m.Ns = append(m.Ns, rr(t, "example.com. 1800 IN SOA ns1.example.com. h.example.com. 1 1 1 1 1800"))
		}
	})
}

// authority is example.com's nameserver, with the challenge CNAME of
// target if it is set.
func authority(t *testing.T, cname string) *fake {
	return serve(t, func(q dns.Question, m *dns.Msg) {
		m.Authoritative = true
		if q.Name == challenge && cname != "" {
			m.Answer = append(m.Answer, rr(t, challenge+" 300 IN CNAME "+cname))
			return
		}
		m.Rcode = dns.RcodeNameError
		m.Ns = append(m.Ns, rr(t, "example.com. 1800 IN SOA ns1.example.com. h.example.com. 1 1 1 1 1800"))
	})
}

func at(addrs map[string]string) func(string, netip.Addr) string {
	return func(host string, _ netip.Addr) string { return addrs[host] }
}

func lookup(t *testing.T, r *dnscheck.Resolver) dnscheck.Answer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ans, err := r.CNAME(ctx, "_acme-challenge.Example.com")
	if err != nil {
		t.Fatal(err)
	}
	return ans
}

// TestARecordJustMadeIsSeenThoughTheResolverRemembersNone: the resolver
// still answers that the challenge record does not exist; the zone's own
// nameserver, asked without recursion, has it.
func TestARecordJustMadeIsSeenThoughTheResolverRemembersNone(t *testing.T) {
	rec, auth := stale(t), authority(t, target)
	ans := lookup(t, &dnscheck.Resolver{Recursive: []string{rec.addr}, Nameserver: at(map[string]string{"ns1.example.com.": auth.addr})})
	if ans.Target != strings.TrimSuffix(target, ".") || ans.From != "ns1.example.com" {
		t.Errorf("got %+v; want %s from ns1.example.com", ans, target)
	}
	if n := rec.count(challenge + "/CNAME"); n != 0 {
		t.Errorf("the resolver was asked for the record itself %d times", n)
	}
	// A nameserver that also resolves for others would answer a question
	// that asks for recursion from its own cache.
	if n := auth.countRecursed(challenge + "/CNAME"); n != 0 || auth.count(challenge+"/CNAME") == 0 {
		t.Errorf("the nameserver was asked %d times, %d of them for recursion", auth.count(challenge+"/CNAME"), n)
	}
}

// TestNoRecordIsNoneFromTheNameserver: the nameserver says there is none;
// that is the answer, and the resolver is not asked instead.
func TestNoRecordIsNoneFromTheNameserver(t *testing.T) {
	rec, auth := stale(t), authority(t, "")
	ans := lookup(t, &dnscheck.Resolver{Recursive: []string{rec.addr}, Nameserver: at(map[string]string{"ns1.example.com.": auth.addr})})
	if ans.Target != "" || ans.From != "ns1.example.com" {
		t.Errorf("got %+v; want none from ns1.example.com", ans)
	}
}

// TestAZoneBelowTheDomainIsFollowed: example.com delegates
// _acme-challenge.example.com to a nameserver of its own, which holds the
// record.
func TestAZoneBelowTheDomainIsFollowed(t *testing.T) {
	rec := stale(t)
	sub := serve(t, func(q dns.Question, m *dns.Msg) {
		m.Authoritative = true
		m.Answer = append(m.Answer, rr(t, challenge+" 300 IN CNAME "+target))
	})
	parent := serve(t, func(q dns.Question, m *dns.Msg) {
		m.Ns = append(m.Ns, rr(t, challenge+" 300 IN NS ns.sub.example.net."))
		m.Extra = append(m.Extra, rr(t, "ns.sub.example.net. 300 IN A 127.0.0.2"))
	})
	ans := lookup(t, &dnscheck.Resolver{Recursive: []string{rec.addr}, Nameserver: at(map[string]string{
		"ns1.example.com.": parent.addr, "ns.sub.example.net.": sub.addr,
	})})
	if ans.Target != strings.TrimSuffix(target, ".") || ans.From != "ns.sub.example.net" {
		t.Errorf("got %+v; want %s from ns.sub.example.net", ans, target)
	}
}

// TestTheResolverAnswersWhenNoNameserverDoes: with the zone's nameserver
// unreachable, the resolver's answer is taken, and says it is the
// resolver's.
func TestTheResolverAnswersWhenNoNameserverDoes(t *testing.T) {
	rec := stale(t)
	closed, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone := closed.LocalAddr().String()
	_ = closed.Close()
	ans := lookup(t, &dnscheck.Resolver{Recursive: []string{rec.addr}, Timeout: 300 * time.Millisecond,
		Nameserver: at(map[string]string{"ns1.example.com.": gone})})
	if ans.Target != "" || ans.From != "the resolver at "+rec.addr {
		t.Errorf("got %+v; want none from the resolver", ans)
	}
}

// TestTheZoneIsFoundAboveACNAME: a resolver that answers the NS question
// for the challenge name with its CNAME, as a fresh one does, still leads
// to example.com's nameserver.
func TestTheZoneIsFoundAboveACNAME(t *testing.T) {
	rec := serve(t, func(q dns.Question, m *dns.Msg) {
		switch {
		case q.Name == challenge:
			m.Answer = append(m.Answer, rr(t, challenge+" 300 IN CNAME "+target))
		case q.Qtype == dns.TypeNS && q.Name == "example.com.":
			m.Answer = append(m.Answer, rr(t, "example.com. 300 IN NS ns1.example.com."))
		case q.Qtype == dns.TypeA && q.Name == "ns1.example.com.":
			m.Answer = append(m.Answer, rr(t, "ns1.example.com. 300 IN A 127.0.0.1"))
		}
	})
	auth := authority(t, target)
	ans := lookup(t, &dnscheck.Resolver{Recursive: []string{rec.addr}, Nameserver: at(map[string]string{"ns1.example.com.": auth.addr})})
	if ans.From != "ns1.example.com" {
		t.Errorf("got %+v; want an answer from ns1.example.com", ans)
	}
}
