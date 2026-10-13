package chvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// confinedLauncher is launcherDir's launcher for a VM with a VM user: the
// test's own gid, which it can give files to, and a KVM device of its group.
func confinedLauncher(t *testing.T, mode string) (*Launcher, string, int) {
	t.Helper()
	gid := os.Getgid()
	if gid == 0 {
		t.Skip("the test's group is root's, which means no VM user")
	}
	l, dir := launcherDir(t, "m1", mode)
	ln, err := readLaunch(dir)
	if err != nil {
		t.Fatal(err)
	}
	ln.VMUser = uint32(gid)
	b, _ := json.Marshal(ln)
	if err := os.WriteFile(filepath.Join(dir, specFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	l.VMUser = uint32(gid)
	l.KVM = filepath.Join(t.TempDir(), "kvm")
	if err := os.WriteFile(l.KVM, nil, 0o660); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.KVM, 0o660); err != nil {
		t.Fatal(err)
	}
	l.UnmountNetns = func() error { return nil }
	l.DropCaps = func() error { return nil }
	return l, dir, gid
}

// TestTheLauncherGivesUpRootOnceCHRuns: with a VM user, the launcher drops
// its capabilities once Cloud Hypervisor has started, and only then; if it
// cannot, it kills Cloud Hypervisor and fails.
func TestTheLauncherGivesUpRootOnceCHRuns(t *testing.T) {
	l, dir, _ := confinedLauncher(t, "stoppable")
	started := false
	// vmm-exec's record appears, while this waits, only if it already runs.
	l.DropCaps = func() error {
		for deadline := time.Now().Add(5 * time.Second); !started && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			started = exists(filepath.Join(dir, chDir, "exec.json"))
		}
		return nil
	}
	if code, _ := stopBySystemd(t, l, dir); code != 0 {
		t.Errorf("exited %d", code)
	}
	if !started {
		t.Error("the launcher dropped its capabilities before Cloud Hypervisor ran, or never")
	}
	// The stop, made with what is kept, gave the guest its SIGTERM.
	if g := l.Guest.(*launcherGuest).asked(); len(g) != 1 {
		t.Errorf("the guest was asked to stop %d times, want once", len(g))
	}

	l, dir, _ = confinedLauncher(t, "hang")
	l.DropCaps = func() error { return errors.New("capset: EPERM") }
	if got := runFor(t, l, t.Context(), "m1"); got != ExitFailed {
		t.Errorf("exited %d, want %d", got, ExitFailed)
	}
	if r, ok := exitOf(t, dir); !ok || !strings.Contains(r.Error, "capabilities") {
		t.Errorf("exit record %+v %t", r, ok)
	}
}

func modeOf(t *testing.T, path string) (os.FileMode, int) {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return os.FileMode(st.Mode & 0o7777), int(st.Gid)
}

// TestTheLauncherRunsCHAsTheVMUser: with a VM user, Cloud Hypervisor runs
// through vmm-exec as that user, with the KVM device's group, and gets the
// taps alone; what it reads in the VM's directory is the user's group's,
// and it writes only in chDir.
func TestTheLauncherRunsCHAsTheVMUser(t *testing.T) {
	l, dir, gid := confinedLauncher(t, "exit:0")
	unmounted := 0
	l.UnmountNetns = func() error { unmounted++; return nil }
	if got := runFor(t, l, t.Context(), "m1"); got != 0 {
		t.Fatalf("exited %d", got)
	}
	if unmounted != 1 {
		t.Errorf("/run/netns unmounted %d times, want once", unmounted)
	}
	var ex struct {
		UID, GID, Fds int
		Groups, Open  []int
	}
	b, err := os.ReadFile(filepath.Join(dir, chDir, "exec.json"))
	if err != nil {
		t.Fatalf("Cloud Hypervisor was not run through vmm-exec: %v", err)
	}
	if err := json.Unmarshal(b, &ex); err != nil {
		t.Fatal(err)
	}
	if ex.UID != gid || ex.GID != gid || !slices.Equal(ex.Groups, []int{gid}) || ex.Fds != 1 {
		t.Errorf("vmm-exec asked for %+v, want uid and gid %d, the KVM group, one fd", ex, gid)
	}
	if !slices.Equal(ex.Open, []int{3}) {
		t.Errorf("vmm-exec had fds %v open past stderr, want the tap's alone", ex.Open)
	}
	for name, want := range map[string]os.FileMode{
		"": 0o710, bootFile: 0o640, chDir: 0o770, serialFIFO: 0o620, specFile: 0o600,
	} {
		mode, g := modeOf(t, filepath.Join(dir, name))
		wantGID := gid
		if name == specFile {
			wantGID = os.Getgid()
		}
		if mode != want || g != wantGID {
			t.Errorf("%q has mode %o and group %d, want %o and %d", name, mode, g, want, wantGID)
		}
	}
	// Owned by the launcher alone, and not the VM's group's to read.
	for _, name := range []string{ConsoleLog, SerialLog, exitFile} {
		if mode, _ := modeOf(t, filepath.Join(dir, name)); mode&0o077 != 0 {
			t.Errorf("%s has mode %o", name, mode)
		}
	}
}

// TestTheLauncherPassesNoOtherFD: an fd the launcher was started with,
// not close-on-exec, does not reach Cloud Hypervisor, with a VM user or
// without.
func TestTheLauncherPassesNoOtherFD(t *testing.T) {
	fd, err := unix.Open(os.DevNull, unix.O_RDONLY, 0) // no O_CLOEXEC: inherited
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	check := func(t *testing.T, l *Launcher, dir string) {
		t.Helper()
		// Run clears it; set it again for each run.
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
			t.Fatal(err)
		}
		if got := runFor(t, l, t.Context(), "m1"); got != 0 {
			t.Fatalf("exited %d", got)
		}
		var ch struct{ Fds []int }
		b, _ := os.ReadFile(filepath.Join(dir, chDir, "args.json"))
		if err := json.Unmarshal(b, &ch); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(ch.Fds, []int{3}) {
			t.Errorf("Cloud Hypervisor had fds %v, want the tap's alone, not the inherited %d", ch.Fds, fd)
		}
	}
	t.Run("as root", func(t *testing.T) {
		l, dir := launcherDir(t, "m1", "exit:0")
		check(t, l, dir)
	})
	t.Run("as a VM user", func(t *testing.T) {
		l, dir, _ := confinedLauncher(t, "exit:0")
		check(t, l, dir)
	})
}

// TestTheLauncherTakesOnlyItsUnitsUser: the unit's VM user and the spec
// file's must be the same, and the spec file the launcher's alone.
func TestTheLauncherTakesOnlyItsUnitsUser(t *testing.T) {
	for name, change := range map[string]func(*Launcher, string){
		"unit without a user":   func(l *Launcher, _ string) { l.VMUser = 0 },
		"unit of another user":  func(l *Launcher, _ string) { l.VMUser++ },
		"spec others can write": func(_ *Launcher, dir string) { _ = os.Chmod(filepath.Join(dir, specFile), 0o620) },
	} {
		t.Run(name, func(t *testing.T) {
			l, dir, _ := confinedLauncher(t, "exit:0")
			change(l, dir)
			if got := runFor(t, l, t.Context(), "m1"); got != ExitFailed {
				t.Errorf("exited %d, want %d", got, ExitFailed)
			}
			if exists(filepath.Join(dir, chDir, "args.json")) {
				t.Error("Cloud Hypervisor was started")
			}
		})
	}
	// A spec of a VM user in a unit with none.
	l, dir := launcherDir(t, "m1", "exit:0")
	ln, _ := readLaunch(dir)
	ln.VMUser = 200000
	b, _ := json.Marshal(ln)
	if err := os.WriteFile(filepath.Join(dir, specFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := runFor(t, l, t.Context(), "m1"); got != ExitFailed {
		t.Errorf("a unit without a user ran a spec with one: exited %d", got)
	}
}

func TestKVMGroups(t *testing.T) {
	kvm := filepath.Join(t.TempDir(), "kvm")
	if err := os.WriteFile(kvm, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for mode, want := range map[os.FileMode]string{0o666: "", 0o660: fmt.Sprint(os.Getgid()), 0o600: "error"} {
		if err := os.Chmod(kvm, mode); err != nil {
			t.Fatal(err)
		}
		got, err := kvmGroups(kvm)
		if err != nil {
			got = "error"
		}
		if os.Getgid() == 0 && mode == 0o660 {
			want = "error" // root's group is no group to give
		}
		if got != want {
			t.Errorf("mode %o: groups %q, want %q", mode, got, want)
		}
	}
}

// driverWith is newDriver with processes running as ids, and a range of
// VM users of count.
func driverWith(t *testing.T, s *fakeSystemd, count uint32, running ...uint32) *Hypervisor {
	t.Helper()
	ids := map[uint32]bool{}
	for _, id := range running {
		ids[id] = true
	}
	g := &fakeGuest{}
	h, err := New(t.Context(), Config{
		Dir: shortDir(t), CloudHypervisor: os.Args[0], Kernel: os.Args[0], Initramfs: os.Args[0], Cmdline: "quiet",
		Launcher: []string{os.Args[0], "vmm"}, StopGrace: testGrace, StopSlack: testSlack,
		VMUsers: UserRange{First: 300000, Count: count},
		systemd: s, guest: g, dynamicOK: true, etc: testEtc(t),
		procUsers: func() (map[uint32]bool, error) { return ids, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	g.h = h
	return h
}

func userOf(t *testing.T, h *Hypervisor, uid string) uint32 {
	t.Helper()
	ln, err := readLaunch(h.dir(uid))
	if err != nil {
		t.Fatal(err)
	}
	return ln.VMUser
}

// TestVMUsers: each VM has an id of its own, the lowest free, none that a
// process runs as; it keeps it across its restarts and the host's, and
// gives it up when it is stopped.
func TestVMUsers(t *testing.T) {
	s := newFakeSystemd()
	h := driverWith(t, s, 4, 300001)
	for _, uid := range []string{"m1", "m2"} {
		if err := h.Start(t.Context(), testSpec(uid, driver.RestartAlways)); err != nil {
			t.Fatal(err)
		}
	}
	if a, b := userOf(t, h, "m1"), userOf(t, h, "m2"); a != 300000 || b != 300002 {
		t.Errorf("ids %d and %d, want 300000 and, past the one a process runs as, 300002", a, b)
	}
	if exec := fmt.Sprint(prop(s.props["hangar-vm-m2.service"], "ExecStart")); !strings.Contains(exec, "--vm-user 300002 m2") {
		t.Errorf("ExecStart=%s", exec)
	}
	// The host rebooted: the units are gone, the spec files are not.
	s.mu.Lock()
	delete(s.units, "hangar-vm-m2.service")
	s.mu.Unlock()
	if err := h.Start(t.Context(), testSpec("m2", driver.RestartAlways)); err != nil {
		t.Fatal(err)
	}
	if got := userOf(t, h, "m2"); got != 300002 {
		t.Errorf("m2 started again as %d, not its 300002", got)
	}
	if err := h.Stop(t.Context(), "m1"); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), testSpec("m3", driver.RestartAlways)); err != nil {
		t.Fatal(err)
	}
	if got := userOf(t, h, "m3"); got != 300000 {
		t.Errorf("m3 has %d, want m1's 300000, free once m1 is stopped", got)
	}
	if err := h.Start(t.Context(), testSpec("m4", driver.RestartAlways)); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), testSpec("m5", driver.RestartAlways)); err == nil {
		t.Error("m5 started with every id taken")
	} else if exists(filepath.Join(h.dir("m5"), specFile)) {
		t.Error("m5 has a spec file without an id")
	}
}

// TestTheSpecGoesLast: a stop that cannot remove all of a VM's directory
// leaves its spec file, and with it its id, held.
func TestTheSpecGoesLast(t *testing.T) {
	s := newFakeSystemd()
	h := driverWith(t, s, 4)
	if err := h.Start(t.Context(), testSpec("m1", driver.RestartAlways)); err != nil {
		t.Fatal(err)
	}
	// After spec.json, as ReadDir sorts them.
	if err := os.WriteFile(filepath.Join(h.dir("m1"), stopFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.removeAll = func(path string) error {
		if filepath.Base(path) == stopFile {
			return errors.New("busy")
		}
		return os.RemoveAll(path)
	}
	if err := h.Stop(t.Context(), "m1"); err == nil {
		t.Fatal("a stop that could not remove the VM's files succeeded")
	}
	if userOf(t, h, "m1") != 300000 {
		t.Error("the spec file went before the rest")
	}
	if err := h.Start(t.Context(), testSpec("m2", driver.RestartAlways)); err != nil {
		t.Fatal(err)
	}
	if got := userOf(t, h, "m2"); got == 300000 {
		t.Error("m2 took the id of m1, whose files are not all gone")
	}
}

// TestVMUsersAreOneEach: no VM is given an id another's spec file holds,
// even its own spec file's, if another holds it too.
func TestVMUsersAreOneEach(t *testing.T) {
	h := driverWith(t, newFakeSystemd(), 4)
	for uid, id := range map[string]uint32{"o1": 300000, "o2": 300001, "m1": 300001} {
		if err := os.MkdirAll(h.dir(uid), 0o700); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(launch{Spec: testSpec(uid, driver.RestartAlways), VMUser: id})
		if err := os.WriteFile(filepath.Join(h.dir(uid), specFile), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, uid := range []string{"m1", "m2"} {
		if err := h.Start(t.Context(), testSpec(uid, driver.RestartAlways)); err != nil {
			t.Fatal(err)
		}
	}
	if a, b := userOf(t, h, "m1"), userOf(t, h, "m2"); a != 300002 || b != 300003 {
		t.Errorf("ids %d and %d, want 300002 and 300003: the others are held", a, b)
	}
}

// TestVMUsersAreLockedAcrossDrivers: a Start waits for the lock on the
// VMs' directory, which another driver, as another process, may hold.
func TestVMUsersAreLockedAcrossDrivers(t *testing.T) {
	h := driverWith(t, newFakeSystemd(), 4)
	lock, err := os.Open(h.cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	// Shared: an exclusive lock waits for it, a shared one would not.
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.Start(context.Background(), testSpec("m1", driver.RestartAlways)) }()
	select {
	case err := <-done:
		t.Fatalf("Start went past another's lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if exists(filepath.Join(h.dir("m1"), specFile)) {
		t.Error("a spec file was written under another's lock")
	}
	_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not go on once the lock was let go")
	}
}

// TestAnUnreadableSpecHoldsItsUser: an id is given out only once every
// spec file that might hold it is read.
func TestAnUnreadableSpecHoldsItsUser(t *testing.T) {
	h := driverWith(t, newFakeSystemd(), 4)
	if err := os.MkdirAll(h.dir("m1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir("m1"), specFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), testSpec("m2", driver.RestartAlways)); err == nil {
		t.Error("started beside a spec file that cannot be read")
	}
}

// TestStartConfinesTheUnit: the unit sees an empty root with the VM's
// directory, the launcher, Cloud Hypervisor, the kernel and the initramfs
// bound into it, and opens no device but KVM, the taps' and the root disk.
func TestStartConfinesTheUnit(t *testing.T) {
	s := newFakeSystemd()
	h := newDriver(t, shortDir(t), s, &fakeGuest{})
	spec := testSpec("m1", driver.RestartAlways)
	if err := h.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	props := s.props["hangar-vm-m1.service"]
	if got := fmt.Sprint(prop(props, "TemporaryFileSystem")); got != "[{/ ro} {/run ro}]" {
		t.Errorf("TemporaryFileSystem=%s, want an empty root", got)
	}
	if got := fmt.Sprint(prop(props, "BindPaths")); got != fmt.Sprintf("[{%s %s false 0}]", h.dir("m1"), h.dir("m1")) {
		t.Errorf("BindPaths=%s, want the VM's directory alone", got)
	}
	ro := fmt.Sprint(prop(props, "BindReadOnlyPaths"))
	for _, want := range []string{"{/run/netns /run/netns true 16384}", fmt.Sprintf("{%s %s false 0}", os.Args[0], os.Args[0])} {
		if !strings.Contains(ro, want) {
			t.Errorf("BindReadOnlyPaths=%s lacks %s", ro, want)
		}
	}
	if got := fmt.Sprint(prop(props, "DeviceAllow")); got != "[{/dev/kvm rw} {/dev/net/tun rw} {/dev/zvol/hangar/roots/m1 rwm}]" {
		t.Errorf("DeviceAllow=%s", got)
	}
	for name, want := range map[string]any{
		"DevicePolicy": "closed", "NoNewPrivileges": true, "PrivateNetwork": true, "ProtectProc": "invisible",
		"MountAPIVFS": true, "AmbientCapabilities": uint64(0), "LimitCORE": uint64(0),
		"MemoryMax": vmMemoryMax(512), "TasksMax": uint64(vmTasks + 4),
	} {
		if got := prop(props, name); got != want {
			t.Errorf("%s=%v, want %v", name, got, want)
		}
	}
	caps, _ := prop(props, "CapabilityBoundingSet").(uint64)
	for _, c := range []uintptr{unix.CAP_SYS_PTRACE, unix.CAP_SYS_MODULE, unix.CAP_SYS_RAWIO, unix.CAP_DAC_READ_SEARCH, unix.CAP_FOWNER, unix.CAP_NET_RAW} {
		if caps&(1<<c) != 0 {
			t.Errorf("the unit keeps capability %d", c)
		}
	}
	for _, p := range props {
		if p.Name == "ProtectSystem" || p.Name == "PrivateTmp" || p.Name == "ProtectHome" {
			t.Errorf("%s would bind the host's root, a writable directory, or nothing of /home into the empty one", p.Name)
		}
	}
}

// TestMemoryMax: the guest's memory, 256 MiB, and 2% of the guest's.
func TestMemoryMax(t *testing.T) {
	for mib, want := range map[int64]uint64{
		256:   256<<20 + 256<<20 + (256<<20)/50,
		1024:  1024<<20 + 256<<20 + 21474836, // 2% of 1 GiB, rounded down
		16384: 16384<<20 + 256<<20 + 343597383,
	} {
		if got := vmMemoryMax(mib); got != want {
			t.Errorf("MemoryMax of a %d MiB guest: %d, want %d", mib, got, want)
		}
	}
	// Past the guest's, at least what was measured with its memory full
	// (docs/decisions/node-agent.md), many times over.
	if extra := vmMemoryMax(16384) - 16384<<20; extra < 512<<20 {
		t.Errorf("a 16 GiB guest's unit has %d MiB past it", extra>>20)
	}
}

// TestSocketOfALegacyVM: a VM a launcher without chDir started is reached
// at its socket until it is started again.
func TestSocketOfALegacyVM(t *testing.T) {
	h := newDriver(t, shortDir(t), newFakeSystemd(), &fakeGuest{})
	if got, want := h.Socket("m1"), filepath.Join(h.dir("m1"), chDir, "vsock"); got != want {
		t.Errorf("socket %s, want %s", got, want)
	}
	if err := os.MkdirAll(h.dir("m1"), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(h.dir("m1"), legacyVsockFile)
	ln, err := net.Listen("unix", legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if got := h.Socket("m1"); got != legacy {
		t.Errorf("socket %s, want the legacy %s", got, legacy)
	}
	_ = ln.Close()

	// Only a socket there is a legacy VM's: not a link to one, a FIFO or
	// a file.
	other := filepath.Join(t.TempDir(), "other")
	oln, err := net.Listen("unix", other)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = oln.Close() }()
	for name, plant := range map[string]func() error{
		"a link to a socket": func() error { return os.Symlink(other, legacy) },
		"a FIFO":             func() error { return unix.Mkfifo(legacy, 0o600) },
		"a file":             func() error { return os.WriteFile(legacy, nil, 0o600) },
	} {
		_ = os.Remove(legacy)
		if err := plant(); err != nil {
			t.Fatal(err)
		}
		if got, want := h.Socket("m1"), filepath.Join(h.dir("m1"), chDir, "vsock"); got != want {
			t.Errorf("%s at the legacy socket's path: socket %s, want %s", name, got, want)
		}
	}
}

func TestNewRefusesADynamicLauncher(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil || checkStatic(sh) == nil {
		t.Skip("no dynamically linked sh")
	}
	_, err = New(t.Context(), Config{Dir: shortDir(t), CloudHypervisor: sh, Kernel: sh, Initramfs: sh,
		Launcher: []string{sh, "vmm"}, systemd: newFakeSystemd(), guest: &fakeGuest{}, etc: testEtc(t)})
	if err == nil || !strings.Contains(err.Error(), "dynamically linked") {
		t.Errorf("took a dynamically linked launcher: %v", err)
	}
	_, err = New(t.Context(), Config{Dir: shortDir(t), CloudHypervisor: sh, Kernel: sh, Initramfs: sh,
		Launcher: []string{"hangar-node", "vmm"}, systemd: newFakeSystemd(), guest: &fakeGuest{}, dynamicOK: true, etc: testEtc(t)})
	if err == nil {
		t.Error("took a launcher by a relative path")
	}
}

// TestVMUsersAreNoOnesElse: the driver refuses a range of ids a user or
// group of the host has one of, or a subordinate range overlaps, but the
// one deploy/host keeps for the VMs.
func TestVMUsersAreNoOnesElse(t *testing.T) {
	r := UserRange{First: 200000, Count: 65536}
	base := map[string]string{
		"passwd": "root:x:0:0:root:/root:/bin/bash\nws:x:1000:1000::/home/ws:/bin/bash\n",
		"group":  "root:x:0:\nkvm:x:993:\n",
		"subuid": "ws:100000:65536\nhangar-vms:200000:65536\n",
		"subgid": "ws:100000:65536\nhangar-vms:200000:65536\n",
	}
	write := func(t *testing.T, files map[string]string) string {
		dir := t.TempDir()
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	if err := r.checkFree(write(t, base)); err != nil {
		t.Errorf("refused a host with the range kept: %v", err)
	}
	for name, change := range map[string][2]string{
		"a user's uid":      {"passwd", "vm:x:200005:100::/:/bin/false\n"},
		"a user's gid":      {"passwd", "vm:x:100:265535::/:/bin/false\n"},
		"a group":           {"group", "vms:x:230000:\n"},
		"a subuid range":    {"subuid", "alice:165536:65536\n"},
		"a subgid range":    {"subgid", "bob:265535:10\n"},
		"a range around it": {"subuid", "carol:100000:600000\n"},
	} {
		files := map[string]string{}
		for k, v := range base {
			files[k] = v
		}
		files[change[0]] += change[1]
		if err := r.checkFree(write(t, files)); err == nil {
			t.Errorf("%s in the range: taken", name)
		}
	}
}

func TestStatusIDs(t *testing.T) {
	ids := map[uint32]bool{}
	status := "Name:\tcloud-hyperviso\nUid:\t200000\t200000\t200000\t200001\nGid:\t200002\t200002\t200002\t200002\nGroups:\t108\n"
	if err := statusIDs([]byte(status), ids); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || !ids[200000] || !ids[200001] || !ids[200002] {
		t.Errorf("ids %v", ids)
	}
	if err := statusIDs([]byte("Name:\tx\n"), ids); err == nil {
		t.Error("took a status without ids")
	}
}

func TestParseExec(t *testing.T) {
	c, argv, err := parseExec([]string{"--uid", "200000", "--gid", "200000", "--groups", "108", "--fds", "2", "--", "/ch", "--kernel", "k"})
	if err != nil || c.uid != 200000 || c.gid != 200000 || !slices.Equal(c.groups, []int{108}) || c.fds != 2 ||
		!slices.Equal(argv, []string{"/ch", "--kernel", "k"}) {
		t.Errorf("%+v %q %v", c, argv, err)
	}
	for _, args := range [][]string{
		{"--uid", "0", "--gid", "1", "--fds", "0", "--", "/ch"},
		{"--uid", "1", "--gid", "0", "--fds", "0", "--", "/ch"},
		{"--uid", "1", "--gid", "1", "--fds", "0"},
		{"--uid", "1", "--gid", "1", "--", "/ch"},
		{"--uid", "1", "--gid", "1", "--groups", "0", "--fds", "0", "--", "/ch"},
	} {
		if _, _, err := parseExec(args); err == nil {
			t.Errorf("took %q", args)
		}
	}
}

func TestCheckConfined(t *testing.T) {
	c := execCreds{uid: 200000, gid: 200000, groups: []int{108}}
	ok := "Uid:\t200000\t200000\t200000\t200000\nGid:\t200000\t200000\t200000\t200000\nGroups:\t108 \n" +
		"CapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t0000000000000000\n" +
		"CapBnd:\t0000000000000000\nCapAmb:\t0000000000000000\nNoNewPrivs:\t1\n"
	if err := checkConfined([]byte(ok), c); err != nil {
		t.Errorf("refused %v", err)
	}
	for _, bad := range []struct{ from, to string }{
		{"CapBnd:\t0000000000000000", "CapBnd:\t000001ffffffffff"},
		{"CapEff:\t0000000000000000", "CapEff:\t0000000000000400"},
		{"NoNewPrivs:\t1", "NoNewPrivs:\t0"},
		{"Uid:\t200000\t200000\t200000\t200000", "Uid:\t200000\t0\t200000\t200000"},
		{"Groups:\t108 ", "Groups:\t108 6"},
		{"CapAmb:\t0000000000000000\n", ""},
	} {
		if err := checkConfined([]byte(strings.Replace(ok, bad.from, bad.to, 1)), c); err == nil {
			t.Errorf("took %q", bad.to)
		}
	}
}

// TestExecMainConfines runs vmm-exec for real, as root: what it runs has
// the ids asked, no capability, no_new_privs, no environment, and no fd
// past those it was told to pass on.
func TestExecMainConfines(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (make test-root)")
	}
	pass, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pass.Close() }()
	leak, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = leak.Close() }()
	cmd := exec.Command(os.Args[0], "vmm-exec", "--uid", "65534", "--gid", "65533", "--groups", "65532", "--fds", "1", "--",
		"/bin/sh", "-c", "cat /proc/self/status; env; echo fds; ls /proc/$$/fd")
	cmd.Env = []string{realExecEnv + "=1", "SECRET=1"}
	cmd.ExtraFiles = []*os.File{pass, leak}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	status, rest, _ := strings.Cut(string(out), "fds\n")
	if err := checkConfined([]byte(status), execCreds{uid: 65534, gid: 65533, groups: []int{65532}}); err != nil {
		t.Errorf("%v:\n%s", err, status)
	}
	if strings.Contains(status, "SECRET") {
		t.Error("the environment was passed on")
	}
	fds := strings.Fields(rest)
	if !slices.Contains(fds, "3") || slices.Contains(fds, "4") {
		t.Errorf("fds %v, want 3 passed on and 4 not", fds)
	}
}
