//go:build testbed

package vms

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// cpuStatOf reads a cgroup's cpu.stat, by key.
func cpuStatOf(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for line := range strings.Lines(readTrim(t, filepath.Join(dir, "cpu.stat"))) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			out[k], _ = strconv.ParseInt(v, 10, 64)
		}
	}
	return out
}

// TestACPUPoolHoldsItsVMsToItsCPUs: two containers of 2 vCPUs each, in a
// CPU pool of 1 CPU and no memory pool, run in the pool's slice, held by
// its cpu.max to 1 CPU together while all four vCPUs are busy, which the
// slice's cpu.stat says it held back; the pool grown to 2 CPUs while they
// run holds them to 2, restarting neither.
func TestACPUPoolHoldsItsVMsToItsCPUs(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	// Its slice goes with it, so the next test's pool starts afresh.
	t.Cleanup(func() { _ = h.hv.SweepMemoryPools(context.Background()) })
	uids := []string{"cpu-a", "cpu-b"}
	pids := map[string]string{}
	mainPID := func(uid string) string {
		out, _ := exec.Command("systemctl", "show", "-P", "MainPID", "hangar-vm-"+uid+".service").Output()
		return strings.TrimSpace(string(out))
	}
	for _, uid := range uids {
		spec := poolMachine(t, h, uid, 3)
		spec.VCPUs, spec.MemoryMiB = 2, 1024
		spec.CPUPool = &driver.CPUPool{Name: poolName, MilliCPU: 1000}
		startAndWait(t, h, spec)
		t.Cleanup(func() { _ = h.hv.Stop(context.Background(), uid) })
		if got, _ := exec.Command("systemctl", "show", "-P", "Slice", "hangar-vm-"+uid+".service").Output(); strings.TrimSpace(string(got)) != "hangar-pool_tb_pool.slice" {
			t.Errorf("%s is in %q, want the pool's slice", uid, strings.TrimSpace(string(got)))
		}
		pids[uid] = mainPID(uid)
	}
	if got := readTrim(t, filepath.Join(poolCgroup, "cpu.max")); got != "20000 20000" {
		t.Errorf("the pool's slice's cpu.max is %q, want 20000 20000: 1 CPU each 20 ms", got)
	}
	if got := readTrim(t, filepath.Join(poolCgroup, "memory.max")); got != "max" {
		t.Errorf("a CPU pool's slice alone has memory.max %q, want max", got)
	}
	for _, uid := range uids {
		inBackground(t, uid, "/bin/memhog burn 2")
	}
	// The burners take a moment to start in the guests; the window below
	// should see them all busy from its first period.
	time.Sleep(3 * time.Second)

	// cores is the CPUs the slice used, and the share of periods its limit
	// cut short, over a window.
	cores := func() (float64, float64) {
		t.Helper()
		before, start := cpuStatOf(t, poolCgroup), time.Now()
		time.Sleep(10 * time.Second)
		after, wall := cpuStatOf(t, poolCgroup), time.Since(start)
		used := float64(after["usage_usec"]-before["usage_usec"]) / float64(wall.Microseconds())
		periods := after["nr_periods"] - before["nr_periods"]
		if periods == 0 {
			return used, 0
		}
		return used, float64(after["nr_throttled"]-before["nr_throttled"]) / float64(periods)
	}
	used, throttled := cores()
	t.Logf("a pool of 1 CPU, 4 vCPUs busy: %.3f CPUs, %.0f%% of periods throttled", used, 100*throttled)
	if used < 0.9 || used > 1.1 {
		t.Errorf("a pool of 1 CPU used %.3f CPUs, want about 1", used)
	}
	if throttled < 0.5 {
		t.Errorf("a pool held to 1 CPU, its 4 vCPUs busy, was throttled in %.0f%% of periods, want most", 100*throttled)
	}
	stats, err := h.hv.CPUPoolStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st := stats[poolName]; st.Periods == 0 || st.Throttled == 0 {
		t.Errorf("the pool's stats %+v, want periods and throttled ones", st)
	}

	if err := h.hv.SetCPUPools(ctx, map[string]int64{poolName: 2000}, nil); err != nil {
		t.Fatal(err)
	}
	if got := readTrim(t, filepath.Join(poolCgroup, "cpu.max")); got != "40000 20000" {
		t.Errorf("grown, the pool's slice's cpu.max is %q, want 40000 20000", got)
	}
	// A period or two under the old limit would count in the window.
	time.Sleep(time.Second)
	if used, _ := cores(); used < 1.8 || used > 2.2 {
		t.Errorf("a pool grown to 2 CPUs used %.3f CPUs, want about 2", used)
	}
	for _, uid := range uids {
		if got := mainPID(uid); got != pids[uid] {
			t.Errorf("%s's launcher is %s, was %s: the pool changing restarted it", uid, got, pids[uid])
		}
	}
	if err := h.hv.SetCPUPools(ctx, nil, map[string]bool{"cpu-a": true, "cpu-b": true}); err != nil {
		t.Fatal(err)
	}
	if got := readTrim(t, filepath.Join(poolCgroup, "cpu.max")); !strings.HasPrefix(got, "max ") {
		t.Errorf("lifted, the pool's slice's cpu.max is %q, want max", got)
	}
}
