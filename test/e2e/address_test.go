package e2e

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// applyAddress binds Address name to the target, or to nothing if target
// is nil.
func (w *world) applyAddress(name string, target *networkv1alpha1.AddressTarget) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.Address, &networkv1alpha1.Address{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec:     &networkv1alpha1.AddressSpec{Target: target},
	}); err != nil {
		w.t.Fatal(err)
	}
}

func toContainer(name string) *networkv1alpha1.AddressTarget {
	return &networkv1alpha1.AddressTarget{Object: &networkv1alpha1.AddressTarget_Container{Container: name}}
}

func toVM(name string) *networkv1alpha1.AddressTarget {
	return &networkv1alpha1.AddressTarget{Object: &networkv1alpha1.AddressTarget_VirtualMachine{VirtualMachine: name}}
}

// hostIPv6 is the IPv6 address the host holds for a machine's eth0.
func (w *world) hostIPv6(machineUID string) netip.Addr {
	ifs := w.host.Snapshot().Interfaces[machineUID]
	if len(ifs) == 0 {
		return netip.Addr{}
	}
	return ifs[0].IPv6
}

// TestAnAddressMovesAndNothingRestarts: an Address bound to a container,
// moved to a VM and deleted moves between the machines' interfaces, on the
// host, in the container's guest, told at once, and in the VM's DHCPv6
// lease, and neither machine starts again; the interface it leaves gets an
// automatic address of its own. A container whose guest cannot be told
// starts again with it, once.
func TestAnAddressMovesAndNothingRestarts(t *testing.T) {
	t.Parallel()
	w := newIdleWorld(t)
	w.ipv6Prefix = hostPrefix
	w.startAgent()
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	w.applyVM("web", 0)
	w.apply(container("api", "nginx"))
	w.waitVMReason("web", "Running")
	w.waitReason("api", "Running")
	vmUID := w.machine2(vmMachineOf("web")).GetMetadata().GetUid()
	ctUID := w.machine("api").GetMetadata().GetUid()
	vmMAC, _ := net.ParseMAC(w.host.Snapshot().VMs[vmUID].Spec.Interfaces[0].MAC)
	started := w.started(vmUID, ctUID)
	netUID := w.networkUID("default")

	w.applyAddress("ip", toContainer("api"))
	var addr netip.Addr
	testutil.Eventually(w.t, "the address reserved and bound to api", func() (bool, string) {
		a, _ := get[*networkv1alpha1.Address](w, registry.Address, "ip")
		p, err := netip.ParsePrefix(a.GetStatus().GetIpv6())
		addr = p.Addr()
		return err == nil && a.GetStatus().GetMachine() == "ct-api", fmt.Sprint(a.GetStatus())
	})
	if !hostPrefix.Contains(addr) {
		t.Fatalf("the address %s is not of the host's prefix", addr)
	}
	testutil.Eventually(w.t, "api holding the address on the host and in its guest", func() (bool, string) {
		g := w.host.GuestIPv6(ctUID)
		return w.hostIPv6(ctUID) == addr && len(g) == 1 && g[0].Address == addr, fmt.Sprint(w.hostIPv6(ctUID), g)
	})

	w.applyAddress("ip", toVM("web"))
	testutil.Eventually(w.t, "web holding the address on the host, leased it by DHCPv6", func() (bool, string) {
		got := w.leasedTo(netUID, vmMAC)
		return w.hostIPv6(vmUID) == addr && got == addr, fmt.Sprint(w.hostIPv6(vmUID), got)
	})
	var auto netip.Addr
	testutil.Eventually(w.t, "api given an automatic address, on the host and in its guest", func() (bool, string) {
		auto = w.hostIPv6(ctUID)
		g := w.host.GuestIPv6(ctUID)
		return auto.IsValid() && auto != addr && len(g) == 1 && g[0].Address == auto, fmt.Sprint(auto, g)
	})

	if _, err := w.root.Delete(context.Background(), registry.Address, client.Key{Project: project, Name: "ip"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(w.t, "web given an automatic address once the address went", func() (bool, string) {
		got := w.leasedTo(netUID, vmMAC)
		h := w.hostIPv6(vmUID)
		return h.IsValid() && h != addr && got == h, fmt.Sprint(h, got)
	})
	testutil.Eventually(w.t, "the address gone", func() (bool, string) {
		_, ok := get[*networkv1alpha1.Address](w, registry.Address, "ip")
		return !ok, ""
	})
	w.checkNotRestarted(started)
	snap := w.host.Snapshot()

	// A guest that cannot be told starts again, with its address on its
	// boot disk, though its VM takes a while to stop, past the guest's
	// timeout.
	w.host.OldGuest(ctUID, true)
	defer w.host.DelayStop(ctUID, 300*time.Millisecond)()
	w.applyAddress("old", toContainer("api"))
	testutil.Eventually(w.t, "api started again with the address on its boot disk", func() (bool, string) {
		a, _ := get[*networkv1alpha1.Address](w, registry.Address, "old")
		p, err := netip.ParsePrefix(a.GetStatus().GetIpv6())
		v := w.host.Snapshot().VMs[ctUID]
		return err == nil && v.State == driver.VMRunning && v.Started.After(snap.VMs[ctUID].Started) && len(v.Spec.Interfaces) == 1 &&
			v.Spec.Interfaces[0].IPv6 == p.Addr(), fmt.Sprint(v.Spec.Interfaces, v.Started)
	})
	again := w.host.Snapshot().VMs[ctUID].Started
	testutil.Unchanged(w.t, "api's VM, started again", 500*time.Millisecond, w.vmVersion(ctUID))
	if v := w.host.Snapshot().VMs[ctUID]; !v.Started.Equal(again) {
		t.Errorf("api started again more than once: %v then %v", again, v.Started)
	}
}

// addressOf waits for Address name to be reserved and returns its address.
func (w *world) addressOf(name string) netip.Addr {
	w.t.Helper()
	var addr netip.Addr
	testutil.Eventually(w.t, "address "+name+" reserved", func() (bool, string) {
		a, _ := get[*networkv1alpha1.Address](w, registry.Address, name)
		p, err := netip.ParsePrefix(a.GetStatus().GetIpv6())
		addr = p.Addr()
		return err == nil, fmt.Sprint(a.GetStatus())
	})
	return addr
}

// guestHolds says whether the machine's guest holds addr on its one
// interface.
func (w *world) guestHolds(machineUID string, addr netip.Addr) (bool, string) {
	g := w.host.GuestIPv6(machineUID)
	return len(g) == 1 && g[0].Address == addr, fmt.Sprint(g)
}

// watchHolders watches, until the returned function is called, that no two
// interfaces on the host hold addr, that the agent never asks the host for
// an address another interface holds, and that the machines never report
// their networking failed, and fails the test then if any did. Anything
// else they report while the agent checks them again, such as their image,
// is no fault.
func (w *world) watchHolders(addr netip.Addr, machines ...string) (stop func()) {
	w.t.Helper()
	var mu sync.Mutex
	var seen []string
	done, stopped := make(chan struct{}), make(chan struct{})
	refused := w.host.IPv6Refusals()
	go func() {
		defer close(stopped)
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond):
			}
			var holders []string
			for uid, ifs := range w.host.Snapshot().Interfaces {
				for _, i := range ifs {
					if i.IPv6 == addr {
						holders = append(holders, uid+"/"+i.Name)
					}
				}
			}
			mu.Lock()
			if len(holders) > 1 {
				seen = append(seen, fmt.Sprintf("%v hold %s", holders, addr))
			}
			if n := w.host.IPv6Refusals(); n > refused {
				seen = append(seen, fmt.Sprintf("the host refused %d interfaces an address another held", n-refused))
				refused = n
			}
			for _, m := range machines {
				obj, err := w.root.Get(context.Background(), registry.Machine, client.Key{Project: project, Name: m})
				if err != nil {
					continue
				}
				c := conditions.Find(obj.(*computev1alpha1.Machine).GetStatus().GetConditions(), "Ready")
				if c.GetReason() == computeapi.ReasonNetworkFailed {
					seen = append(seen, fmt.Sprintf("%s's networking failed: %s", m, c.GetMessage()))
				}
			}
			mu.Unlock()
		}
	}()
	return func() {
		w.t.Helper()
		close(done)
		<-stopped
		mu.Lock()
		defer mu.Unlock()
		if len(seen) > 0 {
			w.t.Errorf("while the address moved: %d faults, the first %q", len(seen), seen[0])
		}
	}
}

// TestNoTwoInterfacesHoldAnAddress: an Address moving from a machine whose
// reconcile waits on its image, as after the agent restarts with the image
// gone, reaches the machine it moves to once the first has let go, without
// waiting for the image; at no moment do both hold it on the host, and the
// machine it moves to never fails to take it.
func TestNoTwoInterfacesHoldAnAddress(t *testing.T) {
	t.Parallel()
	w := newIdleWorld(t)
	w.ipv6Prefix = hostPrefix
	w.host.AddImage("redis", "sha256:redis-1", nginx)
	w.startAgent()
	w.apply(container("api", "nginx"))
	w.apply(container("db", "redis"))
	w.waitReason("api", "Running")
	w.waitReason("db", "Running")
	apiUID, dbUID := w.machine("api").GetMetadata().GetUid(), w.machine("db").GetMetadata().GetUid()
	started := w.started(apiUID, dbUID)
	w.applyAddress("ip", toContainer("api"))
	addr := w.addressOf("ip")
	testutil.Eventually(w.t, "api holding the address", func() (bool, string) {
		return w.hostIPv6(apiUID) == addr, w.hostIPv6(apiUID).String()
	})

	w.stopAgent()
	w.host.LoseBase("sha256:nginx-1")
	letGo := w.host.HoldUnpack("nginx")
	defer letGo()
	stop := w.watchHolders(addr, "ct-db")
	w.startAgent()
	w.applyAddress("ip", toContainer("db"))
	testutil.Eventually(w.t, "db holding the address, on the host and in its guest, with api's image still being made", func() (bool, string) {
		ok, g := w.guestHolds(dbUID, addr)
		return ok && w.hostIPv6(dbUID) == addr, fmt.Sprint(w.hostIPv6(apiUID), w.hostIPv6(dbUID), g)
	})
	stop()
	letGo()
	testutil.Eventually(w.t, "api told an automatic address", func() (bool, string) {
		h := w.hostIPv6(apiUID)
		ok, g := w.guestHolds(apiUID, h)
		return ok && h.IsValid() && h != addr, g
	})
	w.checkNotRestarted(started)
}

// TestAGuestIsToldItsAddressAfterTheAgentRestarts: an agent that starts
// again knows nothing of what it told a guest, so it tells it again; here an
// Address the guest booted with moved away and, while the agent was down,
// back, to what its boot disk carries.
func TestAGuestIsToldItsAddressAfterTheAgentRestarts(t *testing.T) {
	t.Parallel()
	w := newIdleWorld(t)
	w.ipv6Prefix = hostPrefix
	w.startAgent()
	w.applyAddress("ip", toContainer("api"))
	w.apply(container("api", "nginx"))
	w.apply(container("db", "nginx"))
	w.waitReason("api", "Running")
	w.waitReason("db", "Running")
	apiUID := w.machine("api").GetMetadata().GetUid()
	started := w.started(apiUID)
	addr := w.addressOf("ip")
	testutil.Eventually(w.t, "api booted with the address", func() (bool, string) {
		v := w.host.Snapshot().VMs[apiUID]
		ok, g := w.guestHolds(apiUID, addr)
		return ok && len(v.Spec.Interfaces) == 1 && v.Spec.Interfaces[0].IPv6 == addr, g
	})
	w.applyAddress("ip", toContainer("db"))
	testutil.Eventually(w.t, "api told an automatic address", func() (bool, string) {
		h := w.hostIPv6(apiUID)
		ok, g := w.guestHolds(apiUID, h)
		return ok && h.IsValid() && h != addr, g
	})

	w.stopAgent()
	w.applyAddress("ip", toContainer("api"))
	w.startAgent()
	testutil.Eventually(w.t, "api holding the address again, on the host and in its guest", func() (bool, string) {
		ok, g := w.guestHolds(apiUID, addr)
		return ok && w.hostIPv6(apiUID) == addr, fmt.Sprint(w.hostIPv6(apiUID), g)
	})
	w.checkNotRestarted(started)
}

// TestAGuestThatRebootsIsToldItsAddressAgain: a container's guest that boots
// again, by its restart policy, holds what its boot disk gave it when its VM
// started, not the Address that moved to it since, until it is told again.
func TestAGuestThatRebootsIsToldItsAddressAgain(t *testing.T) {
	t.Parallel()
	w := newIdleWorld(t)
	w.ipv6Prefix = hostPrefix
	w.startAgent()
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	apiUID := w.machine("api").GetMetadata().GetUid()
	w.applyAddress("ip", toContainer("api"))
	addr := w.addressOf("ip")
	testutil.Eventually(w.t, "api's guest told the address", func() (bool, string) { return w.guestHolds(apiUID, addr) })
	if v := w.host.Snapshot().VMs[apiUID]; v.Spec.Interfaces[0].IPv6 == addr {
		t.Fatal("api's boot disk carries the address: the test sees nothing")
	}
	w.settle("api")
	if err := w.host.Exit(apiUID, 1); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(w.t, "api's guest, booted again, told the address again", func() (bool, string) {
		ok, g := w.guestHolds(apiUID, addr)
		return ok && w.host.Snapshot().VMs[apiUID].Boots == 2, g
	})
}

// TestAnUpgradeRestartsNoContainer: a VM an older node started, whose hash
// counted its interfaces' IPv6 addresses, runs on after the agent is
// upgraded, and its Address moves as any other's.
func TestAnUpgradeRestartsNoContainer(t *testing.T) {
	t.Parallel()
	w := newIdleWorld(t)
	w.ipv6Prefix = hostPrefix
	w.startAgent()
	w.waitNodePrefix()
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	apiUID := w.machine("api").GetMetadata().GetUid()
	if v := w.host.Snapshot().VMs[apiUID]; !v.Spec.Interfaces[0].IPv6.IsValid() {
		t.Fatal("api started without an IPv6 address: its hashes are the same, and the test sees nothing")
	}
	w.settle("api")
	started := w.started(apiUID)
	w.stopAgent()
	w.host.StartedByOldNode(apiUID)
	w.startAgent()
	w.applyAddress("ip", toContainer("api"))
	addr := w.addressOf("ip")
	testutil.Eventually(w.t, "api holding the address, on the host and in its guest", func() (bool, string) {
		ok, g := w.guestHolds(apiUID, addr)
		return ok && w.hostIPv6(apiUID) == addr, g
	})
	w.settle("api")
	w.checkNotRestarted(started)
}

// waitNodePrefix waits for the node to report its IPv6 prefix, so a machine
// made after starts with an address.
func (w *world) waitNodePrefix() {
	w.t.Helper()
	testutil.Eventually(w.t, "the node reporting its prefix", func() (bool, string) {
		obj, err := w.root.Get(context.Background(), registry.Node, client.Key{Name: apiservertest.Node})
		if err != nil {
			return false, err.Error()
		}
		p := obj.(*computev1alpha1.Node).GetStatus().GetIpv6Prefix()
		return p != "", p
	})
}

// started is when the machines' VMs started, by machine uid.
func (w *world) started(uids ...string) map[string]time.Time {
	snap := w.host.Snapshot()
	out := map[string]time.Time{}
	for _, uid := range uids {
		out[uid] = snap.VMs[uid].Started
	}
	return out
}

// checkNotRestarted fails the test if a VM is not running as it was when it
// started, as started says: stopped and started again, or rebooted.
func (w *world) checkNotRestarted(started map[string]time.Time) {
	w.t.Helper()
	snap := w.host.Snapshot()
	for uid, at := range started {
		if v := snap.VMs[uid]; v.State != driver.VMRunning || !v.Started.Equal(at) {
			w.t.Errorf("machine %s started again: %v at %v, it started at %v", uid, v.State, v.Started, at)
		}
	}
}
