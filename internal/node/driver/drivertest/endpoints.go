package drivertest

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// endpointNets are three networks of one range, as a project's may be: two
// with endpoints of a service on the third.
var endpointNets = []driver.NetworkSpec{
	{UID: "ep-a", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.0/31")},
	{UID: "ep-b", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.2/31")},
	{UID: "ep-svc", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.4/31")},
}

// wantEndpoints fails t unless the host carries exactly want, which is in
// SortEndpoints' order.
func wantEndpoints(t *testing.T, h Host, what string, want ...driver.Endpoint) {
	t.Helper()
	got, err := h.Network.Endpoints(t.Context())
	must(t, err)
	if !slices.Equal(got, want) {
		t.Errorf("%s: the host carries endpoints %+v, want %+v", what, got, want)
	}
}

// networkEndpoints: a set of endpoints is taken whole, read back as ensured,
// ensured again unchanged, and shrunk; a set the host cannot hold is refused
// whole, leaving what it carries as it was; endpoints of a network not on
// the host are left out, and a network's go with it, whether it is the
// endpoint's network or its service's.
func networkEndpoints(t *testing.T, h Host) {
	ctx := t.Context()
	a, b, svc := endpointNets[0], endpointNets[1], endpointNets[2]
	for _, n := range endpointNets {
		must(t, h.Network.EnsureNetwork(ctx, n))
	}
	wantEndpoints(t, h, "before any")
	git := driver.Endpoint{
		Protocol: driver.TCP, NetworkUID: a.UID, Addr: netip.MustParseAddr("10.0.0.7"), Port: 22,
		ServiceNetworkUID: svc.UID, TransitPort: 1024, To: netip.MustParseAddrPort("10.0.0.2:22"),
	}
	// The same service from another network, at an address of the same
	// range.
	gitB := git
	gitB.NetworkUID, gitB.Addr = b.UID, netip.MustParseAddr("10.0.0.9")
	dns := driver.Endpoint{
		Protocol: driver.UDP, NetworkUID: a.UID, Addr: netip.MustParseAddr("10.0.0.8"), Port: 53,
		ServiceNetworkUID: svc.UID, TransitPort: 1024, To: netip.MustParseAddrPort("10.0.0.3:53"),
	}
	elsewhere := git
	elsewhere.NetworkUID = "ep-absent"
	must(t, h.Network.EnsureEndpoints(ctx, []driver.Endpoint{dns, gitB, git, elsewhere}))
	wantEndpoints(t, h, "ensured", git, dns, gitB)

	// Ensured again, in another order, nothing changes.
	must(t, h.Network.EnsureEndpoints(ctx, []driver.Endpoint{gitB, dns, git}))
	wantEndpoints(t, h, "ensured again", git, dns, gitB)

	with := func(e driver.Endpoint, change func(*driver.Endpoint)) driver.Endpoint {
		change(&e)
		return e
	}
	moved := with(git, func(e *driver.Endpoint) { e.To = netip.MustParseAddrPort("10.0.0.4:2222") })
	for what, bad := range map[string]driver.Endpoint{
		"no protocol":                    with(git, func(e *driver.Endpoint) { e.Protocol = 0 }),
		"port 0":                         with(git, func(e *driver.Endpoint) { e.Port = 0 }),
		"an IPv6 address":                with(git, func(e *driver.Endpoint) { e.Addr = netip.MustParseAddr("2001:db8::7") }),
		"a transit port below the range": with(git, func(e *driver.Endpoint) { e.TransitPort = driver.TransitPortsFrom - 1 }),
		"a transit port above the range": with(git, func(e *driver.Endpoint) { e.TransitPort = driver.TransitPortsThrough + 1 }),
		"an IPv6 target":                 with(git, func(e *driver.Endpoint) { e.To = netip.MustParseAddrPort("[2001:db8::2]:22") }),
		"target port 0":                  with(git, func(e *driver.Endpoint) { e.To = netip.MustParseAddrPort("10.0.0.2:0") }),
		"a network uid no network has":   with(git, func(e *driver.Endpoint) { e.NetworkUID = "../ep-a" }),
		"its own network":                with(git, func(e *driver.Endpoint) { e.ServiceNetworkUID = a.UID }),
		"an address and port twice":      with(git, func(e *driver.Endpoint) { e.TransitPort = 1025 }),
		"a transit port to two targets":  with(gitB, func(e *driver.Endpoint) { e.To = netip.MustParseAddrPort("10.0.0.5:22") }),
	} {
		// The set is refused whole: the endpoint it moves stays as it was.
		if err := h.Network.EnsureEndpoints(ctx, []driver.Endpoint{moved, dns, bad}); err == nil {
			t.Errorf("a set with %s was taken: %+v", what, bad)
		}
		wantEndpoints(t, h, "after a set with "+what+" was refused", git, dns, gitB)
	}

	// Moved, and shrunk.
	must(t, h.Network.EnsureEndpoints(ctx, []driver.Endpoint{moved, dns}))
	wantEndpoints(t, h, "one moved and one removed", moved, dns)
	must(t, h.Network.EnsureEndpoints(ctx, []driver.Endpoint{dns}))
	wantEndpoints(t, h, "one left", dns)

	// A network deleted takes its endpoints with it, and so does the
	// service's.
	must(t, h.Network.EnsureEndpoints(ctx, []driver.Endpoint{git, dns, gitB}))
	must(t, h.Network.DeleteNetwork(ctx, b.UID))
	wantEndpoints(t, h, "after an endpoint's network went", git, dns)
	must(t, h.Network.DeleteNetwork(ctx, svc.UID))
	wantEndpoints(t, h, "after the service's network went")

	must(t, h.Network.EnsureEndpoints(ctx, nil))
	wantEndpoints(t, h, "none")
	must(t, h.Network.DeleteNetwork(ctx, a.UID))
}
