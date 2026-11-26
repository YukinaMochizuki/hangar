package apiserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
)

// pooledLimits are limits of memory whose machines' memory may come to
// percent of it.
func pooledLimits(memory string, percent int32) *tenancyv1alpha1.Limits {
	return &tenancyv1alpha1.Limits{Memory: proto.String(memory), MemoryLimitPercent: proto.Int32(percent)}
}

// guaranteed is a container of memory, with the fewest vCPUs the shape
// rules allow it, guaranteed g; "" for none.
func guaranteed(name, memory, g string) *computev1alpha1.Container {
	c := ctr(name)
	c.Spec.Resources = &computev1alpha1.Resources{Memory: memory, MemoryGuaranteed: g}
	return c
}

// wantPrecondition checks err refuses as FailedPrecondition, saying want.
func wantPrecondition(t *testing.T, err error, want, what string) {
	t.Helper()
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: got %v, want failed_precondition saying %q", what, err, want)
	}
}

// wantInvalid checks err refuses as InvalidArgument, saying want.
func wantInvalid(t *testing.T, err error, want, what string) {
	t.Helper()
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: got %v, want invalid_argument saying %q", what, err, want)
	}
}

// TestANodeHoldsAPoolOnce: the machines of a pool on a node hold the least
// of their memory and the pool's size; a machine of no pool holds its
// memory, as before pools.
func TestANodeHoldsAPoolOnce(t *testing.T) {
	t.Parallel()
	gi := func(n uint64) uint64 { return n * quantity.GiB }
	alone := func(mem uint64) held { return held{memory: mem} }
	pooled := func(mem uint64) held { return held{memory: mem, pool: "acme", poolSize: gi(8)} }

	var none nodeHeld
	for _, h := range []held{alone(gi(4)), alone(gi(2))} {
		if need := none.memoryFor(h); need != h.memory {
			t.Errorf("a machine of no pool needs %d, want its memory %d", need, h.memory)
		}
		none.take(h)
	}
	if none.memory() != gi(6) {
		t.Errorf("machines of no pool hold %d, want their sum %d", none.memory(), gi(6))
	}

	var u nodeHeld
	for i, want := range []uint64{gi(4), gi(4), 0, 0} {
		if need := u.memoryFor(pooled(gi(4))); need != want {
			t.Errorf("pooled machine %d needs %d more, want %d", i+1, need, want)
		}
		u.take(pooled(gi(4)))
	}
	if u.memory() != gi(8) {
		t.Errorf("a pool of 8Gi with 4+4+4+4Gi holds %d, want 8Gi", u.memory())
	}
	if need := u.memoryFor(pooled(gi(4))); need != 0 {
		t.Errorf("a fifth 4Gi machine needs %d more, want none", need)
	}
	// Machines of no pool beside a pool hold their own.
	if u.take(alone(gi(1))); u.memory() != gi(9) {
		t.Errorf("a pool of 8Gi beside a machine of 1Gi holds %d, want 9Gi", u.memory())
	}
	// A pool its machines disagree on the size of holds the largest.
	if need := u.memoryFor(held{memory: gi(4), pool: "acme", poolSize: gi(12)}); need != gi(4) {
		t.Errorf("a machine bringing a larger pool size needs %d, want 4Gi", need)
	}
	var mixed nodeHeld
	for _, size := range []uint64{gi(12), gi(8), gi(8)} {
		mixed.take(held{memory: gi(8), pool: "acme", poolSize: size})
	}
	if mixed.memory() != gi(12) {
		t.Errorf("a pool of sizes 12, 8 and 8Gi holds %d, want the largest, 12Gi", mixed.memory())
	}
	// Guarantees past the pool's size, as when a plan is lowered under its
	// running machines: held whole.
	var over nodeHeld
	for range 3 {
		over.take(held{memory: gi(4), guaranteed: gi(3), pool: "acme", poolSize: gi(8)})
	}
	if over.memory() != gi(9) {
		t.Errorf("a pool of 8Gi guaranteeing 3+3+3Gi holds %d, want the guarantees, 9Gi", over.memory())
	}
	// A stopped machine of a pool holds nothing.
	if need := u.memoryFor(held{pool: "acme", poolSize: gi(8)}); need != 0 {
		t.Errorf("a stopped pooled machine needs %d", need)
	}

	// Split across two nodes, the pool holds up to its size on each: more
	// than it can use, never less.
	nodes := map[string]nodeHeld{}
	for i := range 6 {
		n := []string{"n1", "n2"}[i%2]
		h := nodes[n]
		h.take(pooled(gi(4)))
		nodes[n] = h
	}
	if nodes["n1"].memory() != gi(8) || nodes["n2"].memory() != gi(8) {
		t.Errorf("a pool split across two nodes holds %d and %d, want 8Gi on each", nodes["n1"].memory(), nodes["n2"].memory())
	}
}

func TestMemoryLimitPercentIsAPlans(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	plan := func(l *tenancyv1alpha1.Limits) error {
		_, err := s.backend(registry.Plan).Apply(system, &tenancyv1alpha1.Plan{
			Metadata: &metav1.ObjectMeta{Name: "pooled"}, Spec: &tenancyv1alpha1.PlanSpec{Limits: l},
		})
		return err
	}
	wantInvalid(t, plan(pooledLimits("8Gi", 99)), "memory_limit_percent: 99 is not from 100 to 400", "a percent below 100")
	wantInvalid(t, plan(pooledLimits("8Gi", 401)), "memory_limit_percent: 401 is not from 100 to 400", "a percent above 400")
	wantInvalid(t, plan(&tenancyv1alpha1.Limits{MemoryLimitPercent: proto.Int32(200)}), "spec.limits.memory, which is not set", "a percent of no memory")
	for _, p := range []int32{100, 400} {
		if err := plan(pooledLimits("8Gi", p)); err != nil {
			t.Errorf("a percent of %d: %v", p, err)
		}
	}
	_, err := s.backend(registry.Project).Apply(user, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "web"},
		Spec:     &tenancyv1alpha1.ProjectSpec{Tenant: "acme", Limits: pooledLimits("4Gi", 200)},
	})
	wantInvalid(t, err, "memory_limit_percent is not a project's to set", "a project setting a percent")
}

// TestAGuaranteeBelowMemoryIsAPooledTenants: memory_guaranteed keeps to its
// steps and range for anyone, and is below memory only for a tenant whose
// plan's percent is above 100 and who does not pay as it goes; root, whom
// the shape rules exempt, included.
func TestAGuaranteeBelowMemoryIsAPooledTenants(t *testing.T) {
	t.Parallel()
	s, system, root, alice := billedWith(t, false)
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Memory: proto.String("16Gi")})
	cb := s.backend(registry.Container)
	apply := func(ctx context.Context, c *computev1alpha1.Container) error {
		_, err := cb.Apply(ctx, c)
		return err
	}
	wantInvalid(t, apply(alice, guaranteed("a", "1Gi", "300Mi")), "memory_guaranteed 300Mi is not a whole number of 256Mi steps", "a guarantee off the steps")
	wantInvalid(t, apply(alice, guaranteed("a", "1Gi", "2Gi")), "memory_guaranteed 2Gi is above spec.resources.memory 1024Mi", "a guarantee above memory")
	wantInvalid(t, apply(alice, guaranteed("a", "1Gi", "lots")), "spec.resources.memory_guaranteed:", "a guarantee that is no quantity")
	if err := apply(alice, guaranteed("a", "1Gi", "1Gi")); err != nil {
		t.Errorf("a guarantee of all its memory, with no pool: %v", err)
	}
	const noPool = `tenant "acme"'s plan "default" has a memory_limit_percent of 100`
	wantPrecondition(t, apply(alice, guaranteed("b", "1Gi", "512Mi")), noPool, "a guarantee below memory with no pool")
	wantPrecondition(t, apply(root, guaranteed("b", "1Gi", "512Mi")), noPool, "root's guarantee below memory with no pool")
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	vm := vmFromImage("vm", "noble")
	vm.Spec.Resources = &computev1alpha1.VirtualMachineResources{Memory: "2Gi", MemoryGuaranteed: "1Gi"}
	_, err := s.backend(registry.VirtualMachine).Apply(alice, vm)
	wantPrecondition(t, err, noPool, "a VM's guarantee below memory with no pool")

	setDefaultPlan(t, s, pooledLimits("16Gi", 200))
	if err := apply(alice, guaranteed("b", "1Gi", "512Mi")); err != nil {
		t.Errorf("a guarantee below memory in a pool, with no billing account: %v", err)
	}
	if _, err := s.backend(registry.VirtualMachine).Apply(alice, vm); err != nil {
		t.Errorf("a VM's guarantee below memory in a pool: %v", err)
	}

	now := minuteNow(s)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD"))
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 1, now))
	wantPrecondition(t, apply(alice, guaranteed("c", "1Gi", "512Mi")), `tenant "acme"'s billing account is PAY_AS_YOU_GO`, "a guarantee below memory paying as it goes")
	// What a write leaves as it was is not judged again.
	if err := apply(alice, guaranteed("b", "1Gi", "512Mi")); err != nil {
		t.Errorf("applying a pooled container again, unchanged, paying as it goes: %v", err)
	}
	wantPrecondition(t, apply(alice, guaranteed("b", "1Gi", "256Mi")), "PAY_AS_YOU_GO", "lowering a guarantee paying as it goes")
	wantPrecondition(t, apply(alice, guaranteed("b", "2Gi", "512Mi")), "PAY_AS_YOU_GO", "growing the memory over a guarantee paying as it goes")
	if err := apply(alice, guaranteed("b", "1Gi", "")); err != nil {
		t.Errorf("giving a container all its memory again: %v", err)
	}
}

// TestQuotaCountsGuaranteesAndMemory: memory counts what machines are
// guaranteed against the memory limit, and their memory against the limit
// times the plan's percent; at 100 with no guarantee, both are the memory
// as before.
func TestQuotaCountsGuaranteesAndMemory(t *testing.T) {
	t.Parallel()
	s, _, user := running(t)
	cb := s.backend(registry.Container)

	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Memory: proto.String("1Gi")})
	for _, name := range []string{"p1", "p2"} {
		if _, err := cb.Apply(user, ctr(name)); err != nil {
			t.Fatal(err)
		}
	}
	tl, err := s.tally(context.Background(), "acme", nil)
	if err != nil {
		t.Fatal(err)
	}
	if u := tl.tenant.usage(); u.GetMemory() != "1Gi" || u.GetMemoryCeiling() != "1Gi" {
		t.Errorf("with no guarantee, acme uses %v; want memory and memory_ceiling both 1Gi", u)
	}
	_, err = cb.Apply(user, ctr("p3"))
	wantExhausted(t, err, `tenant "acme"'s memory limit is 1Gi (plan "default") and it uses 1Gi; this would bring it to 1536Mi`, "past the memory limit at 100 percent")
	for _, name := range []string{"p1", "p2"} {
		if _, err := cb.Apply(user, stopped(ctr(name))); err != nil {
			t.Fatal(err)
		}
	}

	setDefaultPlan(t, s, pooledLimits("2Gi", 200))
	for _, c := range []*computev1alpha1.Container{guaranteed("a", "1Gi", "512Mi"), guaranteed("b", "2Gi", "512Mi")} {
		if _, err := cb.Apply(user, c); err != nil {
			t.Fatalf("container %s: %v", c.GetMetadata().GetName(), err)
		}
	}
	_, err = cb.Apply(user, guaranteed("c", "2Gi", "512Mi"))
	wantExhausted(t, err, `the memory of tenant "acme"'s machines asked to run may come to 4Gi, 200% of its memory limit of 2Gi (plan "default", memory_limit_percent), and comes to 3Gi; this would bring it to 5Gi`,
		"memory past the limit times the percent")
	if _, err := cb.Apply(user, guaranteed("c", "1Gi", "512Mi")); err != nil {
		t.Fatalf("memory up to the limit times the percent: %v", err)
	}
	_, err = cb.Apply(user, stopped(guaranteed("d", "1Gi", "")))
	if err != nil {
		t.Fatalf("a stopped container counts no memory: %v", err)
	}
	_, err = cb.Apply(user, guaranteed("d", "1Gi", ""))
	wantExhausted(t, err, `tenant "acme"'s memory limit is 2Gi (plan "default") and it uses 1536Mi; this would bring it to 2560Mi`, "guarantees past the memory limit")
	tl, err = s.tally(context.Background(), "acme", nil)
	if err != nil {
		t.Fatal(err)
	}
	if u := tl.tenant.usage(); u.GetMemory() != "1536Mi" || u.GetMemoryCeiling() != "4Gi" {
		t.Errorf("acme uses %v; want memory 1536Mi guaranteed, memory_ceiling 4Gi", u)
	}

	// A project's memory limit holds both sums, with its tenant's percent.
	if _, err := s.backend(registry.Project).Apply(user, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "web"},
		Spec:     &tenancyv1alpha1.ProjectSpec{Tenant: "acme", Limits: &tenancyv1alpha1.Limits{Memory: proto.String("2Gi")}},
	}); err != nil {
		t.Fatal(err)
	}
	setDefaultPlan(t, s, pooledLimits("4Gi", 400))
	_, err = cb.Apply(user, guaranteed("e", "5Gi", "256Mi"))
	wantExhausted(t, err, `the memory of project "web"'s machines asked to run may come to 8Gi, 400% (tenant "acme"'s memory_limit_percent) of its memory limit of 2Gi (its spec.limits), and comes to 4Gi; this would bring it to 9Gi`,
		"a project's memory past its limit times the percent")
	if _, err := cb.Apply(user, guaranteed("e", "4Gi", "256Mi")); err != nil {
		t.Fatalf("a project's memory up to its limit times the percent: %v", err)
	}
	_, err = cb.Apply(user, guaranteed("f", "1Gi", "512Mi"))
	wantExhausted(t, err, `project "web"'s memory limit is 2Gi (its spec.limits) and it uses 1792Mi; this would bring it to 2304Mi`, "a project's guarantees past its limit")
}

// TestPooledMachinesCarryTheirPool: the controllers give the machines of a
// pooled tenant its pool, and follow its plan, its tenant's spec.plan and
// its billing account changing, changing nothing else of the machine.
func TestPooledMachinesCarryTheirPool(t *testing.T) {
	t.Parallel()
	s, system, root, alice := billed(t)
	setDefaultPlan(t, s, pooledLimits("8Gi", 200))
	applyCtr(t, s, alice, guaranteed("api", "1Gi", "512Mi"))
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
	waitPools := func(what string, want *computev1alpha1.MemoryPool) {
		t.Helper()
		for _, name := range machines {
			waitMachine(t, s, system, name, what, func(m *computev1alpha1.Machine) bool { return proto.Equal(m.GetSpec().GetMemoryPool(), want) })
		}
		// A FileShare's server counts toward no quota, so it is in no pool.
		waitSettled(t, s)
		if p := getMachine(t, s, system, resource.Key{Project: "web", Name: shareServerName("data")}).GetSpec().GetMemoryPool(); p != nil {
			t.Errorf("%s: the share's server is in pool %v", what, p)
		}
	}
	m := waitMachine(t, s, system, "ct-api", "in acme's pool", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetMemoryPool() != nil })
	if r := m.GetSpec().GetResources(); r.GetMemory() != "1024Mi" || r.GetMemoryGuaranteed() != "512Mi" {
		t.Errorf("the machine's resources are %v, want 1024Mi guaranteed 512Mi", r)
	}
	waitPools("in acme's pool of 8Gi", &computev1alpha1.MemoryPool{Name: "acme", Size: "8192Mi"})
	waitSettled(t, s)
	before := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"})

	setDefaultPlan(t, s, pooledLimits("16Gi", 200))
	waitPools("in a pool grown with the plan", &computev1alpha1.MemoryPool{Name: "acme", Size: "16384Mi"})
	waitSettled(t, s)
	after := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"})
	was, now := proto.Clone(before.GetSpec()).(*computev1alpha1.MachineSpec), proto.Clone(after.GetSpec()).(*computev1alpha1.MachineSpec)
	was.MemoryPool, now.MemoryPool = nil, nil
	if !proto.Equal(was, now) {
		t.Errorf("the plan changing changed more than the machine's pool: %v, then %v", was, now)
	}

	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Memory: proto.String("16Gi")})
	waitPools("in no pool at 100 percent", nil)

	mustApply(t, s, root, registry.Plan, &tenancyv1alpha1.Plan{
		Metadata: &metav1.ObjectMeta{Name: "big"}, Spec: &tenancyv1alpha1.PlanSpec{Limits: pooledLimits("32Gi", 300)},
	})
	onPlan(t, s, root, "acme", "big")
	waitPools("in the pool of the tenant's new plan", &computev1alpha1.MemoryPool{Name: "acme", Size: "32768Mi"})

	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD"))
	// Settled, only the account wakes the controllers.
	waitSettled(t, s)
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 1, minuteNow(s)))
	waitPools("in no pool paying as it goes", nil)
}

// TestAPoolFitsOnANodeOnce: a node of 8Gi takes a pool of 8Gi's machines
// past 8Gi of memory, and the console's node usage counts the pool once.
func TestAPoolFitsOnANodeOnce(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 8192)
	setDefaultPlan(t, s, pooledLimits("8Gi", 400))
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		applyCtr(t, s, user, guaranteed(name, "4Gi", "1Gi"))
		placedOn(t, s, system, name, "n1")
	}
	got, err := s.nodeUsageSummary(system)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].GetMemory() != "8Gi" || got[0].GetMachines() != 5 {
		t.Errorf("node usage is %v, want 5 machines holding 8Gi", got)
	}

	// At 100 percent the pool is gone, but leaving it would take the node
	// past its memory: each machine keeps its pool and says so.
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Memory: proto.String("64Gi")})
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		waitContainer(t, s, system, name, "kept in its pool, as the node has no room", func(c *computev1alpha1.Container) bool {
			return containerReady(c).GetReason() == "ResizeDoesNotFit"
		})
		if m := getMachine(t, s, system, resource.Key{Project: "web", Name: machineName(name)}); m.GetSpec().GetMemoryPool() == nil {
			t.Errorf("machine %s left its pool on a node with no room for it", name)
		}
	}
	// Stopped, a machine leaves the pool, holding nothing.
	stopCtr(t, s, system, user, guaranteed("a", "4Gi", "1Gi"))
	if m := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-a"}); m.GetSpec().GetMemoryPool() != nil {
		t.Errorf("a stopped machine kept its pool: %v", m.GetSpec().GetMemoryPool())
	}
}

// TestLosingThePoolCountsFullMemory: once its tenant has no pool, by its
// plan's percent or its account, a machine guaranteed less than its memory
// counts and is billed its memory, the quota and the meter alike; over its
// limit then, the tenant can still stop and change machines, but not add.
func TestLosingThePoolCountsFullMemory(t *testing.T) {
	t.Parallel()
	s, system, root, alice := billed(t)
	now := minuteNow(s)
	plan := func(l *tenancyv1alpha1.Limits) {
		p := pricedPlan("pooled", "300")
		p.Spec.Limits = l
		mustApply(t, s, root, registry.Plan, p)
	}
	plan(pooledLimits("8Gi", 200))
	onPlan(t, s, root, "acme", "pooled")
	mustApply(t, s, root, registry.BillingAccount, fixed("acme", now))
	for _, name := range []string{"a", "b"} {
		applyCtr(t, s, alice, guaranteed(name, "2Gi", "512Mi"))
		m := waitMachine(t, s, system, machineName(name), "placed in its pool", func(m *computev1alpha1.Machine) bool {
			return m.GetSpec().GetNode() == "n1" && m.GetSpec().GetMemoryPool() != nil
		})
		reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration())
	}
	minute := now
	// usage.memory, and what the meter bills for the next minute, in MiB.
	agree := func(what, memory string, mib int64) {
		t.Helper()
		waitUsage(t, s, alice, what, func(ts *tenancyv1alpha1.TenantStatus, _ *tenancyv1alpha1.ProjectStatus) bool {
			return ts.GetUsage().GetMemory() == memory && ts.GetUsage().GetMemoryCeiling() == "4Gi"
		})
		if err := s.meterOnce(system, minute); err != nil {
			t.Fatal(err)
		}
		held, err := s.meterHeld(system, "acme", minute, minute.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if held["memory"] != mib {
			t.Errorf("%s: the meter bills %d MiB of memory, the quota counts %s", what, held["memory"], memory)
		}
		minute = minute.Add(time.Minute)
	}
	agree("in a pool", "1Gi", 1024)

	plan(&tenancyv1alpha1.Limits{Memory: proto.String("8Gi")})
	agree("out of the pool at 100 percent", "4Gi", 4096)

	plan(pooledLimits("8Gi", 200))
	agree("in the pool again", "1Gi", 1024)

	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD"))
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 1, now))
	agree("out of the pool paying as it goes", "4Gi", 4096)

	// Over a limit it fits only in a pool, it can still change and stop
	// what it has, but not add.
	plan(&tenancyv1alpha1.Limits{Memory: proto.String("2Gi")})
	cb := s.backend(registry.Container)
	changed := guaranteed("a", "2Gi", "512Mi")
	changed.Spec.Image = "redis"
	if _, err := cb.Apply(alice, changed); err != nil {
		t.Errorf("changing a container's image over the limit: %v", err)
	}
	if _, err := cb.Apply(alice, stopped(guaranteed("b", "2Gi", "512Mi"))); err != nil {
		t.Errorf("stopping a container over the limit: %v", err)
	}
	_, err := cb.Apply(alice, guaranteed("c", "512Mi", ""))
	wantExhausted(t, err, `tenant "acme"'s memory limit is 2Gi (plan "pooled") and it uses 2Gi; this would bring it to 2560Mi`, "a new container over the limit")
}

// TestThePoolsMeterIsItsGuarantee: a pooled machine running is billed what
// it is guaranteed, not its memory.
func TestThePoolsMeterIsItsGuarantee(t *testing.T) {
	t.Parallel()
	s, system, root, alice := billed(t)
	now := minuteNow(s)
	plan := pricedPlan("pooled", "300")
	plan.Spec.Limits = pooledLimits("8Gi", 200)
	mustApply(t, s, root, registry.Plan, plan)
	onPlan(t, s, root, "acme", "pooled")
	mustApply(t, s, root, registry.BillingAccount, fixed("acme", now))
	applyCtr(t, s, alice, guaranteed("api", "2Gi", "512Mi"))
	m := waitMachine(t, s, system, "ct-api", "placed in its pool", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "n1" && m.GetSpec().GetMemoryPool() != nil
	})
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration())
	if err := s.meterOnce(system, now); err != nil {
		t.Fatal(err)
	}
	held, err := s.meterHeld(system, "acme", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if held["memory"] != 512 {
		t.Errorf("a pooled machine's minute = %v, want 512 MiB of memory, its guarantee", held)
	}
}
