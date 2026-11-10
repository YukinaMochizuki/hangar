package drivertest

import (
	"errors"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// usage reads what a running container's VM has used, as the host counts
// it, and what its guest says of its memory: the VM listed with its vCPUs,
// its unit's CPU time never going back, the memory it holds, and the
// guest's memory no more than the VM has. A machine with no running VM has
// no usage.
func usage(t *testing.T, h Host) {
	ctx := t.Context()
	if u, err := h.Hypervisor.Usage(ctx, "m1"); err == nil {
		t.Errorf("a machine with no VM used %+v", u)
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
	vm, ok := vmOf(t, h, "m1")
	if !ok || vm.VCPUs != spec.VCPUs || vm.Firmware || vm.FileShareServer {
		t.Errorf("a container's VM, of %d vCPUs, is listed as %+v (%t)", spec.VCPUs, vm, ok)
	}
	first, err := h.Hypervisor.Usage(ctx, "m1")
	must(t, err)
	// A guest that booted has used some CPU, and holds some memory.
	if first.CPU <= 0 || first.Memory == 0 {
		t.Errorf("a booted VM used %+v", first)
	}
	then, err := h.Hypervisor.Usage(ctx, "m1")
	must(t, err)
	if then.CPU < first.CPU {
		t.Errorf("the VM's CPU time went back from %v to %v", first.CPU, then.CPU)
	}
	mem, err := h.Guest.MemoryUsage(ctx, "m1")
	if errors.Is(err, driver.ErrGuestCannot) {
		t.Fatalf("a guest booted now cannot say its memory: %v", err)
	}
	must(t, err)
	if mem.Total == 0 || mem.Total > uint64(spec.MemoryMiB)<<20 || mem.Available > mem.Total {
		t.Errorf("a guest of %d MiB says its memory is %+v", spec.MemoryMiB, mem)
	}
	// A container's guest serves no share to list.
	if _, err := h.Guest.ListDir(ctx, "m1", driver.ShareListDir{}); !errors.Is(err, driver.ErrGuestCannot) {
		t.Errorf("listing a share of a container's guest: %v, want ErrGuestCannot", err)
	}

	must(t, h.Hypervisor.Stop(ctx, "m1"))
	if u, err := h.Hypervisor.Usage(ctx, "m1"); err == nil {
		t.Errorf("a stopped VM used %+v", u)
	}
}
