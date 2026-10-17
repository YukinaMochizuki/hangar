package e2e

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// TestAStalledStopHoldsUpNoOtherMachine: while one machine's stop waits on
// a guest that ignores SIGTERM, as the real driver waits out the grace
// period, another machine is built and reported ready.
func TestAStalledStopHoldsUpNoOtherMachine(t *testing.T) {
	w := newWorld(t)
	w.applyMachine(bare("slow", "10.0.0.50/24"))
	w.waitMachine("slow", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning)
	uid := w.machine2("slow").GetMetadata().GetUid()

	letGo := w.host.DelayStop(uid, time.Hour)
	t.Cleanup(letGo)
	w.editMachine("slow", func(m *computev1alpha1.Machine) {
		m.Spec.RunState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	})
	testutil.Eventually(w.t, "slow's stop under way", func() (bool, string) {
		n := w.host.GuestStops(uid)
		return n > 0, fmt.Sprintf("%d guest stops", n)
	})

	w.applyMachine(bare("quick", "10.0.0.51/24"))
	w.waitMachine("quick", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning)
	if vm, ok := w.host.Snapshot().VMs[uid]; !ok || vm.State != driver.VMRunning {
		t.Fatalf("slow's stop ended before quick was ready: %+v", vm)
	}
	if n := w.host.GuestStops(uid); n != 1 {
		t.Errorf("slow's guest asked to stop %d times while its stop ran", n)
	}

	letGo()
	w.waitMachine("slow", computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, computeapi.ReasonStopped)
	if _, ok := w.host.Snapshot().VMs[uid]; ok {
		t.Error("slow's VM is still there once stopped")
	}
}

// TestAFailedStopIsReportedAndTriedAgain: a stop that fails in the
// background says why on the machine, which is never reported stopped while
// its VM runs, and the stop is tried again, no sooner than its backoff's
// first second, until it succeeds.
func TestAFailedStopIsReportedAndTriedAgain(t *testing.T) {
	w := newWorld(t)
	w.applyMachine(bare("solo", "10.0.0.50/24"))
	w.waitMachine("solo", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning)
	uid := w.machine2("solo").GetMetadata().GetUid()

	w.host.FailRemoval(errors.New("systemd unreachable"))
	asked := time.Now()
	w.editMachine("solo", func(m *computev1alpha1.Machine) {
		m.Spec.RunState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	})
	m := w.waitMachine("solo", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonBootFailed)
	if msg := conditions.Find(m.GetStatus().GetConditions(), computeapi.MachineReady).GetMessage(); !strings.Contains(msg, "systemd unreachable") {
		t.Errorf("the failed stop says %q", msg)
	}
	if vm, ok := w.host.Snapshot().VMs[uid]; !ok || vm.State != driver.VMRunning {
		t.Fatalf("the VM is not running after its stop failed: %+v", vm)
	}
	testutil.Eventually(w.t, "the stop tried again", func() (bool, string) {
		n := w.host.HypervisorStops(uid)
		return n >= 2, fmt.Sprintf("%d stops", n)
	})
	if d := time.Since(asked); d < time.Second {
		t.Errorf("the failed stop was tried again after %v, before its backoff", d)
	}

	w.host.FailRemoval(nil)
	w.waitMachine("solo", computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, computeapi.ReasonStopped)
	if _, ok := w.host.Snapshot().VMs[uid]; ok {
		t.Error("the VM is still there once stopped")
	}
}

// settled is how long no machine is reconciled before the node is taken to
// be done with what it had in hand. It outlasts the 2 s by which the agent's
// controller retries a reconcile that waits, a retry that still comes once
// the machine is running.
const settled = 2500 * time.Millisecond

// quiet waits until nothing has asked any guest agent on the host whether
// it answers, as every reconcile of a running machine does, for settled, and
// returns how often the machine's was asked. Every machine is waited on, as
// one's reconcile queued behind another's would come once the other's ends.
func (w *world) quiet(uid string) int {
	w.t.Helper()
	var last map[string]int
	since := time.Now()
	testutil.Eventually(w.t, "machine "+uid+" left alone", func() (bool, string) {
		n := map[string]int{uid: w.host.GuestReadies(uid)}
		for other := range w.host.Snapshot().VMs {
			n[other] = w.host.GuestReadies(other)
		}
		if !maps.Equal(n, last) {
			last, since = n, time.Now()
		}
		return time.Since(since) > settled, fmt.Sprintf("reconciles by machine: %v", n)
	})
	return last[uid]
}

// TestAFailingStopBacksOff: a stop that keeps failing is tried again no
// faster than its backoff (a second, then twice as long each time), and
// costs the other machines nothing: none is reconciled for it.
func TestAFailingStopBacksOff(t *testing.T) {
	w := newWorld(t)
	w.applyMachine(bare("a", "10.0.0.50/24"))
	w.applyMachine(bare("b", "10.0.0.51/24"))
	w.waitMachine("a", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning)
	w.waitMachine("b", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning)
	a, b := w.machine2("a").GetMetadata().GetUid(), w.machine2("b").GetMetadata().GetUid()
	readies := w.quiet(a)

	w.host.FailRemoval(errors.New("systemd unreachable"))
	w.editMachine("b", func(m *computev1alpha1.Machine) {
		m.Spec.RunState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	})
	// Begun at 0 s, then no sooner than 1, 3 and 7 s: four at most.
	time.Sleep(12 * time.Second) //nolint:forbidigo // real time
	if n := w.host.HypervisorStops(b); n < 2 || n > 4 {
		t.Errorf("a failing stop was tried %d times in 12 s, want 2 to 4", n)
	}
	if n := w.host.GuestReadies(a); n != readies {
		t.Errorf("a healthy machine was reconciled %d times for another's failing stop", n-readies)
	}
}

// TestNoVMNoStop: a machine with no VM is not stopped when its disk is
// rebuilt or it is deleted, and its deletion checks no other machine again.
func TestNoVMNoStop(t *testing.T) {
	w := newWorld(t)
	w.host.AddImage("nginx:2", "sha256:nginx-2", nginx)
	w.applyMachine(bare("a", "10.0.0.50/24"))
	c := bare("c", "10.0.0.51/24")
	c.Metadata.Finalizers = []string{apiserver.NodeCleanupFinalizer}
	c.Spec.RunState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	w.applyMachine(c)
	w.waitMachine("a", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning)
	w.waitMachine("c", computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, computeapi.ReasonStopped)
	a, uid := w.machine2("a").GetMetadata().GetUid(), w.machine2("c").GetMetadata().GetUid()

	// Built and stopped, it has a root disk and no VM.
	w.editMachine("c", func(m *computev1alpha1.Machine) {
		m.Spec.RunState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING
	})
	w.waitMachine("c", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning)
	w.editMachine("c", func(m *computev1alpha1.Machine) {
		m.Spec.RunState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	})
	w.waitMachine("c", computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, computeapi.ReasonStopped)
	stops := w.host.HypervisorStops(uid)

	// Another image rebuilds the disk of a machine with no VM. The image
	// made ready checks every machine again of its own.
	w.editMachine("c", func(m *computev1alpha1.Machine) {
		m.Spec.GetContainer().Image = "nginx:2"
		m.Spec.RunState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING
	})
	w.waitMachine("c", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning)
	if root := w.host.Snapshot().Roots[uid]; root.Digest != "sha256:nginx-2" {
		t.Fatalf("root disk %+v", root)
	}
	if n := w.host.HypervisorStops(uid); n != stops {
		t.Errorf("rebuilding the disk of a machine with no VM stopped it %d times", n-stops)
	}

	// Deleted with no VM.
	w.editMachine("c", func(m *computev1alpha1.Machine) {
		m.Spec.RunState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	})
	w.waitMachine("c", computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, computeapi.ReasonStopped)
	readies, stops := w.quiet(a), w.host.HypervisorStops(uid)
	if _, err := w.system.Delete(context.Background(), registry.Machine, client.Key{Project: project, Name: "c"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(w.t, "machine c released and gone", func() (bool, string) {
		_, ok := get[*computev1alpha1.Machine](w, registry.Machine, "c")
		return !ok, "still there"
	})
	if n := w.host.HypervisorStops(uid); n != stops {
		t.Errorf("deleting a machine with no VM stopped it %d times", n-stops)
	}
	if n := w.quiet(a); n != readies {
		t.Errorf("deleting another machine reconciled a healthy machine %d times", n-readies)
	}
}
