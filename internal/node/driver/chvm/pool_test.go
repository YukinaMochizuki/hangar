package chvm

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	sd "github.com/coreos/go-systemd/v22/dbus"

	"github.com/YukinaMochizuki/hangar/internal/guest/bootdisk"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// pooledSpec is a VM of 1 GiB in the pool acme-corp of 2 GiB, guaranteed
// guaranteedMiB of its memory, zero for all of it.
func pooledSpec(uid string, guaranteedMiB int64) driver.VMSpec {
	spec := testSpec(uid, driver.RestartAlways)
	spec.MemoryMiB, spec.GuaranteedMiB = 1024, guaranteedMiB
	spec.MemoryPool = &driver.MemoryPool{Name: "acme-corp", SizeMiB: 2048}
	return spec
}

// overhead1G is what the unit of a VM of 1 GiB holds past its guest's memory.
const overhead1G = 256<<20 + (1024<<20)/50

const acmeSlice = "hangar-pool_acme_corp.slice"

func (f *fakeSystemd) propsOf(name string) []sd.Property {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.props[name])
}

// TestAPoolsVMsRunInItsSlice: a pool's VM's unit runs in its pool's slice,
// weighed by its guarantee, and one guaranteed less than its memory is the
// OOM killer's first; the slice holds them to the pool's size past their
// units' overheads, at their weights together. A VM in no pool keeps its
// memory's weight and machinesSlice, whatever guarantee its spec carries.
func TestAPoolsVMsRunInItsSlice(t *testing.T) {
	s := newFakeSystemd()
	h := newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) { c.cgroupRoot = t.TempDir() })
	alone := testSpec("alone", driver.RestartAlways)
	alone.MemoryMiB, alone.GuaranteedMiB = 1024, 256
	for _, spec := range []driver.VMSpec{pooledSpec("shared", 256), pooledSpec("dedicated", 0), alone} {
		if err := h.Start(t.Context(), spec); err != nil {
			t.Fatal(err)
		}
	}
	for uid, want := range map[string]struct {
		slice  string
		weight uint64
		score  int32
	}{
		"shared":    {acmeSlice, 1, sharedOOMScore},
		"dedicated": {acmeSlice, 4, 0},
		"alone":     {machinesSlice, 4, 0},
	} {
		props := s.propsOf(unitName(uid))
		if got := prop(props, "Slice"); got != want.slice {
			t.Errorf("%s runs in %v, want %s", uid, got, want.slice)
		}
		if got := prop(props, "CPUWeight"); got != want.weight {
			t.Errorf("%s weighs %v, want %d", uid, got, want.weight)
		}
		if got := prop(props, "OOMScoreAdjust"); got != want.score {
			t.Errorf("%s's OOM score is %v, want %d", uid, got, want.score)
		}
		if got := prop(props, "MemoryHigh"); got != nil {
			t.Errorf("%s starts with MemoryHigh %v, want none", uid, got)
		}
		if got := prop(props, "OOMPolicy"); got != "kill" {
			t.Errorf("%s's OOMPolicy is %v, want kill", uid, got)
		}
		post := prop(props, "ExecStopPost")
		b, _ := json.Marshal(post)
		if !strings.Contains(string(b), `"--stopped","`+uid+`"`) {
			t.Errorf("%s's ExecStopPost is %s, want the launcher with --stopped %s", uid, b, uid)
		}
	}
	slice := s.propsOf(acmeSlice)
	if got, want := prop(slice, "MemoryMax"), uint64(2048<<20+2*overhead1G); got != want {
		t.Errorf("the pool's slice's MemoryMax is %v, want %d: its size and two units' overheads", got, want)
	}
	if got := prop(slice, "CPUWeight"); got != uint64(5) {
		t.Errorf("the pool's slice weighs %v, want 5, its VMs' together", got)
	}
	if got := prop(slice, "MemorySwapMax"); got != uint64(0) {
		t.Errorf("the pool's slice's MemorySwapMax is %v, want 0", got)
	}
}

// TestAPoolsSliceFollowsItsMembers: setting a pool's size counts the VMs
// running in its slice then, at the weights their cgroups have; a pool no
// VM runs in gets no slice.
func TestAPoolsSliceFollowsItsMembers(t *testing.T) {
	s := newFakeSystemd()
	root := t.TempDir()
	h := newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) { c.cgroupRoot = root })
	ctx := t.Context()
	for _, spec := range []driver.VMSpec{pooledSpec("a", 256), pooledSpec("b", 512)} {
		if err := h.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
	}
	// A's weight was changed while it ran.
	cg := filepath.Join(root, machinesSlice, acmeSlice, unitName("a"))
	if err := os.MkdirAll(cg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cg, "cpu.weight"), []byte("7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.SetMemoryPool(ctx, driver.MemoryPool{Name: "acme-corp", SizeMiB: 4096}); err != nil {
		t.Fatal(err)
	}
	slice := s.propsOf(acmeSlice)
	if got, want := prop(slice, "MemoryMax"), uint64(4096<<20+2*overhead1G); got != want {
		t.Errorf("MemoryMax %v, want %d", got, want)
	}
	if got := prop(slice, "CPUWeight"); got != uint64(9) {
		t.Errorf("CPUWeight %v, want 9: a's 7 and b's 2", got)
	}

	s.exit(t, h, "b", 0)
	if err := h.SetMemoryPool(ctx, driver.MemoryPool{Name: "acme-corp", SizeMiB: 4096}); err != nil {
		t.Fatal(err)
	}
	if got, want := prop(s.propsOf(acmeSlice), "MemoryMax"), uint64(4096<<20+overhead1G); got != want {
		t.Errorf("with b exited, MemoryMax %v, want %d", got, want)
	}

	if err := h.SetMemoryPool(ctx, driver.MemoryPool{Name: "nobody", SizeMiB: 4096}); err != nil {
		t.Fatal(err)
	}
	if got := s.propsOf(poolSlice("nobody")); got != nil {
		t.Errorf("a pool no VM runs in got a slice: %v", got)
	}
	for _, p := range []driver.MemoryPool{{Name: "Acme", SizeMiB: 1}, {Name: "a/b", SizeMiB: 1}, {Name: "acme", SizeMiB: 0}} {
		if err := h.SetMemoryPool(ctx, p); err == nil {
			t.Errorf("set pool %+v", p)
		}
		spec := pooledSpec("c", 0)
		spec.MemoryPool = &p
		if err := h.Start(ctx, spec); err == nil {
			t.Errorf("started a VM in pool %+v", p)
		}
	}
}

// TestTheSweepStopsOnlyEmptyPoolSlices: a pool's slice goes once no VM
// runs in it and its cgroup holds none; one that holds something stays.
func TestTheSweepStopsOnlyEmptyPoolSlices(t *testing.T) {
	s := newFakeSystemd()
	root := t.TempDir()
	h := newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) { c.cgroupRoot = root })
	ctx := t.Context()
	busy, idle, held := pooledSpec("busy", 0), pooledSpec("idle", 0), pooledSpec("held", 0)
	idle.MemoryPool = &driver.MemoryPool{Name: "idle", SizeMiB: 1024}
	held.MemoryPool = &driver.MemoryPool{Name: "held", SizeMiB: 1024}
	for _, spec := range []driver.VMSpec{busy, idle, held} {
		if err := h.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
	}
	s.exit(t, h, "idle", 0)
	s.exit(t, h, "held", 0)
	// Something systemd still has in it.
	if err := os.MkdirAll(filepath.Join(root, machinesSlice, poolSlice("held"), "x.service"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := h.SweepMemoryPools(ctx); err != nil {
		t.Fatal(err)
	}
	have, err := s.listSlices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{acmeSlice, poolSlice("held")}; !slices.Equal(have, want) {
		t.Errorf("slices left %v, want %v", have, want)
	}
}

// TestSetMemoryShareFollowsTheGuarantee: a running VM's weight and its
// processes' OOM score follow the guarantee it is given, its unit's
// memory.oom.group set again with the weight; a weight that is right
// already is not set again.
func TestSetMemoryShareFollowsTheGuarantee(t *testing.T) {
	s := newFakeSystemd()
	root := t.TempDir()
	h := newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) { c.cgroupRoot = root })
	ctx := t.Context()
	spec := pooledSpec("m1", 0)
	if err := h.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := h.SetMemoryShare(ctx, "m1", spec.MemoryPool, 256); err == nil {
		t.Error("set the share of a VM with no cgroup")
	}
	sleep := exec.Command("sleep", "60")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sleep.Process.Kill(); _ = sleep.Wait() })
	cg := filepath.Join(root, machinesSlice, acmeSlice, unitName("m1"))
	for name, content := range map[string]string{"cpu.weight": "4\n", "cgroup.procs": strconv.Itoa(sleep.Process.Pid) + "\n", "memory.oom.group": "0\n"} {
		if err := os.MkdirAll(cg, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cg, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// As it started: nothing to set, and memory.oom.group, set again only
	// with the weight, left as it is.
	if err := h.SetMemoryShare(ctx, "m1", spec.MemoryPool, 0); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(cg, "memory.oom.group")); string(b) != "0\n" {
		t.Errorf("memory.oom.group written with nothing changed: %q", b)
	}

	if err := h.SetMemoryShare(ctx, "m1", spec.MemoryPool, 256); err != nil {
		t.Fatal(err)
	}
	if got := prop(s.propsOf(unitName("m1")), "CPUWeight"); got != uint64(1) {
		t.Errorf("guaranteed 256 MiB, it weighs %v, want 1", got)
	}
	if b, _ := os.ReadFile(filepath.Join(cg, "memory.oom.group")); string(b) != "1" {
		t.Errorf("memory.oom.group is %q, want 1", b)
	}
	if b, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(sleep.Process.Pid), "oom_score_adj")); strings.TrimSpace(string(b)) != strconv.Itoa(sharedOOMScore) {
		t.Errorf("its process's oom_score_adj is %q, want %d", b, sharedOOMScore)
	}
	if err := h.SetMemoryShare(ctx, "nobody", nil, 0); err == nil {
		t.Error("set the share of a machine with no VM")
	}
}

// TestAPoolsVMStartingOrItsSliceFullWakesTheSqueeze: MemoryEvents signals
// as a pool's VM starts, and when its pool's slice's memory.events moves,
// once its size is set.
func TestAPoolsVMStartingOrItsSliceFullWakesTheSqueeze(t *testing.T) {
	s := newFakeSystemd()
	root := t.TempDir()
	h := newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) { c.cgroupRoot = root })
	ctx := t.Context()
	if err := h.Start(ctx, testSpec("plain", driver.RestartAlways)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.MemoryEvents():
		t.Error("a VM in no pool started, and MemoryEvents signalled")
	default:
	}
	if err := h.Start(ctx, pooledSpec("m1", 256)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.MemoryEvents():
	default:
		t.Error("a pool's VM started, and MemoryEvents said nothing")
	}
	slice := filepath.Join(root, machinesSlice, acmeSlice)
	if err := os.MkdirAll(slice, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slice, "memory.events"), []byte("max 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.SetMemoryPool(ctx, *pooledSpec("m1", 256).MemoryPool); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slice, "memory.events"), []byte("max 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.MemoryEvents():
	case <-time.After(10 * time.Second):
		t.Error("the pool's memory.events changed and MemoryEvents said nothing")
	}
}

// TestASharedVMBootsAtItsGuarantee: a Shared VM's balloon is inflated as it
// boots to leave its guest its guarantee, which its boot disk says, for a
// container's guest to wait for; any other VM's is not, and its boot disk
// says nothing of it.
func TestASharedVMBootsAtItsGuarantee(t *testing.T) {
	for name, c := range map[string]struct {
		spec    driver.VMSpec
		want    string
		wantMiB uint64
	}{
		"Shared":    {pooledSpec("m1", 256), "size=768M,free_page_reporting=on", 256},
		"Dedicated": {pooledSpec("m1", 0), "size=0,free_page_reporting=on", 0},
		"no pool": {func() driver.VMSpec {
			s := pooledSpec("m1", 256)
			s.MemoryPool = nil
			return s
		}(), "size=0,free_page_reporting=on", 0},
	} {
		if got := balloonArg(c.spec); got != c.want {
			t.Errorf("%s: --balloon %s, want %s", name, got, c.want)
		}
		b, err := (&Hypervisor{}).bootDisk(c.spec, "")
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := bootdisk.Read(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.GetBootMemoryMib(); got != c.wantMiB {
			t.Errorf("%s: the boot disk says to boot at %d MiB, want %d", name, got, c.wantMiB)
		}
	}
}

// TestAnOOMKillIsRecorded: the unit's ExecStopPost records an OOM kill as
// the VM's exit, only with systemd's result and the cgroup's count both
// saying so; the VM is then listed exited, killed.
func TestAnOOMKillIsRecorded(t *testing.T) {
	s := newFakeSystemd()
	dir := shortDir(t)
	h := newDriver(t, dir, s, &fakeGuest{})
	ctx := t.Context()
	if err := h.Start(ctx, testSpec("m1", driver.RestartNever)); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	procCgroup := filepath.Join(t.TempDir(), "cgroup")
	if err := os.WriteFile(procCgroup, []byte("0::/hangar.slice/hangar-vm-m1.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cg := filepath.Join(root, "hangar.slice", "hangar-vm-m1.service")
	if err := os.MkdirAll(cg, 0o755); err != nil {
		t.Fatal(err)
	}
	events := func(kills int) {
		t.Helper()
		content := "low 0\nhigh 0\nmax 3\noom 1\noom_kill 1\noom_group_kill " + strconv.Itoa(kills) + "\n"
		if err := os.WriteFile(filepath.Join(cg, "memory.events"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	exitFile := filepath.Join(dir, "m1", exitFile)
	events(1)
	if err := RecordOOMKill(dir, "m1", procCgroup, root, "success"); err != nil || exists(exitFile) {
		t.Errorf("a unit that ended successfully: %v, recorded %t", err, exists(exitFile))
	}
	events(0)
	if err := RecordOOMKill(dir, "m1", procCgroup, root, "oom-kill"); err != nil || exists(exitFile) {
		t.Errorf("a unit with no group kill counted: %v, recorded %t", err, exists(exitFile))
	}
	events(1)
	if err := RecordOOMKill(dir, "m1", procCgroup, root, "oom-kill"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	delete(s.units, unitName("m1"))
	s.mu.Unlock()
	vm, ok := listed(t, h, "m1")
	if !ok || vm.State != driver.VMExited || !vm.OOMKilled || vm.ExitCode != ExitNoRecord {
		t.Errorf("listed %+v (%t), want exited, killed, with %d", vm, ok, ExitNoRecord)
	}
	if err := RecordOOMKill(dir, "../m1", procCgroup, root, "oom-kill"); err == nil {
		t.Error("recorded for a uid that is a path")
	}
}

// chAPIServer serves a fake Cloud Hypervisor API at the VM's socket,
// answering each path as handlers say.
func chAPIServer(t *testing.T, h *Hypervisor, uid string, handlers map[string]http.HandlerFunc) {
	t.Helper()
	socket := filepath.Join(h.dir(uid), apiFile)
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	for path, f := range handlers {
		mux.HandleFunc(path, f)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// TestTheBalloonIsReadAndSetThroughCloudHypervisor: what the balloon holds
// and the guest's report come from vm.balloon-stats, or, from a guest that
// gives none, what the balloon holds from vm.info; a balloon is set by
// vm.resize, never past the VM's memory.
func TestTheBalloonIsReadAndSetThroughCloudHypervisor(t *testing.T) {
	s := newFakeSystemd()
	h := newDriver(t, shortDir(t), s, &fakeGuest{})
	ctx := t.Context()
	spec := testSpec("m1", driver.RestartAlways)
	spec.MemoryMiB = 2048
	if err := h.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	stats := true
	var asked []string
	chAPIServer(t, h, "m1", map[string]http.HandlerFunc{
		"GET /api/v1/vm.balloon-stats": func(w http.ResponseWriter, _ *http.Request) {
			if !stats {
				http.Error(w, "Balloon statistics feature was not negotiated", http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, `{"balloon_actual":536870912,"last_update":1764140000123,"stats":{"free_memory":1,"total_memory":1610612736,"available_memory":1073741824,"alloc_stalls":2,"direct_reclaims":3,"async_reclaims":40,"oom_kills":1}}`)
		},
		"GET /api/v1/vm.info": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"state":"Running","memory_actual_size":1610612736}`)
		},
		"PUT /api/v1/vm.resize": func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			asked = append(asked, string(b))
			w.WriteHeader(http.StatusNoContent)
		},
	})
	b, err := h.Balloon(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	want := driver.Balloon{Actual: 512 << 20, Total: 1536 << 20, Available: 1024 << 20, Updated: time.UnixMilli(1764140000123),
		Reclaims: 45, HasReclaims: true, OOMKills: 1, HasOOMKills: true}
	if b != want {
		t.Errorf("balloon %+v, want %+v", b, want)
	}
	stats = false
	if b, err := h.Balloon(ctx, "m1"); err != nil || b != (driver.Balloon{Actual: 512 << 20}) {
		t.Errorf("from a guest giving no statistics: %+v, %v; want what the balloon holds alone", b, err)
	}
	if err := h.SetBalloon(ctx, "m1", 768<<20); err != nil {
		t.Fatal(err)
	}
	if want := []string{`{"desired_balloon":805306368}`}; !slices.Equal(asked, want) {
		t.Errorf("asked %v, want %v", asked, want)
	}
	if err := h.SetBalloon(ctx, "m1", 2049<<20); err == nil {
		t.Error("set a balloon past the VM's memory")
	}
	if _, err := h.Balloon(ctx, "m2"); err == nil {
		t.Error("read the balloon of a VM with no API")
	}
}

// TestOOMGroupKillsAreTheSlicesCount: the VMs killed whole are what
// machinesSlice's memory.events counts.
func TestOOMGroupKillsAreTheSlicesCount(t *testing.T) {
	root := t.TempDir()
	h := newDriverWith(t, shortDir(t), newFakeSystemd(), &fakeGuest{}, func(c *Config) { c.cgroupRoot = root })
	if _, err := h.OOMGroupKills(t.Context()); err == nil {
		t.Error("counted with no memory.events")
	}
	if err := os.MkdirAll(filepath.Join(root, machinesSlice), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, machinesSlice, "memory.events"), []byte("low 0\nhigh 0\nmax 9\noom 2\noom_kill 5\noom_group_kill 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := h.OOMGroupKills(t.Context()); err != nil || n != 2 {
		t.Errorf("counted %d, %v; want 2", n, err)
	}
}
