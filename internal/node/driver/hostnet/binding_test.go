package hostnet

import (
	"net"
	"net/netip"
	"slices"
	"strings"
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

// TestIPv6Binding: a tap sends IPv6 only with an IPv6 address, and then
// under its own rules, which change with the address.
func TestIPv6Binding(t *testing.T) {
	a := boundTap{name: "hnt000000000001", mac: net.HardwareAddr{2, 0, 0, 0, 0, 1}, addr: netip.MustParseAddr("10.0.0.2")}
	v4 := strings.Join(comments(t, bindingChains([]boundTap{a})), "\n")
	if !strings.Contains(v4, "sends only IPv4 and ARP") || strings.Contains(v4, "IPv6") {
		t.Errorf("a tap without an IPv6 address may send IPv6:\n%s", v4)
	}
	a.ipv6 = netip.MustParseAddr("2001:db8:0:1:1:2:3:4")
	v6 := comments(t, bindingChains([]boundTap{a}))
	all := strings.Join(v6, "\n")
	for _, want := range []string{
		"sends IPv6 only from 2001:db8:0:1:1:2:3:4 or link-local", "sends no router advertisements",
		"serves no DHCPv6", "sends nothing to DHCPv6 clients", "advertises only 2001:db8:0:1:1:2:3:4",
		"does not advertise the gateway", "neighbour advertisements say only 02:00:00:00:00:01", "sends only IPv4, IPv6 and ARP",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("no rule %q:\n%s", want, all)
		}
	}
	other := a
	other.ipv6 = netip.MustParseAddr("2001:db8:0:1:1:2:3:5")
	if slices.Equal(comments(t, bindingChains([]boundTap{other})), v6) {
		t.Error("another IPv6 address reads as the old binding")
	}
}
