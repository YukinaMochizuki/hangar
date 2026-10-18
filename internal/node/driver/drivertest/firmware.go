package drivertest

import (
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// firmware: a firmware VM boots from its first volume, with no root disk,
// and is refused without a volume or with a root disk; it runs as any VM
// is listed, holding its volume; a Stop presses its power button, never
// asks a guest agent, and ends its boot in its log as the platform's,
// StoppedSignal, whether the guest powered off or was killed after its
// grace. The volume needs nothing bootable on it: the firmware runs on.
func firmware(t *testing.T, h Host) {
	ctx := t.Context()
	vol, err := h.Storage.EnsureVolume(ctx, "fw1", rootSize, "")
	must(t, err)
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	nic, err := h.Network.EnsureInterface(ctx, eth0("fw"))
	must(t, err)
	t.Cleanup(func() {
		_ = h.Hypervisor.Stop(ctx, "fw")
		_ = h.Hypervisor.DeleteLogs(ctx, "fw")
		_ = h.Network.DeleteInterfaces(ctx, "fw")
		_ = h.Storage.DeleteVolume(ctx, "fw1")
	})
	spec := driver.VMSpec{
		MachineUID: "fw", Machine: "test/fw", VCPUs: 1, MemoryMiB: 256,
		Disks: []driver.Volume{vol}, Firmware: true, Interfaces: []driver.Interface{nic},
		Restart: driver.RestartOnFailure,
	}
	for name, change := range map[string]func(*driver.VMSpec){
		"with a root disk": func(s *driver.VMSpec) { s.Root = driver.Disk{MachineUID: "fw", Path: vol.Path} },
		"with no volume":   func(s *driver.VMSpec) { s.Disks = nil },
		"with a mount":     func(s *driver.VMSpec) { s.Mounts = []driver.Mount{{Disk: 0, Name: "fw1", Target: "/data"}} },
	} {
		bad := spec
		change(&bad)
		if err := h.Hypervisor.Start(ctx, bad); err == nil {
			t.Errorf("started a firmware VM %s", name)
			must(t, h.Hypervisor.Stop(ctx, "fw"))
		}
	}

	must(t, h.Hypervisor.Start(ctx, spec))
	vm, ok := vmOf(t, h, "fw")
	if !ok || vm.State != driver.VMRunning || vm.SpecHash != spec.Hash() {
		t.Fatalf("firmware VM %+v listed %t", vm, ok)
	}
	must(t, h.Hypervisor.Start(ctx, spec))
	if again, _ := vmOf(t, h, "fw"); !again.Started.Equal(vm.Started) {
		t.Error("starting the same firmware spec again restarted the VM")
	}
	if err := h.Storage.DeleteVolume(ctx, "fw1"); err == nil {
		t.Error("deleted the volume a running firmware VM boots from")
	}
	// Running for a while, as a guest would before it is stopped.
	time.Sleep(time.Second) //nolint:forbidigo // stands for a guest running before it is stopped

	must(t, h.Hypervisor.Stop(ctx, "fw"))
	must(t, h.Hypervisor.Stop(ctx, "fw"))
	if vm, ok := vmOf(t, h, "fw"); ok {
		t.Errorf("a stopped firmware VM is listed: %+v", vm)
	}
	if h.AskedToStop(t, "fw") {
		t.Error("a firmware VM's stop asked a guest agent")
	}
	recs := readLog(t, h.Hypervisor, "fw", driver.LogOptions{Tail: driver.AllLines})
	if len(recs) == 0 || recs[len(recs)-1].Stream != console.Exit || recs[len(recs)-1].Boot < 1 {
		t.Fatalf("the firmware VM's log ends with %+v, want the end of a numbered boot", recs)
	}
	if end := recs[len(recs)-1].Exit; end.Signal != driver.StoppedSignal || (end.Code != 0 && end.Code != 253) {
		t.Errorf("the stopped boot ended with %+v, want StoppedSignal, powered off (0) or killed (253)", end)
	}
	must(t, h.Hypervisor.DeleteLogs(ctx, "fw"))
	must(t, h.Network.DeleteInterfaces(ctx, "fw"))
	must(t, h.Storage.DeleteVolume(ctx, "fw1"))
}
