package hostnet

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

var (
	testRootEndpoints = []rootEndpoint{{proto: driver.TCP, veth: vethPrefix + "0123456789ab", service: netip.MustParseAddr("100.64.0.5"), port: 1024}}
	testNSEndpoints   = nsEndpoints{
		in: []nsEndpointIn{{proto: driver.TCP, at: netip.MustParseAddrPort("10.0.0.7:22"), via: netip.MustParseAddrPort("100.64.0.5:1024")}},
		to: []nsEndpointTo{{proto: driver.UDP, port: 1025, to: netip.MustParseAddrPort("10.0.0.2:53")}},
	}
)

// The root forward chain lets across from one network into another, before
// "only replies into a network" turns every other new connection away, what
// the endpoint table marked and nothing a network opens to the internet,
// whatever its veth's index; with or without a shared address.
func TestEndpointsAreLetAcrossNetworks(t *testing.T) {
	for _, shared := range []netip.Addr{{}, netip.MustParseAddr("192.0.2.2")} {
		rules := forwardOf(t, rootChains("eth0", netip.MustParseAddr("192.0.2.1"), shared)).rules
		across := slices.IndexFunc(rules, func(r ruleSpec) bool { return strings.HasPrefix(r.says, "from a network into another") })
		replies := slices.IndexFunc(rules, func(r ruleSpec) bool { return r.says == "only replies into a network" })
		if across < 0 || replies < 0 || across > replies {
			t.Fatalf("shared %s: the forward chain lets endpoints across at %d and only replies at %d", shared, across, replies)
		}
		r := rules[across]
		var mask, want uint32
		for i, e := range r.exprs {
			if ct, ok := e.(*expr.Ct); ok && !ct.SourceRegister && ct.Key == expr.CtKeyMARK {
				mask = binaryutil.NativeEndian.Uint32(r.exprs[i+1].(*expr.Bitwise).Mask)
				want = binaryutil.NativeEndian.Uint32(r.exprs[i+2].(*expr.Cmp).Data)
			}
		}
		if mask != endpointMark|natMarkTag || want != endpointMark ||
			!matchesMeta(r, expr.MetaKeyIIFNAME, []byte(vethPrefix)) || !matchesMeta(r, expr.MetaKeyOIFNAME, []byte(vethPrefix)) {
			t.Errorf("shared %s: the rule letting endpoints across is %q, mask %#x, want %#x", shared, r.says, mask, want)
		}
	}
	// The marks are apart: an endpoint's connection is never taken for a
	// forward's or for one the NAT log follows, nor the other way.
	if endpointMark&(natMarkTag|forwardMark) != 0 {
		t.Errorf("the endpoints' mark %#x shares a bit with the NAT log's tag %#x or the forwards' %#x", endpointMark, natMarkTag, forwardMark)
	}
}

// The root endpoint table runs before the driver's forward chain, so its
// mark is set by the time that chain reads it, and marks only new
// connections from the endpoint's network to the service's transit port.
func TestTheRootEndpointTableMarksFirst(t *testing.T) {
	c := rootEndpointChains(testRootEndpoints)[0]
	if c.hook != *nftables.ChainHookForward || c.prio >= *nftables.ChainPriorityFilter {
		t.Errorf("the root endpoint chain is at hook %d priority %d, want forward before %d", c.hook, c.prio, *nftables.ChainPriorityFilter)
	}
	r := c.rules[0]
	if !matchesMeta(r, expr.MetaKeyIIFNAME, ifname(testRootEndpoints[0].veth)) || !setsEndpointMark(r) {
		t.Errorf("the root endpoint rule %q", r.says)
	}
}

// An endpoint's address takes its port alone: anything else to it is
// dropped in the namespace's input.
func TestAnEndpointsAddressTakesItsPortAlone(t *testing.T) {
	chains := nsEndpointChains(netip.MustParseAddr("100.64.0.1"), testNSEndpoints)
	input := slices.IndexFunc(chains, func(c chainSpec) bool { return c.name == "input" })
	if input < 0 || len(chains[input].rules) != 1 {
		t.Fatalf("the namespace's endpoint table has no input rule per address: %+v", chains)
	}
	r := chains[input].rules[0]
	last, _ := r.exprs[len(r.exprs)-1].(*expr.Verdict)
	if last == nil || last.Kind != expr.VerdictDrop || !strings.Contains(r.says, "10.0.0.7") {
		t.Errorf("the input rule is %q, ending %v", r.says, last)
	}
}

// What the endpoint tables are written with reads back as what they hold,
// for Endpoints.
func TestEndpointRulesReadBack(t *testing.T) {
	root := rootEndpointChains(testRootEndpoints)[0].rules[0]
	m, ok := decodeEndpointRule(&nftables.Rule{Exprs: root.exprs})
	want := testRootEndpoints[0]
	if !ok || m.iif != want.veth || m.daddr != want.service || m.proto != want.proto || m.dport != want.port || m.to.IsValid() {
		t.Errorf("the root rule reads back as %+v (%t)", m, ok)
	}
	nat := nsEndpointChains(netip.MustParseAddr("100.64.0.1"), testNSEndpoints)[0].rules
	in, to := testNSEndpoints.in[0], testNSEndpoints.to[0]
	if m, ok := decodeEndpointRule(&nftables.Rule{Exprs: nat[0].exprs}); !ok || m.iif != nsBridge || m.daddr != in.at.Addr() ||
		m.dport != in.at.Port() || m.proto != in.proto || m.to != in.via {
		t.Errorf("the endpoint's rule reads back as %+v (%t)", m, ok)
	}
	if m, ok := decodeEndpointRule(&nftables.Rule{Exprs: nat[1].exprs}); !ok || m.iif != nsUplink || m.daddr != netip.MustParseAddr("100.64.0.1") ||
		m.dport != to.port || m.proto != to.proto || m.to != to.to {
		t.Errorf("the service's rule reads back as %+v (%t)", m, ok)
	}
	// A rule of another table is no endpoint.
	if _, ok := decodeEndpointRule(&nftables.Rule{Exprs: forwardOf(t, rootChains("eth0", netip.Addr{}, netip.Addr{})).rules[0].exprs}); ok {
		t.Error("a rule of ip hangar reads back as an endpoint")
	}
}

// setsEndpointMark says whether r sets a connection's mark to endpointMark.
func setsEndpointMark(r ruleSpec) bool {
	for i, e := range r.exprs[:len(r.exprs)-1] {
		im, ok := e.(*expr.Immediate)
		ct, isCt := r.exprs[i+1].(*expr.Ct)
		if ok && isCt && ct.Key == expr.CtKeyMARK && ct.SourceRegister && ct.Register == im.Register &&
			slices.Equal(im.Data, binaryutil.NativeEndian.PutUint32(endpointMark)) {
			return true
		}
	}
	return false
}
