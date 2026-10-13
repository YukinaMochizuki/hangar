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
	// The DHCP exceptions are only DHCP: to the broadcast address or the
	// gateway from a tap, and from the gateway to one.
	for _, c := range none {
		for _, r := range c.rules {
			if !strings.Contains(r.says, "DHCP") {
				continue
			}
			if !matchesAddr(r, gw) && !matchesAddr(r, netip.MustParseAddr("255.255.255.255")) {
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
