//go:build testbed

package vms

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// freeScript fills 1 GiB of the guest's /dev/shm and frees it again, then
// reads a file of 256 MiB on its root disk into the page cache and drops
// the cache, saying when each step is done; then it waits.
const freeScript = `dd if=/dev/zero of=/dev/shm/fill bs=1M count=1024 2>/dev/null
echo filled
sleep 15
rm /dev/shm/fill
echo freed
sleep 45
dd if=/dev/zero of=/cache bs=1M count=256 2>/dev/null && sync && cat /cache >/dev/null
echo cached
sleep 15
echo 3 >/proc/sys/vm/drop_caches && echo dropped || echo not dropped
while :; do sleep 1 & wait $!; done`

// TestFreedMemoryReturns: memory a running guest frees goes back to the
// host. A 2 GiB guest fills 1 GiB of /dev/shm, then frees it, and its
// unit's memory must fall by most of that within its page reporting's
// delay. The page cache, dropped in the guest, is only logged: what a
// guest keeps cached stays in use.
func TestFreedMemoryReturns(t *testing.T) {
	h := newHost(t)
	const uid = "mem-free"
	spec := h.machine(t, uid, driver.RestartNever, freeScript)
	spec.MemoryMiB = 2048
	if err := h.hv.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.hv.Stop(context.Background(), uid) })
	current := func() int64 {
		v, _ := exec.Command("systemctl", "show", "-P", "MemoryCurrent", "hangar-vm-"+uid+".service").Output()
		n, _ := strconv.ParseInt(strings.TrimSpace(string(v)), 10, 64)
		return n >> 20
	}
	await := func(line string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Minute)
		for !strings.Contains(stdoutOf(t, uid), line+"\n") {
			if time.Now().After(deadline) || !unitActive(uid) {
				dump(t, uid)
				t.Fatalf("%s never said %q", uid, line)
			}
			time.Sleep(time.Second)
		}
	}
	// The lowest the unit's memory falls to within d.
	lowest := func(d time.Duration) int64 {
		low := current()
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(time.Second) {
			low = min(low, current())
		}
		return low
	}

	await("filled")
	time.Sleep(5 * time.Second)
	filled := current()
	await("freed")
	freed := lowest(30 * time.Second)
	t.Logf("1 GiB of /dev/shm: %d MiB filled, %d MiB within 30 s of freeing it", filled, freed)

	await("cached")
	cached := current()
	await("dropped")
	dropped := lowest(30 * time.Second)
	t.Logf("256 MiB of page cache: %d MiB cached, %d MiB within 30 s of dropping it", cached, dropped)

	if filled-freed < 768 {
		t.Errorf("freeing 1 GiB in the guest gave back %d MiB to the host, want 768 or more", filled-freed)
	}
}

// diskScript fills 1536 MiB of the guest's /dev/shm, guest memory its unit
// holds as Cloud Hypervisor's, writes 2 GiB to its root disk, and then
// reads it back again and again, dropping its own cache before each pass,
// so each pass reaches the zvol.
const diskScript = `dd if=/dev/zero of=/dev/shm/fill bs=1M count=1536 2>/dev/null
dd if=/dev/urandom of=/big bs=1M count=2048 2>/dev/null && sync
echo written
while :; do echo 3 >/proc/sys/vm/drop_caches; cat /big >/dev/null; done`

// TestAGuestsDiskStaysOutOfItsUnitsMemory: a guest reading and writing its
// root disk puts no host page cache in its unit, which never swaps nor
// reaches its MemoryMax (docs/decisions/node-agent.md). A 2 GiB guest with
// most of its memory in use reads 2 GiB from its disk over and over for a
// minute: its unit's page cache must stay under 64 MiB, its swap at 0 and
// its memory.events max where it was.
func TestAGuestsDiskStaysOutOfItsUnitsMemory(t *testing.T) {
	h := newHost(t)
	const uid = "mem-disk"
	ctx := context.Background()
	spec := h.machine(t, uid, driver.RestartNever, diskScript)
	root, err := h.storage.EnsureRoot(ctx, uid, spec.Root.Digest, 4<<30)
	if err != nil {
		t.Fatal(err)
	}
	spec.Root, spec.MemoryMiB, spec.VCPUs = root, 2048, 2
	if err := h.hv.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.hv.Stop(context.Background(), uid) })
	cg := "/sys/fs/cgroup/hangar.slice/hangar-vm-" + uid + ".service"
	read := func(f string) string {
		b, err := os.ReadFile(filepath.Join(cg, f))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	number := func(s string) uint64 {
		n, _ := strconv.ParseUint(s, 10, 64)
		return n
	}
	file := func() uint64 {
		for _, l := range strings.Split(read("memory.stat"), "\n") {
			if v, ok := strings.CutPrefix(l, "file "); ok {
				return number(v)
			}
		}
		t.Fatal("memory.stat has no file")
		return 0
	}
	if got := read("memory.swap.max"); got != "0" {
		t.Errorf("memory.swap.max is %s, want 0", got)
	}
	deadline := time.Now().Add(4 * time.Minute)
	for !strings.Contains(stdoutOf(t, uid), "written\n") {
		if time.Now().After(deadline) || !unitActive(uid) {
			dump(t, uid)
			t.Fatalf("%s never wrote its disk", uid)
		}
		time.Sleep(time.Second)
	}
	atMax := func() uint64 {
		for _, l := range strings.Split(read("memory.events"), "\n") {
			if v, ok := strings.CutPrefix(l, "max "); ok {
				return number(v)
			}
		}
		t.Fatal("memory.events has no max")
		return 0
	}
	before := atMax()
	var most, swap uint64
	for end := time.Now().Add(time.Minute); time.Now().Before(end); time.Sleep(5 * time.Second) {
		most, swap = max(most, file()), max(swap, number(read("memory.swap.current")))
	}
	limited := atMax() - before
	t.Logf("reading its disk for a minute: page cache at most %d MiB, swap at most %d MiB, at its limit %d times, memory %s of %s",
		most>>20, swap>>20, limited, read("memory.current"), read("memory.max"))
	if limited != 0 {
		t.Errorf("the unit reached its MemoryMax %d times, want none", limited)
	}
	if most >= 64<<20 {
		t.Errorf("the unit's page cache reached %d MiB, want under 64", most>>20)
	}
	if swap != 0 {
		t.Errorf("the unit swapped %d MiB of its memory, want none", swap>>20)
	}
	if !unitActive(uid) {
		dump(t, uid)
		t.Errorf("%s stopped", uid)
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
