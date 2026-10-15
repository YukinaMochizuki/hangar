package apiserver

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
)

// bumpMachine writes a machine's status as the system would, so anything
// holding the machine as read before is now stale.
func bumpMachine(t *testing.T, s *Server, system context.Context, key resource.Key, status *computev1alpha1.MachineStatus) {
	t.Helper()
	if _, err := s.backend(registry.Machine).UpdateStatus(system, key, status, nil); err != nil {
		t.Fatal(err)
	}
}

func getMachine(t *testing.T, s *Server, system context.Context, key resource.Key) *computev1alpha1.Machine {
	t.Helper()
	obj, err := s.get(system, registry.Machine, key)
	if err != nil || obj == nil {
		t.Fatalf("machine %s: %v", key.Name, err)
	}
	return obj.(*computev1alpha1.Machine)
}

func TestAResizeThatDoesNotFitIsNotApplied(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 1024)
	applyCtr(t, s, user, ctr("api"))
	waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })

	applyCtr(t, s, user, sized("api", 8192))
	waitContainer(t, s, system, "api", "told the resize does not fit", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == "ResizeDoesNotFit"
	})
	if m := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"}); m.GetSpec().GetResources().GetMemory() != defaultMemory {
		t.Errorf("the node was overcommitted: the machine now asks %s", m.GetSpec().GetResources().GetMemory())
	}

	// A resize that fits goes through.
	applyCtr(t, s, user, sized("api", 1024))
	waitMachine(t, s, system, "ct-api", "resized", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetResources().GetMemory() == "1024Mi"
	})
}

func TestAMachineNameInUseIsReported(t *testing.T) {
	s, system, user := controlled(t)
	squatter := machine("ct-api", "")
	squatter.Metadata.OwnerReferences = []*metav1.OwnerReference{{
		ApiVersion: registry.Container.APIVersion(), Kind: registry.Container.Kind, Name: "api", Uid: "someone-else", Controller: true,
	}}
	squatter.Metadata.Finalizers = []string{"test/hold"}
	if _, err := s.backend(registry.Machine).Apply(system, squatter); err != nil {
		t.Fatal(err)
	}
	applyCtr(t, s, user, ctr("api"))
	waitContainer(t, s, system, "api", "told its machine name is taken", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == "MachineNameInUse"
	})
}

func TestAContainerWhoseNodeStopsReportingIsNotReady(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyCtr(t, s, user, ctr("api"))
	m := waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration())
	waitContainer(t, s, system, "api", "ready", func(c *computev1alpha1.Container) bool {
		return conditions.IsTrue(c.GetStatus().GetConditions(), ContainerReady)
	})

	if _, err := s.backend(registry.Node).UpdateStatus(system, resource.Key{Name: "n1"}, &computev1alpha1.NodeStatus{
		Capacity: &computev1alpha1.NodeResources{Memory: "4096Mi"},
		Conditions: []*metav1.Condition{{
			Type: NodeReady, Status: metav1.ConditionStatus_CONDITION_STATUS_UNKNOWN, Reason: "NodeStatusUnknown",
		}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	waitContainer(t, s, system, "api", "unknown with its node", func(c *computev1alpha1.Container) bool {
		r := containerReady(c)
		return r.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_UNKNOWN && r.GetReason() == "NodeNotReporting"
	})
}

// TestAContainerSaysWhatItsNodeSays: what keeps a machine from running, as
// its node reports it for the current spec, is what its container says.
func TestAContainerSaysWhatItsNodeSays(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyCtr(t, s, user, ctr("api"))
	m := waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	gen := m.GetMetadata().GetGeneration()
	pullFailed := &metav1.Condition{
		Type: computeapi.MachineReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE,
		Reason: computeapi.ReasonImagePullFailed, Message: "nginx: manifest unknown", ObservedGeneration: gen,
	}
	// Not yet applied, failing.
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_CREATING, gen-1, pullFailed)
	waitContainer(t, s, system, "api", "told the image pull failed", func(c *computev1alpha1.Container) bool {
		r := containerReady(c)
		return r.GetReason() == computeapi.ReasonImagePullFailed && r.GetMessage() == "nginx: manifest unknown"
	})

	// Applied, then exited.
	m = getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"})
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_FAILED, gen, &metav1.Condition{
		Type: computeapi.MachineReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE,
		Reason: computeapi.ReasonExited, Message: "the container exited with code 3", ObservedGeneration: gen,
	})
	waitContainer(t, s, system, "api", "told it exited", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetMessage() == "the container exited with code 3"
	})

	// Running, but the node says it is cut off.
	m = getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"})
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, gen, &metav1.Condition{
		Type: computeapi.MachineReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE,
		Reason: computeapi.ReasonNetworkFailed, Message: "netlink: no buffer space", ObservedGeneration: gen,
	})
	waitContainer(t, s, system, "api", "told its network failed though running", func(c *computev1alpha1.Container) bool {
		r := containerReady(c)
		return r.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE && r.GetReason() == computeapi.ReasonNetworkFailed
	})

	// What the node said of an older spec is not passed on.
	pullFailed.ObservedGeneration = gen - 1
	m = getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"})
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_CREATING, gen-1, pullFailed)
	waitContainer(t, s, system, "api", "told it is updating", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == "Updating"
	})
}

func TestFreedMemoryPlacesAWaitingMachine(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 1024)
	applyCtr(t, s, user, sized("a", 1024))
	waitMachine(t, s, system, "ct-a", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	applyCtr(t, s, user, sized("b", 1024))
	waitMachine(t, s, system, "ct-b", "waiting", func(m *computev1alpha1.Machine) bool {
		return scheduled(m).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE
	})

	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "a"}, nil); err != nil {
		t.Fatal(err)
	}
	a := waitMachine(t, s, system, "ct-a", "marked", func(m *computev1alpha1.Machine) bool { return m.GetMetadata().GetDeleteTime() != nil })
	reportMachine(t, s, system, a, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, a.GetMetadata().GetGeneration(),
		&metav1.Condition{Type: MachineReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned"})
	// No node writes anything now: the release alone must place b.
	waitMachine(t, s, system, "ct-b", "placed once memory was freed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
}

func TestAContainerOfTheSameNameGetsANewMachine(t *testing.T) {
	s, system, user := controlled(t)
	applyCtr(t, s, user, ctr("api"))
	first := waitMachine(t, s, system, "ct-api", "created", func(*computev1alpha1.Machine) bool { return true })
	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "api"}, nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.Container, "api")
	applyCtr(t, s, user, ctr("api"))
	waitMachine(t, s, system, "ct-api", "recreated", func(m *computev1alpha1.Machine) bool {
		return m.GetMetadata().GetUid() != first.GetMetadata().GetUid()
	})
}

// TestAMachineMadeForAContainerAlreadyGoneIsCollected: the container
// controller may create a machine just as its container goes; garbage
// collection must not wait for the next sweep to remove it.
func TestAMachineMadeForAContainerAlreadyGoneIsCollected(t *testing.T) {
	s, system, _ := running(t)
	orphan := machine("ct-ghost", "")
	orphan.Metadata.OwnerReferences = []*metav1.OwnerReference{{
		ApiVersion: registry.Container.APIVersion(), Kind: registry.Container.Kind, Name: "ghost", Uid: "gone", Controller: true,
	}}
	if _, err := s.backend(registry.Machine).Apply(system, orphan); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.Machine, "ct-ghost")
}

// TestNoMachineForAContainerBeingDeleted: a container already on its way out
// gets no machine, even from a controller that sees it for the first time.
func TestNoMachineForAContainerBeingDeleted(t *testing.T) {
	s, system, user := running(t)
	held := ctr("api")
	held.Metadata.Finalizers = []string{"test/hold"}
	if _, err := s.backend(registry.Container).Apply(system, held); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "api"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileContainer(system, controller.Key{Project: "web", Name: "api"}); err != nil {
		t.Fatal(err)
	}
	if obj, _ := s.get(system, registry.Machine, resource.Key{Project: "web", Name: "ct-api"}); obj != nil {
		t.Error("a container being deleted got a machine")
	}
}

// TestAMachineBeingDeletedIsLeftAlone: while a container's machine waits for
// its node, changes to the container are reported, not forced onto it.
func TestAMachineBeingDeletedIsLeftAlone(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyCtr(t, s, user, ctr("api"))
	waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	if _, err := s.backend(registry.Machine).Delete(system, resource.Key{Project: "web", Name: "ct-api"}, nil); err != nil {
		t.Fatal(err)
	}
	changed := ctr("api")
	changed.Spec.Image = "nginx:2"
	applyCtr(t, s, user, changed)
	waitContainer(t, s, system, "api", "reported at the new generation", func(c *computev1alpha1.Container) bool {
		return c.GetStatus().GetObservedGeneration() == 2
	})
	if m := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"}); m.GetSpec().GetContainer().GetImage() != "nginx" {
		t.Errorf("a machine being deleted was rewritten to %s", m.GetSpec().GetContainer().GetImage())
	}
}

// TestMemoryDecisionsTakeTurns: a resize check and a placement decided at
// the same time could each see the same free memory and together overcommit
// the node; both wait for the memory lock.
func TestMemoryDecisionsTakeTurns(t *testing.T) {
	s, system, user := runningWith(t, func(c *Config) { c.Nodes = []string{"n1"}; c.NodeMonitorInterval = 1000 * time.Hour })
	waitNode(t, s, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyCtr(t, s, user, ctr("api"))
	c := waitContainer(t, s, system, "api", "stored", func(*computev1alpha1.Container) bool { return true })
	if _, err := s.backend(registry.Machine).Apply(system, machine("ct-api", "n1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Machine).Apply(system, machine("ct-other", "")); err != nil {
		t.Fatal(err)
	}
	placed := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"})
	placed.Metadata.OwnerReferences = []*metav1.OwnerReference{{
		ApiVersion: registry.Container.APIVersion(), Kind: registry.Container.Kind, Name: "api", Uid: c.GetMetadata().GetUid(), Controller: true,
	}}
	grown := sized("api", 1024)
	grown.Metadata = c.GetMetadata()

	s.capacity.Lock()
	done := make(chan string, 2)
	go func() {
		_, _ = s.ensureMachine(system, grown, placed)
		done <- "resize"
	}()
	go func() {
		_ = s.scheduleMachine(system, resource.Key{Project: "web", Name: "ct-other"}, getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-other"}))
		done <- "placement"
	}()
	finished := 0
	select {
	case what := <-done:
		t.Errorf("a %s was decided while another memory decision held the lock", what)
		finished++
	case <-time.After(200 * time.Millisecond):
	}
	s.capacity.Unlock()
	for ; finished < 2; finished++ {
		<-done
	}
}

// TestABlockedResizeGoesThroughOnceThereIsRoom: memory freed on the node is
// enough to retry a resize that did not fit; no heartbeat is needed.
func TestABlockedResizeGoesThroughOnceThereIsRoom(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 2048)
	applyCtr(t, s, user, ctr("api"))
	applyCtr(t, s, user, sized("other", 1024))
	waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	waitMachine(t, s, system, "ct-other", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })

	applyCtr(t, s, user, sized("api", 1536))
	waitContainer(t, s, system, "api", "blocked", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == "ResizeDoesNotFit"
	})
	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "other"}, nil); err != nil {
		t.Fatal(err)
	}
	other := waitMachine(t, s, system, "ct-other", "marked", func(m *computev1alpha1.Machine) bool { return m.GetMetadata().GetDeleteTime() != nil })
	reportMachine(t, s, system, other, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, other.GetMetadata().GetGeneration(),
		&metav1.Condition{Type: MachineReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned"})
	waitMachine(t, s, system, "ct-api", "resized", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetResources().GetMemory() == "1536Mi"
	})
}

// TestCollectingAnOrphanSparesALaterObjectOfTheSameName: an event about an
// orphan that is already gone must not take its successor with it.
func TestCollectingAnOrphanSparesALaterObjectOfTheSameName(t *testing.T) {
	s, system, _ := running(t)
	key := resource.Key{Project: "web", Name: "ct-api"}
	if _, err := s.backend(registry.Machine).Apply(system, machine("ct-api", "")); err != nil {
		t.Fatal(err)
	}
	earlier := machine("ct-api", "")
	earlier.Metadata.Uid = "an-earlier-machine"
	earlier.Metadata.OwnerReferences = []*metav1.OwnerReference{{
		ApiVersion: registry.Container.APIVersion(), Kind: registry.Container.Kind, Name: "gone", Uid: "gone", Controller: true,
	}}
	s.collectIfOrphaned(system, s.backend(registry.Machine), key, earlier)
	if obj, _ := s.get(system, registry.Machine, key); obj == nil {
		t.Error("an orphan's event collected the live machine that took its name")
	}
}

// The race tests below hand a reconcile step an object as it was read before
// someone else wrote it. The step must fail rather than overwrite.

func TestSettingScheduledNeverOverwritesANodeReport(t *testing.T) {
	s, system, _ := running(t)
	key := resource.Key{Project: "web", Name: "ct-api"}
	if _, err := s.backend(registry.Machine).Apply(system, machine("ct-api", "n1")); err != nil {
		t.Fatal(err)
	}
	stale := getMachine(t, s, system, key)
	bumpMachine(t, s, system, key, &computev1alpha1.MachineStatus{Phase: computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING})

	err := s.setScheduled(system, key, stale, &metav1.Condition{Type: MachineScheduled, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Placed"})
	if err == nil {
		t.Error("a status write from stale data succeeded")
	}
	if m := getMachine(t, s, system, key); m.GetStatus().GetPhase() != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING {
		t.Errorf("the node's report was overwritten: phase %v", m.GetStatus().GetPhase())
	}
}

func TestPlacingIsConditionalOnTheMachineAsRead(t *testing.T) {
	s, system, _ := runningWith(t, func(c *Config) { c.Nodes = []string{"n1"}; c.NodeMonitorInterval = 1000 * time.Hour })
	waitNode(t, s, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	key := resource.Key{Project: "web", Name: "ct-api"}
	if _, err := s.backend(registry.Machine).Apply(system, machine("ct-api", "")); err != nil {
		t.Fatal(err)
	}
	stale := getMachine(t, s, system, key)
	bumpMachine(t, s, system, key, &computev1alpha1.MachineStatus{Phase: computev1alpha1.MachinePhase_MACHINE_PHASE_PENDING})

	if err := s.scheduleMachine(system, key, stale); connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("placing from stale data: %v, want aborted", err)
	}
	if m := getMachine(t, s, system, key); m.GetSpec().GetNode() != "" {
		t.Errorf("a stale placement was written: node %s", m.GetSpec().GetNode())
	}
}

func TestUpdatingAMachineIsConditionalOnTheMachineAsRead(t *testing.T) {
	s, system, user := running(t)
	applyCtr(t, s, user, ctr("api"))
	c := waitContainer(t, s, system, "api", "stored", func(*computev1alpha1.Container) bool { return true })
	m, err := s.ensureMachine(system, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := resource.Key{Project: "web", Name: m.GetMetadata().GetName()}
	bumpMachine(t, s, system, key, &computev1alpha1.MachineStatus{Phase: computev1alpha1.MachinePhase_MACHINE_PHASE_PENDING})

	changed := ctr("api")
	changed.Spec.Image = "nginx:2"
	applyCtr(t, s, user, changed)
	c = waitContainer(t, s, system, "api", "changed", func(c *computev1alpha1.Container) bool { return c.GetSpec().GetImage() == "nginx:2" })
	if _, err := s.ensureMachine(system, c, m); connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("updating from a stale machine: %v, want aborted", err)
	}
}

func TestReportingIsConditionalOnTheContainerAsRead(t *testing.T) {
	s, system, user := running(t)
	applyCtr(t, s, user, ctr("api"))
	stale := waitContainer(t, s, system, "api", "stored", func(*computev1alpha1.Container) bool { return true })
	key := resource.Key{Project: "web", Name: "api"}
	if _, err := s.backend(registry.Container).UpdateStatus(system, key, &computev1alpha1.ContainerStatus{ObservedGeneration: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.reportContainer(system, stale, machine("ct-api", ""), nil, nil, ""); connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("reporting from a stale container: %v, want aborted", err)
	}
}
