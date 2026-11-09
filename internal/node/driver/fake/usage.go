package fake

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// bootCPU is the CPU time a VM's unit has used once it booted.
const bootCPU = 100 * time.Millisecond

// A VM's unit uses CPU as RunCPU and SpinCPU say, from bootCPU each boot,
// and memory as HoldMemory says; a container's guest has the VM's memory
// and uses what UseGuestMemory says, or a quarter of it.

func (d hypervisor) Usage(_ context.Context, machineUID string) (driver.VMUsage, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	v, ok := d.h.vms[machineUID]
	if !ok || v.State != driver.VMRunning {
		return driver.VMUsage{}, fmt.Errorf("machine %s: %w", machineUID, errUnknownVM)
	}
	held, ok := d.h.memHeld[machineUID]
	if !ok {
		held = uint64(v.spec.MemoryMiB) << 20 / 2
	}
	return driver.VMUsage{CPU: v.cpuNow(), Memory: held}, nil
}

// cpuNow is the CPU time the VM's unit has used in its current boot.
func (v *vm) cpuNow() time.Duration {
	if v.spin == 0 {
		return v.cpu
	}
	return v.cpu + time.Duration(v.spin*float64(max(v.spec.VCPUs, 1))*float64(time.Since(v.spinFrom)))
}

// SpinCPU has the machine's running VM's unit keep its vCPUs busy, share
// of them all from now on, by the real clock, until told another share.
func (h *Host) SpinCPU(machineUID string, share float64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.vms[machineUID]
	if !ok || v.State != driver.VMRunning {
		return fmt.Errorf("no running VM for machine %s", machineUID)
	}
	v.cpu, v.spin, v.spinFrom = v.cpuNow(), share, time.Now()
	return nil
}

func (d guest) MemoryUsage(ctx context.Context, machineUID string) (driver.GuestMemory, error) {
	d.h.mu.Lock()
	hung := d.h.hung[machineUID]
	d.h.mu.Unlock()
	if hung {
		<-ctx.Done()
		return driver.GuestMemory{}, ctx.Err()
	}
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	v, ok := d.h.vms[machineUID]
	switch {
	case !ok || v.State != driver.VMRunning:
		return driver.GuestMemory{}, errUnknownVM
	case v.spec.Firmware:
		return driver.GuestMemory{}, errors.New("a firmware VM has no guest agent")
	case d.h.silent[machineUID]:
		return driver.GuestMemory{}, fmt.Errorf("the guest agent of machine %s does not answer", machineUID)
	case d.h.oldMemGuests[machineUID] || v.spec.FileShareServer != nil:
		return driver.GuestMemory{}, fmt.Errorf("%w: its memory", driver.ErrGuestCannot)
	}
	total := uint64(v.spec.MemoryMiB) << 20
	used, ok := d.h.guestMemUsed[machineUID]
	if !ok {
		used = total / 4
	}
	return driver.GuestMemory{Total: total, Available: total - min(used, total)}, nil
}

// RunCPU has the machine's running VM's unit use d more CPU time.
func (h *Host) RunCPU(machineUID string, d time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.vms[machineUID]
	if !ok || v.State != driver.VMRunning {
		return fmt.Errorf("no running VM for machine %s", machineUID)
	}
	v.cpu += d
	return nil
}

// HoldMemory has the machine's VM's unit hold held bytes of memory.
func (h *Host) HoldMemory(machineUID string, held uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.memHeld[machineUID] = held
}

// UseGuestMemory has the machine's guest use used bytes of its memory, or
// all of it if that is less.
func (h *Host) UseGuestMemory(machineUID string, used uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.guestMemUsed[machineUID] = used
}

// OldMemoryGuest makes the machine's guest one that cannot say its memory,
// as a hangar-init from before it could; false makes it able again.
func (h *Host) OldMemoryGuest(machineUID string, old bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.oldMemGuests[machineUID] = old
}
