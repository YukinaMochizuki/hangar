package fake

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// A container's guest sees its root disk at the size it had when the VM
// booted, or was last told (ResizeRoot), and its filesystem fills what it
// saw as it booted, or when it was last grown (GrowRoot): as Cloud
// Hypervisor and hangar-init have them.

// What RootCalls lists.
const (
	RootCallUsage  = "usage"
	RootCallResize = "resize"
	RootCallGrow   = "grow"
)

// bootRoot has a container's guest, booting, see its root disk as the host
// has it, and fill it.
func (h *Host) bootRoot(v *vm) {
	v.guestDisk = h.roots[v.MachineUID].Size
	v.guestFS = v.guestDisk
}

func (d hypervisor) ResizeRoot(_ context.Context, machineUID string) (int64, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	d.h.rootCalls[machineUID] = append(d.h.rootCalls[machineUID], RootCallResize)
	v, ok := d.h.vms[machineUID]
	if !ok || v.State != driver.VMRunning {
		return 0, fmt.Errorf("machine %s has no running VM", machineUID)
	}
	if v.spec.Firmware || v.spec.FileShareServer != nil || v.spec.Converter != nil {
		return 0, fmt.Errorf("machine %s's VM has no root disk", machineUID)
	}
	if d.h.noLiveResize[machineUID] {
		return 0, fmt.Errorf("machine %s: %w", machineUID, driver.ErrNoLiveResize)
	}
	v.guestDisk = d.h.roots[machineUID].Size
	return v.guestDisk, nil
}

func (d guest) RootUsage(_ context.Context, machineUID string) (driver.RootUsage, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	d.h.rootCalls[machineUID] = append(d.h.rootCalls[machineUID], RootCallUsage)
	v, err := d.h.rootGuestLocked(machineUID)
	if err != nil {
		return driver.RootUsage{}, err
	}
	if u, ok := d.h.rootClaims[machineUID]; ok {
		return u, nil
	}
	size := uint64(max(v.guestFS, 0))
	return driver.RootUsage{Size: size, Used: min(d.h.rootUsed[machineUID], size)}, nil
}

func (d guest) GrowRoot(ctx context.Context, machineUID string) error {
	d.h.mu.Lock()
	hold := d.h.rootHolds[machineUID]
	d.h.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	d.h.rootCalls[machineUID] = append(d.h.rootCalls[machineUID], RootCallGrow)
	v, err := d.h.rootGuestLocked(machineUID)
	if err != nil {
		return err
	}
	v.guestFS = max(v.guestFS, v.guestDisk)
	return nil
}

// rootGuestLocked is the machine's running container VM whose guest
// answers about its root. Callers hold h.mu.
func (h *Host) rootGuestLocked(machineUID string) (*vm, error) {
	v, ok := h.vms[machineUID]
	if !ok || v.State != driver.VMRunning {
		return nil, errUnknownVM
	}
	if h.silent[machineUID] {
		return nil, fmt.Errorf("the guest agent of machine %s does not answer", machineUID)
	}
	if h.oldRootGuests[machineUID] || h.oldRootGuests[""] {
		return nil, fmt.Errorf("%w: its root filesystem", driver.ErrGuestCannot)
	}
	if v.spec.Firmware || v.spec.FileShareServer != nil || v.spec.Converter != nil {
		return nil, fmt.Errorf("machine %s's guest has no root filesystem of its own", machineUID)
	}
	return v, nil
}

// FillRoot has the machine's guest use used bytes of its root filesystem,
// or all of it if that is less.
func (h *Host) FillRoot(machineUID string, used uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rootUsed[machineUID] = used
}

// ClaimRoot has the machine's guest answer RootUsage with u, whatever its
// filesystem holds, as a guest that lies does; nil has it tell the truth.
func (h *Host) ClaimRoot(machineUID string, u *driver.RootUsage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if u == nil {
		delete(h.rootClaims, machineUID)
		return
	}
	h.rootClaims[machineUID] = *u
}

// OldRootGuest makes the machine's guest one that can neither say how full
// its root filesystem is nor grow it while it runs, as a hangar-init from
// before it could; false makes it able again. The machine uid "" is every
// machine's.
func (h *Host) OldRootGuest(machineUID string, old bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.oldRootGuests[machineUID] = old
}

// NoLiveResize makes the machine's VM one that cannot be told its root
// disk's size while it runs, as one started before it could
// (driver.ErrNoLiveResize); false makes it able again.
func (h *Host) NoLiveResize(machineUID string, no bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.noLiveResize[machineUID] = no
}

// HoldRootGrowth keeps the machine's guest from answering GrowRoot until
// letGo, or the call's context ends.
func (h *Host) HoldRootGrowth(machineUID string) (letGo func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hold := make(chan struct{})
	h.rootHolds[machineUID] = hold
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.rootHolds, machineUID)
			h.mu.Unlock()
			close(hold)
		})
	}
}

// RootCalls lists, in order, the calls about the machine's root disk made
// of the hypervisor and the guest, failed ones too: RootCallUsage,
// RootCallResize and RootCallGrow.
func (h *Host) RootCalls(machineUID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.rootCalls[machineUID])
}
