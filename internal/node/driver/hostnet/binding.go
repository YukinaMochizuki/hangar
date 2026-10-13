package hostnet

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// boundTap is a tap with the only MAC and address it may send as.
type boundTap struct {
	name string
	mac  net.HardwareAddr
	addr netip.Addr
}

var (
	etherIPv4 = []byte{0x08, 0x00}
	etherARP  = []byte{0x08, 0x06}
)

// bindingChains is a network namespace's bridge table: each tap sends only
// as its own MAC and address, and ARP only for them, and nothing but IPv4
// and ARP; a tap without rules of its own sends nothing.
// docs/decisions/networking.md lists the rules.
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
			ruleSpec{
				says: fmt.Sprintf("%s sends only IPv4 and ARP", t.name),
				exprs: slices.Concat(in, []expr.Any{
					&expr.Meta{Key: expr.MetaKeyPROTOCOL, Register: 1},
					&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: etherIPv4},
					&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: etherARP},
				}, drop),
			},
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
// as their aliases say they were ensured.
func ensureBinding(ns netns.NsHandle, h *netlink.Handle) error {
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
		bound = append(bound, boundTap{name: l.Attrs().Name, mac: mac, addr: m.Address.Addr()})
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
