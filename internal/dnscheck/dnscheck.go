// Package dnscheck looks up what a name's own nameservers say it is a CNAME
// of, rather than what a resolver remembers: a recursive resolver keeps a
// "no such record" for as long as the zone's negative TTL, often half an
// hour, so a record a tenant has just added would otherwise stay unseen.
// The apiserver checks a CustomDomain's challenge CNAME with it
// (docs/decisions/custom-domains.md).
//
// The zone, its nameservers and their addresses are the tenant's to set,
// so a lookup is bounded in time and in how many servers it asks, asks
// public addresses alone, and says nothing of the network it ran on in
// its errors.
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

// The bounds of one lookup.
const (
	// maxReferrals is how many delegations below the zone found are
	// followed, as when _acme-challenge.<domain> is a zone of its own.
	maxReferrals = 3
	// maxNameservers is how many of a zone's nameservers are asked, and
	// maxAddresses how many addresses of theirs, at each level.
	maxNameservers = 4
	maxAddresses   = 6
	// defaultTimeout bounds each query, and defaultTotal the whole lookup.
	defaultTimeout = 3 * time.Second
	defaultTotal   = 15 * time.Second
)

// Resolver looks names up at their authoritative nameservers.
type Resolver struct {
	// Recursive are the resolvers, as host:port, asked to find a zone's
	// nameservers and their addresses; empty is the system's.
	Recursive []string
	// Nameserver, if set, is the address, as host:port, a nameserver of
	// the given name and address is asked at; nil asks it on port 53. For
	// tests, which serve nameservers on ports of their own.
	Nameserver func(host string, addr netip.Addr) string
	// Timeout bounds each query, and Total the whole lookup; zero is three
	// seconds and fifteen.
	Timeout, Total time.Duration
}

// Answer is what a lookup found.
type Answer struct {
	// Target is the CNAME's target, in lower case without the trailing dot;
	// "" when the name is no CNAME.
	Target string
	// From says who answered: the nameserver, by name, or "a resolver"
	// when none of the zone's nameservers could be asked.
	From string
}

// CNAME looks up what name is a CNAME of, asking the nameservers of the
// zone it is in, found through the recursive resolvers. When none of them
// answers, it asks the recursive resolvers themselves, and says so in
// From. Its error says what failed in words a tenant may read.
func (r *Resolver) CNAME(ctx context.Context, name string) (Answer, error) {
	total := r.Total
	if total == 0 {
		total = defaultTotal
	}
	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()
	name = dns.Fqdn(strings.ToLower(name))
	recursive, err := r.recursive()
	if err != nil {
		return Answer{}, errors.New("no resolver is configured")
	}
	ans, authErr := r.authoritative(ctx, recursive, name)
	if authErr == nil {
		return ans, nil
	}
	ans, err = r.ask(ctx, recursive, name)
	if err != nil {
		return Answer{}, fmt.Errorf("%w, and no resolver answered either", authErr)
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

// Public reports whether a nameserver at addr may be asked: an address on
// the internet. Not loopback, private, link-local, shared (100.64.0.0/10),
// unique-local, multicast or unspecified: those reach the apiserver's own
// host and networks, which a tenant naming them would probe.
func Public(addr netip.Addr) bool {
	addr = addr.Unmap()
	return addr.IsGlobalUnicast() && !addr.IsPrivate() && !sharedAddressSpace.Contains(addr)
}

var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// authoritative asks the nameservers of name's zone over TCP, which no one
// off the path can spoof, following referrals below the zone towards name.
func (r *Resolver) authoritative(ctx context.Context, recursive []string, name string) (Answer, error) {
	zone, hosts, err := r.zoneNameservers(ctx, recursive, name)
	if err != nil {
		return Answer{}, err
	}
	var glue map[string][]netip.Addr
	for range maxReferrals + 1 {
		servers := r.addresses(ctx, recursive, hosts, glue)
		if len(servers) == 0 {
			return Answer{}, fmt.Errorf("no public address of %s's nameservers %s", strings.TrimSuffix(zone, "."), names(hosts))
		}
		var next string
		for _, s := range servers {
			in, err := r.exchange(ctx, name, dns.TypeCNAME, s.addr, false)
			if err != nil || ctx.Err() != nil {
				continue
			}
			if in.Authoritative && (in.Rcode == dns.RcodeSuccess || in.Rcode == dns.RcodeNameError) {
				return Answer{Target: cnameOf(in, name), From: s.host}, nil
			}
			if child, nextHosts, nextGlue := referral(in, zone, name); in.Rcode == dns.RcodeSuccess && child != "" {
				next, hosts, glue = child, nextHosts, nextGlue
				break
			}
		}
		if ctx.Err() != nil {
			return Answer{}, fmt.Errorf("%s's nameservers did not answer in time", strings.TrimSuffix(zone, "."))
		}
		if next == "" {
			return Answer{}, fmt.Errorf("none of %s's nameservers %s answered for it", strings.TrimSuffix(zone, "."), names(hosts))
		}
		zone = next
	}
	return Answer{}, fmt.Errorf("more than %d delegations below %s", maxReferrals, strings.TrimSuffix(zone, "."))
}

// zoneNameservers finds the zone name is in, and its nameservers: the NS
// of name or, failing that, of each name above it.
func (r *Resolver) zoneNameservers(ctx context.Context, recursive []string, name string) (string, []string, error) {
	for z := name; strings.Count(z, ".") > 1; z = z[strings.Index(z, ".")+1:] {
		var in *dns.Msg
		for _, s := range recursive {
			if m, err := r.exchange(ctx, z, dns.TypeNS, s, true); err == nil {
				in = m
				break
			}
		}
		if in == nil {
			return "", nil, fmt.Errorf("no resolver answered for the nameservers of %s", strings.TrimSuffix(z, "."))
		}
		var hosts []string
		for _, rr := range in.Answer {
			if ns, ok := rr.(*dns.NS); ok && strings.EqualFold(ns.Hdr.Name, z) {
				hosts = append(hosts, strings.ToLower(ns.Ns))
			}
		}
		if len(hosts) > 0 {
			return z, hosts, nil
		}
	}
	return "", nil, fmt.Errorf("no zone above %s has nameservers", strings.TrimSuffix(name, "."))
}

type server struct {
	host string
	addr string
}

// addresses are where at most maxNameservers of hosts are asked, at most
// maxAddresses in all: their glue if given, else their A and AAAA records
// through the recursive resolvers; public addresses alone.
func (r *Resolver) addresses(ctx context.Context, recursive []string, hosts []string, glue map[string][]netip.Addr) []server {
	var out []server
	for _, h := range hosts[:min(len(hosts), maxNameservers)] {
		addrs := glue[h]
		if len(addrs) == 0 {
			for _, t := range []uint16{dns.TypeA, dns.TypeAAAA} {
				for _, s := range recursive {
					if in, err := r.exchange(ctx, h, t, s, true); err == nil {
						addrs = append(addrs, addrsOf(in, h)...)
						break
					}
				}
			}
		}
		for _, a := range addrs {
			if !Public(a) || len(out) == maxAddresses {
				continue
			}
			at := net.JoinHostPort(a.String(), "53")
			if r.Nameserver != nil {
				at = r.Nameserver(h, a)
			}
			out = append(out, server{host: strings.TrimSuffix(h, "."), addr: at})
		}
	}
	return out
}

// exchange asks at the question; a recursive one over UDP, then TCP if the
// answer was cut, and an authoritative one over TCP alone.
func (r *Resolver) exchange(ctx context.Context, name string, qtype uint16, at string, recurse bool) (*dns.Msg, error) {
	q := new(dns.Msg)
	q.SetQuestion(name, qtype)
	q.RecursionDesired = recurse
	c := r.client()
	if !recurse {
		c.Net = "tcp"
	}
	in, _, err := c.ExchangeContext(ctx, q, at)
	if err != nil {
		return nil, err
	}
	if in.Truncated && c.Net != "tcp" {
		c.Net = "tcp"
		if in, _, err = c.ExchangeContext(ctx, q, at); err != nil {
			return nil, err
		}
	}
	return in, nil
}

func (r *Resolver) client() *dns.Client {
	t := r.Timeout
	if t == 0 {
		t = defaultTimeout
	}
	return &dns.Client{Timeout: t}
}

// ask asks the recursive resolvers what name is a CNAME of.
func (r *Resolver) ask(ctx context.Context, recursive []string, name string) (Answer, error) {
	for _, s := range recursive {
		in, err := r.exchange(ctx, name, dns.TypeCNAME, s, true)
		if err != nil || (in.Rcode != dns.RcodeSuccess && in.Rcode != dns.RcodeNameError) {
			continue
		}
		return Answer{Target: cnameOf(in, name), From: "a resolver"}, nil
	}
	return Answer{}, errors.New("no resolver answered")
}

// cnameOf is the target of name's own CNAME in the answer, not of another
// name's, as one further along a chain.
func cnameOf(in *dns.Msg, name string) string {
	for _, rr := range in.Answer {
		if c, ok := rr.(*dns.CNAME); ok && strings.EqualFold(c.Hdr.Name, name) {
			return strings.ToLower(strings.TrimSuffix(c.Target, "."))
		}
	}
	return ""
}

// addrsOf are the addresses the answer gives for host.
func addrsOf(in *dns.Msg, host string) []netip.Addr {
	var out []netip.Addr
	for _, rr := range in.Answer {
		if !strings.EqualFold(rr.Header().Name, host) {
			continue
		}
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

// referral is the zone a non-authoritative answer from zone's nameservers
// delegates name to, its nameservers and the addresses given for them: a
// zone strictly below zone, at name or above it; "" for none.
func referral(in *dns.Msg, zone, name string) (string, []string, map[string][]netip.Addr) {
	var child string
	var hosts []string
	for _, rr := range in.Ns {
		ns, ok := rr.(*dns.NS)
		if !ok {
			continue
		}
		owner := strings.ToLower(ns.Hdr.Name)
		if !strings.HasSuffix(owner, "."+zone) || (owner != name && !strings.HasSuffix(name, "."+owner)) {
			continue
		}
		if child != "" && owner != child {
			continue
		}
		child = owner
		hosts = append(hosts, strings.ToLower(ns.Ns))
	}
	glue := map[string][]netip.Addr{}
	for _, h := range hosts {
		glue[h] = addrsOf(&dns.Msg{Answer: in.Extra}, h)
	}
	return child, hosts, glue
}

func names(hosts []string) string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = strings.TrimSuffix(h, ".")
	}
	return strings.Join(out, ", ")
}
