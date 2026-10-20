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
	// edgePorts are the ports public endpoints reach from the gateway.
	edgePorts []uint16
	// shares are the file share servers it connects to on their port.
	shares []netip.Addr
	// clients, of a file share's server's tap, which takes no policy, are
	// what may connect to it.
	clients []netip.Addr
}

// server says whether the tap is a file share's server's.
func (t tapPolicy) server() bool { return isServerTap(t.name) }

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
	byIface := map[string]driver.InterfacePolicy{}
	for _, p := range policies {
		byIface[p.MachineUID+"/"+p.Name] = p
	}
	var taps []tapPolicy
	size := 0
	for _, l := range links {
		m, ok := decodeTap(l.Attrs().Alias)
		if !isTap(l) || !ok {
			continue
		}
		p, ok := byIface[m.Machine+"/"+m.Name]
		switch {
		case !ok:
		case isServerTap(l.Attrs().Name):
			taps = append(taps, tapPolicy{name: l.Attrs().Name, clients: p.ShareClients})
			size += len(p.ShareClients)
		default:
			taps = append(taps, tapPolicy{name: l.Attrs().Name, policy: p.Policy, edgePorts: p.EdgePorts, shares: p.Shares})
			size += p.Policy.Size() + len(p.EdgePorts) + len(p.Shares)
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

func (d *Driver) ShareRules(_ context.Context, networkUID string) (map[string]driver.ShareRules, error) {
	if err := checkUID(networkUID); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	ns, ok, err := openNS(networkUID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("network %s has no namespace on the host", networkUID)
	}
	defer func() { _ = ns.Close() }()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	links, err := h.LinkList()
	if err != nil {
		return nil, err
	}
	ifaces := map[string]string{} // tap name to machine uid/interface name
	for _, l := range links {
		if m, ok := decodeTap(l.Attrs().Alias); ok && isTap(l) {
			ifaces[l.Attrs().Name] = m.Machine + "/" + m.Name
		}
	}
	conn, err := nftables.New(nftables.WithNetNSFd(int(ns)))
	if err != nil {
		return nil, err
	}
	chains, err := conn.ListChainsOfTableFamily(nftables.TableFamilyBridge)
	if err != nil {
		return nil, fmt.Errorf("listing nft chains: %w", err)
	}
	byChain := map[string][]*nftables.Rule{}
	for _, c := range chains {
		if c.Table.Name != FirewallTable {
			continue
		}
		rules, err := conn.GetRules(c.Table, c)
		if err != nil {
			return nil, fmt.Errorf("listing nft rules: %w", err)
		}
		byChain[c.Name] = rules
	}
	return shareRulesOf(byChain, ifaces), nil
}

// shareRulesOf reads the file share rules in force from a firewall's rules,
// by chain, of the taps named in ifaces, by machine uid and interface name:
// those in fileshare_in, by the server's tap they let in to, and those in a
// tap's out_ chain. A rule counts only if it is as the driver writes it.
func shareRulesOf(byChain map[string][]*nftables.Rule, ifaces map[string]string) map[string]driver.ShareRules {
	out := map[string]driver.ShareRules{}
	_, in, _ := shareServerChains(nil)
	for chain, rules := range byChain {
		tap, client := "", chain == in
		if !client {
			var ok bool
			if tap, ok = strings.CutPrefix(chain, "out_"); !ok {
				continue
			}
		}
		for _, r := range rules {
			name, saddr, daddr := ruleFields(r.Exprs)
			switch {
			case client && saddr.IsValid() && commentIs(r, shareClientRule(name, saddr)):
				if key, ok := ifaces[name]; ok && isServerTap(name) {
					rs := out[key]
					rs.Clients = append(rs.Clients, saddr)
					out[key] = rs
				}
			case !client && daddr.IsValid() && commentIs(r, shareServerRule(tap, daddr)):
				if key, ok := ifaces[tap]; ok {
					rs := out[key]
					rs.Servers = append(rs.Servers, daddr)
					out[key] = rs
				}
			}
		}
	}
	return out
}

// commentIs says whether r is as want writes it: its comment is want's,
// which holds a hash of want's expressions, and so is a hash of its own.
func commentIs(r *nftables.Rule, want ruleSpec) bool {
	c, err := want.comment()
	if err != nil || commentOf(r) != c {
		return false
	}
	have, err := ruleSpec{says: want.says, exprs: r.Exprs}.comment()
	return err == nil && have == c
}

// ruleFields reads what a rule matches of the interface it goes out of and
// of its IPv4 source and destination, each a whole address.
func ruleFields(exprs []expr.Any) (oif string, saddr, daddr netip.Addr) {
	field := ""
	for _, e := range exprs {
		switch e := e.(type) {
		case *expr.Meta:
			field = ""
			if e.Key == expr.MetaKeyOIFNAME {
				field = "oif"
			}
		case *expr.Payload:
			field = ""
			if e.Base == expr.PayloadBaseNetworkHeader && e.Len == 4 && e.Offset == 12 {
				field = "saddr"
			} else if e.Base == expr.PayloadBaseNetworkHeader && e.Len == 4 && e.Offset == 16 {
				field = "daddr"
			}
		case *expr.Cmp:
			switch a, ok := netip.AddrFromSlice(e.Data); {
			case field == "oif":
				oif = strings.TrimRight(string(e.Data), "\x00")
			case field == "saddr" && ok && a.Is4():
				saddr = a
			case field == "daddr" && ok && a.Is4():
				daddr = a
			}
			field = ""
		}
	}
	return oif, saddr, daddr
}

// ensureFirewallTable makes the namespace's firewall if it has none: with no
// policy yet, every tap receives and sends nothing but its network's
// services and replies, and a file share's server's nothing at all. A
// firewall there already is the agent's to keep; for a server's tap, one
// written before servers had chains of their own, which would give it what
// every tap has, is an error until the agent writes it again.
func ensureFirewallTable(ns netns.NsHandle, gateway netip.Addr, server bool) error {
	conn, err := nftables.New(nftables.WithNetNSFd(int(ns)))
	if err != nil {
		return err
	}
	tables, err := conn.ListTablesOfFamily(nftables.TableFamilyBridge)
	if err != nil {
		return fmt.Errorf("listing nft tables: %w", err)
	}
	if slices.ContainsFunc(tables, func(t *nftables.Table) bool { return t.Name == FirewallTable }) {
		if !server {
			return nil
		}
		chains, err := conn.ListChainsOfTableFamily(nftables.TableFamilyBridge)
		if err != nil {
			return fmt.Errorf("listing nft chains: %w", err)
		}
		_, in, out := shareServerChains(nil)
		for _, name := range []string{in, out} {
			if !slices.ContainsFunc(chains, func(c *nftables.Chain) bool { return c.Table.Name == FirewallTable && c.Name == name }) {
				return fmt.Errorf("the network's firewall has no chain %s for file share servers yet", name)
			}
		}
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
// File share servers' taps, known by their names, go to chains of their
// own before anything else, which every tap they have no rule for meets
// closed (shareServerChains).
func firewallChains(gateway netip.Addr, taps []tapPolicy) ([]chainSpec, error) {
	taps = slices.Clone(taps)
	slices.SortFunc(taps, func(a, b tapPolicy) int { return strings.Compare(a.name, b.name) })
	accept := []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}
	drop := []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}}
	servers, serverIn, serverOut := shareServerChains(taps)
	toServers := ruleSpec{says: "a file share's server's taps receive only what their chain lets in",
		exprs: slices.Concat(matchIfnamePrefix(expr.MetaKeyOIFNAME, serverTapPrefix), []expr.Any{&expr.Verdict{Kind: expr.VerdictJump, Chain: serverIn}})}
	fromServers := ruleSpec{says: "a file share's server's taps send only what their chain lets out",
		exprs: slices.Concat(matchIfnamePrefix(expr.MetaKeyIIFNAME, serverTapPrefix), []expr.Any{&expr.Verdict{Kind: expr.VerdictJump, Chain: serverOut}})}
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
	egress := slices.Concat([]ruleSpec{fromServers, notTap, replies, arp, ndFromTap, dad}, mld, []ruleSpec{
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
	forward := slices.Concat([]ruleSpec{toServers, notToTap, replies, arp, ndOnLink, dad}, mld)
	output := slices.Concat([]ruleSpec{toServers, notToTap, replies, arp, ndOnLink}, mld, []ruleSpec{
		{says: fmt.Sprintf("DHCP replies from the server at %s pass", gateway),
			exprs: slices.Concat(matchEtherType(etherIPv4), matchSaddr(netip.PrefixFrom(gateway, 32)), matchUDPPorts(67, 68), accept)},
		{says: fmt.Sprintf("DHCPv6 replies from the server at %s pass", driver.GatewayIPv6),
			exprs: slices.Concat(matchEtherType(etherIPv6), matchSaddr6(netip.PrefixFrom(driver.GatewayIPv6, 128)), matchUDPPorts(547, 546), accept)},
		platformSSH(gateway),
	})
	chains := servers
	for _, t := range taps {
		if t.server() {
			continue
		}
		in, out := "in_"+t.name, "out_"+t.name
		policyIn, err := policyRules(t.name, "in", t.policy.Ingress, true)
		if err != nil {
			return nil, err
		}
		inRules := slices.Concat(edgePorts(gateway, t.name, t.edgePorts), policyIn)
		outRules := toShareServers(t.name, t.shares)
		if t.policy.EgressAll {
			outRules = append(outRules, ruleSpec{says: t.name + " sends anything", exprs: accept})
		} else {
			policyOut, err := policyRules(t.name, "out", t.policy.Egress, false)
			if err != nil {
				return nil, err
			}
			outRules = append(append(outRules, policyOut...), ruleSpec{says: t.name + " sends nothing else", exprs: drop})
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

// shareServerChains are the chains of file share servers' taps, by name,
// which the base chains jump to before anything else: what is sent to them
// (in) and what they send (out). Into one: ARP, and new TCP connections to
// its port from its clients, then what follows on them; from one: ARP, and
// replies on connections made to its port. Nothing else, of a network's
// services, its groups or the platform's ways in: the binding holds what
// they send to their own addresses (docs/decisions/file-shares.md).
func shareServerChains(taps []tapPolicy) (chains []chainSpec, in, out string) {
	in, out = "fileshare_in", "fileshare_out"
	accept := &expr.Verdict{Kind: expr.VerdictAccept}
	arp := ruleSpec{says: "ARP passes", exprs: slices.Concat(matchEtherType(etherARP), []expr.Any{accept})}
	none := ruleSpec{says: "nothing else", exprs: []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}}}
	inRules := []ruleSpec{arp}
	for _, t := range taps {
		if !t.server() {
			continue
		}
		clients := slices.Clone(t.clients)
		slices.SortFunc(clients, netip.Addr.Compare)
		for _, c := range slices.Compact(clients) {
			if !c.Is4() {
				continue // its tap sends no IPv6
			}
			inRules = append(inRules, shareClientRule(t.name, c))
		}
	}
	outRules := []ruleSpec{arp,
		{says: fmt.Sprintf("replies from port %d pass", driver.ShareServerPort),
			exprs: slices.Concat(matchEtherType(etherIPv4), matchL4(unix.IPPROTO_TCP), matchSport(driver.ShareServerPort),
				ctStateIn(expr.CtStateBitESTABLISHED), ctDirection(ctReply), []expr.Any{accept})},
	}
	return []chainSpec{
		{name: in, regular: true, rules: append(inRules, none)},
		{name: out, regular: true, rules: append(outRules, none)},
	}, in, out
}

// toShareServers let a tap connect to the file share servers at addrs on
// their port, whatever its groups say: the declaration is the opening.
func toShareServers(tap string, addrs []netip.Addr) []ruleSpec {
	addrs = slices.Clone(addrs)
	slices.SortFunc(addrs, netip.Addr.Compare)
	var out []ruleSpec
	for _, a := range slices.Compact(addrs) {
		if !a.Is4() {
			continue
		}
		out = append(out, shareServerRule(tap, a))
	}
	return out
}

// shareClientRule lets client connect to the server's tap on its port.
func shareClientRule(tap string, client netip.Addr) ruleSpec {
	return ruleSpec{says: fmt.Sprintf("%s: %s connects to port %d", tap, client, driver.ShareServerPort),
		exprs: slices.Concat(matchIfname(expr.MetaKeyOIFNAME, tap), matchEtherType(etherIPv4), matchSaddr(netip.PrefixFrom(client, 32)),
			matchL4(unix.IPPROTO_TCP), matchDport(driver.ShareServerPort, driver.ShareServerPort),
			ctStateIn(expr.CtStateBitNEW|expr.CtStateBitESTABLISHED), ctDirection(ctOriginal), []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}})}
}

// shareServerRule lets a tap connect to the file share server at addr on
// its port.
func shareServerRule(tap string, addr netip.Addr) ruleSpec {
	return ruleSpec{says: fmt.Sprintf("%s connects to the file share at %s", tap, addr),
		exprs: slices.Concat(matchEtherType(etherIPv4), matchDaddr(netip.PrefixFrom(addr, 32)), matchL4(unix.IPPROTO_TCP),
			matchDport(driver.ShareServerPort, driver.ShareServerPort), []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}})}
}

// A connection's directions, as conntrack's direction key gives them.
const (
	ctOriginal byte = 0
	ctReply    byte = 1
)

// ctStateIn matches a connection in any of the states bits.
func ctStateIn(bits uint32) []expr.Any {
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: binaryutil.NativeEndian.PutUint32(bits), Xor: make([]byte, 4)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: make([]byte, 4)},
	}
}

// ctDirection matches a packet going the connection's way d.
func ctDirection(d byte) []expr.Any {
	return []expr.Any{&expr.Ct{Register: 1, Key: expr.CtKeyDIRECTION}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{d}}}
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

// edgePorts let the edge reach a tap's ports of public endpoints from the
// gateway, whatever its security groups say, while the endpoints exist;
// the edge checks who may (docs/decisions/public-endpoints.md). Only the
// namespace's own sockets send from the gateway.
func edgePorts(gateway netip.Addr, tap string, ports []uint16) []ruleSpec {
	ports = slices.Clone(ports)
	slices.Sort(ports)
	ports = slices.Compact(ports)
	out := make([]ruleSpec, 0, len(ports))
	for _, p := range ports {
		out = append(out, ruleSpec{says: fmt.Sprintf("%s: public endpoints from %s to port %d pass", tap, gateway, p),
			exprs: slices.Concat(matchEtherType(etherIPv4), matchSaddr(netip.PrefixFrom(gateway, 32)), matchL4(unix.IPPROTO_TCP), matchDport(p, p),
				[]expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}})})
	}
	return out
}

// platformSSH lets the platform's SSH reach a tap's port 22 from the
// gateway, whatever its security groups say; RBAC gates it (vm-ssh.md).
// Only the namespace's own sockets send from the gateway.
func platformSSH(gateway netip.Addr) ruleSpec {
	return ruleSpec{says: fmt.Sprintf("the platform's SSH from %s to port 22 passes", gateway),
		exprs: slices.Concat(matchEtherType(etherIPv4), matchSaddr(netip.PrefixFrom(gateway, 32)), matchL4(unix.IPPROTO_TCP), matchDport(22, 22),
			[]expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}})}
}
