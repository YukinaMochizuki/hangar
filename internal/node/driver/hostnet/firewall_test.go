package hostnet

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/google/nftables/expr"

	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

func TestFirewallChains(t *testing.T) {
	gw := netip.MustParseAddr("10.0.0.1")
	none, err := firewallChains(gw, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := func(cs []chainSpec) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.name)
		}
		return out
	}
	if got := names(none); !slices.Equal(got, []string{"prerouting", "forward", "output"}) {
		t.Fatalf("no taps: chains %v", got)
	}
	// With no tap's chains, every base chain ends in a drop: a tap without
	// a policy is closed both ways.
	for _, c := range none {
		if last := c.rules[len(c.rules)-1]; last.says != "nothing else" || verdictOf(last) != expr.VerdictDrop {
			t.Errorf("%s ends with %q, verdict %v", c.name, last.says, verdictOf(last))
		}
	}
	// The DHCP exceptions are only DHCP: to the broadcast address, the
	// servers' group or the gateway from a tap, and from the gateway to one.
	for _, c := range none {
		for _, r := range c.rules {
			if !strings.Contains(r.says, "DHCP") {
				continue
			}
			if !slices.ContainsFunc([]string{"10.0.0.1", "255.255.255.255", "ff02::1:2", "fe80::1"}, func(a string) bool {
				return matchesAddr(r, netip.MustParseAddr(a))
			}) {
				t.Errorf("%s: %q matches any address", c.name, r.says)
			}
		}
	}

	web := tapPolicy{name: "hnt000000000001", policy: secgroup.Policy{
		Ingress: []secgroup.Rule{{Protocol: secgroup.TCP, Ports: []secgroup.PortRange{{From: 443, To: 443}, {From: 80, To: 80}},
			Peers: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}},
		EgressAll: true,
	}}
	sandbox := tapPolicy{name: "hnt000000000002", policy: secgroup.Policy{
		Egress: []secgroup.Rule{{Protocol: secgroup.TCP, Ports: []secgroup.PortRange{{From: 443, To: 443}},
			Peers: []netip.Prefix{netip.MustParsePrefix("140.82.112.0/20"), netip.MustParsePrefix("185.199.108.0/22")}}},
	}}
	cs, err := firewallChains(gw, []tapPolicy{sandbox, web})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]chainSpec{}
	for _, c := range cs {
		byName[c.name] = c
	}
	if in := byName["in_hnt000000000001"]; len(in.rules) != 2 || !in.regular {
		t.Errorf("web's ingress: a rule per port range, got %d", len(in.rules))
	}
	if out := byName["out_hnt000000000001"]; len(out.rules) != 1 || !strings.Contains(out.rules[0].says, "sends anything") {
		t.Errorf("web's egress: %v", out.rules)
	}
	if out := byName["out_hnt000000000002"]; len(out.rules) != 3 || verdictOf(out.rules[2]) != expr.VerdictDrop {
		t.Errorf("sandbox's egress: a rule per peer, then a drop: %v", out.rules)
	}
	if in := byName["in_hnt000000000002"]; len(in.rules) != 0 {
		t.Errorf("sandbox's ingress lets in what no rule allows: %v", in.rules)
	}
	// Listing order does not change the firewall; a policy change does.
	again, _ := firewallChains(gw, []tapPolicy{web, sandbox})
	if !slices.Equal(comments(t, cs), comments(t, again)) {
		t.Error("the firewall depends on the order taps are listed in")
	}
	opened := sandbox
	opened.policy = secgroup.Policy{EgressAll: true}
	if changed, _ := firewallChains(gw, []tapPolicy{web, opened}); slices.Equal(comments(t, cs), comments(t, changed)) {
		t.Error("a changed policy reads as the old one")
	}
	// The network's services stay reachable whatever a tap may send.
	pre := strings.Join(comments(t, []chainSpec{byName["prerouting"]}), "\n")
	for _, want := range []string{"resolver at 10.0.0.1", "metadata service", "DHCP broadcast from a tap", "DHCP to the server at 10.0.0.1"} {
		if !strings.Contains(pre, want) {
			t.Errorf("prerouting lacks %q:\n%s", want, pre)
		}
	}
}

// TestICMPv6PassesOnlyOnTheLink: what passes to a tap whatever its groups,
// neighbour discovery and listener reports, is held to the hop limit only
// a sender on the link can give it and, but from the tap itself, to a
// link-local or no source, which nothing routed has: the internet's ICMPv6
// is the groups' to let in.
func TestICMPv6PassesOnlyOnTheLink(t *testing.T) {
	cs, err := firewallChains(netip.MustParseAddr("10.0.0.1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		for _, r := range c.rules {
			icmp6 := slices.ContainsFunc(r.exprs, func(e expr.Any) bool {
				m, ok := e.(*expr.Cmp)
				return ok && slices.Equal(m.Data, []byte{58})
			})
			if !icmp6 || verdictOf(r) != expr.VerdictAccept {
				continue
			}
			hop := slices.ContainsFunc(r.exprs, func(e expr.Any) bool {
				p, ok := e.(*expr.Payload)
				return ok && p.Base == expr.PayloadBaseNetworkHeader && p.Offset == 7
			})
			src := slices.ContainsFunc(r.exprs, func(e expr.Any) bool {
				p, ok := e.(*expr.Payload)
				return ok && p.Base == expr.PayloadBaseNetworkHeader && p.Offset == 8
			})
			if !hop || (c.name != "prerouting" && !src) {
				t.Errorf("%s: %q passes ICMPv6 a router could have forwarded (hop limit checked %v, source %v)", c.name, r.says, hop, src)
			}
		}
	}
}

// TestPeersMatchTheirOwnFamily: an IPv6 peer matches IPv6 only, and icmp
// with it ICMPv6; an IPv4 peer IPv4 only, so ::/0 opens nothing to IPv4
// and 0.0.0.0/0 nothing to IPv6.
func TestPeersMatchTheirOwnFamily(t *testing.T) {
	rules, err := policyRules("hnt1", "in", []secgroup.Rule{
		{Protocol: secgroup.ICMP, Peers: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}},
	}, true)
	if err != nil || len(rules) != 2 {
		t.Fatalf("%v %v", rules, err)
	}
	family := func(r ruleSpec) (ether []byte, l4 byte) {
		for i, e := range r.exprs {
			if m, ok := e.(*expr.Meta); ok && i+1 < len(r.exprs) {
				c := r.exprs[i+1].(*expr.Cmp)
				switch m.Key {
				case expr.MetaKeyPROTOCOL:
					ether = c.Data
				case expr.MetaKeyL4PROTO:
					l4 = c.Data[0]
				}
			}
		}
		return ether, l4
	}
	if e, l4 := family(rules[0]); !slices.Equal(e, etherIPv4) || l4 != 1 {
		t.Errorf("0.0.0.0/0 icmp matches ether type %x, protocol %d", e, l4)
	}
	if e, l4 := family(rules[1]); !slices.Equal(e, etherIPv6) || l4 != 58 {
		t.Errorf("::/0 icmp matches ether type %x, protocol %d", e, l4)
	}
}

// verdictOf is what a rule does with what it matches.
func verdictOf(r ruleSpec) expr.VerdictKind {
	if v, ok := r.exprs[len(r.exprs)-1].(*expr.Verdict); ok {
		return v.Kind
	}
	return expr.VerdictContinue
}

// matchesAddr says whether r compares some address with a.
func matchesAddr(r ruleSpec, a netip.Addr) bool {
	return slices.ContainsFunc(r.exprs, func(e expr.Any) bool {
		c, ok := e.(*expr.Cmp)
		return ok && c.Op == expr.CmpOpEq && slices.Equal(c.Data, a.AsSlice())
	})
}
