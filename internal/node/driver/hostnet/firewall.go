package hostnet

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

// tapPolicy is a tap with what its interface may receive and send.
type tapPolicy struct {
	name   string
	policy secgroup.Policy
}

func (d *Driver) EnsureFirewall(_ context.Context, networkUID string, policies []driver.InterfacePolicy) error {
	if err := checkUID(networkUID); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	ns, ok, err := openNS(networkUID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("network %s has no namespace on the host", networkUID)
	}
	defer func() { _ = ns.Close() }()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return err
	}
	defer h.Close()
	gateway, err := gatewayOf(h)
	if err != nil {
		return fmt.Errorf("network %s: %w", networkUID, err)
	}
	links, err := h.LinkList()
	if err != nil {
		return err
	}
	byIface := map[string]secgroup.Policy{}
	for _, p := range policies {
		byIface[p.MachineUID+"/"+p.Name] = p.Policy
	}
	var taps []tapPolicy
	size := 0
	for _, l := range links {
		m, ok := decodeTap(l.Attrs().Alias)
		if !isTap(l) || !ok {
			continue
		}
		if p, ok := byIface[m.Machine+"/"+m.Name]; ok {
			taps = append(taps, tapPolicy{name: l.Attrs().Name, policy: p})
			size += p.Size()
		}
	}
	if size > driver.MaxFirewallRules {
		return fmt.Errorf("network %s: security groups make %d rules, more than %d", networkUID, size, driver.MaxFirewallRules)
	}
	chains, err := firewallChains(gateway, taps)
	if err != nil {
		return fmt.Errorf("network %s: %w", networkUID, err)
	}
	return writeFirewall(ns, chains)
}

// ensureFirewallTable makes the namespace's firewall if it has none: with no
// policy yet, every tap receives and sends nothing but its network's
// services and replies. A firewall there already is the agent's to keep.
func ensureFirewallTable(ns netns.NsHandle, gateway netip.Addr) error {
	conn, err := nftables.New(nftables.WithNetNSFd(int(ns)))
	if err != nil {
		return err
	}
	tables, err := conn.ListTablesOfFamily(nftables.TableFamilyBridge)
	if err != nil {
		return fmt.Errorf("listing nft tables: %w", err)
	}
	if slices.ContainsFunc(tables, func(t *nftables.Table) bool { return t.Name == FirewallTable }) {
		return nil
	}
	chains, _ := firewallChains(gateway, nil)
	return writeFirewall(ns, chains)
}

func writeFirewall(ns netns.NsHandle, chains []chainSpec) error {
	conn, err := nftables.New(nftables.WithNetNSFd(int(ns)))
	if err != nil {
		return err
	}
	if err := ensureNamedTable(conn, nftables.TableFamilyBridge, FirewallTable, chains); err != nil {
		return fmt.Errorf("security groups: %w", err)
	}
	return nil
}

// gatewayOf is the address the namespace's bridge holds for its network.
func gatewayOf(h *netlink.Handle) (netip.Addr, error) {
	br, err := linkByName(h, nsBridge)
	if err != nil || br == nil {
		return netip.Addr{}, fmt.Errorf("no bridge: %w", err)
	}
	p, ok, err := addrOf(h, br)
	if err != nil || !ok {
		return netip.Addr{}, fmt.Errorf("the bridge has no gateway: %w", err)
	}
	return p.Addr(), nil
}

// firewallChains is a network's firewall, as the bridge-family table it is
// written as (docs/decisions/networking.md). What a tap sends is checked in
// prerouting, what is sent to it in forward (from another tap) and output
// (from the namespace, or routed there); a connection's replies pass either
// way, and a tap with no chains of its own sends and receives nothing else.
func firewallChains(gateway netip.Addr, taps []tapPolicy) ([]chainSpec, error) {
	taps = slices.Clone(taps)
	slices.SortFunc(taps, func(a, b tapPolicy) int { return strings.Compare(a.name, b.name) })
	accept := []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}
	drop := []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}}
	notTap := ruleSpec{says: "what is not a tap's passes", exprs: slices.Concat(ifnameNotPrefix(expr.MetaKeyIIFNAME, tapPrefix), accept)}
	notToTap := ruleSpec{says: "what is not for a tap passes", exprs: slices.Concat(ifnameNotPrefix(expr.MetaKeyOIFNAME, tapPrefix), accept)}
	replies := ruleSpec{says: "a connection's replies pass", exprs: slices.Concat(ctEstablishedOrRelated(), accept)}
	arp := ruleSpec{says: "ARP passes", exprs: slices.Concat(matchEtherType(etherARP), accept)}

	// Neighbour discovery and listener reports on the link, which no
	// group decides. A router never forwards a packet with hop limit 255,
	// which neighbour discovery needs (RFC 4861), nor one from a link-local
	// address, so nothing routed, as from the internet, passes as either.
	// From a tap, what the binding holds to its own addresses: a guest
	// answers a solicitation for its /128 from it.
	v6 := matchEtherType(etherIPv6)
	unspecified := netip.MustParsePrefix("::/128")
	ndFromTap := ruleSpec{says: "neighbour discovery from a tap passes",
		exprs: slices.Concat(v6, matchHopLimit(255), matchICMPv6Types(133, 136), accept)}
	ndOnLink := ruleSpec{says: "neighbour discovery between link-local addresses passes",
		exprs: slices.Concat(v6, matchHopLimit(255), matchSaddr6(linkLocal), matchICMPv6Types(133, 136), accept)}
	dad := ruleSpec{says: "duplicate address detection passes",
		exprs: slices.Concat(v6, matchHopLimit(255), matchSaddr6(unspecified), matchICMPv6Type(135), accept)}
	mld := []ruleSpec{
		{says: "listener queries and reports between link-local addresses pass",
			exprs: slices.Concat(v6, matchHopLimit(1), matchSaddr6(linkLocal), matchICMPv6Types(130, 132), accept)},
		{says: "listener reports between link-local addresses pass",
			exprs: slices.Concat(v6, matchHopLimit(1), matchSaddr6(linkLocal), matchICMPv6Type(143), accept)},
		{says: "listener reports from no address pass",
			exprs: slices.Concat(v6, matchHopLimit(1), matchSaddr6(unspecified), matchICMPv6Type(143), accept)},
	}
	egress := slices.Concat([]ruleSpec{notTap, replies, arp, ndFromTap, dad}, mld, []ruleSpec{
		{says: "DHCPv6 to the servers' group passes",
			exprs: slices.Concat(matchEtherType(etherIPv6), matchDaddr6(netip.MustParsePrefix("ff02::1:2/128")), matchUDPPorts(546, 547), accept)},
		{says: "DHCP broadcast from a tap passes",
			exprs: slices.Concat(matchEtherType(etherIPv4), matchDaddr(netip.MustParsePrefix("255.255.255.255/32")), matchUDPPorts(68, 67), accept)},
		{says: fmt.Sprintf("DHCP to the server at %s passes", gateway),
			exprs: slices.Concat(matchEtherType(etherIPv4), matchDaddr(netip.PrefixFrom(gateway, 32)), matchUDPPorts(68, 67), accept)},
		{says: fmt.Sprintf("the resolver at %s is always reachable", gateway),
			exprs: slices.Concat(matchEtherType(etherIPv4), matchDaddr(netip.PrefixFrom(gateway, 32)), matchL4(unix.IPPROTO_UDP), matchDport(53, 53), accept)},
		{says: fmt.Sprintf("the resolver at %s is always reachable over TCP", gateway),
			exprs: slices.Concat(matchEtherType(etherIPv4), matchDaddr(netip.PrefixFrom(gateway, 32)), matchL4(unix.IPPROTO_TCP), matchDport(53, 53), accept)},
		{says: "the metadata service is always reachable",
			exprs: slices.Concat(matchEtherType(etherIPv4), matchDaddr(netip.PrefixFrom(driver.MetadataAddr, 32)), matchL4(unix.IPPROTO_TCP), matchDport(80, 80), accept)},
	})
	forward := slices.Concat([]ruleSpec{notToTap, replies, arp, ndOnLink, dad}, mld)
	output := slices.Concat([]ruleSpec{notToTap, replies, arp, ndOnLink}, mld, []ruleSpec{
		{says: fmt.Sprintf("DHCP replies from the server at %s pass", gateway),
			exprs: slices.Concat(matchEtherType(etherIPv4), matchSaddr(netip.PrefixFrom(gateway, 32)), matchUDPPorts(67, 68), accept)},
		{says: fmt.Sprintf("DHCPv6 replies from the server at %s pass", driver.GatewayIPv6),
			exprs: slices.Concat(matchEtherType(etherIPv6), matchSaddr6(netip.PrefixFrom(driver.GatewayIPv6, 128)), matchUDPPorts(547, 546), accept)},
	})
	var chains []chainSpec
	for _, t := range taps {
		in, out := "in_"+t.name, "out_"+t.name
		inRules, err := policyRules(t.name, "in", t.policy.Ingress, true)
		if err != nil {
			return nil, err
		}
		var outRules []ruleSpec
		if t.policy.EgressAll {
			outRules = []ruleSpec{{says: t.name + " sends anything", exprs: accept}}
		} else {
			if outRules, err = policyRules(t.name, "out", t.policy.Egress, false); err != nil {
				return nil, err
			}
			outRules = append(outRules, ruleSpec{says: t.name + " sends nothing else", exprs: drop})
		}
		chains = append(chains, chainSpec{name: in, regular: true, rules: inRules}, chainSpec{name: out, regular: true, rules: outRules})
		jumpIn := slices.Concat(matchIfname(expr.MetaKeyOIFNAME, t.name), []expr.Any{&expr.Verdict{Kind: expr.VerdictJump, Chain: in}})
		jumpOut := slices.Concat(matchIfname(expr.MetaKeyIIFNAME, t.name), []expr.Any{&expr.Verdict{Kind: expr.VerdictJump, Chain: out}})
		egress = append(egress, ruleSpec{says: t.name + "'s egress", exprs: jumpOut})
		forward = append(forward, ruleSpec{says: t.name + "'s ingress", exprs: jumpIn})
		output = append(output, ruleSpec{says: t.name + "'s ingress", exprs: jumpIn})
	}
	none := ruleSpec{says: "nothing else", exprs: drop}
	egress, forward, output = append(egress, none), append(forward, none), append(output, none)
	base := func(name string, hook nftables.ChainHook, rules []ruleSpec) chainSpec {
		return chainSpec{name: name, typ: nftables.ChainTypeFilter, hook: hook, prio: *nftables.ChainPriorityRef(0), rules: rules}
	}
	return append(chains,
		base("prerouting", *nftables.ChainHookPrerouting, egress),
		base("forward", *nftables.ChainHookForward, forward),
		base("output", *nftables.ChainHookOutput, output),
	), nil
}

// policyRules are a tap's ingress or egress rules, each written once for
// each of its peers and port ranges; what matches is accepted.
func policyRules(tap, dir string, rules []secgroup.Rule, ingress bool) ([]ruleSpec, error) {
	var out []ruleSpec
	for _, r := range rules {
		ports := r.Ports
		if len(ports) == 0 {
			ports = []secgroup.PortRange{{}}
		}
		for _, peer := range r.Peers {
			for _, pr := range ports {
				// A peer matches only its own family: ::/0 is not 0.0.0.0/0.
				var e []expr.Any
				icmp := byte(unix.IPPROTO_ICMP)
				switch {
				case peer.Addr().Is4() && ingress:
					e = slices.Concat(matchEtherType(etherIPv4), matchSaddr(peer))
				case peer.Addr().Is4():
					e = slices.Concat(matchEtherType(etherIPv4), matchDaddr(peer))
				case ingress:
					e, icmp = slices.Concat(matchEtherType(etherIPv6), matchSaddr6(peer)), unix.IPPROTO_ICMPV6
				default:
					e, icmp = slices.Concat(matchEtherType(etherIPv6), matchDaddr6(peer)), unix.IPPROTO_ICMPV6
				}
				switch r.Protocol {
				case secgroup.TCP:
					e = append(e, matchL4(unix.IPPROTO_TCP)...)
				case secgroup.UDP:
					e = append(e, matchL4(unix.IPPROTO_UDP)...)
				case secgroup.ICMP:
					e = append(e, matchL4(icmp)...)
				}
				if pr.From != 0 {
					e = append(e, matchDport(pr.From, pr.To)...)
				}
				e = append(e, &expr.Verdict{Kind: expr.VerdictAccept})
				out = append(out, ruleSpec{says: fmt.Sprintf("%s %s %s %s %d-%d", tap, dir, r.Protocol, peer, pr.From, pr.To), exprs: e})
			}
		}
	}
	return out, nil
}

// matchHopLimit matches an IPv6 packet's hop limit.
func matchHopLimit(n byte) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 7, Len: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{n}},
	}
}

func ifnameNotPrefix(key expr.MetaKey, p string) []expr.Any {
	return []expr.Any{&expr.Meta{Key: key, Register: 1}, &expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte(p)}}
}

func ctEstablishedOrRelated() []expr.Any {
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED), Xor: make([]byte, 4)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: make([]byte, 4)},
	}
}

func matchL4(proto byte) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
	}
}

// matchDport matches a destination port from through to; the protocol is
// matched before it.
func matchDport(from, to uint16) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Range{Op: expr.CmpOpEq, Register: 1, FromData: binaryutil.BigEndian.PutUint16(from), ToData: binaryutil.BigEndian.PutUint16(to)},
	}
}
