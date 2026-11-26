package node

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
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
// left when the pool is short.
type memoryShare struct {
	key                      controller.Key
	pool                     *driver.MemoryPool
	memoryMiB, guaranteedMiB int64
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
// outside a pool it is guaranteed all of it.
func shareOf(m *computev1alpha1.Machine, memoryMiB int64) (memoryShare, error) {
	s := memoryShare{key: controller.Key{Project: m.GetMetadata().GetProject(), Name: m.GetMetadata().GetName()}, memoryMiB: memoryMiB, guaranteedMiB: memoryMiB}
	p := m.GetSpec().GetMemoryPool()
	if p == nil {
		return s, nil
	}
	size, err := quantity.Bytes(p.GetSize())
	if err != nil {
		return s, fmt.Errorf("memory pool %s's size: %w", p.GetName(), err)
	}
	s.pool = &driver.MemoryPool{Name: p.GetName(), SizeMiB: int64(size / quantity.MiB)}
	if g := m.GetSpec().GetResources().GetMemoryGuaranteed(); g != "" {
		b, err := quantity.Bytes(g)
		if err != nil {
			return s, fmt.Errorf("memory_guaranteed: %w", err)
		}
		s.guaranteedMiB = min(int64(b/quantity.MiB), memoryMiB)
	}
	return s, nil
}

// withMemoryShare is spec, of machine m, with its memory pool and
// guarantee as m's spec says.
func withMemoryShare(spec driver.VMSpec, m *computev1alpha1.Machine) (driver.VMSpec, error) {
	s, err := shareOf(m, spec.MemoryMiB)
	if err != nil {
		return spec, blocked(computeapi.ReasonBootFailed, err)
	}
	spec.MemoryPool, spec.GuaranteedMiB = s.pool, s.specGuaranteed()
	return spec, nil
}

// specGuaranteed is GuaranteedMiB of a VMSpec of the share: zero for all
// of its memory.
func (s memoryShare) specGuaranteed() int64 {
	if s.pool == nil || s.guaranteedMiB >= s.memoryMiB {
		return 0
	}
	return s.guaranteedMiB
}

// trackMemory keeps what a machine on the node, wanted running, says of
// its memory for the squeeze, or forgets it. a.host is held.
func (a *Agent) trackMemory(uid string, s *memoryShare) {
	if s == nil {
		delete(a.memoryShares, uid)
		return
	}
	a.memoryShares[uid] = *s
}

// applyMemoryShare gives the machine's running VM what follows its pool
// and guarantee while it runs.
func (a *Agent) applyMemoryShare(ctx context.Context, uid string, s memoryShare) {
	if err := a.cfg.Drivers.Hypervisor.SetMemoryShare(ctx, uid, s.pool, s.specGuaranteed()); err != nil {
		a.cfg.Log.Warn("setting the machine's share of the CPUs and its OOM score", "machine_uid", uid, "error", err)
	}
}

// squeezeLoop checks every memory pool on the node as squeeze.Run says.
func (a *Agent) squeezeLoop(ctx context.Context) {
	squeeze.Run(ctx, a.cfg.Clock, a.cfg.Drivers.Hypervisor.MemoryEvents(), a.cfg.SqueezePoll, a.squeeze)
}

// squeeze checks each memory pool of the machines running on the node, off
// the host lock, with the larger step under pressure; it says whether any
// is under pressure now. It keeps nothing from one poll to the next: the
// host and the specs say it all.
func (a *Agent) squeeze(ctx context.Context, pressure bool) bool {
	hv := a.cfg.Drivers.Hypervisor
	vms, err := hv.VMs(ctx)
	if err != nil {
		a.cfg.Log.Debug("listing VMs to squeeze their pools", "error", err)
		return pressure
	}
	a.host.Lock()
	shares := maps.Clone(a.memoryShares)
	a.host.Unlock()
	pools := map[string][]string{}
	started := map[string]time.Time{}
	for _, vm := range vms {
		if s, ok := shares[vm.MachineUID]; ok && s.pool != nil && vm.State == driver.VMRunning {
			pools[s.pool.Name] = append(pools[s.pool.Name], vm.MachineUID)
			started[vm.MachineUID] = vm.Started
		}
	}
	keep := map[string]bool{}
	for _, uids := range pools {
		for _, uid := range uids {
			keep[uid] = true
		}
	}
	a.squeezer.Forget(keep)
	step := int64(squeeze.StepMiB)
	if pressure {
		step = squeeze.PressureStepMiB
	}
	hot := false
	for _, name := range slices.Sorted(maps.Keys(pools)) {
		if ctx.Err() != nil {
			return hot
		}
		hot = a.squeezePool(ctx, name, pools[name], shares, started, step) || hot
	}
	return hot
}

// squeezePool checks one pool, of the running machines uids, and says
// whether it is under pressure.
func (a *Agent) squeezePool(ctx context.Context, name string, uids []string, shares map[string]memoryShare, started map[string]time.Time, step int64) bool {
	hv := a.cfg.Drivers.Hypervisor
	// The pool's machines' specs may disagree for a moment as it changes:
	// the least is the safe one.
	pool := driver.MemoryPool{Name: name}
	members := make([]squeeze.Member, 0, len(uids))
	for _, uid := range uids {
		s := shares[uid]
		if pool.SizeMiB == 0 || s.pool.SizeMiB < pool.SizeMiB {
			pool.SizeMiB = s.pool.SizeMiB
		}
		members = append(members, squeeze.Member{UID: uid, MemoryMiB: s.memoryMiB, GuaranteedMiB: s.guaranteedMiB, Started: started[uid]})
	}
	if err := hv.SetMemoryPool(ctx, pool); err != nil {
		a.cfg.Log.Warn("setting the memory pool's limit", "pool", name, "error", err)
	}
	r, err := a.squeezer.Poll(ctx, hv, a.cfg.Log, a.cfg.Clock.Now(), pool, members, step)
	if err != nil {
		a.cfg.Log.Debug("reading a memory pool", "pool", name, "error", err)
		return false
	}
	if r.Stage != squeeze.Hold {
		a.metrics.squeezed(r.Stage)
		a.cfg.Log.Debug("squeezed a memory pool", "pool", name, "stage", r.Stage.String(), "balloons_mib", r.Balloons, "allowances_mib", r.Allowances)
	}
	return r.Hot
}
