package drivertest

import (
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// cpuLimits runs a VM started held to a CPU limit of its own, a
// burstable machine's baseline: the limit is no part of its hash, and is
// lifted and set again while it runs, without restarting it. A machine
// with no running VM takes no limit.
func cpuLimits(t *testing.T, h Host) {
	ctx := t.Context()
	hv := h.Hypervisor
	if err := hv.SetCPULimit(ctx, "c1", 500); err == nil {
		t.Error("a machine with no VM took a CPU limit")
	}
	spec := vmSpec(t, h, "c1", driver.RestartAlways)
	spec.CPULimit = 400
	t.Cleanup(func() {
		_ = hv.Stop(ctx, "c1")
		_ = hv.DeleteLogs(ctx, "c1")
		_ = h.Network.DeleteInterfaces(ctx, "c1")
		_ = h.Storage.DeleteRoot(ctx, "c1")
	})
	must(t, hv.Start(ctx, spec))
	testutil.EventuallyWithin(t, vmWait, "c1's guest agent answering", func() bool { return h.Guest.Ready(ctx, "c1") == nil })
	vm, _ := vmOf(t, h, "c1")
	unlimited := spec
	unlimited.CPULimit = 0
	if !unlimited.Runs(vm) {
		t.Errorf("a VM started held to a CPU limit does not run its spec without one: %+v", vm)
	}
	limit := func(want int64, when string) {
		t.Helper()
		if h.CPULimit == nil {
			return
		}
		if got := h.CPULimit(t, "c1"); got != want {
			t.Errorf("%s, the VM is held to %d millicores, want %d", when, got, want)
		}
	}
	limit(400, "started held")
	must(t, hv.SetCPULimit(ctx, "c1", 0))
	limit(0, "lifted")
	must(t, hv.SetCPULimit(ctx, "c1", 0))
	must(t, hv.SetCPULimit(ctx, "c1", 400))
	limit(400, "held again")
	if again, ok := vmOf(t, h, "c1"); !ok || again.State != driver.VMRunning || !again.Started.Equal(vm.Started) || again.SpecHash != vm.SpecHash {
		t.Errorf("its CPU limit changed, the VM is %+v (%t), want it running on as %+v", again, ok, vm)
	}
	must(t, hv.Stop(ctx, "c1"))
	if err := hv.SetCPULimit(ctx, "c1", 400); err == nil {
		t.Error("a stopped VM took a CPU limit")
	}
}
