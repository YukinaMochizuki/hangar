package node

import (
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
)

// throttle runs fn at most once per every: a poke runs it at once if it
// has not run for every, and otherwise once every has passed since it
// last did, once however many pokes came between.
type throttle struct {
	clock clockwork.Clock
	every time.Duration
	fn    func()

	mu      sync.Mutex
	last    time.Time
	pending bool
}

func newThrottle(clock clockwork.Clock, every time.Duration, fn func()) *throttle {
	return &throttle{clock: clock, every: every, fn: fn}
}

func (t *throttle) poke() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending {
		return
	}
	now := t.clock.Now()
	if wait := t.last.Add(t.every).Sub(now); !t.last.IsZero() && wait > 0 {
		t.pending = true
		t.clock.AfterFunc(wait, func() {
			t.mu.Lock()
			t.pending, t.last = false, t.clock.Now()
			t.mu.Unlock()
			t.fn()
		})
		return
	}
	t.last = now
	t.fn()
}
