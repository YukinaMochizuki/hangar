package boot

import (
	"net"
	"slices"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

// recordingOps records Set's netlink calls, on links it lists.
type recordingOps struct {
	links []netlink.Link
	calls []string
	// delErr is what AddrDel fails with.
	delErr error
}

func (r *recordingOps) LinkList() ([]netlink.Link, error) { return r.links, nil }
func (r *recordingOps) AddrReplace(l netlink.Link, a *netlink.Addr) error {
	nodad := ""
	if a.Flags&unix.IFA_F_NODAD != 0 {
		nodad = " nodad"
	}
	r.calls = append(r.calls, "add "+l.Attrs().Name+" "+a.IPNet.String()+nodad)
	return nil
}
func (r *recordingOps) AddrDel(l netlink.Link, a *netlink.Addr) error {
	r.calls = append(r.calls, "del "+l.Attrs().Name+" "+a.IPNet.String())
	return r.delErr
}
func (r *recordingOps) RouteReplace(rt *netlink.Route) error {
	r.calls = append(r.calls, "route "+rt.Dst.String()+" via "+rt.Gw.String())
	return nil
}
func (r *recordingOps) RouteDel(rt *netlink.Route) error {
	r.calls = append(r.calls, "unroute "+rt.Dst.String()+" via "+rt.Gw.String())
	return nil
}

func link(name, mac string, index int) netlink.Link {
	hw, _ := net.ParseMAC(mac)
	return &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: name, HardwareAddr: hw, Index: index}}
}

const (
	mac0 = "02:00:00:00:00:01"
	mac1 = "02:00:00:00:00:02"
)

func iface(mac, addr, gw string) *guestv1.Interface {
	return &guestv1.Interface{Mac: mac, Ipv6: addr, Ipv6Gateway: gw}
}

func TestSetMovesAnAddressAndItsRoute(t *testing.T) {
	ops := &recordingOps{links: []netlink.Link{link("eth0", mac0, 2), link("eth1", mac1, 3)}}
	// As the boot disk gave them: eth0 an address and the default route.
	v := newIPv6(ops, []*guestv1.Interface{iface(mac0, "2001:db8::a/128", "fe80::1"), iface(mac1, "", "")})
	steps := []struct {
		name  string
		want  []*guestv1.Interface
		calls []string
	}{
		{"the same as the boot disk's", []*guestv1.Interface{iface(mac0, "2001:db8::a/128", "fe80::1")}, nil},
		// The new address first, so the interface is never without one.
		{"a moved address", []*guestv1.Interface{iface(mac0, "2001:db8::b/128", "fe80::1")},
			[]string{"add eth0 2001:db8::b/128 nodad", "del eth0 2001:db8::a/128"}},
		{"asked again", []*guestv1.Interface{iface(mac0, "2001:db8::b/128", "fe80::1")}, nil},
		{"the address and route taken away", []*guestv1.Interface{iface(mac0, "", "")},
			[]string{"del eth0 2001:db8::b/128", "unroute ::/0 via fe80::1"}},
		{"another interface given both", []*guestv1.Interface{iface(mac0, "", ""), iface(mac1, "2001:db8::c/128", "fe80::1")},
			[]string{"add eth1 2001:db8::c/128 nodad", "route ::/0 via fe80::1"}},
	}
	for _, s := range steps {
		ops.calls = nil
		if err := v.Set(s.want); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if !slices.Equal(ops.calls, s.calls) {
			t.Errorf("%s: calls %q, want %q", s.name, ops.calls, s.calls)
		}
	}
}

func TestSetRefusesABadRequestWhole(t *testing.T) {
	for name, want := range map[string][]*guestv1.Interface{
		"a /64":              {iface(mac0, "2001:db8::a/64", "")},
		"an IPv4 address":    {iface(mac0, "192.0.2.1/32", "")},
		"a mapped address":   {iface(mac0, "::ffff:192.0.2.1/128", "")},
		"no prefix length":   {iface(mac0, "2001:db8::a", "")},
		"an IPv4 gateway":    {iface(mac0, "2001:db8::a/128", "192.0.2.1")},
		"two default routes": {iface(mac0, "2001:db8::a/128", "fe80::1"), iface(mac1, "2001:db8::b/128", "fe80::1")},
		// The first interface's change is not made for the second's fault.
		"an unknown MAC": {iface(mac0, "2001:db8::a/128", "fe80::1"), iface("02:00:00:00:00:99", "2001:db8::b/128", "")},
	} {
		ops := &recordingOps{links: []netlink.Link{link("eth0", mac0, 2), link("eth1", mac1, 3)}}
		v := newIPv6(ops, nil)
		if err := v.Set(want); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if len(ops.calls) != 0 {
			t.Errorf("%s: made calls %q", name, ops.calls)
		}
	}
}

// An address already gone, as one the guest's user removed, is no failure.
func TestSetToleratesAnAddressAlreadyGone(t *testing.T) {
	ops := &recordingOps{links: []netlink.Link{link("eth0", mac0, 2)}, delErr: unix.EADDRNOTAVAIL}
	v := newIPv6(ops, []*guestv1.Interface{iface(mac0, "2001:db8::a/128", "")})
	if err := v.Set([]*guestv1.Interface{iface(mac0, "2001:db8::b/128", "")}); err != nil {
		t.Fatal(err)
	}
	ops.delErr = unix.EPERM
	if err := v.Set([]*guestv1.Interface{iface(mac0, "2001:db8::c/128", "")}); err == nil {
		t.Error("a failing delete was no failure")
	}
}
