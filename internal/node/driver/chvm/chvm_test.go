package chvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sd "github.com/coreos/go-systemd/v22/dbus"

	"github.com/YukinaMochizuki/hangar/internal/guest/bootdisk"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// fakeSystemd keeps units as systemd would, without running them.
type fakeSystemd struct {
	mu      sync.Mutex
	units   map[string]*unitState
	props   map[string][]sd.Property
	starts  int
	stopped []string
	listErr error
}

func newFakeSystemd() *fakeSystemd {
	return &fakeSystemd{units: map[string]*unitState{}, props: map[string][]sd.Property{}}
}

func (f *fakeSystemd) start(_ context.Context, name string, props []sd.Property) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.units[name]; ok {
		return fmt.Errorf("unit %s exists, %s", name, u.Active)
	}
	f.starts++
	// Microseconds, as systemd keeps them.
	f.units[name] = &unitState{Name: name, Active: "active", Started: time.Now().Truncate(time.Microsecond)}
	f.props[name] = props
	return nil
}

func (f *fakeSystemd) stop(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, name)
	delete(f.units, name)
	return nil
}

func (f *fakeSystemd) resetFailed(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.units[name]; ok && u.Active == "failed" {
		delete(f.units, name)
	}
	return nil
}

func (f *fakeSystemd) list(context.Context) ([]unitState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []unitState
	for _, u := range f.units {
		out = append(out, *u)
	}
	return out, nil
}

func (f *fakeSystemd) close() {}

// exit makes a unit's VM exit with code, its restart policy keeping it
// down: the launcher records it, and the unit goes.
func (f *fakeSystemd) exit(t *testing.T, h *Hypervisor, uid string, code int) {
	t.Helper()
	writeExit(t, h, uid, code)
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.units, unitName(uid))
}

func (f *fakeSystemd) stops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.stopped)
}

func writeExit(t *testing.T, h *Hypervisor, uid string, code int) {
	t.Helper()
	b, _ := json.Marshal(exitRecord{Code: code, Started: time.Unix(100, 0), Ended: time.Unix(200, 0)})
	if err := os.WriteFile(filepath.Join(h.dir(uid), exitFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeGuest answers Stop as stop says; nil stop answers and does nothing.
type fakeGuest struct {
	mu    sync.Mutex
	stop  func(ctx context.Context, uid string) error
	asked []string
	// stopping says whether the VM was marked as being stopped when the
	// guest was asked.
	stopping []bool
	h        *Hypervisor
}

func (g *fakeGuest) Ready(context.Context, string) error { return nil }

func (g *fakeGuest) Exec(context.Context, string, driver.ExecSpec) (driver.ExecSession, error) {
	return nil, errors.New("the hypervisor driver never execs")
}

func (g *fakeGuest) Stop(ctx context.Context, uid string, grace time.Duration) error {
	g.mu.Lock()
	g.asked = append(g.asked, uid)
	g.stopping = append(g.stopping, exists(filepath.Join(g.h.dir(uid), stopFile)))
	f := g.stop
	g.mu.Unlock()
	if f == nil {
		return nil
	}
	return f(ctx, uid)
}

// shortDir is a temporary directory short enough for a VM's socket.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "chvm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

const (
	testGrace = 300 * time.Millisecond
	testSlack = 200 * time.Millisecond
)

func newDriver(t *testing.T, dir string, s *fakeSystemd, g *fakeGuest) *Hypervisor {
	t.Helper()
	h, err := New(t.Context(), Config{
		Dir: dir, CloudHypervisor: os.Args[0], Kernel: os.Args[0], Initramfs: os.Args[0], Cmdline: "quiet",
		Launcher:  []string{os.Args[0], "vmm"},
		StopGrace: testGrace, StopSlack: testSlack,
		systemd: s, guest: g, dynamicOK: true, etc: testEtc(t),
		procUsers: func() (map[uint32]bool, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	g.h = h
	return h
}

// testEtc is an /etc whose users and groups have none of the VMs' ids.
func testEtc(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"passwd": "root:x:0:0::/root:/bin/sh\n", "group": "root:x:0:\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func testSpec(uid string, restart driver.RestartPolicy) driver.VMSpec {
	return driver.VMSpec{
		MachineUID: uid, Machine: "proj/Web_Server.1", VCPUs: 2, MemoryMiB: 512,
		Root: driver.Disk{MachineUID: uid, Digest: "sha256:ab", Path: "/dev/zvol/hangar/roots/" + uid, Size: 1 << 30},
		Interfaces: []driver.Interface{{
			MachineUID: uid, Name: "eth0", NetworkUID: "n1", MAC: "02:00:00:00:00:01",
			Address: netip.MustParsePrefix("10.0.0.2/24"), Gateway: netip.MustParseAddr("10.0.0.1"), Device: "hn123",
		}},
		Nameservers: []string{"10.0.0.1"}, Search: []string{"proj.hangar.internal"},
		Run:     driver.RunSpec{Command: []string{"nginx", "-g", "daemon off;"}, Env: []string{"A=b"}, WorkingDir: "/srv", User: "1000"},
		Restart: restart,
	}
}

func listed(t *testing.T, h *Hypervisor, uid string) (driver.VM, bool) {
	t.Helper()
	vms, err := h.VMs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vms {
		if v.MachineUID == uid {
			return v, true
		}
	}
	return driver.VM{}, false
}

func prop(props []sd.Property, name string) any {
	for _, p := range props {
		if p.Name == name {
			return p.Value.Value()
		}
	}
	return nil
}

func TestStartWritesWhatTheLauncherRuns(t *testing.T) {
	for policy, want := range map[driver.RestartPolicy]string{
		driver.RestartAlways: "always", driver.RestartOnFailure: "on-failure", driver.RestartNever: "no",
	} {
		t.Run(want, func(t *testing.T) {
			s := newFakeSystemd()
			h := newDriver(t, shortDir(t), s, &fakeGuest{})
			spec := testSpec("m1", policy)
			if err := h.Start(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
			props := s.props["hangar-vm-m1.service"]
			if got := prop(props, "Restart"); got != want {
				t.Errorf("Restart=%v, want %s", got, want)
			}
			if got := prop(props, "StartLimitIntervalUSec"); got != uint64(0) {
				t.Errorf("StartLimitIntervalUSec=%v: systemd would give up restarting", got)
			}
			if got := prop(props, "CollectMode"); got != "inactive-or-failed" {
				t.Errorf("CollectMode=%v", got)
			}
			if got := prop(props, "RestartMaxDelayUSec"); got != uint64(60_000_000) {
				t.Errorf("RestartMaxDelayUSec=%v", got)
			}
			// systemd stopping the unit, as at host shutdown, waits for the
			// launcher to give the guest its grace.
			if got, want := prop(props, "TimeoutStopUSec"), usec(stopAsk+testGrace+testSlack+killGrace+drainWait); got != want {
				t.Errorf("TimeoutStopUSec=%v, want %d", got, want)
			}
			if got, _ := prop(props, "After").([]string); !slices.Equal(got, []string{"network.target"}) {
				t.Errorf("After=%v: at shutdown the network could go before the VM", got)
			}
			if got, _ := prop(props, "Before").([]string); !slices.Equal(got, []string{"hangar-node.service"}) {
				t.Errorf("Before=%v: at shutdown the agent could see the VM stop and start it again", got)
			}
			exec := fmt.Sprint(prop(props, "ExecStart"))
			if !strings.Contains(exec, os.Args[0]+" vmm --dir "+h.cfg.Dir+" --vm-user 200000 m1") {
				t.Errorf("ExecStart=%s", exec)
			}
			// systemd reads the flag as "-", ignoring the exit status.
			if !strings.HasSuffix(exec, "] false}]") {
				t.Errorf("ExecStart=%s ignores the launcher's exit status", exec)
			}

			ln, err := readLaunch(h.dir("m1"))
			if err != nil {
				t.Fatal(err)
			}
			if ln.Hash != spec.Hash() || ln.Spec.Hash() != spec.Hash() || ln.CloudHypervisor != os.Args[0] || ln.Cmdline != "quiet" ||
				ln.StopGrace != testGrace || ln.StopSlack != testSlack || ln.VMUser != DefaultVMUsers.First {
				t.Errorf("launch %+v", ln)
			}
			f, err := os.Open(filepath.Join(h.dir("m1"), bootFile))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			if fi, _ := f.Stat(); fi.Size()%512 != 0 {
				t.Errorf("the boot disk is %d bytes, not whole sectors", fi.Size())
			}
			cfg, err := bootdisk.Read(f)
			if err != nil {
				t.Fatal(err)
			}
			p := cfg.GetProcess()
			if !slices.Equal(p.GetArgs(), spec.Run.Command) || !slices.Equal(p.GetEnv(), spec.Run.Env) || p.GetWorkingDir() != "/srv" || p.GetUser() != "1000" {
				t.Errorf("process %v", p)
			}
			if cfg.GetHostname() != "web-server-1" || !slices.Equal(cfg.GetNameservers(), []string{"10.0.0.1"}) ||
				!slices.Equal(cfg.GetSearch(), []string{"proj.hangar.internal"}) {
				t.Errorf("hostname %q, nameservers %v, search %v", cfg.GetHostname(), cfg.GetNameservers(), cfg.GetSearch())
			}
			if i := cfg.GetInterfaces(); len(i) != 1 || i[0].GetMac() != "02:00:00:00:00:01" || i[0].GetAddress() != "10.0.0.2/24" || i[0].GetGateway() != "10.0.0.1" {
				t.Errorf("interfaces %v", i)
			}
			if vm, ok := listed(t, h, "m1"); !ok || vm.State != driver.VMRunning || vm.SpecHash != spec.Hash() || vm.Started.IsZero() {
				t.Errorf("listed %+v %t", vm, ok)
			}
		})
	}
}

// TestAVMMayOpenItsVolumes: the unit's device policy lets it open its
// volumes' zvols as it does its root disk's, and nothing else.
func TestAVMMayOpenItsVolumes(t *testing.T) {
	s := newFakeSystemd()
	h := newDriver(t, shortDir(t), s, &fakeGuest{})
	spec := testSpec("m1", driver.RestartAlways)
	spec.Disks = []driver.Volume{{DiskUID: "d1", Path: "/dev/zvol/hangar/disks/d1", Size: 1 << 30}}
	if err := h.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	got := prop(s.props["hangar-vm-m1.service"], "DeviceAllow")
	want := []pathPair{{"/dev/kvm", "rw"}, {"/dev/net/tun", "rw"}, {spec.Root.Path, "rwm"}, {"/dev/zvol/hangar/disks/d1", "rwm"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DeviceAllow=%v, want %v", got, want)
	}
	ln, err := readLaunch(h.dir("m1"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ln.Spec.Disks, spec.Disks) {
		t.Errorf("the launch attaches %+v, want %+v", ln.Spec.Disks, spec.Disks)
	}
}

func TestStartIsIdempotent(t *testing.T) {
	s := newFakeSystemd()
	dir := shortDir(t)
	h := newDriver(t, dir, s, &fakeGuest{})
	spec := testSpec("m1", driver.RestartAlways)
	if err := h.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	first, _ := listed(t, h, "m1")
	other := spec
	other.MemoryMiB = 1024
	// A new instance reads back what the first made.
	fresh := newDriver(t, dir, s, &fakeGuest{})
	for _, d := range []*Hypervisor{h, fresh} {
		if err := d.Start(t.Context(), spec); err != nil {
			t.Error(err)
		}
		if err := d.Start(t.Context(), other); err == nil {
			t.Error("started another spec over a running VM")
		}
		if vm, ok := listed(t, d, "m1"); !ok || !reflect.DeepEqual(vm, first) {
			t.Errorf("listed %+v %t, want %+v", vm, ok, first)
		}
	}
	if s.starts != 1 {
		t.Errorf("%d starts, want 1", s.starts)
	}
}

func TestExitedVMs(t *testing.T) {
	s := newFakeSystemd()
	dir := shortDir(t)
	h := newDriver(t, dir, s, &fakeGuest{})
	spec := testSpec("m1", driver.RestartNever)
	if err := h.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	s.exit(t, h, "m1", 3)
	for _, d := range []*Hypervisor{h, newDriver(t, dir, s, &fakeGuest{})} {
		if vm, ok := listed(t, d, "m1"); !ok || vm.State != driver.VMExited || vm.ExitCode != 3 || vm.SpecHash != spec.Hash() || !vm.Started.Equal(time.Unix(100, 0)) {
			t.Errorf("listed %+v %t", vm, ok)
		}
	}
	other := spec
	other.VCPUs = 4
	if err := h.Start(t.Context(), other); err == nil {
		t.Error("started another spec over an exited VM")
	}
	// The same spec runs again, and its old record goes.
	if err := h.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if vm, ok := listed(t, h, "m1"); !ok || vm.State != driver.VMRunning {
		t.Errorf("started again: %+v %t", vm, ok)
	}
	if exists(filepath.Join(h.dir("m1"), exitFile)) {
		t.Error("the old exit record is left")
	}

}

// TestInterruptedVMs: a VM whose unit went with no record and no stop,
// as at a reboot or with its launcher killed outright, starts again under
// always and on-failure, so it is not listed and the agent starts it; under
// never it has exited, with ExitNoRecord (decision 16).
func TestInterruptedVMs(t *testing.T) {
	for _, tt := range []struct {
		name   string
		policy driver.RestartPolicy
		listed bool
	}{
		{"always", driver.RestartAlways, false},
		{"on-failure", driver.RestartOnFailure, false},
		{"never", driver.RestartNever, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newFakeSystemd()
			dir := shortDir(t)
			h := newDriver(t, dir, s, &fakeGuest{})
			spec := testSpec("m1", tt.policy)
			if err := h.Start(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			delete(s.units, unitName("m1"))
			s.mu.Unlock()
			for _, d := range []*Hypervisor{h, newDriver(t, dir, s, &fakeGuest{})} {
				vm, ok := listed(t, d, "m1")
				if ok != tt.listed || ok && (vm.State != driver.VMExited || vm.ExitCode != ExitNoRecord || vm.SpecHash != spec.Hash()) {
					t.Errorf("listed %+v %t, want listed %t, exited with %d", vm, ok, tt.listed, ExitNoRecord)
				}
			}
			if err := h.Start(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
			if vm, ok := listed(t, h, "m1"); !ok || vm.State != driver.VMRunning {
				t.Errorf("started again: %+v %t", vm, ok)
			}
			// A stop cut short after its unit went is a stop, not an
			// interruption.
			if err := os.WriteFile(filepath.Join(h.dir("m1"), stopFile), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			delete(s.units, unitName("m1"))
			s.mu.Unlock()
			if vm, ok := listed(t, h, "m1"); ok {
				t.Errorf("a VM stopped with its stop cut short is listed: %+v", vm)
			}
		})
	}
}

// TestAStaleStopMarkIsCleared: a stop cut short, then the machine wanted
// running again with the same spec, leaves the VM's next exit to its
// restart policy, not waiting for a stop that never comes.
func TestAStaleStopMarkIsCleared(t *testing.T) {
	s := newFakeSystemd()
	h := newDriver(t, shortDir(t), s, &fakeGuest{})
	spec := testSpec("m1", driver.RestartAlways)
	if err := h.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := h.Stop(ctx, "m1"); err == nil {
		t.Fatal("a Stop cut short says it stopped")
	}
	if err := h.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(h.dir("m1"), stopFile)) {
		t.Error("the stop's mark outlived the start")
	}
	// The container exits on its own, and the unit restarts it.
	writeExit(t, h, "m1", 0)
	if vm, ok := listed(t, h, "m1"); !ok || vm.State != driver.VMRunning {
		t.Errorf("listed %+v %t, want running: its policy restarts it", vm, ok)
	}
	if s.starts != 1 {
		t.Errorf("%d starts, want 1", s.starts)
	}
}

// stopped checks what a Stop leaves: no unit, no runtime files, the logs.
func stopped(t *testing.T, h *Hypervisor, s *fakeSystemd, uid string) {
	t.Helper()
	if !slices.Contains(s.stops(), unitName(uid)) {
		t.Error("the unit was not stopped")
	}
	if vm, ok := listed(t, h, uid); ok {
		t.Errorf("a stopped VM is listed: %+v", vm)
	}
	entries, _ := os.ReadDir(h.dir(uid))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{ConsoleLog}) {
		t.Errorf("the VM's directory holds %v, want only its log", names)
	}
	if logs, err := h.Logs(t.Context()); err != nil || !slices.Contains(logs, uid) {
		t.Errorf("logs %v %v", logs, err)
	}
}

func startWithLog(t *testing.T, h *Hypervisor, uid string) {
	t.Helper()
	if err := h.Start(t.Context(), testSpec(uid, driver.RestartAlways)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir(uid), ConsoleLog), []byte("log"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStopAsksTheGuestFirst(t *testing.T) {
	s := newFakeSystemd()
	g := &fakeGuest{}
	h := newDriver(t, shortDir(t), s, g)
	startWithLog(t, h, "m1")
	// The guest powers off a little after it is asked.
	g.stop = func(_ context.Context, uid string) error {
		time.AfterFunc(50*time.Millisecond, func() { writeExit(t, h, uid, 143) })
		return nil
	}
	start := time.Now()
	if err := h.Stop(t.Context(), "m1"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d >= testGrace {
		t.Errorf("took %v, though the guest was off in 50ms", d)
	}
	if !slices.Equal(g.asked, []string{"m1"}) || !g.stopping[0] {
		t.Errorf("the guest was asked %v, marked as stopping %v", g.asked, g.stopping)
	}
	stopped(t, h, s, "m1")
	if err := h.Stop(t.Context(), "m1"); err != nil {
		t.Errorf("stopping again: %v", err)
	}
	if len(g.asked) != 1 {
		t.Error("a stopped VM's guest was asked again")
	}
}

func TestStopKillsAGuestThatStaysUp(t *testing.T) {
	s := newFakeSystemd()
	h := newDriver(t, shortDir(t), s, &fakeGuest{})
	startWithLog(t, h, "m1")
	start := time.Now()
	if err := h.Stop(t.Context(), "m1"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < testGrace+testSlack || d > testGrace+testSlack+time.Second {
		t.Errorf("killed after %v, want after the grace and the slack, %v", d, testGrace+testSlack)
	}
	stopped(t, h, s, "m1")
}

// TestStopKillsAGuestThatDoesNotAnswer: a guest refusing the stop is
// killed at once, one not answering once the ask times out, in 3s.
func TestStopKillsAGuestThatDoesNotAnswer(t *testing.T) {
	for name, tt := range map[string]struct {
		stop     func(context.Context, string) error
		min, max time.Duration
	}{
		"refusing": {func(context.Context, string) error { return errors.New("connection refused") }, 0, testGrace / 2},
		"hanging": {func(ctx context.Context, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		}, 3 * time.Second, 4 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			s := newFakeSystemd()
			h := newDriver(t, shortDir(t), s, &fakeGuest{stop: tt.stop})
			startWithLog(t, h, "m1")
			start := time.Now()
			if err := h.Stop(t.Context(), "m1"); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d < tt.min || d > tt.max {
				t.Errorf("took %v, want between %v and %v", d, tt.min, tt.max)
			}
			stopped(t, h, s, "m1")
		})
	}
}

// TestAStopCutShort: a Stop whose caller gives up leaves the VM listed,
// and its launcher waiting, for the next Stop to finish.
func TestAStopCutShort(t *testing.T) {
	s := newFakeSystemd()
	h := newDriver(t, shortDir(t), s, &fakeGuest{})
	startWithLog(t, h, "m1")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := h.Stop(ctx, "m1"); err == nil {
		t.Error("a Stop cut short says it stopped")
	}
	// The guest has powered off meanwhile: the launcher waits.
	writeExit(t, h, "m1", 143)
	if vm, ok := listed(t, h, "m1"); !ok || vm.State != driver.VMExited || vm.ExitCode != 143 {
		t.Errorf("listed %+v %t, want exited", vm, ok)
	}
	if err := h.Start(t.Context(), testSpec("m1", driver.RestartAlways)); err == nil {
		t.Error("started a VM whose launcher waits to be stopped")
	}
	if err := h.Stop(t.Context(), "m1"); err != nil {
		t.Fatal(err)
	}
	stopped(t, h, s, "m1")
}

func TestStopOfWhatIsNotThere(t *testing.T) {
	s := newFakeSystemd()
	g := &fakeGuest{}
	h := newDriver(t, shortDir(t), s, g)
	if err := h.Stop(t.Context(), "absent"); err != nil {
		t.Error(err)
	}
	// An exited VM: nothing to ask, its files go.
	startWithLog(t, h, "m1")
	s.exit(t, h, "m1", 0)
	if err := h.Stop(t.Context(), "m1"); err != nil {
		t.Fatal(err)
	}
	if len(g.asked) != 0 || len(s.stops()) != 0 {
		t.Errorf("asked %v, stopped %v", g.asked, s.stops())
	}
	if _, ok := listed(t, h, "m1"); ok {
		t.Error("listed after Stop")
	}
	// A VM never logging leaves no directory.
	if err := h.Start(t.Context(), testSpec("m2", driver.RestartAlways)); err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(t.Context(), "m2"); err != nil {
		t.Fatal(err)
	}
	if exists(h.dir("m2")) {
		t.Error("the directory of a VM with no log is left")
	}
	// A unit left failed, its name taken, is reset by a stop, and by a
	// start.
	for _, uid := range []string{"m3", "m4"} {
		s.units[unitName(uid)] = &unitState{Name: unitName(uid), Active: "failed"}
	}
	if err := h.Stop(t.Context(), "m3"); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), testSpec("m4", driver.RestartAlways)); err != nil {
		t.Fatalf("starting over a failed unit: %v", err)
	}
	if _, ok := s.units[unitName("m3")]; ok {
		t.Error("a failed unit outlived a stop")
	}
}

func TestLogsGoOnlyWithTheVM(t *testing.T) {
	s := newFakeSystemd()
	h := newDriver(t, shortDir(t), s, &fakeGuest{stop: func(context.Context, string) error { return errors.New("no") }})
	startWithLog(t, h, "m1")
	if err := h.DeleteLogs(t.Context(), "m1"); err == nil {
		t.Error("deleted the logs of a running VM")
	}
	s.exit(t, h, "m1", 1)
	if err := h.DeleteLogs(t.Context(), "m1"); err == nil {
		t.Error("deleted the logs of a listed VM")
	}
	if err := h.Stop(t.Context(), "m1"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := h.DeleteLogs(t.Context(), "m1"); err != nil {
			t.Fatal(err)
		}
	}
	if exists(h.dir("m1")) {
		t.Error("the VM's directory is left")
	}
	if logs, err := h.Logs(t.Context()); err != nil || len(logs) != 0 {
		t.Errorf("logs %v %v", logs, err)
	}
}

// TestWhatIsNotTheDrivers: units and files outside its names are never
// listed or touched, and names climbing out are refused.
func TestWhatIsNotTheDrivers(t *testing.T) {
	s := newFakeSystemd()
	base := shortDir(t)
	dir := filepath.Join(base, "vms")
	h := newDriver(t, dir, s, &fakeGuest{})
	for _, name := range []string{"hangar-vmdecoy.service", "decoy.service", "hangar-vm-.service", "hangar-vm-a.b.service", "hangar-vm-x.socket"} {
		s.units[name] = &unitState{Name: name, Active: "active"}
	}
	outside := filepath.Join(base, "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".hidden", "not a uid"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
		// As a VM's directory, with a spec and an exit record.
		for _, f := range []string{ConsoleLog, specFile, exitFile} {
			if err := os.WriteFile(filepath.Join(dir, d, f), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "stray-file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if vms, err := h.VMs(t.Context()); err != nil || len(vms) != 0 {
		t.Errorf("VMs %+v %v", vms, err)
	}
	if logs, err := h.Logs(t.Context()); err != nil || len(logs) != 0 {
		t.Errorf("logs %v %v", logs, err)
	}
	for _, uid := range []string{"", "..", "../outside", "a/b", "-x", ".hidden", "not a uid", strings.Repeat("x", 65)} {
		if err := h.Stop(t.Context(), uid); err == nil {
			t.Errorf("stopped %q", uid)
		}
		if err := h.DeleteLogs(t.Context(), uid); err == nil {
			t.Errorf("deleted the logs of %q", uid)
		}
		spec := testSpec(uid, driver.RestartAlways)
		if err := h.Start(t.Context(), spec); err == nil {
			t.Errorf("started %q", uid)
		}
	}
	if len(s.stops()) != 0 {
		t.Errorf("stopped %v", s.stops())
	}
	for _, p := range []string{outside, filepath.Join(dir, ".hidden", ConsoleLog), filepath.Join(dir, "not a uid", ConsoleLog), filepath.Join(dir, "stray-file")} {
		if !exists(p) {
			t.Errorf("%s is gone", p)
		}
	}
}

func TestAFailedListIsNoEmptyOne(t *testing.T) {
	s := newFakeSystemd()
	h := newDriver(t, shortDir(t), s, &fakeGuest{})
	s.listErr = errors.New("D-Bus down")
	if _, err := h.VMs(t.Context()); err == nil {
		t.Error("VMs with systemd unreachable")
	}
	if err := h.Start(t.Context(), testSpec("m1", driver.RestartAlways)); err == nil {
		t.Error("started with systemd unreachable")
	}
	if err := h.DeleteLogs(t.Context(), "m1"); err == nil {
		t.Error("deleted logs with systemd unreachable")
	}
}

func TestSpecsNoVMCanRun(t *testing.T) {
	h := newDriver(t, shortDir(t), newFakeSystemd(), &fakeGuest{})
	for name, change := range map[string]func(*driver.VMSpec){
		"no vCPUs":            func(s *driver.VMSpec) { s.VCPUs = 0 },
		"no memory":           func(s *driver.VMSpec) { s.MemoryMiB = 0 },
		"another's root":      func(s *driver.VMSpec) { s.Root.MachineUID = "m2" },
		"another's interface": func(s *driver.VMSpec) { s.Interfaces[0].MachineUID = "m2" },
		"no device":           func(s *driver.VMSpec) { s.Interfaces[0].Device = "" },
		"no policy":           func(s *driver.VMSpec) { s.Restart = 7 },
		"a volume without a path": func(s *driver.VMSpec) {
			s.Disks = []driver.Volume{{DiskUID: "d1"}}
		},
		"a volume twice": func(s *driver.VMSpec) {
			s.Disks = []driver.Volume{{DiskUID: "d1", Path: "/dev/zvol/hangar/disks/d1"}, {DiskUID: "d1", Path: "/dev/zvol/hangar/disks/d1"}}
		},
		"the root as a volume": func(s *driver.VMSpec) {
			s.Disks = []driver.Volume{{DiskUID: "d1", Path: s.Root.Path}}
		},
	} {
		spec := testSpec("m1", driver.RestartAlways)
		spec.Interfaces = slices.Clone(spec.Interfaces)
		change(&spec)
		if err := h.Start(t.Context(), spec); err == nil {
			t.Errorf("%s: started", name)
		}
	}
	if exists(h.dir("m1")) {
		t.Error("a refused spec left a directory")
	}
}

func TestNewRefusesItsDirectory(t *testing.T) {
	for _, dir := range []string{"relative", "/a/../b", "/" + strings.Repeat("d", 40)} {
		_, err := New(t.Context(), Config{Dir: dir, CloudHypervisor: os.Args[0], Kernel: os.Args[0], Initramfs: os.Args[0],
			Launcher: []string{"x"}, systemd: newFakeSystemd(), guest: &fakeGuest{}, etc: testEtc(t)})
		if err == nil {
			t.Errorf("took %q", dir)
		}
	}
}

func TestHostnames(t *testing.T) {
	for machine, want := range map[string]string{
		"proj/web":                        "web",
		"proj/Web_Server.1":               "web-server-1",
		"web":                             "web",
		"proj/" + strings.Repeat("a", 70): strings.Repeat("a", 63),
		"proj/---":                        "m1-uid-abcde",
		"proj/":                           "m1-uid-abcde",
	} {
		if got := hostname(driver.VMSpec{Machine: machine, MachineUID: "M1_uid-abcdefgh"}); got != want {
			t.Errorf("hostname of %q: %q, want %q", machine, got, want)
		}
	}
}

// TestOnlyTheFirstInterfaceRoutesByDefault: a machine on several networks
// has its default route by its first interface alone, as the guest takes
// no more than one.
func TestOnlyTheFirstInterfaceRoutesByDefault(t *testing.T) {
	spec := testSpec("m1", driver.RestartAlways)
	second := spec.Interfaces[0]
	second.Name, second.NetworkUID, second.MAC = "eth1", "n2", "02:00:00:00:00:02"
	second.Address, second.Gateway = netip.MustParsePrefix("10.1.0.2/24"), netip.MustParseAddr("10.1.0.1")
	spec.Interfaces = append(spec.Interfaces, second)
	b, err := (&Hypervisor{}).bootDisk(spec)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := bootdisk.Read(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if i := cfg.GetInterfaces(); len(i) != 2 || i[0].GetGateway() != "10.0.0.1" || i[1].GetGateway() != "" || i[1].GetAddress() != "10.1.0.2/24" {
		t.Errorf("interfaces %v", i)
	}
}
