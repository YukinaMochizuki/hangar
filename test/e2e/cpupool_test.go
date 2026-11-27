package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// cpuPooled has the tenant's machines share a pool of cpu CPUs, its
// plan's, their vCPUs adding up to twice that.
func (w *world) cpuPooled(cpu string) {
	w.t.Helper()
	ctx := context.Background()
	if _, err := w.root.Apply(ctx, registry.Plan, &tenancyv1alpha1.Plan{
		Metadata: &metav1.ObjectMeta{Name: "cpu-pooled"},
		Spec:     &tenancyv1alpha1.PlanSpec{Limits: &tenancyv1alpha1.Limits{Cpu: proto.String(cpu), CpuLimitPercent: proto.Int32(200)}},
	}); err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.root.Apply(ctx, registry.Tenant, &tenancyv1alpha1.Tenant{
		Metadata: &metav1.ObjectMeta{Name: apiservertest.Tenant}, Spec: &tenancyv1alpha1.TenantSpec{Plan: "cpu-pooled"},
	}); err != nil {
		w.t.Fatal(err)
	}
}

func cpuContainer(name, cpu, memory string) *computev1alpha1.Container {
	c := container(name, "nginx")
	c.Spec.Resources = &computev1alpha1.Resources{Cpu: cpu, Memory: memory}
	return c
}

// TestACPUPoolHoldsItsMachinesToItsPlansCPUs: a tenant of 4 CPUs at 200
// percent runs 8 vCPUs, its VMs started in its pool, which the node holds
// to 4 CPUs; a 9th vCPU is refused; the plan changing reaches the node as
// the machines are reconciled, not at the squeeze's next look, while they
// run on as launched.
func TestACPUPoolHoldsItsMachinesToItsPlansCPUs(t *testing.T) {
	t.Parallel()
	w := newIdleWorld(t)
	// The squeeze's first look comes at once, the next after the test.
	w.squeezePoll = time.Hour
	w.startAgent()
	w.cpuPooled("4")
	w.apply(cpuContainer("a", "4", "2Gi"))
	w.apply(cpuContainer("b", "4", "2Gi"))
	w.waitReason("a", "Running")
	w.waitReason("b", "Running")
	uids := map[string]string{"a": w.machine("a").GetMetadata().GetUid(), "b": w.machine("b").GetMetadata().GetUid()}
	hashes := map[string]string{}
	for name, uid := range uids {
		vm := w.host.Snapshot().VMs[uid]
		if p := vm.Spec.CPUPool; p == nil || p.Name != apiservertest.Tenant || p.MilliCPU != 4000 || vm.Spec.MemoryPool != nil {
			t.Fatalf("%s launched in CPU pool %+v, memory pool %+v; want acme of 4000, none", name, p, vm.Spec.MemoryPool)
		}
		hashes[name] = vm.SpecHash
	}
	if got := w.host.CPUPool(apiservertest.Tenant); got != 4000 {
		t.Errorf("the node holds the pool to %d millicores, want 4000", got)
	}
	_, err := w.root.Apply(context.Background(), registry.Container, cpuContainer("c", "1", "512Mi"))
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("a 9th vCPU: %v, want resource_exhausted", err)
	}

	w.cpuPooled("3")
	testutil.Eventually(t, "the node given the plan's new CPUs", func() (bool, string) {
		got := w.host.CPUPool(apiservertest.Tenant)
		return got == 3000, fmt.Sprintf("pool of %d", got)
	})
	for name, uid := range uids {
		if vm := w.host.Snapshot().VMs[uid]; vm.Boots != 1 || vm.SpecHash != hashes[name] {
			t.Errorf("%s: boots %d, hash %s; want it run on as launched", name, vm.Boots, vm.SpecHash)
		}
	}
}
