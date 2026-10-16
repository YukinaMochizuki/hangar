package front

import (
	"errors"
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
// an IPv6 /64, which is what one subscriber usually holds; the /56 and the
// /48 around it are capped too, so a network that hands out many /64s does
// not take every connection. The defaults, and why, are in
// docs/decisions/front-door.md.
type Limits struct {
	// Conns caps the open connections, from everyone.
	Conns int
	// ConnsPerClient caps one client's open connections; one more is
	// closed as soon as it is accepted.
	ConnsPerClient int
	// ConnsPer56 and ConnsPer48 cap the open connections from every IPv6
	// client in one /56 and in one /48, as ConnsPerClient does one.
	ConnsPer56, ConnsPer48 int
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

	// ReadHeaderTimeout is how long a client has for its TLS handshake and
	// then each request's header.
	ReadHeaderTimeout time.Duration
	// IdleTimeout closes a connection that carries no request.
	IdleTimeout time.Duration
	// ConsoleWriteTimeout is how long one of the console's files may take
	// to send.
	ConsoleWriteTimeout time.Duration

	// HTTP2 settings: streams at once on a connection, and how a
	// connection that stops answering is dropped.
	HTTP2MaxStreams       int
	HTTP2SendPingTimeout  time.Duration
	HTTP2PingTimeout      time.Duration
	HTTP2WriteByteTimeout time.Duration
}

// DefaultLimits are the limits unless the flags say otherwise.
var DefaultLimits = Limits{
	Conns:             4096,
	ConnsPerClient:    64,
	ConnsPer56:        256,
	ConnsPer48:        1024,
	ConnRate:          10,
	ConnBurst:         50,
	RequestRate:       20,
	RequestBurst:      100,
	RequestsPerClient: 128,

	MaxHeaderBytes:    64 << 10,
	APIBodyBytes:      8 << 20,
	EndpointBodyBytes: 100 << 20,

	ReadHeaderTimeout:   10 * time.Second,
	IdleTimeout:         2 * time.Minute,
	ConsoleWriteTimeout: time.Minute,

	HTTP2MaxStreams:       100,
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
		{"connection rate", l.ConnRate > 0 && l.ConnBurst > 0},
		{"request rate", l.RequestRate > 0 && l.RequestBurst > 0},
		{"requests per client", l.RequestsPerClient > 0},
		{"header bytes", l.MaxHeaderBytes > 0},
		{"body bytes", l.APIBodyBytes > 0 && l.EndpointBodyBytes > 0},
		{"timeouts", l.ReadHeaderTimeout > 0 && l.IdleTimeout > 0 && l.ConsoleWriteTimeout > 0},
		{"HTTP/2 settings", l.HTTP2MaxStreams > 0 && l.HTTP2SendPingTimeout > 0 && l.HTTP2PingTimeout > 0 && l.HTTP2WriteByteTimeout > 0},
	} {
		if !n.ok {
			return errors.New("every limit must be positive: " + n.name)
		}
	}
	return nil
}

// client is who a limit is kept for: an IPv4 address, or an IPv6 /64.
func client(addr netip.Addr) netip.Prefix {
	addr = addr.Unmap()
	bits := 32
	if addr.Is6() {
		bits = 64
	}
	p, _ := addr.Prefix(bits)
	return p
}

// addrOf is the address in a connection's or a request's remote address.
func addrOf(remote string) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap(), true
}

// networks are the IPv6 networks around addr whose connections are capped
// together, with their caps; none for IPv4.
func (l Limits) networks(addr netip.Addr) []network {
	addr = addr.Unmap()
	if !addr.Is6() {
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
	// networkConns are the open connections of each IPv6 /56 and /48 that
	// has any.
	networkConns map[netip.Prefix]int
	clients      map[netip.Prefix]*clientCounts
	lastSweep    time.Time
	now          func() time.Time
}

type clientCounts struct {
	conns, requests int
	connRate        *rate.Limiter
	requestRate     *rate.Limiter
}

func newCounts(l Limits) *counts {
	return &counts{l: l, networkConns: map[netip.Prefix]int{}, clients: map[netip.Prefix]*clientCounts{}, now: time.Now}
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
		}
		k.clients[c] = cc
	}
	return cc
}

// openConn admits a new connection from addr, or says why not.
func (k *counts) openConn(addr netip.Addr) (release func(), why string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.conns >= k.l.Conns {
		return nil, "too many connections"
	}
	nets := k.l.networks(addr)
	for _, n := range nets {
		if k.networkConns[n.prefix] >= n.cap {
			return nil, n.why
		}
	}
	cc := k.get(client(addr))
	if cc.conns >= k.l.ConnsPerClient {
		return nil, "too many connections from the client"
	}
	if !cc.connRate.AllowN(k.now(), 1) {
		return nil, "the client opens connections too fast"
	}
	cc.conns++
	k.conns++
	for _, n := range nets {
		k.networkConns[n.prefix]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			k.mu.Lock()
			defer k.mu.Unlock()
			cc.conns--
			k.conns--
			for _, n := range nets {
				if k.networkConns[n.prefix]--; k.networkConns[n.prefix] == 0 {
					delete(k.networkConns, n.prefix)
				}
			}
		})
	}, ""
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
		release, retry, ok := k.startRequest(client(addr))
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
		release, why := l.counts.openConn(addr)
		if release == nil {
			_ = c.Close()
			if l.refused != nil {
				l.refused(addr, why)
			}
			continue
		}
		tune(c)
		return &countedConn{Conn: c, release: release}, nil
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
	release func()
}

func (c *countedConn) Close() error {
	c.release()
	return c.Conn.Close()
}
