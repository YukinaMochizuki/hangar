package resolver

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

var upstream = netip.MustParseAddrPort("192.0.2.53:53")

// fakeUpstream answers every A question with one address, TTL 60, and
// counts the questions it was asked.
type fakeUpstream struct {
	asked atomic.Int32
	fail  atomic.Bool
	gate  chan struct{} // if set, every answer waits for it
}

func (u *fakeUpstream) exchange(_ context.Context, m *dns.Msg, _ netip.AddrPort) (*dns.Msg, error) {
	u.asked.Add(1)
	if u.gate != nil {
		<-u.gate
	}
	if u.fail.Load() {
		return nil, errors.New("timed out")
	}
	resp := new(dns.Msg).SetReply(m)
	q := m.Question[0]
	switch q.Name {
	case "missing.example.":
		resp.Rcode = dns.RcodeNameError
		resp.Ns = []dns.RR{&dns.SOA{Hdr: dns.RR_Header{Name: "example.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 300},
			Ns: "ns.example.", Mbox: "h.example.", Minttl: 30}}
	case "refused.example.":
		resp.Rcode = dns.RcodeRefused
	case "big.example.":
		for i := range 100 {
			resp.Answer = append(resp.Answer, &dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A: net.IPv4(198, 51, 100, byte(i))})
		}
	default:
		resp.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A: net.IPv4(198, 51, 100, 7)}}
	}
	return resp, nil
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newResolver(t *testing.T) (*Resolver, *fakeUpstream, *clock) {
	t.Helper()
	u := &fakeUpstream{}
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	r := New(Config{Upstreams: []netip.AddrPort{upstream}, Exchange: u.exchange, Now: c.Now})
	r.Set("n1", Records{Project: "Web", Names: map[string][]netip.Addr{
		"api": {netip.MustParseAddr("10.0.0.2")},
		"db":  {netip.MustParseAddr("10.0.0.3")},
	}})
	return r, u, c
}

func ask(t *testing.T, r *Resolver, network, name string, qtype uint16) *dns.Msg {
	t.Helper()
	q := new(dns.Msg).SetQuestion(name, qtype)
	return r.Answer(t.Context(), network, q)
}

func addrs(m *dns.Msg) []string {
	var out []string
	for _, rr := range m.Answer {
		if a, ok := rr.(*dns.A); ok {
			out = append(out, a.A.String())
		}
	}
	return out
}

func TestInternalNames(t *testing.T) {
	r, u, _ := newResolver(t)
	for _, tc := range []struct {
		name  string
		qtype uint16
		rcode int
		want  []string
	}{
		{"api.web.hangar.internal.", dns.TypeA, dns.RcodeSuccess, []string{"10.0.0.2"}},
		{"API.Web.Hangar.Internal.", dns.TypeA, dns.RcodeSuccess, []string{"10.0.0.2"}},
		{"api.web.hangar.internal.", dns.TypeAAAA, dns.RcodeSuccess, nil},
		{"web.hangar.internal.", dns.TypeA, dns.RcodeSuccess, nil},
		{"hangar.internal.", dns.TypeA, dns.RcodeSuccess, nil},
		{"nope.web.hangar.internal.", dns.TypeA, dns.RcodeNameError, nil},
		{"api.other.hangar.internal.", dns.TypeA, dns.RcodeNameError, nil},
		{"x.api.web.hangar.internal.", dns.TypeA, dns.RcodeNameError, nil},
		{"other.hangar.internal.", dns.TypeA, dns.RcodeNameError, nil},
	} {
		got := ask(t, r, "n1", tc.name, tc.qtype)
		if got.Rcode != tc.rcode || !equal(addrs(got), tc.want) || !got.Authoritative || !got.RecursionAvailable {
			t.Errorf("%s %s: rcode %s, %v, aa %v, ra %v; want %s, %v",
				tc.name, dns.TypeToString[tc.qtype], dns.RcodeToString[got.Rcode], addrs(got), got.Authoritative, got.RecursionAvailable,
				dns.RcodeToString[tc.rcode], tc.want)
		}
		if len(got.Answer) == 0 && (len(got.Ns) != 1 || got.Ns[0].Header().Rrtype != dns.TypeSOA) {
			t.Errorf("%s: a negative answer without the zone's SOA: %v", tc.name, got.Ns)
		}
		for _, rr := range got.Answer {
			if rr.Header().Ttl != TTL {
				t.Errorf("%s: TTL %d", tc.name, rr.Header().Ttl)
			}
		}
	}
	if n := u.asked.Load(); n != 0 {
		t.Errorf("the internal zone was forwarded %d times", n)
	}
}

func TestOtherNetworks(t *testing.T) {
	r, _, _ := newResolver(t)
	r.Set("n2", Records{Project: "web", Names: map[string][]netip.Addr{"cache": {netip.MustParseAddr("10.1.0.2")}}})
	if got := ask(t, r, "n2", "api.web.hangar.internal.", dns.TypeA); got.Rcode != dns.RcodeNameError {
		t.Errorf("a machine of another network of the project: %s %v", dns.RcodeToString[got.Rcode], addrs(got))
	}
	if got := ask(t, r, "n1", "cache.web.hangar.internal.", dns.TypeA); got.Rcode != dns.RcodeNameError {
		t.Errorf("a machine of another network of the project: %s %v", dns.RcodeToString[got.Rcode], addrs(got))
	}
}

func TestUnsyncedIsServfail(t *testing.T) {
	r, _, _ := newResolver(t)
	for _, network := range []string{"n9", "n1"} {
		if network == "n1" {
			r.Forget("n1")
		}
		if got := ask(t, r, network, "api.web.hangar.internal.", dns.TypeA); got.Rcode != dns.RcodeServerFailure {
			t.Errorf("network %s without records: %s, want SERVFAIL", network, dns.RcodeToString[got.Rcode])
		}
	}
	// Names outside the zone need no records.
	if got := ask(t, r, "n9", "example.com.", dns.TypeA); got.Rcode != dns.RcodeSuccess {
		t.Errorf("forwarding without records: %s", dns.RcodeToString[got.Rcode])
	}
}

func TestForwardingCaches(t *testing.T) {
	r, u, c := newResolver(t)
	first := ask(t, r, "n1", "example.com.", dns.TypeA)
	if got := addrs(first); !equal(got, []string{"198.51.100.7"}) || first.Authoritative || !first.RecursionAvailable {
		t.Fatalf("forwarded answer %v aa %v ra %v", got, first.Authoritative, first.RecursionAvailable)
	}
	c.advance(20 * time.Second)
	second := ask(t, r, "n2", "Example.COM.", dns.TypeA)
	if u.asked.Load() != 1 {
		t.Errorf("asked upstream %d times, want once: the answer is cached", u.asked.Load())
	}
	if ttl := second.Answer[0].Header().Ttl; ttl != 40 {
		t.Errorf("cached answer's TTL %d, want 40 after 20s of 60", ttl)
	}
	if second.Question[0].Name != "Example.COM." {
		t.Errorf("answer's question %q, want the one asked", second.Question[0].Name)
	}
	c.advance(41 * time.Second)
	ask(t, r, "n1", "example.com.", dns.TypeA)
	if u.asked.Load() != 2 {
		t.Errorf("asked upstream %d times, want twice: the answer expired", u.asked.Load())
	}

	// A missing name is kept for what its SOA says.
	ask(t, r, "n1", "missing.example.", dns.TypeA)
	c.advance(29 * time.Second)
	if got := ask(t, r, "n1", "missing.example.", dns.TypeA); got.Rcode != dns.RcodeNameError || u.asked.Load() != 3 {
		t.Errorf("negative answer %s, asked %d times", dns.RcodeToString[got.Rcode], u.asked.Load())
	}
	c.advance(2 * time.Second)
	ask(t, r, "n1", "missing.example.", dns.TypeA)
	if u.asked.Load() != 4 {
		t.Errorf("asked %d times; a negative answer outlived its SOA minimum", u.asked.Load())
	}
}

func TestUpstreamFailures(t *testing.T) {
	r, u, _ := newResolver(t)
	u.fail.Store(true)
	if got := ask(t, r, "n1", "example.com.", dns.TypeA); got.Rcode != dns.RcodeServerFailure {
		t.Errorf("every upstream failing: %s, want SERVFAIL", dns.RcodeToString[got.Rcode])
	}
	if n := u.asked.Load(); n != 2 {
		t.Errorf("asked %d times, want two tries", n)
	}
	u.fail.Store(false)
	if got := ask(t, r, "n1", "refused.example.", dns.TypeA); got.Rcode != dns.RcodeServerFailure {
		t.Errorf("upstream refusing: %s, want SERVFAIL", dns.RcodeToString[got.Rcode])
	}
	ask(t, r, "n1", "refused.example.", dns.TypeA)
	if n := u.asked.Load(); n != 6 {
		t.Errorf("asked %d times; a refusal must not be cached", n)
	}
}

func TestQuestionsInFlightAreAskedOnce(t *testing.T) {
	u := &fakeUpstream{gate: make(chan struct{})}
	r := New(Config{Upstreams: []netip.AddrPort{upstream}, Exchange: u.exchange})
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if got := ask(t, r, "n1", "example.com.", dns.TypeA); len(got.Answer) != 1 {
				t.Errorf("answer %v", got)
			}
		})
	}
	for u.asked.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // the others join the question asked
	close(u.gate)
	wg.Wait()
	if n := u.asked.Load(); n != 1 {
		t.Errorf("asked upstream %d times for one question asked ten times at once", n)
	}
}

// TestANetworkTakesOnlyItsShare: a network's questions past its bound fail
// at once, and take nothing from another network's.
func TestANetworkTakesOnlyItsShare(t *testing.T) {
	u := &fakeUpstream{gate: make(chan struct{})}
	r := New(Config{Upstreams: []netip.AddrPort{upstream}, Exchange: u.exchange, MaxQuestions: 1})
	r.Set("n2", Records{Project: "web", Names: map[string][]netip.Addr{"cache": {netip.MustParseAddr("10.1.0.2")}}})
	udp1 := serveUDP(t, r, "n1")
	udp2 := serveUDP(t, r, "n2")
	slow := make(chan *dns.Msg, 1)
	go func() { slow <- askUDP(t, udp1, "slow.example.") }()
	for u.asked.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if got := askUDP(t, udp1, "other.example."); got.Rcode != dns.RcodeServerFailure {
		t.Errorf("a question past the network's bound: %s, want SERVFAIL", dns.RcodeToString[got.Rcode])
	}
	if got := askUDP(t, udp2, "cache.web.hangar.internal."); !equal(addrs(got), []string{"10.1.0.2"}) {
		t.Errorf("another network, while the first is at its bound: %v", got)
	}
	close(u.gate)
	if got := <-slow; got.Rcode != dns.RcodeSuccess {
		t.Errorf("the question in flight: %s", dns.RcodeToString[got.Rcode])
	}
}

// TestStoppingClosesConnections: a resolver told to stop ends, and its TCP
// clients are let go, however busy they keep their connections.
func TestStoppingClosesConnections(t *testing.T) {
	r, _, _ := newResolver(t)
	tcp, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.ServeTCP(ctx, "n1", tcp) }()
	conn, err := dns.Dial("tcp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ask := func() error {
		if err := conn.WriteMsg(new(dns.Msg).SetQuestion("api.web.hangar.internal.", dns.TypeA)); err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		_, err := conn.ReadMsg()
		return err
	}
	if err := ask(); err != nil {
		t.Fatal(err)
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the resolver did not stop while a client kept its connection")
	}
	if err := ask(); err == nil {
		t.Error("the connection is still answered after the resolver stopped")
	}
}

func serveUDP(t *testing.T, r *Resolver, network string) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.ServeUDP(ctx, network, c) }()
	t.Cleanup(func() { stop(); <-done })
	return c
}

func askUDP(t *testing.T, at *net.UDPConn, name string) *dns.Msg {
	t.Helper()
	c := &dns.Client{Net: "udp", Timeout: 5 * time.Second}
	got, _, err := c.Exchange(new(dns.Msg).SetQuestion(name, dns.TypeA), at.LocalAddr().String())
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got
}

func TestTooManyForwards(t *testing.T) {
	u := &fakeUpstream{gate: make(chan struct{})}
	r := New(Config{Upstreams: []netip.AddrPort{upstream}, Exchange: u.exchange, MaxForwards: 1})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ask(t, r, "n1", "one.example.", dns.TypeA)
	}()
	for u.asked.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if got := ask(t, r, "n1", "two.example.", dns.TypeA); got.Rcode != dns.RcodeServerFailure {
		t.Errorf("a question past the bound: %s, want SERVFAIL", dns.RcodeToString[got.Rcode])
	}
	close(u.gate)
	<-done
}

func TestRefusesWhatIsNotAQuestion(t *testing.T) {
	r, _, _ := newResolver(t)
	update := new(dns.Msg).SetUpdate("hangar.internal.")
	if got := r.Answer(t.Context(), "n1", update); got.Rcode != dns.RcodeNotImplemented {
		t.Errorf("an update: %s", dns.RcodeToString[got.Rcode])
	}
	none := new(dns.Msg)
	if got := r.Answer(t.Context(), "n1", none); got.Rcode != dns.RcodeFormatError {
		t.Errorf("no question: %s", dns.RcodeToString[got.Rcode])
	}
}

// TestOverSockets serves the resolver as a network has it, and asks it as
// a guest does: over UDP, truncated past what the guest takes, and over TCP.
func TestOverSockets(t *testing.T) {
	r, _, _ := newResolver(t)
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := r.ServeUDP(ctx, "n1", udp); err != nil {
			t.Error(err)
		}
	})
	wg.Go(func() {
		if err := r.ServeTCP(ctx, "n1", tcp); err != nil {
			t.Error(err)
		}
	})
	defer wg.Wait()
	defer stop()

	c := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
	got, _, err := c.Exchange(new(dns.Msg).SetQuestion("db.web.hangar.internal.", dns.TypeA), udp.LocalAddr().String())
	if err != nil || !equal(addrs(got), []string{"10.0.0.3"}) {
		t.Fatalf("over UDP: %v %v", got, err)
	}
	big := new(dns.Msg).SetQuestion("big.example.", dns.TypeA)
	got, _, err = c.Exchange(big, udp.LocalAddr().String())
	if err != nil || !got.Truncated || wireSize(t, got) > dns.MinMsgSize {
		t.Errorf("a large answer to a question without EDNS: truncated %v, %d bytes, %v", got.Truncated, wireSize(t, got), err)
	}
	big.SetEdns0(4096, false)
	got, _, err = c.Exchange(big, udp.LocalAddr().String())
	if err != nil || !got.Truncated || wireSize(t, got) > maxUDPSize {
		t.Errorf("a large answer to a question offering 4096 bytes: truncated %v, %d bytes, %v", got.Truncated, wireSize(t, got), err)
	}
	c.Net = "tcp"
	got, _, err = c.Exchange(big, tcp.Addr().String())
	if err != nil || got.Truncated || len(addrs(got)) != 100 {
		t.Errorf("over TCP: truncated %v, %d answers, %v", got.Truncated, len(addrs(got)), err)
	}
}

// wireSize is the size m had on the wire, compressed as the server sent it.
func wireSize(t *testing.T, m *dns.Msg) int {
	t.Helper()
	m = m.Copy()
	m.Compress = true
	b, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
