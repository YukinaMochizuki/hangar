package node

import (
	"context"
	"errors"
	"slices"
	"time"

	"connectrpc.com/connect"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// poolPoint is how many of a CPU pool's limit's periods passed in a
// minute, and in how many of them the limit held its VMs back.
type poolPoint struct {
	at                 time.Time
	periods, throttled uint64
}

// poolUsage is what the node keeps of one CPU pool's slice's cpu.stat
// (docs/decisions/machine-usage.md, docs/decisions/cpu-pool.md).
type poolUsage struct {
	// at, periods and throttled are when the counters were last read and
	// what they said, for the next sample's deltas.
	at                 time.Time
	periods, throttled uint64
	read               bool
	// minute is the minute being gathered, and its deltas; measured says
	// a delta was taken in it.
	minute         time.Time
	dPeriods, dThr uint64
	measured       bool
	// points are the minutes gathered, oldest first.
	points []poolPoint
}

// record takes the pool's counters read at. A sample's deltas count only
// against one read at most maxSpan before, and only while neither counter
// went back, as when the slice was made again; so a gap or a reset leaves
// minutes with no point, as a machine's usage does. A sample in a later
// minute than the one gathered ends that one.
func (p *poolUsage) record(at time.Time, st driver.CPUPoolStat, maxSpan time.Duration) {
	minute := at.Truncate(time.Minute)
	if !p.minute.IsZero() && minute.After(p.minute) {
		p.endMinute()
	}
	if p.minute.IsZero() || minute.After(p.minute) {
		p.minute = minute
	}
	if p.read && st.Periods >= p.periods && st.Throttled >= p.throttled && at.After(p.at) && at.Sub(p.at) <= maxSpan {
		p.dPeriods += st.Periods - p.periods
		p.dThr += st.Throttled - p.throttled
		p.measured = true
	}
	p.at, p.periods, p.throttled, p.read = at, st.Periods, st.Throttled, true
}

// endMinute keeps the minute gathered as a point, if a delta was taken in
// it, the oldest dropped past usagePoints.
func (p *poolUsage) endMinute() {
	if p.measured {
		p.points = append(p.points, poolPoint{at: p.minute, periods: p.dPeriods, throttled: p.dThr})
		if n := len(p.points); n > usagePoints {
			p.points = slices.Delete(p.points, 0, n-usagePoints)
		}
	}
	p.dPeriods, p.dThr, p.measured = 0, 0, false
}

// slowed is the share of the periods in the points kept in which the
// limit held the pool's VMs back, and false when no period passed in them,
// as in a pool with no limit. Not throttled_usec: it is summed over CPUs,
// so it can read past the time passed.
func slowed(points []poolPoint) (float64, bool) {
	var periods, throttled uint64
	for _, pt := range points {
		periods += pt.periods
		throttled += pt.throttled
	}
	if periods == 0 {
		return 0, false
	}
	return min(float64(throttled)/float64(periods), 1), true
}

// poolSlowed is the pool's slowed share over the points kept, and whether
// there is one.
func (u *usages) poolSlowed(pool string) (float64, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	p, ok := u.pools[pool]
	if !ok {
		return 0, false
	}
	return slowed(p.points)
}

// sampleCPUPools samples each CPU pool's slice's counters, and forgets the
// pools the host no longer reports.
func (a *Agent) sampleCPUPools(ctx context.Context) {
	stats, err := a.cfg.Drivers.Hypervisor.CPUPoolStats(ctx)
	if err != nil {
		a.cfg.Log.Debug("reading the CPU pools' counters", "error", err)
		return
	}
	at := a.cfg.Clock.Now()
	u := a.usages
	u.mu.Lock()
	defer u.mu.Unlock()
	for name := range u.pools {
		if _, ok := stats[name]; !ok {
			delete(u.pools, name)
		}
	}
	for name, st := range stats {
		p := u.pools[name]
		if p == nil {
			p = &poolUsage{}
			u.pools[name] = p
		}
		p.record(at, st, 2*a.cfg.UsagePoll)
	}
}

// cpuPoolHeld is the CPUs, in millicores, the node holds the pool to now,
// as its machines' specs read give it (readCPUPools); zero for none.
func (a *Agent) cpuPoolHeld(pool string) int64 {
	a.host.Lock()
	defer a.host.Unlock()
	return a.readCPUPools().want[pool]
}

// cpuPoolAnswer is what the node says of the pool: its slowed share and
// its CPUs, each nil when it has none.
func (a *Agent) cpuPoolAnswer(pool string) (share *float64, milliCPU *int64) {
	if v, ok := a.usages.poolSlowed(pool); ok {
		share = &v
	}
	if m := a.cpuPoolHeld(pool); m > 0 {
		milliCPU = &m
	}
	return share, milliCPU
}

func (s nodeService) CPUPoolUsage(_ context.Context, req *connect.Request[nodev1.CPUPoolUsageRequest]) (*connect.Response[nodev1.CPUPoolUsageResponse], error) {
	if s.a.cfg.Drivers == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this node runs no machines"))
	}
	pool := req.Msg.GetPool()
	if pool == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("pool is required"))
	}
	share, milli := s.a.cpuPoolAnswer(pool)
	return connect.NewResponse(&nodev1.CPUPoolUsageResponse{Slowed: share, MilliCpu: milli}), nil
}
