package fake

import (
	"context"
	"fmt"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// A VM's balloon holds what SetBalloon last asked, at once; what its unit
// holds (HoldMemory) is then at most its memory less the balloon, and its
// container's guest has that much memory. A pool is held once a VM starts
// in it, at the size it started with or SetMemoryPool last set, and until
// SweepMemoryPools finds no running VM started in it.

func (d hypervisor) SetMemoryShare(_ context.Context, machineUID string, pool *driver.MemoryPool, guaranteedMiB int64) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	v, ok := d.h.vms[machineUID]
	if !ok || v.State != driver.VMRunning {
		return fmt.Errorf("machine %s: %w", machineUID, errUnknownVM)
	}
	v.pool, v.guaranteed = pool, guaranteedMiB
	return nil
}

func (d hypervisor) SetMemoryPool(_ context.Context, pool driver.MemoryPool) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if pool.SizeMiB <= 0 {
		return fmt.Errorf("memory pool %s has a size of %d MiB", pool.Name, pool.SizeMiB)
	}
	if d.h.poolMembersLocked(pool.Name) > 0 {
		d.h.pools[pool.Name] = pool.SizeMiB
	}
	return nil
}

func (d hypervisor) SweepMemoryPools(context.Context) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	for name := range d.h.pools {
		if d.h.poolMembersLocked(name) == 0 {
			delete(d.h.pools, name)
		}
	}
	return nil
}

// poolMembersLocked counts the running VMs started in the pool.
func (h *Host) poolMembersLocked(name string) int {
	n := 0
	for _, v := range h.vms {
		if v.State == driver.VMRunning && v.spec.MemoryPool != nil && v.spec.MemoryPool.Name == name {
			n++
		}
	}
	return n
}

func (d hypervisor) Balloon(_ context.Context, machineUID string) (driver.Balloon, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	v, ok := d.h.vms[machineUID]
	if !ok || v.State != driver.VMRunning {
		return driver.Balloon{}, fmt.Errorf("machine %s: %w", machineUID, errUnknownVM)
	}
	b := driver.Balloon{Actual: v.balloon}
	if !v.spec.Firmware && v.spec.FileShareServer == nil && !d.h.oldMemGuests[machineUID] {
		total := uint64(v.spec.MemoryMiB)<<20 - v.balloon
		used, ok := d.h.guestMemUsed[machineUID]
		if !ok {
			used = uint64(v.spec.MemoryMiB) << 20 / 4
		}
		b.Total, b.Available, b.Updated = total, total-min(used, total), time.Now()
	}
	return b, nil
}

func (d hypervisor) SetBalloon(_ context.Context, machineUID string, size uint64) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	v, ok := d.h.vms[machineUID]
	if !ok || v.State != driver.VMRunning {
		return fmt.Errorf("machine %s: %w", machineUID, errUnknownVM)
	}
	if size > uint64(v.spec.MemoryMiB)<<20 {
		return fmt.Errorf("machine %s: a balloon of %d bytes is past its memory", machineUID, size)
	}
	v.balloon = size
	return nil
}

func (d hypervisor) OOMGroupKills(context.Context) (uint64, error) { return 0, nil }

func (d hypervisor) MemoryEvents() <-chan struct{} { return d.h.memEvents }

// SignalMemoryEvents signals MemoryEvents, as a pool at its limit would.
func (h *Host) SignalMemoryEvents() {
	select {
	case h.memEvents <- struct{}{}:
	default:
	}
}

// MemoryPool is the size, in MiB, the host holds the pool to, and whether
// it holds it at all.
func (h *Host) MemoryPool(name string) (int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	size, ok := h.pools[name]
	return size, ok
}

// MemoryShare is the pool and guarantee the machine's running VM was last
// given; ok is false with no running VM.
func (h *Host) MemoryShare(machineUID string) (pool *driver.MemoryPool, guaranteedMiB int64, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, found := h.vms[machineUID]
	if !found || v.State != driver.VMRunning {
		return nil, 0, false
	}
	return v.pool, v.guaranteed, true
}

// BalloonOf is what the machine's VM's balloon holds.
func (h *Host) BalloonOf(machineUID string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v, ok := h.vms[machineUID]; ok {
		return v.balloon
	}
	return 0
}
