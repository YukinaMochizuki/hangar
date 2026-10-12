package supervise

import (
	"sync"
	"syscall"
	"time"
)

// Stopper stops a process that leads its own process group, as the
// container's main process does, the way docker stop does: SIGTERM to the
// group, then SIGKILL to it once a grace period has passed, unless the
// process has ended by then. A stop asked for before the process has
// started applies as it starts. The zero value is ready to use.
type Stopper struct {
	mu       sync.Mutex
	proc     *Process
	stopping bool
	grace    time.Duration
}

// Stop starts stopping the process, once: a second call changes nothing.
// SIGTERM is sent before it returns, if the process has started.
func (s *Stopper) Stop(grace time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return
	}
	s.stopping, s.grace = true, grace
	if s.proc != nil {
		stopGroup(s.proc, grace)
	}
}

// Started hands the stopper the process once it runs.
func (s *Stopper) Started(p *Process) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proc = p
	if s.stopping {
		stopGroup(p, s.grace)
	}
}

// stopGroup sends p's group SIGTERM now and SIGKILL after grace, neither
// once p has ended: its group id may then be another's.
func stopGroup(p *Process, grace time.Duration) {
	select {
	case <-p.Done():
		return
	default:
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGTERM)
	go func() {
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case <-p.Done():
		case <-t.C:
			_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
		}
	}()
}
