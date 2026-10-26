// Package secgroup is what SecurityGroups mean: their rules as the
// apiserver admits them, and what each interface of a network may receive
// and send, as the node enforces it (docs/decisions/networking.md). Both
// sides use it, so what is admitted is what is enforced.
package secgroup

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
)

// Default is every network's implicit group, which is no object: it lets
// in its own members, and lets everything out.
const Default = "default"

// Bounds on what a group and an interface may hold.
const (
	MaxRules              = 64
	MaxGroupsPerInterface = 5
	maxPortRanges         = 16
)

// Protocol is what a rule matches.
type Protocol int

const (
	All Protocol = iota + 1
	TCP
	UDP
	// ICMP is ICMP for an IPv4 peer and ICMPv6 for an IPv6 one.
	ICMP
)

func (p Protocol) String() string {
	return [...]string{"", "all", "tcp", "udp", "icmp"}[p]
}

// PortRange is From through To, both included.
type PortRange struct{ From, To uint16 }

// Rule is one allow rule. Peers are the ranges it allows, resolved from a
// CIDR, a group's members or a network's transit address; a rule of a group
// with no members, or of a network not known, allows nothing.
type Rule struct {
	Protocol Protocol
	// Ports is empty for every port, and always for All and ICMP.
	Ports []PortRange
	Peers []netip.Prefix
}

// Group is a SecurityGroup's rules as admitted, peers not yet resolved.
type Group struct {
	Ingress, Egress []RuleSpec
}

// RuleSpec is a rule whose peer is a CIDR, a group's name or another
// network's name.
type RuleSpec struct {
	Protocol  Protocol
	Ports     []PortRange
	PeerCIDR  netip.Prefix
	PeerGroup string
	// PeerNetwork is what comes through another network's NetworkEndpoints,
	// from that network's transit address (docs/decisions/endpoint-services.md).
	// Only ingress rules have it.
	PeerNetwork string
}

// FromSpec reads a group's spec, refusing what is not a valid one.
func FromSpec(spec *networkv1alpha1.SecurityGroupSpec) (Group, error) {
	if n := len(spec.GetIngress()) + len(spec.GetEgress()); n > MaxRules {
		return Group{}, fmt.Errorf("%d rules, at most %d", n, MaxRules)
	}
	var g Group
	for i, r := range spec.GetIngress() {
		rs, err := ruleSpec(r.GetProtocol(), r.GetPorts(), r.GetFrom())
		if err != nil {
			return Group{}, fmt.Errorf("ingress[%d]: %w", i, err)
		}
		g.Ingress = append(g.Ingress, rs)
	}
	for i, r := range spec.GetEgress() {
		rs, err := ruleSpec(r.GetProtocol(), r.GetPorts(), r.GetTo())
		if err != nil {
			return Group{}, fmt.Errorf("egress[%d]: %w", i, err)
		}
		// Nothing a member sends goes to another network's transit address:
		// a network endpoint is reached in one direction only.
		if rs.PeerNetwork != "" {
			return Group{}, fmt.Errorf("egress[%d]: only ingress rules take a network peer", i)
		}
		g.Egress = append(g.Egress, rs)
	}
	return g, nil
}

func ruleSpec(p networkv1alpha1.SecurityGroupProtocol, ports string, peer *networkv1alpha1.SecurityGroupPeer) (RuleSpec, error) {
	var rs RuleSpec
	switch p {
	case networkv1alpha1.SecurityGroupProtocol_SECURITY_GROUP_PROTOCOL_ALL:
		rs.Protocol = All
	case networkv1alpha1.SecurityGroupProtocol_SECURITY_GROUP_PROTOCOL_TCP:
		rs.Protocol = TCP
	case networkv1alpha1.SecurityGroupProtocol_SECURITY_GROUP_PROTOCOL_UDP:
		rs.Protocol = UDP
	case networkv1alpha1.SecurityGroupProtocol_SECURITY_GROUP_PROTOCOL_ICMP:
		rs.Protocol = ICMP
	default:
		return RuleSpec{}, fmt.Errorf("protocol is all, tcp, udp or icmp")
	}
	if ports != "" {
		if rs.Protocol != TCP && rs.Protocol != UDP {
			return RuleSpec{}, fmt.Errorf("only tcp and udp rules take ports")
		}
		var err error
		if rs.Ports, err = ParsePorts(ports); err != nil {
			return RuleSpec{}, err
		}
	}
	switch v := peer.GetPeer().(type) {
	case *networkv1alpha1.SecurityGroupPeer_Cidr:
		p, err := netip.ParsePrefix(v.Cidr)
		if err != nil || p.Addr().Is4In6() || p.Addr().Zone() != "" {
			return RuleSpec{}, fmt.Errorf("cidr %q is not an IPv4 or IPv6 range such as 10.0.0.0/24 or ::/0", v.Cidr)
		}
		if p != p.Masked() {
			return RuleSpec{}, fmt.Errorf("cidr %q has host bits set; did you mean %s?", v.Cidr, p.Masked())
		}
		rs.PeerCIDR = p
	case *networkv1alpha1.SecurityGroupPeer_Group:
		if v.Group == "" {
			return RuleSpec{}, fmt.Errorf("a group peer needs a group's name")
		}
		rs.PeerGroup = v.Group
	case *networkv1alpha1.SecurityGroupPeer_Network:
		if v.Network == "" {
			return RuleSpec{}, fmt.Errorf("a network peer needs a network's name")
		}
		rs.PeerNetwork = v.Network
	default:
		return RuleSpec{}, fmt.Errorf("a rule needs a peer: a cidr, a group or a network")
	}
	return rs, nil
}

// ParsePorts reads "443", "8000-8080" and comma lists of them.
func ParsePorts(s string) ([]PortRange, error) {
	var out []PortRange
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		from, to, isRange := strings.Cut(part, "-")
		a, err1 := port(from)
		b := a
		var err2 error
		if isRange {
			b, err2 = port(to)
		}
		if err1 != nil || err2 != nil || b < a {
			return nil, fmt.Errorf("ports %q: %q is not a port 1-65535 or a range of them", s, part)
		}
		out = append(out, PortRange{a, b})
	}
	if len(out) > maxPortRanges {
		return nil, fmt.Errorf("ports %q: at most %d ports or ranges", s, maxPortRanges)
	}
	return out, nil
}

func port(s string) (uint16, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("bad port")
	}
	return uint16(n), nil
}

// Member is one interface on the network: its addresses, IPv4 and any
// IPv6, and its groups.
type Member struct {
	Addrs  []netip.Addr
	Groups []string
	// Isolated makes the interface a member of no group, Default
	// included, whatever Groups says: no group's rule names it as a peer,
	// and its policy allows nothing in or out. What the host's firewall lets
	// every tap reach whatever its policy (its network's DHCP, DNS and
	// metadata, the gateway's SSH, endpoint ports) still applies. A file
	// share's server is (docs/decisions/file-shares.md).
	Isolated bool
}

// Policy is what one interface may receive and send. Replies to what is
// allowed pass either way; that is the enforcer's.
type Policy struct {
	Ingress []Rule
	// EgressAll lets the interface send anything, and Egress is then
	// empty.
	EgressAll bool
	Egress    []Rule
}

// Size is how many single rules p is, each rule counted once for each of its
// peers and port ranges: what a firewall holding it writes.
func (p Policy) Size() int {
	n := 0
	for _, r := range slices.Concat(p.Ingress, p.Egress) {
		n += len(r.Peers) * max(1, len(r.Ports))
	}
	return n
}

// Resolve gives each member of a network its policy, from the network's
// groups by name, and transits, the transit address each other network of
// the project comes from through a NetworkEndpoint, by name. A member naming
// no group is in Default, unless it is Isolated. A group a member names that
// does not exist lets in nothing and lets out nothing, so a member is never
// more open for a group missing; a rule naming a group that does not exist,
// or has no members, or a network not in transits, allows nothing.
func Resolve(groups map[string]Group, members []Member, transits map[string]netip.Addr) []Policy {
	inGroup := map[string][]netip.Prefix{}
	for _, m := range members {
		for _, g := range groupsOf(m) {
			for _, a := range m.Addrs {
				inGroup[g] = append(inGroup[g], netip.PrefixFrom(a, a.BitLen()))
			}
		}
	}
	resolve := func(rs RuleSpec) Rule {
		r := Rule{Protocol: rs.Protocol, Ports: rs.Ports}
		_, exists := groups[rs.PeerGroup]
		switch {
		case rs.PeerCIDR.IsValid():
			r.Peers = []netip.Prefix{rs.PeerCIDR}
		case rs.PeerNetwork != "":
			if a, ok := transits[rs.PeerNetwork]; ok {
				r.Peers = []netip.Prefix{netip.PrefixFrom(a, a.BitLen())}
			}
		case rs.PeerGroup == Default || exists:
			r.Peers = slices.Clone(inGroup[rs.PeerGroup])
		default:
			// A group that is not there has no members, whatever
			// interfaces name it.
		}
		return r
	}
	out := make([]Policy, len(members))
	for i, m := range members {
		p := Policy{EgressAll: !m.Isolated}
		for _, name := range groupsOf(m) {
			if name == Default {
				p.Ingress = append(p.Ingress, Rule{Protocol: All, Peers: slices.Clone(inGroup[Default])})
				continue
			}
			g, ok := groups[name]
			if !ok || len(g.Egress) > 0 {
				p.EgressAll = false
			}
			for _, rs := range g.Ingress {
				p.Ingress = append(p.Ingress, resolve(rs))
			}
			for _, rs := range g.Egress {
				p.Egress = append(p.Egress, resolve(rs))
			}
		}
		if p.EgressAll {
			p.Egress = nil
		}
		out[i] = p
	}
	return out
}

func groupsOf(m Member) []string {
	if m.Isolated {
		return nil
	}
	if len(m.Groups) == 0 {
		return []string{Default}
	}
	return m.Groups
}
