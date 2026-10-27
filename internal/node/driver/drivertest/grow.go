package drivertest

import (
	"errors"
	"slices"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// growDisks: a running VM's root disk and volume, grown by the storage,
// grow in the VM, its boot and hash kept, and it is listed at its new
// sizes, grown, running the grown spec; its guest grows its filesystems.
// Nothing bigger grows nothing; a size the device does not have is
// refused; a stopped VM's disks grow when it next starts.
func growDisks(t *testing.T, h Host) {
	ctx := t.Context()
	spec := vmSpec(t, h, "m1", driver.RestartAlways)
	vol, err := h.Storage.EnsureVolume(ctx, "d1", rootSize, "")
	must(t, err)
	spec.Disks = []driver.Volume{vol}
	must(t, h.Hypervisor.Start(ctx, spec))
	t.Cleanup(func() { _ = h.Hypervisor.Stop(ctx, "m1") })
	testutil.EventuallyWithin(t, vmWait, "the guest agent answering", func() bool { return h.Guest.Ready(ctx, "m1") == nil })
	before, _ := vmOf(t, h, "m1")
	if !slices.Equal(before.DiskSizes, spec.DiskSizes()) || before.Grown {
		t.Errorf("a VM started lists its disks at %v, grown %v; want %v", before.DiskSizes, before.Grown, spec.DiskSizes())
	}
	if grown, err := h.Hypervisor.GrowDisks(ctx, spec); grown || err != nil {
		t.Errorf("growing a VM's disks to the sizes it has: %v, %v", grown, err)
	}

	root, err := h.Storage.EnsureRoot(ctx, "m1", spec.Root.Digest, 2*rootSize)
	must(t, err)
	vol, err = h.Storage.EnsureVolume(ctx, "d1", 2*rootSize, "")
	must(t, err)
	big := spec
	big.Root, big.Disks = root, []driver.Volume{vol}
	// A size the devices do not have is refused, as Cloud Hypervisor
	// refuses one of a block device.
	wrong, _ := big.WithDiskSizes([]int64{root.Size + 1<<20, vol.Size})
	if _, err := h.Hypervisor.GrowDisks(ctx, wrong); err == nil || errors.Is(err, driver.ErrGrowsAtStart) {
		t.Errorf("growing a disk past its device: %v", err)
	}
	grown, err := h.Hypervisor.GrowDisks(ctx, big)
	if !grown || err != nil {
		t.Fatalf("growing a running VM's disks: %v, %v", grown, err)
	}
	vm, ok := vmOf(t, h, "m1")
	switch {
	case !ok || vm.State != driver.VMRunning:
		t.Fatalf("the VM after growing: %+v %t", vm, ok)
	case vm.SpecHash != before.SpecHash || !vm.Started.Equal(before.Started):
		t.Error("growing its disks restarted the VM, or changed its hash")
	case !slices.Equal(vm.DiskSizes, big.DiskSizes()) || !vm.Grown:
		t.Errorf("the VM lists its disks at %v, grown %v; want %v, grown", vm.DiskSizes, vm.Grown, big.DiskSizes())
	case !big.Runs(vm):
		t.Error("the VM does not run its grown spec")
	}
	if err := h.Guest.GrowFilesystems(ctx, "m1"); err != nil {
		t.Errorf("the guest growing its filesystems: %v", err)
	}
	if grown, err := h.Hypervisor.GrowDisks(ctx, big); grown || err != nil {
		t.Errorf("growing a VM's disks again: %v, %v", grown, err)
	}
	if grown, err := h.Hypervisor.GrowDisks(ctx, spec); grown || err != nil {
		t.Errorf("growing a VM's disks to less than they have: %v, %v", grown, err)
	}

	must(t, h.Hypervisor.Stop(ctx, "m1"))
	root, err = h.Storage.EnsureRoot(ctx, "m1", spec.Root.Digest, 3*rootSize)
	must(t, err)
	bigger := big
	bigger.Root = root
	if _, err := h.Hypervisor.GrowDisks(ctx, bigger); !errors.Is(err, driver.ErrGrowsAtStart) {
		t.Errorf("growing a stopped VM's disks: %v, want ErrGrowsAtStart", err)
	}
}
