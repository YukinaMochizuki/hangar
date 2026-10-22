package hostnet

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/mdlayher/netlink"
	"github.com/ti-mo/conntrack"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// protoOf is a forward's protocol as nft matches it.
func protoOf(p driver.ForwardProtocol) byte {
	if p == driver.UDP {
		return unix.IPPROTO_UDP
	}
	return unix.IPPROTO_TCP
}

// rootForward is a forward as the root namespace sees it: a port of the
// shared address, to the namespace end of its network's transit link.
type rootForward struct {
	proto driver.ForwardProtocol
	port  uint16
	to    netip.Addr
}

// forwardChains is the root namespace's forward table: each forward's port
// at the shared address, coming in by the uplink, marked as a forward's
// (forwardMark) and DNATed to its network's namespace, on the same port.
// Machines on the host itself never come in by the uplink, so they do not
// reach a forward there.
func forwardChains(uplink string, shared netip.Addr, fs []rootForward) []chainSpec {
	var rules []ruleSpec
	for _, f := range fs {
		rules = append(rules, ruleSpec{
			says: fmt.Sprintf("%s %s:%d in by %s to %s", f.proto, shared, f.port, uplink, f.to),
			exprs: slices.Concat(matchIfname(expr.MetaKeyIIFNAME, uplink), matchDaddr(netip.PrefixFrom(shared, 32)),
				matchL4(protoOf(f.proto)), matchDport(f.port, f.port), setForwardMark(), dnat(f.to)),
		})
	}
	return []chainSpec{{
		name: "prerouting", typ: nftables.ChainTypeNAT, hook: *nftables.ChainHookPrerouting, prio: *nftables.ChainPriorityNATDest,
		rules: rules,
	}}
}

// nsForwardChains is a network namespace's forward table: each of its
// forwards' ports at the namespace end of its transit link, coming in by
// hn-up, marked as a forward's and DNATed to its machine's address and
// port. The source is left as it is, so the interface's security groups
// judge the client itself.
func nsForwardChains(nsTransit netip.Addr, fs []driver.Forward) []chainSpec {
	var rules []ruleSpec
	for _, f := range fs {
		rules = append(rules, ruleSpec{
			says: fmt.Sprintf("%s %s:%d in by %s to %s", f.Protocol, nsTransit, f.Port, nsUplink, f.To),
			exprs: slices.Concat(matchIfname(expr.MetaKeyIIFNAME, nsUplink), matchDaddr(netip.PrefixFrom(nsTransit, 32)),
				matchL4(protoOf(f.Protocol)), matchDport(f.Port, f.Port), setForwardMark(), dnatPort(f.To)),
		})
	}
	return []chainSpec{{
		name: "prerouting", typ: nftables.ChainTypeNAT, hook: *nftables.ChainHookPrerouting, prio: *nftables.ChainPriorityNATDest,
		rules: rules,
	}}
}

// forwardMark, in a conntrack mark, says a port forward let the
// connection in, in the root namespace and in the network's. It is apart
// from natMarkTag and the veth index below it, which only connections a
// network opens carry: so the NAT log takes no forward's connection, and
// no connection a network opens is taken for a forward's.
const forwardMark = 1 << 30

// setForwardMark is nft's "ct mark set 0x40000000". A connection coming
// in by a forward has no mark before.
func setForwardMark() []expr.Any {
	return []expr.Any{
		&expr.Immediate{Register: 1, Data: binaryutil.NativeEndian.PutUint32(forwardMark)},
		&expr.Ct{Key: expr.CtKeyMARK, Register: 1, SourceRegister: true},
	}
}

// forwardedInRule is the root forward chain's way into a network from the
// internet, with a shared address: a connection from the uplink that the
// forward table DNATed and marked as a forward's, as nft's "ct status dnat
// ct mark & 0x40000000 == 0x40000000 accept". Nothing else from the uplink
// starts a connection into a network.
func forwardedInRule(uplink string) ruleSpec {
	return ruleSpec{
		says: fmt.Sprintf("forwards from %s, DNATed and marked %#x, into a network", uplink, forwardMark),
		exprs: slices.Concat(matchIfname(expr.MetaKeyIIFNAME, uplink), matchIfnamePrefix(expr.MetaKeyOIFNAME, vethPrefix), []expr.Any{
			&expr.Ct{Register: 1, Key: expr.CtKeySTATUS},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
				Mask: binaryutil.NativeEndian.PutUint32(ctStatusDNAT), Xor: make([]byte, 4)},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: make([]byte, 4)},
			&expr.Ct{Register: 1, Key: expr.CtKeyMARK},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
				Mask: binaryutil.NativeEndian.PutUint32(forwardMark), Xor: make([]byte, 4)},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(forwardMark)},
			&expr.Verdict{Kind: expr.VerdictAccept},
		}),
	}
}

// ctStatusDNAT is conntrack's IPS_DST_NAT, a connection whose destination
// was translated.
const ctStatusDNAT = 1 << 5

// dnat is nft's "dnat to addr", keeping the port.
func dnat(to netip.Addr) []expr.Any {
	return []expr.Any{
		&expr.Immediate{Register: 1, Data: to.AsSlice()},
		&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1},
	}
}

// dnatPort is nft's "dnat to addr:port"; the protocol is matched before it.
func dnatPort(to netip.AddrPort) []expr.Any {
	return []expr.Any{
		&expr.Immediate{Register: 1, Data: to.Addr().AsSlice()},
		&expr.Immediate{Register: 2, Data: binaryutil.BigEndian.PutUint16(to.Port())},
		&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1, RegProtoMin: 2, RegProtoMax: 2, Specified: true},
	}
}

// forwardKey is a forward's protocol, as conntrack and nft name it, and
// port.
type forwardKey struct {
	proto byte
	port  uint16
}

func (d *Driver) EnsureForwards(_ context.Context, forwards []driver.Forward) error {
	if len(forwards) > 0 && !d.cfg.SharedIPv4.IsValid() {
		return errors.New("the host has no shared IPv4 address to forward ports of (--shared-ipv4)")
	}
	if err := driver.CheckForwards(forwards); err != nil {
		return err
	}
	byNetwork := map[string][]driver.Forward{}
	for _, f := range forwards {
		byNetwork[f.NetworkUID] = append(byNetwork[f.NetworkUID], f)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	uids, err := namespaces()
	if err != nil {
		return err
	}
	var root []rootForward
	for _, uid := range uids {
		fs := byNetwork[uid]
		var nsTransit netip.Addr
		if len(fs) > 0 {
			if nsTransit, err = d.nsTransitOf(uid); err != nil {
				return err
			}
		}
		// A network not yet whole takes its forwards when it is: the
		// agent ensures them again on its change.
		if !nsTransit.IsValid() {
			fs = nil
		}
		slices.SortFunc(fs, func(a, b driver.Forward) int {
			return cmp.Or(cmp.Compare(a.Protocol, b.Protocol), cmp.Compare(a.Port, b.Port))
		})
		if err := ensureNSForwards(uid, nsTransit, fs); err != nil {
			return fmt.Errorf("network %s: %w", uid, err)
		}
		for _, f := range fs {
			root = append(root, rootForward{proto: f.Protocol, port: f.Port, to: nsTransit})
		}
	}
	slices.SortFunc(root, func(a, b rootForward) int {
		return cmp.Or(cmp.Compare(a.proto, b.proto), cmp.Compare(a.port, b.port))
	})
	conn, err := nftables.New()
	if err != nil {
		return err
	}
	had, err := hasTable(conn, ForwardTable)
	if err != nil {
		return err
	}
	if len(root) == 0 {
		err = deleteNamedTable(conn, nftables.TableFamilyIPv4, ForwardTable)
	} else {
		err = ensureNamedTable(conn, nftables.TableFamilyIPv4, ForwardTable, forwardChains(d.cfg.Uplink, d.cfg.SharedIPv4, root))
	}
	if err != nil || (!had && len(root) == 0) {
		return err
	}
	// What forwards let in before lives on in conntrack, which would keep
	// taking an open flow to the machine it reached, whatever the tables
	// say now.
	wanted := map[forwardKey]netip.Addr{}
	for _, f := range root {
		wanted[forwardKey{protoOf(f.proto), f.port}] = f.to
	}
	return dropStaleFlows(&netlink.Config{}, func(k forwardKey, dst, target netip.AddrPort) bool {
		to, ok := wanted[k]
		return ok && dst.Addr() == d.cfg.SharedIPv4 && target == netip.AddrPortFrom(to, k.port)
	})
}

// nsTransitOf is the namespace end of a network's transit link; invalid
// while the network has none.
func (d *Driver) nsTransitOf(uid string) (netip.Addr, error) {
	veths, err := d.vethsOf(uid)
	if err != nil || len(veths) != 1 {
		return netip.Addr{}, err
	}
	p, ok, err := addrOf(d.root, veths[0])
	if err != nil || !ok {
		return netip.Addr{}, err
	}
	return p.Addr().Next(), nil
}

// hasTable says whether conn's namespace has the driver's table of family
// ip called name.
func hasTable(conn *nftables.Conn, name string) (bool, error) {
	tables, err := conn.ListTablesOfFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return false, fmt.Errorf("listing nft tables: %w", err)
	}
	return slices.ContainsFunc(tables, func(t *nftables.Table) bool { return t.Name == name }), nil
}

// ensureNSForwards makes a network's namespace forward table hold fs, or
// removes it for none, and ends the flows of its forwards gone or moved.
func ensureNSForwards(uid string, nsTransit netip.Addr, fs []driver.Forward) error {
	ns, ok, err := openNS(uid)
	if err != nil || !ok {
		return err
	}
	defer func() { _ = ns.Close() }()
	conn, err := nftables.New(nftables.WithNetNSFd(int(ns)))
	if err != nil {
		return err
	}
	had, err := hasTable(conn, ForwardTable)
	if err != nil {
		return err
	}
	if len(fs) == 0 {
		err = deleteNamedTable(conn, nftables.TableFamilyIPv4, ForwardTable)
	} else {
		err = ensureNamedTable(conn, nftables.TableFamilyIPv4, ForwardTable, nsForwardChains(nsTransit, fs))
	}
	if err != nil || (!had && len(fs) == 0) {
		return err
	}
	wanted := map[forwardKey]netip.AddrPort{}
	for _, f := range fs {
		wanted[forwardKey{protoOf(f.Protocol), f.Port}] = f.To
	}
	return dropStaleFlows(&netlink.Config{NetNS: int(ns)}, func(k forwardKey, dst, target netip.AddrPort) bool {
		to, ok := wanted[k]
		return ok && dst.Addr() == nsTransit && target == to
	})
}

// dropStaleFlows deletes the conntrack entries a forward let in
// (forwardMark), in the namespace of cfg, that keep does not keep: keep
// is given an entry's protocol and destination port, its original
// destination, and its target, where its replies come from.
func dropStaleFlows(cfg *netlink.Config, keep func(k forwardKey, dst, target netip.AddrPort) bool) error {
	c, err := conntrack.Dial(cfg)
	if err != nil {
		return fmt.Errorf("conntrack: %w", err)
	}
	defer func() { _ = c.Close() }()
	flows, err := c.DumpFilter(conntrack.NewFilter().Mark(forwardMark).MarkMask(forwardMark), nil)
	if err != nil {
		return fmt.Errorf("listing forwarded flows: %w", err)
	}
	for _, f := range flows {
		o, r := f.TupleOrig, f.TupleReply
		k := forwardKey{o.Proto.Protocol, o.Proto.DestinationPort}
		dst := netip.AddrPortFrom(o.IP.DestinationAddress, o.Proto.DestinationPort)
		target := netip.AddrPortFrom(r.IP.SourceAddress, r.Proto.SourcePort)
		if keep(k, dst, target) {
			continue
		}
		// Gone meanwhile is as good.
		if err := c.Delete(conntrack.Flow{TupleOrig: o}); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("ending a forwarded flow to %s: %w", dst, err)
		}
	}
	return nil
}

func (d *Driver) Forwards(context.Context) ([]driver.Forward, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	conn, err := nftables.New()
	if err != nil {
		return nil, err
	}
	rootRules, err := forwardRules(conn)
	if err != nil {
		return nil, err
	}
	root := map[forwardKey]netip.Addr{}
	for _, r := range rootRules {
		if k, to, ok := decodeForward(r); ok {
			root[k] = to.Addr()
		}
	}
	uids, err := namespaces()
	if err != nil {
		return nil, err
	}
	var out []driver.Forward
	for _, uid := range uids {
		nsTransit, err := d.nsTransitOf(uid)
		if err != nil {
			return nil, err
		}
		if !nsTransit.IsValid() {
			continue
		}
		rules, err := func() ([]*nftables.Rule, error) {
			ns, ok, err := openNS(uid)
			if err != nil || !ok {
				return nil, err
			}
			defer func() { _ = ns.Close() }()
			conn, err := nftables.New(nftables.WithNetNSFd(int(ns)))
			if err != nil {
				return nil, err
			}
			return forwardRules(conn)
		}()
		if err != nil {
			return nil, fmt.Errorf("network %s: %w", uid, err)
		}
		for _, r := range rules {
			k, to, ok := decodeForward(r)
			if !ok || root[k] != nsTransit {
				continue
			}
			protocol := driver.TCP
			if k.proto == unix.IPPROTO_UDP {
				protocol = driver.UDP
			}
			out = append(out, driver.Forward{Protocol: protocol, Port: k.port, NetworkUID: uid, To: to})
		}
	}
	slices.SortFunc(out, func(a, b driver.Forward) int {
		return cmp.Or(cmp.Compare(a.Protocol, b.Protocol), cmp.Compare(a.Port, b.Port))
	})
	return out, nil
}

// forwardRules are the rules of conn's namespace's forward table, none
// without one.
func forwardRules(conn *nftables.Conn) ([]*nftables.Rule, error) {
	chains, err := conn.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return nil, fmt.Errorf("listing nft chains: %w", err)
	}
	for _, c := range chains {
		if c.Table.Name == ForwardTable && c.Name == "prerouting" {
			rules, err := conn.GetRules(c.Table, c)
			if err != nil {
				return nil, fmt.Errorf("listing nft rules: %w", err)
			}
			return rules, nil
		}
	}
	return nil, nil
}

// decodeForward reads a forward table's rule back: the protocol and port
// it takes and where it DNATs them, with port 0 for one that keeps the
// port.
func decodeForward(r *nftables.Rule) (k forwardKey, to netip.AddrPort, ok bool) {
	regs := map[uint32][]byte{}
	for i, e := range r.Exprs {
		switch e := e.(type) {
		case *expr.Meta:
			if c, isCmp := next(r.Exprs, i).(*expr.Cmp); isCmp && e.Key == expr.MetaKeyL4PROTO && len(c.Data) == 1 {
				k.proto = c.Data[0]
			}
		case *expr.Range:
			if len(e.FromData) == 2 {
				k.port = binaryutil.BigEndian.Uint16(e.FromData)
			}
		case *expr.Immediate:
			regs[e.Register] = e.Data
		case *expr.NAT:
			addr, isAddr := netip.AddrFromSlice(regs[e.RegAddrMin])
			var port uint16
			if p := regs[e.RegProtoMin]; e.RegProtoMin != 0 && len(p) == 2 {
				port = binaryutil.BigEndian.Uint16(p)
			}
			return k, netip.AddrPortFrom(addr, port), e.Type == expr.NATTypeDestNAT && isAddr && addr.Is4() && k.proto != 0 && k.port != 0
		}
	}
	return k, netip.AddrPort{}, false
}

// next is the expression after the i-th, nil for none.
func next(exprs []expr.Any, i int) expr.Any {
	if i+1 < len(exprs) {
		return exprs[i+1]
	}
	return nil
}
