//go:build testbed

package vms

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
)

// vmOf is the machine's VM as the driver lists it.
func vmOf(t *testing.T, h *host, uid string) (driver.VM, bool) {
	t.Helper()
	vms, err := h.hv.VMs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, vm := range vms {
		if vm.MachineUID == uid {
			return vm, true
		}
	}
	return driver.VM{}, false
}

// interrupt takes the machine's unit away as a reboot does: no stop asked
// of the driver, so no stop mark, and the guest never records an exit.
func interrupt(t *testing.T, uid string) {
	t.Helper()
	unit := "hangar-vm-" + uid + ".service"
	mustRun(t, "systemctl", "stop", unit)
	_ = exec.Command("systemctl", "reset-failed", unit).Run()
	for _, f := range []string{"exit.json", "stopping"} {
		if _, err := os.Stat(filepath.Join(vmDir, uid, f)); err == nil {
			t.Fatalf("the interruption left %s", f)
		}
	}
}

// TestInterruptedVMs: a VM interrupted with no record, as at a reboot,
// starts again under always and on-failure (it is not listed, and the
// agent starts it); under never it has exited, with ExitNoRecord. So does
// a VM whose launcher is killed outright (decision 16).
func TestInterruptedVMs(t *testing.T) {
	h := newHost(t)
	for _, tt := range []struct {
		name   string
		policy driver.RestartPolicy
	}{
		{"always", driver.RestartAlways},
		{"on-failure", driver.RestartOnFailure},
		{"never", driver.RestartNever},
	} {
		t.Run(tt.name, func(t *testing.T) {
			uid := "interrupted-" + tt.name
			spec := h.machine(t, uid, tt.policy, guestScript)
			startAndWait(t, h, spec)
			interrupt(t, uid)
			vm, ok := vmOf(t, h, uid)
			if tt.policy == driver.RestartNever {
				if !ok || vm.State != driver.VMExited || vm.ExitCode != chvm.ExitNoRecord || vm.SpecHash != spec.Hash() {
					t.Fatalf("listed %+v %t, want exited with %d", vm, ok, chvm.ExitNoRecord)
				}
			} else if ok {
				t.Fatalf("listed %+v: the agent would not start it again", vm)
			}
			// As the agent does: start what is not listed, or what has
			// exited and is wanted.
			startAndWait(t, h, spec)
			if vm, ok := vmOf(t, h, uid); !ok || vm.State != driver.VMRunning {
				t.Errorf("started again: %+v %t", vm, ok)
			}
			if err := h.hv.Stop(context.Background(), uid); err != nil {
				t.Fatal(err)
			}
		})
	}

	// A launcher killed outright: systemd starts it again under always;
	// under never the VM has exited, with no record.
	for _, tt := range []struct {
		name   string
		policy driver.RestartPolicy
	}{
		{"killed-always", driver.RestartAlways},
		{"killed-never", driver.RestartNever},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := h.machine(t, tt.name, tt.policy, guestScript)
			startAndWait(t, h, spec)
			first, _ := vmOf(t, h, tt.name)
			if err := syscall.Kill(mainPID(t, tt.name), syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Minute)
			for {
				vm, ok := vmOf(t, h, tt.name)
				if tt.policy == driver.RestartNever && ok && vm.State == driver.VMExited {
					if vm.ExitCode != chvm.ExitNoRecord {
						t.Errorf("exited %d, want %d", vm.ExitCode, chvm.ExitNoRecord)
					}
					break
				}
				if tt.policy == driver.RestartAlways && ok && vm.State == driver.VMRunning && vm.Started.After(first.Started) {
					break
				}
				if time.Now().After(deadline) {
					dump(t, tt.name)
					t.Fatalf("listed %+v %t", vm, ok)
				}
				time.Sleep(100 * time.Millisecond)
			}
			if err := h.hv.Stop(context.Background(), tt.name); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestTheDriverReconnectsToSystemd: once its connection to systemd is
// lost, the driver connects again, once, and still hears a VM exit.
func TestTheDriverReconnectsToSystemd(t *testing.T) {
	h := newHost(t)
	spec := h.machine(t, "reconnect", driver.RestartAlways, guestScript)
	startAndWait(t, h, spec)
	before := h.hv.SystemdConnects()
	h.hv.BreakSystemd()
	// The watch notices within 5s; a connection that ended with the
	// context of its dial would be made again every 5s.
	time.Sleep(20 * time.Second)
	if got := h.hv.SystemdConnects(); got != before+1 {
		t.Errorf("%d connections to systemd after one was lost, want %d", got, before+1)
	}
	first, _ := vmOf(t, h, "reconnect")
	for len(h.hv.Changes()) > 0 {
		<-h.hv.Changes()
	}
	exit(t, "reconnect", 3)
	select {
	case <-h.hv.Changes():
	case <-time.After(30 * time.Second):
		t.Fatal("the VM's exit was not heard over the new connection")
	}
	deadline := time.Now().Add(time.Minute)
	for {
		if vm, ok := vmOf(t, h, "reconnect"); ok && vm.State == driver.VMRunning && vm.Started.After(first.Started) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the VM was not started again")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := h.hv.Stop(context.Background(), "reconnect"); err != nil {
		t.Fatal(err)
	}
}

// heartbeat floods the serial console with 64 KiB at a time, and says on
// stdout each time it has.
const heartbeat = `trap 'exit 0' TERM
echo ready
i=0
while :; do
	head -c 65536 /dev/zero | tr '\0' s > /dev/ttyS0
	i=$((i+1))
	echo beat $i
	sleep 0.1
done`

func beats(t *testing.T, uid string) int {
	t.Helper()
	return strings.Count(stdoutOf(t, uid), "beat ")
}

// TestAFailingSerialLogDoesNotFreezeTheGuest: with the VM's directory made
// immutable, the serial log cannot rotate and fails; its output is
// dropped, the guest writing it runs on, and once the directory is
// writable again the log rotates and is written again.
func TestAFailingSerialLogDoesNotFreezeTheGuest(t *testing.T) {
	h := newHost(t)
	spec := h.machine(t, "heartbeat", driver.RestartAlways, heartbeat)
	dir := filepath.Join(vmDir, "heartbeat")
	// The serial console carries about 50 KB/s: a log left near its cap
	// by an earlier run, which the launcher appends to, rotates within
	// seconds.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, chvm.SerialLog), make([]byte, chvm.DefaultSerialMax-200<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("chattr", "-i", dir).Run() })
	startAndWait(t, h, spec)
	mustRun(t, "chattr", "+i", dir)
	failed := func() bool {
		out, _ := exec.Command("journalctl", "--no-pager", "-u", "hangar-vm-heartbeat.service", "-g", "the serial log fails").Output()
		return strings.Contains(string(out), "the serial log fails")
	}
	deadline := time.Now().Add(time.Minute)
	for !failed() {
		if time.Now().After(deadline) {
			dump(t, "heartbeat")
			t.Fatal("the serial log never failed to rotate")
		}
		time.Sleep(200 * time.Millisecond)
	}
	b1 := beats(t, "heartbeat")
	time.Sleep(5 * time.Second)
	b2 := beats(t, "heartbeat")
	t.Logf("the serial log failing: %d beats, then %d", b1, b2)
	if b2 <= b1+1 {
		t.Fatalf("the guest froze: %d beats, then %d", b1, b2)
	}
	mustRun(t, "chattr", "-i", dir)
	deadline = time.Now().Add(time.Minute)
	for {
		if _, err := os.Stat(filepath.Join(dir, chvm.SerialLog+".1")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the serial log never rotated once the directory was writable")
		}
		time.Sleep(200 * time.Millisecond)
	}
	b3 := beats(t, "heartbeat")
	time.Sleep(3 * time.Second)
	if b4 := beats(t, "heartbeat"); b4 <= b3 {
		t.Errorf("no beats after the directory was writable again: %d, then %d", b3, b4)
	}
	if err := h.hv.Stop(context.Background(), "heartbeat"); err != nil {
		t.Fatal(err)
	}
}
