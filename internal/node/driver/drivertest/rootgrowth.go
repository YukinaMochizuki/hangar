package drivertest

import (
	"fmt"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// rootGrowth grows a running container's root disk without starting it
// again: the disk grown, the VM told its size, the guest grows its
// filesystem into it, each twice to no harm, and the VM runs on as the
// spec with its root grown.
func rootGrowth(t *testing.T, h Host) {
	ctx := t.Context()
	if _, err := h.Hypervisor.ResizeRoot(ctx, "m1"); err == nil {
		t.Error("resized the root disk of a machine with no VM")
	}
	spec := vmSpec(t, h, "m1", driver.RestartAlways)
	t.Cleanup(func() {
		_ = h.Hypervisor.Stop(ctx, "m1")
		_ = h.Hypervisor.DeleteLogs(ctx, "m1")
		_ = h.Network.DeleteInterfaces(ctx, "m1")
		_ = h.Storage.DeleteRoot(ctx, "m1")
	})
	must(t, h.Hypervisor.Start(ctx, spec))
	testutil.EventuallyWithin(t, vmWait, "the guest agent answering", func() bool { return h.Guest.Ready(ctx, "m1") == nil })
	before, _ := vmOf(t, h, "m1")
	if before.RootSize != spec.Root.Size {
		t.Errorf("the VM lists its root as started at %d bytes; it was %d", before.RootSize, spec.Root.Size)
	}
	booted, err := h.Guest.RootUsage(ctx, "m1")
	must(t, err)
	if booted.Size == 0 || booted.Size > uint64(spec.Root.Size) || booted.Used > booted.Size {
		t.Fatalf("the guest's root, on a %d-byte disk, is %+v", spec.Root.Size, booted)
	}
	// Told the size it has, nothing changes.
	if size, err := h.Hypervisor.ResizeRoot(ctx, "m1"); err != nil || size != spec.Root.Size {
		t.Errorf("resized to the size it has: %d bytes, %v; want %d", size, err, spec.Root.Size)
	}
	must(t, h.Guest.GrowRoot(ctx, "m1"))

	grown, err := h.Storage.EnsureRoot(ctx, "m1", spec.Root.Digest, 2*spec.Root.Size)
	must(t, err)
	for range 2 {
		size, err := h.Hypervisor.ResizeRoot(ctx, "m1")
		if err != nil || size != grown.Size {
			t.Errorf("resized the grown root to %d bytes, %v; want %d", size, err, grown.Size)
		}
		must(t, h.Guest.GrowRoot(ctx, "m1"))
	}
	var after driver.FSUsage
	testutil.EventuallyWithin(t, vmWait, "the guest's root grown", func() (bool, string) {
		after, err = h.Guest.RootUsage(ctx, "m1")
		return err == nil && after.Size > uint64(spec.Root.Size), fmt.Sprintf("%+v %v, was %+v on %d bytes", after, err, booted, spec.Root.Size)
	})
	if after.Size > uint64(grown.Size) || after.Used > after.Size {
		t.Errorf("the guest's root, on a %d-byte disk, is %+v", grown.Size, after)
	}
	vm, ok := vmOf(t, h, "m1")
	if !ok || vm.State != driver.VMRunning || !vm.Started.Equal(before.Started) || vm.SpecHash != before.SpecHash {
		t.Errorf("the VM grown is %+v; it was %+v", vm, before)
	}
	now := spec
	now.Root = grown
	if !now.Runs(vm) {
		t.Error("the VM does not run its spec with its root grown: the agent would restart it")
	}
}
