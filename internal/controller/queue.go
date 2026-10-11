package controller

import (
	"context"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
)

// queue hands out keys in the order they were first added, each at most once
// while it waits. One worker takes them, so a key is never reconciled twice at
// once; a key added while it is being reconciled waits for the next turn. A
// key whose reconcile failed comes back after a delay that doubles with each
// failure in a row.
type queue struct {
	clock     clockwork.Clock
	base, max time.Duration
	mu        sync.Mutex
	order     []Key
	queued    map[Key]bool
	failures  map[Key]int
	wake      chan struct{}
	closed    bool
}

func newQueue(clock clockwork.Clock, base, max time.Duration) *queue {
	return &queue{
		clock: clock, base: base, max: max,
		queued: map[Key]bool{}, failures: map[Key]int{}, wake: make(chan struct{}, 1),
	}
}

func (q *queue) addAll(keys []Key) {
	for _, k := range keys {
		q.add(k)
	}
}

func (q *queue) add(k Key) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.queued[k] {
		return
	}
	q.queued[k] = true
	q.order = append(q.order, k)
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// get blocks until a key is ready or ctx is done.
func (q *queue) get(ctx context.Context) (Key, bool) {
	for {
		q.mu.Lock()
		if len(q.order) > 0 && !q.closed {
			k := q.order[0]
			q.order = q.order[1:]
			delete(q.queued, k)
			q.mu.Unlock()
			return k, true
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return Key{}, false
		case <-q.wake:
		}
	}
}

// done records how a reconcile of k ended. After a failure it schedules a
// retry and returns the delay; after a success it forgets past failures.
func (q *queue) done(k Key, failed bool) time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !failed {
		delete(q.failures, k)
		return 0
	}
	n := q.failures[k]
	q.failures[k] = n + 1
	delay := q.base << min(n, 30)
	if delay > q.max || delay <= 0 {
		delay = q.max
	}
	q.clock.AfterFunc(delay, func() { q.add(k) })
	return delay
}

func (q *queue) shutdown() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
}
