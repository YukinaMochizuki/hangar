//go:build testbed

package vms

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// unitActive reports whether the machine's unit is loaded and not
// inactive.
func unitActive(uid string) bool {
	out, _ := exec.Command("systemctl", "is-active", "hangar-vm-"+uid+".service").Output()
	s := strings.TrimSpace(string(out))
	return s == "active" || s == "activating" || s == "deactivating"
}

// mainPID is the launcher's pid, 0 if the unit has none.
func mainPID(t *testing.T, uid string) int {
	t.Helper()
	out, _ := exec.Command("systemctl", "show", "-P", "MainPID", "hangar-vm-"+uid+".service").Output()
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return pid
}

// chPID is the pid of the machine's Cloud Hypervisor, 0 if none.
func chPID(t *testing.T, uid string) int {
	t.Helper()
	out, _ := exec.Command("pgrep", "-f", "socket=/var/tmp/hangar-testbed-vms/"+uid+"/vsock").Output()
	pid, _ := strconv.Atoi(strings.TrimSpace(strings.Split(string(out), "\n")[0]))
	return pid
}

// startAndWait starts a VM whose process says "ready" once it has set
// its traps, and waits for it to say so.
func startAndWait(t *testing.T, h *host, spec driver.VMSpec) {
	t.Helper()
	before := strings.Count(stdoutOf(t, spec.MachineUID), "ready\n")
	if err := h.hv.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	ready(t, spec.MachineUID)
	waitReady(t, spec.MachineUID, before+1)
}

// waitReady waits for the machine's log to hold n boots' "ready".
func waitReady(t *testing.T, uid string, n int) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for strings.Count(stdoutOf(t, uid), "ready\n") < n {
		if time.Now().After(deadline) {
			dump(t, uid)
			t.Fatalf("the log never held %d boots' ready", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stoppedForGood checks a stopped VM stays down: no unit, nothing listed,
// for a while after.
func stoppedForGood(t *testing.T, h *host, uid string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if unitActive(uid) {
			t.Fatalf("machine %s's unit is up again after Stop", uid)
		}
		vms, _ := h.hv.VMs(context.Background())
		for _, vm := range vms {
			if vm.MachineUID == uid {
				t.Fatalf("machine %s's VM is listed after Stop: %+v", uid, vm)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if pid := chPID(t, uid); pid != 0 {
		t.Errorf("machine %s's Cloud Hypervisor runs on, pid %d", uid, pid)
	}
}

// TestGracefulStop: the container's process gets SIGTERM, what its handler
// writes is in the log, and the VM, under the always policy, stays down.
func TestGracefulStop(t *testing.T) {
	h := newHost(t)
	spec := h.machine(t, "graceful", driver.RestartAlways, guestScript)
	startAndWait(t, h, spec)
	start := time.Now()
	if err := h.hv.Stop(context.Background(), "graceful"); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	t.Logf("stopped in %v", took)
	if took > testGrace {
		t.Errorf("a process exiting on SIGTERM took %v to stop, past the grace", took)
	}
	if out := stdoutOf(t, "graceful"); !strings.Contains(out, "ready\n") || !strings.Contains(out, "got SIGTERM\n") {
		t.Errorf("stdout %q, want the SIGTERM handler's", out)
	}
	if st, ok := lastExit(t, "graceful"); !ok || st.Code != 0 {
		t.Errorf("last exit %+v %t, want the handler's 0", st, ok)
	}
	stoppedForGood(t, h, "graceful")
}

// TestStopKillsWhatIgnoresSIGTERM: a process ignoring SIGTERM is killed
// once the host's grace, 10 seconds, has passed.
func TestStopKillsWhatIgnoresSIGTERM(t *testing.T) {
	h := newHost(t)
	h.hv = newHypervisor(t, driver.StopGrace)
	spec := h.machine(t, "stubborn", driver.RestartAlways, `trap '' TERM; echo ready; while :; do sleep 1; done`)
	startAndWait(t, h, spec)
	start := time.Now()
	if err := h.hv.Stop(context.Background(), "stubborn"); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	t.Logf("stopped in %v", took)
	if took < driver.StopGrace || took > driver.StopGrace+5*time.Second {
		t.Errorf("stopped in %v, want after the %v grace and within 5s more", took, driver.StopGrace)
	}
	if st, ok := lastExit(t, "stubborn"); !ok || st.Signal != int32(syscall.SIGKILL) {
		t.Errorf("last exit %+v %t, want SIGKILL", st, ok)
	}
	stoppedForGood(t, h, "stubborn")
}

// TestStopKillsAnUnresponsiveGuest: a guest that does not answer, here
// frozen, is killed within the grace and a bound.
func TestStopKillsAnUnresponsiveGuest(t *testing.T) {
	h := newHost(t)
	spec := h.machine(t, "frozen", driver.RestartAlways, guestScript)
	startAndWait(t, h, spec)
	pid := chPID(t, "frozen")
	if pid == 0 {
		t.Fatal("no Cloud Hypervisor")
	}
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := h.hv.Stop(context.Background(), "frozen"); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	t.Logf("stopped in %v", took)
	if took > testGrace+10*time.Second {
		t.Errorf("an unresponsive guest took %v to stop", took)
	}
	stoppedForGood(t, h, "frozen")
}

// TestRestartsAreCounted: systemd counts the restarts the policy makes, and
// a restarted VM keeps its log.
func TestRestartsAreCounted(t *testing.T) {
	h := newHost(t)
	spec := h.machine(t, "counted", driver.RestartOnFailure, guestScript)
	startAndWait(t, h, spec)
	for i := 1; i <= 2; i++ {
		exit(t, "counted", 7)
		deadline := time.Now().Add(60 * time.Second)
		for {
			out, _ := exec.Command("systemctl", "show", "-P", "NRestarts", "hangar-vm-counted.service").Output()
			if strings.TrimSpace(string(out)) == strconv.Itoa(i) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("NRestarts %q, want %d", out, i)
			}
			time.Sleep(100 * time.Millisecond)
		}
		waitReady(t, "counted", i+1)
	}
	if err := h.hv.Stop(context.Background(), "counted"); err != nil {
		t.Fatal(err)
	}
	// The log outlives the stop, and a start again appends to it.
	startAndWait(t, h, spec)
	if n := strings.Count(stdoutOf(t, "counted"), "ready\n"); n != 4 {
		t.Errorf("after a stop and a start the log holds %d boots' output, want 4", n)
	}
}
