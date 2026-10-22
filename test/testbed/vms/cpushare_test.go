//go:build testbed

package vms

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
)

// TestTheVMsShareTheirSliceByMemory: a driver keeping one CPU for the host
// gives the VMs' slice, hangar.slice beside system.slice, its weight and
// the other CPUs; each VM's unit runs in it, weighted by its memory, on
// those CPUs alone. A driver keeping none gives the slice every CPU again.
func TestTheVMsShareTheirSliceByMemory(t *testing.T) {
	online := cpuList(t, "/sys/devices/system/cpu/online")
	if len(online) < 2 {
		t.Skip("one CPU: none to keep for the host")
	}
	cpus, err := chvm.MachineCPUs("/sys", 1)
	if err != nil {
		t.Fatal(err)
	}
	h := newHost(t)
	hv := openHypervisorOn(t, testGrace, cpus)
	t.Cleanup(func() { _ = hv.Close() })

	const slice = "/sys/fs/cgroup/hangar.slice"
	if got := readTrim(t, filepath.Join(slice, "cpu.weight")); got != "1000" {
		t.Errorf("hangar.slice's cpu.weight is %s, want 1000", got)
	}
	if got := cpuList(t, filepath.Join(slice, "cpuset.cpus.effective")); !slices.Equal(got, cpus) {
		t.Errorf("hangar.slice runs on CPUs %v, want %v", got, cpus)
	}

	for uid, want := range map[string]string{"share-small": "2", "share-large": "8"} {
		spec := h.machine(t, uid, driver.RestartNever, guestScript)
		spec.MemoryMiB = map[string]int64{"share-small": 512, "share-large": 2048}[uid]
		if err := hv.Start(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = hv.Stop(context.Background(), uid) })
		unit := "hangar-vm-" + uid + ".service"
		if got, _ := exec.Command("systemctl", "show", "-P", "Slice", unit).Output(); strings.TrimSpace(string(got)) != "hangar.slice" {
			t.Errorf("%s is in %q, want hangar.slice", unit, strings.TrimSpace(string(got)))
		}
		if got := readTrim(t, filepath.Join(slice, unit, "cpu.weight")); got != want {
			t.Errorf("%s of %d MiB has cpu.weight %s, want %s", unit, spec.MemoryMiB, got, want)
		}
		// The slice's cpuset binds the unit's processes; systemd enables the
		// controller no further down than the slice, so the unit has no
		// cpuset files of its own.
		pid, _ := exec.Command("systemctl", "show", "-P", "MainPID", unit).Output()
		if got := allowedCPUs(t, strings.TrimSpace(string(pid))); !slices.Equal(got, cpus) {
			t.Errorf("%s's launcher may run on CPUs %v, want %v", unit, got, cpus)
		}
	}

	// The agent started again with none kept back lets the slice go.
	again := openHypervisorOn(t, testGrace, nil)
	t.Cleanup(func() { _ = again.Close() })
	if got := cpuList(t, filepath.Join(slice, "cpuset.cpus.effective")); !slices.Equal(got, online) {
		t.Errorf("with no CPU kept, hangar.slice runs on CPUs %v, want every one, %v", got, online)
	}
}

// allowedCPUs reads the CPUs process pid may run on, from its status.
func allowedCPUs(t *testing.T, pid string) []int {
	t.Helper()
	status := readTrim(t, filepath.Join("/proc", pid, "status"))
	for line := range strings.Lines(status) {
		if v, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
			return parseCPUs(t, strings.TrimSpace(v))
		}
	}
	t.Fatalf("no Cpus_allowed_list in /proc/%s/status", pid)
	return nil
}

func readTrim(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// cpuList reads a file in the kernel's CPU list format, such as 0-3,8.
func cpuList(t *testing.T, path string) []int {
	t.Helper()
	return parseCPUs(t, readTrim(t, path))
}

// parseCPUs reads the kernel's CPU list format.
func parseCPUs(t *testing.T, list string) []int {
	t.Helper()
	var out []int
	for part := range strings.SplitSeq(list, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			t.Fatalf("%q is not a CPU list", list)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				t.Fatalf("%q is not a CPU list", list)
			}
		}
		for c := a; c <= b; c++ {
			out = append(out, c)
		}
	}
	return out
}
