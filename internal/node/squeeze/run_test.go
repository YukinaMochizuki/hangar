package squeeze_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/hangar/internal/node/squeeze"
)

type call struct {
	at       time.Duration
	pressure bool
}

// TestRunPollsOnPressure: polls come at once, then every period, at once
// on an event, every HotPoll while the poll before was hot, each of those
// for pressure, and never two within MinGap.
func TestRunPollsOnPressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		events := make(chan struct{}, 1)
		var mu sync.Mutex
		var calls []call
		hot := map[int]bool{3: true, 4: true}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			squeeze.Run(ctx, clockwork.NewRealClock(), events, 5*time.Second, func(_ context.Context, pressure bool) bool {
				mu.Lock()
				defer mu.Unlock()
				calls = append(calls, call{time.Since(start), pressure})
				return hot[len(calls)]
			})
		}()
		time.Sleep(6 * time.Second) //nolint:forbidigo // virtual time
		events <- struct{}{}
		synctest.Wait()
		// Two events within MinGap of the last poll: one poll, after it.
		time.Sleep(10 * time.Second) //nolint:forbidigo // virtual time
		events <- struct{}{}
		synctest.Wait()
		cancel()
		<-done
		want := []call{
			{0, false},
			{5 * time.Second, false},
			{6 * time.Second, true},
			{7 * time.Second, true},
			{8 * time.Second, true},
			{13 * time.Second, false},
			{16 * time.Second, true},
		}
		mu.Lock()
		defer mu.Unlock()
		if !slices.Equal(calls, want) {
			t.Errorf("polled %v, want %v", calls, want)
		}
	})
}
