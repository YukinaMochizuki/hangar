package supervise_test

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/guest/supervise"
)

// group starts script leading a session of its own, as the container's
// main process does, its output to out.
func group(t *testing.T, r *supervise.Reaper, script string, out *os.File) *supervise.Process {
	t.Helper()
	p, err := r.Start([]string{"/bin/sh", "-c", script}, &os.ProcAttr{
		Files: []*os.File{nil, out, os.Stderr},
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// waitFor waits for the process to end within d.
func waitFor(t *testing.T, p *supervise.Process, d time.Duration) supervise.Exit {
	t.Helper()
	select {
	case <-p.Done():
		return p.Wait()
	case <-time.After(d):
		t.Fatalf("the process did not end within %v", d)
		return supervise.Exit{}
	}
}

func TestStopTermsTheGroup(t *testing.T) {
	r := supervise.NewReaper()
	defer r.Stop()
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	// The child in the group says it was asked, too.
	p := group(t, r, `sh -c 'trap "echo child; exit 0" TERM; echo armed; while :; do sleep 0.05; done' &
trap 'wait; echo main; exit 7' TERM
echo ready
while :; do sleep 0.05; done`, out)
	waitOutput(t, out, "ready")
	waitOutput(t, out, "armed") // the child's trap is set too
	var s supervise.Stopper
	s.Started(p)
	start := time.Now()
	s.Stop(time.Minute)
	s.Stop(0) // changes nothing: no SIGKILL now
	if e := waitFor(t, p, 10*time.Second); e != (supervise.Exit{Code: 7}) {
		t.Errorf("exit %+v, want the handler's 7", e)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v", d)
	}
	b, _ := os.ReadFile(out.Name())
	if !strings.Contains(string(b), "child") || !strings.Contains(string(b), "main") {
		t.Errorf("output %q, want both handlers'", b)
	}
}

// waitOutput waits for f to hold s, so a script's traps are set.
func waitOutput(t *testing.T, f *os.File, s string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(f.Name())
		if strings.Contains(string(b), s) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q in %q", s, b)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStopKillsAfterGrace(t *testing.T) {
	r := supervise.NewReaper()
	defer r.Stop()
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	p := group(t, r, `trap '' TERM; echo ready; while :; do sleep 0.05; done`, out)
	waitOutput(t, out, "ready")
	var s supervise.Stopper
	s.Started(p)
	start := time.Now()
	s.Stop(500 * time.Millisecond)
	e := waitFor(t, p, 10*time.Second)
	if e.Signal != int(syscall.SIGKILL) {
		t.Errorf("exit %+v, want SIGKILL", e)
	}
	if d := time.Since(start); d < 500*time.Millisecond || d > 5*time.Second {
		t.Errorf("killed after %v, want after the 500ms grace", d)
	}
}

// TestStopBeforeStart: a stop asked for before the process starts applies
// as it starts.
func TestStopBeforeStart(t *testing.T) {
	r := supervise.NewReaper()
	defer r.Stop()
	var s supervise.Stopper
	s.Stop(time.Minute)
	p := group(t, r, `sleep 30`, os.Stdout)
	s.Started(p)
	if e := waitFor(t, p, 10*time.Second); e.Signal != int(syscall.SIGTERM) {
		t.Errorf("exit %+v, want SIGTERM", e)
	}
}
