package apiserver

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// launched applies a burstable container of 1 vCPU at 20% named name, and
// returns the launch decision its machine is written with.
func launched(t *testing.T, s *Server, system, alice context.Context, name string) bool {
	t.Helper()
	applyCtr(t, s, alice, burstable(name, "1", 20))
	return decision(t, s, system, name)
}

// decision waits for container name's machine to carry a launch decision,
// and returns it.
func decision(t *testing.T, s *Server, system context.Context, name string) bool {
	t.Helper()
	m := waitMachine(t, s, system, machineName(name), "decided", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetCpuLaunch() != nil
	})
	return m.GetSpec().GetCpuLaunch().GetGranted()
}

// grantsOf is how many launch grants tenant acme's status records.
func grantsOf(t *testing.T, s *Server) int {
	t.Helper()
	return len(getTenant(t, s, "acme").GetStatus().GetCpuLaunchGrants())
}

// TestATenantGetsTenLaunchGrantsADay: a tenant's first 10 burstable
// machines in 24 hours get launch credits and the 11th none; deleting
// machines gives none back; once 24 hours have passed since, grants free
// up, though a machine decided keeps its decision; a tier change from 100
// takes a grant as a new machine does.
func TestATenantGetsTenLaunchGrantsADay(t *testing.T) {
	t.Parallel()
	s, system, root, alice := billed(t)
	now := minuteNow(s)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD"))
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 1, now))
	for i := range 10 {
		if !launched(t, s, system, alice, fmt.Sprintf("b%d", i)) {
			t.Fatalf("burstable machine %d of 10 got no launch credits", i+1)
		}
	}
	if launched(t, s, system, alice, "b10") {
		t.Error("the 11th burstable machine in 24 hours got launch credits")
	}
	if n := grantsOf(t, s); n != 10 {
		t.Errorf("the tenant records %d grants, want 10", n)
	}

	for i := range 5 {
		name := fmt.Sprintf("b%d", i)
		if _, err := s.backend(registry.Container).Delete(alice, resource.Key{Project: "web", Name: name}, nil); err != nil {
			t.Fatal(err)
		}
		m := waitMachine(t, s, system, machineName(name), "marked for deletion", func(m *computev1alpha1.Machine) bool {
			return m.GetMetadata().GetDeleteTime() != nil
		})
		reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration(),
			&metav1.Condition{Type: MachineReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned"})
		waitGone(t, s, system, registry.Container, name)
	}
	if launched(t, s, system, alice, "c0") {
		t.Error("with 5 of 10 burstable machines deleted, a new one got launch credits")
	}

	fakeClock(s).Advance(launchGrantWindow + time.Minute)
	if !launched(t, s, system, alice, "c1") {
		t.Error("24 hours after the grants, a new burstable machine got no launch credits")
	}
	// Applied again, changed, the machines denied keep their decision.
	applyCtr(t, s, alice, burstable("b10", "2", 20))
	waitMachine(t, s, system, machineName("b10"), "grown", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetResources().GetCpu() == "2"
	})
	if decision(t, s, system, "b10") {
		t.Error("a machine denied launch credits got them once grants freed up")
	}

	applyCtr(t, s, alice, burstable("d", "1", 100))
	m := waitMachine(t, s, system, machineName("d"), "made", func(*computev1alpha1.Machine) bool { return true })
	if m.GetSpec().GetCpuLaunch() != nil {
		t.Errorf("a machine at 100 was decided: %v", m.GetSpec().GetCpuLaunch().GetGranted())
	}
	if !launched(t, s, system, alice, "d") {
		t.Error("a machine made burstable from 100 got no launch credits")
	}
	if n := grantsOf(t, s); n != 2 {
		t.Errorf("after c1 and d, the tenant records %d grants in 24 hours, want 2", n)
	}
}

// checkpoint writes machine name's credits as its node does, as of at,
// reading it again while its controllers write it under the write.
func checkpoint(t *testing.T, s *Server, system context.Context, name string, at time.Time) {
	t.Helper()
	testutil.Eventually(t, "machine "+name+"'s credits written", func() (bool, string) {
		m := waitMachine(t, s, system, machineName(name), "read", func(*computev1alpha1.Machine) bool { return true })
		status := proto.Clone(m.GetStatus()).(*computev1alpha1.MachineStatus)
		if status == nil {
			status = &computev1alpha1.MachineStatus{}
		}
		status.CpuCredits = &computev1alpha1.MachineCPUCredits{Balance: 3, AsOf: timestamppb.New(at)}
		rev := m.GetMetadata().GetResourceVersion()
		_, err := s.backend(registry.Machine).UpdateStatus(system, resource.Key{Project: "web", Name: machineName(name)}, status, &rev)
		if connect.CodeOf(err) == connect.CodeAborted {
			return false, err.Error()
		}
		if err != nil {
			t.Fatal(err)
		}
		return true, ""
	})
}

// burstableIn is burstable's container in run state.
func burstableIn(name string, state computev1alpha1.ContainerRunState) *computev1alpha1.Container {
	c := burstable(name, "1", 20)
	c.Spec.RunState = state
	return c
}

// TestAnExpiredBalanceTakesANewGrant: a burstable machine started again
// with its balance last accounted more than 7 days before is decided
// again, as a new grant against its tenant's 10 a day: granted while some
// are left, denied once they are spent; reconciled again, it keeps that
// decision. Stopped, or with a balance accounted since its decision, it is
// not decided again.
func TestAnExpiredBalanceTakesANewGrant(t *testing.T) {
	t.Parallel()
	s, system, root, alice := billed(t)
	now := minuteNow(s)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD"))
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 1, now))
	stopped := computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED
	running := computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_RUNNING
	decided := map[string]time.Time{}
	for _, name := range []string{"a", "e", "z"} {
		if !launched(t, s, system, alice, name) {
			t.Fatalf("%s got no launch credits", name)
		}
		decided[name] = waitMachine(t, s, system, machineName(name), "decided", func(*computev1alpha1.Machine) bool { return true }).
			GetSpec().GetCpuLaunch().GetDecided().AsTime()
		fakeClock(s).Advance(time.Minute)
		checkpoint(t, s, system, name, s.clock.Now())
		applyCtr(t, s, alice, burstableIn(name, stopped))
	}
	fakeClock(s).Advance(balanceKeep + time.Hour)
	since := func(name string) *computev1alpha1.CPULaunchDecision {
		t.Helper()
		return waitMachine(t, s, system, machineName(name), "decided again", func(m *computev1alpha1.Machine) bool {
			return m.GetSpec().GetCpuLaunch().GetDecided().AsTime().After(decided[name])
		}).GetSpec().GetCpuLaunch()
	}

	applyCtr(t, s, alice, burstableIn("e", running))
	e := since("e")
	if !e.GetGranted() {
		t.Error("started after 8 days with grants left, e got no launch credits")
	}
	if n := grantsOf(t, s); n != 1 {
		t.Errorf("e's new grant: the tenant records %d grants in 24 hours, want 1", n)
	}
	// Reconciled again later, its balance still the old one: the decision
	// stands.
	fakeClock(s).Advance(time.Minute)
	again := burstableIn("e", running)
	again.Metadata.Labels = map[string]string{"touched": "yes"}
	applyCtr(t, s, alice, again)
	waitSettled(t, s)
	if got := waitMachine(t, s, system, machineName("e"), "read", func(*computev1alpha1.Machine) bool { return true }).
		GetSpec().GetCpuLaunch(); !proto.Equal(got, e) {
		t.Errorf("reconciled again, e's decision moved from %v to %v", e, got)
	}
	// Reconciled stopped, z is not decided again.
	c := burstableIn("z", stopped)
	c.Metadata.Labels = map[string]string{"touched": "yes"}
	applyCtr(t, s, alice, c)
	waitMachine(t, s, system, machineName("z"), "relabelled", func(m *computev1alpha1.Machine) bool {
		return m.GetMetadata().GetGeneration() > 0
	})
	waitSettled(t, s)
	if got := waitMachine(t, s, system, machineName("z"), "read", func(*computev1alpha1.Machine) bool { return true }).
		GetSpec().GetCpuLaunch().GetDecided().AsTime(); !got.Equal(decided["z"]) {
		t.Errorf("stopped, z was decided again at %v", got)
	}

	for i := range 9 {
		if !launched(t, s, system, alice, fmt.Sprintf("b%d", i)) {
			t.Fatalf("burstable machine %d of 9 more got no launch credits", i+1)
		}
	}
	applyCtr(t, s, alice, burstableIn("a", running))
	if a := since("a"); a.GetGranted() {
		t.Error("started after 8 days with its tenant's 10 grants spent, a got launch credits")
	}
	if n := grantsOf(t, s); n != 10 {
		t.Errorf("the tenant records %d grants in 24 hours, want 10", n)
	}
}

// TestALaunchIsDecidedOnce: deciding again for the same owner, as after
// its machine's write failed, finds the grant recorded and takes no
// other; a machine reconciled again keeps its grant and takes none.
func TestALaunchIsDecidedOnce(t *testing.T) {
	t.Parallel()
	s, system, root, alice := billed(t)
	now := minuteNow(s)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD"))
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 1, now))
	want := &computev1alpha1.MachineSpec{Resources: &computev1alpha1.Resources{CpuBaseline: 20}}
	for range 2 {
		want.CpuLaunch = nil
		if err := s.decideLaunch(system, "web", "owner-1", nil, want); err != nil {
			t.Fatal(err)
		}
		if !want.GetCpuLaunch().GetGranted() {
			t.Error("the owner's machine got no launch credits")
		}
	}
	if n := grantsOf(t, s); n != 1 {
		t.Errorf("decided twice for one owner, the tenant records %d grants, want 1", n)
	}

	if !launched(t, s, system, alice, "api") {
		t.Fatal("a burstable machine got no launch credits")
	}
	applyCtr(t, s, alice, burstable("api", "2", 40))
	waitMachine(t, s, system, machineName("api"), "changed", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetResources().GetCpuBaseline() == 40
	})
	if !decision(t, s, system, "api") {
		t.Error("reconciled again, the machine lost its launch credits")
	}
	if n := grantsOf(t, s); n != 2 {
		t.Errorf("a machine reconciled again, the tenant records %d grants, want 2", n)
	}
}
