// Package hostnet is the real network driver: each Network is a namespace
// on the host, joined to the root namespace by a veth pair on its transit
// /31, with a bridge holding its gateway and the metadata address, a
// persistent tap per machine interface bound to its MAC and address, and
// IPv4 SNAT in two layers; with an IPv6 prefix, each interface's public
// /128 is routed to it and answered for on the uplink by proxy NDP. It
// keeps no state: what it made is read back
// from links named with its Prefix, their aliases, and its nft tables, and
// it changes nothing else. Host settings it relies on, such
// as IPv4 forwarding, are deploy/host's, not the driver's. See
// docs/decisions/networking.md.
package hostnet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"github.com/google/nftables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// Config is what the driver needs to know of the host.
type Config struct {
	// Uplink is the link the host reaches the internet through.
	Uplink string
	// PublicIPv4 is the host's own address, where the networks reach the
	// front door's ports, and, without SharedIPv4, the address their
	// traffic leaves the uplink as. Unset, they reach nothing of the host's
	// over IPv4, and their traffic leaves as whichever address the uplink
	// has when a connection starts.
	PublicIPv4 netip.Addr
	// SharedIPv4 is the address the networks' traffic leaves the uplink as
	// when set, another than the host's own, so what machines do on the
	// internet is never the host's address's doing
	// (docs/decisions/networking.md, "The shared IPv4 address"). Their TCP
	// and UDP leave it from ports at and above SharedPortsFrom.
	SharedIPv4 netip.Addr
	// IPv6Prefix is the host's public /64, on the uplink's link, which
	// interfaces' IPv6 addresses are of. Unset, the networks have no IPv6.
	IPv6Prefix netip.Prefix
	// PublicIPv6 is the host's own public IPv6 address. The networks reach
	// the front door's ports there, as they do at PublicIPv4, so a machine
	// can call the platform by its public names; unset, they reach nothing
	// of the host's over IPv6.
	PublicIPv6 netip.Addr
}

// Driver is a driver.Network on this host. It must run in the root network
// namespace, as root.
type Driver struct {
	cfg  Config
	root *netlink.Handle
	// mu serializes everything that reads or changes the host, so a list
	// never sees an object half made.
	mu      sync.Mutex
	changes chan struct{}
	// watches are by network uid; "" is the root namespace's.
	watches map[string]*watch
	// stopPoll ends the polling of proxy entries.
	stopPoll chan struct{}
	// natJoins are what each FollowNAT holds, with what releases it.
	natMu    sync.Mutex
	natJoins map[*natJoin]func()
}

var _ driver.Network = (*Driver)(nil)

// New returns a driver watching the host for changes until Close.
func New(cfg Config) (*Driver, error) {
	if cfg.Uplink == "" || len(cfg.Uplink) >= unix.IFNAMSIZ || strings.HasPrefix(cfg.Uplink, Prefix) {
		return nil, fmt.Errorf("uplink %q is not a link name outside the driver's prefix %s", cfg.Uplink, Prefix)
	}
	if cfg.PublicIPv4.IsValid() && !cfg.PublicIPv4.Is4() {
		return nil, fmt.Errorf("public address %s is not IPv4", cfg.PublicIPv4)
	}
	if cfg.SharedIPv4.IsValid() && (!cfg.SharedIPv4.Is4() || cfg.SharedIPv4 == cfg.PublicIPv4) {
		return nil, fmt.Errorf("shared address %s is not an IPv4 address other than the public one", cfg.SharedIPv4)
	}
	if cfg.PublicIPv6.IsValid() && (!cfg.PublicIPv6.Is6() || cfg.PublicIPv6.Is4In6() || cfg.PublicIPv6.Zone() != "") {
		return nil, fmt.Errorf("public address %s is not IPv6", cfg.PublicIPv6)
	}
	if cfg.IPv6Prefix.IsValid() {
		if _, err := computeapi.ParseIPv6Prefix(cfg.IPv6Prefix.String()); err != nil {
			return nil, fmt.Errorf("IPv6 prefix: %w", err)
		}
	}
	root, err := netlink.NewHandle()
	if err != nil {
		return nil, err
	}
	d := &Driver{cfg: cfg, root: root, changes: make(chan struct{}, 1), watches: map[string]*watch{}, stopPoll: make(chan struct{})}
	d.mu.Lock()
	defer d.mu.Unlock()
	uids, err := namespaces()
	if err == nil {
		err = d.rewatch("")
	}
	for _, uid := range uids {
		if err == nil {
			err = d.rewatch(uid)
		}
	}
	if err != nil {
		d.closeLocked()
		return nil, err
	}
	if cfg.IPv6Prefix.IsValid() {
		go d.pollProxies(d.stopPoll)
	}
	return d, nil
}

// Close stops watching the host. What the driver made stays.
func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closeLocked()
	return nil
}

func (d *Driver) closeLocked() {
	select {
	case <-d.stopPoll:
	default:
		close(d.stopPoll)
	}
	for uid := range d.watches {
		d.stopWatch(uid)
	}
	d.root.Close()
}

func (d *Driver) Changes() <-chan struct{} { return d.changes }

func (d *Driver) EnsureNetwork(_ context.Context, n driver.NetworkSpec) error {
	if err := checkNetwork(n); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reviveNAT(n.UID)
	if v, err := sysctl("net.ipv4.ip_forward"); err != nil {
		return err
	} else if v != "1" {
		return errors.New("IPv4 forwarding is off on the host (net.ipv4.ip_forward); deploy/host turns it on")
	}
	if d.cfg.IPv6Prefix.IsValid() {
		if err := d.checkHostIPv6(); err != nil {
			return err
		}
	}
	if err := d.rewatch(""); err != nil {
		return err
	}
	if err := d.ensureRoot(); err != nil {
		return err
	}
	ns, err := ensureNS(n.UID)
	if err != nil {
		return err
	}
	defer func() { _ = ns.Close() }()
	if err := d.rewatch(n.UID); err != nil {
		return err
	}
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return err
	}
	defer h.Close()
	if err := inNS(ns, func() error { return ensureSysctls(nsSysctls) }); err != nil {
		return fmt.Errorf("network %s: sysctls: %w", n.UID, err)
	}
	if lo, err := h.LinkByName("lo"); err != nil {
		return err
	} else if err := ensureUp(h, lo); err != nil {
		return err
	}

	host, inner, err := d.ensureVeth(n.UID, ns, h)
	if err != nil {
		return fmt.Errorf("network %s: veth: %w", n.UID, err)
	}
	if err := ensureAddr(d.root, host, netip.PrefixFrom(n.Transit.Addr(), 31)); err != nil {
		return fmt.Errorf("network %s: host end of the transit link: %w", n.UID, err)
	}
	nsTransit := n.Transit.Addr().Next()
	if err := ensureAddr(h, inner, netip.PrefixFrom(nsTransit, 31)); err != nil {
		return fmt.Errorf("network %s: namespace end of the transit link: %w", n.UID, err)
	}
	if err := ensureDefaultRoute(h, inner, n.Transit.Addr()); err != nil {
		return fmt.Errorf("network %s: default route: %w", n.UID, err)
	}
	br, err := ensureBridge(h, n.UID)
	if err != nil {
		return fmt.Errorf("network %s: bridge: %w", n.UID, err)
	}
	if err := ensureAddr(h, br, netip.PrefixFrom(n.CIDR.Addr().Next(), n.CIDR.Bits()), netip.PrefixFrom(driver.MetadataAddr, 32)); err != nil {
		return fmt.Errorf("network %s: gateway: %w", n.UID, err)
	}
	if err := d.ensureNetworkIPv6(ns, h, host, inner, br); err != nil {
		return fmt.Errorf("network %s: IPv6: %w", n.UID, err)
	}
	conn, err := nftables.New(nftables.WithNetNSFd(int(ns)))
	if err != nil {
		return err
	}
	if err := ensureTable(conn, nftables.TableFamilyIPv4, nsChains(n.CIDR, nsTransit)); err != nil {
		return fmt.Errorf("network %s: %w", n.UID, err)
	}
	if err := ensureNamedTable(conn, nftables.TableFamilyINet, LimitTable, limitChains()); err != nil {
		return fmt.Errorf("network %s: %w", n.UID, err)
	}
	if err := sweepTaps(h); err != nil {
		return err
	}
	// Before any tap can join the bridge, so none is ever unfiltered.
	if err := ensureFirewallTable(ns, n.CIDR.Addr().Next(), false); err != nil {
		return fmt.Errorf("network %s: %w", n.UID, err)
	}
	if err := ensureBinding(ns, h, d.cfg.IPv6Prefix); err != nil {
		return err
	}
	return d.syncIPv6()
}

// ensureRoot makes the root namespace's table, and removes veths the driver
// did not finish making.
func (d *Driver) ensureRoot() error {
	conn, err := nftables.New()
	if err != nil {
		return err
	}
	if err := ensureTable(conn, nftables.TableFamilyIPv4, rootChains(d.cfg.Uplink, d.cfg.PublicIPv4, d.cfg.SharedIPv4)); err != nil {
		return err
	}
	if d.cfg.IPv6Prefix.IsValid() {
		err = ensureTable(conn, nftables.TableFamilyIPv6, rootChains6(d.cfg.Uplink, d.cfg.IPv6Prefix, d.cfg.PublicIPv6))
	} else {
		err = deleteTable(conn, nftables.TableFamilyIPv6)
	}
	if err != nil {
		return err
	}
	links, err := d.root.LinkList()
	if err != nil {
		return err
	}
	for _, l := range links {
		if _, ok := decodeVeth(l.Attrs().Alias); isVeth(l) && !ok {
			if err := d.root.LinkDel(l); err != nil && !isNotFound(err) {
				return err
			}
		}
	}
	return nil
}

// ensureVeth returns the network's veth pair, making it again unless both
// ends are there and joined to each other.
func (d *Driver) ensureVeth(uid string, ns netns.NsHandle, h *netlink.Handle) (host, inner netlink.Link, err error) {
	hosts, err := d.vethsOf(uid)
	if err != nil {
		return nil, nil, err
	}
	if inner, err = linkByName(h, nsUplink); err != nil {
		return nil, nil, err
	}
	if len(hosts) == 1 && inner != nil && inner.Type() == "veth" &&
		inner.Attrs().ParentIndex == hosts[0].Attrs().Index && hosts[0].Attrs().ParentIndex == inner.Attrs().Index {
		host = hosts[0]
	} else {
		for _, l := range hosts {
			if err := d.root.LinkDel(l); err != nil && !isNotFound(err) {
				return nil, nil, err
			}
		}
		if inner != nil {
			if err := h.LinkDel(inner); err != nil && !isNotFound(err) {
				return nil, nil, err
			}
		}
		if host, inner, err = d.makeVeth(uid, ns, h); err != nil {
			return nil, nil, err
		}
	}
	hostMAC, innerMAC := vethMACs(uid)
	if err := ensureMAC(d.root, host, hostMAC); err != nil {
		return nil, nil, err
	}
	if err := ensureMAC(h, inner, innerMAC); err != nil {
		return nil, nil, err
	}
	if err := ensureUp(d.root, host); err != nil {
		return nil, nil, err
	}
	return host, inner, ensureUp(h, inner)
}

func ensureMAC(h *netlink.Handle, l netlink.Link, mac net.HardwareAddr) error {
	if bytes.Equal(l.Attrs().HardwareAddr, mac) {
		return nil
	}
	return h.LinkSetHardwareAddr(l, mac)
}

func (d *Driver) makeVeth(uid string, ns netns.NsHandle, h *netlink.Handle) (host, inner netlink.Link, err error) {
	links, err := d.root.LinkList()
	if err != nil {
		return nil, nil, err
	}
	name, err := freeName(vethPrefix, uid, func(n string) bool {
		return slices.ContainsFunc(links, func(l netlink.Link) bool { return l.Attrs().Name == n })
	})
	if err != nil {
		return nil, nil, err
	}
	alias, err := encodeAlias(vethMeta{Network: uid})
	if err != nil {
		return nil, nil, err
	}
	// Made with its MAC, so udev leaves it; the end made in the namespace
	// udev does not see, and ensureVeth sets its MAC.
	hostMAC, _ := vethMACs(uid)
	v := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: name, HardwareAddr: hostMAC},
		PeerName:  nsUplink, PeerNamespace: netlink.NsFd(int(ns)),
	}
	if err := d.root.LinkAdd(v); err != nil {
		return nil, nil, fmt.Errorf("adding %s: %w", name, err)
	}
	if host, err = d.root.LinkByName(name); err == nil {
		err = d.root.LinkSetAlias(host, alias)
	}
	if err == nil {
		host, err = d.root.LinkByName(name)
	}
	if err == nil {
		inner, err = h.LinkByName(nsUplink)
	}
	if err != nil {
		_ = d.root.LinkDel(v)
		return nil, nil, err
	}
	return host, inner, nil
}

// vethsOf returns the root ends of a network's veths; there is one, unless
// something went wrong.
func (d *Driver) vethsOf(uid string) ([]netlink.Link, error) {
	links, err := d.root.LinkList()
	if err != nil {
		return nil, err
	}
	var out []netlink.Link
	for _, l := range links {
		if n, ok := decodeVeth(l.Attrs().Alias); ok && n == uid && isVeth(l) {
			out = append(out, l)
		}
	}
	return out, nil
}

// ensureBridge makes the bridge holding the gateway, with the network's MAC
// rather than one of its ports', so guests' ARP entries outlive it.
func ensureBridge(h *netlink.Handle, uid string) (netlink.Link, error) {
	br, err := linkByName(h, nsBridge)
	if err != nil {
		return nil, err
	}
	if br != nil && br.Type() != "bridge" {
		if err := h.LinkDel(br); err != nil {
			return nil, err
		}
		br = nil
	}
	mac := gatewayMAC(uid)
	if br == nil {
		if err := h.LinkAdd(&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: nsBridge, HardwareAddr: mac}}); err != nil {
			return nil, err
		}
		if br, err = h.LinkByName(nsBridge); err != nil {
			return nil, err
		}
	}
	if err := ensureMAC(h, br, mac); err != nil {
		return nil, err
	}
	if err := ensureBridgeEntry(h, br, mac); err != nil {
		return nil, err
	}
	return br, ensureUp(h, br)
}

// sweepTaps removes taps the driver did not finish making, unless something
// holds them open.
func sweepTaps(h *netlink.Handle) error {
	links, err := h.LinkList()
	if err != nil {
		return err
	}
	for _, l := range links {
		if _, ok := decodeTap(l.Attrs().Alias); !isTap(l) || ok {
			continue
		}
		if held, err := heldOpen(h, l); err != nil {
			return err
		} else if held {
			continue
		}
		if err := h.LinkDel(l); err != nil && !isNotFound(err) {
			return err
		}
	}
	return nil
}

// isVeth says whether l is a root end of a veth the driver may have made.
func isVeth(l netlink.Link) bool {
	return strings.HasPrefix(l.Attrs().Name, vethPrefix) && l.Type() == "veth"
}

// isTap says whether l is a tap the driver may have made.
func isTap(l netlink.Link) bool {
	return strings.HasPrefix(l.Attrs().Name, tapPrefix) && l.Type() == "tuntap"
}

func (d *Driver) Networks(context.Context) ([]driver.NetworkSpec, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	specs := map[string]*driver.NetworkSpec{}
	spec := func(uid string) *driver.NetworkSpec {
		if specs[uid] == nil {
			specs[uid] = &driver.NetworkSpec{UID: uid}
		}
		return specs[uid]
	}
	links, err := d.root.LinkList()
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if uid, ok := decodeVeth(l.Attrs().Alias); ok && isVeth(l) {
			s := spec(uid)
			if p, ok, err := addrOf(d.root, l); err != nil {
				return nil, err
			} else if ok {
				s.Transit = netip.PrefixFrom(p.Addr(), 31).Masked()
			}
		}
	}
	uids, err := namespaces()
	if err != nil {
		return nil, err
	}
	for _, uid := range uids {
		ns, ok, err := openNS(uid)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		s := spec(uid)
		err = func() error {
			defer func() { _ = ns.Close() }()
			h, err := netlink.NewHandleAt(ns)
			if err != nil {
				return err
			}
			defer h.Close()
			if br, err := linkByName(h, nsBridge); err != nil {
				return err
			} else if br != nil {
				if p, ok, err := addrOf(h, br); err != nil {
					return err
				} else if ok {
					s.CIDR = p.Masked()
				}
			}
			if up, err := linkByName(h, nsUplink); err != nil || up == nil || s.Transit.IsValid() {
				return err
			} else if p, ok, err := addrOf(h, up); err != nil || !ok {
				return err
			} else {
				s.Transit = netip.PrefixFrom(p.Addr(), 31).Masked()
			}
			return nil
		}()
		if err != nil {
			return nil, fmt.Errorf("network %s: %w", uid, err)
		}
	}
	out := make([]driver.NetworkSpec, 0, len(specs))
	for _, s := range specs {
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, b driver.NetworkSpec) int { return strings.Compare(a.UID, b.UID) })
	return out, nil
}

func (d *Driver) DeleteNetwork(_ context.Context, networkUID string) error {
	if checkUID(networkUID) != nil {
		return nil // no namespace or alias can name it
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopWatch(networkUID)
	// A socket in its namespace would keep the namespace alive; a deletion
	// that fails leaves the network joined as before.
	if err := d.forgettingNAT(networkUID, func() error {
		veths, err := d.vethsOf(networkUID)
		if err != nil {
			return err
		}
		for _, l := range veths {
			if err := d.root.LinkDel(l); err != nil && !isNotFound(err) {
				return err
			}
		}
		ns, ok, err := openNS(networkUID)
		if err != nil {
			return err
		}
		if ok {
			// Emptied first, so nothing is left should something else hold
			// the namespace after it is unmounted.
			err := func() error {
				defer func() { _ = ns.Close() }()
				h, err := netlink.NewHandleAt(ns)
				if err != nil {
					return err
				}
				defer h.Close()
				links, err := h.LinkList()
				if err != nil {
					return err
				}
				for _, l := range links {
					if strings.HasPrefix(l.Attrs().Name, Prefix) {
						if err := h.LinkDel(l); err != nil && !isNotFound(err) {
							return err
						}
					}
				}
				conn, err := nftables.New(nftables.WithNetNSFd(int(ns)))
				if err != nil {
					return err
				}
				if err := deleteTable(conn, nftables.TableFamilyBridge); err != nil {
					return err
				}
				if err := deleteNamedTable(conn, nftables.TableFamilyBridge, FirewallTable); err != nil {
					return err
				}
				if err := deleteNamedTable(conn, nftables.TableFamilyINet, LimitTable); err != nil {
					return err
				}
				if err := deleteNamedTable(conn, nftables.TableFamilyIPv4, ForwardTable); err != nil {
					return err
				}
				if err := deleteNamedTable(conn, nftables.TableFamilyIPv4, EndpointTable); err != nil {
					return err
				}
				if err := deleteTable(conn, nftables.TableFamilyIPv6); err != nil {
					return err
				}
				return deleteTable(conn, nftables.TableFamilyIPv4)
			}()
			if err != nil {
				return fmt.Errorf("network %s: %w", networkUID, err)
			}
		}
		return deleteNS(networkUID)
	}); err != nil {
		return err
	}
	if err := d.syncIPv6(); err != nil {
		return err
	}
	return d.deleteRootIfUnused()
}

// deleteRootIfUnused removes the root namespace's table once no network is
// left to use it.
func (d *Driver) deleteRootIfUnused() error {
	uids, err := namespaces()
	if err != nil || len(uids) > 0 {
		return err
	}
	links, err := d.root.LinkList()
	if err != nil {
		return err
	}
	for _, l := range links {
		if isVeth(l) {
			return nil
		}
	}
	conn, err := nftables.New()
	if err != nil {
		return err
	}
	if err := deleteTable(conn, nftables.TableFamilyIPv6); err != nil {
		return err
	}
	if err := deleteNamedTable(conn, nftables.TableFamilyIPv4, ForwardTable); err != nil {
		return err
	}
	if err := deleteNamedTable(conn, nftables.TableFamilyIPv4, EndpointTable); err != nil {
		return err
	}
	return deleteTable(conn, nftables.TableFamilyIPv4)
}

// tap is a tap with the tap prefix in a network's namespace.
type tap struct {
	network string
	link    netlink.Link
	// meta is valid if ok: a tap without is one the driver did not finish
	// making.
	meta tapMeta
	ok   bool
}

// taps lists every tap with the tap prefix in every network's namespace.
func taps() ([]tap, error) {
	uids, err := namespaces()
	if err != nil {
		return nil, err
	}
	var out []tap
	for _, uid := range uids {
		ns, ok, err := openNS(uid)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		links, err := func() ([]netlink.Link, error) {
			defer func() { _ = ns.Close() }()
			h, err := netlink.NewHandleAt(ns)
			if err != nil {
				return nil, err
			}
			defer h.Close()
			return h.LinkList()
		}()
		if err != nil {
			return nil, fmt.Errorf("network %s: %w", uid, err)
		}
		for _, l := range links {
			if isTap(l) {
				m, ok := decodeTap(l.Attrs().Alias)
				out = append(out, tap{network: uid, link: l, meta: m, ok: ok})
			}
		}
	}
	return out, nil
}

// heldOpen says whether something, a VM, holds a tap open: a tap has carrier
// while it is open, which only an up link shows, so a down one is set up.
func heldOpen(h *netlink.Handle, l netlink.Link) (bool, error) {
	if l.Attrs().Flags&net.FlagUp == 0 {
		if err := h.LinkSetUp(l); err != nil {
			return false, err
		}
		up, err := h.LinkByIndex(l.Attrs().Index)
		if err != nil {
			return false, err
		}
		l = up
	}
	return l.Attrs().RawFlags&unix.IFF_LOWER_UP != 0, nil
}

// refuseHeld returns an error if a VM holds any of the taps open.
func refuseHeld(ts []tap) error {
	for _, t := range ts {
		if err := withTap(t, func(h *netlink.Handle, l netlink.Link) error {
			if held, err := heldOpen(h, l); err != nil {
				return err
			} else if held {
				return fmt.Errorf("tap %s of machine %s's %s is held open, by its VM", l.Attrs().Name, t.meta.Machine, t.meta.Name)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) EnsureInterface(_ context.Context, i driver.Interface) (driver.Interface, error) {
	if err := checkInterface(i); err != nil {
		return driver.Interface{}, err
	}
	alias, err := encodeAlias(tapMetaOf(i))
	if err != nil {
		return driver.Interface{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	ns, ok, err := openNS(i.NetworkUID)
	if err != nil {
		return driver.Interface{}, err
	}
	if !ok {
		return driver.Interface{}, fmt.Errorf("machine %s's %s: network %s: %w", i.MachineUID, i.Name, i.NetworkUID, errNoNetwork)
	}
	defer func() { _ = ns.Close() }()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return driver.Interface{}, err
	}
	defer h.Close()
	br, err := linkByName(h, nsBridge)
	if err != nil {
		return driver.Interface{}, err
	}
	var cidr netip.Prefix
	if br != nil {
		p, _, err := addrOf(h, br)
		if err != nil {
			return driver.Interface{}, err
		}
		cidr = p.Masked()
	}
	if !cidr.Contains(i.Address.Addr()) {
		return driver.Interface{}, fmt.Errorf("machine %s's %s: %s is outside network %s (%s)", i.MachineUID, i.Name, i.Address, i.NetworkUID, cidr)
	}
	if i.IPv6.IsValid() {
		if !d.cfg.IPv6Prefix.Contains(i.IPv6) {
			return driver.Interface{}, fmt.Errorf("machine %s's %s: %s is not of the host's IPv6 prefix %s", i.MachineUID, i.Name, i.IPv6, d.cfg.IPv6Prefix)
		}
		if held, err := d.uplinkHolds(i.IPv6); err != nil {
			return driver.Interface{}, err
		} else if held {
			return driver.Interface{}, fmt.Errorf("machine %s's %s: %s is the host's own address", i.MachineUID, i.Name, i.IPv6)
		}
	}

	all, err := taps()
	if err != nil {
		return driver.Interface{}, err
	}
	if i.IPv6.IsValid() {
		for _, t := range all {
			if t.ok && t.meta.IPv6 == i.IPv6 && (t.meta.Machine != i.MachineUID || t.meta.Name != i.Name) {
				return driver.Interface{}, fmt.Errorf("machine %s's %s: %s is machine %s's %s's: %w",
					i.MachineUID, i.Name, i.IPv6, t.meta.Machine, t.meta.Name, driver.ErrIPv6InUse)
			}
		}
	}
	if err := macTaken(all, i); err != nil {
		return driver.Interface{}, err
	}
	// The interface's other taps, on another network, of a server's when it
	// is none or the other way round, or duplicates, go, unless a VM holds
	// one; that is refused, as deleting the interface is.
	var link netlink.Link
	var others []tap
	for _, t := range all {
		if !t.ok || t.meta.Machine != i.MachineUID || t.meta.Name != i.Name {
			continue
		}
		if t.network == i.NetworkUID && link == nil && isServerTap(t.link.Attrs().Name) == i.FileShareServer {
			link = t.link
			continue
		}
		others = append(others, t)
	}
	if err := refuseHeld(others); err != nil {
		return driver.Interface{}, err
	}
	for _, t := range others {
		if err := deleteTap(t); err != nil {
			return driver.Interface{}, err
		}
		if t.network != i.NetworkUID {
			if err := d.rebind(t.network); err != nil {
				return driver.Interface{}, err
			}
		}
	}
	if link == nil {
		if link, err = makeTap(ns, h, i, all); err != nil {
			return driver.Interface{}, err
		}
	}
	if link.Attrs().Alias != alias {
		if err := h.LinkSetAlias(link, alias); err != nil {
			return driver.Interface{}, err
		}
	}
	// Bound before it joins the bridge, so it never sends as another, and
	// behind a firewall, closed if it has no other.
	if err := ensureBinding(ns, h, d.cfg.IPv6Prefix); err != nil {
		return driver.Interface{}, err
	}
	gateway, err := gatewayOf(h)
	if err != nil {
		return driver.Interface{}, fmt.Errorf("network %s: %w", i.NetworkUID, err)
	}
	if err := ensureFirewallTable(ns, gateway, i.FileShareServer); err != nil {
		return driver.Interface{}, err
	}
	// A tap joins the bridge down and comes up once its port is set. An up
	// port with carrier forwards at once, and would be flooded until then.
	if link.Attrs().MasterIndex != br.Attrs().Index {
		if link.Attrs().Flags&net.FlagUp != 0 {
			if err := h.LinkSetDown(link); err != nil {
				return driver.Interface{}, err
			}
		}
		if err := h.LinkSetMasterByIndex(link, br.Attrs().Index); err != nil {
			return driver.Interface{}, err
		}
		if link, err = h.LinkByIndex(link.Attrs().Index); err != nil {
			return driver.Interface{}, err
		}
	}
	ports, err := openPortSocket(ns)
	if err != nil {
		return driver.Interface{}, err
	}
	defer ports.Close()
	mac, _ := net.ParseMAC(i.MAC)
	if err := ensurePort(h, ports, br, link, mac); err != nil {
		return driver.Interface{}, fmt.Errorf("machine %s's %s: %w", i.MachineUID, i.Name, err)
	}
	if err := ensureUp(h, link); err != nil {
		return driver.Interface{}, err
	}
	if err := d.syncIPv6(); err != nil {
		return driver.Interface{}, err
	}
	i.Device = link.Attrs().Name
	return i, nil
}

// makeTap makes a persistent tap for i, named so no other tap on the host
// has its name, and gives it i's alias.
func makeTap(ns netns.NsHandle, h *netlink.Handle, i driver.Interface, all []tap) (netlink.Link, error) {
	links, err := h.LinkList()
	if err != nil {
		return nil, err
	}
	name, err := freeName(tapPrefixOf(i), i.MachineUID+"/"+i.Name, func(n string) bool {
		return slices.ContainsFunc(all, func(t tap) bool { return t.link.Attrs().Name == n }) ||
			slices.ContainsFunc(links, func(l netlink.Link) bool { return l.Attrs().Name == n })
	})
	if err != nil {
		return nil, err
	}
	alias, err := encodeAlias(tapMetaOf(i))
	if err != nil {
		return nil, err
	}
	// A tap is made by opening it, which happens in the namespace of the
	// thread that opens it.
	err = inNS(ns, func() error {
		return netlink.LinkAdd(&netlink.Tuntap{
			LinkAttrs: netlink.LinkAttrs{Name: name}, Mode: netlink.TUNTAP_MODE_TAP,
			Flags: netlink.TUNTAP_NO_PI | netlink.TUNTAP_VNET_HDR | netlink.TUNTAP_TUN_EXCL,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("making tap %s: %w", name, err)
	}
	link, err := h.LinkByName(name)
	if err == nil {
		err = h.LinkSetAlias(link, alias)
	}
	if err == nil {
		link, err = h.LinkByName(name)
	}
	if err != nil {
		if l, lerr := h.LinkByName(name); lerr == nil {
			_ = h.LinkDel(l)
		}
		return nil, err
	}
	return link, nil
}

func deleteTap(t tap) error {
	return withTap(t, func(h *netlink.Handle, l netlink.Link) error {
		if err := h.LinkDel(l); err != nil && !isNotFound(err) {
			return fmt.Errorf("deleting tap %s: %w", l.Attrs().Name, err)
		}
		return nil
	})
}

// withTap calls f with a handle in the tap's namespace, unless the
// namespace is gone.
func withTap(t tap, f func(h *netlink.Handle, l netlink.Link) error) error {
	ns, ok, err := openNS(t.network)
	if err != nil || !ok {
		return err
	}
	defer func() { _ = ns.Close() }()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return err
	}
	defer h.Close()
	return f(h, t.link)
}

func (d *Driver) Interfaces(context.Context) ([]driver.Interface, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	all, err := taps()
	if err != nil {
		return nil, err
	}
	var out []driver.Interface
	for _, t := range all {
		if t.ok {
			out = append(out, t.meta.iface(t.network, t.link.Attrs().Name))
		}
	}
	return out, nil
}

func (d *Driver) DeleteInterfaces(_ context.Context, machineUID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	all, err := taps()
	if err != nil {
		return err
	}
	var mine []tap
	for _, t := range all {
		if t.ok && t.meta.Machine == machineUID {
			mine = append(mine, t)
		}
	}
	if err := refuseHeld(mine); err != nil {
		return err
	}
	for _, t := range mine {
		if err := deleteTap(t); err != nil {
			return err
		}
	}
	// What bound and routed them goes too.
	done := map[string]bool{}
	for _, t := range mine {
		if !done[t.network] {
			done[t.network] = true
			if err := d.rebind(t.network); err != nil {
				return err
			}
		}
	}
	if len(mine) == 0 {
		return nil
	}
	return d.syncIPv6()
}

// rebind binds the taps of a network's namespace as they are now.
func (d *Driver) rebind(uid string) error {
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
	return ensureBinding(ns, h, d.cfg.IPv6Prefix)
}

// linkByName returns the link named name, or nil if there is none.
func linkByName(h *netlink.Handle, name string) (netlink.Link, error) {
	l, err := h.LinkByName(name)
	if isNotFound(err) {
		return nil, nil
	}
	return l, err
}

func isNotFound(err error) bool {
	var nf netlink.LinkNotFoundError
	return errors.As(err, &nf) || errors.Is(err, unix.ENODEV) || errors.Is(err, unix.ENOENT)
}

func ensureUp(h *netlink.Handle, l netlink.Link) error {
	if l.Attrs().Flags&net.FlagUp != 0 {
		return nil
	}
	return h.LinkSetUp(l)
}

// addrOf returns the first IPv4 address of a link other than the metadata
// service's and network endpoints', which a bridge holds beside its
// gateway.
func addrOf(h *netlink.Handle, l netlink.Link) (netip.Prefix, bool, error) {
	addrs, err := h.AddrList(l, unix.AF_INET)
	if err != nil {
		return netip.Prefix{}, false, err
	}
	for _, a := range addrs {
		if p, ok := prefixOf(a.IPNet); ok && p.Addr() != driver.MetadataAddr && a.Label != endpointLabel {
			return p, true, nil
		}
	}
	return netip.Prefix{}, false, nil
}

func prefixOf(n *net.IPNet) (netip.Prefix, bool) {
	a, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, _ := n.Mask.Size()
	return netip.PrefixFrom(a.Unmap(), ones), true
}

func ipNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}

// ensureAddr makes want and more the link's only IPv4 addresses but
// network endpoints', which are EnsureEndpoints', with want's route to the
// rest of its prefix, which flushing routes takes away and nothing gives
// back.
func ensureAddr(h *netlink.Handle, l netlink.Link, want netip.Prefix, more ...netip.Prefix) error {
	addrs, err := h.AddrList(l, unix.AF_INET)
	if err != nil {
		return err
	}
	all := append([]netip.Prefix{want}, more...)
	found := map[netip.Prefix]bool{}
	for _, a := range addrs {
		if a.Label == endpointLabel {
			continue
		}
		if p, _ := prefixOf(a.IPNet); slices.Contains(all, p) {
			found[p] = true
			continue
		}
		if err := h.AddrDel(l, &a); err != nil {
			return err
		}
	}
	for _, p := range all {
		if !found[p] {
			if err := h.AddrAdd(l, &netlink.Addr{IPNet: ipNet(p)}); err != nil {
				return err
			}
		}
	}
	filter := &netlink.Route{LinkIndex: l.Attrs().Index, Dst: ipNet(want.Masked()), Table: unix.RT_TABLE_MAIN}
	routes, err := h.RouteListFiltered(unix.AF_INET, filter, netlink.RT_FILTER_OIF|netlink.RT_FILTER_DST|netlink.RT_FILTER_TABLE)
	if err != nil || len(routes) > 0 {
		return err
	}
	return h.RouteAdd(&netlink.Route{
		LinkIndex: l.Attrs().Index, Dst: ipNet(want.Masked()), Src: want.Addr().AsSlice(),
		Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_KERNEL, Table: unix.RT_TABLE_MAIN,
	})
}

// ensureDefaultRoute makes the namespace's only IPv4 default route the one
// via the host's end of the transit link.
func ensureDefaultRoute(h *netlink.Handle, l netlink.Link, via netip.Addr) error {
	routes, err := h.RouteListFiltered(unix.AF_INET, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return err
	}
	var defaults []netlink.Route
	for _, r := range routes {
		if r.Dst == nil || r.Dst.String() == "0.0.0.0/0" {
			defaults = append(defaults, r)
		}
	}
	if len(defaults) == 1 && defaults[0].LinkIndex == l.Attrs().Index && net.IP.Equal(defaults[0].Gw, via.AsSlice()) {
		return nil
	}
	for _, r := range defaults {
		if err := h.RouteDel(&r); err != nil {
			return err
		}
	}
	return h.RouteAdd(&netlink.Route{LinkIndex: l.Attrs().Index, Gw: via.AsSlice(), Table: unix.RT_TABLE_MAIN})
}
