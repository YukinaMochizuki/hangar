package drivertest

import (
	"errors"
	"fmt"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// volumeGrowth grows a running container's volume without starting it
// again, as rootGrowth does its root: the volume grown, the VM told its
// size, the guest grows the filesystem it mounts read-write from it, each
// twice to no harm, and the VM runs on as the spec with the volume grown.
// The guest answers of no volume it mounts read-only alone.
func volumeGrowth(t *testing.T, h Host) {
	ctx := t.Context()
	spec := vmSpec(t, h, "m1", driver.RestartAlways)
	data, err := h.Storage.EnsureVolume(ctx, "d1", rootSize, "")
	must(t, err)
	logs, err := h.Storage.EnsureVolume(ctx, "d2", rootSize, "")
	must(t, err)
	spec.Disks = []driver.Volume{data, logs}
	spec.Mounts = []driver.Mount{{Disk: 0, Name: "d1", Target: "/data"}, {Disk: 1, Name: "d2", Target: "/logs", ReadOnly: true}}
	t.Cleanup(func() {
		_ = h.Hypervisor.Stop(ctx, "m1")
		_ = h.Hypervisor.DeleteLogs(ctx, "m1")
		_ = h.Network.DeleteInterfaces(ctx, "m1")
		_ = h.Storage.DeleteRoot(ctx, "m1")
		_ = h.Storage.DeleteVolume(ctx, "d1")
		_ = h.Storage.DeleteVolume(ctx, "d2")
	})
	must(t, h.Hypervisor.Start(ctx, spec))
	testutil.EventuallyWithin(t, vmWait, "the guest agent answering", func() bool { return h.Guest.Ready(ctx, "m1") == nil })
	before, _ := vmOf(t, h, "m1")
	if len(before.DiskSizes) != 2 || before.DiskSizes[0] != data.Size || before.DiskSizes[1] != logs.Size {
		t.Errorf("the VM lists its volumes as started at %v; they were %d and %d", before.DiskSizes, data.Size, logs.Size)
	}
	const vdc, vdd = "/dev/vdc", "/dev/vdd"
	booted, err := h.Guest.VolumeUsage(ctx, "m1", vdc)
	must(t, err)
	if booted.Size == 0 || booted.Size > uint64(data.Size) || booted.Used > booted.Size {
		t.Fatalf("the guest's volume, on a %d-byte disk, is %+v", data.Size, booted)
	}
	for _, dev := range []string{vdd, "/dev/vda", "/dev/vde"} {
		if _, err := h.Guest.VolumeUsage(ctx, "m1", dev); err == nil || errors.Is(err, driver.ErrGuestCannot) {
			t.Errorf("the guest's volume usage of %s, no volume it mounts read-write: %v, want an error", dev, err)
		}
		if err := h.Guest.GrowVolume(ctx, "m1", dev); err == nil || errors.Is(err, driver.ErrGuestCannot) {
			t.Errorf("the guest growing %s, no volume it mounts read-write: %v, want an error", dev, err)
		}
	}
	// Told the size it has, nothing changes.
	if size, err := h.Hypervisor.ResizeVolume(ctx, "m1", 0); err != nil || size != data.Size {
		t.Errorf("resized to the size it has: %d bytes, %v; want %d", size, err, data.Size)
	}
	must(t, h.Guest.GrowVolume(ctx, "m1", vdc))
	if _, err := h.Hypervisor.ResizeVolume(ctx, "m1", 2); err == nil {
		t.Error("resized a volume the VM has not")
	}

	grown, err := h.Storage.EnsureVolume(ctx, "d1", 2*data.Size, "")
	must(t, err)
	for range 2 {
		size, err := h.Hypervisor.ResizeVolume(ctx, "m1", 0)
		if err != nil || size != grown.Size {
			t.Errorf("resized the grown volume to %d bytes, %v; want %d", size, err, grown.Size)
		}
		must(t, h.Guest.GrowVolume(ctx, "m1", vdc))
	}
	var after driver.FSUsage
	testutil.EventuallyWithin(t, vmWait, "the guest's volume grown", func() (bool, string) {
		after, err = h.Guest.VolumeUsage(ctx, "m1", vdc)
		return err == nil && after.Size > uint64(data.Size), fmt.Sprintf("%+v %v, was %+v on %d bytes", after, err, booted, data.Size)
	})
	if after.Size > uint64(grown.Size) || after.Used > after.Size {
		t.Errorf("the guest's volume, on a %d-byte disk, is %+v", grown.Size, after)
	}
	// A volume mounted read-only alone is told its size all the same.
	grownLogs, err := h.Storage.EnsureVolume(ctx, "d2", 2*logs.Size, "")
	must(t, err)
	if size, err := h.Hypervisor.ResizeVolume(ctx, "m1", 1); err != nil || size != grownLogs.Size {
		t.Errorf("resized the read-only volume to %d bytes, %v; want %d", size, err, grownLogs.Size)
	}
	vm, ok := vmOf(t, h, "m1")
	if !ok || vm.State != driver.VMRunning || !vm.Started.Equal(before.Started) || vm.SpecHash != before.SpecHash {
		t.Errorf("the VM grown is %+v; it was %+v", vm, before)
	}
	now := spec
	now.Disks = []driver.Volume{grown, grownLogs}
	if !now.Runs(vm) {
		t.Error("the VM does not run its spec with its volumes grown: the agent would restart it")
	}
}
