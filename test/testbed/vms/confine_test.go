//go:build testbed

package vms

import (
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// probeResult is what one of chprobe's tries did.
type probeResult struct {
	OK     bool
	Detail string
}

// vmUserOf is the id the machine's VM runs as, from its spec file.
func vmUserOf(t *testing.T, uid string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(vmDir, uid, "spec.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ln struct{ VMUser int }
	if err := json.Unmarshal(b, &ln); err != nil {
		t.Fatal(err)
	}
	return ln.VMUser
}

// statusOf is a process's /proc status, by key.
func statusOf(t *testing.T, pid int) map[string]string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	st := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(line, ":")
		st[k] = strings.Join(strings.Fields(v), " ")
	}
	return st
}

// TestCHIsConfined shows the confinement from inside Cloud Hypervisor's
// context. One VM, b, boots as any does; the other, a, runs chprobe in
// Cloud Hypervisor's place, which its launcher runs as it runs Cloud
// Hypervisor: as a's VM user, in a's unit. The probe tries, from there, the
// host's files, b's directory, socket, zvol and process, devices,
// privileges and the network. From the host, b's real Cloud Hypervisor is
// checked to run as b's user with no capability, and to see no more of the
// host than its unit binds; a root process in a's cgroup cannot open b's
// zvol, nor can a's user outside any unit reach what is b's.
func TestCHIsConfined(t *testing.T) {
	probe := vmtest.Artifacts().CHProbe
	if err := chvmStatic(probe); err != nil {
		t.Fatalf("%v (make e2e-host builds bin/ch-probe)", err)
	}
	h := newHost(t)
	ctx := context.Background()
	b := h.machine(t, "conf-b", driver.RestartNever, guestScript)
	startAndWait(t, h, b)
	bPID := chPID(t, "conf-b")
	if bPID == 0 {
		t.Fatal("no Cloud Hypervisor for conf-b")
	}

	in := vmtest.Artifacts()
	probing, err := chvm.New(ctx, chvm.Config{
		Dir: vmDir, CloudHypervisor: probe, Kernel: in.Kernel, Initramfs: in.Initramfs, Cmdline: in.Cmdline,
		Launcher: []string{in.Node, "vmm"}, StopGrace: testGrace,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probing.Close() }()
	a := h.machine(t, "conf-a", driver.RestartNever, guestScript)
	if err := probing.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probing.Stop(ctx, "conf-a") }()
	aCh := filepath.Join(vmDir, "conf-a", "ch")
	waitFile(t, "conf-a", filepath.Join(aCh, "ready"))
	aUser, bUser := vmUserOf(t, "conf-a"), vmUserOf(t, "conf-b")
	if aUser == bUser || !between(aUser) || !between(bUser) {
		t.Fatalf("VM users %d and %d: not two of the range", aUser, bUser)
	}

	// The checkout, under /home on a testbed, whose bin/ the unit binds.
	checkout := filepath.Dir(filepath.Dir(in.Node))
	targets, _ := json.Marshal(map[string]any{
		"OtherDir": filepath.Join(vmDir, "conf-b"), "OtherSocket": filepath.Join(vmDir, "conf-b", "ch", "vsock"),
		"OtherZvol": b.Root.Path, "OtherPID": bPID, "OwnZvol": a.Root.Path,
		"HostPaths": []string{
			"/etc", "/etc/passwd", "/etc/hangar", "/root/.bashrc", "/home/ws/.profile", filepath.Join(checkout, "go.mod"),
			"/var/lib/hangar", "/var/lib/hangar/images", "/var/lib/hangar/vms", decoyDir, imageRoot,
			"/usr/bin", "/usr/lib", "/bin/sh", "/boot", "/srv", "/opt", "/mnt", "/tmp", "/var/log",
			"/run/systemd/private", "/run/systemd/journal/socket", "/run/dbus/system_bus_socket", "/run/lock",
			"/proc/kcore", "/proc/sys/kernel", "/proc/sysrq-trigger", "/proc/1/environ",
		},
		"PlantLink": filepath.Join(vmDir, "conf-b", "ch", "vsock"),
	})
	if err := os.WriteFile(filepath.Join(aCh, "targets.json"), targets, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(aCh, "probe.json")
	waitFile(t, "conf-a", out)
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var results map[string]probeResult
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for name, r := range results {
		mark := "ok  "
		if !r.OK {
			mark = "FAIL"
			t.Errorf("from inside a's Cloud Hypervisor: %s: %s", name, r.Detail)
		}
		lines = append(lines, fmt.Sprintf("%s %-48s %s", mark, name, r.Detail))
	}
	sort.Strings(lines)
	t.Logf("--- from inside a's Cloud Hypervisor (uid %d), %d tries:\n%s", aUser, len(results), strings.Join(lines, "\n"))
	if got := results["status Uid"].Detail; got != strings.Repeat(fmt.Sprint(aUser)+" ", 3)+fmt.Sprint(aUser) {
		t.Errorf("the probe ran as %q, not as a's VM user %d", got, aUser)
	}
	if len(results) < 40 {
		t.Errorf("only %d tries came back", len(results))
	}

	// The link a's Cloud Hypervisor left at its socket's path leads to b's
	// socket, as root sees it; the driver, root, does not follow it.
	planted := filepath.Join(aCh, "vsock")
	if c, err := net.Dial("unix", planted); err != nil {
		t.Errorf("the planted link does not reach b's socket, so the check below proves nothing: %v", err)
	} else {
		_ = c.Close()
	}
	if got := probing.Socket("conf-a"); got != planted {
		t.Errorf("the driver's socket for a is %s, not %s", got, planted)
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = probing.Guest().Ready(pctx, "conf-a")
	cancel()
	if !errors.Is(err, chguest.ErrNotSocket) {
		t.Errorf("the driver reaching a's guest through the planted link: %v, want ErrNotSocket", err)
	} else {
		t.Logf("the driver reaching a's guest, a link to b's socket planted at a's: %v", err)
	}

	// a's launcher, once its Cloud Hypervisor runs, keeps no capability
	// but CAP_DAC_OVERRIDE and CAP_KILL, on every thread.
	launcherCaps(t, "conf-a")
	launcherCaps(t, "conf-b")

	// The host's /dev is read-only in the unit, even to root there.
	devRoot := fmt.Sprintf("/proc/%d/root/dev", mainPID(t, "conf-b"))
	if err := os.WriteFile(filepath.Join(devRoot, "hangar-probe"), nil, 0o600); !errors.Is(err, syscall.EROFS) {
		t.Errorf("root, in b's unit, creating a file in /dev: %v, want EROFS", err)
		_ = os.Remove(filepath.Join(devRoot, "hangar-probe"))
	}
	if err := syscall.Mknod(filepath.Join(devRoot, "hangar-probe"), syscall.S_IFBLK|0o600, 0); !errors.Is(err, syscall.EROFS) {
		t.Errorf("root, in b's unit, making a node in /dev: %v, want EROFS", err)
		_ = os.Remove(filepath.Join(devRoot, "hangar-probe"))
	}
	if _, err := os.Stat("/dev/hangar-probe"); err == nil {
		t.Error("the host's /dev holds what root in a unit made")
	}

	// b's real Cloud Hypervisor, from the host.
	st := statusOf(t, bPID)
	want := map[string]string{
		"Uid": strings.Repeat(fmt.Sprint(bUser)+" ", 3) + fmt.Sprint(bUser), "Gid": strings.Repeat(fmt.Sprint(bUser)+" ", 3) + fmt.Sprint(bUser),
		"CapInh": "0000000000000000", "CapPrm": "0000000000000000", "CapEff": "0000000000000000",
		"CapBnd": "0000000000000000", "CapAmb": "0000000000000000", "NoNewPrivs": "1", "Seccomp": "2",
	}
	for k, v := range want {
		if st[k] != v {
			t.Errorf("b's Cloud Hypervisor: %s is %q, want %q", k, st[k], v)
		}
	}
	t.Logf("b's Cloud Hypervisor (pid %d): Uid %s, Gid %s, Groups %q, CapEff %s, CapBnd %s, NoNewPrivs %s, Seccomp %s, Seccomp_filters %s",
		bPID, st["Uid"], st["Gid"], st["Groups"], st["CapEff"], st["CapBnd"], st["NoNewPrivs"], st["Seccomp"], st["Seccomp_filters"])
	// Its root, as it sees it: nothing of the host but what is bound.
	root := fmt.Sprintf("/proc/%d/root", bPID)
	var top []string
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		top = append(top, e.Name())
	}
	t.Logf("b's Cloud Hypervisor sees / as: %v", top)
	for _, name := range top {
		if !slices.Contains([]string{"dev", "proc", "sys", "run", "home", "root", "usr", "var"}, name) {
			t.Errorf("b's Cloud Hypervisor sees /%s", name)
		}
	}
	for _, p := range []string{"/etc", "/var/lib", "/usr/bin", decoyDir, filepath.Join(vmDir, "conf-a")} {
		if _, err := os.Lstat(root + p); err == nil {
			t.Errorf("b's Cloud Hypervisor sees %s", p)
		}
	}
	var vms []string
	entries, _ = os.ReadDir(root + vmDir)
	for _, e := range entries {
		vms = append(vms, e.Name())
	}
	if !slices.Equal(vms, []string{"conf-b"}) {
		t.Errorf("b's Cloud Hypervisor sees %v in %s, want its own directory alone", vms, vmDir)
	}
	mounts, _ := os.ReadFile(fmt.Sprintf("/proc/%d/mountinfo", bPID))
	t.Logf("b's Cloud Hypervisor's mounts:\n%s", mountPoints(string(mounts)))

	// The device policy, apart from the files' modes: root in a's cgroup
	// cannot open b's zvol, which root in b's can.
	if out, err := inCgroup(t, "conf-a", b.Root.Path); err == nil || !strings.Contains(out, "Operation not permitted") {
		t.Errorf("root in a's cgroup opened b's zvol: %v: %s", err, out)
	} else {
		t.Logf("root in a's cgroup, opening b's zvol: %s", strings.TrimSpace(out))
	}
	if out, err := inCgroup(t, "conf-b", b.Root.Path); err != nil {
		t.Errorf("root in b's cgroup could not open b's zvol, so the check above proves nothing: %v: %s", err, out)
	}

	// The uid alone, outside any unit: a's user reaches nothing of b's.
	for name, cmd := range map[string]string{
		"read b's spec file":          "cat " + filepath.Join(vmDir, "conf-b", "spec.json"),
		"list b's directory":          "ls " + filepath.Join(vmDir, "conf-b"),
		"reach b's socket":            "test -e " + filepath.Join(vmDir, "conf-b", "ch", "vsock"),
		"read b's zvol":               "head -c 512 " + b.Root.Path,
		"read b's console log":        "cat " + filepath.Join(vmDir, "conf-b", chvm.ConsoleLog),
		"signal b's Cloud Hypervisor": fmt.Sprintf("kill -0 %d", bPID),
	} {
		c := exec.Command("/bin/sh", "-c", cmd+" >/dev/null")
		c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(aUser), Gid: uint32(aUser), Groups: []uint32{}}}
		if out, err := c.CombinedOutput(); err == nil {
			t.Errorf("a's user, outside its unit, could %s", name)
		} else {
			t.Logf("a's user, outside its unit, %s: %s", name, strings.TrimSpace(string(out)))
		}
	}
	c := exec.Command("/bin/sh", "-c", "exec 3<>/dev/null")
	c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(aUser), Gid: uint32(aUser), Groups: []uint32{}}}
	if out, err := c.CombinedOutput(); err != nil {
		t.Errorf("a's user cannot run a shell, so the tries above prove nothing: %v: %s", err, out)
	}

	// What systemd makes of b's unit. It scores the unit as its main
	// process, the launcher, root with the capabilities it needs, and sees
	// no empty root: it counts ProtectSystem, ProtectHome, PrivateTmp and
	// RootDirectory against it. A change that loosens the unit raises it.
	sec, _ := exec.Command("systemd-analyze", "security", "--no-pager", "hangar-vm-conf-b.service").CombinedOutput()
	t.Logf("systemd-analyze security hangar-vm-conf-b.service:\n%s", sec)
	if m := regexp.MustCompile(`Overall exposure level for \S+: ([0-9.]+)`).FindSubmatch(sec); m == nil {
		t.Error("systemd-analyze gave no exposure level")
	} else if level, _ := strconv.ParseFloat(string(m[1]), 64); level > 3.2 {
		t.Errorf("exposure level %.1f, past the 3.2 the unit had", level)
	}
	for _, p := range []string{"MemoryPeak", "MemoryMax", "TasksCurrent", "TasksMax"} {
		v, _ := exec.Command("systemctl", "show", "-P", p, "hangar-vm-conf-b.service").Output()
		t.Logf("hangar-vm-conf-b.service %s=%s", p, strings.TrimSpace(string(v)))
	}

	// With only what its launcher kept, b still stops as a container
	// does: its process gets SIGTERM, and writes to the log.
	if err := h.hv.Stop(ctx, "conf-b"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdoutOf(t, "conf-b"), "got SIGTERM") {
		t.Error("b's stop gave its container no SIGTERM")
	}
}

func between(id int) bool {
	return id >= int(chvm.DefaultVMUsers.First) && id < int(chvm.DefaultVMUsers.First+chvm.DefaultVMUsers.Count)
}

// launcherCaps checks the capabilities of every thread of the machine's
// launcher: CAP_DAC_OVERRIDE and CAP_KILL in the bounding, permitted and
// effective sets, and nothing else.
func launcherCaps(t *testing.T, uid string) {
	t.Helper()
	pid := mainPID(t, uid)
	tasks, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"CapBnd": "0000000000000022", "CapPrm": "0000000000000022", "CapEff": "0000000000000022",
		"CapInh": "0000000000000000", "CapAmb": "0000000000000000",
	}
	for _, task := range tasks {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/status", pid, task.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			k, v, _ := strings.Cut(line, ":")
			if w, ok := want[k]; ok && strings.TrimSpace(v) != w {
				t.Errorf("%s's launcher, thread %s: %s is %s, want %s", uid, task.Name(), k, strings.TrimSpace(v), w)
			}
		}
	}
	t.Logf("%s's launcher (pid %d), its %d threads: CapBnd, CapPrm and CapEff %s (CAP_DAC_OVERRIDE, CAP_KILL)", uid, pid, len(tasks), want["CapEff"])
}

// waitFile waits for a file the machine's VM writes.
func waitFile(t *testing.T, uid, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			dump(t, uid)
			t.Fatalf("no %s", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// inCgroup tries to open path from a root process put in the machine's
// unit's cgroup.
func inCgroup(t *testing.T, uid, path string) (string, error) {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", mainPID(t, uid)))
	cg, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "0::")
	if err != nil || !ok || !strings.HasSuffix(cg, "/hangar-vm-"+uid+".service") {
		t.Fatalf("no cgroup for %s: %q %v", uid, b, err)
	}
	procs := filepath.Join("/sys/fs/cgroup", cg, "cgroup.procs")
	out, err := exec.Command("/bin/sh", "-c", fmt.Sprintf("echo $$ > %s && exec head -c 512 %s >/dev/null", procs, path)).CombinedOutput()
	return string(out), err
}

// mountPoints is the mount points of a mountinfo, one a line, with their
// options.
func mountPoints(mountinfo string) string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(mountinfo), "\n") {
		f := strings.Fields(line)
		if len(f) > 5 {
			out = append(out, f[4]+" "+f[5])
		}
	}
	return strings.Join(out, "\n")
}

// chvmStatic checks that path is there and needs no dynamic loader.
func chvmStatic(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("%s is dynamically linked", path)
		}
	}
	return nil
}
