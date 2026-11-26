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
	pool := driver.MemoryPool{Name: "acme", SizeMiB: 5120}
	members := []squeeze.Member{
		{UID: "a", MemoryMiB: 4096, GuaranteedMiB: 1024},
		{UID: "c", MemoryMiB: 4096, GuaranteedMiB: 1024},
		{UID: "d", MemoryMiB: 2048, GuaranteedMiB: 2048},
	}
	q := squeeze.NewSqueezer()
	log := slog.New(slog.DiscardHandler)
	r, err := q.Poll(t.Context(), h, log, now, pool, members, squeeze.StepMiB)
	if err != nil {
		t.Fatal(err)
	}
	if r.Stage != squeeze.Hold || len(h.set) > 0 || len(r.Needing) > 0 {
		t.Errorf("first poll: %+v, set %v; want nothing done", r, h.set)
	}
	b := h.balloons["a"]
	b.Reclaims = 15
	h.balloons["a"] = b
	if r, err = q.Poll(t.Context(), h, log, now, pool, members, squeeze.StepMiB); err != nil {
		t.Fatal(err)
	}
	if want := map[string]uint64{"c": 2560 * mib}; r.Stage != squeeze.Safe || !maps.Equal(h.set, want) || !slices.Equal(r.Needing, []string{"a"}) {
		t.Errorf("A reclaiming: %+v, set %v; want safe, %v, A needing", r, h.set, want)
	}

	clear(h.set)
	delete(h.used, "c")
	if _, err := q.Poll(t.Context(), h, log, now, pool, members, squeeze.StepMiB); err == nil || len(h.set) > 0 {
		t.Errorf("with c's memory unknown: %v, set %v; want an error and nothing set", err, h.set)
	}
}
