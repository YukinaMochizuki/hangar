package node

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"syscall"

	"connectrpc.com/connect"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// maxSignal is the highest signal number Linux has.
const maxSignal = 64

// Exec runs a process in a running machine's guest for as long as the call
// lasts. Once the exit is sent the call ends when the caller closes its
// side, so nothing the caller sends is left unread.
func (s nodeService) Exec(ctx context.Context, stream *connect.BidiStream[nodev1.ExecRequest, nodev1.ExecResponse]) error {
	if s.a.cfg.Drivers == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("this node runs no machines"))
	}
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if err := checkExecStart(start); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	uid := start.GetMachineUid()
	vm, err := s.a.vmOf(ctx, uid)
	if err != nil {
		return connect.NewError(connect.CodeUnknown, err)
	}
	if vm == nil || vm.State != driver.VMRunning {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("machine %s's VM is not running", uid))
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	sess, err := s.a.cfg.Drivers.Guest.Exec(ctx, uid, driver.ExecSpec{
		Process: execProcess(vm.Run, start),
		TTY:     start.GetTty(),
		Size:    windowSize(start.GetSize()),
	})
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("machine %s's guest: %w", uid, err))
	}
	if !start.GetStdin() && !start.GetTty() {
		// As docker exec without -i: the process reads end-of-file. A
		// terminal is left open instead, which a shell would read as ^D.
		// A process that has ended already refuses it, and Recv says how
		// it ended; a guest that failed fails Recv too.
		_ = sess.CloseStdin()
	}
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		if err := execInput(stream, sess, start.GetStdin()); err != nil {
			cancel(err)
		}
	}()
	for {
		out, err := sess.Recv()
		if err == nil {
			err = stream.Send(execResponse(out))
		}
		if err != nil {
			// What the caller sends is not waited for: it may send nothing
			// more. Its reader ends as the call does.
			return s.execFailed(ctx, uid, err)
		}
		if out.Exit != nil {
			<-inputDone
			return nil
		}
	}
}

// execFailed is what an exec fails with when it ended by err.
func (s nodeService) execFailed(ctx context.Context, uid string, err error) error {
	var invalid errInvalidInput
	switch {
	case errors.As(context.Cause(ctx), &invalid):
		return connect.NewError(connect.CodeInvalidArgument, invalid)
	case s.agent.Err() != nil:
		return connect.NewError(connect.CodeUnavailable, errors.New("the node agent is stopping"))
	case ctx.Err() != nil:
		return connect.NewError(connect.CodeCanceled, ctx.Err())
	}
	if ce := new(connect.Error); errors.As(err, &ce) && ce.Code() != connect.CodeUnknown && ce.Code() != connect.CodeCanceled {
		return connect.NewError(ce.Code(), fmt.Errorf("machine %s's guest: %s", uid, ce.Message()))
	}
	return connect.NewError(connect.CodeUnavailable, fmt.Errorf("machine %s's guest: %w", uid, err))
}

// errInvalidInput ends an exec whose caller sent what it may not.
type errInvalidInput struct{ error }

// execInput passes on what the caller sends after the start until it
// closes its side, or the exec ends. Once the process has ended, what
// comes is read and dropped.
func execInput(stream *connect.BidiStream[nodev1.ExecRequest, nodev1.ExecResponse], sess driver.ExecSession, stdin bool) error {
	ended := false
	for {
		req, ok := receive(stream)
		if !ok {
			return nil
		}
		if ended {
			continue
		}
		var err error
		switch m := req.GetMessage().(type) {
		case *nodev1.ExecRequest_Stdin:
			if !stdin {
				return errInvalidInput{errors.New("stdin for an exec started without it")}
			}
			err = sess.Stdin(m.Stdin)
		case *nodev1.ExecRequest_StdinClose:
			if !stdin {
				return errInvalidInput{errors.New("stdin for an exec started without it")}
			}
			err = sess.CloseStdin()
		case *nodev1.ExecRequest_Resize:
			err = sess.Resize(windowSize(m.Resize))
		case *nodev1.ExecRequest_Signal:
			if m.Signal < 1 || m.Signal > maxSignal {
				return errInvalidInput{fmt.Errorf("signal %d is not one of Linux's", m.Signal)}
			}
			err = sess.Signal(syscall.Signal(m.Signal))
		case *nodev1.ExecRequest_Start:
			return errInvalidInput{errors.New("an exec starts once")}
		}
		// The process has ended, and Recv says how.
		ended = err != nil
	}
}

// receive returns the caller's next request, or false once it has closed
// its side or gone.
func receive(stream *connect.BidiStream[nodev1.ExecRequest, nodev1.ExecResponse]) (*nodev1.ExecRequest, bool) {
	req, err := stream.Receive()
	return req, err == nil
}

func checkExecStart(start *nodev1.ExecStart) error {
	switch {
	case start == nil:
		return errors.New("the first request must start the process")
	case start.GetMachineUid() == "":
		return errors.New("machine_uid is required")
	case len(start.GetArgs()) == 0 || start.GetArgs()[0] == "":
		return errors.New("a program to run is required")
	}
	for _, kv := range start.GetEnv() {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
			return fmt.Errorf("env %q is not KEY=VALUE", kv)
		}
	}
	return nil
}

// execProcess is the process an exec runs: the container's, as its VM was
// started, with what the start sets instead.
func execProcess(run driver.RunSpec, start *nodev1.ExecStart) driver.RunSpec {
	p := driver.RunSpec{Command: start.GetArgs(), Env: run.Env, WorkingDir: run.WorkingDir, User: run.User}
	for _, kv := range start.GetEnv() {
		k, v, _ := strings.Cut(kv, "=")
		p.Env = withEnv(p.Env, k, v)
	}
	if d := start.GetWorkingDir(); d != "" {
		p.WorkingDir = d
	}
	if u := start.GetUser(); u != "" {
		p.User = u
	}
	return p
}

func windowSize(s *nodev1.WindowSize) driver.WindowSize {
	return driver.WindowSize{
		Rows: uint16(min(s.GetRows(), math.MaxUint16)),
		Cols: uint16(min(s.GetCols(), math.MaxUint16)),
	}
}

func execResponse(out driver.ExecOutput) *nodev1.ExecResponse {
	switch {
	case out.Exit != nil:
		// The guest's claim: no process exits with more than 255.
		code := out.Exit.Code
		if code < 0 || code > 255 {
			code = 255
		}
		return &nodev1.ExecResponse{Message: &nodev1.ExecResponse_Exit{Exit: &nodev1.ExitStatus{Code: code, Signal: out.Exit.Signal}}}
	case out.Stderr:
		return &nodev1.ExecResponse{Message: &nodev1.ExecResponse_Stderr{Stderr: out.Data}}
	}
	return &nodev1.ExecResponse{Message: &nodev1.ExecResponse_Stdout{Stdout: out.Data}}
}
