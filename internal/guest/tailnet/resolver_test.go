package tailnet

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

const (
	hangarResolver = "10.0.0.1:53"
	otherResolver  = "10.0.1.1:53"
)

// recorder answers each query with an A record naming the server asked,
// as 192.0.2.N, failing for the servers in down.
type recorder struct {
	mu    sync.Mutex
	asked []string
	down  map[string]bool
}

func (r *recorder) exchange(_ context.Context, m *dns.Msg, _, addr string) (*dns.Msg, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, addr)
	if r.down[addr] {
		return nil, errors.New("no answer")
	}
	res := new(dns.Msg).SetReply(m)
	ip := map[string]string{hangarResolver: "192.0.2.1", otherResolver: "192.0.2.2", MagicDNS: "192.0.2.100"}[addr]
	res.Answer = append(res.Answer, &dns.A{Hdr: dns.RR_Header{Name: m.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP(ip)})
	return res, nil
}

func query(t *testing.T, r *Resolver, name string) *dns.Msg {
	t.Helper()
	req := new(dns.Msg).SetQuestion(dns.Fqdn(name), dns.TypeA)
	req.Id = 4242
	res := r.answer(t.Context(), req, "udp")
	if res.Id != req.Id {
		t.Errorf("%s: answered as query %d, asked as %d", name, res.Id, req.Id)
	}
	return res
}

func answeredBy(res *dns.Msg) string {
	if len(res.Answer) != 1 {
		return dns.RcodeToString[res.Rcode]
	}
	return res.Answer[0].(*dns.A).A.String()
}

// TestTheDNSSplit: a name under the tailnet's MagicDNS suffix goes to
// tailscaled's resolver; every other name, the platform's own
// *.hangar.internal among them, to the guest's own resolvers, as without
// the tailnet.
func TestTheDNSSplit(t *testing.T) {
	rec := &recorder{}
	r := &Resolver{Upstreams: []string{hangarResolver, otherResolver}, MagicDNS: MagicDNS, exchange: rec.exchange}

	// Before the node has joined, the tailnet answers nothing.
	if got := answeredBy(query(t, r, "box.tail1234.ts.net")); got != "192.0.2.1" {
		t.Errorf("a tailnet name before the join: answered by %s, want the guest's resolver", got)
	}

	r.SetSuffix("tail1234.ts.net.")
	for name, want := range map[string]string{
		"web.default.hangar.internal": "192.0.2.1",
		"web":                         "192.0.2.1",
		"example.com":                 "192.0.2.1",
		"box.tail1234.ts.net":         "192.0.2.100",
		"Box.Tail1234.TS.net":         "192.0.2.100",
		"tail1234.ts.net":             "192.0.2.100",
		// Another tailnet's, or a name ending the same way, is not this
		// tailnet's.
		"box.tail9999.ts.net":     "192.0.2.1",
		"box.eviltail1234.ts.net": "192.0.2.1",
	} {
		if got := answeredBy(query(t, r, name)); got != want {
			t.Errorf("%s: answered by %s, want %s", name, got, want)
		}
	}

	// The guest's resolvers are asked in order, the next when one fails;
	// none answering is SERVFAIL.
	rec.down = map[string]bool{hangarResolver: true}
	if got := answeredBy(query(t, r, "example.com")); got != "192.0.2.2" {
		t.Errorf("with the first resolver down: answered by %s, want the second", got)
	}
	rec.down[otherResolver] = true
	if got := query(t, r, "example.com"); got.Rcode != dns.RcodeServerFailure {
		t.Errorf("with every resolver down: %s, want SERVFAIL", dns.RcodeToString[got.Rcode])
	}
	// A tailnet name never falls back to the guest's resolvers.
	rec.asked = nil
	rec.down = map[string]bool{MagicDNS: true}
	if got := query(t, r, "box.tail1234.ts.net"); got.Rcode != dns.RcodeServerFailure || !slices.Equal(rec.asked, []string{MagicDNS}) {
		t.Errorf("a tailnet name with tailscaled down: %s, asked %q; want SERVFAIL from it alone", dns.RcodeToString[got.Rcode], rec.asked)
	}

	r.SetSuffix("")
	if got := r.Route("box.tail1234.ts.net."); !slices.Equal(got, r.Upstreams) {
		t.Errorf("once the suffix is gone: %q", got)
	}
}

// TestTheResolverServes: Serve answers over UDP and over TCP, on the port
// UDP took, with what the route's server says.
func TestTheResolverServes(t *testing.T) {
	rec := &recorder{}
	r := &Resolver{Upstreams: []string{hangarResolver}, MagicDNS: MagicDNS, exchange: rec.exchange}
	r.SetSuffix("tail1234.ts.net")
	addr, err := r.Serve("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	for _, network := range []string{"udp", "tcp"} {
		for name, want := range map[string]string{"web.default.hangar.internal.": "192.0.2.1", "box.tail1234.ts.net.": "192.0.2.100"} {
			c := &dns.Client{Net: network, Timeout: 5 * time.Second}
			res, _, err := c.Exchange(new(dns.Msg).SetQuestion(name, dns.TypeA), addr.String())
			if err != nil {
				t.Fatalf("%s %s: %v", network, name, err)
			}
			if got := answeredBy(res); got != want {
				t.Errorf("%s %s: answered by %s, want %s", network, name, got, want)
			}
		}
	}
}

func TestUpstreams(t *testing.T) {
	got, err := Upstreams([]string{"10.0.0.1", "fd00::1"})
	if err != nil || !slices.Equal(got, []string{"10.0.0.1:53", "[fd00::1]:53"}) {
		t.Errorf("%q, %v", got, err)
	}
	if _, err := Upstreams([]string{"resolver.example"}); err == nil {
		t.Error("a name taken as a nameserver")
	}
}
