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
)

// unitCgroup is the cgroup of a VM's unit in no pool.
func unitCgroup(uid string) string {
	return filepath.Join("/sys/fs/cgroup/hangar.slice", "hangar-vm-"+uid+".service")
}

// unitCPULimit reads the millicores a VM's unit's cpu.max holds it to, zero
// for none.
func unitCPULimit(t *testing.T, uid string) int64 {
	t.Helper()
	quota, period, ok := strings.Cut(readTrim(t, filepath.Join(unitCgroup(uid), "cpu.max")), " ")
	if !ok || quota == "max" {
		return 0
	}
	q, err1 := strconv.ParseInt(quota, 10, 64)
	p, err2 := strconv.ParseInt(period, 10, 64)
	if err1 != nil || err2 != nil || p == 0 {
		t.Fatalf("cpu.max of %s is %q %q", uid, quota, period)
	}
	return q * 1000 / p
}

// TestABurstableVMIsHeldToItsBaseline: a container of 2 vCPUs started held
// to a burstable machine's 20% baseline, 0.4 CPUs, has a cpu.max of 8 ms
// each 20 ms and uses no more than that with both vCPUs busy; its limit
// lifted, it uses both; held again, 0.4 again; none of it restarts the VM.
func TestABurstableVMIsHeldToItsBaseline(t *testing.T) {
	h := newHost(t)
	const uid = "burst-a"
	spec := poolMachine(t, h, uid, 3)
	spec.VCPUs, spec.MemoryMiB, spec.CPULimit = 2, 1024, 400
	startAndWait(t, h, spec)
	t.Cleanup(func() { _ = h.hv.Stop(context.Background(), uid) })
	mainPID := func() string {
		out, _ := exec.Command("systemctl", "show", "-P", "MainPID", "hangar-vm-"+uid+".service").Output()
		return strings.TrimSpace(string(out))
	}
	pid := mainPID()
	if got := readTrim(t, filepath.Join(unitCgroup(uid), "cpu.max")); got != "8000 20000" {
		t.Errorf("started held, cpu.max is %q, want 8000 20000: 0.4 CPUs each 20 ms", got)
	}
	inBackground(t, uid, "/bin/memhog burn 2")
	// The burners take a moment to start in the guest.
	time.Sleep(3 * time.Second)

	// cores is the CPUs the unit used over a window.
	cores := func() float64 {
		t.Helper()
		before, start := cpuStatOf(t, unitCgroup(uid)), time.Now()
		time.Sleep(10 * time.Second)
		after, wall := cpuStatOf(t, unitCgroup(uid)), time.Since(start)
		return float64(after["usage_usec"]-before["usage_usec"]) / float64(wall.Microseconds())
	}
	if c := cores(); c < 0.36 || c > 0.44 {
		t.Errorf("held to 0.4 CPUs, it used %.3f", c)
	}
	if err := h.hv.SetCPULimit(context.Background(), uid, 0); err != nil {
		t.Fatal(err)
	}
	if got := unitCPULimit(t, uid); got != 0 {
		t.Errorf("lifted, it is held to %d millicores", got)
	}
	if c := cores(); c < 1.8 {
		t.Errorf("lifted, it used %.3f CPUs of its 2 busy vCPUs", c)
	}
	if err := h.hv.SetCPULimit(context.Background(), uid, 400); err != nil {
		t.Fatal(err)
	}
	if c := cores(); c < 0.36 || c > 0.44 {
		t.Errorf("held again to 0.4 CPUs, it used %.3f", c)
	}
	if got := mainPID(); got != pid {
		t.Errorf("its limit changing restarted it: MainPID %s, then %s", pid, got)
	}
}
