// Package controller runs level-triggered reconcilers inside the apiserver.
// A Controller follows some kinds in the store, turns every change into the
// keys it affects, and calls Reconcile for each until it succeeds. Reconcile
// is handed only a key, never the event, so it always works from the current
// state and a missed or repeated event cannot lead it astray. See
// docs/checklists/new-controller.md.
package controller

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/hangar/internal/store"
)

// Key names one object of the kind a reconciler is responsible for. Project
// is empty for global kinds.
type Key struct {
	Project string
	Name    string
}

func (k Key) String() string {
	if k.Project == "" {
		return k.Name
	}
	return k.Project + "/" + k.Name
}

// Reconciler brings the world in line with the object at key. It must be
// idempotent: it is called again after every relevant change, on every
// resync, and after any error.
type Reconciler interface {
	Reconcile(ctx context.Context, key Key) error
}

// ReconcilerFunc adapts a function to Reconciler.
type ReconcilerFunc func(ctx context.Context, key Key) error

func (f ReconcilerFunc) Reconcile(ctx context.Context, key Key) error { return f(ctx, key) }

// Watch says which keys a change to one kind affects. Map is given the
// object as written (or, for a delete, as last stored) and returns the keys
// to reconcile; it may read the store.
type Watch struct {
	Kind string
	Map  func(ctx context.Context, o store.Object) []Key
}

// Self maps an object of the reconciled kind to its own key.
func Self(_ context.Context, o store.Object) []Key {
	return []Key{{Project: o.Project, Name: o.Name}}
}

// Controller feeds a Reconciler. Run it with Run.
type Controller struct {
	// Name appears in logs.
	Name  string
	Store store.Store
	// Watches lists every kind whose changes matter; each object of every
	// watched kind is also mapped on start and on every resync.
	Watches    []Watch
	Reconciler Reconciler
	// Resync is how often every object is mapped again, catching anything a
	// reconcile could not see coming (time passing, for one). Zero means five
	// minutes.
	Resync time.Duration
	// MaxBackoff caps the delay before a failing key is retried. Zero means
	// one minute; the first retry comes after 100ms.
	MaxBackoff time.Duration
	Clock      clockwork.Clock
	Log        *slog.Logger
}

// Run reconciles until ctx is done, and returns once every reconcile it
// started has finished. A failing store is retried, never fatal: one
// controller's trouble must not stop the apiserver's other background work.
func (c *Controller) Run(ctx context.Context) error {
	if c.Clock == nil {
		c.Clock = clockwork.NewRealClock()
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Resync == 0 {
		c.Resync = 5 * time.Minute
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = time.Minute
	}
	log := c.Log.With("controller", c.Name)
	q := newQueue(c.Clock, 100*time.Millisecond, c.MaxBackoff)
	var wg sync.WaitGroup
	defer func() {
		q.shutdown()
		wg.Wait()
	}()

	wg.Add(2)
	go func() {
		defer wg.Done()
		c.work(ctx, q, log)
	}()
	go func() {
		defer wg.Done()
		ticker := c.Clock.NewTicker(c.Resync)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.Chan():
				if _, err := c.mapAll(ctx, q); err != nil && ctx.Err() == nil {
					log.Error("resync failed", "error", err)
				}
			}
		}
	}()

	for {
		rev, err := c.mapAll(ctx, q)
		if err == nil {
			err = c.follow(ctx, q, rev)
		}
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, store.ErrCompacted) {
			log.Info("watch fell behind compaction; listing again")
			continue
		}
		log.Error("following the store failed; listing again shortly", "error", err)
		select {
		case <-ctx.Done():
			return nil
		case <-c.Clock.After(time.Second):
		}
	}
}

// mapAll queues the keys of every object of every watched kind, and returns
// a revision no later than the earliest list, so a watch from it misses
// nothing.
func (c *Controller) mapAll(ctx context.Context, q *queue) (int64, error) {
	rev, err := c.Store.Revision(ctx)
	if err != nil {
		return 0, err
	}
	for _, w := range c.Watches {
		objs, _, err := c.Store.List(ctx, store.ListOptions{Kind: w.Kind})
		if err != nil {
			return 0, err
		}
		for _, o := range objs {
			q.addAll(w.Map(ctx, o))
		}
	}
	return rev, nil
}

func (c *Controller) follow(ctx context.Context, q *queue, since int64) error {
	byKind := map[string]Watch{}
	for _, w := range c.Watches {
		byKind[w.Kind] = w
	}
	return c.Store.Watch(ctx, store.WatchOptions{SinceRevision: since}, func(ev store.Event) error {
		if w, ok := byKind[ev.Object.Kind]; ok {
			q.addAll(w.Map(ctx, ev.Object))
		}
		return nil
	})
}

func (c *Controller) work(ctx context.Context, q *queue, log *slog.Logger) {
	for {
		key, ok := q.get(ctx)
		if !ok {
			return
		}
		err := c.Reconciler.Reconcile(ctx, key)
		switch {
		case err == nil:
			q.done(key, false)
		case ctx.Err() != nil:
			q.done(key, false)
			return
		default:
			delay := q.done(key, true)
			log.Warn("reconcile failed; retrying", "key", key.String(), "in", delay, "error", err)
		}
	}
}
