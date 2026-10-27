// Package agent is hangar-init's side of the guest agent protocol: what the
// node asks of a running guest over vsock.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/creack/pty"
	"google.golang.org/protobuf/types/known/durationpb"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1/guestv1connect"
	"github.com/YukinaMochizuki/hangar/internal/guest/supervise"
)

// Port is the vsock port hangar-init serves the agent on.
const Port = 1024

// MaxMessage bounds one message either way; output and stdin travel in
// smaller pieces.
const MaxMessage = 1 << 20

// Server serves GuestAgentService.
type Server struct {
	// Reaper starts what Exec runs; without one, as in a FileShare's server,
	// which runs nothing of the tenant's, Exec is refused.
	Reaper *supervise.Reaper
	// Root is where processes find their programs and users; "/" in the
	// guest.
	Root    string
	Version string
	// Started is when hangar-init started, for the uptime.
	Started time.Time
	// Cgroup, if set, is the cgroup directory exec'd processes start in.
	Cgroup *os.File
	// OnPanic, if set, hears of a panic the server recovered from.
	OnPanic func(any)
	// StopMain, if set, stops the container's main process as Stop asks:
	// SIGTERM to its process group now, SIGKILL once grace has passed. It
	// returns at once, and does nothing when called again.
	StopMain func(grace time.Duration)
	// ApplyIPv6, if set, makes the interfaces' public IPv6 what SetIPv6
	// asks.
	ApplyIPv6 func(ifaces []*guestv1.Interface) error
	// Grow, if set, grows the guest's filesystems to its disks, as
	// GrowFilesystems asks.
	Grow func() error
}

// MaxGrace bounds the grace period a Stop may ask for.
const MaxGrace = time.Hour

var _ guestv1connect.GuestAgentServiceHandler = (*Server)(nil)

func (s *Server) Ping(context.Context, *connect.Request[guestv1.PingRequest]) (*connect.Response[guestv1.PingResponse], error) {
	return connect.NewResponse(&guestv1.PingResponse{Version: s.Version, Uptime: durationpb.New(time.Since(s.Started))}), nil
}

// Stop stops the container's main process; the guest powers off once it
// has ended.
func (s *Server) Stop(_ context.Context, req *connect.Request[guestv1.StopRequest]) (*connect.Response[guestv1.StopResponse], error) {
	if s.StopMain == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this guest has no main process to stop"))
	}
	grace := req.Msg.GetGrace()
	if err := grace.CheckValid(); grace != nil && err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	d := grace.AsDuration()
	if d < 0 || d > MaxGrace {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("a grace period of %v is not between 0 and %v", d, MaxGrace))
	}
	s.StopMain(d)
	return connect.NewResponse(&guestv1.StopResponse{}), nil
}

// SetIPv6 changes the interfaces' public IPv6 while the container runs.
func (s *Server) SetIPv6(_ context.Context, req *connect.Request[guestv1.SetIPv6Request]) (*connect.Response[guestv1.SetIPv6Response], error) {
	if s.ApplyIPv6 == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this guest's network is not the agent's to change"))
	}
	if err := s.ApplyIPv6(req.Msg.GetInterfaces()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&guestv1.SetIPv6Response{}), nil
}

// GrowFilesystems grows the guest's filesystems to its disks, which the
// node has grown.
func (s *Server) GrowFilesystems(context.Context, *connect.Request[guestv1.GrowFilesystemsRequest]) (*connect.Response[guestv1.GrowFilesystemsResponse], error) {
	if s.Grow == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this guest's filesystems are not the agent's to grow"))
	}
	if err := s.Grow(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&guestv1.GrowFilesystemsResponse{}), nil
}

// Exit codes for a program that cannot run, as shells give them.
const (
	exitCannotRun = 126
	exitNotFound  = 127
)

// drainTimeout bounds how long output is read after the process exits: a
// child it left behind may hold the output open for good.
const drainTimeout = 2 * time.Second

// Exec runs a process for as long as the call lasts. The process group is
// killed if the caller goes away first.
func (s *Server) Exec(ctx context.Context, stream *connect.BidiStream[guestv1.ExecRequest, guestv1.ExecResponse]) error {
	if s.Reaper == nil {
		return connect.NewError(connect.CodeUnimplemented, errors.New("this guest runs no processes"))
	}
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("the first message must start the process"))
	}
	std, err := s.open(start)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	defer std.close()

	argv, attr, err := supervise.Spec(s.Root, start.GetProcess(), std.child)
	if errors.Is(err, supervise.ErrNotFound) {
		return cannotRun(stream, exitNotFound, err)
	}
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	if start.GetTty() {
		attr.Sys.Setctty = true
		attr.Sys.Ctty = 0
	}
	if s.Cgroup != nil {
		attr.Sys.UseCgroupFD = true
		attr.Sys.CgroupFD = int(s.Cgroup.Fd())
	}
	proc, err := s.Reaper.Start(argv, attr)
	switch {
	case errors.Is(err, syscall.ENOENT):
		return cannotRun(stream, exitNotFound, err)
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.ENOEXEC):
		return cannotRun(stream, exitCannotRun, err)
	case err != nil:
		return connect.NewError(connect.CodeInternal, fmt.Errorf("starting %s: %w", argv[0], err))
	}
	std.closeChild()

	var sendMu sync.Mutex
	send := func(r *guestv1.ExecResponse) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(r)
	}
	var output sync.WaitGroup
	for _, o := range std.outputs() {
		output.Add(1)
		go s.safely(func() {
			defer output.Done()
			buf := make([]byte, 32<<10)
			for {
				n, err := o.r.Read(buf)
				if n > 0 {
					if serr := send(o.frame(buf[:n])); serr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		})
	}
	stdin := newStdinQueue(std)
	go s.safely(stdin.run)
	inputDone := make(chan struct{})
	go s.safely(func() {
		defer close(inputDone)
		input(stream, std, stdin, proc)
	})

	exited := make(chan supervise.Exit, 1)
	go func() { exited <- proc.Wait() }()
	var exit supervise.Exit
	select {
	case exit = <-exited:
	case <-ctx.Done():
		select {
		case <-exited: // ended as the caller left: its group may be gone
		default:
			_ = syscall.Kill(-proc.Pid, syscall.SIGKILL)
			<-exited
		}
		stdin.stop()
		std.closeParent()
		output.Wait()
		<-inputDone // nothing reads the call once Exec has returned
		return ctx.Err()
	}
	drained := make(chan struct{})
	go func() {
		output.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(drainTimeout):
		std.closeParent() // what is still held open is cut off
		<-drained
	}
	err = send(&guestv1.ExecResponse{Message: &guestv1.ExecResponse_Exit{
		Exit: &guestv1.ExecExit{Code: int32(exit.Code), Signal: int32(exit.Signal)},
	}})
	stdin.stop()
	std.closeParent()
	// The call ends once the caller closes its side, or goes away.
	<-inputDone
	return err
}

// cannotRun answers for a program that cannot run as a shell would: the
// reason on stderr, and the shell's exit code.
func cannotRun(stream *connect.BidiStream[guestv1.ExecRequest, guestv1.ExecResponse], code int32, err error) error {
	if err := stream.Send(stderrFrame([]byte(err.Error() + "\n"))); err != nil {
		return err
	}
	return stream.Send(&guestv1.ExecResponse{Message: &guestv1.ExecResponse_Exit{Exit: &guestv1.ExecExit{Code: code}}})
}

// safely runs f, recovering from a panic in it: one in hangar-init, which is
// PID 1, would take the guest's kernel down.
func (s *Server) safely(f func()) {
	defer func() {
		if r := recover(); r != nil && s.OnPanic != nil {
			s.OnPanic(r)
		}
	}()
	f()
}

// input applies what the caller sends after the start, until it closes its
// side. Stdin is queued, so a signal is not held up behind it.
func input(stream *connect.BidiStream[guestv1.ExecRequest, guestv1.ExecResponse], std *stdio, stdin *stdinQueue, proc *supervise.Process) {
	for {
		req, err := stream.Receive()
		if err != nil {
			return
		}
		switch m := req.GetMessage().(type) {
		case *guestv1.ExecRequest_Stdin:
			stdin.push(m.Stdin)
		case *guestv1.ExecRequest_StdinClose:
			stdin.close()
		case *guestv1.ExecRequest_Resize:
			std.resize(m.Resize)
		case *guestv1.ExecRequest_Signal:
			_ = proc.Signal(syscall.Signal(m.Signal))
		}
	}
}

// maxStdinQueued bounds the stdin a process has yet to read; past it, the
// caller waits, and what it sends after stdin with it.
const maxStdinQueued = 4 << 20

// stdinQueue feeds a process's stdin from its own goroutine.
type stdinQueue struct {
	std     *stdio
	mu      sync.Mutex
	cond    *sync.Cond
	chunks  [][]byte
	queued  int
	closing bool // close stdin once what is queued is written
	stopped bool // nothing more is written
}

func newStdinQueue(std *stdio) *stdinQueue {
	q := &stdinQueue{std: std}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *stdinQueue) push(p []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.queued >= maxStdinQueued && !q.stopped {
		q.cond.Wait()
	}
	if q.stopped || q.closing {
		return
	}
	q.chunks = append(q.chunks, p)
	q.queued += len(p)
	q.cond.Broadcast()
}

func (q *stdinQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closing = true
	q.cond.Broadcast()
}

// stop drops what is queued; a write under way ends as stdin is closed.
func (q *stdinQueue) stop() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stopped = true
	q.chunks, q.queued = nil, 0
	q.cond.Broadcast()
}

func (q *stdinQueue) run() {
	for {
		q.mu.Lock()
		for len(q.chunks) == 0 && !q.closing && !q.stopped {
			q.cond.Wait()
		}
		if q.stopped {
			q.mu.Unlock()
			return
		}
		if len(q.chunks) == 0 { // and closing
			q.mu.Unlock()
			q.std.closeStdin()
			return
		}
		p := q.chunks[0]
		q.chunks = q.chunks[1:]
		q.mu.Unlock()
		_, err := q.std.stdin.Write(p)
		q.mu.Lock()
		q.queued -= len(p)
		if err != nil {
			q.stopped = true // the process no longer reads it
			q.chunks, q.queued = nil, 0
		}
		q.cond.Broadcast()
		q.mu.Unlock()
	}
}

// stdio is a process's stdin, stdout and stderr: pipes, or a terminal.
type stdio struct {
	child  [3]*os.File
	stdin  io.Writer
	ptmx   *os.File
	pipes  []*os.File // the parent's ends
	stdinW *os.File
	once   sync.Once
}

type output struct {
	r     io.Reader
	frame func([]byte) *guestv1.ExecResponse
}

func stdoutFrame(p []byte) *guestv1.ExecResponse {
	return &guestv1.ExecResponse{Message: &guestv1.ExecResponse_Stdout{Stdout: append([]byte(nil), p...)}}
}

func stderrFrame(p []byte) *guestv1.ExecResponse {
	return &guestv1.ExecResponse{Message: &guestv1.ExecResponse_Stderr{Stderr: append([]byte(nil), p...)}}
}

func (s *Server) open(start *guestv1.ExecStart) (*stdio, error) {
	if start.GetTty() {
		ptmx, tty, err := pty.Open()
		if err != nil {
			return nil, err
		}
		st := &stdio{child: [3]*os.File{tty, tty, tty}, stdin: ptmx, ptmx: ptmx, pipes: []*os.File{ptmx}}
		st.resize(start.GetSize())
		return st, nil
	}
	var files []*os.File
	pipe := func() (r, w *os.File, err error) {
		if r, w, err = os.Pipe(); err == nil {
			files = append(files, r, w)
		}
		return r, w, err
	}
	inR, inW, err := pipe()
	if err == nil {
		var outR, outW, errR, errW *os.File
		if outR, outW, err = pipe(); err == nil {
			if errR, errW, err = pipe(); err == nil {
				return &stdio{
					child: [3]*os.File{inR, outW, errW}, stdin: inW, stdinW: inW,
					pipes: []*os.File{inW, outR, errR},
				}, nil
			}
		}
	}
	for _, f := range files {
		_ = f.Close()
	}
	return nil, err
}

func (st *stdio) outputs() []output {
	if st.ptmx != nil {
		return []output{{st.ptmx, stdoutFrame}}
	}
	return []output{{st.pipes[1], stdoutFrame}, {st.pipes[2], stderrFrame}}
}

func (st *stdio) resize(size *guestv1.WindowSize) {
	if st.ptmx != nil && size != nil {
		_ = pty.Setsize(st.ptmx, &pty.Winsize{Rows: uint16(size.GetRows()), Cols: uint16(size.GetCols())})
	}
}

// closeStdin closes the process's stdin. A terminal has none of its own to
// close; it is sent end-of-file instead.
func (st *stdio) closeStdin() {
	if st.ptmx != nil {
		_, _ = st.ptmx.Write([]byte{4}) // ^D
		return
	}
	_ = st.stdinW.Close()
}

// closeChild closes the child's ends once the child holds them.
func (st *stdio) closeChild() {
	seen := map[*os.File]bool{}
	for _, f := range st.child {
		if f != nil && !seen[f] {
			seen[f] = true
			_ = f.Close()
		}
	}
}

func (st *stdio) closeParent() {
	st.once.Do(func() {
		for _, f := range st.pipes {
			_ = f.Close()
		}
	})
}

func (st *stdio) close() {
	st.closeChild()
	st.closeParent()
}
