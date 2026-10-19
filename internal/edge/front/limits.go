package front

import (
	"container/list"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/YukinaMochizuki/hangar/internal/execws"
)

// Limits are what the front door allows each client and all of them, as
// the host's only listener on :443 and :80. A client is an IPv4 address or
// an IPv6 /64, which is what one subscriber usually holds; the IPv4 /24,
// and the IPv6 /56 and /48, around it are capped too, so a network that
// holds many addresses does not take every connection. The host's own
// prefixes are the exception: every machine on the host has an address of
// its /64, so there each address is a client of its own, and the network
// caps do not apply; nor do they to the machines' IPv4 (hostTransit).
// The defaults, and why, are in docs/decisions/front-door.md.
type Limits struct {
	// OwnPrefixes are the host's own: its machines' addresses, which reach
	// the front door from inside the host. An address in one is a client
	// by itself.
	OwnPrefixes []netip.Prefix

	// Conns caps the open connections, from everyone. Past it a new one
	// takes another's place by fair eviction (fairness.go).
	Conns int
	// ConnsPerClient caps one client's open connections; one more is
	// closed as soon as it is accepted.
	ConnsPerClient int
	// ConnsPer56 and ConnsPer48 cap the open connections from every IPv6
	// client in one /56 and in one /48, as ConnsPerClient does one.
	ConnsPer56, ConnsPer48 int
	// ConnsPer24 caps the open connections from every IPv4 address in one
	// /24.
	ConnsPer24 int
	// ConnRate and ConnBurst cap how fast one client opens connections,
	// each a TLS handshake to pay for.
	ConnRate  float64
	ConnBurst int
	// RequestRate and RequestBurst cap how fast one client sends requests,
	// each HTTP/2 stream one; past them it is answered 429.
	RequestRate  float64
	RequestBurst int
	// RequestsPerClient caps one client's requests in flight, streams and
	// upgraded connections included.
	RequestsPerClient int

	// MaxHeaderBytes caps a request's header.
	MaxHeaderBytes int
	// APIBodyBytes and EndpointBodyBytes cap a request's body to the API
	// and to an endpoint.
	APIBodyBytes, EndpointBodyBytes int64
	// MinBodyRate is the bytes a second a request's body must average over
	// the time spent waiting for it; BodyReadTimeout is the longest one
	// wait may be (bodyrate.go).
	MinBodyRate     int64
	BodyReadTimeout time.Duration

	// ReadHeaderTimeout is how long a client has for its TLS handshake and
	// then each request's header.
	ReadHeaderTimeout time.Duration
	// IdleTimeout closes a connection that carries no request.
	IdleTimeout time.Duration
	// ConsoleWriteTimeout is how long one of the console's files may take
	// to send.
	ConsoleWriteTimeout time.Duration
	// StalledWriteTimeout is how long one write of a response may wait on
	// its client (writes.go).
	StalledWriteTimeout time.Duration

	// HTTP2 settings: streams at once on a connection, and how a
	// connection that stops answering is dropped.
	HTTP2MaxStreams int
	// HTTP2ConnWindow and HTTP2StreamWindow are how much of the bodies a
	// client may send ahead of their handlers, on a connection and on one
	// stream: what the door holds for them. HTTP2MaxFrame is the largest
	// frame it reads, and so holds a buffer for.
	HTTP2ConnWindow, HTTP2StreamWindow, HTTP2MaxFrame int
	HTTP2SendPingTimeout                              time.Duration
	HTTP2PingTimeout                                  time.Duration
	HTTP2WriteByteTimeout                             time.Duration
}

// DefaultLimits are the limits unless the flags say otherwise.
var DefaultLimits = Limits{
	Conns:             1536,
	ConnsPerClient:    64,
	ConnsPer56:        96,
	ConnsPer48:        384,
	ConnsPer24:        384,
	ConnRate:          10,
	ConnBurst:         50,
	RequestRate:       20,
	RequestBurst:      100,
	RequestsPerClient: 128,

	MaxHeaderBytes:    64 << 10,
	APIBodyBytes:      8 << 20,
	EndpointBodyBytes: 100 << 20,
	MinBodyRate:       1 << 10,
	BodyReadTimeout:   30 * time.Second,

	ReadHeaderTimeout:   10 * time.Second,
	IdleTimeout:         2 * time.Minute,
	ConsoleWriteTimeout: time.Minute,
	StalledWriteTimeout: 30 * time.Second,

	HTTP2MaxStreams:       32,
	HTTP2ConnWindow:       256 << 10,
	HTTP2StreamWindow:     256 << 10,
	HTTP2MaxFrame:         16 << 10,
	HTTP2SendPingTimeout:  30 * time.Second,
	HTTP2PingTimeout:      15 * time.Second,
	HTTP2WriteByteTimeout: 30 * time.Second,
}

// Validate says what is wrong with l, nothing when every limit is set.
func (l Limits) Validate() error {
	for _, n := range []struct {
		name string
		ok   bool
	}{
		{"connections", l.Conns > 0},
		{"connections per client", l.ConnsPerClient > 0},
		{"connections per /56 and /48", l.ConnsPer56 > 0 && l.ConnsPer48 > 0},
		{"connections per IPv4 /24", l.ConnsPer24 > 0},
		{"connection rate", l.ConnRate > 0 && l.ConnBurst > 0},
		{"request rate", l.RequestRate > 0 && l.RequestBurst > 0},
		{"requests per client", l.RequestsPerClient > 0},
		{"header bytes", l.MaxHeaderBytes > 0},
		{"body bytes", l.APIBodyBytes > 0 && l.EndpointBodyBytes > 0},
		{"body rate", l.MinBodyRate > 0 && l.BodyReadTimeout > 0},
		{"timeouts", l.ReadHeaderTimeout > 0 && l.IdleTimeout > 0 && l.ConsoleWriteTimeout > 0 && l.StalledWriteTimeout > 0},
		{"HTTP/2 settings", l.HTTP2MaxStreams > 0 && l.HTTP2SendPingTimeout > 0 && l.HTTP2PingTimeout > 0 && l.HTTP2WriteByteTimeout > 0},
		// net/http takes a connection's window of 64 KiB to 4 MiB, a
		// stream's below 4 MiB, and frames of 16 KiB to 16 MiB.
		{"HTTP/2 windows and frame", l.HTTP2ConnWindow >= 64<<10 && l.HTTP2ConnWindow < 4<<20 &&
			l.HTTP2StreamWindow > 0 && l.HTTP2StreamWindow <= l.HTTP2ConnWindow && l.HTTP2MaxFrame >= 16<<10 && l.HTTP2MaxFrame <= 16<<20},
	} {
		if !n.ok {
			return errors.New("every limit must be positive: " + n.name)
		}
	}
	for _, p := range l.OwnPrefixes {
		// Wider than a /48, every client of the internet could be counted
		// by its address alone, so no /64 would be held to one limit.
		if !p.IsValid() || !p.Addr().Is6() || p.Addr().Is4In6() || p.Bits() < 48 || p != p.Masked() {
			return fmt.Errorf("an own prefix is an IPv6 network of /48 or narrower, as the host's own /64 is, not %s", p)
		}
	}
	return nil
}

// client is who a limit is kept for: an IPv4 address, or an IPv6 /64, or
// in one of the host's own prefixes, the address alone.
func (l Limits) client(addr netip.Addr) netip.Prefix {
	addr = addr.Unmap()
	bits := 32
	if addr.Is6() {
		bits = 64
		if l.own(addr) {
			bits = 128
		}
	}
	p, _ := addr.Prefix(bits)
	return p
}

func (l Limits) own(addr netip.Addr) bool {
	for _, p := range l.OwnPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// addrOf is the address in a connection's or a request's remote address.
func addrOf(remote string) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap(), true
}

// hostTransit is where the host's machines' IPv4 reaches the front door
// from, each network's transit /31 (docs/decisions/networking.md); shared
// address space (RFC 6598), never a source from the internet.
var hostTransit = netip.MustParsePrefix("100.64.0.0/10")

// networks are the networks around addr capped together, with their caps:
// the IPv4 /24, or the IPv6 /56 and /48; none for the host's own prefixes
// and its machines' IPv4.
func (l Limits) networks(addr netip.Addr) []network {
	addr = addr.Unmap()
	if addr.Is4() {
		if hostTransit.Contains(addr) {
			return nil
		}
		p24, _ := addr.Prefix(24)
		return []network{{p24, l.ConnsPer24, "too many connections from the client's /24"}}
	}
	if l.own(addr) {
		return nil
	}
	p56, _ := addr.Prefix(56)
	p48, _ := addr.Prefix(48)
	return []network{
		{p56, l.ConnsPer56, "too many connections from the client's /56"},
		{p48, l.ConnsPer48, "too many connections from the client's /48"},
	}
}

type network struct {
	prefix netip.Prefix
	cap    int
	why    string
}

// counts keeps what each client holds and may still do.
type counts struct {
	l Limits

	mu    sync.Mutex
	conns int
	// open are the connections admitted and not yet released.
	open map[*conn]struct{}
	// buckets, inBucket and byClient count the open connections for fair
	// eviction: by bucket, by client within each bucket, by client.
	buckets  *tally[netip.Prefix]
	inBucket map[netip.Prefix]*tally[netip.Prefix]
	byClient *tally[netip.Prefix]
	// idle are the connections that served a request and carry none now,
	// idle longest first.
	idle *list.List
	// networkConns are the open connections of each IPv4 /24, and each
	// IPv6 /56 and /48, that has any.
	networkConns map[netip.Prefix]int
	clients      map[netip.Prefix]*clientCounts
	lastSweep    time.Time
	now          func() time.Time
}

type clientCounts struct {
	conns, requests int
	connRate        *rate.Limiter
	requestRate     *rate.Limiter
	open            map[*conn]struct{}
}

// conn is an admitted connection as the counts keep it. Its fields after
// c are k.mu's.
type conn struct {
	// c is what an eviction closes; nil where only the counts are tested.
	c              net.Conn
	cc             *clientCounts
	client, bucket netip.Prefix
	nets           []network
	opened         time.Time
	// busy says a request is in flight on it, or it was hijacked; idleSince
	// is when it last carried none, and inIdle its place in counts.idle once
	// a request has ended on it.
	busy      bool
	idleSince time.Time
	inIdle    *list.Element
	released  bool
}

func newCounts(l Limits) *counts {
	return &counts{
		l: l, open: map[*conn]struct{}{},
		buckets: newTally[netip.Prefix](), inBucket: map[netip.Prefix]*tally[netip.Prefix]{}, byClient: newTally[netip.Prefix](),
		idle:         list.New(),
		networkConns: map[netip.Prefix]int{}, clients: map[netip.Prefix]*clientCounts{}, now: time.Now,
	}
}

// sweepEvery is how often the counts forget clients that hold nothing and
// whose rates have filled up again, which they would start from anyway.
const sweepEvery = time.Minute

// get returns c's counts. Callers hold k.mu.
func (k *counts) get(c netip.Prefix) *clientCounts {
	now := k.now()
	if now.Sub(k.lastSweep) > sweepEvery {
		k.lastSweep = now
		for p, cc := range k.clients {
			if cc.conns == 0 && cc.requests == 0 && cc.connRate.TokensAt(now) >= float64(k.l.ConnBurst) && cc.requestRate.TokensAt(now) >= float64(k.l.RequestBurst) {
				delete(k.clients, p)
			}
		}
	}
	cc, ok := k.clients[c]
	if !ok {
		cc = &clientCounts{
			connRate:    rate.NewLimiter(rate.Limit(k.l.ConnRate), k.l.ConnBurst),
			requestRate: rate.NewLimiter(rate.Limit(k.l.RequestRate), k.l.RequestBurst),
			open:        map[*conn]struct{}{},
		}
		k.clients[c] = cc
	}
	return cc
}

// openConn admits c, a new connection from addr, or says why not. With
// Conns open it closes another to make room (victim), or refuses c.
func (k *counts) openConn(addr netip.Addr, c net.Conn) (_ *conn, why string) {
	k.mu.Lock()
	client, bucket := k.l.client(addr), k.l.bucket(addr)
	nets := k.l.networks(addr)
	for _, n := range nets {
		if k.networkConns[n.prefix] >= n.cap {
			k.mu.Unlock()
			return nil, n.why
		}
	}
	cc := k.get(client)
	if cc.conns >= k.l.ConnsPerClient {
		k.mu.Unlock()
		return nil, "too many connections from the client"
	}
	var evicted *conn
	if k.conns >= k.l.Conns {
		if evicted = k.victim(bucket, client); evicted == nil {
			k.mu.Unlock()
			return nil, "too many connections"
		}
	}
	if !cc.connRate.AllowN(k.now(), 1) {
		k.mu.Unlock()
		return nil, "the client opens connections too fast"
	}
	if evicted != nil {
		k.releaseLocked(evicted)
	}
	now := k.now()
	x := &conn{c: c, cc: cc, client: client, bucket: bucket, nets: nets, opened: now, idleSince: now}
	k.open[x] = struct{}{}
	cc.open[x] = struct{}{}
	cc.conns++
	k.conns++
	for _, n := range nets {
		k.networkConns[n.prefix]++
	}
	k.buckets.add(bucket, 1)
	if k.inBucket[bucket] == nil {
		k.inBucket[bucket] = newTally[netip.Prefix]()
	}
	k.inBucket[bucket].add(client, 1)
	k.byClient.add(client, 1)
	k.mu.Unlock()
	// Its server sees the connection fail, and closes it.
	if evicted != nil && evicted.c != nil {
		_ = evicted.c.Close()
	}
	return x, ""
}

// release gives x's place back; once, however often it is called.
func (k *counts) release(x *conn) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.releaseLocked(x)
}

func (k *counts) releaseLocked(x *conn) {
	if x.released {
		return
	}
	x.released = true
	delete(k.open, x)
	delete(x.cc.open, x)
	if x.inIdle != nil {
		k.idle.Remove(x.inIdle)
		x.inIdle = nil
	}
	x.cc.conns--
	k.conns--
	for _, n := range x.nets {
		if k.networkConns[n.prefix]--; k.networkConns[n.prefix] == 0 {
			delete(k.networkConns, n.prefix)
		}
	}
	k.buckets.add(x.bucket, -1)
	if t := k.inBucket[x.bucket]; t != nil {
		if t.add(x.client, -1); t.empty() {
			delete(k.inBucket, x.bucket)
		}
	}
	k.byClient.add(x.client, -1)
}

// setBusy records whether x carries a request.
func (k *counts) setBusy(x *conn, busy bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if x.released || x.busy == busy {
		return
	}
	x.busy = busy
	if busy {
		if x.inIdle != nil {
			k.idle.Remove(x.inIdle)
			x.inIdle = nil
		}
		return
	}
	x.idleSince = k.now()
	x.inIdle = k.idle.PushBack(x)
}

// connState is the servers' ConnState: it follows which connections carry
// a request. A hijacked one stays busy: it is a WebSocket's.
func (k *counts) connState(c net.Conn, s http.ConnState) {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	cc, ok := c.(*countedConn)
	if !ok {
		return
	}
	switch s {
	case http.StateActive, http.StateHijacked:
		k.setBusy(cc.x, true)
	case http.StateIdle:
		k.setBusy(cc.x, false)
	}
}

// startRequest admits a request from c, or says how long until one would
// be.
func (k *counts) startRequest(c netip.Prefix) (release func(), retry time.Duration, ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	cc := k.get(c)
	if cc.requests >= k.l.RequestsPerClient {
		return nil, time.Second, false
	}
	now := k.now()
	if r := cc.requestRate.ReserveN(now, 1); !r.OK() || r.DelayFrom(now) > 0 {
		if r.OK() {
			retry = r.DelayFrom(now)
			r.CancelAt(now)
		}
		return nil, max(retry, time.Second), false
	}
	cc.requests++
	var once sync.Once
	return func() {
		once.Do(func() {
			k.mu.Lock()
			defer k.mu.Unlock()
			cc.requests--
		})
	}, 0, true
}

// limitRequests answers 429 to a request past its client's rate or
// in-flight limit, and passes the rest to next.
func (k *counts) limitRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr, ok := addrOf(r.RemoteAddr)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		release, retry, ok := k.startRequest(k.l.client(addr))
		if !ok {
			tooMany(w, retry)
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}

// listener is a TCP listener whose connections count against their
// clients' limits: one past them is closed as soon as it is accepted.
type listener struct {
	net.Listener
	counts *counts
	// refused is told each connection refused, and why.
	refused func(addr netip.Addr, why string)
}

func (l *listener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		addr, ok := addrOf(c.RemoteAddr().String())
		if !ok {
			_ = c.Close()
			continue
		}
		x, why := l.counts.openConn(addr, c)
		if x == nil {
			_ = c.Close()
			if l.refused != nil {
				l.refused(addr, why)
			}
			continue
		}
		tune(c)
		return &countedConn{Conn: c, k: l.counts, x: x}, nil
	}
}

// refusals counts the connections refused, by reason, for a summary that
// a ticker takes every refusalSummaryEvery: one line for a flood, not one a
// connection.
type refusals struct {
	mu sync.Mutex
	by map[string]int
}

// refusalSummaryEvery is how often the refusals are summed up.
const refusalSummaryEvery = time.Minute

func newRefusals() *refusals {
	return &refusals{by: map[string]int{}}
}

// note counts a refusal for why.
func (r *refusals) note(why string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.by[why]++
}

// take returns the counts since it was last called and starts again; nil
// when there were none.
func (r *refusals) take() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.by) == 0 {
		return nil
	}
	by := r.by
	r.by = map[string]int{}
	return by
}

// tune gives a connection the keepalive and user timeout the apiserver's
// listener has: a dead client under a stream whose data is backed up is
// dropped within about a minute (internal/execws).
func tune(c net.Conn) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tc.SetKeepAliveConfig(execws.KeepAlive)
	if rc, err := tc.SyscallConn(); err == nil {
		_ = execws.Control("tcp", "", rc)
	}
}

// countedConn gives its client's count back when it closes.
type countedConn struct {
	net.Conn
	k *counts
	x *conn
}

func (c *countedConn) Close() error {
	c.k.release(c.x)
	return c.Conn.Close()
}
