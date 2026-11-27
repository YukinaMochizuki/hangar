package drivertest

import (
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// cpuPools runs a VM of a CPU pool alone: no memory pool's VM, its pool
// read among the CPU pools; its pool's limit set, lifted and set again
// while it runs, without restarting it. Stopped, its pool is read no more.
func cpuPools(t *testing.T, h Host) {
	ctx := t.Context()
	hv := h.Hypervisor
	pooled := vmSpec(t, h, "c1", driver.RestartAlways)
	pooled.CPUPool = &driver.CPUPool{Name: "conform-cpu", MilliCPU: 500}
	t.Cleanup(func() {
		_ = hv.Stop(ctx, "c1")
		_ = hv.DeleteLogs(ctx, "c1")
		_ = h.Network.DeleteInterfaces(ctx, "c1")
		_ = h.Storage.DeleteRoot(ctx, "c1")
		_ = hv.SweepMemoryPools(ctx)
	})
	// A pool no VM runs in is left alone.
	must(t, hv.SetCPUPools(ctx, map[string]int64{"conform-cpu": 1000}, nil))
	must(t, hv.Start(ctx, pooled))
	testutil.EventuallyWithin(t, vmWait, "c1's guest agent answering", func() bool { return h.Guest.Ready(ctx, "c1") == nil })
	vm, _ := vmOf(t, h, "c1")

	in, err := hv.PoolVMs(ctx)
	must(t, err)
	for _, v := range in {
		if v.MachineUID == "c1" {
			t.Errorf("a VM of a CPU pool alone is listed among the memory pools' VMs: %+v", v)
		}
	}
	stats, err := hv.CPUPoolStats(ctx)
	must(t, err)
	if _, ok := stats["conform-cpu"]; !ok {
		t.Errorf("the CPU pools read are %+v, want conform-cpu among them", stats)
	}

	read := map[string]bool{"c1": true}
	must(t, hv.SetCPUPools(ctx, map[string]int64{"conform-cpu": 1000}, read))
	must(t, hv.SetCPUPools(ctx, nil, read))
	must(t, hv.SetCPUPools(ctx, nil, nil))
	if again, ok := vmOf(t, h, "c1"); !ok || again.State != driver.VMRunning || !again.Started.Equal(vm.Started) || again.SpecHash != vm.SpecHash {
		t.Errorf("its pool's limit changed, the VM is %+v (%t), want it running on as %+v", again, ok, vm)
	}

	must(t, hv.Stop(ctx, "c1"))
	testutil.EventuallyWithin(t, vmWait, "the pool's VM gone from its pool", func() bool {
		stats, err := hv.CPUPoolStats(ctx)
		_, ok := stats["conform-cpu"]
		return err == nil && !ok
	})
}
