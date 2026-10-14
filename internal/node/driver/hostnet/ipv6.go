package hostnet

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// The transit link's IPv6 addresses, link-local only: the host's end and
// the namespace's. The bridge holds driver.GatewayIPv6.
var (
	hostTransitIPv6 = netip.MustParseAddr("fe80::1")
	nsTransitIPv6   = netip.MustParseAddr("fe80::2")
	linkLocal       = netip.MustParsePrefix("fe80::/10")
)

// proxyPoll is how often the uplink's proxy NDP entries are compared with
// the taps' addresses: the kernel signals no change to them.
const proxyPoll = 5 * time.Second

// checkHostIPv6 refuses a host whose settings, deploy/host's, would leave
// the networks' IPv6 dead: forwarding, and proxy NDP on the uplink.
func (d *Driver) checkHostIPv6() error {
	if v, err := sysctl("net.ipv6.conf.all.forwarding"); err != nil {
		return err
	} else if v != "1" {
		return errors.New("IPv6 forwarding is off on the host (net.ipv6.conf.all.forwarding); deploy/host turns it on")
	}
	all, err := sysctl("net.ipv6.conf.all.proxy_ndp")
	if err != nil {
		return err
	}
	uplink, err := sysctl("net.ipv6.conf." + d.cfg.Uplink + ".proxy_ndp")
	if err != nil {
		return err
	}
	if all != "1" && uplink != "1" {
		return fmt.Errorf("proxy NDP is off on %s (net.ipv6.conf.%s.proxy_ndp); deploy/host turns it on", d.cfg.Uplink, d.cfg.Uplink)
	}
	return nil
}

// linkIPv6Sysctls turn IPv6 on for a link with only the address the driver
// gives it: no address of the kernel's own (addr_gen_mode 1, set before
// IPv6 is), and no router advertisements taken.
func linkIPv6Sysctls(name string) [][2]string {
	c := "net.ipv6.conf." + name + "."
	return [][2]string{{c + "addr_gen_mode", "1"}, {c + "accept_ra", "0"}, {c + "disable_ipv6", "0"}}
}

func linkIPv6Off(name string) [][2]string {
	return [][2]string{{"net.ipv6.conf." + name + ".disable_ipv6", "1"}}
}

// ensureAddr6 makes want the link's only IPv6 address, usable at once:
// there is no other host on a link of the driver's to collide with.
func ensureAddr6(h *netlink.Handle, l netlink.Link, want netip.Prefix) error {
	addrs, err := dumped(func() ([]netlink.Addr, error) { return h.AddrList(l, unix.AF_INET6) })
	if err != nil {
		return err
	}
	found := false
	for _, a := range addrs {
		if p, _ := prefixOf(a.IPNet); p == want {
			found = true
			continue
		}
		if err := h.AddrDel(l, &a); err != nil && !errors.Is(err, unix.EADDRNOTAVAIL) {
			return err
		}
	}
	if found {
		return nil
	}
	return h.AddrAdd(l, &netlink.Addr{IPNet: ipNet(want), Flags: unix.IFA_F_NODAD})
}

// ensureNetworkIPv6 gives a network's namespace IPv6 if the host has a
// prefix: its links, their addresses and default route, and its ip6 table;
// without one, IPv6 stays off in it, as it was made.
func (d *Driver) ensureNetworkIPv6(ns netns.NsHandle, h *netlink.Handle, host, inner, br netlink.Link) error {
	conn, err := nftables.New(nftables.WithNetNSFd(int(ns)))
	if err != nil {
		return err
	}
	if !d.cfg.IPv6Prefix.IsValid() {
		if err := inNS(ns, func() error {
			return ensureSysctls(slices.Concat(linkIPv6Off(nsUplink), linkIPv6Off(nsBridge)))
		}); err != nil {
			return err
		}
		return deleteTable(conn, nftables.TableFamilyIPv6)
	}
	if err := ensureSysctls(linkIPv6Sysctls(host.Attrs().Name)); err != nil {
		return fmt.Errorf("host end of the transit link: %w", err)
	}
	if err := ensureAddr6(d.root, host, netip.PrefixFrom(hostTransitIPv6, 64)); err != nil {
		return fmt.Errorf("host end of the transit link: %w", err)
	}
	if err := inNS(ns, func() error {
		return ensureSysctls(slices.Concat([][2]string{{"net.ipv6.conf.all.forwarding", "1"}},
			linkIPv6Sysctls(nsUplink), linkIPv6Sysctls(nsBridge)))
	}); err != nil {
		return err
	}
	if err := ensureAddr6(h, inner, netip.PrefixFrom(nsTransitIPv6, 64)); err != nil {
		return fmt.Errorf("namespace end of the transit link: %w", err)
	}
	if err := ensureAddr6(h, br, netip.PrefixFrom(driver.GatewayIPv6, 64)); err != nil {
		return fmt.Errorf("IPv6 gateway: %w", err)
	}
	if err := ensureDefaultRoute6(h, inner); err != nil {
		return fmt.Errorf("IPv6 default route: %w", err)
	}
	return ensureTable(conn, nftables.TableFamilyIPv6, nsChains6(d.cfg.IPv6Prefix))
}

// ensureDefaultRoute6 makes the namespace's only IPv6 default route the one
// via the host's end of the transit link.
func ensureDefaultRoute6(h *netlink.Handle, l netlink.Link) error {
	routes, err := dumped(func() ([]netlink.Route, error) {
		return h.RouteListFiltered(unix.AF_INET6, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	})
	if err != nil {
		return err
	}
	var defaults []netlink.Route
	for _, r := range routes {
		if r.Dst == nil || r.Dst.String() == "::/0" {
			defaults = append(defaults, r)
		}
	}
	via := hostTransitIPv6.AsSlice()
	if len(defaults) == 1 && defaults[0].LinkIndex == l.Attrs().Index && net.IP.Equal(defaults[0].Gw, via) {
		return nil
	}
	for _, r := range defaults {
		if err := h.RouteDel(&r); err != nil {
			return err
		}
	}
	return h.RouteAdd(&netlink.Route{LinkIndex: l.Attrs().Index, Gw: via, Dst: ipNet(netip.MustParsePrefix("::/0")), Table: unix.RT_TABLE_MAIN})
}

// syncIPv6 routes each tap's IPv6 address into its network, from the root
// namespace to its veth and in the namespace to its bridge, and has the
// uplink answer neighbour solicitations for it; every other such route and
// proxy entry goes. All of it is derived from the taps' aliases. An address
// two taps hold, as EnsureInterface refuses, goes to neither. Without a
// prefix, the root namespace's routes into networks go, and proxy entries
// are left: nothing tells the driver's from others.
func (d *Driver) syncIPv6() error {
	all, err := taps()
	if err != nil {
		return err
	}
	byNet := map[string][]netip.Addr{}
	macs := map[string]map[netip.Addr]net.HardwareAddr{}
	var addrs []netip.Addr
	if d.cfg.IPv6Prefix.IsValid() {
		holders := map[netip.Addr]int{}
		for _, t := range all {
			if t.ok {
				holders[t.meta.IPv6]++
			}
		}
		for _, t := range all {
			mac, err := net.ParseMAC(t.meta.MAC)
			if t.ok && err == nil && d.cfg.IPv6Prefix.Contains(t.meta.IPv6) && holders[t.meta.IPv6] == 1 {
				byNet[t.network] = append(byNet[t.network], t.meta.IPv6)
				addrs = append(addrs, t.meta.IPv6)
				if macs[t.network] == nil {
					macs[t.network] = map[netip.Addr]net.HardwareAddr{}
				}
				macs[t.network][t.meta.IPv6] = mac
			}
		}
	}
	links, err := d.root.LinkList()
	if err != nil {
		return err
	}
	for _, l := range links {
		if uid, ok := decodeVeth(l.Attrs().Alias); ok && isVeth(l) {
			if err := ensureRoutes6(d.root, l, byNet[uid], nsTransitIPv6.AsSlice()); err != nil {
				return fmt.Errorf("network %s: routes into it: %w", uid, err)
			}
		}
	}
	if !d.cfg.IPv6Prefix.IsValid() {
		return nil
	}
	uids, err := namespaces()
	if err != nil {
		return err
	}
	for _, uid := range uids {
		if err := d.syncNamespaceRoutes6(uid, byNet[uid], macs[uid]); err != nil {
			return fmt.Errorf("network %s: %w", uid, err)
		}
	}
	return d.ensureProxies(addrs)
}

func (d *Driver) syncNamespaceRoutes6(uid string, addrs []netip.Addr, macs map[netip.Addr]net.HardwareAddr) error {
	ns, ok, err := openNS(uid)
	if err != nil || !ok {
		return err
	}
	defer func() { _ = ns.Close() }()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return err
	}
	defer h.Close()
	br, err := linkByName(h, nsBridge)
	if err != nil || br == nil {
		return err
	}
	if v, _ := sysctlIn(ns, "net.ipv6.conf."+nsBridge+".disable_ipv6"); v != "0" {
		return nil // not made with IPv6 yet; EnsureNetwork routes it
	}
	if err := ensureRoutes6(h, br, addrs, nil); err != nil {
		return err
	}
	return d.ensureNeighbours6(h, br, macs)
}

// ensureNeighbours6 makes the bridge's neighbour entries for the prefix
// exactly a permanent one for each tap's address, at its MAC: the namespace
// then sends to the tap holding an address now, the moment an address
// moves to another interface of the network, never to one that held it
// before and answered for it, and asks no guest for its address.
func (d *Driver) ensureNeighbours6(h *netlink.Handle, br netlink.Link, macs map[netip.Addr]net.HardwareAddr) error {
	have, err := dumped(func() ([]netlink.Neigh, error) { return h.NeighList(br.Attrs().Index, unix.AF_INET6) })
	if err != nil {
		return err
	}
	kept := map[netip.Addr]bool{}
	for _, n := range have {
		a, ok := netip.AddrFromSlice(n.IP)
		if !ok || !d.cfg.IPv6Prefix.Contains(a.Unmap()) || n.Flags&netlink.NTF_PROXY != 0 {
			continue
		}
		a = a.Unmap()
		if mac, want := macs[a]; want && n.State == netlink.NUD_PERMANENT && bytes.Equal(n.HardwareAddr, mac) {
			kept[a] = true
			continue
		}
		if err := h.NeighDel(&n); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("neighbour entry for %s: %w", a, err)
		}
	}
	for a, mac := range macs {
		if kept[a] {
			continue
		}
		if err := h.NeighSet(&netlink.Neigh{LinkIndex: br.Attrs().Index, Family: unix.AF_INET6, State: netlink.NUD_PERMANENT,
			IP: a.AsSlice(), HardwareAddr: mac}); err != nil {
			return fmt.Errorf("neighbour entry for %s: %w", a, err)
		}
	}
	return nil
}

// sysctlIn reads a sysctl of namespace ns.
func sysctlIn(ns netns.NsHandle, key string) (string, error) {
	var v string
	err := inNS(ns, func() (err error) {
		v, err = sysctl(key)
		return err
	})
	return v, err
}

// ensureRoutes6 makes the /128 routes by link l exactly one to each of
// addrs, via gw, or on the link if gw is nil.
func ensureRoutes6(h *netlink.Handle, l netlink.Link, addrs []netip.Addr, gw net.IP) error {
	routes, err := dumped(func() ([]netlink.Route, error) {
		return h.RouteListFiltered(unix.AF_INET6, &netlink.Route{LinkIndex: l.Attrs().Index, Table: unix.RT_TABLE_MAIN},
			netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE)
	})
	if err != nil {
		return err
	}
	have := map[netip.Addr]bool{}
	for _, r := range routes {
		p, ok := netip.Prefix{}, false
		if r.Dst != nil {
			p, ok = prefixOf(r.Dst)
		}
		if !ok || p.Bits() != 128 {
			continue
		}
		if slices.Contains(addrs, p.Addr()) && net.IP.Equal(r.Gw, gw) && !have[p.Addr()] {
			have[p.Addr()] = true
			continue
		}
		if err := h.RouteDel(&r); err != nil && !errors.Is(err, unix.ESRCH) {
			return err
		}
	}
	for _, a := range addrs {
		if have[a] {
			continue
		}
		r := &netlink.Route{LinkIndex: l.Attrs().Index, Dst: ipNet(netip.PrefixFrom(a, 128)), Gw: gw, Table: unix.RT_TABLE_MAIN}
		if gw == nil {
			r.Scope = netlink.SCOPE_LINK
		}
		if err := h.RouteReplace(r); err != nil {
			return fmt.Errorf("route to %s: %w", a, err)
		}
	}
	return nil
}

// ensureProxies makes the uplink's proxy NDP entries of the prefix exactly
// one for each of addrs.
func (d *Driver) ensureProxies(addrs []netip.Addr) error {
	up, err := d.root.LinkByName(d.cfg.Uplink)
	if err != nil {
		return fmt.Errorf("uplink %s: %w", d.cfg.Uplink, err)
	}
	have, err := d.proxies(up)
	if err != nil {
		return err
	}
	for a := range have {
		if !slices.Contains(addrs, a) {
			if err := d.root.NeighDel(proxyEntry(up, a)); err != nil && !errors.Is(err, unix.ENOENT) {
				return fmt.Errorf("proxy entry for %s: %w", a, err)
			}
		}
	}
	for _, a := range addrs {
		if !have[a] {
			if err := d.root.NeighAdd(proxyEntry(up, a)); err != nil && !errors.Is(err, unix.EEXIST) {
				return fmt.Errorf("proxy entry for %s: %w", a, err)
			}
		}
	}
	return nil
}

func proxyEntry(l netlink.Link, a netip.Addr) *netlink.Neigh {
	return &netlink.Neigh{LinkIndex: l.Attrs().Index, Family: unix.AF_INET6, Flags: netlink.NTF_PROXY, IP: a.AsSlice()}
}

// proxies are the uplink's proxy NDP entries of the prefix, every one of
// which is the driver's.
func (d *Driver) proxies(up netlink.Link) (map[netip.Addr]bool, error) {
	ns, err := dumped(func() ([]netlink.Neigh, error) { return d.root.NeighProxyList(up.Attrs().Index, unix.AF_INET6) })
	if err != nil {
		return nil, fmt.Errorf("listing proxy entries of %s: %w", d.cfg.Uplink, err)
	}
	have := map[netip.Addr]bool{}
	for _, n := range ns {
		if a, ok := netip.AddrFromSlice(n.IP); ok && d.cfg.IPv6Prefix.Contains(a.Unmap()) {
			have[a.Unmap()] = true
		}
	}
	return have, nil
}

// proxiesDrifted says whether the uplink's proxy entries differ from the
// taps' addresses.
func (d *Driver) proxiesDrifted() (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	all, err := taps()
	if err != nil {
		return false, err
	}
	want := map[netip.Addr]bool{}
	for _, t := range all {
		if t.ok && d.cfg.IPv6Prefix.Contains(t.meta.IPv6) {
			want[t.meta.IPv6] = true
		}
	}
	up, err := d.root.LinkByName(d.cfg.Uplink)
	if err != nil {
		return false, err
	}
	have, err := d.proxies(up)
	if err != nil {
		return false, err
	}
	if len(have) != len(want) {
		return true, nil
	}
	for a := range want {
		if !have[a] {
			return true, nil
		}
	}
	return false, nil
}

// pollProxies signals Changes when the proxy entries drift, until stop.
func (d *Driver) pollProxies(stop <-chan struct{}) {
	t := time.NewTicker(proxyPoll)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if drifted, err := d.proxiesDrifted(); err != nil || drifted {
				d.signal()
			}
		}
	}
}

// uplinkHolds says whether the uplink has address a, which no tap may take.
func (d *Driver) uplinkHolds(a netip.Addr) (bool, error) {
	up, err := d.root.LinkByName(d.cfg.Uplink)
	if err != nil {
		return false, fmt.Errorf("uplink %s: %w", d.cfg.Uplink, err)
	}
	addrs, err := dumped(func() ([]netlink.Addr, error) { return d.root.AddrList(up, unix.AF_INET6) })
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(addrs, func(x netlink.Addr) bool { return net.IP.Equal(x.IP, a.AsSlice()) }), nil
}

// dumped lists with list, again while the kernel says the dump was
// interrupted by a change, as it is while it sets a link's IPv6 up.
func dumped[T any](list func() ([]T, error)) ([]T, error) {
	for range 10 {
		got, err := list()
		if !errors.Is(err, netlink.ErrDumpInterrupted) {
			return got, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return list()
}

// rootChains6 is the root namespace's ip6 table, seven rules whose number
// does not depend on how many networks there are;
// docs/decisions/networking.md lists them.
func rootChains6(uplink string, prefix netip.Prefix) []chainSpec {
	drop := []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}}
	accept := []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}
	fromNet := matchIfnamePrefix(expr.MetaKeyIIFNAME, vethPrefix)
	toNet := matchIfnamePrefix(expr.MetaKeyOIFNAME, vethPrefix)
	return []chainSpec{
		{
			name: "prerouting", typ: nftables.ChainTypeFilter, hook: *nftables.ChainHookPrerouting, prio: *nftables.ChainPriorityRaw,
			rules: []ruleSpec{{
				says:  "a network sends only from addresses routed back to it",
				exprs: slices.Concat(fromNet, dropUnlessRoutedBack()),
			}},
		},
		{
			name: "forward", typ: nftables.ChainTypeFilter, hook: *nftables.ChainHookForward, prio: *nftables.ChainPriorityFilter,
			rules: []ruleSpec{{
				says:  "no mail from a network",
				exprs: slices.Concat(fromNet, matchL4(unix.IPPROTO_TCP), matchDport(25, 25), drop),
			}, {
				says:  fmt.Sprintf("a network reaches only %s and other networks", uplink),
				exprs: slices.Concat(fromNet, matchIfnameNot(expr.MetaKeyOIFNAME, uplink), ifnameNotPrefix(expr.MetaKeyOIFNAME, vethPrefix), drop),
			}, {
				says:  fmt.Sprintf("only %s into a network", prefix),
				exprs: slices.Concat(toNet, matchDaddr6Not(prefix), drop),
			}, {
				// What comes in by the uplink or from another network is
				// routed only to a live address, and its tap's security
				// groups decide.
				says: fmt.Sprintf("only replies into a network but from %s and other networks", uplink),
				exprs: slices.Concat(toNet, matchIfnameNot(expr.MetaKeyIIFNAME, uplink), ifnameNotPrefix(expr.MetaKeyIIFNAME, vethPrefix),
					dropUnlessReply()),
			}},
		},
		{
			name: "input", typ: nftables.ChainTypeFilter, hook: *nftables.ChainHookInput, prio: *nftables.ChainPriorityFilter,
			rules: []ruleSpec{{
				says:  "neighbour solicitations from a network",
				exprs: slices.Concat(fromNet, matchICMPv6Type(135), accept),
			}, {
				says:  "neighbour advertisements from a network",
				exprs: slices.Concat(fromNet, matchICMPv6Type(136), accept),
			}, {
				says:  "only replies from a network into the host",
				exprs: slices.Concat(fromNet, dropUnlessReply()),
			}},
		},
	}
}

// nsChains6 is a network namespace's ip6 table: only the prefix and
// link-local addresses from the bridge, nothing by a tap's own stack, and
// nothing back out of the uplink it came in by.
func nsChains6(prefix netip.Prefix) []chainSpec {
	drop := []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}}
	return []chainSpec{{
		name: "prerouting", typ: nftables.ChainTypeFilter, hook: *nftables.ChainHookPrerouting, prio: *nftables.ChainPriorityRaw,
		rules: []ruleSpec{{
			says: fmt.Sprintf("only %s and link-local from %s", prefix, nsBridge),
			exprs: slices.Concat(matchIfname(expr.MetaKeyIIFNAME, nsBridge), matchSaddr6Not(prefix), matchSaddr6Not(linkLocal),
				matchSaddr6Not(netip.MustParsePrefix("::/128")), drop),
		}, {
			says:  "nothing in by a tap's own stack",
			exprs: append(matchIfnamePrefix(expr.MetaKeyIIFNAME, tapPrefix), &expr.Verdict{Kind: expr.VerdictDrop}),
		}},
	}, {
		name: "forward", typ: nftables.ChainTypeFilter, hook: *nftables.ChainHookForward, prio: *nftables.ChainPriorityFilter,
		rules: []ruleSpec{{
			says:  fmt.Sprintf("nothing back out of %s", nsUplink),
			exprs: slices.Concat(matchIfname(expr.MetaKeyIIFNAME, nsUplink), matchIfname(expr.MetaKeyOIFNAME, nsUplink), drop),
		}},
	}}
}

// addr6Is matches an IPv6 address at offset of the network header in p,
// with op.
func addr6Is(offset uint32, p netip.Prefix, op expr.CmpOp) []expr.Any {
	a := p.Masked().Addr().As16()
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: offset, Len: 16},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 16, Mask: net.CIDRMask(p.Bits(), 128), Xor: make([]byte, 16)},
		&expr.Cmp{Op: op, Register: 1, Data: a[:]},
	}
}

func matchSaddr6(p netip.Prefix) []expr.Any    { return addr6Is(8, p, expr.CmpOpEq) }
func matchSaddr6Not(p netip.Prefix) []expr.Any { return addr6Is(8, p, expr.CmpOpNeq) }
func matchDaddr6(p netip.Prefix) []expr.Any    { return addr6Is(24, p, expr.CmpOpEq) }
func matchDaddr6Not(p netip.Prefix) []expr.Any { return addr6Is(24, p, expr.CmpOpNeq) }

// matchICMPv6Type matches ICMPv6 of type t.
func matchICMPv6Type(t byte) []expr.Any {
	return slices.Concat(matchL4(unix.IPPROTO_ICMPV6), []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{t}},
	})
}

// matchICMPv6Types matches ICMPv6 of a type from through to.
func matchICMPv6Types(from, to byte) []expr.Any {
	return slices.Concat(matchL4(unix.IPPROTO_ICMPV6), []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 1},
		&expr.Range{Op: expr.CmpOpEq, Register: 1, FromData: []byte{from}, ToData: []byte{to}},
	})
}

// isOursFamily says whether a table family is one the driver writes.
func isOursFamily(f nftables.TableFamily) bool {
	return f == nftables.TableFamilyIPv4 || f == nftables.TableFamilyIPv6 || f == nftables.TableFamilyBridge
}
