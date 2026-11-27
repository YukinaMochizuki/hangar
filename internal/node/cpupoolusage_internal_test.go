package node

import (
	"testing"
	"time"

	"connectrpc.com/connect"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// poolT0 is the first sample's time in the pool usage tests.
var poolT0 = time.Date(2026, 11, 9, 12, 0, 5, 0, time.UTC)

// poolMaxSpan is the sampler's bound on the time between two samples
// whose deltas count.
const poolMaxSpan = 2 * DefaultUsagePoll

// st is a pool's counters.
func st(periods, throttled uint64) driver.CPUPoolStat {
	return driver.CPUPoolStat{Periods: periods, Throttled: throttled}
}

// wantPoints fails unless p keeps exactly want.
func wantPoints(t *testing.T, p *poolUsage, want ...poolPoint) {
	t.Helper()
	if len(p.points) != len(want) {
		t.Fatalf("the points are %v, want %v", p.points, want)
	}
	for i, w := range want {
		if g := p.points[i]; !g.at.Equal(w.at) || g.periods != w.periods || g.throttled != w.throttled {
			t.Errorf("point %d is %v, want %v", i, g, w)
		}
	}
}

// TestPoolUsageIsPerMinute: a minute's point holds the deltas of the
// samples taken in it; the delta of a minute's first sample, from the
// minute before's last, is its own minute's.
func TestPoolUsageIsPerMinute(t *testing.T) {
	t.Parallel()
	var p poolUsage
	for i := range uint64(7) {
		p.record(poolT0.Add(time.Duration(i)*DefaultUsagePoll), st(i*500, i*50), poolMaxSpan)
	}
	// 12:00:05 to 12:00:55 in the first minute; 12:01:05 ended it.
	wantPoints(t, &p, poolPoint{at: poolT0.Truncate(time.Minute), periods: 2500, throttled: 250})
	if got, ok := slowed(p.points); !ok || !near(got, 0.1) {
		t.Errorf("slowed is %v, %v; want 0.1", got, ok)
	}
}

// TestPoolUsageAfterAGap: a sample more than two polls after the one
// before counts nothing, and a minute with no sample has no point.
func TestPoolUsageAfterAGap(t *testing.T) {
	t.Parallel()
	var p poolUsage
	minute := poolT0.Truncate(time.Minute)
	p.record(minute.Add(5*time.Second), st(0, 0), poolMaxSpan)
	p.record(minute.Add(15*time.Second), st(500, 100), poolMaxSpan)
	// Missed :25 and :35: the 1000 periods between :15 and :45 are lost.
	p.record(minute.Add(45*time.Second), st(1500, 900), poolMaxSpan)
	p.record(minute.Add(55*time.Second), st(2000, 900), poolMaxSpan)
	// Nothing in 12:01, then a gap into 12:02.
	p.record(minute.Add(2*time.Minute+15*time.Second), st(9000, 9000), poolMaxSpan)
	p.record(minute.Add(2*time.Minute+25*time.Second), st(9500, 9100), poolMaxSpan)
	p.record(minute.Add(3*time.Minute+5*time.Second), st(10000, 9100), poolMaxSpan)
	wantPoints(t, &p,
		poolPoint{at: minute, periods: 1000, throttled: 100},
		poolPoint{at: minute.Add(2 * time.Minute), periods: 500, throttled: 100},
	)
}

// TestPoolUsageAcrossACounterReset: counters going back, as from a slice
// made again, count nothing in that sample; the next counts from them. A
// minute whose one delta crossed a reset has no point.
func TestPoolUsageAcrossACounterReset(t *testing.T) {
	t.Parallel()
	var p poolUsage
	minute := poolT0.Truncate(time.Minute)
	p.record(minute.Add(5*time.Second), st(5000, 0), poolMaxSpan)
	p.record(minute.Add(15*time.Second), st(5500, 0), poolMaxSpan)
	// Only the periods going back, nothing having been throttled.
	p.record(minute.Add(25*time.Second), st(100, 0), poolMaxSpan)
	p.record(minute.Add(35*time.Second), st(600, 50), poolMaxSpan)
	p.record(minute.Add(45*time.Second), st(600, 50), poolMaxSpan)
	// Only the throttled count going back.
	p.record(minute.Add(55*time.Second), st(1100, 10), poolMaxSpan)
	// 12:01's one sample went back.
	p.record(minute.Add(time.Minute+5*time.Second), st(50, 0), poolMaxSpan)
	p.record(minute.Add(2*time.Minute+5*time.Second), st(5000, 0), poolMaxSpan)
	wantPoints(t, &p, poolPoint{at: minute, periods: 1000, throttled: 50})
}

// TestPoolUsageKeepsAnHour: past 60 minutes the oldest point goes, and
// what it counted with it.
func TestPoolUsageKeepsAnHour(t *testing.T) {
	t.Parallel()
	var p poolUsage
	minute := poolT0.Truncate(time.Minute)
	var periods, throttled uint64
	// Every period of the first ten minutes throttled, none after.
	for i := 0; i <= 70*6; i++ {
		at := poolT0.Add(time.Duration(i) * DefaultUsagePoll)
		if i > 0 {
			periods += 500
			if at.Sub(minute) < 10*time.Minute {
				throttled += 500
			}
		}
		p.record(at, st(periods, throttled), poolMaxSpan)
	}
	if len(p.points) != usagePoints {
		t.Fatalf("%d points kept, want %d", len(p.points), usagePoints)
	}
	if first := p.points[0].at; !first.Equal(minute.Add(10 * time.Minute)) {
		t.Errorf("the oldest point kept is of %v, want 12:10", first)
	}
	if got, ok := slowed(p.points); !ok || got != 0 {
		t.Errorf("slowed is %v, %v; want 0, the throttled minutes gone", got, ok)
	}
}

// TestPoolSlowedNeedsPeriods: no points, or points with no period, as of
// a pool with no CPU limit, have no slowed share.
func TestPoolSlowedNeedsPeriods(t *testing.T) {
	t.Parallel()
	if got, ok := slowed(nil); ok {
		t.Errorf("no points: slowed %v", got)
	}
	if got, ok := slowed([]poolPoint{{at: poolT0, periods: 0}, {at: poolT0.Add(time.Minute), periods: 0}}); ok {
		t.Errorf("no periods: slowed %v", got)
	}
	if got, ok := slowed([]poolPoint{{at: poolT0, periods: 300, throttled: 30}, {at: poolT0.Add(time.Minute), periods: 100, throttled: 90}}); !ok || !near(got, 0.3) {
		t.Errorf("400 periods, 120 throttled: slowed %v, %v; want 0.3", got, ok)
	}
}

// startPooled starts pc, a container of 1 vCPU in CPU pool acme of 2
// CPUs, which the agent holds the pool to, and mp, one of 2 vCPUs in
// memory pool memonly alone, whose slice has no CPU limit.
func startPooled(t *testing.T, u *usageHost) {
	t.Helper()
	ctx := t.Context()
	root, err := u.host.Storage().EnsureRoot(ctx, "pc", "sha256:a", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	pool := &driver.CPUPool{Name: "acme", MilliCPU: 2000}
	if err := u.host.Hypervisor().Start(ctx, driver.VMSpec{MachineUID: "pc", Machine: "p/pc", VCPUs: 1, MemoryMiB: 512, Root: root, CPUPool: pool}); err != nil {
		t.Fatal(err)
	}
	mroot, err := u.host.Storage().EnsureRoot(ctx, "mp", "sha256:a", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	mem := &driver.MemoryPool{Name: "memonly", SizeMiB: 1024}
	if err := u.host.Hypervisor().Start(ctx, driver.VMSpec{MachineUID: "mp", Machine: "p/mp", VCPUs: 2, MemoryMiB: 512, Root: mroot, MemoryPool: mem, ReservedMiB: 512}); err != nil {
		t.Fatal(err)
	}
	u.a.memoryShares = map[string]memoryShare{
		"pc": {cpuPool: pool, memoryMiB: 512, reservedMiB: 512},
		"mp": {pool: mem, memoryMiB: 512, reservedMiB: 512},
	}
}

// poolTick has the host count the pools' counters, then samples them a
// poll later.
func (u *usageHost) poolTick(t *testing.T, stats map[string]driver.CPUPoolStat) {
	t.Helper()
	for name, s := range stats {
		u.host.SetCPUPoolStat(name, s)
	}
	u.clock.Advance(DefaultUsagePoll)
	u.a.sampleCPUPools(t.Context())
}

func (u *usageHost) poolUsage(t *testing.T, pool string) *nodev1.CPUPoolUsageResponse {
	t.Helper()
	res, err := nodeService{a: u.a, agent: t.Context()}.CPUPoolUsage(t.Context(), connect.NewRequest(&nodev1.CPUPoolUsageRequest{Pool: pool}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg
}

// TestUsageSaysHowOftenTheCPUPoolSlowed: a pooled machine's usage, and
// its pool's, say the share of the pool's periods throttled in the minutes
// kept and the CPUs the node holds it to; a machine in no pool, a pool with
// no periods, and a pool the node knows nothing of, say neither.
func TestUsageSaysHowOftenTheCPUPoolSlowed(t *testing.T) {
	t.Parallel()
	u := newUsageHost(t)
	startPooled(t, u)
	for i := range uint64(6) {
		u.poolTick(t, map[string]driver.CPUPoolStat{"acme": st((i+1)*500, (i+1)*125), "memonly": st(0, 0)})
	}
	// 12:00:15 to 12:00:55 counted 2000 periods, 500 throttled; 12:01:05
	// ended the minute.
	pc := mustUsage(t, u, "pc")
	if pc.CpuPoolSlowed == nil || !near(pc.GetCpuPoolSlowed(), 0.25) || pc.GetCpuPoolMilliCpu() != 2000 {
		t.Errorf("the pooled machine's pool: slowed %v, %v millicores; want 0.25 of 2000", pc.CpuPoolSlowed, pc.CpuPoolMilliCpu)
	}
	for _, uid := range []string{"ct", "mp"} {
		if got := mustUsage(t, u, uid); got.CpuPoolSlowed != nil || got.CpuPoolMilliCpu != nil {
			t.Errorf("machine %s, in no CPU pool: slowed %v, %v millicores", uid, got.CpuPoolSlowed, got.CpuPoolMilliCpu)
		}
	}
	u.a.usages.mu.Lock()
	_, sampled := u.a.usages.pools["memonly"]
	u.a.usages.mu.Unlock()
	if !sampled {
		t.Error("the memory pool's slice was not sampled")
	}
	if got := u.poolUsage(t, "acme"); got.Slowed == nil || !near(got.GetSlowed(), 0.25) || got.GetMilliCpu() != 2000 {
		t.Errorf("pool acme: %v; want 0.25 of 2000 millicores", got)
	}
	for _, pool := range []string{"memonly", "absent"} {
		if got := u.poolUsage(t, pool); got.Slowed != nil || got.MilliCpu != nil {
			t.Errorf("pool %s: %v, want nothing", pool, got)
		}
	}
	if got := u.poolUsage(t, "acme").GetMinutes(); len(got) != 1 || !got[0].GetTime().AsTime().Equal(poolT0.Truncate(time.Minute)) || !near(got[0].GetSlowed(), 0.25) {
		t.Errorf("pool acme's minutes: %v, want 12:00 at 0.25", got)
	}
	// Once no VM runs in a pool the host reports it no more, and it is
	// forgotten.
	if err := u.host.Hypervisor().Stop(t.Context(), "pc"); err != nil {
		t.Fatal(err)
	}
	u.poolTick(t, nil)
	u.a.usages.mu.Lock()
	_, kept := u.a.usages.pools["acme"]
	u.a.usages.mu.Unlock()
	if kept {
		t.Error("pool acme, no longer reported, is still kept")
	}
}

// TestCPUPoolUsageSaysWhatEachMemberUsesNow: each VM running in the
// pool's slice is a member, with the CPU of its latest usage sample times
// its vCPUs; one in another pool's slice or in none is not. The oldest of
// the members' latest samples is when the pool was sampled; a member
// whose latest went stale says nothing of now, and is not counted in it.
func TestCPUPoolUsageSaysWhatEachMemberUsesNow(t *testing.T) {
	t.Parallel()
	u := newUsageHost(t)
	startPooled(t, u)
	u.tick(t, nil)
	// Over the next poll pc, of 1 vCPU, used 6 s of CPU: 600 millicores.
	u.tick(t, map[string]time.Duration{"pc": 6 * time.Second, "ct": 10 * time.Second, "mp": 2 * time.Second})
	sampled := u.clock.Now()
	res := u.poolUsage(t, "acme")
	if len(res.GetMembers()) != 1 || res.GetMembers()[0].GetMachineUid() != "pc" || res.GetMembers()[0].GetMilliCpu() != 600 {
		t.Fatalf("acme's members: %v, want pc at 600 millicores", res.GetMembers())
	}
	if !res.GetSampleTime().AsTime().Equal(sampled) {
		t.Errorf("sampled %v, want %v", res.GetSampleTime().AsTime(), sampled)
	}
	if got := u.poolUsage(t, "memonly").GetMembers(); len(got) != 1 || got[0].GetMachineUid() != "mp" || got[0].GetMilliCpu() != 200 {
		t.Errorf("memonly's members: %v, want mp at 200 millicores, 2 s over 10 s", got)
	}

	// Four polls on with no sample, pc's latest is stale.
	u.clock.Advance(4 * DefaultUsagePoll)
	res = u.poolUsage(t, "acme")
	if len(res.GetMembers()) != 1 || res.GetMembers()[0].MilliCpu != nil || res.GetSampleTime() != nil {
		t.Errorf("with pc's sample stale: %v, want pc with nothing of now, and no sample time", res)
	}
}

// TestPoolMinutesAreEachTheirOwn: each minute says its own share, and a
// minute in which no period passed is left out.
func TestPoolMinutesAreEachTheirOwn(t *testing.T) {
	t.Parallel()
	u := newUsages()
	minute := poolT0.Truncate(time.Minute)
	u.pools["acme"] = &poolUsage{points: []poolPoint{
		{at: minute, periods: 1000, throttled: 100},
		{at: minute.Add(time.Minute), periods: 0},
		{at: minute.Add(2 * time.Minute), periods: 500, throttled: 250},
	}}
	got := u.poolMinutes("acme")
	if len(got) != 2 || !got[0].GetTime().AsTime().Equal(minute) || !near(got[0].GetSlowed(), 0.1) ||
		!got[1].GetTime().AsTime().Equal(minute.Add(2*time.Minute)) || !near(got[1].GetSlowed(), 0.5) {
		t.Errorf("minutes %v, want 12:00 at 0.1 and 12:02 at 0.5", got)
	}
	if got := u.poolMinutes("absent"); got != nil {
		t.Errorf("a pool not sampled: %v", got)
	}
}

// TestPoolMembersSampledAtTheOldest: the pool's sample time is the oldest
// of its members' fresh latest samples, whichever order they are in.
func TestPoolMembersSampledAtTheOldest(t *testing.T) {
	t.Parallel()
	u := newUsages()
	for uid, at := range map[string]time.Time{"a": poolT0.Add(5 * time.Second), "b": poolT0, "c": poolT0.Add(9 * time.Second)} {
		u.machines[uid] = &machineUsage{vcpus: 1, pool: "acme", latest: &usageSample{at: at, cpu: 0.5}}
	}
	members, oldest := u.poolMembers("acme", poolT0.Add(-time.Minute))
	if len(members) != 3 || !oldest.Equal(poolT0) {
		t.Errorf("%d members sampled at %v, want 3 at %v", len(members), oldest, poolT0)
	}
}
