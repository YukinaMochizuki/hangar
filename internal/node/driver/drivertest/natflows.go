package drivertest

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// natFlows: once FollowNAT has started, a connection out of a network is
// reported with the source its destination saw and the network and machine
// it came from, also between networks sharing a range, and for a network
// made on the transit link of one deleted.
func natFlows(t *testing.T, h Host) {
	ctx := t.Context()
	src := h.Network.(driver.NATFlows)
	a := driver.NetworkSpec{UID: "nat-a", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.0/31")}
	b := driver.NetworkSpec{UID: "nat-b", CIDR: a.CIDR, Transit: netip.MustParsePrefix("100.64.0.2/31")}
	c := driver.NetworkSpec{UID: "nat-c", CIDR: netip.MustParsePrefix("10.2.0.0/24"), Transit: a.Transit}
	nic := func(machine string, n driver.NetworkSpec, mac string) driver.Interface {
		i := eth0(machine)
		i.NetworkUID, i.MAC = n.UID, mac
		i.Address = netip.PrefixFrom(n.CIDR.Addr().Next().Next(), n.CIDR.Bits())
		i.Gateway = n.CIDR.Addr().Next()
		return i
	}
	ensure := func(n driver.NetworkSpec, i driver.Interface) driver.Interface {
		t.Helper()
		must(t, h.Network.EnsureNetwork(ctx, n))
		made, err := h.Network.EnsureInterface(ctx, i)
		must(t, err)
		openFirewall(t, h, n, made)
		return made
	}

	flows := make(chan driver.NATFlow, 1024)
	lost := make(chan driver.NATLost, 16)
	started := make(chan time.Time, 1)
	fctx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() {
		errc <- src.FollowNAT(fctx, driver.NATHandler{
			Started: func(at time.Time) { started <- at },
			Flow:    func(f driver.NATFlow) { flows <- f },
			Lost:    func(l driver.NATLost) { lost <- l },
		})
	}()
	defer func() {
		cancel()
		if err := <-errc; err != nil {
			t.Errorf("FollowNAT: %v", err)
		}
	}()
	select {
	case <-started:
	case err := <-errc:
		t.Fatalf("FollowNAT did not start: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("FollowNAT did not start")
	}

	// connect has i connect out, and checks the connection is reported as
	// from its network and machine.
	connect := func(i driver.Interface, port uint16) {
		t.Helper()
		dst, seen := h.Connect(t, i, port)
		deadline := time.After(10 * time.Second)
		for {
			select {
			case f := <-flows:
				if f.Public != seen || f.Destination != dst {
					continue
				}
				if f.NetworkUID != i.NetworkUID || f.Inner != netip.AddrPortFrom(i.Address.Addr(), port) {
					t.Errorf("%s's connection, seen from %s, is reported from network %q at %s, want %s at %s:%d",
						i.MachineUID, seen, f.NetworkUID, f.Inner, i.NetworkUID, i.Address.Addr(), port)
				}
				return
			case l := <-lost:
				t.Errorf("connections lost: %+v", l)
			case <-deadline:
				t.Fatalf("%s's connection, seen from %s, not reported", i.MachineUID, seen)
			}
		}
	}
	ia := ensure(a, nic("nat-ma", a, "02:00:00:00:0a:02"))
	ib := ensure(b, nic("nat-mb", b, "02:00:00:00:0b:02"))
	connect(ia, 41000)
	connect(ib, 41000)

	must(t, h.Network.DeleteInterfaces(ctx, ia.MachineUID))
	must(t, h.Network.DeleteNetwork(ctx, a.UID))
	ic := ensure(c, nic("nat-mc", c, "02:00:00:00:0c:02"))
	connect(ic, 41001)
	connect(ib, 41001)
	must(t, h.Network.DeleteInterfaces(ctx, ib.MachineUID))
	must(t, h.Network.DeleteInterfaces(ctx, ic.MachineUID))
	must(t, h.Network.DeleteNetwork(ctx, b.UID))
	must(t, h.Network.DeleteNetwork(ctx, c.UID))
}

// openFirewall lets the interface send anything.
func openFirewall(t *testing.T, h Host, n driver.NetworkSpec, i driver.Interface) {
	t.Helper()
	p := driver.InterfacePolicy{MachineUID: i.MachineUID, Name: i.Name}
	p.Policy.EgressAll = true
	must(t, h.Network.EnsureFirewall(t.Context(), n.UID, []driver.InterfacePolicy{p}))
}
