package chguest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"

	"connectrpc.com/connect"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/guest/agent"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// Exec runs a process through the guest agent's Exec, on a connection of
// its own that lasts as long as the process: ending ctx closes it, and
// hangar-init then kills the process's group.
func (g Guest) Exec(ctx context.Context, machineUID string, spec driver.ExecSpec) (driver.ExecSession, error) {
	if len(spec.Process.Command) == 0 {
		return nil, errors.New("no program to run")
	}
	socket := g.Socket(machineUID)
	var conns ownConns
	c := newClient(transport(func(ctx context.Context) (net.Conn, error) {
		return conns.add(Dial(ctx, socket, agent.Port))
	}))
	ctx, cancel := context.WithCancel(ctx)
	stream := c.Exec(ctx)
	s := &execSession{stream: stream, cancel: cancel}
	context.AfterFunc(ctx, func() {
		_ = stream.CloseResponse()
		s.closeInput()
		// Not CloseIdleConnections: the connection is not idle until the
		// reset is through, and an exec's is its own.
		conns.close()
	})
	err := s.send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Start{Start: &guestv1.ExecStart{
		Process: &guestv1.Process{
			Args: spec.Process.Command, Env: spec.Process.Env,
			WorkingDir: spec.Process.WorkingDir, User: spec.Process.User,
		},
		Tty:  spec.TTY,
		Size: &guestv1.WindowSize{Rows: uint32(spec.Size.Rows), Cols: uint32(spec.Size.Cols)},
	}}})
	// A failed send says only that the call is over; Recv says why.
	if err != nil && !errors.Is(err, io.EOF) {
		cancel()
		return nil, err
	}
	return s, nil
}

// ownConns are the connections an exec dialed, closed when it ends.
type ownConns struct {
	mu     sync.Mutex
	conns  []net.Conn
	closed bool
}

func (o *ownConns) add(c net.Conn, err error) (net.Conn, error) {
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		_ = c.Close()
		return nil, net.ErrClosed
	}
	o.conns = append(o.conns, c)
	return c, nil
}

func (o *ownConns) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	for _, c := range o.conns {
		_ = c.Close()
	}
}

type execSession struct {
	stream *connect.BidiStreamForClient[guestv1.ExecRequest, guestv1.ExecResponse]
	cancel context.CancelFunc

	inputMu sync.Mutex // the call takes one Send at a time
	closed  bool       // CloseRequest was called

	// pending is what is left of a message larger than
	// driver.MaxExecOutput.
	pending       []byte
	pendingStderr bool
	ended         bool
}

func (s *execSession) send(r *guestv1.ExecRequest) error {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	if s.closed {
		return errors.New("the exec has ended")
	}
	return s.stream.Send(r)
}

func (s *execSession) closeInput() {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	if !s.closed {
		s.closed = true
		_ = s.stream.CloseRequest()
	}
}

func (s *execSession) Stdin(p []byte) error {
	return s.send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Stdin{Stdin: p}})
}

func (s *execSession) CloseStdin() error {
	return s.send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_StdinClose{StdinClose: &guestv1.StdinClose{}}})
}

func (s *execSession) Resize(size driver.WindowSize) error {
	return s.send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Resize{
		Resize: &guestv1.WindowSize{Rows: uint32(size.Rows), Cols: uint32(size.Cols)},
	}})
}

func (s *execSession) Signal(sig syscall.Signal) error {
	return s.send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Signal{Signal: int32(sig)}})
}

func (s *execSession) Recv() (driver.ExecOutput, error) {
	if len(s.pending) > 0 {
		return s.cut(s.pending, s.pendingStderr), nil
	}
	for !s.ended {
		res, err := s.stream.Receive()
		if err != nil {
			s.end()
			if errors.Is(err, io.EOF) {
				err = fmt.Errorf("the guest ended the exec without saying how: %w", io.ErrUnexpectedEOF)
			}
			return driver.ExecOutput{}, err
		}
		switch m := res.GetMessage().(type) {
		case *guestv1.ExecResponse_Stdout:
			if len(m.Stdout) > 0 {
				return s.cut(m.Stdout, false), nil
			}
		case *guestv1.ExecResponse_Stderr:
			if len(m.Stderr) > 0 {
				return s.cut(m.Stderr, true), nil
			}
		case *guestv1.ExecResponse_Exit:
			s.end()
			return driver.ExecOutput{Exit: &console.ExitStatus{Code: m.Exit.GetCode(), Signal: m.Exit.GetSignal()}}, nil
		}
	}
	return driver.ExecOutput{}, io.EOF
}

// cut returns at most driver.MaxExecOutput of p, keeping the rest for the
// next Recv.
func (s *execSession) cut(p []byte, stderr bool) driver.ExecOutput {
	n := min(len(p), driver.MaxExecOutput)
	s.pending, s.pendingStderr = p[n:], stderr
	return driver.ExecOutput{Data: p[:n], Stderr: stderr}
}

// end ends the call once the guest has said how the process ended, or
// failed: the guest waits for the request side to close before it ends
// the call itself.
func (s *execSession) end() {
	s.ended = true
	s.cancel()
}
