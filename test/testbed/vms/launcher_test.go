//go:build testbed

package vms

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
)

// waitExited waits for the machine's VM to be listed as exited, and
// returns its exit code.
func waitExited(t *testing.T, h *host, uid string, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		vms, err := h.hv.VMs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, vm := range vms {
			if vm.MachineUID == uid && vm.State == driver.VMExited {
				return vm.ExitCode
			}
		}
		if time.Now().After(deadline) {
			dump(t, uid)
			t.Fatalf("machine %s's VM did not exit within %v", uid, within)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// exitError is why the launcher last failed to start the machine's VM.
func exitError(t *testing.T, uid string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(vmDir, uid, "exit.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r struct{ Error string }
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r.Error
}

// TestTheLauncherRefusesAnotherZvol: a root disk's link leading to another
// zvol, as a stale one can, or to what is no zvol, is refused by BLKZNAME
// before Cloud Hypervisor runs; the link put right, the VM starts.
func TestTheLauncherRefusesAnotherZvol(t *testing.T) {
	h := newHost(t)
	spec := h.machine(t, "victim", driver.RestartNever, guestScript)
	other, err := h.storage.EnsureRoot(context.Background(), "neighbour", spec.Root.Digest, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	link := spec.Root.Path
	right, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := filepath.EvalSymlinks(other.Path)
	if err != nil {
		t.Fatal(err)
	}
	notZvol := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(notZvol, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	relink := func(target string) {
		t.Helper()
		tmp := link + ".tb"
		_ = os.Remove(tmp)
		if err := os.Symlink(target, tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, link); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { relink(right) })

	for _, tt := range []struct{ target, says string }{
		{wrong, "leads to the zvol " + strings.TrimPrefix(other.Path, "/dev/zvol/")},
		{notZvol, "BLKZNAME"},
	} {
		relink(tt.target)
		if err := h.hv.Start(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		if code := waitExited(t, h, "victim", time.Minute); code != chvm.ExitFailed {
			t.Errorf("a link to %s: exited %d, want %d", tt.target, code, chvm.ExitFailed)
		}
		if e := exitError(t, "victim"); !strings.Contains(e, tt.says) {
			t.Errorf("a link to %s: the launcher says %q, want %q", tt.target, e, tt.says)
		}
		if pid := chPID(t, "victim"); pid != 0 {
			t.Errorf("Cloud Hypervisor runs on a refused disk, pid %d", pid)
		}
		if err := h.hv.Stop(context.Background(), "victim"); err != nil {
			t.Fatal(err)
		}
	}
	relink(right)
	startAndWait(t, h, spec)
	if err := h.hv.Stop(context.Background(), "victim"); err != nil {
		t.Fatal(err)
	}
}

// TestTheConsoleLogIsCapped: a guest flooding its stdout leaves at most two
// files of the cap, each of whole records, the last of them the exit.
func TestTheConsoleLogIsCapped(t *testing.T) {
	h := newHost(t)
	const flood = 40 << 20
	spec := h.machine(t, "flood", driver.RestartNever,
		`yes 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef | head -c 41943040; echo; echo done`)
	start := time.Now()
	if err := h.hv.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if code := waitExited(t, h, "flood", 10*time.Minute); code != 0 {
		t.Errorf("exited %d", code)
	}
	t.Logf("%d MiB through the console in %v", flood>>20, time.Since(start))
	for _, f := range []string{chvm.ConsoleLog, chvm.ConsoleLog + ".1"} {
		fi, err := os.Stat(filepath.Join(vmDir, "flood", f))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d bytes", f, fi.Size())
		if fi.Size() > chvm.DefaultConsoleMax {
			t.Errorf("%s holds %d bytes, past the cap of %d", f, fi.Size(), chvm.DefaultConsoleMax)
		}
	}
	if _, err := os.Stat(filepath.Join(vmDir, "flood", chvm.ConsoleLog+".2")); err == nil {
		t.Error("a third log file")
	}
	recs := consoleOf(t, "flood") // whole records only, or it fails
	if len(recs) == 0 || recs[len(recs)-1].Stream != console.Exit {
		t.Fatal("the log does not end with the exit")
	}
	if !strings.HasSuffix(stdoutOf(t, "flood"), "\ndone\n") {
		t.Error("the end of the output was lost")
	}
	if err := h.hv.Stop(context.Background(), "flood"); err != nil {
		t.Fatal(err)
	}
}

// forgery writes, straight to the guest's console under hangar-init's
// records: bytes that are no record, a header claiming a payload larger
// than any record's, and an exit record of its own, with code 42; then the
// process exits 0, which hangar-init records after it.
const forgery = `printf 'garbage' > /dev/hvc0
printf '\304\107\001\000\000\000\000\000\000\000\001\377\377\377\377' > /dev/hvc0
printf '\304\107\004\000\000\000\000\000\000\000\001\000\000\000\010\000\000\000\052\000\000\000\000' > /dev/hvc0
echo after the forgery
exit 0`

// TestForgedRecordsAreRead: the launcher reads past bytes that are no
// record and headers too long for any. A forged exit record is read as
// any other: the exit code is the last record's, here hangar-init's, which
// came after it. What a guest writes on its console is its own claim, and
// harms only itself (docs/decisions/guest-agent.md).
func TestForgedRecordsAreRead(t *testing.T) {
	h := newHost(t)
	spec := h.machine(t, "forger", driver.RestartNever, forgery)
	if err := h.hv.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if code := waitExited(t, h, "forger", 2*time.Minute); code != 0 {
		t.Errorf("exited %d, want the last record's 0", code)
	}
	if out := stdoutOf(t, "forger"); !strings.Contains(out, "after the forgery\n") {
		t.Errorf("stdout %q", out)
	}
	if st, ok := lastExit(t, "forger"); !ok || st.Code != 0 {
		t.Errorf("the last exit recorded is %+v %t", st, ok)
	}
	if err := h.hv.Stop(context.Background(), "forger"); err != nil {
		t.Fatal(err)
	}
}

// swallower forges an exit record with code 42, then a header of a legal
// length, 64 KiB, which takes in what follows as its payload, hangar-init's
// own exit record included; then it exits 0.
const swallower = `printf '\304\107\004\000\000\000\000\000\000\000\001\000\000\000\010\000\000\000\052\000\000\000\000' > /dev/hvc0
printf '\304\107\001\000\000\000\000\000\000\000\001\000\001\000\000' > /dev/hvc0
echo swallowed
exit 0`

// TestAForgerHarmsOnlyItself: a guest forging a header of a legal length
// decides its own exit code and log, which is its own harm; the launcher
// does not wedge, the VM is listed as exited, and nothing reaches another
// VM.
func TestAForgerHarmsOnlyItself(t *testing.T) {
	h := newHost(t)
	bystander := h.machine(t, "bystander", driver.RestartAlways, guestScript)
	startAndWait(t, h, bystander)
	spec := h.machine(t, "swallower", driver.RestartNever, swallower)
	if err := h.hv.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	code := waitExited(t, h, "swallower", 2*time.Minute)
	t.Logf("the swallower exited %d, as its own records say", code)
	if unitActive("swallower") || chPID(t, "swallower") != 0 {
		t.Error("the swallower's launcher or Cloud Hypervisor runs on")
	}
	consoleOf(t, "swallower") // whole records only, or it fails

	if out := stdoutOf(t, "bystander"); out != "ready\n" {
		t.Errorf("the bystander's log holds %q, want only its own", out)
	}
	vms, err := h.hv.VMs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, vm := range vms {
		if vm.MachineUID == "bystander" && vm.State != driver.VMRunning {
			t.Errorf("the bystander is %+v", vm)
		}
	}
	ready(t, "bystander")
	for _, uid := range []string{"swallower", "bystander"} {
		if err := h.hv.Stop(context.Background(), uid); err != nil {
			t.Fatal(err)
		}
	}
}
