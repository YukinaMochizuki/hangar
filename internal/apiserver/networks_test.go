package apiserver

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
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
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

func network(name, cidr string) *networkv1alpha1.Network {
	return &networkv1alpha1.Network{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
		Spec:     &networkv1alpha1.NetworkSpec{Ipv4Cidr: cidr},
	}
}

func getNetwork(t *testing.T, s *Server, system context.Context, name string) *networkv1alpha1.Network {
	t.Helper()
	obj, err := s.get(system, registry.Network, resource.Key{Project: "web", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if obj == nil {
		return nil
	}
	return obj.(*networkv1alpha1.Network)
}

func ipv4Of(m *computev1alpha1.Machine) string {
	return m.GetSpec().GetInterfaces()[0].GetIpv4()
}

func TestNetworkAdmission(t *testing.T) {
	s, _, user := running(t)
	b := s.backend(registry.Network)
	for i, tt := range []struct {
		cidr string
		ok   bool
	}{
		{"10.1.0.0/24", true},
		{"172.16.4.0/22", true},
		{"192.168.1.0/28", true},
		{"10.1.0.5/24", false},   // host bits
		{"10.0.0.0/8", false},    // too big
		{"10.1.0.0/29", false},   // too small
		{"8.8.8.0/24", false},    // public
		{"100.64.0.0/24", false}, // the transit range
		{"fd00::/64", false},
		{"bogus", false},
	} {
		_, err := b.Apply(user, network(fmt.Sprintf("n%d", i), tt.cidr))
		if got := err == nil; got != tt.ok {
			t.Errorf("cidr %s: accepted %t, want %t (%v)", tt.cidr, got, tt.ok, err)
		}
	}
	if _, err := b.Apply(user, network("fixed", "10.2.0.0/24")); err != nil {
		t.Fatal(err)
	}
	_, err := b.Apply(user, network("fixed", "10.3.0.0/24"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "changing a network's range")

	// Every project's default has the one range, whoever creates it.
	if _, err := s.backend(registry.Project).Apply(user, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "api"}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "acme"},
	}); err != nil {
		t.Fatal(err)
	}
	other := network(DefaultNetwork, "10.5.0.0/24")
	other.Metadata.Project = "api"
	_, err = b.Apply(user, other)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a default network with another range")
	other.Spec.Ipv4Cidr = DefaultNetworkCIDR
	if _, err := b.Apply(user, other); err != nil {
		t.Errorf("a default network with the default range: %v", err)
	}
}

func TestEveryProjectGetsADefaultNetwork(t *testing.T) {
	s, system, user := controlled(t)
	var first *networkv1alpha1.Network
	testutil.Eventually(t, "the default network with its transit link", func() bool {
		first = getNetwork(t, s, system, DefaultNetwork)
		return first != nil && conditions.IsTrue(first.GetStatus().GetConditions(), NetworkReady)
	})
	// Signing up made other projects' networks, which may have been given
	// their links first.
	if transit, err := netip.ParsePrefix(first.GetStatus().GetTransitIpv4()); first.GetSpec().GetIpv4Cidr() != DefaultNetworkCIDR ||
		err != nil || transit.Bits() != 31 || !transitRange.Contains(transit.Addr()) {
		t.Errorf("default network: %v / %v", first.GetSpec(), first.GetStatus())
	}
	// A network with its transit link is left alone.
	testutil.Unchanged(t, "a ready network", 300*time.Millisecond,
		versionOf(s, system, registry.Network, resource.Key{Project: "web", Name: DefaultNetwork}))
	if rv := getNetwork(t, s, system, DefaultNetwork).GetMetadata().GetResourceVersion(); rv != first.GetMetadata().GetResourceVersion() {
		t.Errorf("a ready network kept being written: resource version %d, then %d", first.GetMetadata().GetResourceVersion(), rv)
	}

	// Deleted, it comes back.
	if _, err := s.backend(registry.Network).Delete(user, resource.Key{Project: "web", Name: DefaultNetwork}, nil); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "a new default network", func() bool {
		n := getNetwork(t, s, system, DefaultNetwork)
		return n != nil && n.GetMetadata().GetUid() != first.GetMetadata().GetUid()
	})
}

func TestMachinesGetDistinctAddressesTheyKeep(t *testing.T) {
	s, system, user := controlled(t)
	seen := map[string]string{}
	for _, name := range []string{"a", "b", "c", "d"} {
		applyCtr(t, s, user, ctr(name))
		m := waitMachine(t, s, system, "ct-"+name, "addressed", func(m *computev1alpha1.Machine) bool { return ipv4Of(m) != "" })
		if other, dup := seen[ipv4Of(m)]; dup {
			t.Errorf("%s and %s share %s", name, other, ipv4Of(m))
		}
		seen[ipv4Of(m)] = name
	}
	if _, ok := seen["10.0.0.2/24"]; !ok {
		t.Errorf("the first address handed out is not 10.0.0.2: %v", seen)
	}

	// A change to the container keeps the address.
	a := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-a"})
	changed := ctr("a")
	changed.Spec.Image = "nginx:2"
	applyCtr(t, s, user, changed)
	a2 := waitMachine(t, s, system, "ct-a", "updated", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetContainer().GetImage() == "nginx:2"
	})
	if ipv4Of(a2) != ipv4Of(a) {
		t.Errorf("an update moved the address from %s to %s", ipv4Of(a), ipv4Of(a2))
	}

	// Once a machine is gone, its address is handed out again.
	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "a"}, nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.Machine, "ct-a")
	applyCtr(t, s, user, ctr("e"))
	e := waitMachine(t, s, system, "ct-e", "addressed", func(m *computev1alpha1.Machine) bool { return ipv4Of(m) != "" })
	if ipv4Of(e) != ipv4Of(a) {
		t.Errorf("the freed %s was not reused; got %s", ipv4Of(a), ipv4Of(e))
	}
}

func TestAMachineBeingDeletedKeepsItsAddress(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyCtr(t, s, user, ctr("a"))
	a := waitMachine(t, s, system, "ct-a", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "a"}, nil); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "ct-a", "marked", func(m *computev1alpha1.Machine) bool { return m.GetMetadata().GetDeleteTime() != nil })
	applyCtr(t, s, user, ctr("b"))
	b := waitMachine(t, s, system, "ct-b", "addressed", func(m *computev1alpha1.Machine) bool { return ipv4Of(m) != "" })
	if ipv4Of(b) == ipv4Of(a) {
		t.Errorf("a machine still on its node lost %s to a new one", ipv4Of(a))
	}
}

func TestConcurrentMachinesNeverShareAnAddress(t *testing.T) {
	s, system, user := running(t)
	const n = 20
	containers := make([]*computev1alpha1.Container, n)
	for i := range containers {
		applyCtr(t, s, user, ctr(fmt.Sprintf("c%d", i)))
		containers[i] = waitContainer(t, s, system, fmt.Sprintf("c%d", i), "stored", func(*computev1alpha1.Container) bool { return true })
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for _, c := range containers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ensureMachine(system, c, nil)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for _, c := range containers {
		addr := ipv4Of(getMachine(t, s, system, resource.Key{Project: "web", Name: machineName(c.GetMetadata().GetName())}))
		if seen[addr] {
			t.Errorf("%s handed out twice", addr)
		}
		seen[addr] = true
	}
}

// TestAFullNetworkSaysSo: a container that finds its network full says so,
// and gets a machine as soon as an address frees.
func TestAFullNetworkSaysSo(t *testing.T) {
	s, system, user := controlled(t)
	testutil.Eventually(t, "the default network", func() bool { return getNetwork(t, s, system, DefaultNetwork) != nil })
	// 10.0.0.0/24 has 253 addresses for machines: .2 to .254.
	for i := 2; i <= 254; i++ {
		m := machine(fmt.Sprintf("m%d", i), "")
		m.Spec.Interfaces[0].Ipv4 = fmt.Sprintf("10.0.0.%d/24", i)
		if _, err := s.backend(registry.Machine).Apply(system, m); err != nil {
			t.Fatal(err)
		}
	}
	applyCtr(t, s, user, ctr("late"))
	waitContainer(t, s, system, "late", "told the network is full", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == ReasonNoAddressLeft
	})

	if _, err := s.backend(registry.Machine).Delete(system, resource.Key{Project: "web", Name: "m7"}, nil); err != nil {
		t.Fatal(err)
	}
	m := waitMachine(t, s, system, "ct-late", "created once an address is free", func(m *computev1alpha1.Machine) bool { return ipv4Of(m) != "" })
	if ipv4Of(m) != "10.0.0.7/24" {
		t.Errorf("got %s, not the freed 10.0.0.7/24", ipv4Of(m))
	}
}

// TestANetworkInUseCannotBeDeleted: a user cannot delete a network machines
// are on, and can once they are gone.
func TestANetworkInUseCannotBeDeleted(t *testing.T) {
	s, system, user := controlled(t)
	applyCtr(t, s, user, ctr("api"))
	waitMachine(t, s, system, "ct-api", "addressed", func(m *computev1alpha1.Machine) bool { return ipv4Of(m) != "" })
	_, err := s.backend(registry.Network).Delete(user, resource.Key{Project: "web", Name: DefaultNetwork}, nil)
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "deleting a network in use")
	if err != nil && !strings.Contains(err.Error(), "ct-api") {
		t.Errorf("the refusal does not name the machine: %v", err)
	}
	if n := getNetwork(t, s, system, DefaultNetwork); n.GetMetadata().GetDeleteTime() != nil {
		t.Error("a refused delete marked the network")
	}

	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "api"}, nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.Machine, "ct-api")
	if _, err := s.backend(registry.Network).Delete(user, resource.Key{Project: "web", Name: DefaultNetwork}, nil); err != nil {
		t.Errorf("deleting a network no machine is on: %v", err)
	}
}

// TestAContainerProceedsOnceItsNetworkExists: a container waiting for its
// network is picked up when the network appears, not at the next resync.
func TestAContainerProceedsOnceItsNetworkExists(t *testing.T) {
	s, system, user := running(t)
	// Held by its finalizer, which no controller is running to remove.
	if _, err := s.backend(registry.Network).Delete(user, resource.Key{Project: "web", Name: DefaultNetwork}, nil); err != nil {
		t.Fatal(err)
	}
	applyCtr(t, s, user, ctr("api"))
	// With controllers running a default network is gone only for a moment,
	// so what the container says meanwhile is checked by hand.
	if err := s.reconcileContainer(system, controller.Key{Project: "web", Name: "api"}); err != nil {
		t.Fatal(err)
	}
	waitContainer(t, s, system, "api", "told it has no network", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == ReasonNetworkNotReady
	})

	startControllers(t, s, system)
	waitMachine(t, s, system, "ct-api", "created once the new default network exists", func(m *computev1alpha1.Machine) bool {
		return ipv4Of(m) != ""
	})
}

func TestTransitLinksAreDistinctAndReused(t *testing.T) {
	s, system, user := controlled(t)
	ready := func(name, what string) *networkv1alpha1.Network {
		t.Helper()
		var n *networkv1alpha1.Network
		testutil.Eventually(t, what, func() bool {
			n = getNetwork(t, s, system, name)
			return n != nil && conditions.IsTrue(n.GetStatus().GetConditions(), NetworkReady)
		})
		return n
	}
	first := ready(DefaultNetwork, "the default network's transit link")
	// Signing up made networks too; once they have their links, none takes
	// the one freed below.
	waitSettled(t, s)
	if _, err := s.backend(registry.Network).Apply(user, network("second", "10.9.0.0/24")); err != nil {
		t.Fatal(err)
	}
	if got := ready("second", "the second network's transit link").GetStatus().GetTransitIpv4(); got == "" || got == first.GetStatus().GetTransitIpv4() {
		t.Errorf("second transit %s, first %s", got, first.GetStatus().GetTransitIpv4())
	}

	// The default network goes and comes back; the new one gets the freed link.
	if _, err := s.backend(registry.Network).Delete(user, resource.Key{Project: "web", Name: DefaultNetwork}, nil); err != nil {
		t.Fatal(err)
	}
	var again *networkv1alpha1.Network
	testutil.Eventually(t, "a new default network with a transit link", func() bool {
		again = getNetwork(t, s, system, DefaultNetwork)
		return again != nil && again.GetMetadata().GetUid() != first.GetMetadata().GetUid() &&
			conditions.IsTrue(again.GetStatus().GetConditions(), NetworkReady)
	})
	if got, want := again.GetStatus().GetTransitIpv4(), first.GetStatus().GetTransitIpv4(); got != want {
		t.Errorf("the freed transit %s was not reused; got %s", want, got)
	}
}

// TestADeletedProjectsNetworkWaitsForItsMachines: deleting a project takes
// its networks with it, each kept until its machines have left their node.
func TestADeletedProjectsNetworkWaitsForItsMachines(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyCtr(t, s, user, ctr("api"))
	waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })

	if _, err := s.backend(registry.Project).Delete(user, resource.Key{Name: "web"}, nil); err != nil {
		t.Fatal(err)
	}
	m := waitMachine(t, s, system, "ct-api", "marked", func(m *computev1alpha1.Machine) bool { return m.GetMetadata().GetDeleteTime() != nil })
	testutil.Eventually(t, "the network to be marked", func() bool {
		n := getNetwork(t, s, system, DefaultNetwork)
		return n != nil && n.GetMetadata().GetDeleteTime() != nil
	})
	testutil.Unchanged(t, "a network with a machine still on it", 300*time.Millisecond,
		versionOf(s, system, registry.Network, resource.Key{Project: "web", Name: DefaultNetwork}))

	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration(),
		&metav1.Condition{Type: MachineReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned"})
	testutil.Eventually(t, "the network to go", func() bool { return getNetwork(t, s, system, DefaultNetwork) == nil })
}

func TestFirstFreeHostSkipsReservedAddresses(t *testing.T) {
	cidr := netip.MustParsePrefix("192.168.5.0/29")
	taken := map[netip.Addr]bool{netip.MustParseAddr("192.168.5.2"): true}
	got, ok := firstFreeHost(cidr, taken)
	if !ok || got != netip.MustParseAddr("192.168.5.3") {
		t.Errorf("got %v %t", got, ok)
	}
	for _, a := range []string{"192.168.5.3", "192.168.5.4", "192.168.5.5", "192.168.5.6"} {
		taken[netip.MustParseAddr(a)] = true
	}
	if got, ok := firstFreeHost(cidr, taken); ok {
		t.Errorf("handed out %v from a full /29", got)
	}
}

// TestADeletingProjectGetsNoNewNetwork: a project on its way out is not
// handed a default network again.
func TestADeletingProjectGetsNoNewNetwork(t *testing.T) {
	s, system, user := running(t)
	if _, err := s.backend(registry.Network).Delete(user, resource.Key{Project: "web", Name: DefaultNetwork}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileNetwork(system, controller.Key{Project: "web", Name: DefaultNetwork}); err != nil {
		t.Fatal(err)
	}
	// A network nobody releases keeps the project in deletion.
	if _, err := s.backend(registry.Network).Apply(user, network("other", "10.1.0.0/24")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Project).Delete(user, resource.Key{Name: "web"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileProject(system, controller.Key{Name: "web"}); err != nil {
		t.Fatalf("reconciling a deleting project: %v", err)
	}
	if getNetwork(t, s, system, DefaultNetwork) != nil {
		t.Error("a deleting project got a default network")
	}
}

// startControllers starts a second server, with controllers, over the store
// of one running without them: an apiserver starting over a store written
// while no controller ran.
func startControllers(t *testing.T, s *Server, system context.Context) {
	t.Helper()
	cfg := s.cfg
	cfg.DisableControllers = false
	next := New(cfg)
	ctx, cancel := context.WithCancel(system)
	done := make(chan error, 1)
	go func() { done <- next.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// TestMachinesWithoutAnAddressGetOne: machines written before addresses
// existed are given one when the apiserver starts.
func TestMachinesWithoutAnAddressGetOne(t *testing.T) {
	s, system, user := running(t)
	applyCtr(t, s, user, ctr("api"))
	c := waitContainer(t, s, system, "api", "stored", func(*computev1alpha1.Container) bool { return true })
	m := machine("ct-api", "")
	m.Metadata.OwnerReferences = []*metav1.OwnerReference{{
		ApiVersion: registry.Container.APIVersion(), Kind: registry.Container.Kind,
		Name: "api", Uid: c.GetMetadata().GetUid(), Controller: true,
	}}
	if _, err := s.backend(registry.Machine).Apply(system, m); err != nil {
		t.Fatal(err)
	}
	startControllers(t, s, system)
	waitMachine(t, s, system, "ct-api", "given an address", func(m *computev1alpha1.Machine) bool { return ipv4Of(m) == "10.0.0.2/24" })
}

// TestANetworkIsNotReleasedUnderAMachineJoiningIt: a network being deleted
// stays while a machine is being given an address on it.
func TestANetworkIsNotReleasedUnderAMachineJoiningIt(t *testing.T) {
	s, system, user := running(t)
	// What ensureMachine does: choose an address and write the machine,
	// holding ipam throughout.
	s.ipam.Lock()
	addr, err := s.freeAddress(system, "web", DefaultNetwork)
	if err != nil {
		s.ipam.Unlock()
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Network).Delete(user, resource.Key{Project: "web", Name: DefaultNetwork}, nil); err != nil {
		s.ipam.Unlock()
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() { released <- s.reconcileNetwork(system, controller.Key{Project: "web", Name: DefaultNetwork}) }()
	// The release waits on ipam; one that did not would finish meanwhile.
	testutil.Unchanged(t, "the network being deleted, ipam held", 50*time.Millisecond,
		versionOf(s, system, registry.Network, resource.Key{Project: "web", Name: DefaultNetwork}))
	m := machine("m", "")
	m.Spec.Interfaces[0].Ipv4 = addr
	_, err = s.backend(registry.Machine).Apply(system, m)
	s.ipam.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if getNetwork(t, s, system, DefaultNetwork) == nil {
		t.Errorf("the network went while machine m joined it with %s", addr)
	}
}

// TestCreateNeverOverwrites: a controller that read "absent" and creates
// leaves an object written in between as it is.
func TestCreateNeverOverwrites(t *testing.T) {
	s, system, user := running(t)
	mine := network("lab", "10.7.0.0/24")
	mine.Metadata.Labels = map[string]string{"team": "a"}
	if _, err := s.backend(registry.Network).Apply(user, mine); err != nil {
		t.Fatal(err)
	}
	_, err := s.backend(registry.Network).Create(system, network("lab", "10.7.0.0/24"))
	wantCodeOf(t, err, connect.CodeAlreadyExists, "creating over an existing network")
	n := getNetwork(t, s, system, "lab")
	if n.GetMetadata().GetLabels()["team"] != "a" || len(n.GetMetadata().GetFinalizers()) == 0 || len(n.GetMetadata().GetOwnerReferences()) == 0 {
		t.Errorf("the network was overwritten: %v", n.GetMetadata())
	}
}
