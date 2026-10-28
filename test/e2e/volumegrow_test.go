package e2e

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// applyGrowingDisk applies a Disk of size that grows by itself up to max,
// 2x at 80%.
func (w *world) applyGrowingDisk(name, size, max string) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.Disk, &storagev1alpha1.Disk{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec:     &storagev1alpha1.DiskSpec{Size: size, Grow: &storagev1alpha1.DiskGrow{Max: max}},
	}); err != nil {
		w.t.Fatal(err)
	}
}

// withData is a container mounting the Disk data read-write at /data.
func withData(name string) *computev1alpha1.Container {
	c := container(name, "nginx")
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{{Type: "volume", Source: "data", Target: "/data"}}
	return c
}

// machineCondition waits for the container's machine's condition of type
// typ to be status, for reason, at its current generation.
func (w *world) machineCondition(name, typ string, status metav1.ConditionStatus, reason string) {
	w.t.Helper()
	testutil.Eventually(w.t, fmt.Sprintf("%s's %s %v, %s", name, typ, status, reason), func() (bool, string) {
		m := w.machine(name)
		c := conditions.Find(m.GetStatus().GetConditions(), typ)
		return c.GetStatus() == status && c.GetReason() == reason && c.GetMessage() != "" &&
			c.GetObservedGeneration() == m.GetMetadata().GetGeneration(), fmt.Sprint(c)
	})
}

// grownBy is the calls of RootCalls that grow something, as opposed to
// asking how full it is.
func grownBy(calls []string) []string {
	return slices.DeleteFunc(slices.Clone(calls), func(c string) bool {
		return c == fake.RootCallUsage || strings.HasPrefix(c, fake.RootCallVolumeUsage)
	})
}

// TestADiskVolumeGrowsByItself: a Disk that grows by itself, a running
// container's volume filling past its threshold, grows, step by step, while
// the VM runs, the VM told, then the guest's filesystem grown, until its
// max; below its threshold it asks for nothing.
func TestADiskVolumeGrowsByItself(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.applyGrowingDisk("data", "2Gi", "8Gi")
	w.apply(withData("db"))
	w.waitReason("db", "Running")
	muid := w.machine("db").GetMetadata().GetUid()
	duid := w.waitDisk("data", "a volume of db", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetAttachment().GetName() == "db"
	}).GetMetadata().GetUid()
	before := w.host.Snapshot().VMs[muid]

	w.host.FillVolume(duid, 3<<29) // 1.5Gi, 75%
	testutil.Unchanged(t, "a volume below its threshold", 300*time.Millisecond, func() (string, error) {
		return fmt.Sprintf("volume %d bytes, asks %v", w.host.Snapshot().Volumes[duid].Size, w.machine("db").GetStatus().GetDisksWanted()), nil
	})
	grown := func(size int64) {
		t.Helper()
		testutil.Eventually(t, fmt.Sprintf("data grown to %d bytes, the guest's filesystem with it", size), func() (bool, string) {
			s := w.host.Snapshot()
			vm := s.VMs[muid]
			d, _ := get[*storagev1alpha1.Disk](w, registry.Disk, "data")
			return s.Volumes[duid].Size == size && len(vm.GuestVolumeFS) == 1 && vm.GuestVolumeFS[0] == size &&
					d.GetStatus().GetSize() == fmt.Sprintf("%dGi", size>>30),
				fmt.Sprintf("volume %d bytes, the guest's filesystem %v, the disk's size %q", s.Volumes[duid].Size, vm.GuestVolumeFS, d.GetStatus().GetSize())
		})
	}
	cleared := func(what string) {
		t.Helper()
		testutil.Eventually(t, what, func() (bool, string) {
			got := w.machine("db").GetStatus().GetDisksWanted()
			return len(got) == 0, fmt.Sprint(got)
		})
	}
	w.host.FillVolume(duid, 7<<28) // 1.75Gi, 87.5%
	grown(4 << 30)
	cleared("the ask cleared at 1.75Gi of 4Gi")
	w.host.FillVolume(duid, 7<<29)
	grown(8 << 30)
	w.host.FillVolume(duid, 8<<30)
	cleared("the ask cleared at the max")
	testutil.Unchanged(t, "a full volume at its max", 300*time.Millisecond, func() (string, error) {
		return fmt.Sprintf("volume %d bytes, asks %v", w.host.Snapshot().Volumes[duid].Size, w.machine("db").GetStatus().GetDisksWanted()), nil
	})
	vm := w.host.Snapshot().VMs[muid]
	if vm.Boots != before.Boots || !vm.Started.Equal(before.Started) {
		t.Errorf("the VM was started again to grow: started %v, booted %d times; was %v, %d", vm.Started, vm.Boots, before.Started, before.Boots)
	}
	want := []string{fake.RootCallResizeVolume + " 0", fake.RootCallGrowVolume + " 0", fake.RootCallResizeVolume + " 0", fake.RootCallGrowVolume + " 0"}
	if got := grownBy(w.host.RootCalls(muid)); !slices.Equal(got, want) {
		t.Errorf("data grew by %q; want the VM told, then the guest grown, for each step", got)
	}
	if c := conditions.Find(w.machine("db").GetStatus().GetConditions(), computeapi.VolumeGrowth); c != nil {
		t.Errorf("VolumeGrowth %v; want none, nothing failed", c)
	}
}

// TestAnOldGuestsVolumeIsNotGrownWhileItRuns: a guest from before
// VolumeUsage and GrowVolume keeps its volume's filesystem as it is, its
// machine says so, and its VM is not started again for it.
func TestAnOldGuestsVolumeIsNotGrownWhileItRuns(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.host.OldVolumeGuest("", true)
	w.applyGrowingDisk("data", "2Gi", "8Gi")
	w.apply(withData("db"))
	w.waitReason("db", "Running")
	muid := w.machine("db").GetMetadata().GetUid()
	before := w.host.Snapshot().VMs[muid]
	w.machineCondition("db", computeapi.VolumeGrowth, metav1.ConditionStatus_CONDITION_STATUS_FALSE, computeapi.ReasonGuestCannotGrow)

	// Grown by hand, it is told the VM, which runs on.
	w.applyGrowingDisk("data", "4Gi", "8Gi")
	testutil.Eventually(t, "the VM told data's size", func() (bool, string) {
		vm := w.host.Snapshot().VMs[muid]
		return len(vm.GuestVolumes) == 1 && vm.GuestVolumes[0] == 4<<30, fmt.Sprint(vm.GuestVolumes)
	})
	w.waitReason("db", "Running")
	vm := w.host.Snapshot().VMs[muid]
	if vm.Boots != before.Boots || !vm.Started.Equal(before.Started) || vm.GuestVolumeFS[0] != 2<<30 {
		t.Errorf("the old guest booted %d times, at %v, its filesystem %v; want it running on at 2Gi", vm.Boots, vm.Started, vm.GuestVolumeFS)
	}
	if n := len(slices.DeleteFunc(w.host.RootCalls(muid), func(c string) bool { return !strings.HasPrefix(c, fake.RootCallVolumeUsage) })); n != 1 {
		t.Errorf("the old guest was asked how full its volume is %d times; want once", n)
	}
	w.machineCondition("db", computeapi.VolumeGrowth, metav1.ConditionStatus_CONDITION_STATUS_FALSE, computeapi.ReasonGuestCannotGrow)
}

// TestAGrownVolumeWaitsForAnOldVMsNextBoot: a VM an older node started,
// which cannot be told its volumes' sizes while it runs, is not started
// again for a volume to grow: it is told once, which it refuses, says so,
// and sees the volume at its next boot.
func TestAGrownVolumeWaitsForAnOldVMsNextBoot(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.applyDisk("data", "2Gi")
	w.apply(withData("db"))
	w.waitReason("db", "Running")
	muid := w.machine("db").GetMetadata().GetUid()
	w.settle("db")
	w.host.StartedByOldNode(muid)
	w.host.NoLiveResize(muid, true)
	before := w.host.Snapshot().VMs[muid]

	w.applyDisk("data", "4Gi")
	w.machineCondition("db", computeapi.VolumeGrowth, metav1.ConditionStatus_CONDITION_STATUS_FALSE, computeapi.ReasonVMCannotResize)
	vm := w.host.Snapshot().VMs[muid]
	if vm.Boots != before.Boots || !vm.Started.Equal(before.Started) || vm.GuestVolumes[0] != 2<<30 {
		t.Errorf("the old VM, its volume grown, booted %d times, at %v, and sees %v; want it running on at 2Gi", vm.Boots, vm.Started, vm.GuestVolumes)
	}
	if got := grownBy(w.host.RootCalls(muid)); !slices.Equal(got, []string{fake.RootCallResizeVolume + " 0"}) {
		t.Errorf("the old VM was asked %q; want one resize, refused", got)
	}

	w.host.NoLiveResize(muid, false)
	if err := w.host.Exit(muid, 0); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the VM booted again on the grown volume", func() (bool, string) {
		vm := w.host.Snapshot().VMs[muid]
		return vm.Started.After(before.Started) && len(vm.GuestVolumeFS) == 1 && vm.GuestVolumeFS[0] == 4<<30,
			fmt.Sprintf("at %v, its filesystem %v", vm.Started, vm.GuestVolumeFS)
	})
	w.machineCondition("db", computeapi.VolumeGrowth, metav1.ConditionStatus_CONDITION_STATUS_TRUE, computeapi.ReasonVolumeGrew)
}
