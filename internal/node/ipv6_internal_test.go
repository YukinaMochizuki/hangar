package node

import (
	"errors"
	"net/netip"
	"testing"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
)

var (
	testPrefix6 = netip.MustParsePrefix("2001:db8:0:1::/64")
	moving      = netip.MustParseAddr("2001:db8:0:1:8a00::1")
	auto        = netip.MustParseAddr("2001:db8:0:1:8a00::2")
	testNet6    = driver.NetworkSpec{UID: "n1", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.0/31")}
)

// handOffHost is an agent on a fake host with an IPv6 prefix, where machine
// "old" holds the address moving and machine "new" holds auto.
func handOffHost(t *testing.T) (*Agent, *fake.Host, driver.Interface) {
	t.Helper()
	host := fake.New()
	host.SetIPv6Prefix(testPrefix6)
	a := &Agent{cfg: Config{IPv6Prefix: testPrefix6, Drivers: &Drivers{
		Images: host.Images(), Storage: host.Storage(), Network: host.Network(), Hypervisor: host.Hypervisor(), Guest: host.Guest(),
	}}}
	if err := host.Network().EnsureNetwork(t.Context(), testNet6); err != nil {
		t.Fatal(err)
	}
	nic := func(uid, mac string, host byte, v6 netip.Addr) driver.Interface {
		return driver.Interface{
			MachineUID: uid, Name: "eth0", NetworkUID: testNet6.UID, MAC: mac,
			Address: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 0, 0, host}), 24), Gateway: netip.MustParseAddr("10.0.0.1"), IPv6: v6,
		}
	}
	if _, err := host.Network().EnsureInterface(t.Context(), nic("old", "02:00:00:00:00:02", 2, moving)); err != nil {
		t.Fatal(err)
	}
	made, err := host.Network().EnsureInterface(t.Context(), nic("new", "02:00:00:00:00:03", 3, auto))
	if err != nil {
		t.Fatal(err)
	}
	return a, host, made
}

// TestTheNodeWaitsForAnAddressToBeLetGo: an interface whose spec gives it
// the address another machine's interface still holds, as an Address moving
// before the machine it leaves has let go, is made without it, and the
// node waits; it never asks the host for it.
func TestTheNodeWaitsForAnAddressToBeLetGo(t *testing.T) {
	a, host, nic := handOffHost(t)
	nic.IPv6 = moving
	made, _, pending, err := a.ensureInterfaces(t.Context(), "new", []driver.Interface{nic}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(pending, controller.ErrWaiting) {
		t.Errorf("pending %v, want waiting", pending)
	}
	if len(made) != 1 || made[0].IPv6.IsValid() {
		t.Errorf("made %+v, want eth0 without an IPv6 address", made)
	}
	if n := host.IPv6Refusals(); n != 0 {
		t.Errorf("the host refused %d interfaces an address another held", n)
	}
}

// TestLettingGoTakesNoAddressAnotherHolds: a machine letting go of its
// address before its spec's, which another machine's interface still
// holds, is left with none until it is let go, and never asks the host for
// it; the machine that holds it, letting go, gives it up.
func TestLettingGoTakesNoAddressAnotherHolds(t *testing.T) {
	a, host, _ := handOffHost(t)
	machine := func(uid string, v6 netip.Addr) *computev1alpha1.Machine {
		return &computev1alpha1.Machine{
			Metadata: &metav1.ObjectMeta{Uid: uid},
			Spec: &computev1alpha1.MachineSpec{Interfaces: []*computev1alpha1.MachineInterface{{
				Name: "eth0", Ipv6: netip.PrefixFrom(v6, 128).String(),
			}}},
		}
	}
	held := func(uid string) netip.Addr {
		ifs, err := host.Network().Interfaces(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range ifs {
			if i.MachineUID == uid {
				return i.IPv6
			}
		}
		return netip.Addr{}
	}
	if err := a.letGoOfIPv6(t.Context(), machine("new", moving)); err != nil {
		t.Fatal(err)
	}
	if got := held("new"); got.IsValid() {
		t.Errorf("new holds %s while old does; want none", got)
	}
	if n := host.IPv6Refusals(); n != 0 {
		t.Errorf("the host refused %d interfaces an address another held", n)
	}
	if err := a.letGoOfIPv6(t.Context(), machine("old", auto)); err != nil {
		t.Fatal(err)
	}
	if got := held("old"); got != auto {
		t.Errorf("old, letting go, holds %s; want %s", got, auto)
	}
}
