package node

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// grownFS is the sizes of a VM's disks its guest grew its filesystems to in
// one boot.
type grownFS struct {
	boot  time.Time
	sizes []int64
}

// growTimeout bounds a guest's growing its filesystems, under the host
// lock: an online ext4 resize adds the new block groups, seconds even for
// hundreds of GiB.
const growTimeout = 30 * time.Second

// ensureGrown grows a running VM's disks to spec's sizes, which the storage
// has grown their devices to, without restarting it (docs/decisions/
// storage-zfs.md), and then has its guest grow its filesystems to them,
// unless the guest boots its own kernel, whose filesystems are its own. It
// returns running, or, when a disk did not grow whole, says so on the
// machine, which runs on: it is never restarted to grow a disk. A failure
// is tried again; a VM that cannot grow a disk until it starts again is
// not.
func (a *Agent) ensureGrown(ctx context.Context, spec driver.VMSpec, vm *driver.VM, running *observation) (*observation, error) {
	uid := spec.MachineUID
	failed := func(msg string) *observation {
		obs := notReady(computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonDiskGrowFailed, msg)
		obs.started = running.started
		return obs
	}
	grown, err := a.cfg.Drivers.Hypervisor.GrowDisks(ctx, spec)
	switch {
	case errors.Is(err, driver.ErrGrowsAtStart):
		return failed("a disk grew, and the running VM cannot see it: " + err.Error()), nil
	case err != nil:
		a.cfg.Log.Warn("a disk of a running machine did not grow in its VM", "machine", spec.Machine, "machine_uid", uid, "error", err)
		return failed("a disk grew, and the running VM did not take its new size: " + err.Error()),
			fmt.Errorf("machine %s's disks: %w", spec.Machine, err)
	}
	if spec.Firmware || (!grown && !vm.Grown) {
		return running, nil
	}
	sizes := vm.DiskSizes
	if grown {
		sizes = spec.DiskSizes()
	}
	if g, ok := a.grownFS[uid]; ok && g.boot.Equal(vm.Started) && slices.Equal(g.sizes, sizes) {
		return running, nil
	}
	gctx, cancel := context.WithTimeout(ctx, growTimeout)
	err = a.cfg.Drivers.Guest.GrowFilesystems(gctx, uid)
	cancel()
	if err != nil {
		delete(a.grownFS, uid)
		a.cfg.Log.Warn("a running machine's guest did not grow its filesystems to its grown disks", "machine", spec.Machine, "machine_uid", uid, "error", err)
		obs := failed("a disk grew, and the guest did not grow its filesystem to it: " + err.Error())
		if errors.Is(err, driver.ErrGuestCannot) {
			// Started before it could: it grows them as it next boots.
			return obs, nil
		}
		return obs, fmt.Errorf("%w: machine %s's filesystems: %w", controller.ErrWaiting, spec.Machine, err)
	}
	a.grownFS[uid] = grownFS{boot: vm.Started, sizes: sizes}
	return running, nil
}
