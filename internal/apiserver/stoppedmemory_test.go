package apiserver

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	monitoringv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/monitoring/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

const (
	ctrRunning = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_RUNNING
	ctrStopped = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED
	mRunning   = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING
	mStopped   = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
)

// placedOn waits until the container's machine is on node, and returns it.
func placedOn(t *testing.T, s *Server, system context.Context, name, node string) *computev1alpha1.Machine {
	t.Helper()
	return waitMachine(t, s, system, machineName(name), "placed on "+node, func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == node })
}

// stopCtr asks the container to stop and, as its node does, stops its
// machine at the spec asking it to.
func stopCtr(t *testing.T, s *Server, system, user context.Context, c *computev1alpha1.Container) *computev1alpha1.Machine {
	t.Helper()
	applyCtr(t, s, user, withRunState(c, ctrStopped))
	m := waitMachine(t, s, system, machineName(c.GetMetadata().GetName()), "asked to stop", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetRunState() == mStopped
	})
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration())
	return m
}

// unschedulable waits until the container's machine is told no node has
// room for it.
func unschedulable(t *testing.T, s *Server, system context.Context, name string) {
	t.Helper()
	waitMachine(t, s, system, machineName(name), "unschedulable", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "" && scheduled(m).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE
	})
}

// TestANodeFullOfStoppedMachinesPlacesANewOne: a machine its node has
// stopped gives back its memory, and keeps its node and its disk; one still
// stopping holds its memory until the node says it has stopped.
func TestANodeFullOfStoppedMachinesPlacesANewOne(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 2048)
	applyCtr(t, s, user, sized("a", 1024))
	applyCtr(t, s, user, sized("b", 1024))
	placedOn(t, s, system, "a", "n1")
	placedOn(t, s, system, "b", "n1")

	// Asked to stop, but not stopped yet: the VM may still run.
	applyCtr(t, s, user, withRunState(sized("a", 1024), ctrStopped))
	a := waitMachine(t, s, system, "ct-a", "asked to stop", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetRunState() == mStopped })
	applyCtr(t, s, user, sized("c", 1024))
	unschedulable(t, s, system, "c")

	// Stopped by its node, a has given its memory back.
	reportMachine(t, s, system, a, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, a.GetMetadata().GetGeneration())
	placedOn(t, s, system, "c", "n1")
	if m := getMachine(t, s, system, resource.Key{Project: "web", Name: a.GetMetadata().GetName()}); m.GetSpec().GetNode() != "n1" {
		t.Errorf("the stopped machine left its node for %q", m.GetSpec().GetNode())
	}

	// Stopping b too makes room for d.
	stopCtr(t, s, system, user, sized("b", 1024))
	applyCtr(t, s, user, sized("d", 1024))
	placedOn(t, s, system, "d", "n1")
}

// TestAStartWaitsForRoomOnItsNode: a stopped container started on a node
// with no room for it stays stopped and says why, and starts by itself once
// another machine there stops.
func TestAStartWaitsForRoomOnItsNode(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 1024)
	applyCtr(t, s, user, sized("a", 1024))
	placedOn(t, s, system, "a", "n1")
	stopCtr(t, s, system, user, sized("a", 1024))
	applyCtr(t, s, user, sized("b", 1024))
	placedOn(t, s, system, "b", "n1")

	applyCtr(t, s, user, withRunState(sized("a", 1024), ctrRunning))
	c := waitContainer(t, s, system, "a", "waiting for room", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == computeapi.ReasonStartDoesNotFit
	})
	want := "node n1 has 0Mi of memory free for this machine, not 1024Mi; it starts once there is room"
	if got := containerReady(c).GetMessage(); got != want {
		t.Errorf("Ready says %q, want %q", got, want)
	}
	if st := containerStopped(c); st.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || st.GetReason() != computeapi.ReasonStartDoesNotFit {
		t.Errorf("Stopped is %v", st)
	}
	if m := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-a"}); m.GetSpec().GetRunState() != mStopped {
		t.Errorf("the node was overcommitted: the machine is asked to %v", m.GetSpec().GetRunState())
	}

	// b stopping is room enough; no heartbeat is needed.
	stopCtr(t, s, system, user, sized("b", 1024))
	waitMachine(t, s, system, "ct-a", "started", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetRunState() == mRunning })
}

// TestAStartBeforeTheStopTookEffectKeepsItsPlace: a machine started again
// before its node stopped it never gave its memory back, so nothing could
// have taken it.
func TestAStartBeforeTheStopTookEffectKeepsItsPlace(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 1024)
	applyCtr(t, s, user, sized("a", 1024))
	placedOn(t, s, system, "a", "n1")
	applyCtr(t, s, user, withRunState(sized("a", 1024), ctrStopped))
	waitMachine(t, s, system, "ct-a", "asked to stop", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetRunState() == mStopped })
	applyCtr(t, s, user, sized("b", 1024))
	unschedulable(t, s, system, "b")

	applyCtr(t, s, user, withRunState(sized("a", 1024), ctrRunning))
	waitMachine(t, s, system, "ct-a", "started", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetRunState() == mRunning })
	waitSettled(t, s)
	if m := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-b"}); m.GetSpec().GetNode() != "" {
		t.Errorf("b was placed on %s, where a runs", m.GetSpec().GetNode())
	}
}

// TestAStoppedMachineGrowsItsMemoryWhenStarted: memory a stopped machine
// asks for is not refused, though the node has no room for it, and is
// checked as the machine starts. Until its node observes the change the
// machine holds that memory again; the node here never does.
func TestAStoppedMachineGrowsItsMemoryWhenStarted(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 1024)
	applyCtr(t, s, user, sized("a", 1024))
	placedOn(t, s, system, "a", "n1")
	stopCtr(t, s, system, user, sized("a", 1024))

	applyCtr(t, s, user, withRunState(sized("a", 2048), ctrStopped))
	waitMachine(t, s, system, "ct-a", "grown while stopped", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetResources().GetMemory() == "2048Mi"
	})
	applyCtr(t, s, user, sized("a", 2048))
	waitContainer(t, s, system, "a", "waiting for room", func(c *computev1alpha1.Container) bool {
		r := containerReady(c)
		return r.GetReason() == computeapi.ReasonStartDoesNotFit && r.GetMessage() == "node n1 has 1024Mi of memory free for this machine, not 2048Mi; it starts once there is room"
	})

	// A start that fits goes through.
	applyCtr(t, s, user, sized("a", 1024))
	waitMachine(t, s, system, "ct-a", "started", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetRunState() == mRunning && m.GetSpec().GetResources().GetMemory() == "1024Mi"
	})
}

// TestAStartGoesThroughOnceANeighbourIsDeleted: a machine going from the
// node, once released, makes room for a start waiting there.
func TestAStartGoesThroughOnceANeighbourIsDeleted(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 1024)
	applyCtr(t, s, user, sized("a", 1024))
	placedOn(t, s, system, "a", "n1")
	stopCtr(t, s, system, user, sized("a", 1024))
	applyCtr(t, s, user, sized("b", 1024))
	placedOn(t, s, system, "b", "n1")
	applyCtr(t, s, user, sized("a", 1024))
	waitContainer(t, s, system, "a", "waiting for room", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == computeapi.ReasonStartDoesNotFit
	})

	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "b"}, nil); err != nil {
		t.Fatal(err)
	}
	b := waitMachine(t, s, system, "ct-b", "marked", func(m *computev1alpha1.Machine) bool { return m.GetMetadata().GetDeleteTime() != nil })
	reportMachine(t, s, system, b, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, b.GetMetadata().GetGeneration(),
		&metav1.Condition{Type: MachineReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned"})
	waitMachine(t, s, system, "ct-a", "started", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetRunState() == mRunning })
}

// TestAVMStartWaitsForRoomOnItsNode: a VM, bound to the node its root disk
// is on, waits there to start as a container does.
func TestAVMStartWaitsForRoomOnItsNode(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 1024)
	madeDisk(t, s, system, user, "db-root", "n1", "10Gi")
	vm := vmOfDisk("db", "db-root")
	vm.Spec.Resources = &computev1alpha1.VirtualMachineResources{Cpu: "1", Memory: "1Gi"}
	vm.Spec.RunState = computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED
	vms := s.backend(registry.VirtualMachine)
	if _, err := vms.Apply(user, vm); err != nil {
		t.Fatal(err)
	}
	m := waitMachine(t, s, system, "vm-db", "placed, asked to stop", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "n1" && m.GetSpec().GetRunState() == mStopped
	})
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration())
	applyCtr(t, s, user, sized("a", 1024))
	placedOn(t, s, system, "a", "n1")

	vm = proto.Clone(vm).(*computev1alpha1.VirtualMachine)
	vm.Spec.RunState = computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_RUNNING
	if _, err := vms.Apply(user, vm); err != nil {
		t.Fatal(err)
	}
	waitVM(t, s, system, "db", "waiting for room", func(vm *computev1alpha1.VirtualMachine) bool {
		return vmReady(vm).GetReason() == computeapi.ReasonStartDoesNotFit &&
			conditions.Find(vm.GetStatus().GetConditions(), VMStopped).GetReason() == computeapi.ReasonStartDoesNotFit
	})
	if m := getMachine(t, s, system, resource.Key{Project: "web", Name: "vm-db"}); m.GetSpec().GetRunState() != mStopped {
		t.Errorf("the node was overcommitted: the machine is asked to %v", m.GetSpec().GetRunState())
	}

	stopCtr(t, s, system, user, sized("a", 1024))
	waitMachine(t, s, system, "vm-db", "started", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetRunState() == mRunning })
}

// TestNodeUsageSummaryCountsStoppedMachines: a machine its node has stopped
// counts among the node's machines and holds its disk, but neither its
// memory nor its vCPUs, which are summed apart as what it takes back if
// started.
func TestNodeUsageSummaryCountsStoppedMachines(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 8192, 100)
	a := withResources("a", &computev1alpha1.Resources{Cpu: "2", Memory: "1Gi", Disk: "10Gi"})
	b := withResources("b", &computev1alpha1.Resources{Cpu: "1", Memory: "512Mi", Disk: "5Gi"})
	applyCtr(t, s, user, a)
	applyCtr(t, s, user, b)
	placedOn(t, s, system, "a", "n1")
	placedOn(t, s, system, "b", "n1")
	stopCtr(t, s, system, user, proto.Clone(a).(*computev1alpha1.Container))

	got, err := s.nodeUsageSummary(system)
	if err != nil {
		t.Fatal(err)
	}
	want := &monitoringv1alpha1.NodeUsage{Node: "n1", Machines: 2, Cpu: "1", Memory: "512Mi", Disk: "15Gi", StoppedMachines: 1, StoppedMemory: "1Gi"}
	if len(got) != 1 || !proto.Equal(got[0], want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
