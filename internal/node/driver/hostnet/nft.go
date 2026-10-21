package hostnet

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/google/nftables/userdata"
	"golang.org/x/sys/unix"
)

// chainSpec is a base chain of the driver's table as it should be.
type chainSpec struct {
	name  string
	typ   nftables.ChainType
	hook  nftables.ChainHook
	prio  nftables.ChainPriority
	rules []ruleSpec
	// regular is a chain on no hook, which only jumps reach; it has no
	// type, hook or priority.
	regular bool
}

// ruleSpec is a rule and what its comment says of it; the comment written
// adds a hash of the expressions, by which a rule is known to be as wanted.
type ruleSpec struct {
	says  string
	exprs []expr.Any
}

// comment is the rule's comment as written: what it says and the hash.
func (r ruleSpec) comment() (string, error) {
	h := sha256.New()
	for _, e := range r.exprs {
		b, err := expr.Marshal(byte(nftables.TableFamilyIPv4), e)
		if err != nil {
			return "", err
		}
		h.Write(b)
	}
	return fmt.Sprintf("hangar: %s [%s]", r.says, hex.EncodeToString(h.Sum(nil))[:16]), nil
}

// frontDoorPorts are the host's own TCP ports a network may reach, at the
// host's public addresses: the front door's, which anyone on the internet
// reaches there too (docs/decisions/front-door.md). A machine calling the
// platform by its public names gets to the host this way.
var frontDoorPorts = []uint16{80, 443}

// frontDoorRules accept new connections from a network to the front door's
// ports at addr, matched by daddr; none without an address.
func frontDoorRules(addr netip.Addr, daddr func(netip.Prefix) []expr.Any) []ruleSpec {
	if !addr.IsValid() {
		return nil
	}
	var out []ruleSpec
	for _, p := range frontDoorPorts {
		out = append(out, ruleSpec{
			says: fmt.Sprintf("TCP %d at %s from a network, the front door", p, addr),
			exprs: slices.Concat(matchIfnamePrefix(expr.MetaKeyIIFNAME, vethPrefix), daddr(netip.PrefixFrom(addr, addr.BitLen())),
				matchL4(unix.IPPROTO_TCP), matchDport(p, p), []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}),
		})
	}
	return out
}

// mailPorts are the TCP ports no network may connect to: SMTP and SMTP over
// TLS, the ports spam is sent from. 587, submission, needs an account at the
// server, so it stays open (docs/decisions/networking.md, "Abuse
// protection").
var mailPorts = []uint16{25, 465}

// abuseRules come first in a forward chain of either family: from a
// network, no TCP to the mail ports, and nothing conntrack finds invalid,
// which no connection, rate limit or NAT log accounts for.
func abuseRules() []ruleSpec {
	fromNet := matchIfnamePrefix(expr.MetaKeyIIFNAME, vethPrefix)
	drop := []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}}
	var out []ruleSpec
	for _, p := range mailPorts {
		out = append(out, ruleSpec{
			says:  fmt.Sprintf("no TCP %d from a network", p),
			exprs: slices.Concat(fromNet, matchL4(unix.IPPROTO_TCP), matchDport(p, p), drop),
		})
	}
	return append(out, ruleSpec{
		says:  "nothing invalid from a network",
		exprs: slices.Concat(fromNet, matchCtState(expr.CtStateBitINVALID), drop),
	})
}

// natMarkTag, in a conntrack mark, says the connection came from a
// network; the other bits hold its veth's index. Indexes are not reused
// while the host runs, so a network made again on another's transit link
// is told apart (FollowNAT).
const natMarkTag = 1 << 31

// markNetworkRule marks each new connection from a network, last in the
// IPv4 forward chain, so only those that leave are marked.
func markNetworkRule() ruleSpec {
	return ruleSpec{
		says: "a network's new connections are marked with its veth",
		exprs: slices.Concat(matchIfnamePrefix(expr.MetaKeyIIFNAME, vethPrefix), matchCtState(expr.CtStateBitNEW), []expr.Any{
			&expr.Meta{Key: expr.MetaKeyIIF, Register: 1},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
				Mask: binaryutil.NativeEndian.PutUint32(^uint32(natMarkTag)), Xor: binaryutil.NativeEndian.PutUint32(natMarkTag)},
			&expr.Ct{Key: expr.CtKeyMARK, Register: 1, SourceRegister: true},
		}),
	}
}

// rootChains is the root namespace's table, whose size does not depend on
// how many networks there are; docs/decisions/networking.md lists its rules.
func rootChains(uplink string, public netip.Addr) []chainSpec {
	transit := netip.MustParsePrefix(transitRange)
	nat := ruleSpec{
		says:  fmt.Sprintf("%s out of %s masquerades", transit, uplink),
		exprs: append(matchSaddr(transit), append(matchIfname(expr.MetaKeyOIFNAME, uplink), &expr.Masq{})...),
	}
	if public.IsValid() {
		nat = ruleSpec{
			says:  fmt.Sprintf("%s out of %s as %s", transit, uplink, public),
			exprs: append(matchSaddr(transit), append(matchIfname(expr.MetaKeyOIFNAME, uplink), snat(public)...)...),
		}
	}
	return []chainSpec{
		{
			name: "prerouting", typ: nftables.ChainTypeFilter, hook: *nftables.ChainHookPrerouting, prio: *nftables.ChainPriorityRaw,
			rules: []ruleSpec{{
				says:  "a network sends only from addresses routed back to it",
				exprs: append(matchIfnamePrefix(expr.MetaKeyIIFNAME, vethPrefix), dropUnlessRoutedBack()...),
			}},
		},
		{
			name: "postrouting", typ: nftables.ChainTypeNAT, hook: *nftables.ChainHookPostrouting, prio: *nftables.ChainPriorityNATSource,
			rules: []ruleSpec{nat},
		},
		{
			name: "forward", typ: nftables.ChainTypeFilter, hook: *nftables.ChainHookForward, prio: *nftables.ChainPriorityFilter,
			rules: append(abuseRules(), ruleSpec{
				says:  "only replies into a network",
				exprs: append(matchIfnamePrefix(expr.MetaKeyOIFNAME, vethPrefix), dropUnlessReply()...),
			}, ruleSpec{
				// The rule above lets no connection into a network that a reply
				// from it could leave by another link.
				says: fmt.Sprintf("a network reaches only %s", uplink),
				exprs: append(matchIfnamePrefix(expr.MetaKeyIIFNAME, vethPrefix),
					append(matchIfnameNot(expr.MetaKeyOIFNAME, uplink), &expr.Verdict{Kind: expr.VerdictDrop})...),
			}, markNetworkRule()),
		},
		{
			name: "input", typ: nftables.ChainTypeFilter, hook: *nftables.ChainHookInput, prio: *nftables.ChainPriorityFilter,
			rules: append(frontDoorRules(public, matchDaddr), ruleSpec{
				says:  "only replies from a network into the host",
				exprs: append(matchIfnamePrefix(expr.MetaKeyIIFNAME, vethPrefix), dropUnlessReply()...),
			}),
		},
	}
}

// nsChains is a network namespace's table: only its range let in from the
// bridge, before conntrack sees it, and the range SNATed to its end of the
// transit link.
func nsChains(cidr netip.Prefix, nsTransit netip.Addr) []chainSpec {
	return []chainSpec{{
		name: "prerouting", typ: nftables.ChainTypeFilter, hook: *nftables.ChainHookPrerouting, prio: *nftables.ChainPriorityRaw,
		rules: []ruleSpec{{
			// A guest asks DHCP for its address before it has one, by
			// broadcast, which is never routed on: the packet goes no
			// further than the namespace's DHCP server.
			says: fmt.Sprintf("DHCP from no address on %s", nsBridge),
			exprs: slices.Concat(matchIfname(expr.MetaKeyIIFNAME, nsBridge),
				matchSaddr(netip.MustParsePrefix("0.0.0.0/32")), matchDaddr(netip.MustParsePrefix("255.255.255.255/32")),
				matchUDPPorts(68, 67), []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}),
		}, {
			says: fmt.Sprintf("only %s from %s", cidr, nsBridge),
			exprs: append(matchIfname(expr.MetaKeyIIFNAME, nsBridge),
				append(matchSaddrNot(cidr), &expr.Verdict{Kind: expr.VerdictDrop})...),
		}, {
			// The bridge passes frames for the 802.1D group addresses up a
			// tap's own stack, past its own hooks and so the binding.
			says:  "nothing in by a tap's own stack",
			exprs: append(matchIfnamePrefix(expr.MetaKeyIIFNAME, tapPrefix), &expr.Verdict{Kind: expr.VerdictDrop}),
		}},
	}, {
		name: "postrouting", typ: nftables.ChainTypeNAT, hook: *nftables.ChainHookPostrouting, prio: *nftables.ChainPriorityNATSource,
		rules: []ruleSpec{{
			says:  fmt.Sprintf("%s out of %s as %s", cidr, nsUplink, nsTransit),
			exprs: append(matchSaddr(cidr), append(matchIfname(expr.MetaKeyOIFNAME, nsUplink), snat(nsTransit)...)...),
		}},
	}}
}

func matchSaddr(p netip.Prefix) []expr.Any { return saddrIs(p, expr.CmpOpEq) }

// matchDaddr matches a destination in p.
func matchDaddr(p netip.Prefix) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: net.CIDRMask(p.Bits(), 32), Xor: make([]byte, 4)},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: p.Addr().AsSlice()},
	}
}

// matchUDPPorts matches UDP from port src to port dst.
func matchUDPPorts(src, dst uint16) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_UDP}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(src)},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(dst)},
	}
}

func matchSaddrNot(p netip.Prefix) []expr.Any { return saddrIs(p, expr.CmpOpNeq) }

func saddrIs(p netip.Prefix, op expr.CmpOp) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: net.CIDRMask(p.Bits(), 32), Xor: make([]byte, 4)},
		&expr.Cmp{Op: op, Register: 1, Data: p.Addr().AsSlice()},
	}
}

// matchIfname matches a whole interface name, NUL-padded as the kernel
// keeps it.
func matchIfname(key expr.MetaKey, name string) []expr.Any { return ifnameIs(key, name, expr.CmpOpEq) }

func matchIfnameNot(key expr.MetaKey, name string) []expr.Any {
	return ifnameIs(key, name, expr.CmpOpNeq)
}

func ifnameIs(key expr.MetaKey, name string, op expr.CmpOp) []expr.Any {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, name)
	return []expr.Any{&expr.Meta{Key: key, Register: 1}, &expr.Cmp{Op: op, Register: 1, Data: b}}
}

// matchIfnamePrefix matches interface names starting with p, as nft's
// "name*" does.
func matchIfnamePrefix(key expr.MetaKey, p string) []expr.Any {
	return []expr.Any{&expr.Meta{Key: key, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte(p)}}
}

// dropUnlessRoutedBack is nft's "fib saddr . iif oif missing drop": strict
// reverse-path filtering.
func dropUnlessRoutedBack() []expr.Any {
	return []expr.Any{
		&expr.Fib{Register: 1, FlagSADDR: true, FlagIIF: true, ResultOIF: true},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: make([]byte, 4)},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}
}

func dropUnlessReply() []expr.Any {
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{
			SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED), Xor: make([]byte, 4),
		},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: make([]byte, 4)},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}
}

func snat(to netip.Addr) []expr.Any {
	return []expr.Any{
		&expr.Immediate{Register: 1, Data: to.AsSlice()},
		&expr.NAT{Type: expr.NATTypeSourceNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1},
	}
}

func commentOf(r *nftables.Rule) string {
	c, _ := userdata.GetString(r.UserData, userdata.TypeComment)
	return c
}

// ensureTable makes the driver's table on conn hold exactly want, replacing
// it in one transaction if it differs, and touching nothing if it does not.
func ensureTable(conn *nftables.Conn, family nftables.TableFamily, want []chainSpec) error {
	return ensureNamedTable(conn, family, Table, want)
}

// ensureNamedTable is ensureTable for the driver's table called name.
func ensureNamedTable(conn *nftables.Conn, family nftables.TableFamily, name string, want []chainSpec) error {
	same, exists, err := tableIs(conn, family, name, want)
	if err != nil || same {
		return err
	}
	t := &nftables.Table{Family: family, Name: name}
	if exists {
		conn.DelTable(t)
	}
	conn.AddTable(t)
	accept := nftables.ChainPolicyAccept
	// Every chain first, so a rule may jump to any of them.
	chains := make([]*nftables.Chain, len(want))
	for i, c := range want {
		if c.regular {
			chains[i] = conn.AddChain(&nftables.Chain{Name: c.name, Table: t})
		} else {
			chains[i] = conn.AddChain(&nftables.Chain{Name: c.name, Table: t, Type: c.typ, Hooknum: &c.hook, Priority: &c.prio, Policy: &accept})
		}
	}
	for i, c := range want {
		for _, r := range c.rules {
			comment, err := r.comment()
			if err != nil {
				return err
			}
			conn.AddRule(&nftables.Rule{
				Table: t, Chain: chains[i], Exprs: r.exprs,
				UserData: userdata.AppendString(nil, userdata.TypeComment, comment),
			})
		}
	}
	if err := conn.Flush(); err != nil {
		// ENOBUFS is the socket's having had no room for the replies, which
		// grow with the table (each rule is echoed): the kernel commits or
		// refuses the batch before it replies, and the table says which.
		if errors.Is(err, unix.ENOBUFS) {
			same, _, verr := tableIs(conn, family, name, want)
			if verr == nil && same {
				return nil
			}
			err = errors.Join(err, verr)
		}
		return fmt.Errorf("writing nft table %s: %w", name, err)
	}
	return nil
}

// tableIs says whether the driver's table of family called name holds
// exactly want, and whether it exists at all.
func tableIs(conn *nftables.Conn, family nftables.TableFamily, name string, want []chainSpec) (same, exists bool, err error) {
	tables, err := conn.ListTablesOfFamily(family)
	if err != nil {
		return false, false, fmt.Errorf("listing nft tables: %w", err)
	}
	var t *nftables.Table
	for _, tt := range tables {
		if tt.Name == name {
			t = tt
		}
	}
	if t == nil {
		return false, false, nil
	}
	chains, err := conn.ListChainsOfTableFamily(family)
	if err != nil {
		return false, true, fmt.Errorf("listing nft chains: %w", err)
	}
	have := map[string]*nftables.Chain{}
	for _, c := range chains {
		if c.Table.Name == name {
			have[c.Name] = c
		}
	}
	if len(have) != len(want) {
		return false, true, nil
	}
	for _, w := range want {
		c := have[w.name]
		if c == nil {
			return false, true, nil
		}
		if w.regular && c.Hooknum != nil {
			return false, true, nil
		}
		if !w.regular && (c.Type != w.typ || c.Hooknum == nil || *c.Hooknum != w.hook || c.Priority == nil || *c.Priority != w.prio ||
			(c.Policy != nil && *c.Policy != nftables.ChainPolicyAccept)) {
			return false, true, nil
		}
		rules, err := conn.GetRules(t, c)
		if err != nil {
			return false, true, fmt.Errorf("listing nft rules: %w", err)
		}
		if len(rules) != len(w.rules) {
			return false, true, nil
		}
		for i, r := range rules {
			want, err := w.rules[i].comment()
			if err != nil {
				return false, true, err
			}
			if commentOf(r) != want {
				return false, true, nil
			}
		}
	}
	return true, true, nil
}

// deleteTable removes the driver's table of family on conn, if it has one.
func deleteTable(conn *nftables.Conn, family nftables.TableFamily) error {
	return deleteNamedTable(conn, family, Table)
}

// deleteNamedTable removes the driver's table of family called name, if
// it has one.
func deleteNamedTable(conn *nftables.Conn, family nftables.TableFamily, name string) error {
	tables, err := conn.ListTablesOfFamily(family)
	if err != nil {
		return fmt.Errorf("listing nft tables: %w", err)
	}
	for _, t := range tables {
		if t.Name == name {
			conn.DelTable(t)
			return conn.Flush()
		}
	}
	return nil
}

// isOurs says whether an nft monitor event is about the driver's table.
func isOurs(data any) bool {
	var t *nftables.Table
	switch v := data.(type) {
	case *nftables.Table:
		t = v
	case *nftables.Chain:
		t = v.Table
	case *nftables.Rule:
		t = v.Table
	}
	return t != nil && isOursFamily(t.Family) && (t.Name == Table || t.Name == FirewallTable)
}
