package chvm

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// cpuPooledSpec is a VM of 1 GiB in the CPU pool acme-corp of milliCPU,
// in no memory pool.
func cpuPooledSpec(uid string, milliCPU int64) driver.VMSpec {
	spec := testSpec(uid, driver.RestartAlways)
	spec.MemoryMiB = 1024
	spec.CPUPool = &driver.CPUPool{Name: "acme-corp", MilliCPU: milliCPU}
	return spec
}

// unit1G is all the unit of a VM of 1 GiB may hold: its guest's memory and
// its overhead.
const unit1G = 1024<<20 + overhead1G

// TestACPUPoolsVMsRunInItsSlice: a VM in a CPU pool alone runs in its
// tenant's pool's slice, held to the pool's CPUs each period, at its
// memory's weight, its memory held to no limit; it is no memory pool's VM.
func TestACPUPoolsVMsRunInItsSlice(t *testing.T) {
	s := newFakeSystemd()
	h := newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) { c.cgroupRoot = t.TempDir() })
	ctx := t.Context()
	for _, spec := range []driver.VMSpec{cpuPooledSpec("a", 2500), cpuPooledSpec("b", 2500)} {
		if err := h.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
		if got := prop(s.propsOf(unitName(spec.MachineUID)), "Slice"); got != acmeSlice {
			t.Errorf("%s runs in %v, want %s", spec.MachineUID, got, acmeSlice)
		}
	}
	slice := s.propsOf(acmeSlice)
	if got := prop(slice, "CPUQuotaPerSecUSec"); got != uint64(2_500_000) {
		t.Errorf("CPUQuotaPerSecUSec %v, want 2.5 s a second", got)
	}
	if got := prop(slice, "CPUQuotaPeriodUSec"); got != usec(cpuQuotaPeriod) {
		t.Errorf("CPUQuotaPeriodUSec %v, want %d", got, usec(cpuQuotaPeriod))
	}
	if got := prop(slice, "MemoryMax"); got != uint64(math.MaxUint64) {
		t.Errorf("MemoryMax %v, want none for a slice of a CPU pool alone", got)
	}
	if got := prop(slice, "CPUWeight"); got != uint64(8) {
		t.Errorf("CPUWeight %v, want 8, two 1 GiB VMs' weights", got)
	}
	if vms, err := h.PoolVMs(ctx); err != nil || len(vms) != 0 {
		t.Errorf("a CPU pool's VMs are listed as a memory pool's: %+v, %v", vms, err)
	}
	if err := checkSpec(func() driver.VMSpec { s := cpuPooledSpec("c", 0); return s }()); err == nil {
		t.Error("a VM in a CPU pool of no CPU passes")
	}
	both := pooledSpec("d", 0)
	both.CPUPool = &driver.CPUPool{Name: "other", MilliCPU: 1000}
	if err := checkSpec(both); err == nil {
		t.Error("a VM in two tenants' pools passes")
	}
}

// TestAVMOfACPUPoolAloneAddsAllItsMemoryToAMemoryPoolsSlice: in a slice a
// memory pool holds to a limit, a VM started for its CPU pool alone adds
// all its unit may hold, and the pool's limit is set with it counted so,
// so the memory pool's VMs are held to the pool's size as before.
func TestAVMOfACPUPoolAloneAddsAllItsMemoryToAMemoryPoolsSlice(t *testing.T) {
	s := newFakeSystemd()
	root := t.TempDir()
	h := newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) { c.cgroupRoot = root })
	ctx := t.Context()
	if err := h.Start(ctx, pooledSpec("m", 0)); err != nil {
		t.Fatal(err)
	}
	want := uint64(2048<<20 + overhead1G)
	if got := prop(s.propsOf(acmeSlice), "MemoryMax"); got != want {
		t.Fatalf("MemoryMax %v, want %d", got, want)
	}
	sliceDir := filepath.Join(root, machinesSlice, acmeSlice)
	if err := os.MkdirAll(sliceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sliceDir, "memory.max"), []byte(strconv.FormatUint(want&^4095, 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(ctx, cpuPooledSpec("c", 1000)); err != nil {
		t.Fatal(err)
	}
	if got := prop(s.propsOf(acmeSlice), "MemoryMax"); got != want&^4095+unit1G {
		t.Errorf("with a CPU pool's VM started, MemoryMax %v, want %d", got, want&^4095+unit1G)
	}
	if err := h.SetMemoryPool(ctx, "acme-corp", 2048); err != nil {
		t.Fatal(err)
	}
	if got, want := prop(s.propsOf(acmeSlice), "MemoryMax"), uint64(2048<<20+overhead1G+unit1G); got != want {
		t.Errorf("set again, MemoryMax %v, want %d: the pool's size, m's overhead, and all of c's unit", got, want)
	}
	if got := prop(s.propsOf(acmeSlice), "CPUQuotaPerSecUSec"); got != uint64(1_000_000) {
		t.Errorf("CPUQuotaPerSecUSec %v, want 1 s a second", got)
	}
}

// TestSetCPUPoolsHoldsEachPoolsSlice: each pool VMs run in is held to what
// it is given, and lifted where it is given none; what its cgroup has
// already is not set again; a pool no VM runs in gets no slice.
func TestSetCPUPoolsHoldsEachPoolsSlice(t *testing.T) {
	s := newFakeSystemd()
	root := t.TempDir()
	h := newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) { c.cgroupRoot = root })
	ctx := t.Context()
	mem := pooledSpec("m", 0)
	mem.MemoryPool.Name = "mem-only"
	for _, spec := range []driver.VMSpec{cpuPooledSpec("c", 2000), mem} {
		if err := h.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
	}
	memSlice := poolSlice("mem-only")
	if got := prop(s.propsOf(memSlice), "CPUQuotaPerSecUSec"); got != nil {
		t.Errorf("a memory pool's slice started with a CPU limit %v", got)
	}
	if err := h.SetCPUPools(ctx, map[string]int64{"acme-corp": 4000, "mem-only": 500, "nobody": 1000}, nil); err != nil {
		t.Fatal(err)
	}
	if got := prop(s.propsOf(acmeSlice), "CPUQuotaPerSecUSec"); got != uint64(4_000_000) {
		t.Errorf("acme-corp's CPUQuotaPerSecUSec %v, want 4 s a second", got)
	}
	if got := prop(s.propsOf(memSlice), "CPUQuotaPerSecUSec"); got != uint64(500_000) {
		t.Errorf("mem-only's CPUQuotaPerSecUSec %v, want 0.5 s a second", got)
	}
	if got := s.propsOf(poolSlice("nobody")); got != nil {
		t.Errorf("a pool no VM runs in got a slice: %v", got)
	}
	if got := prop(s.propsOf(memSlice), "MemoryMax"); got == nil {
		t.Error("setting a slice's CPU limit dropped its MemoryMax")
	}

	// The cgroup as the kernel then shows it: nothing to set.
	if err := os.MkdirAll(filepath.Join(root, machinesSlice, acmeSlice), 0o755); err != nil {
		t.Fatal(err)
	}
	period := strconv.FormatInt(cpuQuotaPeriod.Microseconds(), 10)
	writeCPUMax := func(v string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, machinesSlice, acmeSlice, "cpu.max"), []byte(v+" "+period+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeCPUMax(strconv.FormatInt((4 * cpuQuotaPeriod).Microseconds(), 10))
	s.props[acmeSlice] = nil
	if err := h.SetCPUPools(ctx, map[string]int64{"acme-corp": 4000, "mem-only": 500}, nil); err != nil || s.propsOf(acmeSlice) != nil {
		t.Errorf("set what the slice has: %v, %v", err, s.propsOf(acmeSlice))
	}
	// Given none, its machines read, a pool's limit is lifted.
	read := map[string]bool{"c": true, "m": true}
	if err := h.SetCPUPools(ctx, map[string]int64{"mem-only": 500}, read); err != nil {
		t.Fatal(err)
	}
	if got := prop(s.propsOf(acmeSlice), "CPUQuotaPerSecUSec"); got != uint64(math.MaxUint64) {
		t.Errorf("lifted, CPUQuotaPerSecUSec %v, want infinity", got)
	}
	writeCPUMax("max")
	s.props[acmeSlice] = nil
	if err := h.SetCPUPools(ctx, map[string]int64{"mem-only": 500}, read); err != nil || s.propsOf(acmeSlice) != nil {
		t.Errorf("lifted what is lifted already: %v, %v", err, s.propsOf(acmeSlice))
	}
}

// TestCPUPoolStatsAreTheSlicesCounts: a pool's slice's cpu.stat says how
// often its limit held it back.
func TestCPUPoolStatsAreTheSlicesCounts(t *testing.T) {
	s := newFakeSystemd()
	root := t.TempDir()
	h := newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) { c.cgroupRoot = root })
	ctx := t.Context()
	if err := h.Start(ctx, cpuPooledSpec("c", 2000)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, machinesSlice, acmeSlice)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := "usage_usec 9000000\nuser_usec 8000000\nsystem_usec 1000000\nnr_periods 600\nnr_throttled 150\nthrottled_usec 2500000\nnr_bursts 0\nburst_usec 0\n"
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := h.CPUPoolStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := driver.CPUPoolStat{Periods: 600, Throttled: 150, ThrottledTime: 2500 * time.Millisecond, Usage: 9 * time.Second}
	if len(got) != 1 || got["acme-corp"] != want {
		t.Errorf("stats %+v, want acme-corp's %+v", got, want)
	}
}

// TestAnUnreadVMKeepsItsCPUPoolAsLaunched: a pool none of the specs read
// gives a limit keeps the most its VMs not read yet were launched with, as
// when the agent starts again; once they are read, it follows them.
func TestAnUnreadVMKeepsItsCPUPoolAsLaunched(t *testing.T) {
	s := newFakeSystemd()
	h := newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) { c.cgroupRoot = t.TempDir() })
	ctx := t.Context()
	for _, spec := range []driver.VMSpec{cpuPooledSpec("a", 2000), cpuPooledSpec("b", 3000)} {
		if err := h.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		what string
		want map[string]int64
		read map[string]bool
		got  uint64
	}{
		{"none read", nil, nil, 3_000_000},
		{"a read, giving none", nil, map[string]bool{"a": true}, 3_000_000},
		{"both read, giving 1000", map[string]int64{"acme-corp": 1000}, map[string]bool{"a": true, "b": true}, 1_000_000},
		{"both read, giving none", nil, map[string]bool{"a": true, "b": true}, math.MaxUint64},
	} {
		if err := h.SetCPUPools(ctx, c.want, c.read); err != nil {
			t.Fatal(err)
		}
		if got := prop(s.propsOf(acmeSlice), "CPUQuotaPerSecUSec"); got != c.got {
			t.Errorf("%s: CPUQuotaPerSecUSec %v, want %d", c.what, got, c.got)
		}
	}
}

// TestAVMInBothPoolsOrNeither: a VM in its tenant's memory and CPU pools
// runs in the one slice, held to both, and a member of the memory pool; a
// VM in neither runs in machinesSlice, as before pools.
func TestAVMInBothPoolsOrNeither(t *testing.T) {
	s := newFakeSystemd()
	h := newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) { c.cgroupRoot = t.TempDir() })
	ctx := t.Context()
	both := pooledSpec("both", 0)
	both.CPUPool = &driver.CPUPool{Name: "acme-corp", MilliCPU: 1500}
	neither := testSpec("neither", driver.RestartAlways)
	for _, spec := range []driver.VMSpec{both, neither} {
		if err := h.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
	}
	if got := prop(s.propsOf(unitName("both")), "Slice"); got != acmeSlice {
		t.Errorf("both runs in %v, want %s", got, acmeSlice)
	}
	if got := prop(s.propsOf(unitName("neither")), "Slice"); got != machinesSlice {
		t.Errorf("neither runs in %v, want %s", got, machinesSlice)
	}
	slice := s.propsOf(acmeSlice)
	if got, want := prop(slice, "MemoryMax"), uint64(2048<<20+overhead1G); got != want {
		t.Errorf("MemoryMax %v, want %d: the memory pool's size and both's overhead", got, want)
	}
	if got := prop(slice, "CPUQuotaPerSecUSec"); got != uint64(1_500_000) {
		t.Errorf("CPUQuotaPerSecUSec %v, want 1.5 s a second", got)
	}
	if vms, err := h.PoolVMs(ctx); err != nil || len(vms) != 1 || vms[0].MachineUID != "both" {
		t.Errorf("the memory pools' VMs %+v (%v), want both", vms, err)
	}
	if got := prop(s.propsOf(machinesSlice), "CPUQuotaPerSecUSec"); got != nil {
		t.Errorf("machinesSlice was given a CPU limit %v", got)
	}
}

// TestAMemoryPoolsFirstVMInACPUPoolsSliceIsHeld: a slice made for a CPU
// pool alone holds no memory to a limit; the first VM of a memory pool to
// start in it holds the slice to the pool's size from the start, the CPU
// pool's VM's whole unit on top.
func TestAMemoryPoolsFirstVMInACPUPoolsSliceIsHeld(t *testing.T) {
	s := newFakeSystemd()
	root := t.TempDir()
	h := newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) { c.cgroupRoot = root })
	ctx := t.Context()
	if err := h.Start(ctx, cpuPooledSpec("c", 1000)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, machinesSlice, acmeSlice)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(ctx, pooledSpec("m", 0)); err != nil {
		t.Fatal(err)
	}
	if got, want := prop(s.propsOf(acmeSlice), "MemoryMax"), uint64(2048<<20+overhead1G+unit1G); got != want {
		t.Errorf("MemoryMax %v, want %d: the pool's size, m's overhead and all of c's unit", got, want)
	}
}
