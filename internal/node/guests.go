package node

import (
	"context"
	"errors"
	"sync"
)

// guests asks off the host lock the guest agents that once let a call to
// them run out its time (docs/decisions/node-agent.md), so a hung guest
// holds the lock, and the worker, for one call per boot. Guarded by the
// host lock.
type guests struct {
	// slow holds the machines whose guest agent let a call run out its
	// time in this boot, until one answers.
	slow map[string]*slowGuest
	// asking holds the machines whose guest agent is being asked, in this
	// boot or one before: one ask at a time reaches a guest.
	asking map[string]bool
	// wg counts the asks under way, for the agent to wait for them as it
	// stops.
	wg sync.WaitGroup
}

// slowGuest is a guest agent asked off the host lock, in one boot.
type slowGuest struct {
	// last is how the last ask of it ended.
	last error
	// answered says an ask ended since its machine was last reconciled.
	answered bool
}

func newGuests() *guests {
	return &guests{slow: map[string]*slowGuest{}, asking: map[string]bool{}}
}

// guestReady asks the machine's guest agent whether it answers, waiting at
// most guestTimeout. A guest that did not answer in time is asked off the
// host lock from then on: until it answers, the reconcile gets how the last
// ask ended, and every machine is checked again when one is answered. The
// caller holds the host lock.
func (a *Agent) guestReady(ctx context.Context, uid string) error {
	g := a.guests
	s := g.slow[uid]
	if s == nil {
		gctx, cancel := context.WithTimeout(ctx, guestTimeout)
		defer cancel()
		err := a.cfg.Drivers.Guest.Ready(gctx, uid)
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			g.slow[uid] = &slowGuest{last: err}
		}
		return err
	}
	if s.answered {
		s.answered = false
		if s.last == nil {
			delete(g.slow, uid)
			return nil
		}
	}
	a.beginAsk(ctx, uid, s)
	return s.last
}

// beginAsk asks the machine's guest agent in the background, unless an ask
// of it is under way. An answer has every machine checked again; a failure
// is left to the machine's own retry, so a guest that keeps not answering
// costs the others nothing. The caller holds the host lock.
func (a *Agent) beginAsk(ctx context.Context, uid string, s *slowGuest) {
	g := a.guests
	if g.asking[uid] {
		return
	}
	g.asking[uid] = true
	g.wg.Go(func() {
		gctx, cancel := context.WithTimeout(ctx, guestTimeout)
		err := a.cfg.Drivers.Guest.Ready(gctx, uid)
		cancel()
		a.host.Lock()
		delete(g.asking, uid)
		// A guest forgotten meanwhile is of a VM since started again, or
		// gone; the answer would speak for a boot it was not asked in.
		current := g.slow[uid] == s
		if current {
			s.last, s.answered = err, true
		}
		a.host.Unlock()
		if current && err == nil {
			a.recheck()
		}
	})
}

// forgetGuest drops what is known of the machine's guest agent, as when its
// VM starts again or the machine is gone. An ask under way ends unheard.
// The caller holds the host lock.
func (a *Agent) forgetGuest(uid string) {
	delete(a.guests.slow, uid)
}
