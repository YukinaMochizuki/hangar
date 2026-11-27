package chvm

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// TestAVMStartsHeldToItsCPULimit: a VM started with a CPU limit of its own
// has its unit held to it from the start, each cpuQuotaPeriod, as a pool's
// slice is; one started with none has no quota; the limit is no part of
// its hash.
func TestAVMStartsHeldToItsCPULimit(t *testing.T) {
	s := newFakeSystemd()
	h := newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) { c.cgroupRoot = t.TempDir() })
	ctx := t.Context()
	held := testSpec("held", driver.RestartAlways)
	held.CPULimit = 400
	free := testSpec("free", driver.RestartAlways)
	for _, spec := range []driver.VMSpec{held, free} {
		if err := h.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
	}
	props := s.propsOf(unitName("held"))
	if got := prop(props, "CPUQuotaPerSecUSec"); got != uint64(400_000) {
		t.Errorf("started held to 400 millicores, CPUQuotaPerSecUSec %v, want 0.4 s a second", got)
	}
	if got := prop(props, "CPUQuotaPeriodUSec"); got != usec(cpuQuotaPeriod) {
		t.Errorf("CPUQuotaPeriodUSec %v, want %d", got, usec(cpuQuotaPeriod))
	}
	if got := prop(s.propsOf(unitName("free")), "CPUQuotaPerSecUSec"); got != nil {
		t.Errorf("started with no limit, CPUQuotaPerSecUSec %v", got)
	}
	unheld := held
	unheld.CPULimit = 0
	if held.Hash() != unheld.Hash() {
		t.Error("a VM's CPU limit changes its hash")
	}
}

// TestSetCPULimitHoldsAndLiftsTheUnit: a running VM's unit is held to the
// limit asked, or lifted for zero, where its cpu.max differs, and its
// memory.oom.group is set again with it; a cpu.max that is already what is
// asked is left alone; a machine whose unit has no cgroup fails.
func TestSetCPULimitHoldsAndLiftsTheUnit(t *testing.T) {
	s := newFakeSystemd()
	root := t.TempDir()
	h := newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) { c.cgroupRoot = root })
	ctx := t.Context()
	if err := h.Start(ctx, testSpec("m1", driver.RestartAlways)); err != nil {
		t.Fatal(err)
	}
	if err := h.SetCPULimit(ctx, "m1", 400); err == nil {
		t.Error("held a VM with no cgroup")
	}
	cg := filepath.Join(root, machinesSlice, unitName("m1"))
	if err := os.MkdirAll(cg, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cg, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cpu.max", "max 100000\n")
	write("memory.oom.group", "0\n")
	if err := h.SetCPULimit(ctx, "m1", 400); err != nil {
		t.Fatal(err)
	}
	if got := prop(s.propsOf(unitName("m1")), "CPUQuotaPerSecUSec"); got != uint64(400_000) {
		t.Errorf("held, CPUQuotaPerSecUSec %v, want 0.4 s a second", got)
	}
	if got := prop(s.propsOf(unitName("m1")), "CPUQuotaPeriodUSec"); got != usec(cpuQuotaPeriod) {
		t.Errorf("held, CPUQuotaPeriodUSec %v, want %d", got, usec(cpuQuotaPeriod))
	}
	if b, _ := os.ReadFile(filepath.Join(cg, "memory.oom.group")); string(b) != "1" {
		t.Errorf("memory.oom.group is %q, want 1", b)
	}

	// As systemd writes it: held already, nothing is set.
	write("cpu.max", "8000 20000\n")
	write("memory.oom.group", "0\n")
	if err := h.SetCPULimit(ctx, "m1", 400); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(cg, "memory.oom.group")); string(b) != "0\n" {
		t.Errorf("held already, the unit was set again: memory.oom.group %q", b)
	}

	if err := h.SetCPULimit(ctx, "m1", 0); err != nil {
		t.Fatal(err)
	}
	if got := prop(s.propsOf(unitName("m1")), "CPUQuotaPerSecUSec"); got != uint64(math.MaxUint64) {
		t.Errorf("lifted, CPUQuotaPerSecUSec %v, want infinity", got)
	}
	if err := h.SetCPULimit(ctx, "nobody", 400); err == nil {
		t.Error("held a machine with no VM")
	}
}
