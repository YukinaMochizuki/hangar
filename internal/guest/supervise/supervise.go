// Package supervise starts processes in the guest and learns how they end.
// hangar-init is PID 1, so every orphan in the guest becomes its child and
// must be reaped; one reaper does all the reaping and hands each exit to
// whoever started that process. os/exec's Wait cannot be used beside it: it
// would race the reaper for the same exits.
package supervise

import (
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Exit is how a process ended.
type Exit struct {
	// Code is the exit code, or 128 plus the signal's number.
	Code int
	// Signal is the signal that ended it, 0 if none.
	Signal int
}

func exitOf(ws unix.WaitStatus) Exit {
	if ws.Signaled() {
		return Exit{Code: 128 + int(ws.Signal()), Signal: int(ws.Signal())}
	}
	return Exit{Code: ws.ExitStatus()}
}

// Reaper reaps every child of the process and hands the exits of the ones
// it started to their waiters.
type Reaper struct {
	mu      sync.Mutex
	waiting map[int]*Process
	sigs    chan os.Signal
	done    chan struct{}
	stopped chan struct{}
}

// NewReaper starts reaping. Stop ends it. A process has one at a time: two
// would take each other's exits.
func NewReaper() *Reaper {
	r := &Reaper{waiting: map[int]*Process{}, sigs: make(chan os.Signal, 1), done: make(chan struct{}), stopped: make(chan struct{})}
	signal.Notify(r.sigs, unix.SIGCHLD)
	go r.loop()
	return r
}

// Stop stops reaping. Once it returns this reaper reaps nothing more, so a
// reaper started after it gets every exit.
func (r *Reaper) Stop() {
	signal.Stop(r.sigs)
	close(r.done)
	<-r.stopped
}

func (r *Reaper) loop() {
	defer close(r.stopped)
	for {
		select {
		case <-r.done:
			return
		case <-r.sigs:
		}
		// select picks at random when both are ready.
		select {
		case <-r.done:
			return
		default:
		}
		r.reap()
	}
}

// Shield keeps signals from ending this process, as PID 1 needs: Go's
// runtime exits on SIGTERM, SIGINT, SIGHUP or SIGQUIT, and a signal sent
// to it with kill(2), such as SIGSEGV, would crash it.
func Shield() {
	// Caught rather than ignored: an ignored signal stays ignored in the
	// processes started later, a caught one is reset when they exec.
	c := make(chan os.Signal, 16)
	signal.Notify(c)
	go func() {
		for range c {
		}
	}()
}

// reap collects every child that has ended. It runs under r.mu, as Start
// does, so a process is registered before its exit can be reaped; an exit
// nobody waits for is an orphan's, and is dropped.
func (r *Reaper) reap() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
		if p, ok := r.waiting[pid]; ok {
			delete(r.waiting, pid)
			p.exit <- exitOf(ws)
			close(p.done)
			_ = p.proc.Release()
		}
	}
}

// Process is a started process.
type Process struct {
	Pid  int
	exit chan Exit
	done chan struct{}
	// proc signals through its pidfd, so never a later process given the
	// same pid; it is not waited on, the reaper reaps.
	proc *os.Process
}

// Wait returns how the process ended, once it has.
func (p *Process) Wait() Exit {
	e := <-p.exit
	p.exit <- e // later calls get it too
	return e
}

// Done is closed once the process has ended.
func (p *Process) Done() <-chan struct{} { return p.done }

// Signal sends the process a signal, unless it has ended.
func (p *Process) Signal(sig syscall.Signal) error {
	return p.proc.Signal(sig)
}

// Start starts argv[0] (a path; see LookPath) with attr. Its exit is
// delivered to the returned Process however soon it comes.
func (r *Reaper) Start(argv []string, attr *os.ProcAttr) (*Process, error) {
	if len(argv) == 0 {
		return nil, errors.New("no program to run")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := os.StartProcess(argv[0], argv, attr)
	if err != nil {
		return nil, err
	}
	proc := &Process{Pid: p.Pid, exit: make(chan Exit, 1), done: make(chan struct{}), proc: p}
	r.waiting[p.Pid] = proc
	return proc, nil
}
