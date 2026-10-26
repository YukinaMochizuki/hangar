package hostnet

import (
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

// dnatOf is what a rule DNATs to: the address, and the port if it names
// one; ok is false for a rule that DNATs nothing.
func dnatOf(r ruleSpec) (to netip.Addr, port uint16, ok bool) {
	regs := map[uint32][]byte{}
	for _, e := range r.exprs {
		switch e := e.(type) {
		case *expr.Immediate:
			regs[e.Register] = e.Data
		case *expr.NAT:
			if e.Type != expr.NATTypeDestNAT {
				return
			}
			to, _ = netip.AddrFromSlice(regs[e.RegAddrMin])
			if e.RegProtoMin != 0 {
				port = binaryutil.BigEndian.Uint16(regs[e.RegProtoMin])
			}
			return to, port, true
		}
	}
	return
}

// daddrOf is the single destination address r matches, if any.
func daddrOf(r ruleSpec) (netip.Addr, bool) {
	for i, e := range r.exprs {
		p, ok := e.(*expr.Payload)
		if !ok || p.Base != expr.PayloadBaseNetworkHeader || p.Offset != 16 {
			continue
		}
		if c, ok := r.exprs[i+2].(*expr.Cmp); ok {
			return netip.AddrFromSlice(c.Data)
		}
	}
	return netip.Addr{}, false
}

// natsSource says whether any rule of chains SNATs or masquerades.
func natsSource(chains []chainSpec) bool {
	for _, c := range chains {
		for _, r := range c.rules {
			for _, e := range r.exprs {
				if n, ok := e.(*expr.NAT); ok && n.Type == expr.NATTypeSourceNAT {
					return true
				}
				if _, ok := e.(*expr.Masq); ok {
					return true
				}
			}
		}
	}
	return false
}

// The root table takes each port of the shared address coming in by the
// uplink to its network's namespace, on the same port, and nothing else.
func TestTheRootForwardsTheSharedAddressToANetwork(t *testing.T) {
	shared := netip.MustParseAddr("192.0.2.2")
	fs := []rootForward{
		{proto: driver.TCP, port: 10022, to: netip.MustParseAddr("100.64.0.1")},
		{proto: driver.UDP, port: 10053, to: netip.MustParseAddr("100.64.0.3")},
	}
	chains := forwardChains("eth0", shared, fs)
	if len(chains) != 1 || chains[0].typ != nftables.ChainTypeNAT || chains[0].hook != *nftables.ChainHookPrerouting ||
		chains[0].prio != *nftables.ChainPriorityNATDest {
		t.Fatalf("the forward table is %+v, want one NAT chain at prerouting's dstnat priority", chains)
	}
	rules := chains[0].rules
	if len(rules) != len(fs) {
		t.Fatalf("%d rules for %d forwards", len(rules), len(fs))
	}
	for i, f := range fs {
		r := rules[i]
		from, through, _ := dportsOf(r)
		to, port, ok := dnatOf(r)
		daddr, _ := daddrOf(r)
		if !matchesMeta(r, expr.MetaKeyIIFNAME, ifname("eth0")) || !matchesMeta(r, expr.MetaKeyL4PROTO, []byte{protoOf(f.proto)}) ||
			daddr != shared || from != f.port || through != f.port || !ok || to != f.to || port != 0 {
			t.Errorf("forward %d: rule %q matches ports %d-%d at %s, DNATs to %s:%d", i, r.says, from, through, daddr, to, port)
		}
	}
	if natsSource(chains) {
		t.Error("the forward table SNATs: the client's address must reach the machine")
	}
	for _, r := range rules {
		if !setsForwardMark(r) {
			t.Errorf("rule %q does not mark its connections as a forward's", r.says)
		}
	}
	// Another port, protocol, shared address or namespace is another rule.
	base := comments(t, forwardChains("eth0", shared, fs[:1]))
	for _, other := range [][]chainSpec{
		forwardChains("eth0", shared, []rootForward{{proto: driver.TCP, port: 10023, to: fs[0].to}}),
		forwardChains("eth0", shared, []rootForward{{proto: driver.UDP, port: 10022, to: fs[0].to}}),
		forwardChains("eth0", netip.MustParseAddr("192.0.2.3"), fs[:1]),
		forwardChains("eth0", shared, []rootForward{{proto: driver.TCP, port: 10022, to: fs[1].to}}),
		forwardChains("eth1", shared, fs[:1]),
	} {
		if slices.Equal(comments(t, other), base) {
			t.Errorf("forwards of other parameters share the comments %q", base)
		}
	}
}

// A namespace takes each of its forwards' ports at its end of the transit
// link to its machine's address and port, the client's address as it is.
func TestANetworkForwardsToItsMachine(t *testing.T) {
	nsTransit := netip.MustParseAddr("100.64.0.1")
	fs := []driver.Forward{
		{Protocol: driver.TCP, Port: 10022, NetworkUID: "n1", To: netip.MustParseAddrPort("10.0.0.2:22")},
		{Protocol: driver.UDP, Port: 10053, NetworkUID: "n1", To: netip.MustParseAddrPort("10.0.0.3:53")},
	}
	chains := nsForwardChains(nsTransit, fs)
	if len(chains) != 1 || chains[0].typ != nftables.ChainTypeNAT || chains[0].prio != *nftables.ChainPriorityNATDest {
		t.Fatalf("the namespace's forward table is %+v, want one NAT chain at dstnat priority", chains)
	}
	for i, f := range fs {
		r := chains[0].rules[i]
		from, _, _ := dportsOf(r)
		to, port, ok := dnatOf(r)
		daddr, _ := daddrOf(r)
		if !matchesMeta(r, expr.MetaKeyIIFNAME, ifname(nsUplink)) || !matchesMeta(r, expr.MetaKeyL4PROTO, []byte{protoOf(f.Protocol)}) ||
			daddr != nsTransit || from != f.Port || !ok || netip.AddrPortFrom(to, port) != f.To {
			t.Errorf("forward %d: rule %q matches %s:%d, DNATs to %s:%d", i, r.says, daddr, from, to, port)
		}
	}
	if natsSource(chains) {
		t.Error("the namespace's forward table SNATs: the client's address must reach the machine")
	}
	for _, r := range chains[0].rules {
		if !setsForwardMark(r) {
			t.Errorf("rule %q does not mark its connections as a forward's", r.says)
		}
	}
	if slices.Equal(comments(t, chains), comments(t, nsForwardChains(nsTransit, []driver.Forward{fs[0], {
		Protocol: driver.UDP, Port: 10053, NetworkUID: "n1", To: netip.MustParseAddrPort("10.0.0.3:5353"),
	}}))) {
		t.Error("forwards to another target port share their comments")
	}
}

// With a shared address the root forward chain lets in, before turning
// away every other new connection into a network, what came by the uplink
// and the forward table DNATed and marked as a forward's, and only that.
func TestForwardsAreLetIntoANetwork(t *testing.T) {
	public, shared := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	rules := forwardOf(t, rootChains("eth0", public, shared)).rules
	in := slices.IndexFunc(rules, func(r ruleSpec) bool { return strings.HasPrefix(r.says, "forwards from") })
	replies := slices.IndexFunc(rules, func(r ruleSpec) bool { return r.says == "only replies into a network" })
	if in < 0 || replies < 0 || in > replies {
		t.Fatalf("the forward chain lets forwards in at %d and only replies at %d: %q", in, replies, comments(t, []chainSpec{{rules: rules}}))
	}
	r := rules[in]
	var status, marked bool
	for i, e := range r.exprs {
		ct, ok := e.(*expr.Ct)
		if !ok || ct.SourceRegister {
			continue
		}
		b, isBitwise := r.exprs[i+1].(*expr.Bitwise)
		c, isCmp := r.exprs[i+2].(*expr.Cmp)
		switch ct.Key {
		case expr.CtKeySTATUS:
			status = isBitwise && binaryutil.NativeEndian.Uint32(b.Mask) == ctStatusDNAT
		case expr.CtKeyMARK:
			marked = isBitwise && isCmp && binaryutil.NativeEndian.Uint32(b.Mask) == forwardMark &&
				c.Op == expr.CmpOpEq && slices.Equal(c.Data, binaryutil.NativeEndian.PutUint32(forwardMark))
		}
	}
	last, _ := r.exprs[len(r.exprs)-1].(*expr.Verdict)
	if !status || !marked || last == nil || last.Kind != expr.VerdictAccept ||
		!matchesMeta(r, expr.MetaKeyIIFNAME, ifname("eth0")) || !matchesMeta(r, expr.MetaKeyOIFNAME, []byte(vethPrefix)) {
		t.Errorf("the rule letting forwards in is %q: DNATed %t, marked as a forward's %t, accepts %v", r.says, status, marked, last)
	}
	// Nothing a network opens is marked as a forward's: the NAT log's tag
	// and the veth index are below forwardMark's bit, and no veth index
	// reaches it.
	if natMarkTag&forwardMark != 0 {
		t.Errorf("the NAT log's tag %#x has the forwards' bit %#x", natMarkTag, forwardMark)
	}
	// Without a shared address there is nothing to let in.
	if slices.ContainsFunc(forwardOf(t, rootChains("eth0", public, netip.Addr{})).rules, func(r ruleSpec) bool {
		return strings.HasPrefix(r.says, "forwards from")
	}) {
		t.Error("the forward chain lets forwards in without a shared address")
	}
}

// No rule the driver writes loads a conntrack key that takes a direction
// (CtKeySRC, CtKeyDST, CtKeyPROTOSRC, CtKeyPROTODST, as nft's "ct
// original ip daddr"): google/nftables v0.3.0 writes NFTA_CT_DIRECTION as
// a u32, the kernel echoes it back as a u8, and the library's unmarshal
// then refuses it, so the table holding such a rule could never be read
// back and every ensure after the first would fail.
func TestNoRuleTakesAConntrackDirection(t *testing.T) {
	public, shared := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	prefix := netip.MustParsePrefix("2001:db8:0:1::/64")
	host := netip.MustParsePrefix("10.0.0.2/32")
	firewall, err := firewallChains(netip.MustParseAddr("10.0.0.1"), []tapPolicy{
		{name: tapPrefix + "0123456789ab", edgePorts: []uint16{8080}, shares: []netip.Addr{netip.MustParseAddr("10.0.0.9")}, policy: secgroup.Policy{
			Ingress: []secgroup.Rule{{Protocol: secgroup.TCP, Ports: []secgroup.PortRange{{From: 22, To: 22}}, Peers: []netip.Prefix{host}}},
			Egress:  []secgroup.Rule{{Protocol: secgroup.UDP, Ports: []secgroup.PortRange{{From: 53, To: 53}}, Peers: []netip.Prefix{host}}},
		}},
		{name: serverTapPrefix + "0123456789a", clients: []netip.Addr{netip.MustParseAddr("10.0.0.2")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tables := map[string][]chainSpec{
		"ip hangar":                     rootChains("eth0", netip.Addr{}, netip.Addr{}),
		"ip hangar, public":             rootChains("eth0", public, netip.Addr{}),
		"ip hangar, shared":             rootChains("eth0", public, shared),
		"ip6 hangar":                    rootChains6("eth0", prefix, netip.MustParseAddr("2001:db8:0:1::1")),
		"a namespace's ip hangar":       nsChains(netip.MustParsePrefix("10.0.0.0/24"), netip.MustParseAddr("100.64.0.1")),
		"a namespace's ip6 hangar":      nsChains6(prefix),
		"inet hangar_limit":             limitChains(),
		"ip hangar_forward":             forwardChains("eth0", shared, []rootForward{{proto: driver.TCP, port: 10022, to: netip.MustParseAddr("100.64.0.1")}}),
		"a namespace's hangar_forward":  nsForwardChains(netip.MustParseAddr("100.64.0.1"), []driver.Forward{{Protocol: driver.UDP, Port: 10053, NetworkUID: "n1", To: netip.MustParseAddrPort("10.0.0.2:53")}}),
		"ip hangar_endpoint":            rootEndpointChains(testRootEndpoints),
		"a namespace's hangar_endpoint": nsEndpointChains(netip.MustParseAddr("100.64.0.1"), testNSEndpoints),
		"bridge hangar":                 bindingChains([]boundTap{{name: tapPrefix + "0123456789ab", mac: net.HardwareAddr{2, 0, 0, 0, 0, 1}, addr: host.Addr(), ipv6: netip.MustParseAddr("2001:db8:0:1::2")}}),
		"bridge hangar_sg":              firewall,
	}
	for name, chains := range tables {
		for _, c := range chains {
			for _, r := range c.rules {
				for _, e := range r.exprs {
					ct, ok := e.(*expr.Ct)
					if !ok {
						continue
					}
					switch ct.Key {
					case expr.CtKeySRC, expr.CtKeyDST, expr.CtKeyPROTOSRC, expr.CtKeyPROTODST:
						t.Errorf("%s: rule %q loads conntrack key %d, which takes a direction", name, r.says, ct.Key)
					}
				}
			}
		}
	}
}

// Without a shared address there is nothing to forward from; the rest of
// what a host refuses is the conformance suite's (drivertest).
func TestNoForwardWithoutASharedAddress(t *testing.T) {
	d := &Driver{cfg: Config{Uplink: "eth0"}}
	f := driver.Forward{Protocol: driver.TCP, Port: 10022, NetworkUID: "n1", To: netip.MustParseAddrPort("10.0.0.2:22")}
	if err := d.EnsureForwards(t.Context(), []driver.Forward{f}); err == nil {
		t.Error("a host without a shared address took a forward")
	}
}

// What the forward tables are written with reads back as the forwards
// they hold, for Forwards.
func TestForwardRulesReadBack(t *testing.T) {
	nsTransit := netip.MustParseAddr("100.64.0.1")
	root := forwardChains("eth0", netip.MustParseAddr("192.0.2.2"), []rootForward{{proto: driver.UDP, port: 10053, to: nsTransit}})
	k, to, ok := decodeForward(&nftables.Rule{Exprs: root[0].rules[0].exprs})
	if !ok || k != (forwardKey{unix.IPPROTO_UDP, 10053}) || to != netip.AddrPortFrom(nsTransit, 0) {
		t.Errorf("the root rule reads back as %+v to %s (%t)", k, to, ok)
	}
	f := driver.Forward{Protocol: driver.TCP, Port: 10022, NetworkUID: "n1", To: netip.MustParseAddrPort("10.0.0.2:22")}
	ns := nsForwardChains(nsTransit, []driver.Forward{f})
	k, to, ok = decodeForward(&nftables.Rule{Exprs: ns[0].rules[0].exprs})
	if !ok || k != (forwardKey{unix.IPPROTO_TCP, 10022}) || to != f.To {
		t.Errorf("the namespace's rule reads back as %+v to %s (%t)", k, to, ok)
	}
	// A rule of another table is no forward.
	if _, _, ok := decodeForward(&nftables.Rule{Exprs: forwardOf(t, rootChains("eth0", netip.Addr{}, netip.Addr{})).rules[0].exprs}); ok {
		t.Error("a rule of ip hangar reads back as a forward")
	}
}

// setsForwardMark says whether r sets a connection's mark to forwardMark.
func setsForwardMark(r ruleSpec) bool {
	for i, e := range r.exprs[:len(r.exprs)-1] {
		im, ok := e.(*expr.Immediate)
		ct, isCt := r.exprs[i+1].(*expr.Ct)
		if ok && isCt && ct.Key == expr.CtKeyMARK && ct.SourceRegister && ct.Register == im.Register &&
			slices.Equal(im.Data, binaryutil.NativeEndian.PutUint32(forwardMark)) {
			return true
		}
	}
	return false
}
