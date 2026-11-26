package e2e

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// pooled has the tenant's machines share a pool of memory, its plan's.
func (w *world) pooled(memory string) {
	w.t.Helper()
	ctx := context.Background()
	if _, err := w.root.Apply(ctx, registry.Plan, &tenancyv1alpha1.Plan{
		Metadata: &metav1.ObjectMeta{Name: "pooled"},
		Spec:     &tenancyv1alpha1.PlanSpec{Limits: &tenancyv1alpha1.Limits{Memory: proto.String(memory), MemoryLimitPercent: proto.Int32(400)}},
	}); err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.root.Apply(ctx, registry.Tenant, &tenancyv1alpha1.Tenant{
		Metadata: &metav1.ObjectMeta{Name: apiservertest.Tenant}, Spec: &tenancyv1alpha1.TenantSpec{Plan: "pooled"},
	}); err != nil {
		w.t.Fatal(err)
	}
}

func reservedContainer(name, memory, reserved string) *computev1alpha1.Container {
	c := container(name, "nginx")
	c.Spec.Resources = &computev1alpha1.Resources{Memory: memory, MemoryReserved: reserved}
	return c
}

// TestAPoolsChangesReachItsRunningMachines: a pool's size, or a machine's
// reservation, changing restarts nothing: each VM runs as it was launched,
// its node given what follows them while it runs, and the pool's slice
// held to its size, or what its machines have reserved together if more,
// by the squeeze.
func TestAPoolsChangesReachItsRunningMachines(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.pooled("4Gi")
	w.apply(reservedContainer("a", "2Gi", "1Gi"))
	w.apply(reservedContainer("b", "2Gi", "1Gi"))
	w.waitReason("a", "Running")
	w.waitReason("b", "Running")
	uids := map[string]string{"a": w.machine("a").GetMetadata().GetUid(), "b": w.machine("b").GetMetadata().GetUid()}
	launched := map[string]fake.VMView{}
	for name, uid := range uids {
		vm := w.host.Snapshot().VMs[uid]
		if p := vm.Spec.MemoryPool; p == nil || p.Name != apiservertest.Tenant || p.SizeMiB != 4096 || vm.Spec.ReservedMiB != 1024 {
			t.Fatalf("%s launched in %+v, reserved %d; want acme of 4096, 1024", name, p, vm.Spec.ReservedMiB)
		}
		launched[name] = vm
	}
	// Booted at their reservations, then given the pool.
	testutil.Eventually(t, "the guests given the pool", func() (bool, string) {
		a, b := w.host.BalloonOf(uids["a"]), w.host.BalloonOf(uids["b"])
		size, _ := w.host.MemoryPool(apiservertest.Tenant)
		return a == 0 && b == 0 && size == 4096, fmt.Sprintf("balloons %d, %d; pool %d", a, b, size)
	})

	// Its plan lowered past what they have reserved together.
	w.pooled("1Gi")
	w.apply(reservedContainer("b", "2Gi", "512Mi"))
	testutil.Eventually(t, "the node given the new pool and reservation", func() (bool, string) {
		pa, ga, _ := w.host.MemoryShare(uids["a"])
		pb, gb, _ := w.host.MemoryShare(uids["b"])
		size, _ := w.host.MemoryPool(apiservertest.Tenant)
		return pa != nil && pa.SizeMiB == 1024 && pb != nil && pb.SizeMiB == 1024 && ga == 1024 && gb == 512 && size == 1536,
			fmt.Sprintf("a %+v %d, b %+v %d, pool %d", pa, ga, pb, gb, size)
	})
	for name, uid := range uids {
		vm := w.host.Snapshot().VMs[uid]
		if vm.Boots != 1 || !vm.Started.Equal(launched[name].Started) || vm.SpecHash != launched[name].SpecHash || fmt.Sprint(vm.Run) != fmt.Sprint(launched[name].Run) {
			t.Errorf("%s: boots %d, started %v, hash %s; want it run on as launched", name, vm.Boots, vm.Started, vm.SpecHash)
		}
	}
}

// TestAnOOMKillIsReported: a machine whose VM the host's OOM killer killed
// says when, though its restart policy started it again.
func TestAnOOMKillIsReported(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	uid := w.machine("api").GetMetadata().GetUid()
	w.settle("api")
	if err := w.host.OOMKill(uid); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the kill reported", func() (bool, string) {
		c := conditions.Find(w.machine("api").GetStatus().GetConditions(), computeapi.MachineOOMKilled)
		return c != nil && c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE && c.GetReason() == computeapi.ReasonOutOfMemory, fmt.Sprint(c)
	})
	w.waitReason("api", "Running")
	if vm := w.host.Snapshot().VMs[uid]; vm.Boots != 2 {
		t.Errorf("boots %d, want 2", vm.Boots)
	}
}
