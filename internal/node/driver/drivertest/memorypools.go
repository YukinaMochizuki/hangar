package drivertest

import (
	"slices"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// memoryPools runs a Shared VM of a memory pool beside one of none: the
// pool's VM listed as it was launched, booting with its guest at its
// guarantee, the rest in its balloon, which follows what it is set to and
// refuses more than the VM's memory; its share and its pool's limit
// changed while it runs without restarting it; the VM of no pool listed in
// none, its balloon empty. Stopped, its pool lists nothing and is swept.
// The host's OOM kill count and memory events are there to read.
func memoryPools(t *testing.T, h Host) {
	ctx := t.Context()
	hv := h.Hypervisor
	pool := driver.MemoryPool{Name: "conform", SizeMiB: 1024}
	pooled := vmSpec(t, h, "m1", driver.RestartAlways)
	pooled.MemoryMiB, pooled.MemoryPool, pooled.GuaranteedMiB = 512, &pool, 256
	alone := vmSpec(t, h, "m2", driver.RestartAlways)
	t.Cleanup(func() {
		for _, m := range []string{"m1", "m2"} {
			_ = hv.Stop(ctx, m)
			_ = hv.DeleteLogs(ctx, m)
			_ = h.Network.DeleteInterfaces(ctx, m)
			_ = h.Storage.DeleteRoot(ctx, m)
		}
		_ = hv.SweepMemoryPools(ctx)
	})
	if hv.MemoryEvents() == nil {
		t.Error("no memory events to wait on")
	}
	if _, err := hv.OOMGroupKills(ctx); err != nil {
		t.Errorf("counting OOM group kills: %v", err)
	}
	if err := hv.SetMemoryShare(ctx, "m1", &pool, 256); err == nil {
		t.Error("set the share of a machine with no VM")
	}
	if err := hv.SetMemoryPool(ctx, "Not A Name", 512); err == nil {
		t.Error("set the limit of a pool its name cannot be")
	}
	must(t, hv.Start(ctx, pooled))
	must(t, hv.Start(ctx, alone))
	for _, m := range []string{"m1", "m2"} {
		testutil.EventuallyWithin(t, vmWait, m+"'s guest agent answering", func() bool { return h.Guest.Ready(ctx, m) == nil })
	}

	vm, _ := vmOf(t, h, "m1")
	in, err := hv.PoolVMs(ctx)
	must(t, err)
	want := driver.PoolVM{MachineUID: "m1", Pool: pool, MemoryMiB: 512, GuaranteedMiB: 256}
	if len(in) != 1 || in[0].MachineUID != "m1" || in[0].Pool != pool || in[0].MemoryMiB != 512 || in[0].GuaranteedMiB != 256 ||
		in[0].Started.Sub(vm.Started).Abs() > time.Second {
		t.Errorf("the pools' VMs are %+v, want %+v started at %v", in, want, vm.Started)
	}
	balloonOf := func(m string) driver.Balloon {
		t.Helper()
		b, err := hv.Balloon(ctx, m)
		must(t, err)
		return b
	}
	near := func(b uint64, mib int64) bool { return int64(b>>20) >= mib-16 && int64(b>>20) <= mib+16 }
	testutil.EventuallyWithin(t, vmWait, "the pool's guest booted at its guarantee", func() bool { return near(balloonOf("m1").Actual, 256) })
	if b := balloonOf("m2"); b.Actual != 0 {
		t.Errorf("the VM of no pool booted with a balloon of %d", b.Actual)
	}

	must(t, hv.SetBalloon(ctx, "m1", 0))
	testutil.EventuallyWithin(t, vmWait, "the pool's balloon given back", func() bool { return near(balloonOf("m1").Actual, 0) })
	must(t, hv.SetBalloon(ctx, "m1", 128<<20))
	testutil.EventuallyWithin(t, vmWait, "the pool's balloon set again", func() bool { return near(balloonOf("m1").Actual, 128) })
	if err := hv.SetBalloon(ctx, "m1", 513<<20); err == nil {
		t.Error("set a balloon past its VM's memory")
	}

	// What follows a running VM changes, and it runs on.
	must(t, hv.SetMemoryShare(ctx, "m1", &driver.MemoryPool{Name: "conform", SizeMiB: 2048}, 384))
	must(t, hv.SetMemoryPool(ctx, "conform", 768))
	must(t, hv.SetMemoryPool(ctx, "conform", 0))
	if again, ok := vmOf(t, h, "m1"); !ok || again.State != driver.VMRunning || !again.Started.Equal(vm.Started) || again.SpecHash != vm.SpecHash {
		t.Errorf("its share and pool changed, the VM is %+v (%t), want it running on as %+v", again, ok, vm)
	}
	if in, err := hv.PoolVMs(ctx); err != nil || len(in) != 1 || in[0].Pool != pool || in[0].GuaranteedMiB != 256 {
		t.Errorf("its share changed, the pools' VMs are %+v (%v), want it as launched", in, err)
	}

	must(t, hv.Stop(ctx, "m1"))
	testutil.EventuallyWithin(t, vmWait, "the pool's VM gone from its pool", func() bool {
		in, err := hv.PoolVMs(ctx)
		return err == nil && !slices.ContainsFunc(in, func(v driver.PoolVM) bool { return v.MachineUID == "m1" })
	})
	must(t, hv.SweepMemoryPools(ctx))
}
