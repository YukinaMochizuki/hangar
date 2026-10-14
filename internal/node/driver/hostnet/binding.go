package hostnet

import (
	"fmt"
	"net"
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
)

// boundTap is a tap with the only MAC and addresses it may send as: its
// IPv4 address and, if it has one, its IPv6 address.
type boundTap struct {
	name string
	mac  net.HardwareAddr
	addr netip.Addr
	ipv6 netip.Addr
}

var (
	etherIPv4 = []byte{0x08, 0x00}
	etherARP  = []byte{0x08, 0x06}
	etherIPv6 = []byte{0x86, 0xdd}
)

// bindingChains is a network namespace's bridge table: each tap sends only
// as its own MAC and addresses, ARP and neighbour discovery only for them,
// no router advertisement or DHCPv6 server's message, and nothing but IPv4,
// ARP and, with an IPv6 address, IPv6; a tap without rules of its own sends
// nothing. docs/decisions/networking.md lists the rules.
func bindingChains(taps []boundTap) []chainSpec {
	taps = slices.Clone(taps)
	slices.SortFunc(taps, func(a, b boundTap) int { return strings.Compare(a.name, b.name) })
	var rules []ruleSpec
	for _, t := range taps {
		in := matchIfname(expr.MetaKeyIIFNAME, t.name)
		drop := []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}}
		rules = append(rules,
			ruleSpec{
				says:  fmt.Sprintf("%s sends only as %s", t.name, t.mac),
				exprs: slices.Concat(in, etherSaddrNot(t.mac), drop),
			},
			ruleSpec{
				says: fmt.Sprintf("%s asks DHCP from no address", t.name),
				exprs: slices.Concat(in, matchEtherType(etherIPv4),
					matchSaddr(netip.MustParsePrefix("0.0.0.0/32")), matchDaddr(netip.MustParsePrefix("255.255.255.255/32")),
					matchUDPPorts(68, 67), []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}),
			},
			ruleSpec{
				says:  fmt.Sprintf("%s sends IPv4 only from %s", t.name, t.addr),
				exprs: slices.Concat(in, matchEtherType(etherIPv4), matchSaddrNot(netip.PrefixFrom(t.addr, 32)), drop),
			},
			ruleSpec{
				says:  fmt.Sprintf("%s's ARP says only %s", t.name, t.mac),
				exprs: slices.Concat(in, matchEtherType(etherARP), networkBytesNot(8, t.mac), drop),
			},
			ruleSpec{
				says:  fmt.Sprintf("%s's ARP says only %s", t.name, t.addr),
				exprs: slices.Concat(in, matchEtherType(etherARP), networkBytesNot(14, t.addr.AsSlice()), drop),
			},
		)
		if t.ipv6.IsValid() {
			rules = append(rules, ipv6Binding(t, in)...)
		}
		only := []expr.Any{
			&expr.Meta{Key: expr.MetaKeyPROTOCOL, Register: 1},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: etherIPv4},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: etherARP},
		}
		says := fmt.Sprintf("%s sends only IPv4 and ARP", t.name)
		if t.ipv6.IsValid() {
			only = append(only, &expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: etherIPv6})
			says = fmt.Sprintf("%s sends only IPv4, IPv6 and ARP", t.name)
		}
		rules = append(rules,
			ruleSpec{says: says, exprs: slices.Concat(in, only, drop)},
			ruleSpec{
				says:  fmt.Sprintf("%s as bound", t.name),
				exprs: slices.Concat(in, []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}),
			},
		)
	}
	rules = append(rules, ruleSpec{
		says:  "a tap not bound sends nothing",
		exprs: slices.Concat(matchIfnamePrefix(expr.MetaKeyIIFNAME, tapPrefix), []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}}),
	})
	return []chainSpec{{
		name: "prerouting", typ: nftables.ChainTypeFilter, hook: *nftables.ChainHookPrerouting,
		prio: *nftables.ChainPriorityRef(-200), rules: rules,
	}}
}

// ipv6Binding are the rules binding a tap's IPv6, after its MAC is: it
// sends from its address or a link-local one, or from none only as
// duplicate address detection and listener reports do; it advertises
// only its own addresses, never the gateway's, and only its own MAC for
// them; and it sends neither router advertisements, redirects, nor what
// only a DHCPv6 server sends. A tap may use any link-local address, as
// guests choose their own.
func ipv6Binding(t boundTap, in []expr.Any) []ruleSpec {
	drop := []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}}
	v6 := matchEtherType(etherIPv6)
	unspecified := netip.MustParsePrefix("::/128")
	icmpType := []expr.Any{&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 1}}
	return []ruleSpec{
		{
			says: fmt.Sprintf("%s sends IPv6 only from %s or link-local", t.name, t.ipv6),
			exprs: slices.Concat(in, v6, matchSaddr6Not(netip.PrefixFrom(t.ipv6, 128)), matchSaddr6Not(linkLocal),
				matchSaddr6Not(unspecified), drop),
		},
		{
			says: fmt.Sprintf("%s sends from no address only ICMPv6", t.name),
			exprs: slices.Concat(in, v6, matchSaddr6(unspecified), []expr.Any{
				&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
				&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{unix.IPPROTO_ICMPV6}},
			}, drop),
		},
		{
			says: fmt.Sprintf("%s sends from no address only neighbour solicitations and listener reports", t.name),
			exprs: slices.Concat(in, v6, matchSaddr6(unspecified), matchL4(unix.IPPROTO_ICMPV6), icmpType, []expr.Any{
				&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{135}},
				&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{143}},
			}, drop),
		},
		{
			says:  fmt.Sprintf("%s sends no router advertisements", t.name),
			exprs: slices.Concat(in, v6, matchICMPv6Type(134), drop),
		},
		{
			says:  fmt.Sprintf("%s sends no redirects or router renumbering", t.name),
			exprs: slices.Concat(in, v6, matchICMPv6Types(137, 138), drop),
		},
		{
			says:  fmt.Sprintf("%s serves no DHCPv6", t.name),
			exprs: slices.Concat(in, v6, matchL4(unix.IPPROTO_UDP), matchSport(547), drop),
		},
		{
			says:  fmt.Sprintf("%s sends nothing to DHCPv6 clients", t.name),
			exprs: slices.Concat(in, v6, matchL4(unix.IPPROTO_UDP), matchDport(546, 546), drop),
		},
		{
			says: fmt.Sprintf("%s advertises only %s and link-local addresses", t.name, t.ipv6),
			exprs: slices.Concat(in, v6, matchICMPv6Type(136), transportAddr6Is(8, netip.PrefixFrom(t.ipv6, 128), expr.CmpOpNeq),
				transportAddr6Is(8, linkLocal, expr.CmpOpNeq), drop),
		},
		{
			says: fmt.Sprintf("%s does not advertise the gateway", t.name),
			exprs: slices.Concat(in, v6, matchICMPv6Type(136), transportAddr6Is(8, netip.PrefixFrom(driver.GatewayIPv6, 128), expr.CmpOpEq),
				drop),
		},
		// A solicitation from an address, and an advertisement, carry at
		// most one option, their link-layer address, and only the tap's
		// MAC. The kernel takes the first such option wherever it is, past
		// any other, so one option alone is what can be checked; a
		// solicitation from no address carries none (RFC 4861), and the
		// kernel drops one that does.
		{
			says: fmt.Sprintf("%s's neighbour solicitations carry at most their link-layer address", t.name),
			exprs: slices.Concat(in, v6, matchSaddr6Not(unspecified), matchICMPv6Type(135),
				ipv6LengthNot(24, 32), drop),
		},
		{
			says: fmt.Sprintf("%s's neighbour solicitations say only %s", t.name, t.mac),
			exprs: slices.Concat(in, v6, matchSaddr6Not(unspecified), matchICMPv6Type(135),
				ipv6LengthIs(32), ndOptionNot(1, t.mac), drop),
		},
		{
			says:  fmt.Sprintf("%s's neighbour advertisements carry at most their link-layer address", t.name),
			exprs: slices.Concat(in, v6, matchICMPv6Type(136), ipv6LengthNot(24, 32), drop),
		},
		{
			says:  fmt.Sprintf("%s's neighbour advertisements say only %s", t.name, t.mac),
			exprs: slices.Concat(in, v6, matchICMPv6Type(136), ipv6LengthIs(32), ndOptionNot(2, t.mac), drop),
		},
	}
}

// ipv6LengthIs matches an IPv6 payload of n bytes: an ND message of 24
// and one 8-byte option, with no extension header.
func ipv6LengthIs(n uint16) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 4, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(n)},
	}
}

// ipv6LengthNot matches an IPv6 payload of neither length.
func ipv6LengthNot(a, b uint16) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 4, Len: 2},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: binaryutil.BigEndian.PutUint16(a)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: binaryutil.BigEndian.PutUint16(b)},
	}
}

// transportAddr6Is matches an IPv6 address at offset of the transport
// header, such as a neighbour advertisement's target, in p, with op.
func transportAddr6Is(offset uint32, p netip.Prefix, op expr.CmpOp) []expr.Any {
	a := p.Masked().Addr().As16()
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: offset, Len: 16},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 16, Mask: net.CIDRMask(p.Bits(), 128), Xor: make([]byte, 16)},
		&expr.Cmp{Op: op, Register: 1, Data: a[:]},
	}
}

// ndOptionNot matches a neighbour solicitation or advertisement whose one
// option is not a link-layer address option of type opt (1, source, or 2,
// target) carrying mac.
func ndOptionNot(opt byte, mac net.HardwareAddr) []expr.Any {
	want := append([]byte{opt, 1}, mac...)
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 24, Len: 8},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: want},
	}
}

// matchSport matches a source port; the protocol is matched before it.
func matchSport(port uint16) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(port)},
	}
}

func matchEtherType(t []byte) []expr.Any {
	return []expr.Any{&expr.Meta{Key: expr.MetaKeyPROTOCOL, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: t}}
}

func etherSaddrNot(mac net.HardwareAddr) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseLLHeader, Offset: 6, Len: 6},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: mac},
	}
}

// networkBytesNot matches a network header whose bytes at offset are not b.
func networkBytesNot(offset uint32, b []byte) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: offset, Len: uint32(len(b))},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: b},
	}
}

// ensureBinding makes the namespace's bridge table bind each of its taps,
// as their aliases say they were ensured; IPv6 addresses outside the host's
// prefix, as with none, are not taken.
func ensureBinding(ns netns.NsHandle, h *netlink.Handle, prefix netip.Prefix) error {
	links, err := h.LinkList()
	if err != nil {
		return err
	}
	var bound []boundTap
	for _, l := range links {
		m, ok := decodeTap(l.Attrs().Alias)
		if !isTap(l) || !ok {
			continue
		}
		mac, err := net.ParseMAC(m.MAC)
		if err != nil || len(mac) != 6 || !m.Address.Addr().Is4() {
			continue // without a MAC and address to send as, it sends nothing
		}
		t := boundTap{name: l.Attrs().Name, mac: mac, addr: m.Address.Addr()}
		if prefix.Contains(m.IPv6) {
			t.ipv6 = m.IPv6
		}
		bound = append(bound, t)
	}
	conn, err := nftables.New(nftables.WithNetNSFd(int(ns)))
	if err != nil {
		return err
	}
	if err := ensureTable(conn, nftables.TableFamilyBridge, bindingChains(bound)); err != nil {
		return fmt.Errorf("binding taps: %w", err)
	}
	return nil
}
