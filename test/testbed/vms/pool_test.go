//go:build testbed

package vms

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/squeeze"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// poolScript runs each file dropped in /dev/shm/q in the background, a
// child of the main process, so what it starts outlives the exec that
// dropped it.
const poolScript = `mkdir -p /dev/shm/q /dev/shm/r
echo ready
while :; do
  for f in /dev/shm/q/*; do
    [ -e "$f" ] || continue
    mv "$f" /dev/shm/r/
    sh /dev/shm/r/"${f##*/}" &
  done
  sleep 0.2
done`

var poolCmds atomic.Int32

// inBackground runs cmd in the guest's background, under poolScript.
func inBackground(t *testing.T, uid, cmd string) {
	t.Helper()
	n := poolCmds.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, code, err := execIn(ctx, uid, "sh", "-c", fmt.Sprintf(`echo "$0" > /dev/shm/x.%d && mv /dev/shm/x.%d /dev/shm/q/%d`, n, n, n), cmd)
	if err != nil || code != 0 {
		t.Fatalf("starting %q in %s: %v, %d, %s", cmd, uid, err, code, out)
	}
}

// inGuest runs cmd in the guest within d and returns what it says.
func inGuest(t *testing.T, uid, cmd string, d time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	out, code, err := execIn(ctx, uid, "sh", "-c", cmd)
	if err != nil || code != 0 {
		t.Errorf("%q in %s: %v, %d, %s", cmd, uid, err, code, out)
	}
	return strings.TrimSpace(out)
}

// poolMachine is h.machine with memhog in its image, as /bin/memhog, and a
// root disk of rootGiB.
func poolMachine(t *testing.T, h *host, uid string, rootGiB int64) driver.VMSpec {
	t.Helper()
	ctx := context.Background()
	hog, err := os.ReadFile(vmtest.Artifacts().MemHog)
	if err != nil {
		t.Fatalf("%v (make e2e-host builds bin/mem-hog)", err)
	}
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	_ = w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "bin/memhog", Mode: 0o755, Size: int64(len(hog))})
	_, _ = w.Write(hog)
	_ = w.Close()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf.Bytes())), nil })
	if err != nil {
		t.Fatal(err)
	}
	h.published++
	img, err := mutate.AppendLayers(image(t, fmt.Sprintf("pool %s %d", uid, h.published)), layer)
	if err != nil {
		t.Fatal(err)
	}
	ref, digest := h.reg.Push(t, "app:"+uid, img)
	if err := h.storage.EnsureBase(ctx, digest, 0, func(dir string) error {
		return h.images.Unpack(ctx, ref, digest, dir, driver.Anonymous)
	}); err != nil {
		t.Fatal(err)
	}
	root, err := h.storage.EnsureRoot(ctx, uid, digest, rootGiB<<30)
	if err != nil {
		t.Fatal(err)
	}
	nw := testNet()
	if err := h.net.EnsureNetwork(ctx, nw); err != nil {
		t.Fatal(err)
	}
	n := hostCount.Add(1) // an address of its own
	nic, err := h.net.EnsureInterface(ctx, driver.Interface{
		MachineUID: uid, Name: "eth0", NetworkUID: nw.UID, MAC: fmt.Sprintf("02:00:00:00:%02x:01", n%256),
		Address: mustPrefix(fmt.Sprintf("10.0.0.%d/24", 2+n%250)), Gateway: mustAddr("10.0.0.1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return driver.VMSpec{
		MachineUID: uid, Machine: "testbed/" + uid, VCPUs: 1, MemoryMiB: 256,
		Root: root, Interfaces: []driver.Interface{nic}, Nameservers: []string{"1.1.1.1"},
		Run:     driver.RunSpec{Command: []string{"/bin/sh", "-c", poolScript}},
		Restart: driver.RestartNever,
	}
}

// poolWatch runs the squeeze on a pool as the agent does (squeeze.Run,
// every node.DefaultSqueezePoll and on pressure), and keeps what it saw.
type poolWatch struct {
	hv   driver.Hypervisor
	pool driver.MemoryPool
	q    *squeeze.Squeezer

	mu         sync.Mutex
	members    []squeeze.Member
	stages     map[squeeze.Stage]int
	maxBalloon map[string]int64
	polls, hot int
}

// setMembers has the next polls see members, as the agent's reconciles do.
func (w *poolWatch) setMembers(members ...squeeze.Member) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.members = members
}

func (w *poolWatch) run(ctx context.Context, log *slog.Logger) {
	squeeze.Run(ctx, clockwork.NewRealClock(), w.hv.MemoryEvents(), node.DefaultSqueezePoll, func(ctx context.Context, pressure bool) bool {
		step := int64(squeeze.StepMiB)
		if pressure {
			step = squeeze.PressureStepMiB
		}
		w.mu.Lock()
		members := slices.Clone(w.members)
		w.mu.Unlock()
		// When each boot started, as the agent reads it.
		if vms, err := w.hv.VMs(ctx); err == nil {
			for i := range members {
				for _, vm := range vms {
					if vm.MachineUID == members[i].UID {
						members[i].Started = vm.Started
					}
				}
			}
		}
		r, err := w.q.Poll(ctx, w.hv, log, time.Now(), w.pool.SizeMiB, members, step)
		if err != nil {
			log.Warn("polling the pool", "error", err)
			return true
		}
		// As the agent's: never under what the guests have now.
		var held int64
		for _, size := range r.Sizes {
			held += size
		}
		if err := w.hv.SetMemoryPool(ctx, w.pool.Name, max(w.pool.SizeMiB, held)); err != nil {
			log.Warn("setting the pool", "error", err)
		}
		w.mu.Lock()
		w.polls++
		if r.Hot {
			w.hot++
		}
		w.stages[r.Stage]++
		for uid, mib := range r.Balloons {
			w.maxBalloon[uid] = max(w.maxBalloon[uid], mib)
		}
		w.mu.Unlock()
		if r.Stage != squeeze.Hold {
			log.Info("squeezed", "stage", r.Stage.String(), "pressure", pressure, "balloons_mib", r.Balloons,
				"allowances_mib", r.Allowances, "needing", r.Needing, "pool_mib", readMiB(poolCgroup, "memory.current"))
		}
		return r.Hot
	})
}

func (w *poolWatch) seen() (map[squeeze.Stage]int, map[string]int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	stages := map[squeeze.Stage]int{}
	for k, v := range w.stages {
		stages[k] = v
	}
	balloons := map[string]int64{}
	for k, v := range w.maxBalloon {
		balloons[k] = v
	}
	return stages, balloons
}

const (
	poolName   = "tb-pool"
	poolCgroup = "/sys/fs/cgroup/hangar.slice/hangar-pool_tb_pool.slice"
)

// readMiB reads a cgroup file of one number, in MiB; -1 if it cannot.
func readMiB(dir, file string) int64 {
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return -1
	}
	return n >> 20
}

// cgroupEvents reads a cgroup's memory.events.
func cgroupEvents(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "memory.events"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if k, v, ok := strings.Cut(l, " "); ok {
			out[k], _ = strconv.ParseInt(v, 10, 64)
		}
	}
	return out
}

// guestKills counts the processes the guest's own OOM killer killed.
func guestKills(t *testing.T, uid string) int {
	t.Helper()
	n, _ := strconv.Atoi(inGuest(t, uid, "dmesg | grep -c 'Killed process' || true", 20*time.Second))
	return n
}

// mostLate is the most any "late N ms" line memhog tick wrote in out says.
func mostLate(out string) int {
	worst := -1
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "late "); ok {
			if n, err := strconv.Atoi(strings.TrimSuffix(v, " ms")); err == nil {
				worst = max(worst, n)
			}
		}
	}
	return worst
}

// balloonWait is how long hangar-init held the container's start for its
// boot balloon, as its console says, in its last boot.
func balloonWait(t *testing.T, uid string) string {
	t.Helper()
	out := "no wait"
	for _, r := range consoleOf(t, uid) {
		if s := string(r.Payload); r.Stream == console.Init && (strings.Contains(s, "for the guest's memory") || strings.Contains(s, "starting all the same")) {
			out = strings.TrimSpace(s)
		}
	}
	return out
}

// TestASharedContainerReadingAsItBootsIsNotKilled: two Shared containers
// of 4 GiB guaranteed 1 GiB whose processes write and read 3500 MiB at
// once as they start, beside a Dedicated one of 2 GiB in a pool of 5 GiB,
// three times over: their guests wait for their boot balloons, and neither
// is killed, nor any process of theirs, nor does the pool reach its
// slice's limit. How long each waited is logged.
func TestASharedContainerReadingAsItBootsIsNotKilled(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	pool := &driver.MemoryPool{Name: poolName, SizeMiB: 5120}
	const reader = `echo ready
dd if=/dev/zero of=/cache bs=1M count=3500 2>/dev/null && sync && echo 3 > /proc/sys/vm/drop_caches && cat /cache >/dev/null && echo read
while :; do sleep 1; done`
	shared := []string{"boot-a", "boot-c"}
	specs := map[string]driver.VMSpec{}
	for _, uid := range shared {
		spec := poolMachine(t, h, uid, 6)
		spec.VCPUs, spec.MemoryMiB, spec.MemoryPool, spec.GuaranteedMiB = 2, 4096, pool, 1024
		spec.Run.Command = []string{"/bin/sh", "-c", reader}
		specs[uid] = spec
	}
	d := poolMachine(t, h, "boot-d", 3)
	d.VCPUs, d.MemoryMiB, d.MemoryPool = 2, 2048, pool
	specs["boot-d"] = d
	w := &poolWatch{hv: h.hv, pool: *pool, q: squeeze.NewSqueezer(), stages: map[squeeze.Stage]int{}, maxBalloon: map[string]int64{}}
	w.setMembers(
		squeeze.Member{UID: "boot-a", MemoryMiB: 4096, GuaranteedMiB: 1024},
		squeeze.Member{UID: "boot-c", MemoryMiB: 4096, GuaranteedMiB: 1024},
		squeeze.Member{UID: "boot-d", MemoryMiB: 2048, GuaranteedMiB: 2048},
	)
	for run := range 3 {
		// The squeeze runs as the VMs start, as the agent's does.
		wctx, stop := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Go(func() { w.run(wctx, slog.New(slog.DiscardHandler)) })
		start := time.Now()
		for _, uid := range []string{"boot-d", "boot-a", "boot-c"} {
			if err := h.hv.Start(ctx, specs[uid]); err != nil {
				t.Fatal(err)
			}
		}
		for _, uid := range shared {
			for strings.Count(stdoutOf(t, uid), "read\n") <= run {
				if !unitActive(uid) || time.Since(start) > 5*time.Minute {
					dump(t, uid)
					t.Fatalf("run %d: %s never read its file (VM up: %t)", run, uid, unitActive(uid))
				}
				time.Sleep(500 * time.Millisecond)
			}
		}
		ev := cgroupEvents(t, poolCgroup)
		t.Logf("run %d: both read in %v; waits a: %s, c: %s; pool peak %d MiB; pool events %v", run, time.Since(start).Round(100*time.Millisecond),
			balloonWait(t, "boot-a"), balloonWait(t, "boot-c"), readMiB(poolCgroup, "memory.peak"), ev)
		if ev["max"] != 0 || ev["oom"] != 0 || ev["oom_group_kill"] != 0 {
			t.Errorf("run %d: the pool reached its slice's limit: %v", run, ev)
		}
		for _, uid := range shared {
			if n := guestKills(t, uid); n != 0 {
				t.Errorf("run %d: %s's guest killed %d processes", run, uid, n)
			}
		}
		stop()
		wg.Wait()
		for _, uid := range []string{"boot-a", "boot-c", "boot-d"} {
			if err := h.hv.Stop(ctx, uid); err != nil {
				t.Fatal(err)
			}
		}
		if err := h.hv.SweepMemoryPools(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// sizeOf is what the machine's guest has of its memory, in MiB, what its
// balloon leaves it; -1 if it cannot be read.
func sizeOf(h *host, uid string, memoryMiB int64) int64 {
	b, err := h.hv.Balloon(context.Background(), uid)
	if err != nil {
		return -1
	}
	return memoryMiB - int64(b.Actual>>20)
}

// guestOOMKills is what the guest's own OOM killer killed, as its kernel's
// log and as its balloon's statistics count it.
func guestOOMKills(t *testing.T, h *host, uid string) string {
	t.Helper()
	stats := "none"
	if b, err := h.hv.Balloon(context.Background(), uid); err == nil && b.HasOOMKills {
		stats = strconv.FormatUint(b.OOMKills, 10)
	}
	return fmt.Sprintf("%d in its log, %s in its statistics", guestKills(t, uid), stats)
}

// TestAMemoryPoolIsKeptWithinItsSize: three machines in a pool of 5 GiB,
// two Shared ones of 4 GiB guaranteed 1 GiB and a Dedicated one of 2 GiB,
// their memory twice the pool's, run in the pool's slice, its MemoryMax
// the pool and their units' overheads, weighed by their guarantees, the
// Shared ones the OOM killer's first, booting at their guarantees; the
// squeeze runs as the agent runs it. (a) The Shared ones write and read
// files at disk speed while the Dedicated one takes 1.5 GiB: no VM is
// killed, nor any guest's process, the pool never reaches its slice's
// limit, the Shared guests' sizes and the Dedicated memory never pass the
// pool's size, the Dedicated one is never squeezed and its vCPUs never
// wait more than 100 ms; the Shared ones' worst wait is logged, and is at
// most 5 s. Then one reads on while the other is idle: it is given the
// other's room past its floor within a minute, how fast logged. (c) With
// nothing read, nobody gives back what it has; with the others stopped,
// the one left is given all its memory back. (b) The others started again,
// one Shared one fills anonymous memory past the pool: the Dedicated one
// is never squeezed, and a kill, if any, is a Shared one's: its guest's,
// or the host's of its whole VM, recorded as an OOM kill. The pool's slice
// goes with its last VM.
func TestAMemoryPoolIsKeptWithinItsSize(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	pool := &driver.MemoryPool{Name: poolName, SizeMiB: 5120}
	sharedUIDs := []string{"pool-a", "pool-c"}
	const ded = "pool-d"
	all := append(slices.Clone(sharedUIDs), ded)
	specs := map[string]driver.VMSpec{}
	for _, uid := range sharedUIDs {
		spec := poolMachine(t, h, uid, 6)
		spec.VCPUs, spec.MemoryMiB, spec.MemoryPool, spec.GuaranteedMiB = 2, 4096, pool, 1024
		specs[uid] = spec
	}
	spec := poolMachine(t, h, ded, 3)
	spec.VCPUs, spec.MemoryMiB, spec.MemoryPool = 2, 2048, pool
	specs[ded] = spec
	for _, uid := range all {
		startAndWait(t, h, specs[uid])
	}
	// Each Shared container's process starts once its boot balloon is
	// inflated.
	for _, uid := range sharedUIDs {
		t.Logf("%s's process started with %d MiB, after %s", uid, sizeOf(h, uid, 4096), balloonWait(t, uid))
	}
	if w := balloonWait(t, ded); w != "no wait" {
		t.Errorf("the Dedicated one waited for a boot balloon: %s", w)
	}

	// How the units run.
	for uid, want := range map[string]struct{ weight, score string }{
		"pool-a": {"4", "1000"}, "pool-c": {"4", "1000"}, ded: {"8", "0"},
	} {
		unit := filepath.Join(poolCgroup, "hangar-vm-"+uid+".service")
		if got := readTrim(t, filepath.Join(unit, "cpu.weight")); got != want.weight {
			t.Errorf("%s's cpu.weight is %s, want %s", uid, got, want.weight)
		}
		if got := readTrim(t, filepath.Join(unit, "memory.oom.group")); got != "1" {
			t.Errorf("%s's memory.oom.group is %s, want 1", uid, got)
		}
		if got := readTrim(t, filepath.Join(unit, "memory.high")); got != "max" {
			t.Errorf("%s's memory.high is %s, want max", uid, got)
		}
		if got := readTrim(t, fmt.Sprintf("/proc/%d/oom_score_adj", chPID(t, uid))); got != want.score {
			t.Errorf("%s's Cloud Hypervisor's oom_score_adj is %s, want %s", uid, got, want.score)
		}
	}
	// 338, 338 and 296 MiB of overheads, 256 MiB and 2% of each guest's,
	// as the kernel keeps it, in whole pages.
	overheads := 2*int64(256<<20+(4096<<20)/50) + int64(256<<20+(2048<<20)/50)
	if got, want := readTrim(t, filepath.Join(poolCgroup, "memory.max")), fmt.Sprint((5120<<20+overheads)&^4095); got != want {
		t.Errorf("the pool's memory.max is %s, want %s: the pool and its units' overheads", got, want)
	}
	if got := readTrim(t, filepath.Join(poolCgroup, "cpu.weight")); got != "16" {
		t.Errorf("the pool's cpu.weight is %s, want 16, its VMs' together", got)
	}
	limitMiB := (5120<<20 + overheads) >> 20

	members := map[string]squeeze.Member{
		"pool-a": {UID: "pool-a", MemoryMiB: 4096, GuaranteedMiB: 1024},
		"pool-c": {UID: "pool-c", MemoryMiB: 4096, GuaranteedMiB: 1024},
		ded:      {UID: ded, MemoryMiB: 2048, GuaranteedMiB: 2048},
	}
	w := &poolWatch{hv: h.hv, pool: *pool, q: squeeze.NewSqueezer(), stages: map[squeeze.Stage]int{}, maxBalloon: map[string]int64{}}
	w.setMembers(members["pool-a"], members["pool-c"], members[ded])
	wctx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { w.run(wctx, slog.New(slog.NewTextHandler(testWriter{t}, nil))) })
	defer func() { stop(); wg.Wait() }()

	// What the Shared guests have and the Dedicated memory, at most, every
	// 200 ms while they read.
	var mostSum atomic.Int64
	sample, stopSample := context.WithCancel(ctx)
	var sampled sync.WaitGroup
	sampled.Go(func() {
		for sample.Err() == nil {
			a, c := sizeOf(h, "pool-a", 4096), sizeOf(h, "pool-c", 4096)
			if a >= 0 && c >= 0 {
				mostSum.Store(max(mostSum.Load(), a+c+2048))
			}
			time.Sleep(200 * time.Millisecond)
		}
	})

	// (a) Disk-speed page cache in the Shared ones, anonymous memory
	// within its guarantee in the Dedicated one.
	offsets := map[string]int{}
	killsBefore := map[string]string{}
	// A ticker on each vCPU.
	for _, uid := range all {
		for _, cpu := range []string{"0", "1"} {
			inBackground(t, uid, "taskset -c "+cpu+" memhog tick || memhog tick")
		}
		offsets[uid] = len(stdoutOf(t, uid))
		killsBefore[uid] = guestOOMKills(t, h, uid)
	}
	start := time.Now()
	inBackground(t, ded, "memhog 1500")
	var fills sync.WaitGroup
	for _, uid := range sharedUIDs {
		fills.Go(func() {
			inGuest(t, uid, "dd if=/dev/zero of=/cache bs=1M count=3500 2>/dev/null && sync && echo 3 > /proc/sys/vm/drop_caches && "+
				"cat /cache >/dev/null && cat /cache >/dev/null", 10*time.Minute)
		})
	}
	fills.Wait()
	took := time.Since(start)
	time.Sleep(2 * node.DefaultSqueezePoll)
	stages, balloons := w.seen()
	ev := cgroupEvents(t, poolCgroup)
	late := map[string]int{}
	kills := map[string]string{}
	for _, uid := range all {
		late[uid] = mostLate(stdoutOf(t, uid)[offsets[uid]:])
		kills[uid] = guestOOMKills(t, h, uid)
	}
	w.mu.Lock()
	polls, hot := w.polls, w.hot
	w.mu.Unlock()
	t.Logf("(a) the Shared ones wrote and read 3500 MiB twice in %v: pool peak %d MiB of %d, now %d; sizes with the Dedicated memory at most %d MiB of 5120, now a %d, c %d; %d polls, %d hot; stages %v; balloons at most %v MiB; pool events %v; ticks late at most %v ms; guests' OOM kills %v (before %v)",
		took.Round(time.Second), readMiB(poolCgroup, "memory.peak"), limitMiB, readMiB(poolCgroup, "memory.current"),
		mostSum.Load(), sizeOf(h, "pool-a", 4096), sizeOf(h, "pool-c", 4096), polls, hot, stages, balloons, ev, late, kills, killsBefore)
	if ev["max"] != 0 || ev["oom"] != 0 || ev["oom_kill"] != 0 {
		t.Errorf("(a) the pool reached its slice's limit: %v", ev)
	}
	if mostSum.Load() > 5120 {
		t.Errorf("(a) the Shared guests' sizes and the Dedicated memory reached %d MiB, past the pool's 5120", mostSum.Load())
	}
	for _, uid := range all {
		if !unitActive(uid) {
			t.Fatalf("(a) %s's VM is gone", uid)
		}
		if kills[uid] != killsBefore[uid] {
			t.Errorf("(a) %s's guest killed: %s, before %s", uid, kills[uid], killsBefore[uid])
		}
	}
	if !strings.Contains(stdoutOf(t, ded), "held 1500") {
		t.Errorf("(a) the Dedicated one never held its 1500 MiB")
	}
	if late[ded] < 0 || late[ded] > 100 {
		t.Errorf("(a) the Dedicated one's vCPUs waited up to %d ms, want at most 100", late[ded])
	}
	for _, uid := range sharedUIDs {
		if late[uid] < 0 || late[uid] > 5000 {
			t.Errorf("(a) %s's vCPUs waited up to %d ms, want at most 5 s", uid, late[uid])
		}
	}
	if balloons[ded] != 0 {
		t.Errorf("(a) the Dedicated machine was squeezed to %d MiB", balloons[ded])
	}

	// One reads on, the other idle: the reader is given the other's room
	// past its guarantee, 2048 of the 3072 the Dedicated memory leaves.
	from := sizeOf(h, "pool-a", 4096)
	inBackground(t, "pool-a", "while [ ! -e /dev/shm/stop ]; do cat /cache >/dev/null; done")
	start = time.Now()
	var at5s int64 = -1
	grown := time.Duration(0)
	for time.Since(start) < time.Minute {
		a := sizeOf(h, "pool-a", 4096)
		if at5s < 0 && time.Since(start) >= 5*time.Second {
			at5s = a
		}
		if a >= 2048-squeeze.DeadbandMiB {
			grown = time.Since(start)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	inGuest(t, "pool-a", "touch /dev/shm/stop", time.Minute)
	stopSample()
	sampled.Wait()
	t.Logf("(a) one reading, the other idle: from %d MiB, %d after 5 s, %d MiB reached in %v (0: not in a minute); now a %d, c %d; sizes with the Dedicated memory at most %d MiB",
		from, at5s, 2048-squeeze.DeadbandMiB, grown.Round(100*time.Millisecond), sizeOf(h, "pool-a", 4096), sizeOf(h, "pool-c", 4096), mostSum.Load())
	if grown == 0 {
		t.Errorf("(a) the reader never had the idle one's room: %d MiB after a minute", sizeOf(h, "pool-a", 4096))
	}
	if mostSum.Load() > 5120 {
		t.Errorf("(a) the Shared guests' sizes and the Dedicated memory reached %d MiB, past the pool's 5120", mostSum.Load())
	}

	// (c) Nothing read: nobody gives back what it has. The others gone:
	// the one left has all its memory back.
	for _, uid := range sharedUIDs {
		inGuest(t, uid, "rm -f /cache && sync && echo 3 > /proc/sys/vm/drop_caches", time.Minute)
	}
	time.Sleep(3 * node.DefaultSqueezePoll)
	a, c := sizeOf(h, "pool-a", 4096), sizeOf(h, "pool-c", 4096)
	t.Logf("(c) idle: a %d, c %d MiB", a, c)
	if a+c < 3072-2*squeeze.DeadbandMiB {
		t.Errorf("(c) idle, the Shared guests have %d and %d MiB, less than the 3072 the pool has for them", a, c)
	}
	w.setMembers(members["pool-a"])
	for _, uid := range []string{"pool-c", ded} {
		if err := h.hv.Stop(ctx, uid); err != nil {
			t.Fatal(err)
		}
	}
	start = time.Now()
	for sizeOf(h, "pool-a", 4096) != 4096 {
		if time.Since(start) > 2*time.Minute {
			t.Fatalf("(c) alone in the pool, a has %d MiB", sizeOf(h, "pool-a", 4096))
		}
		time.Sleep(200 * time.Millisecond)
	}
	stages, _ = w.seen()
	t.Logf("(c) alone, a has all of its memory back in %v; stages %v", time.Since(start).Round(100*time.Millisecond), stages)

	// (b) The others back, which a makes room for, then anonymous memory
	// past the pool in a.
	for _, uid := range []string{"pool-c", ded} {
		startAndWait(t, h, specs[uid])
	}
	w.setMembers(members["pool-a"], members["pool-c"], members[ded])
	start = time.Now()
	for sizeOf(h, "pool-a", 4096)+sizeOf(h, "pool-c", 4096) > 3072 {
		if time.Since(start) > 2*time.Minute {
			t.Fatalf("(b) a %d and c %d MiB, past the 3072 the pool has for them", sizeOf(h, "pool-a", 4096), sizeOf(h, "pool-c", 4096))
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("(b) the others started, a made room in %v: a %d, c %d MiB", time.Since(start).Round(100*time.Millisecond), sizeOf(h, "pool-a", 4096), sizeOf(h, "pool-c", 4096))
	before := cgroupEvents(t, poolCgroup)
	dedKills := guestKills(t, ded)
	aKills := guestKills(t, "pool-a")
	inBackground(t, "pool-a", "memhog 3600")
	start = time.Now()
	deadline := start.Add(3 * time.Minute)
	outcome := ""
	for outcome == "" {
		switch {
		case !unitActive("pool-a"):
			outcome = "the Shared VM was killed whole"
		case guestKills(t, "pool-a") > aKills:
			outcome = "the Shared VM's guest killed a process of its own"
		case strings.Contains(stdoutOf(t, "pool-a"), "held 3600"):
			outcome = "the Shared VM held it"
		case time.Now().After(deadline):
			t.Fatalf("(b) nothing gave in 3 minutes: pool %d MiB", readMiB(poolCgroup, "memory.current"))
		}
		time.Sleep(500 * time.Millisecond)
	}
	time.Sleep(2 * node.DefaultSqueezePoll)
	stages, balloons = w.seen()
	ev = cgroupEvents(t, poolCgroup)
	t.Logf("(b) %s after %v; pool peak %d MiB of %d; stages %v; balloons at most %v MiB; pool events %v (before %v)",
		outcome, time.Since(start).Round(100*time.Millisecond), readMiB(poolCgroup, "memory.peak"), limitMiB, stages, balloons, ev, before)
	if !unitActive(ded) {
		t.Error("(b) the Dedicated VM was killed")
	}
	if n := guestKills(t, ded); n != dedKills {
		t.Errorf("(b) the Dedicated VM's guest killed %d processes", n-dedKills)
	}
	if balloons[ded] != 0 {
		t.Errorf("(b) the Dedicated machine was squeezed to %d MiB", balloons[ded])
	}
	for _, uid := range sharedUIDs {
		if unitActive(uid) {
			continue
		}
		vm, ok := vmOf(t, h, uid)
		if !ok || vm.State != driver.VMExited || !vm.OOMKilled {
			t.Errorf("(b) %s, killed whole, is listed %+v (%t), want exited, OOM-killed", uid, vm, ok)
		}
		if ev["oom_group_kill"] <= before["oom_group_kill"] {
			t.Errorf("(b) %s went with no OOM group kill counted: %v", uid, ev)
		}
		if n, err := h.hv.OOMGroupKills(ctx); err != nil || n == 0 {
			t.Errorf("(b) the driver counts %d OOM group kills (%v)", n, err)
		}
	}

	stop()
	wg.Wait()
	for _, uid := range all {
		if err := h.hv.Stop(ctx, uid); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.hv.SweepMemoryPools(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(poolCgroup); err == nil {
		t.Error("the pool's slice outlived its last VM and the sweep")
	}
}

// testWriter is t's log as an io.Writer.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}
