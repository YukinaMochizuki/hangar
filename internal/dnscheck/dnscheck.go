// Package dnscheck looks up what a name's own nameservers say it is a CNAME
// of, rather than what a resolver remembers: a recursive resolver keeps a
// "no such record" for as long as the zone's negative TTL, often half an
// hour, so a record a tenant has just added would otherwise stay unseen.
// The apiserver checks a CustomDomain's challenge CNAME with it
// (docs/decisions/custom-domains.md).
package dnscheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// maxReferrals bounds how many delegations below the zone found are
// followed, as when _acme-challenge.<domain> is a zone of its own.
const maxReferrals = 3

// Resolver looks names up at their authoritative nameservers.
type Resolver struct {
	// Recursive are the resolvers, as host:port, asked to find a zone's
	// nameservers and their addresses; empty is the system's.
	Recursive []string
	// Nameserver, if set, is the address, as host:port, a nameserver of
	// the given name and address is asked at; nil asks it on port 53.
	Nameserver func(host string, addr netip.Addr) string
	// Timeout bounds each query; zero is five seconds.
	Timeout time.Duration
}

// Answer is what a lookup found.
type Answer struct {
	// Target is the CNAME's target, in lower case without the trailing dot;
	// "" when the name is no CNAME.
	Target string
	// From says who answered: the nameserver, as host, or "the resolver
	// at …" when no authoritative nameserver could be asked.
	From string
}

// CNAME looks up what name is a CNAME of, asking the nameservers of the
// zone it is in, found through the recursive resolvers. When none of them
// answers, it asks the recursive resolvers themselves, and says so in
// From.
func (r *Resolver) CNAME(ctx context.Context, name string) (Answer, error) {
	name = dns.Fqdn(strings.ToLower(name))
	recursive, err := r.recursive()
	if err != nil {
		return Answer{}, err
	}
	ans, authErr := r.authoritative(ctx, recursive, name)
	if authErr == nil {
		return ans, nil
	}
	ans, err = r.ask(ctx, recursive, name, true)
	if err != nil {
		return Answer{}, fmt.Errorf("its nameservers: %w; the resolvers: %w", authErr, err)
	}
	return ans, nil
}

func (r *Resolver) recursive() ([]string, error) {
	if len(r.Recursive) > 0 {
		return r.Recursive, nil
	}
	cc, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(cc.Servers))
	for _, s := range cc.Servers {
		out = append(out, net.JoinHostPort(s, cc.Port))
	}
	return out, nil
}

func (r *Resolver) client() *dns.Client {
	t := r.Timeout
	if t == 0 {
		t = 5 * time.Second
	}
	return &dns.Client{Timeout: t}
}

// authoritative asks the nameservers of name's zone, following referrals
// below it.
func (r *Resolver) authoritative(ctx context.Context, recursive []string, name string) (Answer, error) {
	hosts, err := r.zoneNameservers(ctx, recursive, name)
	if err != nil {
		return Answer{}, err
	}
	var glue map[string][]netip.Addr
	for range maxReferrals + 1 {
		servers := r.addresses(ctx, recursive, hosts, glue)
		if len(servers) == 0 {
			return Answer{}, fmt.Errorf("no address for the nameservers %s", strings.Join(hosts, ", "))
		}
		var lastErr error
		referred := false
		for _, s := range servers {
			in, err := r.exchange(ctx, name, dns.TypeCNAME, s.addr, false)
			if err != nil {
				lastErr = err
				continue
			}
			if in.Authoritative && (in.Rcode == dns.RcodeSuccess || in.Rcode == dns.RcodeNameError) {
				return Answer{Target: cnameOf(in, name), From: s.host}, nil
			}
			if next, nextGlue := referral(in); in.Rcode == dns.RcodeSuccess && len(next) > 0 {
				hosts, glue, referred = next, nextGlue, true
				break
			}
			lastErr = fmt.Errorf("%s answered %s without authority", s.host, dns.RcodeToString[in.Rcode])
		}
		if !referred {
			if lastErr == nil {
				lastErr = errors.New("no nameserver answered")
			}
			return Answer{}, lastErr
		}
	}
	return Answer{}, fmt.Errorf("more than %d referrals", maxReferrals)
}

// zoneNameservers finds the nameservers of the zone name is in: the NS of
// name or, failing that, of each name above it.
func (r *Resolver) zoneNameservers(ctx context.Context, recursive []string, name string) ([]string, error) {
	for z := name; strings.Count(z, ".") > 1; z = z[strings.Index(z, ".")+1:] {
		var in *dns.Msg
		var lastErr error
		for _, s := range recursive {
			m, err := r.exchange(ctx, z, dns.TypeNS, s, true)
			if err != nil {
				lastErr = err
				continue
			}
			in = m
			break
		}
		if in == nil {
			return nil, fmt.Errorf("finding the nameservers of %s: %w", z, lastErr)
		}
		var hosts []string
		for _, rr := range in.Answer {
			if ns, ok := rr.(*dns.NS); ok && strings.EqualFold(ns.Hdr.Name, z) {
				hosts = append(hosts, strings.ToLower(ns.Ns))
			}
		}
		if len(hosts) > 0 {
			return hosts, nil
		}
	}
	return nil, fmt.Errorf("no zone above %s has nameservers", name)
}

type server struct {
	host string
	addr string
}

// addresses are where hosts are asked: their glue if given, else their A
// and AAAA records through the recursive resolvers.
func (r *Resolver) addresses(ctx context.Context, recursive []string, hosts []string, glue map[string][]netip.Addr) []server {
	var out []server
	for _, h := range hosts {
		addrs := glue[h]
		if len(addrs) == 0 {
			for _, t := range []uint16{dns.TypeA, dns.TypeAAAA} {
				for _, s := range recursive {
					in, err := r.exchange(ctx, h, t, s, true)
					if err != nil {
						continue
					}
					addrs = append(addrs, addrsOf(in)...)
					break
				}
			}
		}
		for _, a := range addrs {
			at := net.JoinHostPort(a.String(), "53")
			if r.Nameserver != nil {
				at = r.Nameserver(h, a)
			}
			out = append(out, server{host: strings.TrimSuffix(h, "."), addr: at})
		}
	}
	return out
}

func (r *Resolver) exchange(ctx context.Context, name string, qtype uint16, at string, recurse bool) (*dns.Msg, error) {
	q := new(dns.Msg)
	q.SetQuestion(name, qtype)
	q.RecursionDesired = recurse
	in, _, err := r.client().ExchangeContext(ctx, q, at)
	if err != nil {
		return nil, err
	}
	if in.Truncated {
		tcp := r.client()
		tcp.Net = "tcp"
		if in, _, err = tcp.ExchangeContext(ctx, q, at); err != nil {
			return nil, err
		}
	}
	return in, nil
}

// ask asks the recursive resolvers what name is a CNAME of.
func (r *Resolver) ask(ctx context.Context, recursive []string, name string, recurse bool) (Answer, error) {
	var lastErr error
	for _, s := range recursive {
		in, err := r.exchange(ctx, name, dns.TypeCNAME, s, recurse)
		if err != nil {
			lastErr = err
			continue
		}
		if in.Rcode != dns.RcodeSuccess && in.Rcode != dns.RcodeNameError {
			lastErr = fmt.Errorf("%s answered %s", s, dns.RcodeToString[in.Rcode])
			continue
		}
		return Answer{Target: cnameOf(in, name), From: "the resolver at " + s}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no resolver to ask")
	}
	return Answer{}, lastErr
}

func cnameOf(in *dns.Msg, name string) string {
	for _, rr := range in.Answer {
		if c, ok := rr.(*dns.CNAME); ok && strings.EqualFold(c.Hdr.Name, name) {
			return strings.ToLower(strings.TrimSuffix(c.Target, "."))
		}
	}
	return ""
}

func addrsOf(in *dns.Msg) []netip.Addr {
	var out []netip.Addr
	for _, rr := range in.Answer {
		switch v := rr.(type) {
		case *dns.A:
			if a, ok := netip.AddrFromSlice(v.A.To4()); ok {
				out = append(out, a)
			}
		case *dns.AAAA:
			if a, ok := netip.AddrFromSlice(v.AAAA); ok {
				out = append(out, a)
			}
		}
	}
	return out
}

// referral is the nameservers a non-authoritative answer delegates to, and
// the addresses it gives for them.
func referral(in *dns.Msg) ([]string, map[string][]netip.Addr) {
	var hosts []string
	for _, rr := range in.Ns {
		if ns, ok := rr.(*dns.NS); ok {
			hosts = append(hosts, strings.ToLower(ns.Ns))
		}
	}
	glue := map[string][]netip.Addr{}
	for _, rr := range in.Extra {
		name := strings.ToLower(rr.Header().Name)
		glue[name] = append(glue[name], addrsOf(&dns.Msg{Answer: []dns.RR{rr}})...)
	}
	return hosts, glue
}
