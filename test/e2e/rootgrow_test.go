package e2e

import (
	"fmt"
	"slices"
	"testing"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// growing is a container whose 4Gi root disk grows by itself, 2x at 80%, up
// to 16Gi.
func growing(name string) *computev1alpha1.Container {
	c := container(name, "nginx")
	c.Spec.Resources = &computev1alpha1.Resources{Disk: "4Gi", DiskGrow: &computev1alpha1.DiskGrow{Max: "16Gi"}}
	return c
}

// TestARootDiskGrowsByItself: a container's root filling past its
// threshold grows, step by step, while its VM runs, until its max; one
// below its threshold asks for nothing.
func TestARootDiskGrowsByItself(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.apply(growing("api"))
	w.waitReason("api", "Running")
	uid := w.machine("api").GetMetadata().GetUid()
	before := w.host.Snapshot().VMs[uid]

	w.host.FillRoot(uid, 3<<30) // 75%
	testutil.Unchanged(t, "a root below its threshold", 300*time.Millisecond, func() (string, error) {
		return fmt.Sprintf("root %d bytes, asks %q", w.host.Snapshot().Roots[uid].Size, w.machine("api").GetStatus().GetDiskWanted()), nil
	})

	grown := func(size int64) {
		t.Helper()
		testutil.Eventually(t, fmt.Sprintf("the root grown to %d bytes, the guest's filesystem with it", size), func() (bool, string) {
			s := w.host.Snapshot()
			vm := s.VMs[uid]
			c, _ := get[*computev1alpha1.Container](w, registry.Container, "api")
			return s.Roots[uid].Size == size && vm.GuestRootFS == size && c.GetStatus().GetDisk() == fmt.Sprintf("%dGi", size>>30),
				fmt.Sprintf("root %d bytes, the guest's filesystem %d, the container's disk %q", s.Roots[uid].Size, vm.GuestRootFS, c.GetStatus().GetDisk())
		})
	}
	cleared := func(what string) {
		t.Helper()
		testutil.Eventually(t, what, func() (bool, string) {
			got := w.machine("api").GetStatus().GetDiskWanted()
			return got == "", got
		})
	}
	w.host.FillRoot(uid, 7<<29) // 3.5Gi, 87.5%
	grown(8 << 30)
	cleared("the ask cleared at 3.5Gi of 8Gi")
	w.host.FillRoot(uid, 7<<30)
	grown(16 << 30)
	w.host.FillRoot(uid, 16<<30)
	cleared("the ask cleared at the max")
	testutil.Unchanged(t, "a full root at its max", 300*time.Millisecond, func() (string, error) {
		return fmt.Sprintf("root %d bytes, asks %q", w.host.Snapshot().Roots[uid].Size, w.machine("api").GetStatus().GetDiskWanted()), nil
	})
	vm := w.host.Snapshot().VMs[uid]
	if vm.Boots != before.Boots || !vm.Started.Equal(before.Started) {
		t.Errorf("the VM was started again to grow: started %v, booted %d times; was %v, %d", vm.Started, vm.Boots, before.Started, before.Boots)
	}
	if !slices.Equal(slices.DeleteFunc(w.host.RootCalls(uid), func(c string) bool { return c == fake.RootCallUsage }),
		[]string{fake.RootCallResize, fake.RootCallGrow, fake.RootCallResize, fake.RootCallGrow}) {
		t.Errorf("the root grew by %q; want the VM told, then the guest grown, for each step", w.host.RootCalls(uid))
	}
}

// TestAGrownRootWaitsForAnOldVMsNextBoot: a VM an older node started, which
// cannot be told its root's size while it runs, is not started again for
// its root to grow: it is told once, which it refuses, and sees the disk
// at its next boot.
func TestAGrownRootWaitsForAnOldVMsNextBoot(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := container("api", "nginx")
	c.Spec.Resources = &computev1alpha1.Resources{Disk: "4Gi"}
	w.apply(c)
	w.waitReason("api", "Running")
	uid := w.machine("api").GetMetadata().GetUid()
	w.settle("api")
	w.host.StartedByOldNode(uid)
	w.host.NoLiveResize(uid, true)
	before := w.host.Snapshot().VMs[uid]

	c.Spec.Resources.Disk = "8Gi"
	w.apply(c)
	w.waitReason("api", "Running")
	testutil.Eventually(t, "the root grown", func() (bool, string) {
		s := w.host.Snapshot().Roots[uid]
		return s.Size == 8<<30, fmt.Sprint(s.Size)
	})
	testutil.Unchanged(t, "the VM, told once", 300*time.Millisecond, func() (string, error) {
		vm := w.host.Snapshot().VMs[uid]
		return fmt.Sprintf("booted %d times at %v, sees %d bytes, told %q", vm.Boots, vm.Started, vm.GuestDisk, w.host.RootCalls(uid)), nil
	})
	vm := w.host.Snapshot().VMs[uid]
	if vm.Boots != before.Boots || !vm.Started.Equal(before.Started) || vm.GuestDisk != 4<<30 {
		t.Errorf("the old VM, its root grown, booted %d times, at %v, and sees %d bytes; want it running on at 4Gi", vm.Boots, vm.Started, vm.GuestDisk)
	}
	if calls := w.host.RootCalls(uid); !slices.Equal(calls, []string{fake.RootCallResize}) {
		t.Errorf("the old VM was asked %q; want one resize, refused", calls)
	}
	w.rootGrowth("api", metav1.ConditionStatus_CONDITION_STATUS_FALSE, computeapi.ReasonVMCannotResize)

	// Its next boot, by its restart policy, sees the disk grown.
	w.host.NoLiveResize(uid, false)
	if err := w.host.Exit(uid, 0); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the VM booted again on the grown root", func() (bool, string) {
		vm := w.host.Snapshot().VMs[uid]
		return vm.State == driver.VMRunning && vm.Started.After(before.Started) && vm.GuestRootFS == 8<<30,
			fmt.Sprintf("%v at %v, its filesystem %d", vm.State, vm.Started, vm.GuestRootFS)
	})
	w.rootGrowth("api", metav1.ConditionStatus_CONDITION_STATUS_TRUE, computeapi.ReasonRootGrew)
}

// rootGrowth waits for the container's machine's RootGrowth condition to
// be status, for reason, at its current generation.
func (w *world) rootGrowth(name string, status metav1.ConditionStatus, reason string) {
	w.t.Helper()
	testutil.Eventually(w.t, fmt.Sprintf("%s's RootGrowth %v, %s", name, status, reason), func() (bool, string) {
		m := w.machine(name)
		c := conditions.Find(m.GetStatus().GetConditions(), computeapi.RootGrowth)
		return c.GetStatus() == status && c.GetReason() == reason && c.GetMessage() != "" &&
			c.GetObservedGeneration() == m.GetMetadata().GetGeneration(), fmt.Sprint(c)
	})
}

// TestAnOldGuestIsNotGrownWhileItRuns: a guest from before GrowRoot keeps
// its filesystem as it is, and its VM is not started again for it.
func TestAnOldGuestIsNotGrownWhileItRuns(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.host.OldRootGuest("", true)
	w.apply(growing("api"))
	w.waitReason("api", "Running")
	uid := w.machine("api").GetMetadata().GetUid()
	usages := func() int {
		return len(slices.DeleteFunc(w.host.RootCalls(uid), func(c string) bool { return c != fake.RootCallUsage }))
	}
	testutil.Eventually(t, "the old guest asked", func() (bool, string) { return usages() > 0, fmt.Sprint(usages()) })
	before := w.host.Snapshot().VMs[uid]
	w.host.FillRoot(uid, 4<<30)

	c := growing("api")
	c.Spec.Resources.Disk = "8Gi"
	w.apply(c)
	testutil.Eventually(t, "the root grown, the VM told", func() (bool, string) {
		vm := w.host.Snapshot().VMs[uid]
		return w.host.Snapshot().Roots[uid].Size == 8<<30 && vm.GuestDisk == 8<<30, fmt.Sprint(vm.GuestDisk)
	})
	w.waitReason("api", "Running")
	testutil.Unchanged(t, "the old guest", 300*time.Millisecond, func() (string, error) {
		vm := w.host.Snapshot().VMs[uid]
		calls := slices.DeleteFunc(w.host.RootCalls(uid), func(c string) bool { return c == fake.RootCallUsage })
		return fmt.Sprintf("booted %d times at %v, its filesystem %d, grown by %q", vm.Boots, vm.Started, vm.GuestRootFS, calls), nil
	})
	vm := w.host.Snapshot().VMs[uid]
	if vm.Boots != before.Boots || !vm.Started.Equal(before.Started) || vm.GuestRootFS != 4<<30 {
		t.Errorf("the old guest booted %d times, at %v, its filesystem %d bytes; want it running on at 4Gi", vm.Boots, vm.Started, vm.GuestRootFS)
	}
	if n := usages(); n != 1 {
		t.Errorf("the old guest was asked how full its root is %d times; want once", n)
	}
	if got := w.machine("api").GetStatus().GetDiskWanted(); got != "" {
		t.Errorf("a guest that cannot say how full it is asks for %q", got)
	}
	w.rootGrowth("api", metav1.ConditionStatus_CONDITION_STATUS_FALSE, computeapi.ReasonGuestCannotGrow)
	if r := ready(w.waitReason("api", "Running")); r.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("a container whose root cannot grow is not Ready: %v", r)
	}
}

// TestARootThatGrowsHasNoCondition: a machine whose root grows while it
// runs, and never failed to, carries no RootGrowth condition.
func TestARootThatGrowsHasNoCondition(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.apply(growing("api"))
	w.waitReason("api", "Running")
	uid := w.machine("api").GetMetadata().GetUid()
	w.host.FillRoot(uid, 4<<30)
	testutil.Eventually(t, "the root grown", func() (bool, string) {
		vm := w.host.Snapshot().VMs[uid]
		return vm.GuestRootFS == 8<<30, fmt.Sprint(vm.GuestRootFS)
	})
	w.settle("api")
	if c := conditions.Find(w.machine("api").GetStatus().GetConditions(), computeapi.RootGrowth); c != nil {
		t.Errorf("RootGrowth %v; want none", c)
	}
}
