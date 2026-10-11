package controller

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
)

func openStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// recorder is a reconciler that counts calls per key and can be made to fail.
type recorder struct {
	mu      sync.Mutex
	calls   map[Key]int
	fail    map[Key]int
	running map[Key]bool
	overlap atomic.Bool
	block   chan struct{}
}

func newRecorder() *recorder {
	return &recorder{calls: map[Key]int{}, fail: map[Key]int{}, running: map[Key]bool{}}
}

func (r *recorder) Reconcile(_ context.Context, k Key) error {
	r.mu.Lock()
	if r.running[k] {
		r.overlap.Store(true)
	}
	r.running[k] = true
	r.calls[k]++
	failing := r.fail[k] > 0
	if failing {
		r.fail[k]--
	}
	block := r.block
	r.mu.Unlock()
	if block != nil {
		<-block
	}
	r.mu.Lock()
	r.running[k] = false
	r.mu.Unlock()
	if failing {
		return errors.New("injected")
	}
	return nil
}

func (r *recorder) count(k Key) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[k]
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func run(t *testing.T, c *Controller) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
}

func TestReconcilesExistingObjectsThenChanges(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	if _, err := st.Create(ctx, store.Key{Kind: "A", Project: "p", Name: "old"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	r := newRecorder()
	run(t, &Controller{
		Name: "t", Source: StoreSource{Store: st}, Reconciler: r, Clock: clockwork.NewFakeClock(),
		Watches: []Watch{
			{Kind: "A", Map: Self},
			// A change to a B reconciles the A of the same name.
			{Kind: "B", Map: func(_ context.Context, o store.Object) []Key { return []Key{{Project: "p", Name: o.Name}} }},
		},
	})
	eventually(t, "the existing object is reconciled", func() bool { return r.count(Key{"p", "old"}) == 1 })

	if _, err := st.Create(ctx, store.Key{Kind: "A", Project: "p", Name: "new"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a new object is reconciled", func() bool { return r.count(Key{"p", "new"}) == 1 })
	if _, err := st.Create(ctx, store.Key{Kind: "B", Name: "old"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a change to a watched kind maps to its key", func() bool { return r.count(Key{"p", "old"}) == 2 })
	if _, err := st.Create(ctx, store.Key{Kind: "C", Name: "other"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Delete(ctx, store.Key{Kind: "A", Project: "p", Name: "new"}, 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a delete is reconciled", func() bool { return r.count(Key{"p", "new"}) == 2 })
	if n := r.count(Key{Name: "other"}); n != 0 {
		t.Errorf("an unwatched kind was reconciled %d times", n)
	}
}

func TestFailuresAreRetriedWithBackoff(t *testing.T) {
	st := openStore(t)
	clock := clockwork.NewFakeClock()
	if _, err := st.Create(t.Context(), store.Key{Kind: "A", Name: "flaky"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	r := newRecorder()
	k := Key{Name: "flaky"}
	r.fail[k] = 2
	run(t, &Controller{Name: "t", Source: StoreSource{Store: st}, Reconciler: r, Clock: clock, Watches: []Watch{{Kind: "A", Map: Self}}})

	eventually(t, "the first attempt", func() bool { return r.count(k) == 1 })
	// The retry waits for its backoff: nothing happens until the clock moves.
	time.Sleep(50 * time.Millisecond)
	if n := r.count(k); n != 1 {
		t.Fatalf("retried before the backoff: %d calls", n)
	}
	if err := clock.BlockUntilContext(t.Context(), 2); err != nil { // the resync ticker and the retry
		t.Fatal(err)
	}
	clock.Advance(100 * time.Millisecond)
	eventually(t, "the first retry", func() bool { return r.count(k) == 2 })
	// The second failure doubles the delay.
	if err := clock.BlockUntilContext(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	clock.Advance(100 * time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	if n := r.count(k); n != 2 {
		t.Fatalf("the second retry came before its doubled delay: %d calls", n)
	}
	clock.Advance(100 * time.Millisecond)
	eventually(t, "the second retry", func() bool { return r.count(k) == 3 })
}

func TestAKeyIsNeverReconciledTwiceAtOnce(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	key := store.Key{Kind: "A", Name: "busy"}
	if _, err := st.Create(ctx, key, []byte("0")); err != nil {
		t.Fatal(err)
	}
	r := newRecorder()
	r.block = make(chan struct{})
	run(t, &Controller{Name: "t", Source: StoreSource{Store: st}, Reconciler: r, Clock: clockwork.NewFakeClock(), Watches: []Watch{{Kind: "A", Map: Self}}})
	k := Key{Name: "busy"}
	eventually(t, "the first reconcile starts", func() bool { return r.count(k) == 1 })

	// Changes while it runs collapse into one more reconcile afterwards.
	for i := range 5 {
		if _, err := st.Update(ctx, key, []byte{byte(i)}, 0); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	r.mu.Lock()
	block := r.block
	r.block = nil
	r.mu.Unlock()
	close(block)
	eventually(t, "the changes are reconciled", func() bool { return r.count(k) == 2 })
	time.Sleep(50 * time.Millisecond)
	if n := r.count(k); n != 2 {
		t.Errorf("five changes during one reconcile caused %d more", n-1)
	}
	if r.overlap.Load() {
		t.Error("one key was reconciled twice at once")
	}
}

// compactingStore makes the first watch fail as if it fell behind
// compaction.
type compactingStore struct {
	store.Store
	failed atomic.Bool
}

func (s *compactingStore) Watch(ctx context.Context, opts store.WatchOptions, send func(store.Event) error) error {
	if s.failed.CompareAndSwap(false, true) {
		return store.ErrCompacted
	}
	return s.Store.Watch(ctx, opts, send)
}

func TestRelistsAfterCompaction(t *testing.T) {
	st := &compactingStore{Store: openStore(t)}
	ctx := t.Context()
	if _, err := st.Create(ctx, store.Key{Kind: "A", Name: "a"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	r := newRecorder()
	run(t, &Controller{Name: "t", Source: StoreSource{Store: st}, Reconciler: r, Clock: clockwork.NewFakeClock(), Watches: []Watch{{Kind: "A", Map: Self}}})
	eventually(t, "the object is listed again", func() bool { return r.count(Key{Name: "a"}) >= 1 })
	if _, err := st.Create(ctx, store.Key{Kind: "A", Name: "b"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the new watch follows changes", func() bool { return r.count(Key{Name: "b"}) == 1 })
}

// TestRunWaitsForTheReconcileInFlight: Run must not return while a reconcile
// may still be writing, or the caller closes the store under it.
func TestRunWaitsForTheReconcileInFlight(t *testing.T) {
	st := openStore(t)
	if _, err := st.Create(t.Context(), store.Key{Kind: "A", Name: "slow"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	r := newRecorder()
	r.block = make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- (&Controller{Name: "t", Source: StoreSource{Store: st}, Reconciler: r, Clock: clockwork.NewFakeClock(), Watches: []Watch{{Kind: "A", Map: Self}}}).Run(ctx)
	}()
	eventually(t, "the reconcile starts", func() bool { return r.count(Key{Name: "slow"}) == 1 })
	cancel()
	select {
	case <-done:
		t.Fatal("Run returned with a reconcile still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(r.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// flakyStore fails its first List the way a busy database might.
type flakyStore struct {
	store.Store
	failed atomic.Bool
}

func (s *flakyStore) List(ctx context.Context, opts store.ListOptions) ([]store.Object, int64, error) {
	if s.failed.CompareAndSwap(false, true) {
		return nil, 0, errors.New("database is locked")
	}
	return s.Store.List(ctx, opts)
}

func TestAFailingStoreIsRetried(t *testing.T) {
	st := &flakyStore{Store: openStore(t)}
	clock := clockwork.NewFakeClock()
	if _, err := st.Create(t.Context(), store.Key{Kind: "A", Name: "a"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	r := newRecorder()
	run(t, &Controller{Name: "t", Source: StoreSource{Store: st}, Reconciler: r, Clock: clock, Watches: []Watch{{Kind: "A", Map: Self}}})
	if err := clock.BlockUntilContext(t.Context(), 2); err != nil { // resync ticker and the retry wait
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	eventually(t, "the controller recovers", func() bool { return r.count(Key{Name: "a"}) == 1 })
}

func TestResyncReconcilesEverythingAgain(t *testing.T) {
	st := openStore(t)
	clock := clockwork.NewFakeClock()
	if _, err := st.Create(t.Context(), store.Key{Kind: "A", Name: "a"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	r := newRecorder()
	run(t, &Controller{Name: "t", Source: StoreSource{Store: st}, Reconciler: r, Clock: clock, Resync: time.Minute, Watches: []Watch{{Kind: "A", Map: Self}}})
	eventually(t, "the first reconcile", func() bool { return r.count(Key{Name: "a"}) == 1 })
	if err := clock.BlockUntilContext(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	eventually(t, "the resync", func() bool { return r.count(Key{Name: "a"}) == 2 })
}

// writeBetweenLists writes an A while the controller lists Bs, after it
// listed As.
type writeBetweenLists struct {
	StoreSource
	once sync.Once
}

func (w *writeBetweenLists) List(ctx context.Context, kind string) ([]store.Object, int64, error) {
	if kind == "B" {
		var err error
		w.once.Do(func() {
			_, err = w.Store.Create(ctx, store.Key{Kind: "A", Name: "between"}, []byte("x"))
		})
		if err != nil {
			return nil, 0, err
		}
	}
	return w.StoreSource.List(ctx, kind)
}

// TestAChangeBetweenListsIsNotMissed: the watch starts from the earliest
// list, so a change made after one kind was listed is still seen.
func TestAChangeBetweenListsIsNotMissed(t *testing.T) {
	st := openStore(t)
	r := newRecorder()
	run(t, &Controller{
		Name: "t", Source: &writeBetweenLists{StoreSource: StoreSource{Store: st}}, Reconciler: r, Clock: clockwork.NewFakeClock(),
		Watches: []Watch{{Kind: "A", Map: Self}, {Kind: "B", Map: Self}},
	})
	eventually(t, "the A written between the lists", func() bool { return r.count(Key{Name: "between"}) == 1 })
}

// TestWaitingIsCheckedSoon: a key waiting on something outside the
// controller is checked again within MaxWait, however long it has waited.
func TestWaitingIsCheckedSoon(t *testing.T) {
	st := openStore(t)
	clock := clockwork.NewFakeClock()
	if _, err := st.Create(t.Context(), store.Key{Kind: "A", Name: "booting"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	waiting := ReconcilerFunc(func(context.Context, Key) error {
		calls.Add(1)
		return fmt.Errorf("%w: still booting", ErrWaiting)
	})
	run(t, &Controller{Name: "t", Source: StoreSource{Store: st}, Reconciler: waiting, Clock: clock, MaxWait: 200 * time.Millisecond,
		Watches: []Watch{{Kind: "A", Map: Self}}})
	for i := int32(1); i <= 6; i++ {
		eventually(t, fmt.Sprintf("check %d", i), func() bool { return calls.Load() == i })
		if err := clock.BlockUntilContext(t.Context(), 2); err != nil { // the resync ticker and the retry
			t.Fatal(err)
		}
		clock.Advance(200 * time.Millisecond)
	}
}

// TestASkipResyncKindIsMappedOnlyAsItChanges: a kind marked SkipResync is
// not mapped on start or resync, and is as it changes.
func TestASkipResyncKindIsMappedOnlyAsItChanges(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	clock := clockwork.NewFakeClock()
	for _, k := range []store.Key{{Kind: "A", Name: "a"}, {Kind: "B", Name: "before"}} {
		if _, err := st.Create(ctx, k, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	var mappedB atomic.Int32
	r := newRecorder()
	run(t, &Controller{
		Name: "t", Source: StoreSource{Store: st}, Reconciler: r, Clock: clock, Resync: time.Minute,
		Watches: []Watch{
			{Kind: "A", Map: Self},
			{Kind: "B", SkipResync: true, Map: func(_ context.Context, o store.Object) []Key {
				mappedB.Add(1)
				return []Key{{Name: o.Name}}
			}},
		},
	})
	eventually(t, "the start", func() bool { return r.count(Key{Name: "a"}) == 1 })
	if err := clock.BlockUntilContext(ctx, 1); err != nil { // the resync ticker
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	eventually(t, "the resync", func() bool { return r.count(Key{Name: "a"}) == 2 })
	if n := mappedB.Load(); n != 0 {
		t.Errorf("a SkipResync kind was mapped %d times on start and resync", n)
	}
	if _, err := st.Create(ctx, store.Key{Kind: "B", Name: "after"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the change mapped", func() bool { return r.count(Key{Name: "after"}) == 1 })
}

// TestAControllerMustListSomething: a controller whose every watch skips
// resyncs would start from nothing; it refuses to run.
func TestAControllerMustListSomething(t *testing.T) {
	c := &Controller{Name: "t", Source: StoreSource{Store: openStore(t)}, Reconciler: newRecorder(),
		Watches: []Watch{{Kind: "A", Map: Self, SkipResync: true}}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := c.Run(ctx); err == nil {
		t.Error("a controller that lists nothing ran")
	}
}
