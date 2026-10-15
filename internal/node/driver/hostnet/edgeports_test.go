package hostnet

import (
	"net/netip"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

// TestEdgePortsPass: a tap's edge ports let TCP from the gateway to those
// ports in, first in its own ingress chain, each port once, whatever its
// groups say; nothing else passes by them: not another tap's ports, not
// UDP, not another address, not another port.
func TestEdgePortsPass(t *testing.T) {
	gw := netip.MustParseAddr("10.0.0.1")
	served := tapPolicy{name: "hnt000000000001", policy: secgroup.Policy{EgressAll: true}, edgePorts: []uint16{8080, 80, 8080}}
	other := tapPolicy{name: "hnt000000000002", policy: secgroup.Policy{EgressAll: true}}
	cs, err := firewallChains(gw, []tapPolicy{served, other})
	if err != nil {
		t.Fatal(err)
	}
	edge := func(c chainSpec) []ruleSpec {
		var out []ruleSpec
		for _, r := range c.rules {
			if strings.Contains(r.says, "public endpoints") {
				out = append(out, r)
			}
		}
		return out
	}
	for _, c := range cs {
		rules := edge(c)
		switch c.name {
		case "in_" + served.name:
			if len(rules) != 2 || c.rules[0].says != rules[0].says || c.rules[1].says != rules[1].says {
				t.Fatalf("%s: edge rules %d, first rules %q, %q", c.name, len(rules), c.rules[0].says, c.rules[1].says)
			}
		default:
			if len(rules) != 0 {
				t.Errorf("%s lets the edge through", c.name)
			}
		}
	}
	var in []ruleSpec
	for _, c := range cs {
		if c.name == "in_"+served.name {
			in = edge(c)
		}
	}
	tcp, udp := byte(unix.IPPROTO_TCP), byte(unix.IPPROTO_UDP)
	for _, tc := range []struct {
		what string
		p    packet
		want bool
	}{
		{"TCP from the gateway to port 8080", packet{tcp, "10.0.0.1", "10.0.0.5", 40000, 8080}, true},
		{"TCP from the gateway to port 80", packet{tcp, "10.0.0.1", "10.0.0.5", 40000, 80}, true},
		{"TCP from the gateway to port 9090", packet{tcp, "10.0.0.1", "10.0.0.5", 40000, 9090}, false},
		{"UDP from the gateway to port 8080", packet{udp, "10.0.0.1", "10.0.0.5", 40000, 8080}, false},
		{"TCP from another guest to port 8080", packet{tcp, "10.0.0.6", "10.0.0.5", 40000, 8080}, false},
	} {
		got := false
		for _, r := range in {
			got = got || accepts(t, r, tc.p)
		}
		if got != tc.want {
			t.Errorf("%s: accepted %t, want %t", tc.what, got, tc.want)
		}
	}
}
