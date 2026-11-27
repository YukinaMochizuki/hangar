package fake

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// A VM's balloon holds what SetBalloon last asked, at once; what its unit
// holds (HoldMemory) is then at most its memory less the balloon, and its
// container's guest has that much memory. A pool is held once a VM starts
// in it, at the size it started with or SetMemoryPool last set, and until
// SweepMemoryPools finds no running VM started in it.

func (d hypervisor) SetMemoryShare(_ context.Context, machineUID string, pool *driver.MemoryPool, reservedMiB int64) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	v, ok := d.h.vms[machineUID]
	if !ok || v.State != driver.VMRunning {
		return fmt.Errorf("machine %s: %w", machineUID, errUnknownVM)
	}
	v.pool, v.reserved = pool, reservedMiB
	return nil
}

// poolName is what names a pool, as chvm's: a DNS label.
var poolName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func (d hypervisor) SetMemoryPool(_ context.Context, name string, guestMiB int64) error {
	if !poolName.MatchString(name) {
		return fmt.Errorf("memory pool %q cannot name a slice", name)
	}
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.poolMembersLocked(name) > 0 {
		d.h.pools[name] = guestMiB
	}
	return nil
}

func (d hypervisor) PoolVMs(context.Context) ([]driver.PoolVM, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	var out []driver.PoolVM
	for _, uid := range slices.Sorted(maps.Keys(d.h.vms)) {
		v := d.h.vms[uid]
		if v.State != driver.VMRunning || v.spec.MemoryPool == nil {
			continue
		}
		out = append(out, driver.PoolVM{
			MachineUID: uid, Pool: *v.spec.MemoryPool, MemoryMiB: v.spec.MemoryMiB,
			ReservedMiB: v.spec.Reserved(), Started: v.Started,
		})
	}
	return out, nil
}

// OOMKill has the host's OOM killer kill the machine's running VM whole:
// its restart policy starts it again, or it is exited, killed; either way
// the VM says when.
func (h *Host) OOMKill(machineUID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.vms[machineUID]
	if !ok || v.State != driver.VMRunning {
		return fmt.Errorf("no running VM for machine %s", machineUID)
	}
	h.endExecsLocked(machineUID)
	v.LastOOMKill = time.Now()
	if v.spec.Restart == driver.RestartNever {
		v.State, v.ExitCode, v.OOMKilled, v.balloon = driver.VMExited, 253, true, 0
	} else {
		v.boots++
		v.Started = time.Now()
		h.boot(machineUID)
		h.bootGuest(v)
	}
	signal(h.vmChanges)
	return nil
}

// UnresponsiveBalloon has the machine's VM's balloon take no SetBalloon
// from now, as a guest with no balloon driver, or a hung one.
func (h *Host) UnresponsiveBalloon(machineUID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stuckBalloons[machineUID] = true
}

func (d hypervisor) SweepMemoryPools(context.Context) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	for name := range d.h.pools {
		if d.h.poolMembersLocked(name) == 0 {
			delete(d.h.pools, name)
		}
	}
	// A CPU pool's slice goes with its last VM, as the real driver's does.
	for name := range d.h.cpuPools {
		if !slices.ContainsFunc(slices.Collect(maps.Values(d.h.vms)), func(v *vm) bool { return v.State == driver.VMRunning && v.spec.Pool() == name }) {
			delete(d.h.cpuPools, name)
		}
	}
	return nil
}

func (d hypervisor) SetCPUPools(_ context.Context, want map[string]int64, read map[string]bool) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	limits := map[string]int64{}
	for uid, v := range d.h.vms {
		name := v.spec.Pool()
		if v.State != driver.VMRunning || name == "" {
			continue
		}
		limits[name] = max(limits[name], want[name])
		if p := v.spec.CPUPool; p != nil && !read[uid] {
			limits[name] = max(limits[name], p.MilliCPU)
		}
	}
	for name, m := range limits {
		if m > 0 {
			d.h.cpuPools[name] = m
		} else {
			delete(d.h.cpuPools, name)
		}
	}
	return nil
}

func (d hypervisor) CPUPoolStats(context.Context) (map[string]driver.CPUPoolStat, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	out := map[string]driver.CPUPoolStat{}
	for _, v := range d.h.vms {
		if name := v.spec.Pool(); v.State == driver.VMRunning && name != "" {
			out[name] = d.h.cpuStats[name]
		}
	}
	return out, nil
}

// CPUPool is the CPU limit, in millicores, SetCPUPools holds the pool to;
// zero for none.
func (h *Host) CPUPool(name string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cpuPools[name]
}

// SetCPUPoolStat sets what CPUPoolStats answers for the pool.
func (h *Host) SetCPUPoolStat(name string, st driver.CPUPoolStat) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cpuStats[name] = st
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
	if d.h.stuckBalloons[machineUID] {
		return nil
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

// MemoryShare is the pool and reservation the machine's running VM was last
// given; ok is false with no running VM.
func (h *Host) MemoryShare(machineUID string) (pool *driver.MemoryPool, reservedMiB int64, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, found := h.vms[machineUID]
	if !found || v.State != driver.VMRunning {
		return nil, 0, false
	}
	return v.pool, v.reserved, true
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
