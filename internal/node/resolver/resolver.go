// Package resolver is the resolver the node serves each network on its
// gateway (docs/decisions/networking.md). It answers for the machines of the
// network under the zone hangar.internal, as <name>.<project>.hangar.internal,
// and forwards every other name to the node's upstream resolvers, caching
// what they answer. A network's records are set by the agent from the API;
// until they are, its internal names fail with SERVFAIL rather than NXDOMAIN,
// so a client retries instead of caching an answer that may be wrong.
package resolver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/netutil"
	"golang.org/x/sync/singleflight"
)

// Zone is the only zone the resolver answers for itself. It is never
// forwarded.
const Zone = "hangar.internal."

// TTL is the time to live of every internal answer: short, so a machine
// recreated with another address is seen soon.
const TTL = 5

// maxUDPSize caps an answer over UDP, beyond which it is truncated: the size
// DNS Flag Day 2020 settled on, which no path fragments.
const maxUDPSize = 1232

// Config configures a Resolver.
type Config struct {
	// Upstreams are the resolvers every name outside Zone is forwarded to,
	// tried in turn.
	Upstreams []netip.AddrPort
	// Timeout bounds one try of an upstream; zero means two seconds.
	Timeout time.Duration
	// Tries is how many upstream tries a question gets before it fails with
	// SERVFAIL; zero means two.
	Tries int
	// MaxQuestions bounds the questions of one network being answered at
	// once, so no network's machines take what the others need; zero means
	// 128. MaxForwards bounds those being forwarded, over every network;
	// zero means 1024. A question past either fails with SERVFAIL at once.
	MaxQuestions, MaxForwards int
	// MaxTCPConns bounds the TCP connections of one network; zero means 32.
	// Past it, a connection waits in the listen queue.
	MaxTCPConns int
	// ShutdownGrace bounds how long questions being answered hold up a
	// network's resolver stopping; zero means a second.
	ShutdownGrace time.Duration
	// CacheSize bounds the answers kept; zero means 10000.
	CacheSize int
	// Exchange asks an upstream; nil means over the network, falling back to
	// TCP when the UDP answer is truncated. Tests replace it.
	Exchange func(ctx context.Context, m *dns.Msg, upstream netip.AddrPort) (*dns.Msg, error)
	Now      func() time.Time
	Log      *slog.Logger
}

// Records are one network's internal names: the addresses of each machine
// name in the network's project.
type Records struct {
	Project string
	Names   map[string][]netip.Addr
}

// Resolver answers the networks' questions. It is safe for concurrent use.
type Resolver struct {
	cfg      Config
	cache    *cache
	flight   singleflight.Group
	forwards chan struct{}

	mu      sync.RWMutex
	records map[string]Records // by network uid
	// questions holds a slot for each question of a network being
	// answered, by network uid.
	questions map[string]chan struct{}
}

// New returns a resolver.
func New(cfg Config) *Resolver {
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.Tries == 0 {
		cfg.Tries = 2
	}
	if cfg.MaxQuestions == 0 {
		cfg.MaxQuestions = 128
	}
	if cfg.MaxForwards == 0 {
		cfg.MaxForwards = 1024
	}
	if cfg.MaxTCPConns == 0 {
		cfg.MaxTCPConns = 32
	}
	if cfg.ShutdownGrace == 0 {
		cfg.ShutdownGrace = time.Second
	}
	if cfg.CacheSize == 0 {
		cfg.CacheSize = 10000
	}
	if cfg.Exchange == nil {
		cfg.Exchange = exchange(cfg.Timeout)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Resolver{
		cfg: cfg, cache: newCache(cfg.CacheSize), forwards: make(chan struct{}, cfg.MaxForwards),
		records: map[string]Records{}, questions: map[string]chan struct{}{},
	}
}

// Set replaces a network's records.
func (r *Resolver) Set(networkUID string, recs Records) {
	names := make(map[string][]netip.Addr, len(recs.Names))
	for n, addrs := range recs.Names {
		names[strings.ToLower(n)] = addrs
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records[networkUID] = Records{Project: strings.ToLower(recs.Project), Names: names}
}

// Forget drops a network's records.
func (r *Resolver) Forget(networkUID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.records, networkUID)
	delete(r.questions, networkUID)
}

// slots returns the network's question slots.
func (r *Resolver) slots(networkUID string) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.questions[networkUID]
	if s == nil {
		s = make(chan struct{}, r.cfg.MaxQuestions)
		r.questions[networkUID] = s
	}
	return s
}

// Search is the search domain of a network's machines in project, so a bare
// name finds a machine of the same project.
func Search(project string) string {
	return strings.ToLower(project) + "." + strings.TrimSuffix(Zone, ".")
}

// ServeUDP and ServeTCP answer a network's questions on its socket until ctx
// is done or the socket is closed.
func (r *Resolver) ServeUDP(ctx context.Context, networkUID string, c net.PacketConn) error {
	return r.serve(ctx, &dns.Server{PacketConn: c, Handler: r.handler(networkUID, true)})
}

func (r *Resolver) ServeTCP(ctx context.Context, networkUID string, l net.Listener) error {
	l = netutil.LimitListener(l, r.cfg.MaxTCPConns)
	return r.serve(ctx, &dns.Server{Listener: l, Handler: r.handler(networkUID, false)})
}

// serve runs srv until ctx is done or its socket is closed, then shuts it
// down: closing its socket alone leaves its TCP connections served for as
// long as their clients keep them busy.
func (r *Resolver) serve(ctx context.Context, srv *dns.Server) error {
	started := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(started) }
	done := make(chan error, 1)
	go func() { done <- srv.ActivateAndServe() }()
	var err error
	select {
	case err = <-done:
		return ignoreClosed(err)
	case <-started:
	}
	ended := false
	select {
	case err = <-done:
		ended = true
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), r.cfg.ShutdownGrace)
	defer cancel()
	_ = srv.ShutdownContext(sctx)
	if !ended {
		err = <-done
	}
	return ignoreClosed(err)
}

func ignoreClosed(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (r *Resolver) handler(networkUID string, udp bool) dns.Handler {
	return dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		var resp *dns.Msg
		slots := r.slots(networkUID)
		select {
		case slots <- struct{}{}:
			resp = r.Answer(context.Background(), networkUID, q)
			<-slots
		default:
			resp = new(dns.Msg).SetRcode(q, dns.RcodeServerFailure)
		}
		size := dns.MaxMsgSize
		if udp {
			size = dns.MinMsgSize
			if opt := q.IsEdns0(); opt != nil {
				size = max(min(int(opt.UDPSize()), maxUDPSize), dns.MinMsgSize)
			}
		}
		resp.Truncate(size)
		_ = w.WriteMsg(resp)
	})
}

// Answer answers one question asked on a network.
func (r *Resolver) Answer(ctx context.Context, networkUID string, q *dns.Msg) *dns.Msg {
	resp := new(dns.Msg)
	if q.Opcode != dns.OpcodeQuery {
		return resp.SetRcode(q, dns.RcodeNotImplemented)
	}
	if len(q.Question) != 1 {
		return resp.SetRcode(q, dns.RcodeFormatError)
	}
	question := q.Question[0]
	name := strings.ToLower(question.Name)
	var out *dns.Msg
	if name == Zone || strings.HasSuffix(name, "."+Zone) {
		out = r.internal(networkUID, q, name)
	} else {
		out = r.forward(ctx, q, name)
	}
	out.RecursionAvailable = true
	if opt := q.IsEdns0(); opt != nil {
		out.SetEdns0(maxUDPSize, false)
	}
	return out
}

// internal answers a name under Zone from the network's records.
func (r *Resolver) internal(networkUID string, q *dns.Msg, name string) *dns.Msg {
	resp := new(dns.Msg)
	r.mu.RLock()
	recs, synced := r.records[networkUID]
	r.mu.RUnlock()
	if !synced {
		return resp.SetRcode(q, dns.RcodeServerFailure)
	}
	resp.SetReply(q)
	resp.Authoritative = true
	question := q.Question[0]
	projectZone := recs.Project + "." + Zone
	machine, inProject := strings.CutSuffix(name, "."+projectZone)
	switch {
	case name == Zone:
		if question.Qtype == dns.TypeSOA || question.Qtype == dns.TypeANY {
			resp.Answer = []dns.RR{soa()}
		}
	case name == projectZone:
		// A name with machines under it, and nothing of its own.
	case inProject && !strings.Contains(machine, ".") && len(recs.Names[machine]) > 0:
		if question.Qtype == dns.TypeA || question.Qtype == dns.TypeANY {
			for _, a := range recs.Names[machine] {
				resp.Answer = append(resp.Answer, &dns.A{
					Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: TTL},
					A:   a.AsSlice(),
				})
			}
		}
	default:
		resp.Rcode = dns.RcodeNameError
	}
	if len(resp.Answer) == 0 {
		resp.Ns = []dns.RR{soa()}
	}
	return resp
}

func soa() *dns.SOA {
	return &dns.SOA{
		Hdr: dns.RR_Header{Name: Zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: TTL},
		Ns:  "ns." + Zone, Mbox: "hostmaster." + Zone,
		Serial: 1, Refresh: 3600, Retry: 600, Expire: 86400, Minttl: TTL,
	}
}

// forward answers a name outside Zone from the cache, or else from the
// upstreams, asking them once for every question asked at the same time.
func (r *Resolver) forward(ctx context.Context, q *dns.Msg, name string) *dns.Msg {
	question := q.Question[0]
	key := cacheKey{name: name, qtype: question.Qtype, qclass: question.Qclass}
	if hit, ok := r.cache.get(key, r.cfg.Now()); ok {
		return reply(q, hit)
	}
	v, err, _ := r.flight.Do(fmt.Sprintf("%s/%d/%d", name, question.Qtype, question.Qclass), func() (any, error) {
		select {
		case r.forwards <- struct{}{}:
			defer func() { <-r.forwards }()
		default:
			return nil, errors.New("too many questions being forwarded")
		}
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(question.Name), question.Qtype)
		m.Question[0].Qclass = question.Qclass
		m.RecursionDesired = true
		m.SetEdns0(maxUDPSize, false)
		answer, err := r.ask(ctx, m)
		if err != nil {
			return nil, err
		}
		r.cache.put(key, answer, r.cfg.Now())
		return answer, nil
	})
	if err != nil {
		r.cfg.Log.Debug("forwarding a question failed", "name", name, "error", err)
		return new(dns.Msg).SetRcode(q, dns.RcodeServerFailure)
	}
	return reply(q, v.(*dns.Msg))
}

// ask tries the upstreams in turn, and takes the first answer that is one:
// a name found, or found not to exist.
func (r *Resolver) ask(ctx context.Context, m *dns.Msg) (*dns.Msg, error) {
	if len(r.cfg.Upstreams) == 0 {
		return nil, errors.New("no upstream resolvers")
	}
	var errs []error
	for i := range r.cfg.Tries {
		up := r.cfg.Upstreams[i%len(r.cfg.Upstreams)]
		tctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
		answer, err := r.cfg.Exchange(tctx, m.Copy(), up)
		cancel()
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", up, err))
		case answer.Rcode != dns.RcodeSuccess && answer.Rcode != dns.RcodeNameError:
			errs = append(errs, fmt.Errorf("%s: %s", up, dns.RcodeToString[answer.Rcode]))
		default:
			return answer, nil
		}
	}
	return nil, errors.Join(errs...)
}

// reply is answer as the reply to q.
func reply(q, answer *dns.Msg) *dns.Msg {
	resp := answer.Copy()
	resp.Id = q.Id
	resp.Response = true
	resp.Authoritative = false
	resp.RecursionDesired = q.RecursionDesired
	resp.CheckingDisabled = q.CheckingDisabled
	resp.Question = q.Question
	resp.Extra = nil
	return resp
}

func exchange(timeout time.Duration) func(context.Context, *dns.Msg, netip.AddrPort) (*dns.Msg, error) {
	udp := &dns.Client{Net: "udp", Timeout: timeout, UDPSize: maxUDPSize}
	tcp := &dns.Client{Net: "tcp", Timeout: timeout}
	return func(ctx context.Context, m *dns.Msg, upstream netip.AddrPort) (*dns.Msg, error) {
		answer, _, err := udp.ExchangeContext(ctx, m, upstream.String())
		if err == nil && answer.Truncated {
			answer, _, err = tcp.ExchangeContext(ctx, m, upstream.String())
		}
		return answer, err
	}
}
