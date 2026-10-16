package drivertest

import (
	"context"
	"errors"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// ExecPrograms are the programs the exec part runs in a guest, as the
// guest's root holds them.
type ExecPrograms struct {
	// Output writes stdout to stdout and stderr to stderr, and exits with
	// code.
	Output func(stdout, stderr string, code int) []string
	// Cat copies stdin to stdout until stdin closes, then exits with 0.
	Cat []string
	// Size prints the terminal's size as "ROWS COLS", reads a line of
	// stdin, prints the size again, and exits with 0.
	Size []string
	// Show prints the value of the environment variable HANGAR_EXEC, the
	// working directory, then the user's uid, a line each.
	Show []string
	// Wait runs until it is killed, with a child of its own that Running
	// says is running: a kill of only the process it runs leaves the
	// child.
	Wait func(marker string) []string
	// Running prints 1 while the child of Wait(marker) runs, 0 otherwise.
	Running func(marker string) []string
}

// execRun is what an exec's process wrote, and how it ended.
type execRun struct {
	stdout, stderr string
	exit           driver.ExecOutput
	err            error
}

func startExec(t *testing.T, ctx context.Context, h Host, spec driver.ExecSpec) driver.ExecSession {
	t.Helper()
	s, err := h.Guest.Exec(ctx, "m1", spec)
	must(t, err)
	return s
}

// drain reads an exec's output to its end, within a minute.
func drain(t *testing.T, s driver.ExecSession) execRun {
	t.Helper()
	done := make(chan execRun, 1)
	go func() {
		var r execRun
		var out, errOut strings.Builder
		defer func() { r.stdout, r.stderr = out.String(), errOut.String(); done <- r }()
		for {
			o, err := s.Recv()
			if err != nil {
				r.err = err
				return
			}
			if o.Exit != nil {
				r.exit = o
				if _, err := s.Recv(); !errors.Is(err, io.EOF) {
					r.err = errors.New("a Recv after the exit did not end")
				}
				return
			}
			if o.Stderr {
				errOut.Write(o.Data)
			} else {
				out.Write(o.Data)
			}
		}
	}()
	select {
	case r := <-done:
		return r
	case <-time.After(time.Minute):
		t.Fatal("the exec did not end")
		return execRun{}
	}
}

func (r execRun) check(t *testing.T, what string, code, signal int32) {
	t.Helper()
	switch {
	case r.err != nil:
		t.Errorf("%s: %v (stdout %q, stderr %q)", what, r.err, r.stdout, r.stderr)
	case r.exit.Exit.Code != code || r.exit.Exit.Signal != signal:
		t.Errorf("%s ended with %+v, want code %d and signal %d (stdout %q, stderr %q)", what, *r.exit.Exit, code, signal, r.stdout, r.stderr)
	}
}

// exec: a process runs in a running guest beside its container's, with
// what it is given and nothing of the container's; its output, and how it
// ended, come back; ending its context kills its whole group.
func exec(t *testing.T, h Host) {
	ctx := t.Context()
	p := h.Exec
	// A guest may fail at once, or only once the session is read.
	if s, err := h.Guest.Exec(ctx, "m1", driver.ExecSpec{Process: driver.RunSpec{Command: p.Cat}}); err == nil {
		if r := drain(t, s); r.err == nil {
			t.Error("an exec ran in a machine with no VM")
		}
	}
	spec := vmSpec(t, h, "m1", driver.RestartAlways)
	must(t, h.Hypervisor.Start(ctx, spec))
	testutil.EventuallyWithin(t, vmWait, "the guest agent answering", func() bool { return h.Guest.Ready(ctx, "m1") == nil })
	run := func(spec driver.ExecSpec) driver.ExecSession { return startExec(t, ctx, h, spec) }
	proc := func(args []string) driver.RunSpec { return driver.RunSpec{Command: args} }

	r := drain(t, run(driver.ExecSpec{Process: proc(p.Output("out\n", "err\n", 3))}))
	r.check(t, "output", 3, 0)
	if r.stdout != "out\n" || r.stderr != "err\n" {
		t.Errorf("stdout %q and stderr %q, want out and err", r.stdout, r.stderr)
	}

	r = drain(t, run(driver.ExecSpec{Process: proc([]string{"hangar-no-such-program"})}))
	r.check(t, "a program not found", 127, 0)

	s := run(driver.ExecSpec{Process: proc(p.Cat)})
	must(t, s.Stdin([]byte("hello\n")))
	must(t, s.CloseStdin())
	r = drain(t, s)
	r.check(t, "cat", 0, 0)
	if r.stdout != "hello\n" {
		t.Errorf("cat wrote %q, want hello", r.stdout)
	}

	s = run(driver.ExecSpec{Process: proc(p.Cat)})
	must(t, s.Signal(syscall.SIGTERM))
	drain(t, s).check(t, "cat sent SIGTERM", 128+int32(syscall.SIGTERM), int32(syscall.SIGTERM))

	// On a terminal, stdout and stderr are one stream.
	r = drain(t, run(driver.ExecSpec{Process: proc(p.Output("out\n", "err\n", 0)), TTY: true, Size: driver.WindowSize{Rows: 24, Cols: 80}}))
	r.check(t, "output on a terminal", 0, 0)
	if !strings.Contains(r.stdout, "out") || !strings.Contains(r.stdout, "err") || r.stderr != "" {
		t.Errorf("on a terminal stdout %q and stderr %q, want both in stdout", r.stdout, r.stderr)
	}

	s = run(driver.ExecSpec{Process: proc(p.Size), TTY: true, Size: driver.WindowSize{Rows: 24, Cols: 80}})
	// Resized once the first size is out, so it is the size it started with.
	var first strings.Builder
	for !strings.Contains(first.String(), "24 80") {
		o, err := s.Recv()
		if err != nil || o.Exit != nil {
			t.Fatalf("size ended before printing 24 80: %+v %v, after %q", o, err, first.String())
		}
		first.Write(o.Data)
	}
	must(t, s.Resize(driver.WindowSize{Rows: 30, Cols: 100}))
	must(t, s.Stdin([]byte("\n")))
	r = drain(t, s)
	r.check(t, "size", 0, 0)
	if !strings.Contains(r.stdout, "30 100") {
		t.Errorf("a terminal resized from 24x80 to 30x100 printed %q, then %q", first.String(), r.stdout)
	}

	r = drain(t, run(driver.ExecSpec{Process: driver.RunSpec{
		Command: p.Show, Env: []string{"HANGAR_EXEC=yes", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}, WorkingDir: "/tmp", User: "65534",
	}}))
	r.check(t, "show", 0, 0)
	if r.stdout != "yes\n/tmp\n65534\n" {
		t.Errorf("show printed %q, want its environment's yes, /tmp and 65534", r.stdout)
	}

	// Ending the context kills the process's group, its child included.
	running := func() string {
		r := drain(t, run(driver.ExecSpec{Process: proc(p.Running("drivertest"))}))
		r.check(t, "running", 0, 0)
		return strings.TrimSpace(r.stdout)
	}
	waitCtx, cancel := context.WithCancel(ctx)
	s = startExec(t, waitCtx, h, driver.ExecSpec{Process: proc(p.Wait("drivertest"))})
	recvDone := make(chan error, 1)
	go func() {
		_, err := s.Recv()
		recvDone <- err
	}()
	testutil.EventuallyWithin(t, vmWait, "the waiting process's child running", func() bool { return running() == "1" })
	cancel()
	select {
	case err := <-recvDone:
		if err == nil {
			t.Error("an exec ended by its context sent output")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Recv did not end with the exec's context")
	}
	testutil.EventuallyWithin(t, vmWait, "the waiting process's child killed", func() bool { return running() == "0" })

	must(t, h.Hypervisor.Stop(ctx, "m1"))
	must(t, h.Hypervisor.DeleteLogs(ctx, "m1"))
	must(t, h.Network.DeleteInterfaces(ctx, "m1"))
	must(t, h.Storage.DeleteRoot(ctx, "m1"))
}
