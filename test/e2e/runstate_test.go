package e2e

import (
	"fmt"
	"testing"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// waitStopped waits for the container's Stopped condition to have the
// status, at its current generation.
func (w *world) waitStopped(name string, status metav1.ConditionStatus) *computev1alpha1.Container {
	w.t.Helper()
	var c *computev1alpha1.Container
	testutil.Eventually(w.t, fmt.Sprintf("container %s Stopped=%v", name, status), func() (bool, string) {
		var ok bool
		c, ok = get[*computev1alpha1.Container](w, registry.Container, name)
		s := conditions.Find(c.GetStatus().GetConditions(), "Stopped")
		return ok && s.GetStatus() == status && s.GetObservedGeneration() == c.GetMetadata().GetGeneration(), fmt.Sprintf("%v", s)
	})
	return c
}

// TestAContainerStopsAndStartsOnItsDisk: a container applied with run state
// STOPPED has its VM taken down, its disk and interface kept, and says
// Stopped; applied RUNNING again it boots the same disk.
func TestAContainerStopsAndStartsOnItsDisk(t *testing.T) {
	w := newWorld(t)
	c := container("api", "nginx")
	c.Spec.RestartPolicy = computev1alpha1.RestartPolicy_RESTART_POLICY_ALWAYS
	w.apply(c)
	w.waitReason("api", "Running")
	w.waitStopped("api", metav1.ConditionStatus_CONDITION_STATUS_FALSE)
	uid := w.machine("api").GetMetadata().GetUid()

	c.Spec.RunState = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED
	w.apply(c)
	stopped := w.waitStopped("api", metav1.ConditionStatus_CONDITION_STATUS_TRUE)
	if r := ready(stopped); r.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || r.GetReason() != "Stopped" {
		t.Errorf("a stopped container's Ready: %v", r)
	}
	s := w.host.Snapshot()
	if _, running := s.VMs[uid]; running {
		t.Error("a stopped container's VM is still there, under restart policy always")
	}
	if s.Roots[uid].Digest != "sha256:nginx-1" || len(s.Interfaces[uid]) != 1 {
		t.Errorf("a stopped container lost its disk or interface: %+v", s)
	}
	if m := w.machine("api"); m.GetStatus().GetPhase() != computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED {
		t.Errorf("machine phase %v", m.GetStatus().GetPhase())
	}

	c.Spec.RunState = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_RUNNING
	w.apply(c)
	w.waitReason("api", "Running")
	w.waitStopped("api", metav1.ConditionStatus_CONDITION_STATUS_FALSE)
	if vm, ok := w.host.Snapshot().VMs[uid]; !ok || vm.Spec.Root.Digest != "sha256:nginx-1" {
		t.Errorf("started again on %+v (running: %t)", vm.Spec.Root, ok)
	}

	w.delete("api")
	w.waitGone("api")
	w.waitHostEmpty()
}
