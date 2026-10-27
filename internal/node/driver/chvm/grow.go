package chvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// GrowDisks grows a running VM's disks to spec's sizes through Cloud
// Hypervisor's vm.resize-disk, which, for a block device, checks the device
// is that size already, the storage having grown it, and tells the guest
// its disk's new capacity. The spec file then holds the new sizes, and says
// the VM grew; a disk that failed to grow keeps its size there, and is
// grown on the next call.
func (h *Hypervisor) GrowDisks(ctx context.Context, spec driver.VMSpec) (bool, error) {
	uid := spec.MachineUID
	if err := checkUID(uid); err != nil {
		return false, err
	}
	unlock := h.locks.Lock(uid)
	defer unlock()
	unit, err := h.unit(ctx, uid)
	if err != nil {
		return false, err
	}
	vm, listed, err := h.vm(uid, unit)
	if err != nil {
		return false, err
	}
	if !listed {
		return false, fmt.Errorf("machine %s has no VM: %w", uid, driver.ErrGrowsAtStart)
	}
	if !spec.Runs(vm) {
		return false, fmt.Errorf("machine %s's VM runs another spec", uid)
	}
	dir := h.dir(uid)
	ln, err := readLaunch(dir)
	if err != nil {
		return false, err
	}
	have, want := ln.Spec.DiskSizes(), spec.DiskSizes()
	if len(have) != len(want) {
		return false, fmt.Errorf("machine %s's VM has %d disks, not %d", uid, len(have), len(want))
	}
	var grow []int
	for i := range want {
		if want[i] > have[i] {
			grow = append(grow, i)
		}
	}
	switch {
	case len(grow) == 0:
		return false, nil
	case vm.State != driver.VMRunning:
		return false, fmt.Errorf("machine %s's VM is not running: %w", uid, driver.ErrGrowsAtStart)
	case !ln.Growable:
		return false, fmt.Errorf("machine %s's VM was started before its disks could grow while it ran: %w", uid, driver.ErrGrowsAtStart)
	}
	var errs []error
	for _, i := range grow {
		id, ok := diskID(ln.Spec, i)
		if !ok {
			errs = append(errs, fmt.Errorf("machine %s's VM has no disk %d to grow", uid, i))
			continue
		}
		if err := h.resizeDisk(ctx, filepath.Join(dir, apiFile), id, want[i]); err != nil {
			errs = append(errs, fmt.Errorf("growing disk %s to %d bytes: %w", id, want[i], err))
			continue
		}
		have[i] = want[i]
	}
	grown := len(errs) < len(grow)
	if grown {
		// Its sizes are what its guest sees, before anything else fails.
		ln.Spec, _ = ln.Spec.WithDiskSizes(have)
		ln.Grown = true
		b, err := json.Marshal(ln)
		if err != nil {
			return false, err
		}
		if err := writeAtomic(filepath.Join(dir, specFile), b); err != nil {
			return false, errors.Join(append(errs, err)...)
		}
	}
	return grown, errors.Join(errs...)
}

// diskID is Cloud Hypervisor's id of a VM's disk, the i-th as
// VMSpec.DiskSizes lists them: its root disk, or its share's volume, then
// its volumes; false for one it does not have.
func diskID(s driver.VMSpec, i int) (string, bool) {
	switch {
	case i == 0 && s.FileShareServer != nil:
		return volumeNode(0), true
	case i == 0 && s.Root.Path != "":
		return rootDiskID, true
	case i > 0 && i <= len(s.Disks):
		return volumeNode(i - 1), true
	}
	return "", false
}

// resizeDisk tells the VM whose Cloud Hypervisor serves its API at socket
// that its disk id is now size bytes, its device's size.
func resizeDisk(ctx context.Context, socket, id string, size int64) error {
	body, err := json.Marshal(struct {
		ID          string `json:"id"`
		DesiredSize int64  `json:"desired_size"`
	}{id, size})
	if err != nil {
		return err
	}
	return callCH(ctx, socket, "vm.resize-disk", body)
}
