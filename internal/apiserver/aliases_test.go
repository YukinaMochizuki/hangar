package apiserver

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
)

func aliasedCtr(name string, aliases ...string) *computev1alpha1.Container {
	c := ctr(name)
	c.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: "default", Aliases: aliases}}
	return c
}

func TestAliasAdmission(t *testing.T) {
	t.Parallel()
	s, _, user := running(t)
	b := s.backend(registry.Container)
	nine := make([]string, 9)
	for i := range nine {
		nine[i] = fmt.Sprintf("a%d", i)
	}
	for name, tc := range map[string]struct {
		aliases []string
		ok      bool
	}{
		"none":            {nil, true},
		"eight":           {nine[:8], true},
		"more than eight": {nine, false},
		"twice":           {[]string{"db", "db"}, false},
		"not a label":     {[]string{"db.internal"}, false},
		"upper case":      {[]string{"DB"}, false},
		"empty":           {[]string{""}, false},
	} {
		_, err := b.Apply(user, aliasedCtr("api", tc.aliases...))
		if got := err == nil; got != tc.ok {
			t.Errorf("%s: accepted %t, want %t (%v)", name, got, tc.ok, err)
		}
		if !tc.ok && connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
}

// TestAMachineBehindItsContainerHoldsItsAliases: a container whose resize
// does not fit keeps its machine as it was, aliases and all, which its
// network's resolver goes on answering; so the alias it gave up is refused
// to another until its machine gives it up too.
func TestAMachineBehindItsContainerHoldsItsAliases(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 1024)
	applyCtr(t, s, user, aliasedCtr("api", "db"))
	waitMachine(t, s, system, "ct-api", "placed, holding db", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "n1" && slices.Equal(m.GetSpec().GetInterfaces()[0].GetAliases(), []string{"db"})
	})

	applyCtr(t, s, user, sized("api", 8192))
	waitContainer(t, s, system, "api", "told the resize does not fit", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == "ResizeDoesNotFit"
	})
	if got := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"}).GetSpec().GetInterfaces()[0].GetAliases(); !slices.Equal(got, []string{"db"}) {
		t.Fatalf("the machine's aliases %v, want it still holding db", got)
	}
	_, err := s.backend(registry.Container).Apply(user, aliasedCtr("web", "db"))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "container api's machine ct-api") {
		t.Errorf("an alias the container gave up but its machine holds: %v, want it refused naming the machine", err)
	}
	// The container itself is not held back by its own machine.
	applyCtr(t, s, user, aliasedCtr("api", "db", "cache"))

	// A resize that fits takes the machine with it; db is free.
	applyCtr(t, s, user, sized("api", 1024))
	waitMachine(t, s, system, "ct-api", "resized, holding no alias", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetResources().GetMemory() == "1024Mi" && len(m.GetSpec().GetInterfaces()[0].GetAliases()) == 0
	})
	applyCtr(t, s, user, aliasedCtr("web", "db"))
}

// TestAnAliasIsHeldUntilItsHolderIsGone: a container being deleted holds
// its aliases, as its machine still answers them, until it is gone.
func TestAnAliasIsHeldUntilItsHolderIsGone(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyCtr(t, s, user, aliasedCtr("api", "db"))
	// Placed, so its deletion waits for its node to let go.
	waitMachine(t, s, system, "ct-api", "placed, holding db", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "n1" && slices.Equal(m.GetSpec().GetInterfaces()[0].GetAliases(), []string{"db"})
	})
	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "api"}, nil); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "ct-api", "being deleted", func(m *computev1alpha1.Machine) bool { return m.GetMetadata().GetDeleteTime() != nil })
	_, err := s.backend(registry.Container).Apply(user, aliasedCtr("web", "db"))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "container api") {
		t.Errorf("an alias of a container being deleted: %v, want it refused naming it", err)
	}
	// The node lets go of the machine, as it does once it has stopped it.
	if err := s.RemoveFinalizer(system, registry.Machine, resource.Key{Project: "web", Name: "ct-api"}, NodeCleanupFinalizer); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.Container, "api")
	waitGone(t, s, system, registry.Machine, "ct-api")
	applyCtr(t, s, user, aliasedCtr("web", "db"))
}

// TestOneAliasOneMachineAcrossKindsUnderRacingApplies: containers and VMs
// applied at once, all asking for one alias on one network: exactly one of
// them has it.
func TestOneAliasOneMachineAcrossKindsUnderRacingApplies(t *testing.T) {
	t.Parallel()
	s, system, _ := running(t)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	const n = 8
	errs := make([]error, 2*n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, errs[i] = s.backend(registry.Container).Apply(system, aliasedCtr(fmt.Sprintf("c-%d", i), "db"))
		})
		wg.Go(func() {
			vm := vmFromImage(fmt.Sprintf("v-%d", i), "noble")
			vm.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: "default", Aliases: []string{"db"}}}
			_, errs[n+i] = s.backend(registry.VirtualMachine).Apply(system, vm)
		})
	}
	wg.Wait()
	var won int
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case connect.CodeOf(err) == connect.CodeFailedPrecondition && strings.Contains(err.Error(), `alias "db"`):
		default:
			t.Errorf("an apply: %v", err)
		}
	}
	holders := 0
	for i := range n {
		if obj, err := s.get(system, registry.Container, resource.Key{Project: "web", Name: fmt.Sprintf("c-%d", i)}); err != nil {
			t.Fatal(err)
		} else if obj != nil {
			holders++
		}
		if obj, err := s.get(system, registry.VirtualMachine, resource.Key{Project: "web", Name: fmt.Sprintf("v-%d", i)}); err != nil {
			t.Fatal(err)
		} else if obj != nil {
			holders++
		}
	}
	if won != 1 || holders != 1 {
		t.Errorf("%d applies won and %d machines hold the alias; want one", won, holders)
	}
}

// TestOneAliasOneMachineUnderRacingApplies: containers applied at once,
// all asking for one alias on one network: exactly one has it. As the
// system, whose writes hold no quota lock, so attach is all that keeps them
// apart.
func TestOneAliasOneMachineUnderRacingApplies(t *testing.T) {
	t.Parallel()
	s, system, _ := running(t)
	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, errs[i] = s.backend(registry.Container).Apply(system, aliasedCtr(fmt.Sprintf("t-%d", i), "db"))
		})
	}
	wg.Wait()
	var won int
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case connect.CodeOf(err) == connect.CodeFailedPrecondition:
		default:
			t.Errorf("an apply: %v", err)
		}
	}
	holders := 0
	for i := range n {
		obj, err := s.get(system, registry.Container, resource.Key{Project: "web", Name: fmt.Sprintf("t-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if c, ok := obj.(*computev1alpha1.Container); ok && slices.Contains(c.GetSpec().GetInterfaces()[0].GetAliases(), "db") {
			holders++
		}
	}
	if won != 1 || holders != 1 {
		t.Errorf("%d applies won and %d containers hold the alias; want one", won, holders)
	}
}
