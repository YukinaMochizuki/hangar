package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	billingv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/billing/v1alpha1"
	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// paysAsItGoes opens a PAY_AS_YOU_GO account for the world's tenant.
func (w *world) paysAsItGoes() {
	w.t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	if _, err := w.root.Apply(ctx, registry.PriceList, &billingv1alpha1.PriceList{
		Metadata: &metav1.ObjectMeta{Name: "std"},
		Spec: &billingv1alpha1.PriceListSpec{Currency: "TWD", Entries: []*billingv1alpha1.PriceEntry{{
			EffectiveTime: timestamppb.New(now.AddDate(0, -1, 0)), Prices: &billingv1alpha1.Prices{Cpu: "0.6"},
		}}},
	}); err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.root.Apply(ctx, registry.BillingAccount, &billingv1alpha1.BillingAccount{
		Metadata: &metav1.ObjectMeta{Name: apiservertest.Tenant},
		Spec: &billingv1alpha1.BillingAccountSpec{
			Type: billingv1alpha1.BillingType_BILLING_TYPE_PAY_AS_YOU_GO, Currency: "TWD", PriceList: "std",
			PeriodMonths: 1, AnchorTime: timestamppb.New(now.AddDate(0, 0, -1)),
		},
	}); err != nil {
		w.t.Fatal(err)
	}
}

// creditWorld is a world paying as it goes whose agent accounts credits
// each 200 ms and writes them each checkpoint, its agent started.
func creditWorld(t *testing.T, checkpoint time.Duration) *world {
	t.Helper()
	w := newIdleWorld(t)
	w.usagePoll = 200 * time.Millisecond
	w.creditCheckpoint = checkpoint
	w.startAgent()
	w.paysAsItGoes()
	return w
}

// spendUntilHeld waits for machine's launch credits written and its VM
// let go, which starts held until they are, then keeps its vCPUs busy and
// spends 15 CPU-minutes at once, past a 2-vCPU 20% machine's launch
// credits, until its node holds it to its baseline, 400 millicores. It
// returns the machine's uid.
func (w *world) spendUntilHeld(machine string) string {
	w.t.Helper()
	uid := w.machine2(machine).GetMetadata().GetUid()
	testutil.Eventually(w.t, "its launch credits written and its VM let go", func() (bool, string) {
		st := w.machine2(machine).GetStatus().GetCpuCredits()
		return st.GetBalance() > 11 && w.host.CPULimit(uid) == 0, fmt.Sprintf("credits %v, held to %d millicores", st, w.host.CPULimit(uid))
	})
	if err := w.host.SpinCPU(uid, 1); err != nil {
		w.t.Fatal(err)
	}
	if err := w.host.RunCPU(uid, 15*time.Minute); err != nil {
		w.t.Fatal(err)
	}
	testutil.Eventually(w.t, "the VM held to its baseline", func() (bool, string) {
		return w.host.CPULimit(uid) == 400, fmt.Sprintf("held to %d millicores", w.host.CPULimit(uid))
	})
	return uid
}

// TestABaselineRaisedWhileTheAgentIsDownIsLifted: a burstable container
// held to its baseline, raised to 100 while its node's agent is down, has
// its VM's limit lifted once the agent is back, though it has no ledger.
func TestABaselineRaisedWhileTheAgentIsDownIsLifted(t *testing.T) {
	t.Parallel()
	w := creditWorld(t, 100*time.Millisecond)
	c := cpuContainer("burst", "2", "1Gi")
	c.Spec.Resources.CpuBaseline = 20
	w.apply(c)
	w.waitReason("burst", "Running")
	uid := w.spendUntilHeld("ct-burst")

	w.stopAgent()
	if got := w.host.CPULimit(uid); got != 400 {
		t.Fatalf("with the agent stopped, the VM is held to %d millicores, want 400", got)
	}
	c.Spec.Resources.CpuBaseline = 100
	w.apply(c)
	testutil.Eventually(t, "the machine of all its vCPUs", func() (bool, string) {
		r := w.machine("burst").GetSpec().GetResources()
		return r.GetCpuBaseline() == 0, fmt.Sprint(r)
	})
	w.startAgent()
	w.waitSettled()
	testutil.Eventually(t, "the VM's limit lifted", func() (bool, string) {
		return w.host.CPULimit(uid) == 0, fmt.Sprintf("held to %d millicores", w.host.CPULimit(uid))
	})
}

// TestABurstableVirtualMachineStartsHeld: a VM of 2 vCPUs at 20% held to
// its baseline, stopped and started, starts held, as a container does.
func TestABurstableVirtualMachineStartsHeld(t *testing.T) {
	t.Parallel()
	w := creditWorld(t, 100*time.Millisecond)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	apply := func(state computev1alpha1.VirtualMachineRunState) {
		t.Helper()
		if _, err := w.root.Apply(context.Background(), registry.VirtualMachine, &computev1alpha1.VirtualMachine{
			Metadata: &metav1.ObjectMeta{Project: project, Name: "web"},
			Spec: &computev1alpha1.VirtualMachineSpec{
				RootDisk: &computev1alpha1.VirtualMachineRootDisk{Source: &computev1alpha1.VirtualMachineRootDisk_FromImage{
					FromImage: &computev1alpha1.VirtualMachineImageDisk{Image: &storagev1alpha1.ImageSource{Name: "noble"}, Size: "10Gi"},
				}},
				Resources: &computev1alpha1.VirtualMachineResources{Cpu: "2", Memory: "2Gi", CpuBaseline: 20},
				RunState:  state,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	apply(computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_RUNNING)
	w.waitVMReason("web", "Running")
	uid := w.spendUntilHeld(vmMachineOf("web"))
	apply(computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED)
	w.waitVMReason("web", computeapi.ReasonStopped)
	apply(computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_RUNNING)
	w.waitVMReason("web", "Running")
	if vm := w.host.Snapshot().VMs[uid]; !vm.Spec.Firmware || vm.Spec.CPULimit != 400 {
		t.Errorf("started again with no credits, the VM started held to %d millicores (firmware %t), want 400", vm.Spec.CPULimit, vm.Spec.Firmware)
	}
}

// TestAStoppingAgentWritesItsBalances: an agent that stops writes each
// balance it accounted past its last checkpoint, however long before its
// next.
func TestAStoppingAgentWritesItsBalances(t *testing.T) {
	t.Parallel()
	w := creditWorld(t, time.Hour)
	c := cpuContainer("burst", "2", "1Gi")
	c.Spec.Resources.CpuBaseline = 20
	w.apply(c)
	w.waitReason("burst", "Running")
	uid := w.machine("burst").GetMetadata().GetUid()
	testutil.Eventually(t, "its launch credits written", func() (bool, string) {
		st := w.machine("burst").GetStatus().GetCpuCredits()
		return st.GetBalance() > 11, fmt.Sprint(st)
	})
	if err := w.host.RunCPU(uid, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	api := w.logsAs(apiservertest.Root)
	testutil.Eventually(t, "5 CPU-minutes spent", func() (bool, string) {
		r, err := api.Usage(context.Background(), connect.NewRequest(&computev1alpha1.UsageRequest{Project: project, Kind: registry.Container.Kind, Name: "burst"}))
		if err != nil {
			return false, err.Error()
		}
		return r.Msg.GetCpuCredits() < 8, fmt.Sprint(r.Msg)
	})
	w.stopAgent()
	if st := w.machine("burst").GetStatus().GetCpuCredits(); st.GetBalance() >= 8 {
		t.Errorf("the stopped agent left the balance at %v, want what it had spent down to, under 8", st)
	}
}

// TestABurstableMachineSpendsDownToItsBaselineAndRecovers: a container of
// 2 vCPUs at 20% starts with its launch credits, 12 CPU-minutes, written
// to its machine's status, and runs free; busy past them, its node holds
// it to 0.4 CPUs, and its usage says so; stopped and started, its VM
// starts held; idle, it earns and is let go. An agent started again reads
// its balance back rather than giving it launch credits again, restarting
// nothing.
func TestABurstableMachineSpendsDownToItsBaselineAndRecovers(t *testing.T) {
	t.Parallel()
	w := newIdleWorld(t)
	// Reads more than two polls apart are a gap, charged nothing: a poll
	// of a few writes' time, as in the other usage tests, would make most
	// of them gaps.
	w.usagePoll = 200 * time.Millisecond
	w.creditCheckpoint = 100 * time.Millisecond
	w.startAgent()
	w.paysAsItGoes()
	c := cpuContainer("burst", "2", "1Gi")
	c.Spec.Resources.CpuBaseline = 20
	w.apply(c)
	w.waitReason("burst", "Running")
	uid := w.machine("burst").GetMetadata().GetUid()
	api := w.logsAs(apiservertest.Root)
	usage := func() *computev1alpha1.UsageResponse {
		t.Helper()
		r, err := api.Usage(context.Background(), connect.NewRequest(&computev1alpha1.UsageRequest{Project: project, Kind: registry.Container.Kind, Name: "burst"}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg
	}
	testutil.Eventually(t, "its launch credits written and its VM let go", func() (bool, string) {
		st := w.machine("burst").GetStatus().GetCpuCredits()
		return st.GetAsOf() != nil && st.GetBalance() > 11 && w.host.CPULimit(uid) == 0,
			fmt.Sprintf("credits %v, held to %d millicores", st, w.host.CPULimit(uid))
	})
	if u := usage(); u.GetCpuBaseline() != 20 || u.GetCpuCreditCap() != 576 || u.GetCpuHeld() || u.GetCpuCredits() <= 11 {
		t.Errorf("free with its launch credits, its usage says %v", u)
	}

	if err := w.host.SpinCPU(uid, 1); err != nil {
		t.Fatal(err)
	}
	if err := w.host.RunCPU(uid, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the VM held to its baseline", func() (bool, string) {
		return w.host.CPULimit(uid) == 400, fmt.Sprintf("held to %d millicores", w.host.CPULimit(uid))
	})
	if u := usage(); !u.GetCpuHeld() || u.GetCpuCredits() > 1 {
		t.Errorf("held, its usage says %v", u)
	}
	// Busy at its baseline it earns what it spends: it stays held.
	testutil.Eventually(t, "the held VM's credits written", func() (bool, string) {
		st := w.machine("burst").GetStatus().GetCpuCredits()
		return st.GetBalance() < 1, fmt.Sprint(st)
	})
	if got := w.host.CPULimit(uid); got != 400 {
		t.Errorf("busy at its baseline, the VM is held to %d millicores, want 400", got)
	}
	// Started again, its VM starts held: the limit its unit had goes with
	// it.
	c.Spec.RunState = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED
	w.apply(c)
	w.waitStopped("burst", metav1.ConditionStatus_CONDITION_STATUS_TRUE)
	c.Spec.RunState = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_RUNNING
	w.apply(c)
	w.waitReason("burst", "Running")
	vm := w.host.Snapshot().VMs[uid]
	if vm.Spec.CPULimit != 400 {
		t.Errorf("started again with no credits, the VM started held to %d millicores, want 400", vm.Spec.CPULimit)
	}
	started := vm.Started

	testutil.Eventually(t, "the idle VM let go", func() (bool, string) {
		return w.host.CPULimit(uid) == 0, fmt.Sprintf("held to %d millicores", w.host.CPULimit(uid))
	})
	if u := usage(); u.GetCpuHeld() || u.GetCpuCredits() <= 0 {
		t.Errorf("let go, its usage says %v", u)
	}

	w.stopAgent()
	w.startAgent()
	testutil.Eventually(t, "the restarted agent's balance", func() (bool, string) {
		r, err := api.Usage(context.Background(), connect.NewRequest(&computev1alpha1.UsageRequest{Project: project, Kind: registry.Container.Kind, Name: "burst"}))
		if err != nil {
			return false, err.Error()
		}
		return r.Msg.CpuCredits != nil, fmt.Sprint(r.Msg)
	})
	if u := usage(); u.GetCpuCredits() > 1 {
		t.Errorf("after the agent started again, the balance is %v, want what it had, short of its launch credits", u.GetCpuCredits())
	}
	if vm := w.host.Snapshot().VMs[uid]; !vm.Started.Equal(started) {
		t.Errorf("the VM started again at %v, first at %v", vm.Started, started)
	}
}
