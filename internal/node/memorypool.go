package node

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/squeeze"
)

// DefaultSqueezePoll is how often each memory pool on the node is checked
// by default (docs/decisions/node-agent.md).
const DefaultSqueezePoll = 5 * time.Second

// memoryShare is what a machine's spec says of its memory, as its last
// reconcile read it: its pool, if any, and the least of its memory it is
// left when the pool is short; and of its CPUs, its CPU pool, if any.
type memoryShare struct {
	key                    controller.Key
	pool                   *driver.MemoryPool
	memoryMiB, reservedMiB int64
	cpuPool                *driver.CPUPool
}

// wantedShare is what a machine's spec says of its memory, nil for one
// not wanted running, or whose spec cannot say: its converge refuses it.
func wantedShare(m *computev1alpha1.Machine) *memoryShare {
	if m.GetMetadata().GetDeleteTime() != nil || m.GetSpec().GetRunState() == computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED {
		return nil
	}
	_, memoryMiB, err := vmSize(m.GetSpec().GetResources())
	if err != nil {
		return nil
	}
	s, err := shareOf(m, memoryMiB)
	if err != nil {
		return nil
	}
	return &s
}

// shareOf reads what a machine's spec says of its memory, of memoryMiB:
// outside a pool all of it is reserved.
func shareOf(m *computev1alpha1.Machine, memoryMiB int64) (memoryShare, error) {
	s := memoryShare{key: controller.Key{Project: m.GetMetadata().GetProject(), Name: m.GetMetadata().GetName()}, memoryMiB: memoryMiB, reservedMiB: memoryMiB}
	if c := m.GetSpec().GetCpuPool(); c != nil {
		milli, err := quantity.MilliCPU(c.GetCpu())
		if err != nil {
			return s, fmt.Errorf("CPU pool %s's cpu: %w", c.GetName(), err)
		}
		s.cpuPool = &driver.CPUPool{Name: c.GetName(), MilliCPU: milli}
	}
	p := m.GetSpec().GetMemoryPool()
	if p == nil {
		return s, nil
	}
	size, err := quantity.Bytes(p.GetSize())
	if err != nil {
		return s, fmt.Errorf("memory pool %s's size: %w", p.GetName(), err)
	}
	s.pool = &driver.MemoryPool{Name: p.GetName(), SizeMiB: int64(size / quantity.MiB)}
	if g := m.GetSpec().GetResources().GetMemoryReserved(); g != "" {
		b, err := quantity.Bytes(g)
		if err != nil {
			return s, fmt.Errorf("memory_reserved: %w", err)
		}
		s.reservedMiB = min(int64(b/quantity.MiB), memoryMiB)
	}
	return s, nil
}

// withMemoryShare is spec, of machine m, with its memory pool and
// reservation as m's spec says.
func withMemoryShare(spec driver.VMSpec, m *computev1alpha1.Machine) (driver.VMSpec, error) {
	s, err := shareOf(m, spec.MemoryMiB)
	if err != nil {
		return spec, blocked(computeapi.ReasonBootFailed, err)
	}
	spec.MemoryPool, spec.ReservedMiB, spec.CPUPool = s.pool, s.specReserved(), s.cpuPool
	return spec, nil
}

// specReserved is ReservedMiB of a VMSpec of the share: zero for all
// of its memory.
func (s memoryShare) specReserved() int64 {
	if s.pool == nil || s.reservedMiB >= s.memoryMiB {
		return 0
	}
	return s.reservedMiB
}

// trackMemory keeps what a machine on the node, wanted running, says of
// its memory for the squeeze, or forgets it. a.host is held.
func (a *Agent) trackMemory(uid string, s *memoryShare) {
	a.sharesRead++
	if s == nil {
		delete(a.memoryShares, uid)
		return
	}
	a.memoryShares[uid] = *s
}

// applyMemoryShare gives the machine's running VM what follows its pool
// and reservation while it runs.
func (a *Agent) applyMemoryShare(ctx context.Context, uid string, s memoryShare) {
	if err := a.cfg.Drivers.Hypervisor.SetMemoryShare(ctx, uid, s.pool, s.specReserved()); err != nil {
		a.cfg.Log.Warn("setting the machine's share of the CPUs and its OOM score", "machine_uid", uid, "error", err)
	}
}

// cpuPoolsRead is what the CPU pools' limits are set from: the limits, in
// millicores, the specs of the machines read give their pools, the most
// while they disagree as a plan changes; which machines were read; and
// a.sharesRead then.
type cpuPoolsRead struct {
	want map[string]int64
	read map[string]bool
	at   uint64
}

// readCPUPools reads the CPU pools' limits from memoryShares. a.host is
// held.
func (a *Agent) readCPUPools() cpuPoolsRead {
	r := cpuPoolsRead{want: map[string]int64{}, read: map[string]bool{}, at: a.sharesRead}
	for uid, s := range a.memoryShares {
		r.read[uid] = true
		if p := s.cpuPool; p != nil {
			r.want[p.Name] = max(r.want[p.Name], p.MilliCPU)
		}
	}
	return r
}

// setCPUPools holds each pool on the host to the CPUs r gives it
// (docs/decisions/cpu-pool.md), unless the limits were set from a later
// read already. A pool none of the machines read names has none, but for
// what its VMs not read yet were launched with, so the agent starting
// leaves each pool as it was.
func (a *Agent) setCPUPools(ctx context.Context, r cpuPoolsRead) {
	a.cpuPools.Lock()
	defer a.cpuPools.Unlock()
	if r.at < a.cpuPoolsSet {
		return
	}
	if err := a.cfg.Drivers.Hypervisor.SetCPUPools(ctx, r.want, r.read); err != nil {
		a.cfg.Log.Warn("setting the CPU pools' limits", "error", err)
		return
	}
	a.cpuPoolsSet = r.at
}

// applyCPUPools holds each pool on the host to the CPUs the machines' specs
// give it now (setCPUPools). a.host is held.
func (a *Agent) applyCPUPools(ctx context.Context) {
	a.setCPUPools(ctx, a.readCPUPools())
}

// squeezeLoop checks every memory pool on the node as squeeze.Run says.
// Each check holds the CPU pools to their limits again too.
func (a *Agent) squeezeLoop(ctx context.Context) {
	squeeze.Run(ctx, a.cfg.Clock, a.cfg.Drivers.Hypervisor.MemoryEvents(), a.cfg.SqueezePoll, a.squeeze)
}

// squeeze checks each memory pool on the node, off the host lock, with the
// larger step under pressure; it says whether any is under pressure now.
// What a pool holds is read back from the host, its slice's VMs as they
// were started; the specs say only what of it changes while they run.
func (a *Agent) squeeze(ctx context.Context, pressure bool) bool {
	hv := a.cfg.Drivers.Hypervisor
	a.host.Lock()
	cpus := a.readCPUPools()
	a.host.Unlock()
	a.setCPUPools(ctx, cpus)
	inPools, err := hv.PoolVMs(ctx)
	if err != nil {
		a.cfg.Log.Debug("listing the pools' VMs", "error", err)
		return pressure
	}
	vms, err := hv.VMs(ctx)
	if err != nil {
		a.cfg.Log.Debug("listing VMs to squeeze their pools", "error", err)
		return pressure
	}
	a.host.Lock()
	shares := maps.Clone(a.memoryShares)
	a.host.Unlock()
	plans := planPools(inPools, vms, shares)
	keep := map[string]bool{}
	for _, p := range plans {
		for _, m := range p.members {
			keep[m.UID] = true
		}
	}
	a.squeezer.Forget(keep)
	a.poolReads.forget(plans)
	oom := lastOOMKills(vms)
	step := int64(squeeze.StepMiB)
	if pressure {
		step = squeeze.PressureStepMiB
	}
	hot := false
	for _, name := range slices.Sorted(maps.Keys(plans)) {
		if ctx.Err() != nil {
			return hot
		}
		hot = a.squeezePool(ctx, plans[name], shares, oom, step) || hot
	}
	return hot
}

// poolPlan is what one pool's poll works on.
type poolPlan struct {
	name    string
	members []squeeze.Member
	// sizeMiB is what the members share: the pool's size, or its members'
	// reservations if more, and the memory of those in its slice that left it.
	sizeMiB int64
	// inSlice are the members in its slice; the others run outside it, as
	// started before their machines joined the pool.
	inSlice map[string]bool
	// belongs says some VM in its slice is still of the pool; with none, its
	// slice holds them to no limit until swept.
	belongs bool
}

// planPools is each pool's plan, from its slices' VMs as launched, the running VMs, and the specs (docs/decisions/node-agent.md "Memory pools").
func planPools(inPools []driver.PoolVM, vms []driver.VM, shares map[string]memoryShare) map[string]*poolPlan {
	plans := map[string]*poolPlan{}
	planOf := func(name string) *poolPlan {
		p := plans[name]
		if p == nil {
			p = &poolPlan{name: name, inSlice: map[string]bool{}}
			plans[name] = p
		}
		return p
	}
	specSize, startedSize := map[string]int64{}, map[string]int64{}
	least := func(m map[string]int64, name string, size int64) {
		if have, ok := m[name]; !ok || size < have {
			m[name] = size
		}
	}
	reserved, left := map[string]int64{}, map[string]int64{}
	inSlice := map[string]bool{}
	for _, v := range inPools {
		name := v.Pool.Name
		p := planOf(name)
		p.inSlice[v.MachineUID] = true
		inSlice[v.MachineUID] = true
		m := squeeze.Member{UID: v.MachineUID, MemoryMiB: v.MemoryMiB, ReservedMiB: v.ReservedMiB, Started: v.Started}
		s, read := shares[v.MachineUID]
		switch {
		case !read:
			least(startedSize, name, v.Pool.SizeMiB)
		case s.pool != nil && s.pool.Name == name:
			least(specSize, name, s.pool.SizeMiB)
			m.ReservedMiB = min(s.reservedMiB, v.MemoryMiB)
		default:
			m.ReservedMiB = v.MemoryMiB
			left[name] += v.MemoryMiB
			p.members = append(p.members, m)
			continue
		}
		p.belongs = true
		reserved[name] += m.ReservedMiB
		p.members = append(p.members, m)
	}
	for _, vm := range vms {
		s, ok := shares[vm.MachineUID]
		if !ok || s.pool == nil || vm.State != driver.VMRunning || inSlice[vm.MachineUID] {
			continue
		}
		name := s.pool.Name
		p := planOf(name)
		p.belongs = true
		least(specSize, name, s.pool.SizeMiB)
		reserved[name] += s.reservedMiB
		p.members = append(p.members, squeeze.Member{UID: vm.MachineUID, MemoryMiB: s.memoryMiB, ReservedMiB: s.memoryMiB, Started: vm.Started})
	}
	for name, p := range plans {
		size, ok := specSize[name]
		if !ok {
			size = startedSize[name]
		}
		p.sizeMiB = max(size, reserved[name]) + left[name]
	}
	return plans
}

// sliceLimit is the guests' memory a pool's slice is held to, zero for none (docs/decisions/node-agent.md "Memory pools").
func (p *poolPlan) sliceLimit(sizes map[string]int64) int64 {
	if !p.belongs {
		return 0
	}
	limit, held := p.sizeMiB, int64(0)
	for _, m := range p.members {
		if !p.inSlice[m.UID] {
			limit -= m.MemoryMiB
			continue
		}
		held += sizes[m.UID]
	}
	return max(limit, held, 1)
}

// squeezePool polls one pool, sets its slice's limit, keeps what it read
// for MemoryPoolUsage, oom being the VMs' last OOM kills, and says whether
// it is under pressure.
func (a *Agent) squeezePool(ctx context.Context, p *poolPlan, shares map[string]memoryShare, oom map[string]time.Time, step int64) bool {
	hv := a.cfg.Drivers.Hypervisor
	now := a.cfg.Clock.Now()
	r, err := a.squeezer.Poll(ctx, hv, a.cfg.Log, now, p.sizeMiB, p.members, step)
	if err != nil {
		a.cfg.Log.Debug("reading a memory pool", "pool", p.name, "error", err)
		return false
	}
	a.poolReads.keep(p.name, readOf(now, p, r, oom))
	if err := hv.SetMemoryPool(ctx, p.name, p.sliceLimit(r.Sizes)); err != nil {
		a.cfg.Log.Warn("setting the memory pool's limit", "pool", p.name, "error", err)
	}
	if r.Stage != squeeze.Hold {
		a.metrics.squeezed(r.Stage)
		a.cfg.Log.Debug("squeezed a memory pool", "pool", p.name, "stage", r.Stage.String(), "balloons_mib", r.Balloons, "allowances_mib", r.Allowances)
	}
	for _, uid := range r.Unresponsive {
		a.metrics.balloonUnresponsive.Inc()
		a.cfg.Log.Warn("a pool's machine's balloon does not follow what it is asked; it is asked nothing more until its VM starts again", "pool", p.name, "machine_uid", uid)
		if s, ok := shares[uid]; ok && a.requeue != nil {
			// Its reconcile reports it.
			a.requeue(s.key)
		}
	}
	return r.Hot
}

// deflateLeft gives back, whole, the balloon of each running VM in no
// pool's slice: none is ever squeezed, but one whose pool's slice it has
// left by starting again may have been.
func (a *Agent) deflateLeft(ctx context.Context, vms []driver.VM) {
	hv := a.cfg.Drivers.Hypervisor
	inPools, err := hv.PoolVMs(ctx)
	if err != nil {
		return
	}
	pooled := map[string]bool{}
	for _, v := range inPools {
		pooled[v.MachineUID] = true
	}
	for _, vm := range vms {
		if vm.State != driver.VMRunning || pooled[vm.MachineUID] || vm.FileShareServer {
			continue
		}
		if b, err := hv.Balloon(ctx, vm.MachineUID); err == nil && b.Actual > 0 {
			if err := hv.SetBalloon(ctx, vm.MachineUID, 0); err != nil {
				a.cfg.Log.Warn("giving back a balloon", "machine_uid", vm.MachineUID, "error", err)
			}
		}
	}
}

// memoryConditions are what the node says of a machine's memory beside its
// Ready: that its balloon does not follow, while so; and when the host's
// OOM killer last killed its VM whole, which stays.
func (a *Agent) memoryConditions(uid string, vm *driver.VM) []*metav1.Condition {
	var out []*metav1.Condition
	if a.squeezer.Unresponsive(uid) {
		out = append(out, &metav1.Condition{
			Type: computeapi.MachineBalloonResponsive, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE,
			Reason:  computeapi.ReasonBalloonUnresponsive,
			Message: "the machine's balloon does not follow what its node asks, as a guest with no balloon driver: it is held at all it has until it starts again",
		})
	}
	if vm != nil && !vm.LastOOMKill.IsZero() {
		out = append(out, &metav1.Condition{
			Type: computeapi.MachineOOMKilled, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE,
			Reason:  computeapi.ReasonOutOfMemory,
			Message: "the host killed the machine's VM whole at " + vm.LastOOMKill.UTC().Format(time.RFC3339) + ", its tenant's machines on the node out of the memory they share",
		})
	}
	return out
}
