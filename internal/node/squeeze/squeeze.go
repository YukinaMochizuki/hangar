// Package squeeze keeps the machines of a memory pool on a node within
// the pool's size by their balloons (docs/decisions/node-agent.md): at
// each poll it reads what they hold and what their guests use and need,
// and holds each Shared machine's guest at its allowance, a size its
// balloon gives it, never handing out more than the balloons have given
// back. It keeps nothing from one poll to the next but each guest's last
// count of its reclaims. It neither polls nor knows which machines are in
// a pool: the node agent does.
package squeeze

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// Bounds of the squeeze, from the balloon's measurements on a testbed
// (docs/decisions/node-agent.md): a balloon takes memory back at about
// 440 MiB/s, and gives it back in 0.2 s.
const (
	// StepMiB is the most a member's balloon grows by in one calm poll;
	// PressureStepMiB in one polled for pressure, about what the balloon
	// takes back in the second between such polls. A balloon shrinks by
	// as much as there is room for at once.
	StepMiB         = 512
	PressureStepMiB = 1024
	// MarginMiB and MarginPercent are what a guest is left past what it
	// uses, the larger of 256 MiB and a tenth of its memory: its safe
	// floor, below which it is squeezed only when its pool is short. A
	// guest with less of its size available needs more.
	MarginMiB     = 256
	MarginPercent = 10
	// FullPercent is how much of its size a guest that reports nothing of
	// its reclaims holds on the host, in hundredths, once it needs more.
	FullPercent = 90
	// DeadbandMiB is how far a guest's size may be from its allowance
	// before its balloon is set: an allowance moves a little at each poll.
	DeadbandMiB = 32
	// BootWindow is how long after its VM started a Shared guest may still
	// be inflating the balloon it boots with: hangar-init's wait for it
	// (boot.BalloonTimeout), after which it runs, and counts at what it has.
	// BootSlackMiB is how far above its reservation its size may be once
	// inflated (boot.BalloonSlackMiB).
	BootWindow   = 15 * time.Second
	BootSlackMiB = 64
	// StaleStats is how old a guest's report of its memory may be before
	// it says nothing: Cloud Hypervisor asks for a new one each time it is
	// asked for one, a poll apart.
	StaleStats = 30 * time.Second
)

// Member is a running machine of a pool, by its spec: its memory, and how
// much of it is reserved, in MiB. One with all of it reserved is
// Dedicated, one with less Shared. Started is when its VM's running
// boot started.
type Member struct {
	UID                    string
	MemoryMiB, ReservedMiB int64
	Started                time.Time
}

func (m Member) shared() bool { return m.ReservedMiB < m.MemoryMiB }

// Stage is what a poll does to a pool.
type Stage int

const (
	// Hold changes no balloon.
	Hold Stage = iota
	// Safe inflates balloons, none below its guest's safe floor; Reserve
	// one below it, toward its reservation, where its guest may kill a
	// process of its own, the pool being short.
	Safe
	Reserve
	// Deflate only gives memory back.
	Deflate
)

func (s Stage) String() string {
	switch s {
	case Safe:
		return "safe"
	case Reserve:
		return "reserve"
	case Deflate:
		return "deflate"
	}
	return "hold"
}

// Result is what a poll did to a pool, its sizes in MiB.
type Result struct {
	Stage Stage
	// Balloons are the balloons it set; Allowances the sizes the Shared
	// members' guests are held at; Needing the members that need more.
	Balloons, Allowances map[string]int64
	Needing              []string
	// Sizes are what each member's guest has now, its memory less what its
	// balloon holds, whatever was set.
	Sizes map[string]int64
	// Unread are the members whose balloons could not be read: their
	// Sizes are their whole memory, though their guests may have less.
	Unread []string
	// InUse is what each member's guest uses of its memory, as it last
	// reported it (GuestInUse); a member whose guest reported nothing, or
	// not lately, is not in it.
	InUse map[string]int64
	// Unresponsive are the members whose balloons stopped following what
	// they are asked at this poll (Squeezer.Unresponsive).
	Unresponsive []string
	// Hot says a guest needs more, or a balloon has further to go: polled
	// again soon, for pressure.
	Hot bool
}

// How long a balloon may not follow what it was asked before it is asked
// nothing more: StuckFor, short of it by more than DeadbandMiB and moving
// less than MovedMiB from one poll to the next. Long enough for a guest
// killing a process of its own to give its balloon what it frees.
const (
	StuckFor = time.Minute
	MovedMiB = 16
)

// Squeezer squeezes the pools of a node. It keeps each guest's last count
// of its reclaims, to tell it moving, and how its balloon followed what it
// was asked; it is safe for concurrent use.
type Squeezer struct {
	mu       sync.Mutex
	reclaims map[string]uint64
	balloons map[string]*followed
}

// followed is how a member's balloon followed what it was asked, in the
// boot started at started.
type followed struct {
	started, stuckSince time.Time
	asked, last         int64
	hasAsked            bool
	unresponsive        bool
	// unresponsiveAt is when it was found unresponsive.
	unresponsiveAt time.Time
	// booted says its guest was seen at the size it boots with: past it
	// later, it was grown, and is booting no more.
	booted bool
}

// NewSqueezer returns a squeezer that has seen nothing yet.
func NewSqueezer() *Squeezer {
	return &Squeezer{reclaims: map[string]uint64{}, balloons: map[string]*followed{}}
}

// Unresponsive reports whether the machine's balloon, in its boot running,
// stopped following what it was asked: it is asked nothing more until the
// VM starts again, and counted at what its guest has.
func (q *Squeezer) Unresponsive(uid string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	f := q.balloons[uid]
	return f != nil && f.unresponsive
}

// UnresponsiveSince is when the machine's balloon was found Unresponsive,
// and false while it is not.
func (q *Squeezer) UnresponsiveSince(uid string) (time.Time, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	f := q.balloons[uid]
	if f == nil || !f.unresponsive {
		return time.Time{}, false
	}
	return f.unresponsiveAt, true
}

// Poll reads a pool's members at now and sets their balloons as decide says (docs/decisions/node-agent.md "Memory pools").
func (q *Squeezer) Poll(ctx context.Context, hv driver.Hypervisor, log *slog.Logger, now time.Time, sizeMiB int64, members []Member, stepMiB int64) (Result, error) {
	states := make([]state, 0, len(members))
	var stuck, unread []string
	for _, mb := range members {
		used, err := hv.Usage(ctx, mb.UID)
		if err != nil {
			return Result{}, fmt.Errorf("the memory of machine %s: %w", mb.UID, err)
		}
		m := state{Member: mb, useMiB: int64(used.Memory >> 20), now: now}
		b, err := hv.Balloon(ctx, mb.UID)
		switch {
		case err != nil:
			// A VM whose balloon cannot be read, as one whose VMM serves
			// no API, cannot be squeezed either.
			m.noBalloon = true
			unread = append(unread, mb.UID)
			log.Debug("reading a pool's machine's balloon", "machine_uid", mb.UID, "error", err)
		default:
			m.balloonMiB = min(int64(b.Actual>>20), m.MemoryMiB)
			m.inUseMiB, m.stats = GuestInUse(b, now)
			if m.stats {
				m.availableMiB = int64(b.Available >> 20)
				m.counters = b.HasReclaims
				if b.HasReclaims {
					m.reclaimed = q.moved(mb.UID, b.Reclaims)
				}
			}
			var newly bool
			m.noBalloon, newly = q.follow(mb.UID, mb.Started, m.balloonMiB, now)
			if newly {
				stuck = append(stuck, mb.UID)
			}
			m.booted = q.booted(mb.UID, m.size() <= m.ReservedMiB+BootSlackMiB)
		}
		states = append(states, m)
	}
	r := decide(sizeMiB, states, stepMiB)
	r.Unresponsive, r.Unread = stuck, unread
	r.InUse = map[string]int64{}
	for _, m := range states {
		if m.stats {
			r.InUse[m.UID] = m.inUseMiB
		}
	}
	for _, uid := range slices.Sorted(maps.Keys(r.Balloons)) {
		if err := hv.SetBalloon(ctx, uid, uint64(r.Balloons[uid])<<20); err != nil {
			log.Warn("setting a pool's machine's balloon", "machine_uid", uid, "error", err)
			continue
		}
		q.asked(uid, r.Balloons[uid])
	}
	return r, nil
}

// follow takes what a member's balloon holds now, against what it was
// asked, and reports whether it no longer follows, and whether it stopped
// at this poll.
func (q *Squeezer) follow(uid string, started time.Time, actual int64, now time.Time) (unresponsive, newly bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	f := q.balloons[uid]
	if f == nil || !f.started.Equal(started) {
		f = &followed{started: started, last: actual}
		q.balloons[uid] = f
	}
	if f.unresponsive {
		return true, false
	}
	switch {
	case !f.hasAsked || abs(f.asked-actual) <= DeadbandMiB || abs(actual-f.last) >= MovedMiB:
		f.stuckSince = time.Time{}
	case f.stuckSince.IsZero():
		f.stuckSince = now
	}
	f.last = actual
	if !f.stuckSince.IsZero() && now.Sub(f.stuckSince) >= StuckFor {
		f.unresponsive, f.unresponsiveAt = true, now
		return true, true
	}
	return false, false
}

// booted keeps that a member's guest, followed, is at its boot size now, if
// so, and reports whether it was in this boot.
func (q *Squeezer) booted(uid string, now bool) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	f := q.balloons[uid]
	if f == nil {
		return now
	}
	f.booted = f.booted || now
	return f.booted
}

// asked keeps what a member's balloon was asked.
func (q *Squeezer) asked(uid string, target int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if f := q.balloons[uid]; f != nil {
		f.asked, f.hasAsked = target, true
	}
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// moved reports whether a guest's count of its reclaims has grown since the
// poll before, and keeps it; a count that went back, as after a reboot,
// has not.
func (q *Squeezer) moved(uid string, n uint64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	last, ok := q.reclaims[uid]
	q.reclaims[uid] = n
	return ok && n > last
}

// Forget drops what the squeezer keeps of machines but those in keep.
func (q *Squeezer) Forget(keep map[string]bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for uid := range q.balloons {
		if !keep[uid] {
			delete(q.balloons, uid)
		}
	}
	for uid := range q.reclaims {
		if !keep[uid] {
			delete(q.reclaims, uid)
		}
	}
}

// GuestInUse is what a guest uses of its memory, in MiB, as it last
// reported it to its balloon: its MemTotal, which the balloon lowers, less
// its MemAvailable. A guest that never reported, or not for StaleStats,
// says nothing.
func GuestInUse(b driver.Balloon, now time.Time) (int64, bool) {
	if b.Updated.IsZero() || now.Sub(b.Updated) > StaleStats || b.Available > b.Total {
		return 0, false
	}
	return int64((b.Total - b.Available) >> 20), true
}

// state is a member as a poll reads it, its sizes in MiB.
type state struct {
	Member
	now time.Time
	// useMiB is what its unit holds on the host, memory.current.
	useMiB int64
	// balloonMiB is what its balloon holds now; noBalloon says it cannot
	// be read or set.
	balloonMiB int64
	noBalloon  bool
	// stats says its guest reported its memory lately: inUseMiB is its
	// MemTotal less its MemAvailable, availableMiB its MemAvailable;
	// counters says it reports its reclaims, reclaimed that they moved
	// since the poll before.
	stats                  bool
	inUseMiB, availableMiB int64
	counters, reclaimed    bool
	// booted says it was seen at its boot size since its VM started.
	booted bool
}

// size is what the guest has of its memory: what its balloon leaves it.
func (m state) size() int64 { return m.MemoryMiB - m.balloonMiB }

// booting reports a Shared guest still inflating the balloon it boots
// with, which leaves it its reservation: its balloon is left to it, its
// container's process not started yet (docs/decisions/guest-agent.md);
// one seen at its boot size since was grown, and has booted.
func (m state) booting() bool {
	return m.shared() && !m.noBalloon && !m.booted && m.now.Sub(m.Started) < BootWindow && m.size() > m.ReservedMiB+BootSlackMiB
}

// margin is what a guest is left past what it uses, and has available
// before it needs more.
func margin(miB int64) int64 { return max(MarginMiB, miB*MarginPercent/100) }

// needs reports whether a Shared guest needs more memory: it has little
// available, or it reclaimed some since the poll before; one that reports
// neither, its unit holding nearly all its size.
func (m state) needs() bool {
	full := m.useMiB*100 >= m.size()*FullPercent
	switch {
	case m.noBalloon || m.booting():
		// Nothing can be done for it, or not yet.
		return false
	case !m.stats:
		return full
	case m.availableMiB < margin(m.size()) || m.reclaimed:
		return true
	}
	return !m.counters && full
}

// floor is the least a Shared guest is held at while its pool is not
// short: what it uses and a margin, or its reservation if more; for one that
// reports nothing, or whose balloon cannot be set, its size now.
func (m state) floor() int64 {
	if m.booting() {
		return m.ReservedMiB
	}
	if !m.stats || m.noBalloon {
		return m.size()
	}
	return min(max(m.inUseMiB+margin(m.MemoryMiB), m.ReservedMiB), m.MemoryMiB)
}

// allowances are the sizes a pool's Shared guests are held at (docs/decisions/node-agent.md "Memory pools").
func allowances(sizeMiB int64, members []state) map[string]int64 {
	budget := sizeMiB
	var shared []state
	for _, m := range members {
		if m.shared() {
			shared = append(shared, m)
		} else {
			budget -= m.MemoryMiB
		}
	}
	out := map[string]int64{}
	floors := make([]int64, len(shared))
	var sum int64
	needy := false
	for i, m := range shared {
		floors[i] = m.floor()
		sum += floors[i]
		needy = needy || m.needs()
	}
	if sum > budget {
		above := make([]int64, len(shared))
		for i, m := range shared {
			if !m.noBalloon {
				above[i] = floors[i] - m.ReservedMiB
			}
		}
		for i, m := range shared {
			out[m.UID] = max(floors[i]-portion(sum-budget, above, i, true), m.ReservedMiB)
		}
		return out
	}
	vals := slices.Clone(floors)
	spare := budget - sum
	// kept is what a Shared guest keeps of its size when nobody needs more.
	kept := make([]int64, len(shared))
	for i, m := range shared {
		kept[i] = min(max(m.size(), floors[i]), m.MemoryMiB)
		if m.booting() {
			kept[i] = floors[i]
		}
	}
	memory := make([]int64, len(shared))
	for i, m := range shared {
		memory[i] = m.MemoryMiB
	}
	if needy {
		// Those in need grow first; the others give up only what they take.
		needs := slices.Clone(floors)
		for i, m := range shared {
			if m.needs() {
				needs[i] = m.MemoryMiB
			}
		}
		spare = handOut(vals, spare, needs)
		spare = handOut(vals, spare, kept)
	} else {
		// Nobody needs more: nobody gives any back, if all fit.
		var keep int64
		for _, k := range kept {
			keep += k
		}
		if keep <= budget {
			vals, spare = kept, budget-keep
		}
	}
	handOut(vals, spare, memory)
	for i, m := range shared {
		out[m.UID] = vals[i]
	}
	return out
}

// handOut raises vals toward caps by spare, by how far each is below its
// cap, and returns what is left of spare.
func handOut(vals []int64, spare int64, caps []int64) int64 {
	room := make([]int64, len(vals))
	for i := range vals {
		room[i] = max(caps[i]-vals[i], 0)
	}
	given := int64(0)
	for i := range vals {
		more := min(portion(spare, room, i, false), room[i])
		vals[i] += more
		given += more
	}
	return spare - given
}

// portion is weights[i]'s part of total, rounded down, or up if up.
func portion(total int64, weights []int64, i int, up bool) int64 {
	var sum int64
	for _, w := range weights {
		sum += w
	}
	if sum == 0 {
		return 0
	}
	if up {
		return (total*weights[i] + sum - 1) / sum
	}
	return total * weights[i] / sum
}

// decide says which balloons to set in a pool of sizeMiB, and how hot it is (docs/decisions/node-agent.md "Memory pools").
func decide(sizeMiB int64, members []state, stepMiB int64) Result {
	r := Result{Balloons: map[string]int64{}, Allowances: allowances(sizeMiB, members), Sizes: map[string]int64{}}
	room := sizeMiB
	type grower struct {
		i    int
		want int64
		// first: a Dedicated member, then one in need.
		first, need bool
	}
	var growers []grower
	for i, m := range members {
		room -= m.size()
		r.Sizes[m.UID] = m.size()
		switch {
		case !m.shared():
			// Given back whole, but only into room freed, as any.
			if m.balloonMiB > 0 && !m.noBalloon {
				r.Hot = true
				growers = append(growers, grower{i: i, want: m.balloonMiB, first: true})
			}
		default:
			if m.needs() {
				r.Needing = append(r.Needing, m.UID)
			}
			// Soon done, and then given its room.
			r.Hot = r.Hot || m.booting()
		}
	}
	r.Hot = r.Hot || len(r.Needing) > 0
	for i, m := range members {
		a, ok := r.Allowances[m.UID]
		if !ok || m.noBalloon || m.booting() {
			continue
		}
		switch d := m.size() - a; {
		case d > DeadbandMiB:
			r.Hot = true
			r.Balloons[m.UID] = m.balloonMiB + min(d, stepMiB)
			st := Safe
			if a < m.floor() {
				st = Reserve
			}
			r.Stage = max(r.Stage, st)
		case -d > DeadbandMiB:
			r.Hot = true
			growers = append(growers, grower{i: i, want: -d, need: m.needs()})
		}
	}
	slices.SortStableFunc(growers, func(x, y grower) int {
		switch {
		case x.first != y.first:
			if x.first {
				return -1
			}
			return 1
		case x.need != y.need:
			if x.need {
				return -1
			}
			return 1
		}
		return 0
	})
	for _, g := range growers {
		if room <= 0 {
			break
		}
		m := members[g.i]
		more := min(g.want, room)
		room -= more
		r.Balloons[m.UID] = m.balloonMiB - more
		if r.Stage == Hold {
			r.Stage = Deflate
		}
	}
	return r
}

// Under pressure, once a guest needs more or a balloon has further to go,
// or the host signals that a pool's VM started or its slice reached its
// limit, a pool is polled every HotPoll with PressureStepMiB; never more
// often than MinGap.
const (
	HotPoll = time.Second
	MinGap  = 200 * time.Millisecond
)

// Run calls poll at once, then every period, at once when events signals,
// and every HotPoll while poll says the pools are under pressure, until
// ctx ends. poll is told whether it polls for pressure: woken by events,
// or hot at the poll before.
func Run(ctx context.Context, clock clockwork.Clock, events <-chan struct{}, period time.Duration, poll func(ctx context.Context, pressure bool) (hot bool)) {
	timer := clock.NewTimer(0)
	defer timer.Stop()
	hot := false
	for {
		woken := false
		select {
		case <-ctx.Done():
			return
		case <-timer.Chan():
		case <-events:
			woken = true
		}
		hot = poll(ctx, hot || woken)
		next := period
		if hot {
			next = HotPoll
		}
		timer.Reset(next)
		select {
		case <-ctx.Done():
			return
		case <-clock.After(MinGap):
		}
	}
}
