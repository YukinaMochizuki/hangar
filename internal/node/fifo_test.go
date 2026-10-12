package node

import (
	"context"
	"testing"
)

// TestATurnCancelledAsItComesIsPassedOn: a waiter whose turn comes as it
// gives up passes the slot on, rather than losing it. Its turn and its
// cancellation come together, so either may be seen first; many rounds
// see both.
func TestATurnCancelledAsItComesIsPassedOn(t *testing.T) {
	s := newFIFO(1)
	for range 2000 {
		if err := s.acquire(t.Context()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		got := make(chan error, 1)
		go func() { got <- s.acquire(ctx) }()
		for s.waiting() == 0 {
			// Until it waits.
		}
		cancel()
		s.release()
		if err := <-got; err == nil {
			s.release()
		}
		s.mu.Lock()
		free, queued := s.free, len(s.queue)
		s.mu.Unlock()
		if free != 1 || queued != 0 {
			t.Fatalf("%d slots free and %d waiting, want the one slot free", free, queued)
		}
	}
}
