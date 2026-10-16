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

// interrupt takes the machine's unit away as a reboot does: systemd stops
// it with no stop asked of the driver, so no stop mark, and the launcher
// records no exit, however the guest's process ended. It returns how long
// the stop took.
func interrupt(t *testing.T, uid string) time.Duration {
	t.Helper()
	unit := "hangar-vm-" + uid + ".service"
	start := time.Now()
	mustRun(t, "systemctl", "stop", unit)
	took := time.Since(start)
	_ = exec.Command("systemctl", "reset-failed", unit).Run()
	for _, f := range []string{"exit.json", "stopping"} {
		if _, err := os.Stat(filepath.Join(vmDir, uid, f)); err == nil {
			t.Fatalf("the interruption left %s", f)
		}
	}
	if pid := chPID(t, uid); pid != 0 {
		t.Fatalf("Cloud Hypervisor runs on after the unit's stop, pid %d", pid)
	}
	return took
}

// TestInterruptedVMs: a VM interrupted with no record, as at a reboot,
// starts again under always and on-failure (it is not listed, and the
// agent starts it); under never it has exited, with ExitNoRecord. So does
// a VM whose launcher is killed outright (decision 16). Interrupted by
// systemd stopping its unit, as at host shutdown, the container gets
// SIGTERM first, and its handler's exit 0 makes no difference.
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
			took := interrupt(t, uid)
			t.Logf("systemctl stop took %v", took)
			if out := stdoutOf(t, uid); !strings.Contains(out, "got SIGTERM\n") {
				t.Errorf("stdout %q, want the SIGTERM handler's", out)
			}
			if st, ok := lastExit(t, uid); !ok || st.Code != 0 {
				t.Errorf("last exit on the console %+v %t, want the handler's 0", st, ok)
			}
			if took > testGrace {
				t.Errorf("a process exiting on SIGTERM took %v to stop, past the grace", took)
			}
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

// TestAShutdownKillsWhatIgnoresSIGTERM: systemd stopping the unit of a VM
// whose process ignores SIGTERM, as at host shutdown, waits for the host's
// grace, 10 seconds, and not much longer: the guest kills the process
// and powers off, and the unit's stop timeout leaves room for it. The VM
// reads as interrupted.
func TestAShutdownKillsWhatIgnoresSIGTERM(t *testing.T) {
	h := newHost(t)
	h.hv = newHypervisor(t, driver.StopGrace)
	spec := h.machine(t, "stubborn-shutdown", driver.RestartNever, `trap 'echo ignoring SIGTERM' TERM; echo ready; while :; do sleep 1; done`)
	startAndWait(t, h, spec)
	timeout := strings.TrimSpace(mustRun(t, "systemctl", "show", "-P", "TimeoutStopUSec", "hangar-vm-stubborn-shutdown.service"))
	t.Logf("TimeoutStopUSec=%s", timeout)
	if timeout != "25s" {
		t.Errorf("TimeoutStopUSec=%s, want 25s: 3s to ask, the 10s grace, 5s of slack, 2s to kill Cloud Hypervisor, 5s to read its console", timeout)
	}
	took := interrupt(t, "stubborn-shutdown")
	t.Logf("systemctl stop took %v", took)
	if took < driver.StopGrace || took > driver.StopGrace+chvm.DefaultStopSlack {
		t.Errorf("stopped in %v, want after the %v grace and within the %v slack", took, driver.StopGrace, chvm.DefaultStopSlack)
	}
	if out := stdoutOf(t, "stubborn-shutdown"); !strings.Contains(out, "ignoring SIGTERM\n") {
		dump(t, "stubborn-shutdown")
		t.Errorf("stdout %q, want the SIGTERM handler's", out)
	}
	if st, ok := lastExit(t, "stubborn-shutdown"); !ok || st.Signal != int32(syscall.SIGKILL) {
		t.Errorf("last exit on the console %+v %t, want SIGKILL", st, ok)
	}
	if vm, ok := vmOf(t, h, "stubborn-shutdown"); !ok || vm.State != driver.VMExited || vm.ExitCode != chvm.ExitNoRecord {
		t.Errorf("listed %+v %t, want exited with %d", vm, ok, chvm.ExitNoRecord)
	}
	if err := h.hv.Stop(context.Background(), "stubborn-shutdown"); err != nil {
		t.Fatal(err)
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

// heartbeat floods the serial console with 4 KiB at a time, and says on
// stdout each time it has. The serial console carries as little as 10 KB/s
// on a nested testbed, so a beat comes about twice a second.
const heartbeat = `trap 'exit 0' TERM
echo ready
i=0
while :; do
	head -c 4096 /dev/zero | tr '\0' s > /dev/ttyS0
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
	// A log left near its cap by an earlier run, which the launcher
	// appends to, rotates within seconds; not before the guest is ready,
	// its kernel's boot taking a few KB.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, chvm.SerialLog), make([]byte, chvm.DefaultSerialMax-64<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("chattr", "-i", dir).Run() })
	// The unit's journal holds earlier runs' failures too.
	since := time.Now().Format(time.DateTime)
	startAndWait(t, h, spec)
	mustRun(t, "chattr", "+i", dir)
	failed := func() bool {
		out, _ := exec.Command("journalctl", "--no-pager", "-u", "hangar-vm-heartbeat.service", "--since", since, "-g", "the serial log fails").Output()
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
