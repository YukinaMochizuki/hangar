package node

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// scriptedGuest answers the calls it was told to at once, and each other
// call when the test answers it, or when the caller gives up.
type scriptedGuest struct {
	driver.Guest
	mu sync.Mutex
	// now holds the answers given at once, by call number.
	now map[int]error
	// later holds the answers to give the other calls, by call number.
	later    map[int]chan error
	started  chan int
	n        int
	inflight int
	most     int
}

func newScriptedGuest() *scriptedGuest {
	return &scriptedGuest{now: map[int]error{}, later: map[int]chan error{}, started: make(chan int, 100)}
}

func (g *scriptedGuest) Ready(ctx context.Context, uid string) error {
	g.mu.Lock()
	n := g.n
	g.n++
	if err, ok := g.now[n]; ok {
		g.mu.Unlock()
		return err
	}
	ch := make(chan error, 1)
	g.later[n] = ch
	g.inflight++
	g.most = max(g.most, g.inflight)
	g.mu.Unlock()
	g.started <- n
	defer func() {
		g.mu.Lock()
		g.inflight--
		g.mu.Unlock()
	}()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// script has call n answered at once with err.
func (g *scriptedGuest) script(n int, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.now[n] = err
}

func (g *scriptedGuest) answer(n int, err error) {
	g.mu.Lock()
	ch := g.later[n]
	g.mu.Unlock()
	ch <- err
}

func (g *scriptedGuest) calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}

type guestWorld struct {
	t      *testing.T
	a      *Agent
	g      *scriptedGuest
	cancel context.CancelFunc
	ctx    context.Context
}

func newGuestWorld(t *testing.T) *guestWorld {
	g := newScriptedGuest()
	ctx, cancel := context.WithCancel(context.Background())
	w := &guestWorld{t: t, g: g, ctx: ctx, cancel: cancel, a: &Agent{
		cfg:     Config{Drivers: &Drivers{Guest: g}},
		guests:  newGuests(),
		trigger: make(chan struct{}, 1),
	}}
	// No ask outlives the agent.
	t.Cleanup(func() {
		cancel()
		done := make(chan struct{})
		go func() { w.a.guests.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("an ask of a guest outlived the agent")
		}
	})
	return w
}

// ready is what a reconcile of the machine gets of its guest.
func (w *guestWorld) ready() error {
	w.a.host.Lock()
	defer w.a.host.Unlock()
	return w.a.guestReady(w.ctx, "m")
}

func (w *guestWorld) restarted() {
	w.a.host.Lock()
	defer w.a.host.Unlock()
	w.a.forgetGuest("m")
}

func (w *guestWorld) askStarted(n int) {
	w.t.Helper()
	select {
	case got := <-w.g.started:
		if got != n {
			w.t.Fatalf("call %d asked in the background, want %d", got, n)
		}
	case <-time.After(time.Second):
		w.t.Fatalf("call %d never asked", n)
	}
}

func (w *guestWorld) rechecked() bool {
	select {
	case <-w.a.trigger:
		return true
	case <-time.After(200 * time.Millisecond):
		return false
	}
}

// TestAHungGuestIsAskedOffTheLockOnce: a guest that let a call run out is
// asked in the background, one ask at a time however often its machine is
// reconciled, and every machine is checked again once it answers.
func TestAHungGuestIsAskedOffTheLockOnce(t *testing.T) {
	w := newGuestWorld(t)
	w.g.script(0, context.DeadlineExceeded)
	if err := w.ready(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first ask: %v", err)
	}
	for range 5 {
		if err := w.ready(); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("while asked in the background: %v", err)
		}
	}
	w.askStarted(1)
	if n := w.g.calls(); n != 2 {
		t.Errorf("%d calls, want the first and one in the background", n)
	}

	// A failure is the machine's own to retry: it checks no other one.
	w.g.answer(1, errors.New("refused"))
	if w.rechecked() {
		t.Error("a failed ask had every machine checked again")
	}
	if err := w.ready(); err == nil || err.Error() != "refused" {
		t.Fatalf("after a failed ask: %v", err)
	}
	w.askStarted(2)

	w.g.answer(2, nil)
	if !w.rechecked() {
		t.Fatal("an answer did not have every machine checked again")
	}
	if err := w.ready(); err != nil {
		t.Fatalf("after an answer: %v", err)
	}
	// It answers: it is asked under the lock again.
	w.g.script(3, nil)
	if err := w.ready(); err != nil {
		t.Fatal(err)
	}
	w.g.mu.Lock()
	defer w.g.mu.Unlock()
	if w.g.most != 1 {
		t.Errorf("%d asks of one guest at once", w.g.most)
	}
}

// TestAnAskOfAnEarlierBootIsNotHeard: an answer to an ask begun before the
// VM started again speaks for no guest, and no second background ask
// begins while it is under way.
func TestAnAskOfAnEarlierBootIsNotHeard(t *testing.T) {
	w := newGuestWorld(t)
	w.g.script(0, context.DeadlineExceeded)
	_ = w.ready()
	_ = w.ready()
	w.askStarted(1)

	w.restarted()
	w.g.script(2, context.DeadlineExceeded)
	if err := w.ready(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first ask of the new boot: %v", err)
	}
	_ = w.ready()
	if n := w.g.calls(); n != 3 {
		t.Fatalf("%d calls, want no second background ask", n)
	}

	w.g.answer(1, nil)
	if w.rechecked() {
		t.Error("an answer of the earlier boot had every machine checked again")
	}
	// The earlier boot's ask has ended; the new one's begins.
	if err := w.ready(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an answer of the earlier boot was heard: %v", err)
	}
	w.askStarted(3)
	w.g.answer(3, nil)
	if !w.rechecked() {
		t.Fatal("an answer did not have every machine checked again")
	}
	if err := w.ready(); err != nil {
		t.Fatalf("after the new boot's answer: %v", err)
	}
}

// TestAnAskEndsWithTheAgent: an ask under way ends when the agent does.
func TestAnAskEndsWithTheAgent(t *testing.T) {
	w := newGuestWorld(t)
	w.g.script(0, context.DeadlineExceeded)
	_ = w.ready()
	_ = w.ready()
	w.askStarted(1)
	// newGuestWorld's cleanup cancels and waits for it.
}
