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
	// be inflating the balloon it boots with: past hangar-init's wait for it
	// (boot.BalloonTimeout). BootSlackMiB is how far above its guarantee its
	// size may be once inflated (boot.BalloonSlackMiB).
	BootWindow   = 30 * time.Second
	BootSlackMiB = 64
	// StaleStats is how old a guest's report of its memory may be before
	// it says nothing: Cloud Hypervisor asks for a new one each time it is
	// asked for one, a poll apart.
	StaleStats = 30 * time.Second
)

// Member is a running machine of a pool, by its spec: its memory, and the
// least of it it is guaranteed, in MiB. One guaranteed all of it is
// Dedicated, one guaranteed less Shared. Started is when its VM's running
// boot started.
type Member struct {
	UID                      string
	MemoryMiB, GuaranteedMiB int64
	Started                  time.Time
}

func (m Member) shared() bool { return m.GuaranteedMiB < m.MemoryMiB }

// Stage is what a poll does to a pool.
type Stage int

const (
	// Hold changes no balloon.
	Hold Stage = iota
	// Safe inflates balloons, none below its guest's safe floor; Guarantee
	// one below it, toward its guarantee, where its guest may kill a
	// process of its own, the pool being short.
	Safe
	Guarantee
	// Deflate only gives memory back.
	Deflate
)

func (s Stage) String() string {
	switch s {
	case Safe:
		return "safe"
	case Guarantee:
		return "guarantee"
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
	// Hot says a guest needs more, or a balloon has further to go: polled
	// again soon, for pressure.
	Hot bool
}

// Squeezer squeezes the pools of a node. It keeps each guest's last count
// of its reclaims, to tell it moving; it is safe for concurrent use.
type Squeezer struct {
	mu       sync.Mutex
	reclaims map[string]uint64
}

// NewSqueezer returns a squeezer that has seen no reclaims yet.
func NewSqueezer() *Squeezer { return &Squeezer{reclaims: map[string]uint64{}} }

// Poll reads the members of a pool from the host at now, and sets their
// balloons as decide says, one growing by at most stepMiB. A member whose
// memory on the host cannot be read leaves what the pool holds unknown,
// and it does nothing. What it fails to set is logged.
func (q *Squeezer) Poll(ctx context.Context, hv driver.Hypervisor, log *slog.Logger, now time.Time, pool driver.MemoryPool, members []Member, stepMiB int64) (Result, error) {
	states := make([]state, 0, len(members))
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
			log.Debug("reading a pool's machine's balloon", "pool", pool.Name, "machine_uid", mb.UID, "error", err)
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
		}
		states = append(states, m)
	}
	r := decide(pool.SizeMiB, states, stepMiB)
	for _, uid := range slices.Sorted(maps.Keys(r.Balloons)) {
		if err := hv.SetBalloon(ctx, uid, uint64(r.Balloons[uid])<<20); err != nil {
			log.Warn("setting a pool's machine's balloon", "pool", pool.Name, "machine_uid", uid, "error", err)
		}
	}
	return r, nil
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
}

// size is what the guest has of its memory: what its balloon leaves it.
func (m state) size() int64 { return m.MemoryMiB - m.balloonMiB }

// booting reports a Shared guest still inflating the balloon it boots
// with, which leaves it its guarantee: its balloon is left to it, its
// container's process not started yet (docs/decisions/guest-agent.md).
func (m state) booting() bool {
	return m.shared() && !m.noBalloon && m.now.Sub(m.Started) < BootWindow && m.size() > m.GuaranteedMiB+BootSlackMiB
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
// short: what it uses and a margin, or its guarantee if more; for one that
// reports nothing, or whose balloon cannot be set, its size now.
func (m state) floor() int64 {
	if m.booting() {
		return m.GuaranteedMiB
	}
	if !m.stats || m.noBalloon {
		return m.size()
	}
	return min(max(m.inUseMiB+margin(m.MemoryMiB), m.GuaranteedMiB), m.MemoryMiB)
}

// allowances are the sizes the Shared members' guests are held at in a pool
// of sizeMiB. They share what the Dedicated members' whole memory leaves of
// it, used or not, so a Dedicated machine is never short: each holds its
// floor, and the rest goes to those that need more, by how far each could
// grow, the others held at their floors; with none in need, each keeps its
// size and what is left is handed out by how far each could grow. Short,
// what the floors take past the budget is taken from them by how far each
// is above its guarantee, never below it.
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
				above[i] = floors[i] - m.GuaranteedMiB
			}
		}
		for i, m := range shared {
			out[m.UID] = max(floors[i]-portion(sum-budget, above, i, true), m.GuaranteedMiB)
		}
		return out
	}
	base := floors
	if !needy {
		// Nobody needs more: nobody gives any back.
		base = make([]int64, len(shared))
		sum = 0
		for i, m := range shared {
			base[i] = min(max(m.size(), floors[i]), m.MemoryMiB)
			if m.booting() {
				base[i] = floors[i]
			}
			sum += base[i]
		}
		if sum > budget {
			base, sum = floors, 0
			for _, f := range floors {
				sum += f
			}
		}
	}
	grow := make([]int64, len(shared))
	for i, m := range shared {
		if !needy || m.needs() {
			grow[i] = m.MemoryMiB - base[i]
		}
	}
	for i, m := range shared {
		out[m.UID] = min(base[i]+portion(budget-sum, grow, i, false), m.MemoryMiB)
	}
	return out
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

// decide says what to do to a pool of sizeMiB whose running machines are
// members: each Shared guest is brought toward its allowance, a balloon
// growing by at most stepMiB, and shrinking only into what the balloons
// already leave free of the pool, those in need first, so that what the
// Shared guests have and the Dedicated machines' memory together never
// pass the pool's size. A Dedicated machine's balloon is given back whole.
func decide(sizeMiB int64, members []state, stepMiB int64) Result {
	r := Result{Balloons: map[string]int64{}, Allowances: allowances(sizeMiB, members)}
	room := sizeMiB
	for _, m := range members {
		switch {
		case !m.shared():
			room -= m.MemoryMiB
			if m.balloonMiB > 0 && !m.noBalloon {
				r.Balloons[m.UID] = 0
				r.Stage = Deflate
			}
		default:
			room -= m.size()
			if m.needs() {
				r.Needing = append(r.Needing, m.UID)
			}
			// Soon done, and then given its room.
			r.Hot = r.Hot || m.booting()
		}
	}
	r.Hot = r.Hot || len(r.Needing) > 0
	type grower struct {
		i    int
		want int64
	}
	var growers []grower
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
				st = Guarantee
			}
			if r.Stage == Deflate || st > r.Stage {
				r.Stage = st
			}
		case -d > DeadbandMiB:
			r.Hot = true
			growers = append(growers, grower{i, -d})
		}
	}
	// Those in need first, then the others, each into what is left.
	slices.SortStableFunc(growers, func(x, y grower) int {
		nx, ny := members[x.i].needs(), members[y.i].needs()
		switch {
		case nx == ny:
			return 0
		case nx:
			return -1
		}
		return 1
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
