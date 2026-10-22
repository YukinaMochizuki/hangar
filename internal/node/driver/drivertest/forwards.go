package drivertest

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// forwardNets are two networks of one range, as two projects' may be.
var forwardNets = []driver.NetworkSpec{
	{UID: "fwd-a", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.0/31")},
	{UID: "fwd-b", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.2/31")},
}

func forwardTo(n driver.NetworkSpec, proto driver.ForwardProtocol, port uint16, to string) driver.Forward {
	return driver.Forward{Protocol: proto, Port: port, NetworkUID: n.UID, To: netip.MustParseAddrPort(to)}
}

// forwardsInForce reads the host's forwards back.
func forwardsInForce(t *testing.T, h Host) []driver.Forward {
	t.Helper()
	fs, err := h.Network.Forwards(t.Context())
	must(t, err)
	return fs
}

// wantForwards fails t unless the host forwards exactly want, which is in
// the order Forwards lists: by protocol, then port.
func wantForwards(t *testing.T, h Host, what string, want ...driver.Forward) {
	t.Helper()
	if got := forwardsInForce(t, h); !slices.Equal(got, want) {
		t.Errorf("%s: the host forwards %+v, want %+v", what, got, want)
	}
}

// portForwards: a set of forwards is taken whole, read back as ensured,
// ensured again unchanged, and shrunk; a set the host cannot hold is
// refused whole, leaving what it forwards as it was; forwards of a network
// not on the host are left out, and a network's go with it.
func portForwards(t *testing.T, h Host) {
	ctx := t.Context()
	a, b := forwardNets[0], forwardNets[1]
	for _, n := range forwardNets {
		must(t, h.Network.EnsureNetwork(ctx, n))
	}
	wantForwards(t, h, "before any")
	web := forwardTo(a, driver.TCP, 10080, "10.0.0.2:8080")
	dns := forwardTo(b, driver.UDP, 10053, "10.0.0.2:53")
	sameTCP := forwardTo(b, driver.TCP, 10053, "10.0.0.3:5353")
	elsewhere := forwardTo(driver.NetworkSpec{UID: "fwd-absent"}, driver.TCP, 10090, "10.0.0.4:80")
	must(t, h.Network.EnsureForwards(ctx, []driver.Forward{dns, web, sameTCP, elsewhere}))
	// The same port of the other protocol is another forward.
	wantForwards(t, h, "ensured", sameTCP, web, dns)

	// Ensured again, in another order, nothing changes.
	must(t, h.Network.EnsureForwards(ctx, []driver.Forward{web, sameTCP, dns}))
	wantForwards(t, h, "ensured again", sameTCP, web, dns)

	with := func(f driver.Forward, change func(*driver.Forward)) driver.Forward {
		change(&f)
		return f
	}
	moved := with(web, func(f *driver.Forward) { f.To = netip.MustParseAddrPort("10.0.0.9:9090") })
	for what, bad := range map[string]driver.Forward{
		"a port the networks leave from": with(web, func(f *driver.Forward) { f.Port = driver.SharedPortsFrom }),
		"the last port":                  with(web, func(f *driver.Forward) { f.Port = 65535 }),
		"port 0":                         with(web, func(f *driver.Forward) { f.Port = 0 }),
		"no protocol":                    with(web, func(f *driver.Forward) { f.Protocol = 0 }),
		"an IPv6 target":                 with(web, func(f *driver.Forward) { f.To = netip.MustParseAddrPort("[2001:db8::2]:22") }),
		"target port 0":                  with(web, func(f *driver.Forward) { f.To = netip.MustParseAddrPort("10.0.0.2:0") }),
		"no target":                      with(web, func(f *driver.Forward) { f.To = netip.AddrPort{} }),
		"a network uid no network has":   with(web, func(f *driver.Forward) { f.NetworkUID = "../fwd-a" }),
		"one port of a protocol twice":   with(dns, func(f *driver.Forward) { f.NetworkUID = a.UID }),
	} {
		// The set is refused whole: the forward it moves stays where it was.
		if err := h.Network.EnsureForwards(ctx, []driver.Forward{moved, dns, bad}); err == nil {
			t.Errorf("a set with %s was taken: %+v", what, bad)
		}
		wantForwards(t, h, "after a set with "+what+" was refused", sameTCP, web, dns)
	}

	// Moved, and shrunk.
	must(t, h.Network.EnsureForwards(ctx, []driver.Forward{moved, dns}))
	wantForwards(t, h, "one moved and one removed", moved, dns)
	must(t, h.Network.EnsureForwards(ctx, []driver.Forward{dns}))
	wantForwards(t, h, "one left", dns)

	// A network deleted takes its forwards with it.
	must(t, h.Network.EnsureForwards(ctx, []driver.Forward{web, dns}))
	must(t, h.Network.DeleteNetwork(ctx, b.UID))
	wantForwards(t, h, "after its network went", web)

	must(t, h.Network.EnsureForwards(ctx, nil))
	wantForwards(t, h, "none")
	must(t, h.Network.DeleteNetwork(ctx, a.UID))
}

// noPortForwards: a host without a shared address refuses any forward,
// takes an empty set, and forwards nothing.
func noPortForwards(t *testing.T, h Host) {
	ctx := t.Context()
	a := forwardNets[0]
	must(t, h.Network.EnsureNetwork(ctx, a))
	if err := h.Network.EnsureForwards(ctx, []driver.Forward{forwardTo(a, driver.TCP, 10080, "10.0.0.2:8080")}); err == nil {
		t.Error("a host without a shared address took a forward")
	}
	must(t, h.Network.EnsureForwards(ctx, nil))
	wantForwards(t, h, "without a shared address")
	must(t, h.Network.DeleteNetwork(ctx, a.UID))
}
