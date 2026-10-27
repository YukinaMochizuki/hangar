package fake

import (
	"context"
	"errors"
	"fmt"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// growth is what GrowDisks and GrowFilesystems did, and are made to do, by
// machine uid.
type growth struct {
	// disks counts the disks GrowDisks grew, and filesystems the
	// GrowFilesystems calls a guest answered.
	disks, filesystems map[string]int
	// diskErr fails GrowDisks, and fsErr GrowFilesystems.
	diskErr, fsErr map[string]error
	// fixed are VMs started before their disks could grow while they ran.
	fixed map[string]bool
}

func (g *growth) init() {
	if g.disks == nil {
		g.disks, g.filesystems = map[string]int{}, map[string]int{}
		g.diskErr, g.fsErr = map[string]error{}, map[string]error{}
		g.fixed = map[string]bool{}
	}
}

// FailGrowDisks makes GrowDisks fail with err for the machine, as Cloud
// Hypervisor refusing a new size; nil clears it.
func (h *Host) FailGrowDisks(machineUID string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.growth.init()
	h.growth.diskErr[machineUID] = err
}

// FailGrowFilesystems makes the machine's guest fail GrowFilesystems with
// err; nil clears it.
func (h *Host) FailGrowFilesystems(machineUID string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.growth.init()
	h.growth.fsErr[machineUID] = err
}

// FixDisks makes the machine's running VM one started before its disks
// could grow while it ran, as an older node started it: GrowDisks fails
// with ErrGrowsAtStart until it is started again.
func (h *Host) FixDisks(machineUID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.growth.init()
	h.growth.fixed[machineUID] = true
}

// GrownDisks counts the disks GrowDisks grew in the machine's VMs, and
// GrownFilesystems the GrowFilesystems calls its guests answered.
func (h *Host) GrownDisks(machineUID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.growth.disks[machineUID]
}

func (h *Host) GrownFilesystems(machineUID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.growth.filesystems[machineUID]
}

// GrowDisks grows the running VM's disks to spec's sizes, refusing, as
// Cloud Hypervisor does, a size its device does not have.
func (d hypervisor) GrowDisks(_ context.Context, spec driver.VMSpec) (bool, error) {
	h := d.h
	h.mu.Lock()
	defer h.mu.Unlock()
	h.growth.init()
	uid := spec.MachineUID
	v, ok := h.vms[uid]
	if !ok {
		return false, fmt.Errorf("machine %s has no VM: %w", uid, driver.ErrGrowsAtStart)
	}
	if !spec.Runs(v.VM) {
		return false, fmt.Errorf("machine %s's VM runs another spec", uid)
	}
	have, want := v.spec.DiskSizes(), spec.DiskSizes()
	var grow []int
	for i := range want {
		if want[i] > have[i] {
			grow = append(grow, i)
		}
	}
	switch {
	case len(grow) == 0:
		return false, nil
	case v.State != driver.VMRunning:
		return false, fmt.Errorf("machine %s's VM is not running: %w", uid, driver.ErrGrowsAtStart)
	case h.growth.fixed[uid]:
		return false, fmt.Errorf("machine %s's VM was started before its disks could grow while it ran: %w", uid, driver.ErrGrowsAtStart)
	case h.growth.diskErr[uid] != nil:
		return false, h.growth.diskErr[uid]
	}
	// Each disk on its own, as Cloud Hypervisor is asked for each.
	var errs []error
	for _, i := range grow {
		if dev := h.deviceSizeLocked(spec, i); dev != want[i] {
			errs = append(errs, fmt.Errorf("machine %s's disk %d: the device has %d bytes, not %d", uid, i, dev, want[i]))
			continue
		}
		have[i] = want[i]
		h.growth.disks[uid]++
	}
	grown := len(errs) < len(grow)
	if grown {
		v.spec, _ = v.spec.WithDiskSizes(have)
		v.DiskSizes, v.Grown = have, true
	}
	return grown, errors.Join(errs...)
}

// deviceSizeLocked is the size the storage has the spec's i-th disk at.
func (h *Host) deviceSizeLocked(spec driver.VMSpec, i int) int64 {
	switch {
	case i == 0 && spec.FileShareServer != nil:
		return h.shareVols[spec.FileShareServer.Volume.ShareUID].Size
	case i == 0:
		return h.roots[spec.MachineUID].Size
	}
	return h.volumes[spec.Disks[i-1].DiskUID].Size
}

func (d guest) GrowFilesystems(_ context.Context, machineUID string) error {
	h := d.h
	h.mu.Lock()
	defer h.mu.Unlock()
	h.growth.init()
	if v, ok := h.vms[machineUID]; !ok || v.State != driver.VMRunning {
		return errUnknownVM
	}
	if h.silent[machineUID] {
		return fmt.Errorf("the guest agent of machine %s does not answer", machineUID)
	}
	if h.oldGuests[machineUID] {
		return fmt.Errorf("machine %s's guest: %w", machineUID, driver.ErrGuestCannot)
	}
	if err := h.growth.fsErr[machineUID]; err != nil {
		return err
	}
	h.growth.filesystems[machineUID]++
	return nil
}
