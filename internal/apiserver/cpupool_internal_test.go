package apiserver

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
)

// cpuPooledLimits are limits of cpu whose machines' vCPUs may come to
// percent of it.
func cpuPooledLimits(cpu string, percent int32) *tenancyv1alpha1.Limits {
	return &tenancyv1alpha1.Limits{Cpu: proto.String(cpu), CpuLimitPercent: proto.Int32(percent)}
}

// vcpus is a container of cpu vCPUs and the least memory the shape rules
// allow them.
func vcpus(name, cpu, memory string) *computev1alpha1.Container {
	return quotaCtr(name, cpu, memory, "")
}

func TestCPULimitPercentIsAPlans(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	plan := func(l *tenancyv1alpha1.Limits) error {
		_, err := s.backend(registry.Plan).Apply(system, &tenancyv1alpha1.Plan{
			Metadata: &metav1.ObjectMeta{Name: "pooled"}, Spec: &tenancyv1alpha1.PlanSpec{Limits: l},
		})
		return err
	}
	wantInvalid(t, plan(cpuPooledLimits("4", 99)), "cpu_limit_percent: 99 is not from 100 to 400", "a percent below 100")
	wantInvalid(t, plan(cpuPooledLimits("4", 401)), "cpu_limit_percent: 401 is not from 100 to 400", "a percent above 400")
	wantInvalid(t, plan(&tenancyv1alpha1.Limits{CpuLimitPercent: proto.Int32(200)}), "spec.limits.cpu, which is not set", "a percent of no cpu")
	for _, p := range []int32{100, 400} {
		if err := plan(cpuPooledLimits("4", p)); err != nil {
			t.Errorf("a percent of %d: %v", p, err)
		}
	}
	// Memory's percent and CPU's are each their own.
	both := cpuPooledLimits("4", 300)
	both.Memory, both.MemoryLimitPercent = proto.String("8Gi"), proto.Int32(150)
	if err := plan(both); err != nil {
		t.Errorf("a memory percent beside a cpu percent: %v", err)
	}
	_, err := s.backend(registry.Project).Apply(user, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "web"},
		Spec:     &tenancyv1alpha1.ProjectSpec{Tenant: "acme", Limits: cpuPooledLimits("2", 200)},
	})
	wantInvalid(t, err, "cpu_limit_percent is not a project's to set", "a project setting a percent")
}

// TestQuotaHoldsVCPUsToThePercent: in a CPU pool, the vCPUs of machines
// asked to run may come to the cpu limit times the plan's percent, for the
// tenant and for a project's own cpu limit alike; usage.cpu is their sum,
// which may read past the plan's cpu. At 100, or paying as it goes, the cpu
// limit holds as before.
func TestQuotaHoldsVCPUsToThePercent(t *testing.T) {
	t.Parallel()
	s, _, root, alice := billed(t)
	cb := s.backend(registry.Container)

	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Cpu: proto.String("4")})
	if _, err := cb.Apply(alice, vcpus("a", "4", "2Gi")); err != nil {
		t.Fatal(err)
	}
	_, err := cb.Apply(alice, vcpus("b", "1", "512Mi"))
	wantExhausted(t, err, `tenant "acme"'s cpu limit is 4 (plan "default") and it uses 4; this would bring it to 5`, "past the cpu limit at 100 percent")

	setDefaultPlan(t, s, cpuPooledLimits("4", 200))
	if _, err := cb.Apply(alice, vcpus("b", "4", "2Gi")); err != nil {
		t.Fatalf("vCPUs up to the limit times the percent: %v", err)
	}
	_, err = cb.Apply(alice, vcpus("c", "1", "512Mi"))
	wantExhausted(t, err, `the vCPUs of tenant "acme"'s machines asked to run may come to 8, 200% of its cpu limit of 4 (plan "default", cpu_limit_percent), and come to 8; this would bring them to 9`,
		"vCPUs past the limit times the percent")
	if _, err := cb.Apply(alice, stopped(vcpus("c", "1", "512Mi"))); err != nil {
		t.Fatalf("a stopped container counts no vCPUs: %v", err)
	}
	tl, err := s.tally(context.Background(), "acme", nil)
	if err != nil {
		t.Fatal(err)
	}
	if u := tl.tenant.usage(); u.GetCpu() != "8" {
		t.Errorf("acme uses cpu %q, want 8, past its plan's 4", u.GetCpu())
	}

	// A project's cpu limit holds its vCPUs with its tenant's percent.
	if _, err := s.backend(registry.Project).Apply(alice, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "web"},
		Spec:     &tenancyv1alpha1.ProjectSpec{Tenant: "acme", Limits: &tenancyv1alpha1.Limits{Cpu: proto.String("4")}},
	}); err != nil {
		t.Fatal(err)
	}
	setDefaultPlan(t, s, cpuPooledLimits("8", 300))
	_, err = cb.Apply(alice, vcpus("d", "5", "3Gi"))
	wantExhausted(t, err, `the vCPUs of project "web"'s machines asked to run may come to 12, 300% (tenant "acme"'s cpu_limit_percent) of its cpu limit of 4 (its spec.limits), and come to 8; this would bring them to 13`,
		"a project's vCPUs past its limit times the percent")
	if _, err := cb.Apply(alice, vcpus("d", "4", "2Gi")); err != nil {
		t.Fatalf("a project's vCPUs up to its limit times the percent: %v", err)
	}

	// Paying as it goes, a tenant has no CPU pool: the cpu limit holds, and
	// what it already runs past it may stay, as over any lowered limit.
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD"))
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 1, minuteNow(s)))
	_, err = cb.Apply(alice, vcpus("e", "1", "512Mi"))
	wantExhausted(t, err, `tenant "acme"'s cpu limit is 8 (plan "default") and it uses 12; this would bring it to 13`, "past the cpu limit paying as it goes")
	changed := vcpus("a", "4", "2Gi")
	changed.Spec.Image = "redis"
	if _, err := cb.Apply(alice, changed); err != nil {
		t.Errorf("changing a container's image over the cpu limit: %v", err)
	}
}

// TestCPUPooledMachinesCarryTheirPool: the controllers give the machines of
// a CPU-pooled tenant its CPU pool, apart from its memory pool, follow its
// plan and its billing account changing, and never give one to a
// FileShare's server.
func TestCPUPooledMachinesCarryTheirPool(t *testing.T) {
	t.Parallel()
	s, system, root, alice := billed(t)
	setDefaultPlan(t, s, cpuPooledLimits("4", 200))
	applyCtr(t, s, alice, ctr("api"))
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.VirtualMachine).Apply(alice, vmFromImage("vm", "noble")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.FileShare).Apply(alice, fileShare("data", "2Gi")); err != nil {
		t.Fatal(err)
	}
	machines := []string{"ct-api", vmMachineName("vm")}
	waitPools := func(what string, cpu *computev1alpha1.CPUPool, memory *computev1alpha1.MemoryPool) {
		t.Helper()
		for _, name := range machines {
			waitMachine(t, s, system, name, what, func(m *computev1alpha1.Machine) bool {
				return proto.Equal(m.GetSpec().GetCpuPool(), cpu) && proto.Equal(m.GetSpec().GetMemoryPool(), memory)
			})
		}
		waitSettled(t, s)
		server := getMachine(t, s, system, resource.Key{Project: "web", Name: shareServerName("data")})
		if p := server.GetSpec().GetCpuPool(); p != nil {
			t.Errorf("%s: the share's server is in CPU pool %v", what, p)
		}
	}
	waitPools("in acme's CPU pool of 4", &computev1alpha1.CPUPool{Name: "acme", Cpu: "4"}, nil)
	before := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"})

	setDefaultPlan(t, s, cpuPooledLimits("2500m", 200))
	waitPools("in a CPU pool shrunk with the plan", &computev1alpha1.CPUPool{Name: "acme", Cpu: "2500m"}, nil)
	after := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"})
	was, now := proto.Clone(before.GetSpec()).(*computev1alpha1.MachineSpec), proto.Clone(after.GetSpec()).(*computev1alpha1.MachineSpec)
	was.CpuPool, now.CpuPool = nil, nil
	if !proto.Equal(was, now) {
		t.Errorf("the plan changing changed more than the machine's CPU pool: %v, then %v", was, now)
	}

	both := cpuPooledLimits("4", 200)
	both.Memory, both.MemoryLimitPercent = proto.String("8Gi"), proto.Int32(200)
	setDefaultPlan(t, s, both)
	waitPools("in both pools", &computev1alpha1.CPUPool{Name: "acme", Cpu: "4"}, &computev1alpha1.MemoryPool{Name: "acme", Size: "8192Mi"})

	setDefaultPlan(t, s, pooledLimits("8Gi", 200))
	waitPools("in the memory pool alone at 100 percent of cpu", nil, &computev1alpha1.MemoryPool{Name: "acme", Size: "8192Mi"})

	setDefaultPlan(t, s, cpuPooledLimits("4", 200))
	waitPools("in the CPU pool again", &computev1alpha1.CPUPool{Name: "acme", Cpu: "4"}, nil)

	// The tenant moved to another plan, paying it a fixed fee, while its
	// machines are there.
	big := pricedPlan("big", "300")
	big.Spec.Limits = cpuPooledLimits("6", 300)
	mustApply(t, s, root, registry.Plan, big)
	onPlan(t, s, root, "acme", "big")
	waitPools("in the CPU pool of the tenant's new plan", &computev1alpha1.CPUPool{Name: "acme", Cpu: "6"}, nil)
	mustApply(t, s, root, registry.BillingAccount, fixed("acme", minuteNow(s)))
	waitSettled(t, s)
	waitPools("in its pool on a fixed fee", &computev1alpha1.CPUPool{Name: "acme", Cpu: "6"}, nil)

	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD"))
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 1, minuteNow(s)))
	waitPools("in no pool paying as it goes", nil, nil)
}
