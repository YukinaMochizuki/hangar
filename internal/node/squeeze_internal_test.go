package node

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	dto "github.com/prometheus/client_model/go"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/squeeze"
)

const mib = 1 << 20

// poolVM is a VM of poolHost: as it was launched, its balloon, what its
// guest uses, whether its balloon takes what it is asked, or cannot be
// read, and when it was last killed whole.
type poolVM struct {
	launched           driver.PoolVM
	inSlice            bool
	balloonMiB, useMiB int64
	stuck, unreadable  bool
	lastOOMKill        time.Time
}

// poolHost is a hypervisor running vms, started an hour before its
// clock's now; it keeps the limits its pools' slices were last given, of
// memory and of CPUs.
type poolHost struct {
	driver.Hypervisor
	clock  clockwork.Clock
	vms    map[string]*poolVM
	limits map[string]int64
	cpus   map[string]int64
	read   map[string]bool
}

func (h *poolHost) PoolVMs(context.Context) ([]driver.PoolVM, error) {
	var out []driver.PoolVM
	for _, v := range h.vms {
		if v.inSlice {
			out = append(out, v.launched)
		}
	}
	return out, nil
}

func (h *poolHost) VMs(context.Context) ([]driver.VM, error) {
	var out []driver.VM
	for uid, v := range h.vms {
		out = append(out, driver.VM{MachineUID: uid, State: driver.VMRunning, Started: v.launched.Started, LastOOMKill: v.lastOOMKill})
	}
	return out, nil
}

func (h *poolHost) Usage(_ context.Context, uid string) (driver.VMUsage, error) {
	v, ok := h.vms[uid]
	if !ok {
		return driver.VMUsage{}, errors.New("no VM")
	}
	return driver.VMUsage{Memory: uint64(min(v.useMiB+50, v.launched.MemoryMiB-v.balloonMiB)) * mib}, nil
}

func (h *poolHost) Balloon(_ context.Context, uid string) (driver.Balloon, error) {
	v, ok := h.vms[uid]
	if !ok || v.unreadable {
		return driver.Balloon{}, errors.New("no VM")
	}
	total := v.launched.MemoryMiB - v.balloonMiB
	return driver.Balloon{
		Actual: uint64(v.balloonMiB) * mib, Total: uint64(total) * mib, Available: uint64(max(total-v.useMiB, 0)) * mib,
		Updated: h.clock.Now(),
	}, nil
}

func (h *poolHost) SetBalloon(_ context.Context, uid string, size uint64) error {
	if v := h.vms[uid]; !v.stuck {
		v.balloonMiB = int64(size / mib)
	}
	return nil
}

func (h *poolHost) SetMemoryPool(_ context.Context, name string, guestMiB int64) error {
	h.limits[name] = guestMiB
	return nil
}

func (h *poolHost) MemoryEvents() <-chan struct{} { return nil }

func (h *poolHost) SetCPUPools(_ context.Context, want map[string]int64, read map[string]bool) error {
	h.cpus, h.read = maps.Clone(want), maps.Clone(read)
	return nil
}

// newPoolAgent is an agent on a host running, in pool acme of 4096 with
// reservations of 1024, a and s, each of 4096 at 2048, the balloon the
// rest: a using all but 148 of it, in need, s 100.
func newPoolAgent(t *testing.T) (*Agent, *poolHost) {
	t.Helper()
	clock := clockwork.NewFakeClockAt(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC))
	h := &poolHost{clock: clock, vms: map[string]*poolVM{}, limits: map[string]int64{}}
	for uid, use := range map[string]int64{"a": 1900, "s": 100} {
		h.vms[uid] = &poolVM{
			launched: driver.PoolVM{
				MachineUID: uid, Pool: driver.MemoryPool{Name: "acme", SizeMiB: 4096}, MemoryMiB: 4096, ReservedMiB: 1024,
				Started: clock.Now().Add(-time.Hour),
			},
			inSlice: true, balloonMiB: 2048, useMiB: use,
		}
	}
	a := &Agent{
		cfg: Config{
			Name: "n1", Clock: clock, Log: slog.New(slog.DiscardHandler),
			Drivers: &Drivers{Hypervisor: h},
		},
		memoryShares: map[string]memoryShare{}, squeezer: squeeze.NewSqueezer(), metrics: newAgentMetrics(),
		poolReads: &poolReads{pools: map[string]poolRead{}},
	}
	return a, h
}

// share is a machine's spec's share of its memory: of memoryMiB, in pool
// (of sizeMiB) if named, reserved reservedMiB.
func share(uid, pool string, sizeMiB, memoryMiB, reservedMiB int64) memoryShare {
	s := memoryShare{key: controller.Key{Project: "p", Name: uid}, memoryMiB: memoryMiB, reservedMiB: reservedMiB}
	if pool != "" {
		s.pool = &driver.MemoryPool{Name: pool, SizeMiB: sizeMiB}
	}
	return s
}

func balloons(h *poolHost) map[string]int64 {
	out := map[string]int64{}
	for uid, v := range h.vms {
		out[uid] = v.balloonMiB
	}
	return out
}

// TestAMemberCountsAsLaunchedUntilItsUnitIsGone: a VM in a pool's slice is
// held at what it was launched with, and A in need is not given what it
// has, whatever its spec now says of it: stopping it, not read since the
// agent started again, or its memory shrunk, which waits for its restart.
func TestAMemberCountsAsLaunchedUntilItsUnitIsGone(t *testing.T) {
	t.Parallel()
	for name, shares := range map[string]map[string]memoryShare{
		"stopping":            {"a": share("a", "acme", 4096, 4096, 1024)},
		"agent started again": {},
		"half reconciled":     {"a": share("a", "acme", 4096, 4096, 1024)},
		"memory shrunk":       {"a": share("a", "acme", 4096, 4096, 1024), "s": share("s", "acme", 4096, 1024, 1024)},
	} {
		a, h := newPoolAgent(t)
		a.memoryShares = shares
		if !a.squeeze(t.Context(), false) {
			t.Errorf("%s: not hot", name)
		}
		// S to its floor, its reservation, a step; A grows into nothing yet.
		if got, want := balloons(h), map[string]int64{"a": 2048, "s": 2048 + squeeze.StepMiB}; !maps.Equal(got, want) || h.limits["acme"] != 4096 {
			t.Errorf("%s: balloons %v, limit %d; want %v, 4096", name, got, h.limits["acme"], want)
		}
	}
}

// TestAMemberThatLeftItsPoolIsGivenBackItsMemory: a member whose spec now
// names no pool is held at its whole memory, which its pool's size gains,
// and its balloon given back first; with none of its slice's VMs of the
// pool any more, the slice holds them to no limit. A running VM in no
// pool's slice is given back its balloon whole.
func TestAMemberThatLeftItsPoolIsGivenBackItsMemory(t *testing.T) {
	t.Parallel()
	a, h := newPoolAgent(t)
	a.memoryShares = map[string]memoryShare{"a": share("a", "acme", 4096, 4096, 1024), "s": share("s", "", 0, 4096, 4096)}
	a.squeeze(t.Context(), false)
	if got, want := balloons(h), map[string]int64{"a": 0, "s": 0}; !maps.Equal(got, want) || h.limits["acme"] != 8192 {
		t.Errorf("s left: balloons %v, limit %d; want %v, 8192", got, h.limits["acme"], want)
	}

	a, h = newPoolAgent(t)
	a.memoryShares = map[string]memoryShare{"a": share("a", "", 0, 4096, 4096), "s": share("s", "", 0, 4096, 4096)}
	a.squeeze(t.Context(), false)
	if limit, ok := h.limits["acme"]; !ok || limit > 0 {
		t.Errorf("both left: limit %d (%t), want lifted", limit, ok)
	}

	a, h = newPoolAgent(t)
	h.vms["s"].inSlice = false
	vms, _ := h.VMs(t.Context())
	a.deflateLeft(t.Context(), vms)
	if got, want := balloons(h), map[string]int64{"a": 2048, "s": 0}; !maps.Equal(got, want) {
		t.Errorf("s in no slice: balloons %v, want %v", got, want)
	}
}

// TestAPoolsLimitComesDownOnlyOnceItsGuestsFit: a pool's size lowered, its
// slice's limit stays at what its guests have until their balloons bring
// them under it; and a pool is never smaller than what its members are
// reserved together.
func TestAPoolsLimitComesDownOnlyOnceItsGuestsFit(t *testing.T) {
	t.Parallel()
	a, h := newPoolAgent(t)
	a.memoryShares = map[string]memoryShare{"a": share("a", "acme", 1024, 4096, 1024), "s": share("s", "acme", 1024, 4096, 1024)}
	plans := planPools(must(h.PoolVMs(t.Context())), must(h.VMs(t.Context())), a.memoryShares)
	if got := plans["acme"].sizeMiB; got != 2048 {
		t.Errorf("a pool of 1024 reserving 2048 is of %d, want 2048", got)
	}
	a.squeeze(t.Context(), false)
	if h.limits["acme"] != 4096 {
		t.Errorf("first poll: limit %d, want 4096, what they have", h.limits["acme"])
	}
	for range 4 {
		a.squeeze(t.Context(), false)
	}
	if got, want := balloons(h), map[string]int64{"a": 3072, "s": 3072}; !maps.Equal(got, want) || h.limits["acme"] != 2048 {
		t.Errorf("once followed: balloons %v, limit %d; want %v, 2048", got, h.limits["acme"], want)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestABalloonThatDoesNotFollowIsReported: a member whose balloon does not
// follow for squeeze.StuckFor is counted, logged, and its machine requeued,
// once; its machine then says so, and is held at what it has.
func TestABalloonThatDoesNotFollowIsReported(t *testing.T) {
	t.Parallel()
	a, h := newPoolAgent(t)
	clock := a.cfg.Clock.(*clockwork.FakeClock)
	h.vms["s"].stuck = true
	a.memoryShares = map[string]memoryShare{"a": share("a", "acme", 2048, 4096, 1024), "s": share("s", "acme", 2048, 4096, 1024)}
	var requeued []controller.Key
	a.requeue = func(keys ...controller.Key) { requeued = append(requeued, keys...) }
	for range 10 {
		a.squeeze(t.Context(), false)
		clock.Advance(10 * time.Second)
	}
	var counted dto.Metric
	if err := a.metrics.balloonUnresponsive.Write(&counted); err != nil {
		t.Fatal(err)
	}
	if n := counted.GetCounter().GetValue(); n != 1 || len(requeued) != 1 || requeued[0].Name != "s" {
		t.Errorf("counted %v, requeued %v; want s once", n, requeued)
	}
	conds := a.memoryConditions("s", nil)
	if len(conds) != 1 || conds[0].GetType() != computeapi.MachineBalloonResponsive || conds[0].GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || conds[0].GetReason() != computeapi.ReasonBalloonUnresponsive {
		t.Errorf("s's conditions %v, want its balloon unresponsive", conds)
	}
	if len(a.memoryConditions("a", nil)) != 0 {
		t.Errorf("a's conditions %v, want none", a.memoryConditions("a", nil))
	}
	// A at its reservation, S at all it has.
	if got := balloons(h); got["a"] != 3072 || h.limits["acme"] != 1024+2048 {
		t.Errorf("balloons %v, limit %d; want a at 3072, limit 3072", got, h.limits["acme"])
	}
}

// TestAnOOMKillIsSaid: a machine whose VM the host's OOM killer killed says
// when, whether its VM runs again or not.
func TestAnOOMKillIsSaid(t *testing.T) {
	t.Parallel()
	a, _ := newPoolAgent(t)
	at := time.Date(2026, 11, 9, 11, 0, 0, 0, time.UTC)
	for _, vm := range []*driver.VM{{State: driver.VMRunning, LastOOMKill: at}, {State: driver.VMExited, OOMKilled: true, LastOOMKill: at}} {
		conds := a.memoryConditions("a", vm)
		if len(conds) != 1 || conds[0].GetType() != computeapi.MachineOOMKilled || conds[0].GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE || conds[0].GetReason() != computeapi.ReasonOutOfMemory {
			t.Errorf("%v: conditions %v, want OOMKilled", vm.State, conds)
		}
	}
	if conds := a.memoryConditions("a", &driver.VM{State: driver.VMRunning}); len(conds) != 0 {
		t.Errorf("never killed: %v", conds)
	}
}
