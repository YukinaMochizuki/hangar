package apiserver

import (
	"testing"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

func containerStopped(c *computev1alpha1.Container) *metav1.Condition {
	return conditions.Find(c.GetStatus().GetConditions(), ContainerStopped)
}

// stoppedIs reports whether the container's Stopped condition has the status
// and reason, computed for its current generation.
func stoppedIs(c *computev1alpha1.Container, status metav1.ConditionStatus, reason string) bool {
	s := containerStopped(c)
	return s.GetStatus() == status && s.GetReason() == reason && s.GetObservedGeneration() == c.GetMetadata().GetGeneration()
}

func withRunState(c *computev1alpha1.Container, rs computev1alpha1.ContainerRunState) *computev1alpha1.Container {
	c.Spec.RunState = rs
	return c
}

func nodeStopped(gen int64) *metav1.Condition {
	return &metav1.Condition{
		Type: computeapi.MachineReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE,
		Reason: computeapi.ReasonStopped, Message: "the machine is stopped", ObservedGeneration: gen,
	}
}

// TestARunStateReachesTheMachine: a container's run state, RUNNING when
// unset, is its machine's; its restart policy is left as it is; and the
// container says Stopped only once its node has stopped the machine at the
// spec that asks for it.
func TestARunStateReachesTheMachine(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	running := ctr("api")
	running.Spec.RestartPolicy = computev1alpha1.RestartPolicy_RESTART_POLICY_ON_FAILURE
	applyCtr(t, s, user, running)
	m := waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	if m.GetSpec().GetRunState() != computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING {
		t.Errorf("an unset run state made a machine %v", m.GetSpec().GetRunState())
	}
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration())
	waitContainer(t, s, system, "api", "running, not stopped", func(c *computev1alpha1.Container) bool {
		return conditions.IsTrue(c.GetStatus().GetConditions(), ContainerReady) &&
			stoppedIs(c, metav1.ConditionStatus_CONDITION_STATUS_FALSE, "RunStateRunning")
	})

	// Stopping is a new machine generation, the policy untouched.
	gen := m.GetMetadata().GetGeneration()
	stop := ctr("api")
	stop.Spec.RestartPolicy = computev1alpha1.RestartPolicy_RESTART_POLICY_ON_FAILURE
	applyCtr(t, s, user, withRunState(stop, computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED))
	m = waitMachine(t, s, system, "ct-api", "asked to stop", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetRunState() == computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	})
	if m.GetMetadata().GetGeneration() != gen+1 || m.GetSpec().GetRestartPolicy() != computev1alpha1.RestartPolicy_RESTART_POLICY_ON_FAILURE {
		t.Errorf("stopping: generation %d (was %d), restart policy %v", m.GetMetadata().GetGeneration(), gen, m.GetSpec().GetRestartPolicy())
	}
	waitContainer(t, s, system, "api", "stopping", func(c *computev1alpha1.Container) bool {
		return stoppedIs(c, metav1.ConditionStatus_CONDITION_STATUS_FALSE, "Stopping") && containerReady(c).GetReason() == "Updating"
	})

	// A stopped VM reported for the older spec is not the stop asked for,
	// even with the node's condition written at the newer one, as the node
	// dates every condition it writes.
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, gen, nodeStopped(gen+1))
	waitContainer(t, s, system, "api", "told the report, still stopping", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == computeapi.ReasonStopped &&
			stoppedIs(c, metav1.ConditionStatus_CONDITION_STATUS_FALSE, "Stopping")
	})
	m = getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"})
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, gen+1, nodeStopped(gen+1))
	c := waitContainer(t, s, system, "api", "stopped", func(c *computev1alpha1.Container) bool {
		return stoppedIs(c, metav1.ConditionStatus_CONDITION_STATUS_TRUE, computeapi.ReasonStopped)
	})
	if r := containerReady(c); r.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || r.GetReason() != computeapi.ReasonStopped {
		t.Errorf("a stopped container's Ready: %v", r)
	}

	// A node that stops reporting can no longer vouch for the stop.
	silent := &computev1alpha1.NodeStatus{Conditions: []*metav1.Condition{{
		Type: NodeReady, Status: metav1.ConditionStatus_CONDITION_STATUS_UNKNOWN, Reason: "NodeStatusUnknown",
	}}}
	if _, err := s.backend(registry.Node).UpdateStatus(asNode(system, "n1"), resource.Key{Name: "n1"}, silent, nil); err != nil {
		t.Fatal(err)
	}
	waitContainer(t, s, system, "api", "stop unknown", func(c *computev1alpha1.Container) bool {
		return stoppedIs(c, metav1.ConditionStatus_CONDITION_STATUS_UNKNOWN, "NodeNotReporting")
	})
	nodeWithRoom(t, s, system, "n1", 4096)
	waitContainer(t, s, system, "api", "stopped again", func(c *computev1alpha1.Container) bool {
		return stoppedIs(c, metav1.ConditionStatus_CONDITION_STATUS_TRUE, computeapi.ReasonStopped)
	})

	// Apply replaces the whole spec: a manifest without the run state starts
	// the container again.
	applyCtr(t, s, user, running)
	m = waitMachine(t, s, system, "ct-api", "asked to run", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetRunState() == computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING
	})
	waitContainer(t, s, system, "api", "no longer stopped", func(c *computev1alpha1.Container) bool {
		return stoppedIs(c, metav1.ConditionStatus_CONDITION_STATUS_FALSE, "RunStateRunning")
	})

	// A container asked to run whose process exited 0 is not Stopped.
	g := m.GetMetadata().GetGeneration()
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, g, &metav1.Condition{
		Type: computeapi.MachineReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE,
		Reason: computeapi.ReasonExited, Message: "the container exited with code 0", ObservedGeneration: g,
	})
	waitContainer(t, s, system, "api", "exited, not stopped", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == computeapi.ReasonExited &&
			stoppedIs(c, metav1.ConditionStatus_CONDITION_STATUS_FALSE, "RunStateRunning")
	})
}

// TestAContainerCreatedStoppedGetsAStoppedMachine: a container may be
// created stopped, and an explicit RUNNING is the same as unset.
func TestAContainerCreatedStoppedGetsAStoppedMachine(t *testing.T) {
	s, system, user := controlled(t)
	applyCtr(t, s, user, withRunState(ctr("cold"), computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED))
	waitMachine(t, s, system, "ct-cold", "made stopped", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetRunState() == computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	})
	// Unplaced, nothing has stopped it yet.
	waitContainer(t, s, system, "cold", "stopping", func(c *computev1alpha1.Container) bool {
		return stoppedIs(c, metav1.ConditionStatus_CONDITION_STATUS_FALSE, "Stopping")
	})
	applyCtr(t, s, user, withRunState(ctr("warm"), computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_RUNNING))
	waitMachine(t, s, system, "ct-warm", "made running", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetRunState() == computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING
	})
}

func TestAnUnknownRunStateIsRefused(t *testing.T) {
	s, _, user := controlled(t)
	_, err := s.backend(registry.Container).Apply(user, withRunState(ctr("odd"), computev1alpha1.ContainerRunState(7)))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("run state 7: %v", err)
	}
}
