package squeeze_test

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/squeeze"
)

// host is a hypervisor whose VMs hold what used says, their balloons and
// guests' reports what balloons says; it records the balloons set.
type host struct {
	driver.Hypervisor
	used     map[string]uint64
	balloons map[string]driver.Balloon
	set      map[string]uint64
}

func (h *host) Usage(_ context.Context, uid string) (driver.VMUsage, error) {
	u, ok := h.used[uid]
	if !ok {
		return driver.VMUsage{}, errors.New("no running VM")
	}
	return driver.VMUsage{Memory: u}, nil
}

func (h *host) Balloon(_ context.Context, uid string) (driver.Balloon, error) {
	b, ok := h.balloons[uid]
	if !ok {
		return driver.Balloon{}, errors.New("no API")
	}
	return b, nil
}

func (h *host) SetBalloon(_ context.Context, uid string, size uint64) error {
	h.set[uid] = size
	return nil
}

// TestAPollTellsAGuestReclaiming: a guest whose reclaims move between two
// polls needs more, and is given what another Shared guest holds past its
// floor, that one's balloon growing; the first poll, with nothing to
// compare, changes nothing. A member whose memory cannot be read leaves
// the pool as it is.
func TestAPollTellsAGuestReclaiming(t *testing.T) {
	t.Parallel()
	now := time.Now()
	const mib = 1 << 20
	h := &host{
		used: map[string]uint64{"a": 1100 * mib, "c": 2000 * mib, "d": 500 * mib},
		balloons: map[string]driver.Balloon{
			"a": {Actual: 3072 * mib, Total: 1024 * mib, Available: 700 * mib, Updated: now, Reclaims: 10, HasReclaims: true},
			"c": {Actual: 2048 * mib, Total: 2048 * mib, Available: 1900 * mib, Updated: now, Reclaims: 5, HasReclaims: true},
			"d": {},
		},
		set: map[string]uint64{},
	}
	members := []squeeze.Member{
		{UID: "a", MemoryMiB: 4096, GuaranteedMiB: 1024},
		{UID: "c", MemoryMiB: 4096, GuaranteedMiB: 1024},
		{UID: "d", MemoryMiB: 2048, GuaranteedMiB: 2048},
	}
	q := squeeze.NewSqueezer()
	log := slog.New(slog.DiscardHandler)
	r, err := q.Poll(t.Context(), h, log, now, 5120, members, squeeze.StepMiB)
	if err != nil {
		t.Fatal(err)
	}
	if r.Stage != squeeze.Hold || len(h.set) > 0 || len(r.Needing) > 0 {
		t.Errorf("first poll: %+v, set %v; want nothing done", r, h.set)
	}
	b := h.balloons["a"]
	b.Reclaims = 15
	h.balloons["a"] = b
	if r, err = q.Poll(t.Context(), h, log, now, 5120, members, squeeze.StepMiB); err != nil {
		t.Fatal(err)
	}
	if want := map[string]uint64{"c": 2560 * mib}; r.Stage != squeeze.Safe || !maps.Equal(h.set, want) || !slices.Equal(r.Needing, []string{"a"}) {
		t.Errorf("A reclaiming: %+v, set %v; want safe, %v, A needing", r, h.set, want)
	}

	clear(h.set)
	delete(h.used, "c")
	if _, err := q.Poll(t.Context(), h, log, now, 5120, members, squeeze.StepMiB); err == nil || len(h.set) > 0 {
		t.Errorf("with c's memory unknown: %v, set %v; want an error and nothing set", err, h.set)
	}
}

// TestABalloonThatDoesNotFollowIsAskedNoMore: a balloon that does not move
// toward what it is asked for StuckFor is said unresponsive once, then
// asked nothing more and the pool not kept hot for it; its VM started
// again, it is asked again.
func TestABalloonThatDoesNotFollowIsAskedNoMore(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	start := time.Unix(1_000_000_000, 0)
	// Its balloons never move from what balloons says.
	h := &host{
		used:     map[string]uint64{"a": 4000 * mib, "d": 500 * mib},
		balloons: map[string]driver.Balloon{"a": {Total: 4096 * mib, Available: 3800 * mib}, "d": {}},
		set:      map[string]uint64{},
	}
	members := []squeeze.Member{
		{UID: "a", MemoryMiB: 4096, GuaranteedMiB: 1024, Started: start},
		{UID: "d", MemoryMiB: 2048, GuaranteedMiB: 2048, Started: start},
	}
	q := squeeze.NewSqueezer()
	log := slog.New(slog.DiscardHandler)
	poll := func(now time.Time) squeeze.Result {
		t.Helper()
		b := h.balloons["a"]
		b.Updated = now
		h.balloons["a"] = b
		clear(h.set)
		r, err := q.Poll(t.Context(), h, log, now, 4096, members, squeeze.StepMiB)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	var said []string
	for i := range 10 {
		r := poll(start.Add(time.Minute + time.Duration(i)*10*time.Second))
		said = append(said, r.Unresponsive...)
		if i >= 7 && (r.Hot || len(h.set) > 0) {
			t.Errorf("poll %d, once given up: %+v, set %v; want nothing asked, not hot", i, r, h.set)
		}
		if i < 7 && h.set["a"] == 0 {
			t.Errorf("poll %d: a not asked, set %v", i, h.set)
		}
	}
	if !slices.Equal(said, []string{"a"}) || !q.Unresponsive("a") {
		t.Errorf("said %v, unresponsive %t; want a once", said, q.Unresponsive("a"))
	}
	members[0].Started = start.Add(time.Hour)
	poll(start.Add(time.Hour + time.Minute))
	if q.Unresponsive("a") || h.set["a"] == 0 {
		t.Errorf("started again: unresponsive %t, set %v; want asked again", q.Unresponsive("a"), h.set)
	}
}

// TestAGuestGrownAfterItBootedIsSqueezed: a Shared guest seen at its boot
// size, its guarantee, has booted: grown past it within squeeze.BootWindow
// of its start, it is squeezed as any, not left to a balloon it no longer
// inflates.
func TestAGuestGrownAfterItBootedIsSqueezed(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	now := time.Unix(1_000_000_000, 0)
	h := &host{
		used:     map[string]uint64{"a": 1000 * mib, "c": 1000 * mib},
		balloons: map[string]driver.Balloon{},
		set:      map[string]uint64{},
	}
	at := func(uid string, sizeMiB, inUseMiB uint64) {
		h.balloons[uid] = driver.Balloon{Actual: (4096 - sizeMiB) * mib, Total: sizeMiB * mib, Available: (sizeMiB - inUseMiB) * mib, Updated: now}
	}
	members := []squeeze.Member{
		{UID: "a", MemoryMiB: 4096, GuaranteedMiB: 1024, Started: now},
		{UID: "c", MemoryMiB: 4096, GuaranteedMiB: 1024, Started: now.Add(-time.Hour)},
	}
	q := squeeze.NewSqueezer()
	log := slog.New(slog.DiscardHandler)
	poll := func() {
		t.Helper()
		clear(h.set)
		if _, err := q.Poll(t.Context(), h, log, now, 4096, members, squeeze.StepMiB); err != nil {
			t.Fatal(err)
		}
	}
	// Booting, A is left to its balloon.
	at("a", 3072, 100)
	at("c", 1024, 1000)
	poll()
	if _, ok := h.set["a"]; ok {
		t.Errorf("booting, a was set %v", h.set)
	}
	// At its boot size, then grown, then C in need: A gives it room.
	at("a", 1024, 100)
	poll()
	now = now.Add(time.Second)
	at("a", 3072, 100)
	poll()
	if got := h.set["a"]; got != (1024+squeeze.StepMiB)*mib {
		t.Errorf("grown after it booted, a was set %d MiB, want %d", got/mib, 1024+squeeze.StepMiB)
	}
}
