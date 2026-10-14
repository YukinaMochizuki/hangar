package apiserver

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// address is an Address of project web bound to container ctr's interface,
// or to nothing if ctr is empty.
func address(name, ctr, iface string) *networkv1alpha1.Address {
	a := &networkv1alpha1.Address{Metadata: &metav1.ObjectMeta{Project: "web", Name: name}, Spec: &networkv1alpha1.AddressSpec{}}
	if ctr != "" {
		a.Spec.Target = &networkv1alpha1.AddressTarget{Object: &networkv1alpha1.AddressTarget_Container{Container: ctr}, Interface: iface}
	}
	return a
}

func applyAddress(t *testing.T, s *Server, user context.Context, a *networkv1alpha1.Address) {
	t.Helper()
	if _, err := s.backend(registry.Address).Apply(user, a); err != nil {
		t.Fatal(err)
	}
}

// waitAddress waits until the Address satisfies ok.
func waitAddress(t *testing.T, s *Server, system context.Context, name, what string, ok func(*networkv1alpha1.Address) bool) *networkv1alpha1.Address {
	t.Helper()
	var last *networkv1alpha1.Address
	deadline := time.Now().Add(10 * time.Second)
	for {
		obj, err := s.get(system, registry.Address, resource.Key{Project: "web", Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if obj != nil {
			last = obj.(*networkv1alpha1.Address)
			if ok(last) {
				return last
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("address %s never %s; last seen: %v", name, what, last)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func boundReason(a *networkv1alpha1.Address) string {
	return conditions.Find(a.GetStatus().GetConditions(), AddressBound).GetReason()
}

func TestAddressAdmission(t *testing.T) {
	s, _, user := running(t)
	b := s.backend(registry.Address)
	if _, err := b.Apply(user, address("free", "", "")); err != nil {
		t.Errorf("an address bound to nothing: %v", err)
	}
	if _, err := b.Apply(user, address("waits", "not-made-yet", "")); err != nil {
		t.Errorf("an address of a container not made yet: %v", err)
	}
	for name, a := range map[string]*networkv1alpha1.Address{
		"a target naming no object": {Metadata: &metav1.ObjectMeta{Project: "web", Name: "bad1"},
			Spec: &networkv1alpha1.AddressSpec{Target: &networkv1alpha1.AddressTarget{Interface: "eth0"}}},
		"a target's name not a name":       address("bad2", "Not_A_Name", ""),
		"an interface's name not a name":   address("bad3", "api", "Eth 0"),
		"an interface another address has": address("bad4", "not-made-yet", "eth0"),
	} {
		_, err := b.Apply(user, a)
		if name == "an interface another address has" {
			wantCodeOf(t, err, connect.CodeFailedPrecondition, name)
			continue
		}
		wantCodeOf(t, err, connect.CodeInvalidArgument, name)
	}
	// Another interface of the same container, and the same interface of a
	// VirtualMachine of the same name, are other interfaces.
	if _, err := b.Apply(user, address("eth1", "not-made-yet", "eth1")); err != nil {
		t.Errorf("another interface of the container: %v", err)
	}
	vm := address("vm", "", "")
	vm.Spec.Target = &networkv1alpha1.AddressTarget{Object: &networkv1alpha1.AddressTarget_VirtualMachine{VirtualMachine: "not-made-yet"}}
	if _, err := b.Apply(user, vm); err != nil {
		t.Errorf("a VirtualMachine's interface of a container's name: %v", err)
	}
	// An address applied again is no other address.
	if _, err := b.Apply(user, address("waits", "not-made-yet", "eth0")); err != nil {
		t.Errorf("applying an address again: %v", err)
	}
}

// TestBindingIsOneDecision: addresses racing for one interface, only one
// gets it.
func TestBindingIsOneDecision(t *testing.T) {
	s, system, user := running(t)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = s.backend(registry.Address).Apply(user, address(fmt.Sprintf("a%d", i), "api", ""))
		})
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		if err == nil {
			won++
		}
	}
	all, err := s.addresses(system, "web")
	if err != nil || won != 1 || len(all) != 1 {
		t.Errorf("%d addresses bound one interface, %d stored (%v); errors %v", won, len(all), err, errs)
	}
}

func TestAnAddressIsReservedFromTheNodesPrefix(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeReporting(t, s, "n1", "")
	applyAddress(t, s, user, address("ip", "", ""))
	waitAddress(t, s, system, "ip", "waiting for a prefix", func(a *networkv1alpha1.Address) bool { return boundReason(a) == ReasonNoPrefix })
	nodeReporting(t, s, "n1", testPrefix)
	a := waitAddress(t, s, system, "ip", "reserved", func(a *networkv1alpha1.Address) bool { return a.GetStatus().GetIpv6() != "" })
	checkIPv6(t, a.GetStatus().GetIpv6())
	if boundReason(a) != ReasonUnbound {
		t.Errorf("an address bound to nothing says %q", boundReason(a))
	}
	taken, err := s.takenIPv6(system)
	if err != nil || !taken[netip.MustParsePrefix(a.GetStatus().GetIpv6()).Addr()] {
		t.Errorf("a reserved address may be drawn as an interface's own: %v", err)
	}
}

// TestAnAddressFollowsItsTarget: bound, retargeted, unbound and deleted, an
// address moves between interfaces, which fall back to an automatic address
// of their own; deleting its target leaves it reserved.
func TestAnAddressFollowsItsTarget(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeReporting(t, s, "n1", testPrefix)
	for _, c := range []string{"a", "b"} {
		applyCtr(t, s, user, ctr(c))
	}
	ipv6OfCtr := func(c string) string {
		return ipv6Of(getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-" + c}))
	}
	for _, c := range []string{"a", "b"} {
		waitMachine(t, s, system, "ct-"+c, "given IPv6", func(m *computev1alpha1.Machine) bool { return ipv6Of(m) != "" })
	}
	autoA := ipv6OfCtr("a")

	applyAddress(t, s, user, address("ip", "a", ""))
	addr := waitAddress(t, s, system, "ip", "bound to a", func(a *networkv1alpha1.Address) bool {
		return boundReason(a) == ReasonBound && a.GetStatus().GetMachine() == "ct-a"
	}).GetStatus().GetIpv6()
	if got := ipv6OfCtr("a"); got != addr {
		t.Fatalf("a holds %s, its address is %s", got, addr)
	}
	if addr == autoA {
		t.Fatal("the address is a's automatic one")
	}

	applyAddress(t, s, user, address("ip", "b", ""))
	waitAddress(t, s, system, "ip", "moved to b", func(a *networkv1alpha1.Address) bool { return a.GetStatus().GetMachine() == "ct-b" })
	if got := ipv6OfCtr("b"); got != addr {
		t.Errorf("b holds %s, the address is %s", got, addr)
	}
	waitMachine(t, s, system, "ct-a", "given an automatic address again", func(m *computev1alpha1.Machine) bool {
		return ipv6Of(m) != "" && ipv6Of(m) != addr
	})
	checkIPv6(t, ipv6OfCtr("a"))

	applyAddress(t, s, user, address("ip", "", ""))
	waitAddress(t, s, system, "ip", "unbound", func(a *networkv1alpha1.Address) bool { return boundReason(a) == ReasonUnbound })
	waitMachine(t, s, system, "ct-b", "given an automatic address", func(m *computev1alpha1.Machine) bool {
		return ipv6Of(m) != "" && ipv6Of(m) != addr
	})
	if got := waitAddress(t, s, system, "ip", "kept", func(*networkv1alpha1.Address) bool { return true }).GetStatus().GetIpv6(); got != addr {
		t.Errorf("unbinding changed the address from %s to %s", addr, got)
	}

	// Its target deleted, the address stays reserved, and binds the target
	// made again.
	applyAddress(t, s, user, address("ip", "a", ""))
	waitAddress(t, s, system, "ip", "bound to a again", func(a *networkv1alpha1.Address) bool { return a.GetStatus().GetMachine() == "ct-a" })
	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "a"}, nil); err != nil {
		t.Fatal(err)
	}
	m := waitMachine(t, s, system, "ct-a", "marked for deletion", func(m *computev1alpha1.Machine) bool {
		return m.GetMetadata().GetDeleteTime() != nil
	})
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration(),
		&metav1.Condition{Type: MachineReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned"})
	waitGone(t, s, system, registry.Machine, "ct-a")
	waitGone(t, s, system, registry.Container, "a")
	a := waitAddress(t, s, system, "ip", "unbound, its target gone", func(a *networkv1alpha1.Address) bool {
		return boundReason(a) == ReasonTargetNotFound && a.GetStatus().GetMachine() == ""
	})
	if a.GetStatus().GetIpv6() != addr {
		t.Errorf("deleting the target changed the address from %s to %s", addr, a.GetStatus().GetIpv6())
	}
	applyCtr(t, s, user, ctr("a"))
	waitMachine(t, s, system, "ct-a", "given the address", func(m *computev1alpha1.Machine) bool { return ipv6Of(m) == addr })

	// Deleted, the address leaves its interface an automatic one.
	if _, err := s.backend(registry.Address).Delete(user, resource.Key{Project: "web", Name: "ip"}, nil); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "ct-a", "given an automatic address after its address went", func(m *computev1alpha1.Machine) bool {
		return ipv6Of(m) != "" && ipv6Of(m) != addr
	})
	waitGone(t, s, system, registry.Address, "ip")
}

// TestNoTwoMachinesHoldAnAddress: an Address moved back and forth between
// two containers is held, at every revision of the store, by one machine at
// most: the machine it moves to takes it only once the one it leaves has
// let go.
func TestNoTwoMachinesHoldAnAddress(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeReporting(t, s, "n1", testPrefix)
	for _, c := range []string{"a", "b"} {
		applyCtr(t, s, user, ctr(c))
		waitMachine(t, s, system, "ct-"+c, "given IPv6", func(m *computev1alpha1.Machine) bool { return ipv6Of(m) != "" })
	}
	applyAddress(t, s, user, address("ip", "a", ""))
	addr := waitAddress(t, s, system, "ip", "bound to a", func(a *networkv1alpha1.Address) bool {
		return a.GetStatus().GetMachine() == "ct-a"
	}).GetStatus().GetIpv6()

	rev, err := s.store.Revision(system)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(system)
	var mu sync.Mutex
	var doubles []string
	held := map[string]string{} // by machine, as of the last event
	watched := make(chan error, 1)
	go func() {
		watched <- s.store.Watch(ctx, store.WatchOptions{Kind: registry.Machine.Kind, Project: "web", SinceRevision: rev}, func(e store.Event) error {
			m, ok := decodeMachine(e.Object)
			if !ok {
				return fmt.Errorf("decode machine %s", e.Object.Name)
			}
			mu.Lock()
			defer mu.Unlock()
			held[e.Object.Name] = ""
			if e.Type != store.EventDeleted {
				held[e.Object.Name] = ipv6Of(m)
			}
			var holders []string
			for name, a := range held {
				if a == addr {
					holders = append(holders, name)
				}
			}
			if len(holders) > 1 {
				doubles = append(doubles, fmt.Sprintf("revision %d: %v", e.Revision, holders))
			}
			return nil
		})
	}()
	for i, to := range []string{"b", "a", "b", "a", "b"} {
		applyAddress(t, s, user, address("ip", to, ""))
		waitAddress(t, s, system, "ip", fmt.Sprintf("moved to %s (%d)", to, i), func(a *networkv1alpha1.Address) bool {
			return a.GetStatus().GetMachine() == "ct-"+to
		})
	}
	waitMachine(t, s, system, "ct-a", "given an automatic address", func(m *computev1alpha1.Machine) bool {
		return ipv6Of(m) != "" && ipv6Of(m) != addr
	})
	cancel()
	<-watched
	mu.Lock()
	defer mu.Unlock()
	if len(doubles) > 0 {
		t.Errorf("two machines held %s: %v", addr, doubles)
	}
}

// TestAMovingAddressIsWaitedOn: a container an Address moves to while the
// machine it leaves still holds it waits for it, as waiting and not as a
// failure, and is reported meanwhile; the machine letting go is what maps
// to it again.
func TestAMovingAddressIsWaitedOn(t *testing.T) {
	s, system, user := runningWith(t, func(c *Config) { c.Nodes = []string{"n1"} })
	waitNode(t, s, "n1")
	nodeReporting(t, s, "n1", testPrefix)
	reconcile := func(c string) error {
		t.Helper()
		err := s.reconcileContainer(system, controller.Key{Project: "web", Name: c})
		if err == nil {
			// Placed, and given its addresses, by the scheduler.
			if err := s.reconcileMachine(system, controller.Key{Project: "web", Name: "ct-" + c}); err != nil {
				t.Fatal(err)
			}
		}
		return err
	}
	for _, c := range []string{"a", "b"} {
		applyCtr(t, s, user, ctr(c))
		if err := reconcile(c); err != nil {
			t.Fatal(err)
		}
	}
	applyAddress(t, s, user, address("ip", "a", ""))
	if err := s.reconcileAddress(system, controller.Key{Project: "web", Name: "ip"}); err != nil {
		t.Fatal(err)
	}
	if err := reconcile("a"); err != nil {
		t.Fatal(err)
	}
	machineOf := func(c string) *computev1alpha1.Machine {
		return getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-" + c})
	}
	addr := ipv6Of(machineOf("a"))
	if a, _ := s.get(system, registry.Address, resource.Key{Project: "web", Name: "ip"}); a.(*networkv1alpha1.Address).GetStatus().GetIpv6() != addr {
		t.Fatalf("a holds %s, not the address", addr)
	}

	applyAddress(t, s, user, address("ip", "b", ""))
	grown := ctr("b")
	grown.Spec.Args = []string{"changed"}
	applyCtr(t, s, user, grown)
	err := s.reconcileContainer(system, controller.Key{Project: "web", Name: "b"})
	if !errors.Is(err, controller.ErrWaiting) {
		t.Errorf("b, waiting for a to let go, returned %v, want waiting", err)
	}
	if got := ipv6Of(machineOf("b")); got == addr {
		t.Fatal("b holds the address while a does")
	}
	c, _ := s.get(system, registry.Container, resource.Key{Project: "web", Name: "b"})
	if b := c.(*computev1alpha1.Container); b.GetStatus().GetObservedGeneration() != b.GetMetadata().GetGeneration() {
		t.Errorf("b, waiting, is not reported: status of generation %d, b is at %d", b.GetStatus().GetObservedGeneration(), b.GetMetadata().GetGeneration())
	}

	if err := s.reconcileContainer(system, controller.Key{Project: "web", Name: "a"}); err != nil {
		t.Fatal(err)
	}
	mobj, err := s.store.Get(system, s.backend(registry.Machine).storeKey(resource.Key{Project: "web", Name: "ct-a"}))
	if err != nil {
		t.Fatal(err)
	}
	if keys := s.machineOwnerAndNeighbours(system, mobj); !slices.Contains(keys, controller.Key{Project: "web", Name: "b"}) {
		t.Errorf("a letting go maps to %v, not to b", keys)
	}
	if err := s.reconcileContainer(system, controller.Key{Project: "web", Name: "b"}); err != nil {
		t.Fatal(err)
	}
	if got := ipv6Of(machineOf("b")); got != addr {
		t.Errorf("b holds %s once a let go, not the address %s", got, addr)
	}
}

// TestATargetIsOfItsAddressesProject: an Address names a container of its
// own project; one of the same name in another project is not given it.
func TestATargetIsOfItsAddressesProject(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeReporting(t, s, "n1", testPrefix)
	if _, err := s.backend(registry.Project).Apply(user, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "other"}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "acme"},
	}); err != nil {
		t.Fatal(err)
	}
	elsewhere := ctr("api")
	elsewhere.Metadata.Project = "other"
	applyCtr(t, s, user, elsewhere)
	applyAddress(t, s, user, address("ip", "api", ""))
	a := waitAddress(t, s, system, "ip", "waiting for its target", func(a *networkv1alpha1.Address) bool {
		return a.GetStatus().GetIpv6() != "" && boundReason(a) == ReasonTargetNotFound
	})
	m := waitMachineIn(t, s, system, "other", "ct-api", "given IPv6", func(m *computev1alpha1.Machine) bool { return ipv6Of(m) != "" })
	if ipv6Of(m) == a.GetStatus().GetIpv6() {
		t.Errorf("project other's api holds project web's address %s", ipv6Of(m))
	}
	time.Sleep(100 * time.Millisecond)
	a = waitAddress(t, s, system, "ip", "still waiting", func(*networkv1alpha1.Address) bool { return true })
	if boundReason(a) != ReasonTargetNotFound || a.GetStatus().GetMachine() != "" {
		t.Errorf("project web's address is %s by %q", boundReason(a), a.GetStatus().GetMachine())
	}
}

// TestAddressesCatchUpOnStart: Addresses and their targets written while no
// controller ran are reserved and bound once the apiserver starts with them.
func TestAddressesCatchUpOnStart(t *testing.T) {
	s, system, user := runningWith(t, func(c *Config) { c.Nodes = []string{"n1"}; c.NodeMonitorInterval = 1000 * time.Hour })
	waitNode(t, s, "n1")
	nodeReporting(t, s, "n1", testPrefix)
	applyCtr(t, s, user, ctr("api"))
	applyAddress(t, s, user, address("ip", "api", ""))
	applyAddress(t, s, user, address("free", "", ""))
	startControllers(t, s, system)
	a := waitAddress(t, s, system, "ip", "reserved and bound", func(a *networkv1alpha1.Address) bool {
		return boundReason(a) == ReasonBound && a.GetStatus().GetMachine() == "ct-api"
	})
	waitMachine(t, s, system, "ct-api", "given the address", func(m *computev1alpha1.Machine) bool {
		return ipv6Of(m) == a.GetStatus().GetIpv6()
	})
	free := waitAddress(t, s, system, "free", "reserved", func(a *networkv1alpha1.Address) bool { return boundReason(a) == ReasonUnbound })
	checkIPv6(t, free.GetStatus().GetIpv6())
}

// waitMachineIn waits until machine name of project satisfies ok.
func waitMachineIn(t *testing.T, s *Server, system context.Context, project, name, what string, ok func(*computev1alpha1.Machine) bool) *computev1alpha1.Machine {
	t.Helper()
	var last *computev1alpha1.Machine
	deadline := time.Now().Add(10 * time.Second)
	for {
		obj, err := s.get(system, registry.Machine, resource.Key{Project: project, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if obj != nil {
			last = obj.(*computev1alpha1.Machine)
			if ok(last) {
				return last
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("machine %s/%s never %s; last seen: %v", project, name, what, last)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
