package tailnet

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// ResolverAddr is where a guest on a tailnet resolves names: its
// /etc/resolv.conf's one nameserver, on loopback, at the address Docker's
// embedded resolver takes, which images leave free for it.
const ResolverAddr = "127.0.0.11"

// MagicDNS is tailscaled's resolver, which answers the tailnet's names.
const MagicDNS = "100.100.100.100:53"

// exchangeTimeout bounds one upstream's answer before the next is asked.
const exchangeTimeout = 2 * time.Second

// Resolver splits the guest's DNS: a name under the tailnet's MagicDNS
// suffix goes to tailscaled's resolver, every other name to the resolvers
// the guest had without the tailnet, as it would have without it.
type Resolver struct {
	// Upstreams are the guest's own resolvers, host:port, asked in order.
	Upstreams []string
	// MagicDNS is the tailnet's resolver, host:port.
	MagicDNS string

	mu      sync.Mutex
	suffix  string
	servers []*dns.Server
	// exchange, if set, replaces asking a server over the network, for
	// tests.
	exchange func(ctx context.Context, m *dns.Msg, network, addr string) (*dns.Msg, error)
}

// SetSuffix sets the tailnet's MagicDNS suffix, such as tail1234.ts.net;
// empty sends every name upstream.
func (r *Resolver) SetSuffix(suffix string) {
	s := strings.ToLower(strings.Trim(suffix, "."))
	if s != "" {
		s = dns.Fqdn(s)
	}
	r.mu.Lock()
	r.suffix = s
	r.mu.Unlock()
}

// Route is the servers a query for name is asked, in order.
func (r *Resolver) Route(name string) []string {
	n := dns.Fqdn(strings.ToLower(name))
	r.mu.Lock()
	suffix := r.suffix
	r.mu.Unlock()
	if suffix != "" && (n == suffix || strings.HasSuffix(n, "."+suffix)) {
		return []string{r.MagicDNS}
	}
	return r.Upstreams
}

// ServeDNS answers a query with what the first server of its route that
// answers says, SERVFAIL if none does.
func (r *Resolver) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	network := "udp"
	if _, ok := w.RemoteAddr().(*net.TCPAddr); ok {
		network = "tcp"
	}
	_ = w.WriteMsg(r.answer(context.Background(), req, network))
}

func (r *Resolver) answer(ctx context.Context, req *dns.Msg, network string) *dns.Msg {
	if len(req.Question) != 1 {
		return new(dns.Msg).SetRcode(req, dns.RcodeFormatError)
	}
	for _, server := range r.Route(req.Question[0].Name) {
		res, err := r.ask(ctx, req, network, server)
		if err == nil && res != nil {
			res.Id = req.Id
			return res
		}
	}
	return new(dns.Msg).SetRcode(req, dns.RcodeServerFailure)
}

func (r *Resolver) ask(ctx context.Context, req *dns.Msg, network, server string) (*dns.Msg, error) {
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	if r.exchange != nil {
		return r.exchange(ctx, req, network, server)
	}
	c := &dns.Client{Net: network, Timeout: exchangeTimeout}
	res, _, err := c.ExchangeContext(ctx, req, server)
	return res, err
}

// Serve answers on hostport, such as 127.0.0.11:53, over UDP and TCP,
// until Close or the listeners fail; it returns once both listen, with the
// address they listen on, which a port of 0 picks.
func (r *Resolver) Serve(hostport string) (net.Addr, error) {
	pc, err := net.ListenPacket("udp", hostport)
	if err != nil {
		return nil, err
	}
	// TCP on the port UDP took, so a client that falls back to TCP finds it.
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	servers := []*dns.Server{{PacketConn: pc, Handler: r}, {Listener: ln, Handler: r}}
	for _, s := range servers {
		started := make(chan struct{})
		s.NotifyStartedFunc = func() { close(started) }
		go func() { _ = s.ActivateAndServe() }()
		// Shutdown refuses a server not started yet.
		<-started
	}
	r.mu.Lock()
	r.servers = append(r.servers, servers...)
	r.mu.Unlock()
	return pc.LocalAddr(), nil
}

// Close stops answering, and frees the address Serve listened on.
func (r *Resolver) Close() error {
	r.mu.Lock()
	servers := r.servers
	r.servers = nil
	r.mu.Unlock()
	var errs []error
	for _, s := range servers {
		errs = append(errs, s.Shutdown())
	}
	return errors.Join(errs...)
}

// Upstreams are the nameservers, as resolv.conf lists them, as host:port.
func Upstreams(nameservers []string) ([]string, error) {
	var out []string
	for _, ns := range nameservers {
		ip := net.ParseIP(ns)
		if ip == nil {
			return nil, errors.New("nameserver " + ns + " is not an IP address")
		}
		out = append(out, net.JoinHostPort(ip.String(), "53"))
	}
	return out, nil
}
