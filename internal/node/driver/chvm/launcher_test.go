package chvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// The test binary plays Cloud Hypervisor when fakeCHEnv is set, doing what
// it names: see fakeCH.
const fakeCHEnv = "CHVM_FAKE_CH"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeCHEnv); mode != "" {
		os.Exit(fakeCH(mode))
	}
	os.Exit(m.Run())
}

// fakeCH plays Cloud Hypervisor: it records its arguments and whether fds
// 3 and 4 were given to it in args.json beside its vsock socket, writes to
// the serial file, then frames what mode says on its stdout, the console:
//
//	exit:N   a record on each stream, then exit code N, then powers off
//	flood:N  N records of 64 KiB, then exit code 0
//	crash    a record, then dies with no exit record
//	hang     a record, then waits to be killed
//	stoppable  a record, then, once the guest is asked to stop (a file
//	         "stop" beside the socket, see launcherGuest), says it got
//	         SIGTERM and exits 0
//	garbage  bytes that are no record, forged and oversized headers, then
//	         exit code 5
func fakeCH(mode string) int {
	args := os.Args[1:]
	var vsock, serial string
	for i, a := range args {
		if i > 0 && args[i-1] == "--vsock" {
			vsock = a[strings.Index(a, "socket=")+len("socket="):]
		}
		if i > 0 && args[i-1] == "--serial" {
			serial = strings.TrimPrefix(a, "file=")
		}
	}
	var fds []int
	for fd := 3; fd <= 4; fd++ {
		if f := os.NewFile(uintptr(fd), ""); f != nil {
			if _, err := f.Stat(); err == nil {
				fds = append(fds, fd)
			}
		}
	}
	b, _ := json.Marshal(map[string]any{"args": args, "fds": fds})
	_ = os.WriteFile(filepath.Join(filepath.Dir(vsock), "args.json"), b, 0o600)
	if f, err := os.OpenFile(serial, os.O_WRONLY, 0); err == nil {
		_, _ = f.WriteString("kernel: hello\n")
		_ = f.Close()
	}
	w := console.NewWriter(os.Stdout, time.Now)
	_ = w.Write(console.Init, []byte("hangar-init: booted\n"))
	verb, arg, _ := strings.Cut(mode, ":")
	switch verb {
	case "exit":
		var code int32
		_, _ = fmt.Sscan(arg, &code)
		_ = w.Write(console.Stdout, []byte("out\n"))
		_ = w.Write(console.Stderr, []byte("err\n"))
		_ = w.WriteExit(console.ExitStatus{Code: code})
	case "flood":
		var n int
		_, _ = fmt.Sscan(arg, &n)
		chunk := make([]byte, console.MaxPayload)
		for range n {
			_ = w.Write(console.Stdout, chunk)
		}
		_ = w.WriteExit(console.ExitStatus{})
	case "crash":
		return 1
	case "hang":
		select {}
	case "stoppable":
		for !exists(filepath.Join(filepath.Dir(vsock), "stop")) {
			time.Sleep(10 * time.Millisecond)
		}
		_ = w.Write(console.Stdout, []byte("got SIGTERM\n"))
		_ = w.WriteExit(console.ExitStatus{})
	case "garbage":
		_, _ = os.Stdout.Write([]byte("\xc4\x47\x01not quite a header"))
		var huge [15]byte
		huge[0], huge[1], huge[2] = 0xC4, 0x47, byte(console.Stdout)
		huge[11], huge[12] = 0xff, 0xff
		_, _ = os.Stdout.Write(huge[:])
		_, _ = os.Stdout.Write(make([]byte, 100_000))
		_ = w.Write(console.Stdout, []byte("after the garbage\n"))
		_ = w.WriteExit(console.ExitStatus{Code: 5})
	}
	return 0
}

// launcherDir makes a VM's directory as Start leaves it, with Cloud
// Hypervisor this test binary in mode, and a launcher for it whose root
// disk and taps are plain files.
func launcherDir(t *testing.T, uid, mode string) (*Launcher, string) {
	t.Helper()
	t.Setenv(fakeCHEnv, mode)
	base := t.TempDir()
	dir := filepath.Join(base, uid)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := driver.VMSpec{
		MachineUID: uid, Machine: "p/" + uid, VCPUs: 2, MemoryMiB: 512,
		Root: driver.Disk{MachineUID: uid, Path: "/dev/zvol/hangar/roots/" + uid},
		Interfaces: []driver.Interface{{
			MachineUID: uid, Name: "eth0", NetworkUID: "n1", MAC: "02:00:00:00:00:01",
			Address: netip.MustParsePrefix("10.0.0.2/24"), Gateway: netip.MustParseAddr("10.0.0.1"), Device: "hn123",
		}},
		Run: driver.RunSpec{Command: []string{"true"}},
	}
	b, err := json.Marshal(launch{
		Spec: spec, Hash: spec.Hash(), CloudHypervisor: os.Args[0], Kernel: "/k", Initramfs: "/i", Cmdline: "quiet",
		StopGrace: testGrace, StopSlack: testSlack,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, specFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	file := func(name string) (*os.File, error) { return os.Create(filepath.Join(base, name)) }
	l := &Launcher{
		Dir:      base,
		OpenRoot: func(path string) (*os.File, error) { return file("root") },
		OpenTap:  func(i driver.Interface) (*os.File, error) { return file("tap-" + i.Device) },
		Guest:    &launcherGuest{dir: dir},
	}
	return l, dir
}

// launcherGuest is the guest of a fake Cloud Hypervisor: Stop leaves the
// file "stop" in the VM's directory, which the mode stoppable waits for,
// unless err says it fails.
type launcherGuest struct {
	dir    string
	err    error
	mu     sync.Mutex
	graces []time.Duration
}

func (g *launcherGuest) Ready(context.Context, string) error { return nil }

func (g *launcherGuest) Stop(_ context.Context, _ string, grace time.Duration) error {
	g.mu.Lock()
	g.graces = append(g.graces, grace)
	g.mu.Unlock()
	if g.err != nil {
		return g.err
	}
	return os.WriteFile(filepath.Join(g.dir, "stop"), nil, 0o600)
}

func (g *launcherGuest) asked() []time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.graces)
}

func runFor(t *testing.T, l *Launcher, ctx context.Context, uid string) int {
	t.Helper()
	done := make(chan int, 1)
	go func() { done <- l.Run(ctx, uid) }()
	select {
	case code := <-done:
		return code
	case <-time.After(30 * time.Second):
		t.Fatal("the launcher did not return")
		return 0
	}
}

func exitOf(t *testing.T, dir string) (exitRecord, bool) {
	t.Helper()
	r, ok, err := readExit(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r, ok
}

func TestTheLauncherExitsAsTheContainer(t *testing.T) {
	for _, code := range []int{0, 3, 143} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			l, dir := launcherDir(t, "m1", fmt.Sprintf("exit:%d", code))
			// An earlier run's record is not this one's.
			if err := os.WriteFile(filepath.Join(dir, exitFile), []byte(`{"Code":99}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := runFor(t, l, t.Context(), "m1"); got != code {
				t.Errorf("the launcher exited %d, the container %d", got, code)
			}
			if r, ok := exitOf(t, dir); !ok || r.Code != code || r.Error != "" {
				t.Errorf("exit record %+v %t", r, ok)
			}
			var got []string
			for _, r := range records(t, filepath.Join(dir, ConsoleLog)) {
				if r.Stream != console.Exit {
					got = append(got, string(r.Payload))
				}
			}
			if want := []string{"hangar-init: booted\n", "out\n", "err\n"}; !slices.Equal(got, want) {
				t.Errorf("console log %q, want %q", got, want)
			}
			if b, _ := os.ReadFile(filepath.Join(dir, SerialLog)); string(b) != "kernel: hello\n" {
				t.Errorf("serial log %q", b)
			}
			var ch struct {
				Args []string
				Fds  []int
			}
			b, _ := os.ReadFile(filepath.Join(dir, "args.json"))
			if err := json.Unmarshal(b, &ch); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(ch.Fds, []int{3, 4}) {
				t.Errorf("Cloud Hypervisor had fds %v, want the root disk's and a tap's", ch.Fds)
			}
			for _, want := range []string{"boot=2", "size=512M", "path=/dev/fd/3,image_type=raw", "fd=4,mac=02:00:00:00:00:01", "tty"} {
				if !slices.Contains(ch.Args, want) {
					t.Errorf("Cloud Hypervisor's arguments %q lack %q", ch.Args, want)
				}
			}
		})
	}
}

func TestTheLauncherLogsAcrossRuns(t *testing.T) {
	l, dir := launcherDir(t, "m1", "exit:0")
	runFor(t, l, t.Context(), "m1")
	runFor(t, l, t.Context(), "m1")
	if n := len(records(t, filepath.Join(dir, ConsoleLog))); n != 2*4 {
		t.Errorf("%d records after two runs, want 8", n)
	}
}

func TestTheLauncherCapsAFlood(t *testing.T) {
	l, dir := launcherDir(t, "m1", "flood:100") // 6.4 MiB
	l.ConsoleMax = 1 << 20
	if got := runFor(t, l, t.Context(), "m1"); got != 0 {
		t.Errorf("exited %d", got)
	}
	for _, f := range []string{ConsoleLog, ConsoleLog + ".1"} {
		if s := size(t, filepath.Join(dir, f)); s > l.ConsoleMax {
			t.Errorf("%s holds %d bytes, past the cap of %d", f, s, l.ConsoleMax)
		}
		records(t, filepath.Join(dir, f)) // whole records only
	}
	if s, err := os.Stat(filepath.Join(dir, ConsoleLog+".2")); err == nil {
		t.Errorf("a third file: %v", s.Name())
	}
}

func TestTheLauncherSurvivesGarbage(t *testing.T) {
	l, dir := launcherDir(t, "m1", "garbage")
	if got := runFor(t, l, t.Context(), "m1"); got != 5 {
		t.Errorf("exited %d, want the exit after the garbage, 5", got)
	}
	recs := records(t, filepath.Join(dir, ConsoleLog))
	if len(recs) < 2 || string(recs[len(recs)-2].Payload) != "after the garbage\n" {
		t.Errorf("the records after the garbage were lost: %d records", len(recs))
	}
}

func TestTheLauncherWithoutARecord(t *testing.T) {
	l, dir := launcherDir(t, "m1", "crash")
	if got := runFor(t, l, t.Context(), "m1"); got != ExitNoRecord {
		t.Errorf("exited %d, want %d", got, ExitNoRecord)
	}
	if r, ok := exitOf(t, dir); !ok || r.Code != ExitNoRecord {
		t.Errorf("exit record %+v %t", r, ok)
	}
}

func TestTheLauncherRefusesWhatItCannotOpen(t *testing.T) {
	for _, tt := range []struct {
		name   string
		break_ func(*Launcher)
	}{
		{"root", func(l *Launcher) {
			l.OpenRoot = func(string) (*os.File, error) { return nil, errors.New("not the machine's zvol") }
		}},
		{"tap", func(l *Launcher) {
			l.OpenTap = func(driver.Interface) (*os.File, error) { return nil, errors.New("no tap") }
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l, dir := launcherDir(t, "m1", "exit:0")
			tt.break_(l)
			if got := runFor(t, l, t.Context(), "m1"); got != ExitFailed {
				t.Errorf("exited %d, want %d", got, ExitFailed)
			}
			if r, ok := exitOf(t, dir); !ok || r.Code != ExitFailed || r.Error == "" {
				t.Errorf("exit record %+v %t", r, ok)
			}
			if exists(filepath.Join(dir, "args.json")) {
				t.Error("Cloud Hypervisor was started")
			}
		})
	}
	// Nor a spec of another machine, nor a uid that names no VM.
	l, dir := launcherDir(t, "m1", "exit:0")
	if err := os.Rename(dir, filepath.Join(l.Dir, "m2")); err != nil {
		t.Fatal(err)
	}
	if got := runFor(t, l, t.Context(), "m2"); got != ExitFailed {
		t.Errorf("another machine's spec: exited %d", got)
	}
	if got := runFor(t, l, t.Context(), "../m2"); got != ExitFailed {
		t.Errorf("a uid climbing out: exited %d", got)
	}
}

// TestOpenZvolTakesOnlyZvolLinks: what is not a clean /dev/zvol link is
// refused before it is opened; the testbed tests the device check itself.
func TestOpenZvolTakesOnlyZvolLinks(t *testing.T) {
	for _, path := range []string{"/dev/null", "/dev/zvol/", "/dev/zvol", "/dev/zvol/../null", "/dev/zvol/hangar/../../null", "dev/zvol/x"} {
		if f, err := OpenZvol(path); err == nil {
			_ = f.Close()
			t.Errorf("opened %s", path)
		}
	}
}

// TestTheLauncherStops: stopped by the driver, which has marked the VM
// and asked its guest already, it kills Cloud Hypervisor at once and exits
// 0, recording no exit.
func TestTheLauncherStops(t *testing.T) {
	l, dir := launcherDir(t, "m1", "hang")
	if err := os.WriteFile(filepath.Join(dir, stopFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(500*time.Millisecond, cancel)
	start := time.Now()
	if got := runFor(t, l, ctx, "m1"); got != 0 {
		t.Errorf("exited %d, want 0", got)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v to stop", d)
	}
	if _, ok := exitOf(t, dir); ok {
		t.Error("a stop was recorded as an exit")
	}
	if g := l.Guest.(*launcherGuest).asked(); len(g) != 0 {
		t.Errorf("the guest was asked to stop again: %v", g)
	}
}

// stopBySystemd runs the launcher and stops it, as systemd does, once its
// guest has booted; it returns the code and how long the stop took.
func stopBySystemd(t *testing.T, l *Launcher, dir string) (int, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() { done <- l.Run(ctx, "m1") }()
	deadline := time.Now().Add(10 * time.Second)
	for len(records(t, filepath.Join(dir, ConsoleLog))) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the guest never booted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	cancel()
	select {
	case code := <-done:
		return code, time.Since(start)
	case <-time.After(30 * time.Second):
		t.Fatal("the launcher did not return")
		return 0, 0
	}
}

// TestTheLauncherStopsTheGuestForSystemd: stopped by systemd alone, as at
// host shutdown, the launcher asks the guest to stop with the launch's
// grace, and the container's handler writes to the log; however the
// container ended, nothing is recorded, so the VM reads as interrupted.
func TestTheLauncherStopsTheGuestForSystemd(t *testing.T) {
	l, dir := launcherDir(t, "m1", "stoppable")
	// A grace long enough to tell a guest powering off from one killed
	// after it, even under the race detector, which holds an exit for 1s.
	ln, err := readLaunch(dir)
	if err != nil {
		t.Fatal(err)
	}
	ln.StopGrace = 20 * time.Second
	b, _ := json.Marshal(ln)
	if err := os.WriteFile(filepath.Join(dir, specFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	code, took := stopBySystemd(t, l, dir)
	if code != 0 {
		t.Errorf("exited %d, want 0", code)
	}
	if took > 5*time.Second {
		t.Errorf("a guest stopping at once took %v", took)
	}
	if g := l.Guest.(*launcherGuest).asked(); !slices.Equal(g, []time.Duration{ln.StopGrace}) {
		t.Errorf("the guest was asked to stop with %v, want the launch's grace once", g)
	}
	var out []string
	for _, r := range records(t, filepath.Join(dir, ConsoleLog)) {
		if r.Stream == console.Stdout {
			out = append(out, string(r.Payload))
		}
	}
	if !slices.Equal(out, []string{"got SIGTERM\n"}) {
		t.Errorf("stdout %q, want the handler's", out)
	}
	if r, ok := exitOf(t, dir); ok {
		t.Errorf("a stop by systemd was recorded as an exit: %+v", r)
	}
}

// TestTheLauncherKillsAGuestThatStaysUp: a guest that takes the stop but
// does not power off is killed after the grace and the slack; one that
// does not take it, at once.
func TestTheLauncherKillsAGuestThatStaysUp(t *testing.T) {
	l, dir := launcherDir(t, "m1", "hang")
	l.KillGrace = 100 * time.Millisecond
	code, took := stopBySystemd(t, l, dir)
	if code != 0 {
		t.Errorf("exited %d, want 0", code)
	}
	if took < testGrace+testSlack || took > testGrace+testSlack+time.Second {
		t.Errorf("stopped in %v, want after the grace and the slack, %v", took, testGrace+testSlack)
	}
	if r, ok := exitOf(t, dir); ok {
		t.Errorf("a stop by systemd was recorded as an exit: %+v", r)
	}

	l, dir = launcherDir(t, "m1", "hang")
	l.KillGrace = 100 * time.Millisecond
	l.Guest.(*launcherGuest).err = errors.New("no answer")
	if _, took := stopBySystemd(t, l, dir); took > testGrace {
		t.Errorf("a guest not taking the stop was killed in %v", took)
	}
}

// TestTheLauncherWaitsWhileStopping: a VM being stopped that powers off
// is not left for the restart policy: the launcher waits to be stopped.
func TestTheLauncherWaitsWhileStopping(t *testing.T) {
	l, dir := launcherDir(t, "m1", "exit:143")
	if err := os.WriteFile(filepath.Join(dir, stopFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() { done <- l.Run(ctx, "m1") }()
	deadline := time.Now().Add(10 * time.Second)
	for !exists(filepath.Join(dir, exitFile)) {
		if time.Now().After(deadline) {
			t.Fatal("no exit record")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case code := <-done:
		t.Fatalf("exited %d while being stopped", code)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	if code := <-done; code != 143 {
		t.Errorf("exited %d, want 143", code)
	}
}
