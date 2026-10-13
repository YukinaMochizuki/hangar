package apiserver

import (
	"net/netip"
	"strings"
	"testing"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
)

func onNetworks(c *computev1alpha1.Container, networks ...string) *computev1alpha1.Container {
	for _, n := range networks {
		c.Spec.Interfaces = append(c.Spec.Interfaces, &computev1alpha1.NetworkInterface{Network: n})
	}
	return c
}

func TestInterfaceAdmission(t *testing.T) {
	s, _, user := running(t)
	b := s.backend(registry.Container)
	nine := make([]string, 9)
	for i := range nine {
		nine[i] = "n" + string(rune('a'+i))
	}
	for name, tc := range map[string]struct {
		networks []string
		ok       bool
	}{
		"none":                  {nil, true},
		"two":                   {[]string{"default", "backend"}, true},
		"one not made yet":      {[]string{"later"}, true},
		"a network twice":       {[]string{"default", "default"}, false},
		"a name no network has": {[]string{"Not_A_Name"}, false},
		"an empty name":         {[]string{""}, false},
		"more than eight":       {nine, false},
	} {
		_, err := b.Apply(user, onNetworks(ctr("api"), tc.networks...))
		if got := err == nil; got != tc.ok {
			t.Errorf("%s: accepted %t, want %t (%v)", name, got, tc.ok, err)
		}
	}
}

// TestAContainerOnSeveralNetworks: each interface asked for is the
// machine's, by position, with an address of its own network.
func TestAContainerOnSeveralNetworks(t *testing.T) {
	s, system, user := controlled(t)
	if _, err := s.backend(registry.Network).Apply(user, network("backend", "10.1.0.0/24")); err != nil {
		t.Fatal(err)
	}
	applyCtr(t, s, user, onNetworks(ctr("api"), "backend", "default"))
	m := waitMachine(t, s, system, "ct-api", "both interfaces addressed", func(m *computev1alpha1.Machine) bool {
		nics := m.GetSpec().GetInterfaces()
		return len(nics) == 2 && nics[0].GetIpv4() != "" && nics[1].GetIpv4() != ""
	})
	for i, want := range []struct{ name, network, cidr string }{
		{"eth0", "backend", "10.1.0.0/24"},
		{"eth1", "default", "10.0.0.0/24"},
	} {
		nic := m.GetSpec().GetInterfaces()[i]
		addr, err := netip.ParsePrefix(nic.GetIpv4())
		if nic.GetName() != want.name || nic.GetNetwork() != want.network || err != nil ||
			!netip.MustParsePrefix(want.cidr).Contains(addr.Addr()) || !strings.HasSuffix(nic.GetIpv4(), "/24") {
			t.Errorf("interface %d: %v, want %s on %s in %s", i, nic, want.name, want.network, want.cidr)
		}
	}

	// Leaving a network leaves one interface, eth0, on the other.
	applyCtr(t, s, user, onNetworks(ctr("api"), "default"))
	waitMachine(t, s, system, "ct-api", "on default alone", func(m *computev1alpha1.Machine) bool {
		nics := m.GetSpec().GetInterfaces()
		return len(nics) == 1 && nics[0].GetName() == "eth0" && nics[0].GetNetwork() == "default" && nics[0].GetIpv4() != ""
	})
}
