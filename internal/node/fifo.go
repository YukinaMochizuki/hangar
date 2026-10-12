package node

import (
	"context"
	"slices"
	"sync"
)

// fifo is a semaphore that serves its waiters in the order they came.
type fifo struct {
	mu    sync.Mutex
	free  int
	queue []chan struct{}
}

func newFIFO(n int) *fifo { return &fifo{free: n} }

// acquire takes a slot, waiting its turn, unless ctx is done first.
func (s *fifo) acquire(ctx context.Context) error {
	s.mu.Lock()
	if s.free > 0 && len(s.queue) == 0 {
		s.free--
		s.mu.Unlock()
		return nil
	}
	turn := make(chan struct{})
	s.queue = append(s.queue, turn)
	s.mu.Unlock()
	select {
	case <-turn:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		if i := slices.Index(s.queue, turn); i >= 0 {
			s.queue = slices.Delete(s.queue, i, i+1)
			s.mu.Unlock()
			return ctx.Err()
		}
		s.mu.Unlock()
		// Its turn came as ctx was done: the slot goes to the next.
		s.release()
		return ctx.Err()
	}
}

// release gives a slot back, to the longest waiting first.
func (s *fifo) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) > 0 {
		close(s.queue[0])
		s.queue = s.queue[1:]
		return
	}
	s.free++
}

// waiting returns how many wait for a slot.
func (s *fifo) waiting() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}
