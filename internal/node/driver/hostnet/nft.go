package hostnet

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"

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
			rules: []ruleSpec{{
				says:  "only replies into a network",
				exprs: append(matchIfnamePrefix(expr.MetaKeyOIFNAME, vethPrefix), dropUnlessReply()...),
			}, {
				// The rule above lets no connection into a network that a reply
				// from it could leave by another link.
				says: fmt.Sprintf("a network reaches only %s", uplink),
				exprs: append(matchIfnamePrefix(expr.MetaKeyIIFNAME, vethPrefix),
					append(matchIfnameNot(expr.MetaKeyOIFNAME, uplink), &expr.Verdict{Kind: expr.VerdictDrop})...),
			}},
		},
		{
			name: "input", typ: nftables.ChainTypeFilter, hook: *nftables.ChainHookInput, prio: *nftables.ChainPriorityFilter,
			rules: []ruleSpec{{
				says:  "only replies from a network into the host",
				exprs: append(matchIfnamePrefix(expr.MetaKeyIIFNAME, vethPrefix), dropUnlessReply()...),
			}},
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
			says: fmt.Sprintf("only %s from %s", cidr, nsBridge),
			exprs: append(matchIfname(expr.MetaKeyIIFNAME, nsBridge),
				append(matchSaddrNot(cidr), &expr.Verdict{Kind: expr.VerdictDrop})...),
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
func ensureTable(conn *nftables.Conn, want []chainSpec) error {
	same, exists, err := tableIs(conn, want)
	if err != nil || same {
		return err
	}
	t := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: Table}
	if exists {
		conn.DelTable(t)
	}
	conn.AddTable(t)
	accept := nftables.ChainPolicyAccept
	for _, c := range want {
		ch := conn.AddChain(&nftables.Chain{Name: c.name, Table: t, Type: c.typ, Hooknum: &c.hook, Priority: &c.prio, Policy: &accept})
		for _, r := range c.rules {
			comment, err := r.comment()
			if err != nil {
				return err
			}
			conn.AddRule(&nftables.Rule{
				Table: t, Chain: ch, Exprs: r.exprs,
				UserData: userdata.AppendString(nil, userdata.TypeComment, comment),
			})
		}
	}
	if err := conn.Flush(); err != nil {
		return fmt.Errorf("writing nft table %s: %w", Table, err)
	}
	return nil
}

// tableIs says whether the driver's table holds exactly want, and whether
// it exists at all.
func tableIs(conn *nftables.Conn, want []chainSpec) (same, exists bool, err error) {
	tables, err := conn.ListTablesOfFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return false, false, fmt.Errorf("listing nft tables: %w", err)
	}
	var t *nftables.Table
	for _, tt := range tables {
		if tt.Name == Table {
			t = tt
		}
	}
	if t == nil {
		return false, false, nil
	}
	chains, err := conn.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return false, true, fmt.Errorf("listing nft chains: %w", err)
	}
	have := map[string]*nftables.Chain{}
	for _, c := range chains {
		if c.Table.Name == Table {
			have[c.Name] = c
		}
	}
	if len(have) != len(want) {
		return false, true, nil
	}
	for _, w := range want {
		c := have[w.name]
		if c == nil || c.Type != w.typ || c.Hooknum == nil || *c.Hooknum != w.hook || c.Priority == nil || *c.Priority != w.prio ||
			(c.Policy != nil && *c.Policy != nftables.ChainPolicyAccept) {
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

// deleteTable removes the driver's table on conn, if it has one.
func deleteTable(conn *nftables.Conn) error {
	tables, err := conn.ListTablesOfFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return fmt.Errorf("listing nft tables: %w", err)
	}
	for _, t := range tables {
		if t.Name == Table {
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
	return t != nil && t.Family == nftables.TableFamilyIPv4 && t.Name == Table
}
