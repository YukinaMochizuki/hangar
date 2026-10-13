//go:build testbed

package vms

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// fillScript fills the guest's /dev/shm, a tmpfs of half its memory or
// more, and says how much of its memory is used; then it waits.
const fillScript = `dd if=/dev/zero of=/dev/shm/fill bs=1M 2>/dev/null
echo filled $(du -m /dev/shm/fill | cut -f1) MiB, $(grep -E '^(MemTotal|MemFree):' /proc/meminfo | tr -s ' ' | tr '\n' ' ')
while :; do sleep 1 & wait $!; done`

// TestMemoryOverhead, with HANGAR_TESTBED_MEASURE_MEMORY=1, logs how much
// memory a VM's unit takes past its guest's, its guest's memory filled, for
// guests of 1, 4 and 16 GiB: what MemoryMax must leave room for. It needs
// a testbed with memory for the largest (24 GiB).
func TestMemoryOverhead(t *testing.T) {
	if os.Getenv("HANGAR_TESTBED_MEASURE_MEMORY") == "" {
		t.Skip("HANGAR_TESTBED_MEASURE_MEMORY is not set")
	}
	h := newHost(t)
	for _, mib := range []int64{1024, 4096, 16384} {
		uid := fmt.Sprintf("mem-%d", mib)
		spec := h.machine(t, uid, driver.RestartNever, fillScript)
		spec.MemoryMiB = mib
		if err := h.hv.Start(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Minute)
		for !strings.Contains(stdoutOf(t, uid), "filled") {
			if time.Now().After(deadline) || !unitActive(uid) {
				dump(t, uid)
				t.Fatalf("%s never filled its memory", uid)
			}
			time.Sleep(time.Second)
		}
		show := func(p string) uint64 {
			v, _ := exec.Command("systemctl", "show", "-P", p, "hangar-vm-"+uid+".service").Output()
			n, _ := strconv.ParseUint(strings.TrimSpace(string(v)), 10, 64)
			return n
		}
		peak, current, max := show("MemoryPeak"), show("MemoryCurrent"), show("MemoryMax")
		guest := uint64(mib) << 20
		t.Logf("guest %5d MiB, %s: MemoryPeak %d MiB (%d MiB past the guest's), MemoryCurrent %d MiB, MemoryMax %d MiB, TasksCurrent %d",
			mib, strings.TrimSpace(lastLine(stdoutOf(t, uid))), peak>>20, int64(peak>>20)-int64(guest>>20), current>>20, max>>20, show("TasksCurrent"))
		if err := h.hv.Stop(context.Background(), uid); err != nil {
			t.Fatal(err)
		}
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
