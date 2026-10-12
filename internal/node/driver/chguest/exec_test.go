package chguest_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1/guestv1connect"
	"github.com/YukinaMochizuki/hangar/internal/guest/agent"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// scripted is a guest agent whose Exec echoes stdin back, ends on stdin's
// close or a signal, and says how each call ended on calls.
type scripted struct {
	guestv1connect.UnimplementedGuestAgentServiceHandler
	// big, if set, is sent as one stdout message first.
	big []byte
	// noExit ends the call at stdin's close without an exit.
	noExit bool
	calls  chan error
}

func (g *scripted) Exec(ctx context.Context, s *connect.BidiStream[guestv1.ExecRequest, guestv1.ExecResponse]) (err error) {
	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		g.calls <- err
	}()
	if _, err := s.Receive(); err != nil {
		return err
	}
	if len(g.big) > 0 {
		if err := s.Send(&guestv1.ExecResponse{Message: &guestv1.ExecResponse_Stdout{Stdout: g.big}}); err != nil {
			return err
		}
	}
	exit := func(code, sig int32) error {
		if err := s.Send(&guestv1.ExecResponse{Message: &guestv1.ExecResponse_Exit{Exit: &guestv1.ExecExit{Code: code, Signal: sig}}}); err != nil {
			return err
		}
		// As hangar-init, until the caller closes its side.
		drain := func() {
			for {
				if _, err := s.Receive(); err != nil {
					return
				}
			}
		}
		drain()
		return nil
	}
	for {
		req, err := s.Receive()
		if err != nil {
			return err
		}
		switch m := req.GetMessage().(type) {
		case *guestv1.ExecRequest_Stdin:
			if err := s.Send(&guestv1.ExecResponse{Message: &guestv1.ExecResponse_Stderr{Stderr: m.Stdin}}); err != nil {
				return err
			}
		case *guestv1.ExecRequest_StdinClose:
			if g.noExit {
				return nil
			}
			return exit(0, 0)
		case *guestv1.ExecRequest_Signal:
			return exit(128+m.Signal, m.Signal)
		}
	}
}

func scriptedGuest(t *testing.T, g *scripted) driver.Guest {
	g.calls = make(chan error, 100)
	return guestOn(serveCH(t, agent.Port, g, func() {}))
}

// recvAll reads a session to its end: output, then the exit or a failure.
func recvAll(t *testing.T, s driver.ExecSession) (stdout, stderr string, exit driver.ExecOutput, err error) {
	t.Helper()
	var out, errb strings.Builder
	for {
		o, err := s.Recv()
		if err != nil {
			return out.String(), errb.String(), exit, err
		}
		if o.Exit != nil {
			if _, err := s.Recv(); !errors.Is(err, io.EOF) {
				t.Errorf("Recv after the exit: %v", err)
			}
			return out.String(), errb.String(), o, nil
		}
		if len(o.Data) > driver.MaxExecOutput {
			t.Errorf("an output of %d bytes", len(o.Data))
		}
		if o.Stderr {
			errb.Write(o.Data)
		} else {
			out.Write(o.Data)
		}
	}
}

func callEnded(t *testing.T, g *scripted) error {
	t.Helper()
	select {
	case err := <-g.calls:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the guest's call did not end")
		return nil
	}
}

func TestExecSessionCarriesInputAndOutput(t *testing.T) {
	g := &scripted{}
	s, err := scriptedGuest(t, g).Exec(t.Context(), "m1", driver.ExecSpec{Process: driver.RunSpec{Command: []string{"x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Stdin([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := s.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	_, stderr, exit, err := recvAll(t, s)
	if err != nil || stderr != "hi" || exit.Exit.Code != 130 || exit.Exit.Signal != 2 {
		t.Errorf("stderr %q exit %+v err %v", stderr, exit.Exit, err)
	}
	_ = callEnded(t, g)
	if err := s.Stdin([]byte("late")); err == nil {
		t.Error("stdin taken after the exit")
	}
}

// TestEndingAnExecReachesTheGuest: ending the exec's context resets the
// guest's call, which is what makes hangar-init kill the process.
func TestEndingAnExecReachesTheGuest(t *testing.T) {
	g := &scripted{}
	ctx, cancel := context.WithCancel(t.Context())
	s, err := scriptedGuest(t, g).Exec(ctx, "m1", driver.ExecSpec{Process: driver.RunSpec{Command: []string{"x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Stdin([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if o, err := s.Recv(); err != nil || string(o.Data) != "hi" {
		t.Fatalf("%+v %v", o, err)
	}
	cancel()
	if err := callEnded(t, g); !errors.Is(err, context.Canceled) {
		t.Errorf("the guest's call ended with %v, want its context cancelled", err)
	}
	if _, err := s.Recv(); err == nil {
		t.Error("Recv of an ended exec")
	}
}

func TestAnExecCutsLargeOutput(t *testing.T) {
	big := []byte(strings.Repeat("0123456789abcdef", 32<<10)) // 512 KiB
	g := &scripted{big: big}
	s, err := scriptedGuest(t, g).Exec(t.Context(), "m1", driver.ExecSpec{Process: driver.RunSpec{Command: []string{"x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CloseStdin(); err != nil {
		t.Fatal(err)
	}
	stdout, _, exit, err := recvAll(t, s)
	if err != nil || stdout != string(big) || exit.Exit.Code != 0 {
		t.Errorf("%d bytes of %d, exit %+v, err %v", len(stdout), len(big), exit.Exit, err)
	}
}

func TestAnExecWithoutAnExitFails(t *testing.T) {
	g := &scripted{noExit: true}
	s, err := scriptedGuest(t, g).Exec(t.Context(), "m1", driver.ExecSpec{Process: driver.RunSpec{Command: []string{"x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CloseStdin(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := recvAll(t, s); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a call ended without an exit: %v", err)
	}
}

func TestAnExecOfNoGuestFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	s, err := guestOn(fakeCH(t, agent.Port+1)).Exec(ctx, "m1", driver.ExecSpec{Process: driver.RunSpec{Command: []string{"x"}}})
	if err != nil {
		return
	}
	if _, _, _, err := recvAll(t, s); err == nil || ctx.Err() != nil {
		t.Errorf("an exec with no agent listening: %v", err)
	}
}

// TestExecsLeaveNothingOpen: every exec, ended or cancelled, leaves no
// connection or goroutine behind.
func TestExecsLeaveNothingOpen(t *testing.T) {
	g := &scripted{}
	guest := scriptedGuest(t, g)
	once := func(cancelIt bool) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		s, err := guest.Exec(ctx, "m1", driver.ExecSpec{Process: driver.RunSpec{Command: []string{"x"}}})
		if err != nil {
			t.Fatal(err)
		}
		if cancelIt {
			// Once the guest has the call.
			if err := s.Stdin([]byte("x")); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Recv(); err != nil {
				t.Fatal(err)
			}
			cancel()
		} else if err := s.CloseStdin(); err != nil {
			t.Fatal(err)
		}
		_, _, _, _ = recvAll(t, s)
		_ = callEnded(t, g)
	}
	once(false)
	fds, goroutines := openFDs(t), runtime.NumGoroutine()
	for i := range 30 {
		once(i%2 == 0)
	}
	deadline := time.Now().Add(5 * time.Second)
	for openFDs(t) > fds+2 || runtime.NumGoroutine() > goroutines+5 {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("after 30 execs: %d fds (from %d), %d goroutines (from %d)\n%s",
				openFDs(t), fds, runtime.NumGoroutine(), goroutines, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestEndingAnExecKillsTheProcessGroup: through hangar-init's agent, a
// child the process started goes with it.
func TestEndingAnExecKillsTheProcessGroup(t *testing.T) {
	execsAsRoot(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := guestOn(fakeCH(t, agent.Port)).Exec(ctx, "m1", driver.ExecSpec{
		Process: driver.RunSpec{Command: []string{"sh", "-c", "sleep 30 & echo $!; wait"}, Env: []string{"PATH=/usr/bin:/bin"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	o, err := s.Recv()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(o.Data)))
	if err != nil {
		t.Fatalf("pid %q: %v", o.Data, err)
	}
	cancel()
	deadline := time.Now().Add(10 * time.Second)
	for {
		// Killed, it may be left a zombie until whoever it was handed to
		// reaps it.
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if errors.Is(err, os.ErrNotExist) || strings.Contains(string(stat), ") Z ") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child %d still runs after its exec ended: %s", pid, stat)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
