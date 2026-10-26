package hostnet

import (
	"bytes"
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
	vnl "github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// endpointMark, in a conntrack mark, says a network endpoint let the
// connection in, in the root namespace and in both networks'. It is apart
// from natMarkTag, which no connection an endpoint lets in carries, and from
// forwardMark.
const endpointMark = 1 << 29

// setEndpointMark is nft's "ct mark set 0x20000000".
func setEndpointMark() []expr.Any {
	return []expr.Any{
		&expr.Immediate{Register: 1, Data: binaryutil.NativeEndian.PutUint32(endpointMark)},
		&expr.Ct{Key: expr.CtKeyMARK, Register: 1, SourceRegister: true},
	}
}

// endpointInRule is the root forward chain's way from one network into
// another: a connection the endpoint table marked, and no connection a
// network opens to the internet, which carries natMarkTag once it is past
// this rule. As nft's "ct mark & 0xa0000000 == 0x20000000 accept".
func endpointInRule() ruleSpec {
	return ruleSpec{
		says: fmt.Sprintf("from a network into another, what an endpoint marked %#x", endpointMark),
		exprs: slices.Concat(matchIfnamePrefix(expr.MetaKeyIIFNAME, vethPrefix), matchIfnamePrefix(expr.MetaKeyOIFNAME, vethPrefix), []expr.Any{
			&expr.Ct{Register: 1, Key: expr.CtKeyMARK},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
				Mask: binaryutil.NativeEndian.PutUint32(endpointMark | natMarkTag), Xor: make([]byte, 4)},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(endpointMark)},
			&expr.Verdict{Kind: expr.VerdictAccept},
		}),
	}
}

// rootEndpoint is a network's way to a service network's transit port, as
// the root namespace lets it through.
type rootEndpoint struct {
	proto   driver.ForwardProtocol
	veth    string
	service netip.Addr
	port    uint16
}

// rootEndpointChains is the root namespace's endpoint table: before the
// driver's forward chain, a new connection from a network's veth to a
// transit port of a service network it has an endpoint of is marked, which
// endpointInRule then lets through. Nothing else crosses from one network
// to another.
func rootEndpointChains(rs []rootEndpoint) []chainSpec {
	var rules []ruleSpec
	for _, r := range rs {
		rules = append(rules, ruleSpec{
			says: fmt.Sprintf("%s from %s to %s:%d", r.proto, r.veth, r.service, r.port),
			exprs: slices.Concat(matchIfname(expr.MetaKeyIIFNAME, r.veth), matchDaddr(netip.PrefixFrom(r.service, 32)),
				matchL4(protoOf(r.proto)), matchDport(r.port, r.port), matchCtState(expr.CtStateBitNEW), setEndpointMark()),
		})
	}
	return []chainSpec{{
		name: "forward", typ: nftables.ChainTypeFilter, hook: *nftables.ChainHookForward,
		prio:  *nftables.ChainPriorityRef(*nftables.ChainPriorityFilter - 1),
		rules: rules,
	}}
}

// nsEndpoints is what one network's namespace holds of the endpoints: those
// of the network, and those to its machines.
type nsEndpoints struct {
	// in are the network's own endpoints, each DNATed to its service
	// network's transit address and port.
	in []nsEndpointIn
	// to are the transit ports of the network's services, each DNATed to its
	// machine, and from, for each, the transit addresses of the networks
	// with an endpoint of it.
	to   []nsEndpointTo
	from map[nsEndpointTo][]netip.Addr
}

type nsEndpointIn struct {
	proto driver.ForwardProtocol
	at    netip.AddrPort
	via   netip.AddrPort
}

type nsEndpointTo struct {
	proto driver.ForwardProtocol
	port  uint16
	to    netip.AddrPort
}

// addrs are the network's endpoints' addresses, each once.
func (e nsEndpoints) addrs() []netip.Addr {
	var out []netip.Addr
	for _, in := range e.in {
		if !slices.Contains(out, in.at.Addr()) {
			out = append(out, in.at.Addr())
		}
	}
	return out
}

// nsEndpointChains is a network namespace's endpoint table. An endpoint's
// address and port, coming in from the bridge, is marked and DNATed to its
// service network's transit address and port, and leaves by hn-up as the
// network's transit address, as everything else does. Anything else to an
// endpoint's address goes no further than the namespace, where nothing
// takes it. A service's transit port, coming in by hn-up, is marked and
// DNATed to its machine, the source kept, so the machine's security groups
// judge the network it came from.
func nsEndpointChains(nsTransit netip.Addr, e nsEndpoints) []chainSpec {
	var nat, input []ruleSpec
	for _, in := range e.in {
		nat = append(nat, ruleSpec{
			says: fmt.Sprintf("%s %s in by %s to %s", in.proto, in.at, nsBridge, in.via),
			exprs: slices.Concat(matchIfname(expr.MetaKeyIIFNAME, nsBridge), matchDaddr(netip.PrefixFrom(in.at.Addr(), 32)),
				matchL4(protoOf(in.proto)), matchDport(in.at.Port(), in.at.Port()), setEndpointMark(), dnatPort(in.via)),
		})
	}
	for _, a := range e.addrs() {
		input = append(input, ruleSpec{
			says:  fmt.Sprintf("nothing else to endpoint %s", a),
			exprs: append(matchDaddr(netip.PrefixFrom(a, 32)), &expr.Verdict{Kind: expr.VerdictDrop}),
		})
	}
	for _, to := range e.to {
		nat = append(nat, ruleSpec{
			says: fmt.Sprintf("%s %s:%d in by %s to %s", to.proto, nsTransit, to.port, nsUplink, to.to),
			exprs: slices.Concat(matchIfname(expr.MetaKeyIIFNAME, nsUplink), matchDaddr(netip.PrefixFrom(nsTransit, 32)),
				matchL4(protoOf(to.proto)), matchDport(to.port, to.port), setEndpointMark(), dnatPort(to.to)),
		})
	}
	return []chainSpec{{
		name: "prerouting", typ: nftables.ChainTypeNAT, hook: *nftables.ChainHookPrerouting, prio: *nftables.ChainPriorityNATDest,
		rules: nat,
	}, {
		name: "input", typ: nftables.ChainTypeFilter, hook: *nftables.ChainHookInput, prio: *nftables.ChainPriorityFilter,
		rules: input,
	}}
}

// hostNetwork is what EnsureEndpoints needs of a network on the host.
type hostNetwork struct {
	transit netip.Addr
	veth    string
}

func (d *Driver) EnsureEndpoints(_ context.Context, endpoints []driver.Endpoint) error {
	if err := driver.CheckEndpoints(endpoints); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	uids, err := namespaces()
	if err != nil {
		return err
	}
	nets, err := d.hostNetworks(uids)
	if err != nil {
		return err
	}
	es := slices.Clone(endpoints)
	driver.SortEndpoints(es)
	// A network not yet whole takes its endpoints when it is: the agent
	// ensures them again on its change.
	es = slices.DeleteFunc(es, func(e driver.Endpoint) bool {
		_, in := nets[e.NetworkUID]
		_, svc := nets[e.ServiceNetworkUID]
		return !in || !svc
	})
	byNS := map[string]*nsEndpoints{}
	of := func(uid string) *nsEndpoints {
		if byNS[uid] == nil {
			byNS[uid] = &nsEndpoints{from: map[nsEndpointTo][]netip.Addr{}}
		}
		return byNS[uid]
	}
	var root []rootEndpoint
	for _, e := range es {
		svc := nets[e.ServiceNetworkUID].transit
		of(e.NetworkUID).in = append(of(e.NetworkUID).in, nsEndpointIn{
			proto: e.Protocol, at: netip.AddrPortFrom(e.Addr, e.Port), via: netip.AddrPortFrom(svc, e.TransitPort),
		})
		to := nsEndpointTo{proto: e.Protocol, port: e.TransitPort, to: e.To}
		p := of(e.ServiceNetworkUID)
		if !slices.Contains(p.to, to) {
			p.to = append(p.to, to)
		}
		if from := nets[e.NetworkUID].transit; !slices.Contains(p.from[to], from) {
			p.from[to] = append(p.from[to], from)
		}
		r := rootEndpoint{proto: e.Protocol, veth: nets[e.NetworkUID].veth, service: svc, port: e.TransitPort}
		if !slices.Contains(root, r) {
			root = append(root, r)
		}
	}
	// The root first, and whatever becomes of the namespaces: it alone
	// decides which network crosses to which, so a way taken away is gone
	// even while another network's namespace cannot be written.
	if err := ensureRootEndpoints(nets, root); err != nil {
		return err
	}
	var errs []error
	for _, uid := range uids {
		want := nsEndpoints{}
		if e := byNS[uid]; e != nil {
			want = *e
		}
		// A namespace not whole holds none: what it held before goes, so
		// no address of an endpoint let go lingers on its bridge.
		if err := ensureNSEndpoints(uid, nets[uid].transit, want); err != nil {
			errs = append(errs, fmt.Errorf("network %s: %w", uid, err))
		}
	}
	return errors.Join(errs...)
}

// ensureRootEndpoints makes the root namespace's endpoint table hold rs, or
// removes it for none, and ends the flows it no longer lets across.
func ensureRootEndpoints(nets map[string]hostNetwork, rs []rootEndpoint) error {
	conn, err := nftables.New()
	if err != nil {
		return err
	}
	had, err := hasTable(conn, EndpointTable)
	if err != nil {
		return err
	}
	if len(rs) == 0 {
		err = deleteNamedTable(conn, nftables.TableFamilyIPv4, EndpointTable)
	} else {
		err = ensureNamedTable(conn, nftables.TableFamilyIPv4, EndpointTable, rootEndpointChains(rs))
	}
	if err != nil || (!had && len(rs) == 0) {
		return err
	}
	// The root namespace's entry of an open flow would otherwise keep
	// passing it as one already admitted.
	return dropStaleEndpointFlows(&netlink.Config{}, func(proto byte, src, dst, _ netip.AddrPort) bool {
		return slices.ContainsFunc(rs, func(r rootEndpoint) bool {
			n, ok := nets[uidOfTransit(nets, src.Addr())]
			return ok && n.veth == r.veth && protoOf(r.proto) == proto && dst == netip.AddrPortFrom(r.service, r.port)
		})
	})
}

// uidOfTransit is the network whose namespace end of its transit link is a;
// "" for none.
func uidOfTransit(nets map[string]hostNetwork, a netip.Addr) string {
	for uid, n := range nets {
		if n.transit == a {
			return uid
		}
	}
	return ""
}

// hostNetworks are the networks of uids whose transit link is whole, by
// uid.
func (d *Driver) hostNetworks(uids []string) (map[string]hostNetwork, error) {
	out := map[string]hostNetwork{}
	for _, uid := range uids {
		veths, err := d.vethsOf(uid)
		if err != nil {
			return nil, err
		}
		if len(veths) != 1 {
			continue
		}
		t, err := d.nsTransitOf(uid)
		if err != nil {
			return nil, err
		}
		if t.IsValid() {
			out[uid] = hostNetwork{transit: t, veth: veths[0].Attrs().Name}
		}
	}
	return out, nil
}

// ensureNSEndpoints makes a network's namespace's endpoint table and its
// bridge's endpoint addresses hold e, or removes them for none, and ends
// the flows of its endpoints gone or moved, and those of networks no longer
// let through to its services. nsTransit is invalid for a namespace not
// whole, which e is then empty for.
func ensureNSEndpoints(uid string, nsTransit netip.Addr, e nsEndpoints) error {
	ns, ok, err := openNS(uid)
	if err != nil || !ok {
		return err
	}
	defer func() { _ = ns.Close() }()
	h, err := vnl.NewHandleAt(ns)
	if err != nil {
		return err
	}
	defer h.Close()
	conn, err := nftables.New(nftables.WithNetNSFd(int(ns)))
	if err != nil {
		return err
	}
	had, err := hasTable(conn, EndpointTable)
	if err != nil {
		return err
	}
	// Addresses going go before the table that drops what else comes to
	// them, and addresses coming after it, so no address is ever held
	// without its rules.
	if err := endpointAddrs(h, e.addrs(), false); err != nil {
		return fmt.Errorf("endpoint addresses: %w", err)
	}
	none := len(e.in) == 0 && len(e.to) == 0
	if none {
		err = deleteNamedTable(conn, nftables.TableFamilyIPv4, EndpointTable)
	} else {
		err = ensureNamedTable(conn, nftables.TableFamilyIPv4, EndpointTable, nsEndpointChains(nsTransit, e))
	}
	if err != nil {
		return err
	}
	if err := endpointAddrs(h, e.addrs(), true); err != nil {
		return fmt.Errorf("endpoint addresses: %w", err)
	}
	if !had && none {
		return nil
	}
	return dropStaleEndpointFlows(&netlink.Config{NetNS: int(ns)}, func(proto byte, src, dst, reply netip.AddrPort) bool {
		if nsTransit.IsValid() && dst.Addr() == nsTransit {
			return slices.ContainsFunc(e.to, func(to nsEndpointTo) bool {
				return protoOf(to.proto) == proto && to.port == dst.Port() && to.to == reply && slices.Contains(e.from[to], src.Addr())
			})
		}
		return slices.ContainsFunc(e.in, func(in nsEndpointIn) bool {
			return protoOf(in.proto) == proto && in.at == dst && in.via == reply
		})
	})
}

// endpointAddrs removes the bridge's addresses labelled as endpoints' but
// those in want, or with add, adds those of want it lacks, each a /32: the
// bridge answers ARP for them, and the endpoint table takes what comes to
// them.
func endpointAddrs(h *vnl.Handle, want []netip.Addr, add bool) error {
	br, err := linkByName(h, nsBridge)
	if err != nil {
		return err
	}
	if br == nil {
		if add && len(want) > 0 {
			return errors.New("the network has no bridge yet")
		}
		return nil
	}
	addrs, err := h.AddrList(br, unix.AF_INET)
	if err != nil {
		return err
	}
	have := map[netip.Addr]bool{}
	for _, a := range addrs {
		if a.Label != endpointLabel {
			continue
		}
		p, _ := prefixOf(a.IPNet)
		if p.Bits() == 32 && slices.Contains(want, p.Addr()) {
			have[p.Addr()] = true
			continue
		}
		if err := h.AddrDel(br, &a); err != nil {
			return err
		}
	}
	for _, a := range want {
		if have[a] || !add {
			continue
		}
		if err := h.AddrAdd(br, &vnl.Addr{IPNet: ipNet(netip.PrefixFrom(a, 32)), Label: endpointLabel}); err != nil {
			return fmt.Errorf("%s: %w", a, err)
		}
	}
	return nil
}

// dropStaleEndpointFlows deletes the conntrack entries an endpoint let in
// (endpointMark), in the namespace of cfg, that keep does not keep: keep is
// given an entry's protocol, its original source and destination, and its
// reply's source, where the connection was NATed to.
func dropStaleEndpointFlows(cfg *netlink.Config, keep func(proto byte, src, dst, reply netip.AddrPort) bool) error {
	c, err := conntrack.Dial(cfg)
	if err != nil {
		return fmt.Errorf("conntrack: %w", err)
	}
	defer func() { _ = c.Close() }()
	flows, err := c.DumpFilter(conntrack.NewFilter().Mark(endpointMark).MarkMask(endpointMark), nil)
	if err != nil {
		return fmt.Errorf("listing endpoints' flows: %w", err)
	}
	for _, f := range flows {
		o, r := f.TupleOrig, f.TupleReply
		src := netip.AddrPortFrom(o.IP.SourceAddress, o.Proto.SourcePort)
		dst := netip.AddrPortFrom(o.IP.DestinationAddress, o.Proto.DestinationPort)
		reply := netip.AddrPortFrom(r.IP.SourceAddress, r.Proto.SourcePort)
		if keep(o.Proto.Protocol, src, dst, reply) {
			continue
		}
		// Gone meanwhile is as good.
		if err := c.Delete(conntrack.Flow{TupleOrig: o}); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("ending an endpoint's flow to %s: %w", dst, err)
		}
	}
	return nil
}

func (d *Driver) Endpoints(context.Context) ([]driver.Endpoint, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	uids, err := namespaces()
	if err != nil {
		return nil, err
	}
	nets, err := d.hostNetworks(uids)
	if err != nil {
		return nil, err
	}
	conn, err := nftables.New()
	if err != nil {
		return nil, err
	}
	rootRules, err := chainRules(conn, EndpointTable, "forward")
	if err != nil {
		return nil, err
	}
	allowed := map[rootEndpoint]bool{}
	for _, r := range rootRules {
		if m, ok := decodeEndpointRule(r); ok {
			allowed[rootEndpoint{proto: m.proto, veth: m.iif, service: m.daddr, port: m.dport}] = true
		}
	}
	ins := map[string][]endpointMatch{}
	tos := map[string][]endpointMatch{}
	for uid := range nets {
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
			return chainRules(conn, EndpointTable, "prerouting")
		}()
		if err != nil {
			return nil, fmt.Errorf("network %s: %w", uid, err)
		}
		for _, r := range rules {
			m, ok := decodeEndpointRule(r)
			switch {
			case !ok || !m.to.IsValid():
			case m.iif == nsBridge:
				ins[uid] = append(ins[uid], m)
			case m.iif == nsUplink && m.daddr == nets[uid].transit:
				tos[uid] = append(tos[uid], m)
			}
		}
	}
	var out []driver.Endpoint
	for uid, ms := range ins {
		for _, in := range ms {
			svc := uidOfTransit(nets, in.to.Addr())
			if svc == "" || !allowed[rootEndpoint{proto: in.proto, veth: nets[uid].veth, service: in.to.Addr(), port: in.to.Port()}] {
				continue
			}
			i := slices.IndexFunc(tos[svc], func(to endpointMatch) bool { return to.proto == in.proto && to.dport == in.to.Port() })
			if i < 0 {
				continue
			}
			out = append(out, driver.Endpoint{
				Protocol: in.proto, NetworkUID: uid, Addr: in.daddr, Port: in.dport,
				ServiceNetworkUID: svc, TransitPort: in.to.Port(), To: tos[svc][i].to,
			})
		}
	}
	driver.SortEndpoints(out)
	return out, nil
}

// chainRules are the rules of conn's namespace's chain of the driver's table
// called table, family ip; none without one.
func chainRules(conn *nftables.Conn, table, chain string) ([]*nftables.Rule, error) {
	chains, err := conn.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return nil, fmt.Errorf("listing nft chains: %w", err)
	}
	for _, c := range chains {
		if c.Table.Name == table && c.Name == chain {
			rules, err := conn.GetRules(c.Table, c)
			if err != nil {
				return nil, fmt.Errorf("listing nft rules: %w", err)
			}
			return rules, nil
		}
	}
	return nil, nil
}

// endpointMatch is an endpoint table's rule read back: what it matches, and
// where it DNATs to, if it does.
type endpointMatch struct {
	iif   string
	daddr netip.Addr
	proto driver.ForwardProtocol
	dport uint16
	to    netip.AddrPort
}

// decodeEndpointRule reads back a rule of an endpoint table that matches an
// input link, a destination address, a protocol and a port.
func decodeEndpointRule(r *nftables.Rule) (m endpointMatch, ok bool) {
	regs := map[uint32][]byte{}
	for i, e := range r.Exprs {
		switch e := e.(type) {
		case *expr.Meta:
			c, isCmp := next(r.Exprs, i).(*expr.Cmp)
			switch {
			case !isCmp:
			case e.Key == expr.MetaKeyIIFNAME:
				m.iif = string(bytes.TrimRight(c.Data, "\x00"))
			case e.Key == expr.MetaKeyL4PROTO && len(c.Data) == 1 && c.Data[0] == unix.IPPROTO_TCP:
				m.proto = driver.TCP
			case e.Key == expr.MetaKeyL4PROTO && len(c.Data) == 1 && c.Data[0] == unix.IPPROTO_UDP:
				m.proto = driver.UDP
			}
		case *expr.Payload:
			if e.Base != expr.PayloadBaseNetworkHeader || e.Offset != 16 || e.Len != 4 || i+2 >= len(r.Exprs) {
				continue
			}
			if c, isCmp := r.Exprs[i+2].(*expr.Cmp); isCmp {
				m.daddr, _ = netip.AddrFromSlice(c.Data)
			}
		case *expr.Range:
			if len(e.FromData) == 2 {
				m.dport = binaryutil.BigEndian.Uint16(e.FromData)
			}
		case *expr.Immediate:
			regs[e.Register] = e.Data
		case *expr.NAT:
			addr, isAddr := netip.AddrFromSlice(regs[e.RegAddrMin])
			p := regs[e.RegProtoMin]
			if e.Type != expr.NATTypeDestNAT || !isAddr || e.RegProtoMin == 0 || len(p) != 2 {
				return m, false
			}
			m.to = netip.AddrPortFrom(addr, binaryutil.BigEndian.Uint16(p))
		}
	}
	return m, m.iif != "" && m.daddr.Is4() && m.proto != 0 && m.dport != 0
}
