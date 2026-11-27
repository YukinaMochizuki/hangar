package apiserver

import (
	"context"
	"testing"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
)

// burstable is a container of 1Gi and cpu vCPUs at baseline percent; 0 for
// none.
func burstable(name, cpu string, baseline int32) *computev1alpha1.Container {
	c := ctr(name)
	c.Spec.Resources = &computev1alpha1.Resources{Cpu: cpu, Memory: "1Gi", CpuBaseline: baseline}
	return c
}

// TestABaselineBelow100IsAPayAsYouGoTenants: a baseline is 20, 40 or 100;
// below 100 only for a tenant paying as it goes, whoever writes it; a
// write that leaves a baseline as it was is not judged again once the
// tenant has left PAY_AS_YOU_GO, but one that changes it is.
func TestABaselineBelow100IsAPayAsYouGoTenants(t *testing.T) {
	t.Parallel()
	s, system, root, alice := billedWith(t, false)
	cb := s.backend(registry.Container)
	apply := func(ctx context.Context, c *computev1alpha1.Container) error {
		_, err := cb.Apply(ctx, c)
		return err
	}
	for _, b := range []int32{10, 50, 101, -20} {
		wantInvalid(t, apply(alice, burstable("a", "2", b)), "is not 20, 40 or 100", "a baseline off the tiers")
	}
	for _, b := range []int32{0, 100} {
		if err := apply(alice, burstable("a", "2", b)); err != nil {
			t.Errorf("a baseline of %d with no billing account: %v", b, err)
		}
	}
	const notPayg = `only a tenant whose billing account is PAY_AS_YOU_GO may have; tenant "acme"'s is not`
	wantPrecondition(t, apply(alice, burstable("b", "2", 20)), notPayg, "a baseline of 20 with no billing account")
	wantPrecondition(t, apply(root, burstable("b", "2", 20)), notPayg, "root's baseline of 20 with no billing account")
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	vm := vmFromImage("vm", "noble")
	vm.Spec.Resources = &computev1alpha1.VirtualMachineResources{Cpu: "2", Memory: "2Gi", CpuBaseline: 40}
	_, err := s.backend(registry.VirtualMachine).Apply(alice, vm)
	wantPrecondition(t, err, notPayg, "a VM's baseline of 40 with no billing account")

	now := minuteNow(s)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD"))
	fixedOn(t, s, root, "acme", "pro", "300", now)
	wantPrecondition(t, apply(alice, burstable("b", "2", 20)), notPayg, "a baseline of 20 on a FIXED account")

	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 1, now))
	if err := apply(alice, burstable("b", "2", 20)); err != nil {
		t.Errorf("a baseline of 20 paying as it goes: %v", err)
	}
	if _, err := s.backend(registry.VirtualMachine).Apply(alice, vm); err != nil {
		t.Errorf("a VM's baseline of 40 paying as it goes: %v", err)
	}
	wantInvalid(t, apply(alice, burstable("c", "2", 10)), "is not 20, 40 or 100", "a baseline of 10 paying as it goes")

	fixedAccount := fixed("acme", now)
	fixedAccount.Spec.PriceList = "std"
	mustApply(t, s, root, registry.BillingAccount, fixedAccount)
	// What a write leaves as it was is not judged again.
	if err := apply(alice, burstable("b", "1", 20)); err != nil {
		t.Errorf("shrinking a burstable container, its baseline as it was, on a FIXED account: %v", err)
	}
	vm.Spec.Resources.Memory = "4Gi"
	if _, err := s.backend(registry.VirtualMachine).Apply(alice, vm); err != nil {
		t.Errorf("growing a burstable VM, its baseline as it was, on a FIXED account: %v", err)
	}
	wantPrecondition(t, apply(alice, burstable("b", "1", 40)), notPayg, "raising a baseline on a FIXED account")
	if err := apply(alice, burstable("b", "1", 100)); err != nil {
		t.Errorf("giving a container all its vCPUs again: %v", err)
	}
	wantPrecondition(t, apply(alice, burstable("b", "1", 20)), notPayg, "a baseline of 20 again on a FIXED account")
}

// TestABurstableMachineIsBilledAtItsTier: a burstable container's machine
// carries its baseline, one of 100 none; the quota counts all its vCPUs;
// running, the meter records its vCPUs as its tier's item in place of cpu.
func TestABurstableMachineIsBilledAtItsTier(t *testing.T) {
	t.Parallel()
	s, system, root, alice := billed(t)
	now := minuteNow(s)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD"))
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 1, now))
	for name, b := range map[string]int32{"small": 20, "mid": 40, "full": 100} {
		applyCtr(t, s, alice, burstable(name, "2", b))
		want := b % 100
		m := waitMachine(t, s, system, "ct-"+name, "placed with its baseline", func(m *computev1alpha1.Machine) bool {
			return m.GetSpec().GetNode() == "n1" && m.GetSpec().GetResources().GetCpuBaseline() == want
		})
		reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration())
	}
	if err := s.meterOnce(system, now); err != nil {
		t.Fatal(err)
	}
	held, err := s.meterHeld(system, "acme", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if held["cpu"] != 2000 || held["cpu_burstable_20"] != 2000 || held["cpu_burstable_40"] != 2000 {
		t.Errorf("a minute of three running 2-vCPU containers at 100, 20 and 40%% = %v, want 2000 millicores of each item", held)
	}
	waitUsage(t, s, alice, "all 6 vCPUs counted", func(ts *tenancyv1alpha1.TenantStatus, _ *tenancyv1alpha1.ProjectStatus) bool {
		return ts.GetUsage().GetCpu() == "6"
	})
}
