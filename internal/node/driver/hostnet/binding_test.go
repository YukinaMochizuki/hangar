package hostnet

import (
	"net"
	"net/netip"
	"slices"
	"testing"
)

func TestBindingChains(t *testing.T) {
	a := boundTap{name: "hnt000000000001", mac: net.HardwareAddr{2, 0, 0, 0, 0, 1}, addr: netip.MustParseAddr("10.0.0.2")}
	b := boundTap{name: "hnt000000000002", mac: net.HardwareAddr{2, 0, 0, 0, 0, 2}, addr: netip.MustParseAddr("10.0.0.3")}
	none := comments(t, bindingChains(nil))
	if len(none) != 1 {
		t.Fatalf("no taps: %q, want only the rule dropping unbound taps", none)
	}
	two := comments(t, bindingChains([]boundTap{b, a}))
	if len(two) != 15 || two[14] != none[0] {
		t.Fatalf("two taps: %d rules, want 7 each and the unbound rule last: %q", len(two), two)
	}
	if !slices.Equal(two, comments(t, bindingChains([]boundTap{a, b}))) {
		t.Error("the rules depend on the order taps are listed in")
	}
	// Another MAC or address for a tap must change its rules' comments, or
	// tableIs would keep the old binding.
	for _, other := range []boundTap{
		{name: a.name, mac: net.HardwareAddr{2, 0, 0, 0, 0, 9}, addr: a.addr},
		{name: a.name, mac: a.mac, addr: netip.MustParseAddr("10.0.0.9")},
	} {
		if got := comments(t, bindingChains([]boundTap{other, b})); slices.Equal(got, two) {
			t.Errorf("binding %+v reads as binding %+v", other, a)
		}
	}
}
