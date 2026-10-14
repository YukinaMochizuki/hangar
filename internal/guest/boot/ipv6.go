package boot

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

// IPv6 is the public IPv6 the platform gave the guest's interfaces, at boot
// and since, which Set changes while the container runs. What else the
// interfaces hold, as their link-local addresses, it leaves alone.
type IPv6 struct {
	mu  sync.Mutex
	ops linkOps
	// given is each interface's address and default route as last set, by
	// MAC.
	given map[string]*guestv1.Interface
}

// linkOps are the netlink calls Set makes.
type linkOps interface {
	LinkList() ([]netlink.Link, error)
	AddrReplace(link netlink.Link, addr *netlink.Addr) error
	AddrDel(link netlink.Link, addr *netlink.Addr) error
	RouteReplace(route *netlink.Route) error
	RouteDel(route *netlink.Route) error
}

// netlinkOps are linkOps on the guest's own network namespace.
type netlinkOps struct{}

func (netlinkOps) LinkList() ([]netlink.Link, error) { return netlink.LinkList() }
func (netlinkOps) AddrReplace(l netlink.Link, a *netlink.Addr) error {
	return netlink.AddrReplace(l, a)
}
func (netlinkOps) AddrDel(l netlink.Link, a *netlink.Addr) error { return netlink.AddrDel(l, a) }
func (netlinkOps) RouteReplace(r *netlink.Route) error           { return netlink.RouteReplace(r) }
func (netlinkOps) RouteDel(r *netlink.Route) error               { return netlink.RouteDel(r) }

// NewIPv6 starts from what the boot disk gave the interfaces, which Network
// has set.
func NewIPv6(ifaces []*guestv1.Interface) *IPv6 {
	return newIPv6(netlinkOps{}, ifaces)
}

func newIPv6(ops linkOps, ifaces []*guestv1.Interface) *IPv6 {
	v := &IPv6{ops: ops, given: map[string]*guestv1.Interface{}}
	for _, i := range ifaces {
		if mac, err := net.ParseMAC(i.GetMac()); err == nil {
			v.given[mac.String()] = &guestv1.Interface{Ipv6: i.GetIpv6(), Ipv6Gateway: i.GetIpv6Gateway()}
		}
	}
	return v
}

// Set makes each interface's public address and IPv6 default route what
// want says: the address it held taken away, the new one added, usable at
// once, as at boot. An interface want does not name is left as it is. A
// request with an address other than an IPv6 /128, a gateway other than an
// IPv6 address, or more than one default route, is refused whole.
func (v *IPv6) Set(want []*guestv1.Interface) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	routes := 0
	for _, w := range want {
		if a := w.GetIpv6(); a != "" {
			if p, err := netip.ParsePrefix(a); err != nil || !p.Addr().Is6() || p.Addr().Is4In6() || p.Bits() != 128 {
				return fmt.Errorf("interface %s: %q is no IPv6 /128", w.GetMac(), a)
			}
		}
		if g := w.GetIpv6Gateway(); g != "" {
			if a, err := netip.ParseAddr(g); err != nil || !a.Is6() || a.Is4In6() {
				return fmt.Errorf("interface %s: IPv6 gateway %q", w.GetMac(), g)
			}
			routes++
		}
	}
	if routes > 1 {
		return fmt.Errorf("%d interfaces have an IPv6 default route; one may", routes)
	}
	links, err := v.ops.LinkList()
	if err != nil {
		return err
	}
	for _, w := range want {
		if _, err := linkByMAC(links, w.GetMac()); err != nil {
			return err
		}
	}
	for _, w := range want {
		link, _ := linkByMAC(links, w.GetMac())
		mac := link.Attrs().HardwareAddr.String()
		had := v.given[mac]
		if had == nil {
			had = &guestv1.Interface{}
		}
		if had.GetIpv6() != w.GetIpv6() {
			if w.GetIpv6() != "" {
				a, _ := netlink.ParseAddr(w.GetIpv6())
				a.Flags |= unix.IFA_F_NODAD
				if err := v.ops.AddrReplace(link, a); err != nil {
					return fmt.Errorf("interface %s: %w", mac, err)
				}
			}
			if had.GetIpv6() != "" {
				if a, err := netlink.ParseAddr(had.GetIpv6()); err == nil {
					if err := v.ops.AddrDel(link, a); err != nil && !errors.Is(err, unix.EADDRNOTAVAIL) {
						return fmt.Errorf("interface %s: %w", mac, err)
					}
				}
			}
		}
		if had.GetIpv6Gateway() != w.GetIpv6Gateway() {
			if w.GetIpv6Gateway() != "" {
				gw := net.ParseIP(w.GetIpv6Gateway())
				if err := v.ops.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: defaultIPv6(), Gw: gw}); err != nil {
					return fmt.Errorf("interface %s: IPv6 default route: %w", mac, err)
				}
			} else if gw := net.ParseIP(had.GetIpv6Gateway()); gw != nil {
				_ = v.ops.RouteDel(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: defaultIPv6(), Gw: gw})
			}
		}
		v.given[mac] = &guestv1.Interface{Ipv6: w.GetIpv6(), Ipv6Gateway: w.GetIpv6Gateway()}
	}
	return nil
}

func defaultIPv6() *net.IPNet { return &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)} }
