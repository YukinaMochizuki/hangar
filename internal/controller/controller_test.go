package controller

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
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
	testutil.Eventually(t, "the existing object is reconciled", func() bool { return r.count(Key{"p", "old"}) == 1 })

	if _, err := st.Create(ctx, store.Key{Kind: "A", Project: "p", Name: "new"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "a new object is reconciled", func() bool { return r.count(Key{"p", "new"}) == 1 })
	if _, err := st.Create(ctx, store.Key{Kind: "B", Name: "old"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "a change to a watched kind maps to its key", func() bool { return r.count(Key{"p", "old"}) == 2 })
	if _, err := st.Create(ctx, store.Key{Kind: "C", Name: "other"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Delete(ctx, store.Key{Kind: "A", Project: "p", Name: "new"}, 0); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "a delete is reconciled", func() bool { return r.count(Key{"p", "new"}) == 2 })
	if n := r.count(Key{Name: "other"}); n != 0 {
		t.Errorf("an unwatched kind was reconciled %d times", n)
	}
}

// memSource is a Source in memory, for the timing tests: they run in a
// synctest bubble, which cannot hold the SQLite store. The tests on
// StoreSource and internal/store/storetest cover the store's watch.
type memSource struct {
	mu      sync.Mutex
	rev     int64
	objects map[store.Key]store.Object
	events  []store.Event
	// changed is closed, and replaced, on every write.
	changed chan struct{}
	// listFailures is how many Lists fail before one succeeds.
	listFailures int
}

func newMemSource(keys ...store.Key) *memSource {
	s := &memSource{objects: map[store.Key]store.Object{}, changed: make(chan struct{})}
	for _, k := range keys {
		s.put(k)
	}
	return s
}

// put creates or updates the object at k.
func (s *memSource) put(k store.Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	typ := store.EventUpdated
	if _, ok := s.objects[k]; !ok {
		typ = store.EventCreated
	}
	s.rev++
	o := store.Object{Key: k, Data: []byte("x"), Revision: s.rev}
	s.objects[k] = o
	s.events = append(s.events, store.Event{Type: typ, Object: o, Revision: s.rev})
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *memSource) List(_ context.Context, kind string) ([]store.Object, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listFailures > 0 {
		s.listFailures--
		return nil, 0, errors.New("database is locked")
	}
	var out []store.Object
	for k, o := range s.objects {
		if k.Kind == kind {
			out = append(out, o)
		}
	}
	return out, s.rev, nil
}

func (s *memSource) Watch(ctx context.Context, kinds []string, since int64, fn func(store.Event) error) error {
	for {
		s.mu.Lock()
		var todo []store.Event
		for _, ev := range s.events {
			if ev.Revision > since && slices.Contains(kinds, ev.Object.Kind) {
				todo = append(todo, ev)
			}
		}
		since = s.rev
		changed := s.changed
		s.mu.Unlock()
		for _, ev := range todo {
			if err := fn(ev); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func TestFailuresAreRetriedWithBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newMemSource(store.Key{Kind: "A", Name: "flaky"})
		r := newRecorder()
		k := Key{Name: "flaky"}
		r.fail[k] = 2
		run(t, &Controller{Name: "t", Source: src, Reconciler: r, Watches: []Watch{{Kind: "A", Map: Self}}})
		want := func(n int, what string) {
			t.Helper()
			synctest.Wait()
			if got := r.count(k); got != n {
				t.Fatalf("%s: %d calls, want %d", what, got, n)
			}
		}
		want(1, "the first attempt")
		time.Sleep(100*time.Millisecond - time.Nanosecond) //nolint:forbidigo // virtual time
		want(1, "just before the first backoff")
		time.Sleep(time.Nanosecond) //nolint:forbidigo // virtual time
		want(2, "the first retry")
		// The second failure doubles the delay.
		time.Sleep(200*time.Millisecond - time.Nanosecond) //nolint:forbidigo // virtual time
		want(2, "just before the doubled delay")
		time.Sleep(time.Nanosecond) //nolint:forbidigo // virtual time
		want(3, "the second retry")
	})
}

func TestAKeyIsNeverReconciledTwiceAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		key := store.Key{Kind: "A", Name: "busy"}
		src := newMemSource(key)
		r := newRecorder()
		r.block = make(chan struct{})
		run(t, &Controller{Name: "t", Source: src, Reconciler: r, Watches: []Watch{{Kind: "A", Map: Self}}})
		k := Key{Name: "busy"}
		synctest.Wait()
		if n := r.count(k); n != 1 {
			t.Fatalf("%d reconciles started, want 1", n)
		}

		// Changes while it runs collapse into one more reconcile afterwards.
		for range 5 {
			src.put(key)
		}
		synctest.Wait()
		r.mu.Lock()
		block := r.block
		r.block = nil
		r.mu.Unlock()
		close(block)
		synctest.Wait()
		if n := r.count(k); n != 2 {
			t.Errorf("five changes during one reconcile caused %d more", n-1)
		}
		if r.overlap.Load() {
			t.Error("one key was reconciled twice at once")
		}
	})
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
	testutil.Eventually(t, "the object is listed again", func() bool { return r.count(Key{Name: "a"}) >= 1 })
	if _, err := st.Create(ctx, store.Key{Kind: "A", Name: "b"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the new watch follows changes", func() bool { return r.count(Key{Name: "b"}) == 1 })
}

// TestRunWaitsForTheReconcileInFlight: Run must not return while a reconcile
// may still be writing, or the caller closes the store under it.
func TestRunWaitsForTheReconcileInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newMemSource(store.Key{Kind: "A", Name: "slow"})
		r := newRecorder()
		r.block = make(chan struct{})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			done <- (&Controller{Name: "t", Source: src, Reconciler: r, Watches: []Watch{{Kind: "A", Map: Self}}}).Run(ctx)
		}()
		synctest.Wait()
		if n := r.count(Key{Name: "slow"}); n != 1 {
			t.Fatalf("%d reconciles started, want 1", n)
		}
		cancel()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("Run returned with a reconcile still running")
		default:
		}
		close(r.block)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// TestAFailingSourceIsRetried: a List that fails, as a busy database's
// may, is tried again a second later.
func TestAFailingSourceIsRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newMemSource(store.Key{Kind: "A", Name: "a"})
		src.listFailures = 1
		r := newRecorder()
		run(t, &Controller{Name: "t", Source: src, Reconciler: r, Watches: []Watch{{Kind: "A", Map: Self}}})
		time.Sleep(time.Second - time.Nanosecond) //nolint:forbidigo // virtual time
		synctest.Wait()
		if n := r.count(Key{Name: "a"}); n != 0 {
			t.Fatalf("%d reconciles before the List was tried again", n)
		}
		time.Sleep(time.Nanosecond) //nolint:forbidigo // virtual time
		synctest.Wait()
		if n := r.count(Key{Name: "a"}); n != 1 {
			t.Errorf("%d reconciles once the List was tried again, want 1", n)
		}
	})
}

func TestResyncReconcilesEverythingAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newMemSource(store.Key{Kind: "A", Name: "a"})
		r := newRecorder()
		run(t, &Controller{Name: "t", Source: src, Reconciler: r, Resync: time.Minute, Watches: []Watch{{Kind: "A", Map: Self}}})
		time.Sleep(time.Minute - time.Nanosecond) //nolint:forbidigo // virtual time
		synctest.Wait()
		if n := r.count(Key{Name: "a"}); n != 1 {
			t.Fatalf("%d reconciles before the resync, want 1", n)
		}
		time.Sleep(time.Nanosecond) //nolint:forbidigo // virtual time
		synctest.Wait()
		if n := r.count(Key{Name: "a"}); n != 2 {
			t.Errorf("%d reconciles after the resync, want 2", n)
		}
	})
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
	testutil.Eventually(t, "the A written between the lists", func() bool { return r.count(Key{Name: "between"}) == 1 })
}

// TestWaitingIsCheckedSoon: a key waiting on something outside the
// controller is checked again within MaxWait, however long it has waited.
func TestWaitingIsCheckedSoon(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newMemSource(store.Key{Kind: "A", Name: "booting"})
		var calls atomic.Int32
		waiting := ReconcilerFunc(func(context.Context, Key) error {
			calls.Add(1)
			return fmt.Errorf("%w: still booting", ErrWaiting)
		})
		run(t, &Controller{Name: "t", Source: src, Reconciler: waiting, MaxWait: 200 * time.Millisecond,
			Watches: []Watch{{Kind: "A", Map: Self}}})
		for i := int32(1); i <= 6; i++ {
			synctest.Wait()
			if n := calls.Load(); n != i {
				t.Fatalf("%d checks, want %d", n, i)
			}
			time.Sleep(200 * time.Millisecond) //nolint:forbidigo // virtual time
		}
	})
}

// TestAFailureAfterWaitsIsRetriedSoon: waiting is not failing, so a key
// that waited many times and then fails is retried after the first
// backoff, not the longest; and a wait after failures is checked again
// within MaxWait.
func TestAFailureAfterWaitsIsRetriedSoon(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newMemSource(store.Key{Kind: "A", Name: "disk"})
		const waits = 12 // enough to reach a one-minute backoff had they counted
		var calls atomic.Int32
		r := ReconcilerFunc(func(context.Context, Key) error {
			switch n := calls.Add(1); {
			case n <= waits:
				return fmt.Errorf("%w: the image's base is being made", ErrWaiting)
			case n <= waits+3:
				return errors.New("injected")
			case n == waits+4:
				return fmt.Errorf("%w: once more", ErrWaiting)
			}
			return nil
		})
		run(t, &Controller{Name: "t", Source: src, Reconciler: r, Watches: []Watch{{Kind: "A", Map: Self}}})
		step := func(n int32, by time.Duration) {
			t.Helper()
			synctest.Wait()
			if got := calls.Load(); got != n {
				t.Fatalf("%d calls, want %d", got, n)
			}
			time.Sleep(by) //nolint:forbidigo // virtual time
		}
		// Each wait in a row doubles the delay, up to MaxWait's default.
		for i := int32(1); i <= waits; i++ {
			step(i, min(100*time.Millisecond<<(i-1), 2*time.Second))
		}
		// The failures back off from the first delay: 100, 200, 400ms.
		step(waits+1, 100*time.Millisecond)
		step(waits+2, 200*time.Millisecond)
		step(waits+3, 400*time.Millisecond)
		// Three failures in a row would next wait 800ms; a wait after them is
		// checked again as soon as a first wait is.
		step(waits+4, 100*time.Millisecond)
		synctest.Wait()
		if n := calls.Load(); n != waits+5 {
			t.Errorf("%d calls, want %d: the reconcile after the last wait", n, waits+5)
		}
	})
}

// TestASkipResyncKindIsMappedOnlyAsItChanges: a kind marked SkipResync is
// not mapped on start or resync, and is as it changes.
func TestASkipResyncKindIsMappedOnlyAsItChanges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newMemSource(store.Key{Kind: "A", Name: "a"}, store.Key{Kind: "B", Name: "before"})
		var mappedB atomic.Int32
		r := newRecorder()
		run(t, &Controller{
			Name: "t", Source: src, Reconciler: r, Resync: time.Minute,
			Watches: []Watch{
				{Kind: "A", Map: Self},
				{Kind: "B", SkipResync: true, Map: func(_ context.Context, o store.Object) []Key {
					mappedB.Add(1)
					return []Key{{Name: o.Name}}
				}},
			},
		})
		time.Sleep(time.Minute) //nolint:forbidigo // virtual time
		synctest.Wait()
		if n := r.count(Key{Name: "a"}); n != 2 {
			t.Fatalf("%d reconciles on start and resync, want 2", n)
		}
		if n := mappedB.Load(); n != 0 {
			t.Errorf("a SkipResync kind was mapped %d times on start and resync", n)
		}
		src.put(store.Key{Kind: "B", Name: "after"})
		synctest.Wait()
		if n := r.count(Key{Name: "after"}); n != 1 {
			t.Errorf("the change was reconciled %d times, want 1", n)
		}
	})
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
