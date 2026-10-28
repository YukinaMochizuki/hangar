package fake

import (
	"context"
	"fmt"
	"slices"

	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// A container's guest sees each of its volumes at the size it had when the
// VM booted, or was last told (ResizeVolume), and the filesystem of each it
// mounts read-write fills what it saw as it booted, or when it was last
// grown (GrowVolume), as its root's does.

// What RootCalls lists of volumes, each followed by a space and the
// volume's index in the VM's Disks.
const (
	RootCallVolumeUsage  = "volume-usage"
	RootCallResizeVolume = "resize-volume"
	RootCallGrowVolume   = "grow-volume"
)

// bootVolumes has a container's guest, booting, see its volumes as the host
// has them, and fill them.
func (h *Host) bootVolumes(v *vm) {
	v.guestVols = make([]int64, len(v.spec.Disks))
	for n, d := range v.spec.Disks {
		v.guestVols[n] = h.volumes[d.DiskUID].Size
	}
	v.guestVolFS = slices.Clone(v.guestVols)
}

func (d hypervisor) ResizeVolume(_ context.Context, machineUID string, n int) (int64, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	d.h.rootCalls[machineUID] = append(d.h.rootCalls[machineUID], fmt.Sprintf("%s %d", RootCallResizeVolume, n))
	v, ok := d.h.vms[machineUID]
	if !ok || v.State != driver.VMRunning {
		return 0, fmt.Errorf("machine %s has no running VM", machineUID)
	}
	if v.spec.Firmware || v.spec.FileShareServer != nil || v.spec.Converter != nil {
		return 0, fmt.Errorf("machine %s's VM is no container's", machineUID)
	}
	if n < 0 || n >= len(v.spec.Disks) {
		return 0, fmt.Errorf("machine %s's VM has no volume %d", machineUID, n)
	}
	if d.h.noLiveResize[machineUID] {
		return 0, fmt.Errorf("machine %s: %w", machineUID, driver.ErrNoLiveResize)
	}
	v.guestVols[n] = d.h.volumes[v.spec.Disks[n].DiskUID].Size
	return v.guestVols[n], nil
}

func (d guest) VolumeUsage(_ context.Context, machineUID, device string) (driver.FSUsage, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	v, n, err := d.h.volumeGuestLocked(machineUID, device, RootCallVolumeUsage)
	if err != nil {
		return driver.FSUsage{}, err
	}
	diskUID := v.spec.Disks[n].DiskUID
	if u, ok := d.h.volumeClaims[diskUID]; ok {
		return u, nil
	}
	size := uint64(max(v.guestVolFS[n], 0))
	return driver.FSUsage{Size: size, Used: min(d.h.volumeUsed[diskUID], size)}, nil
}

func (d guest) GrowVolume(ctx context.Context, machineUID, device string) error {
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
	v, n, err := d.h.volumeGuestLocked(machineUID, device, RootCallGrowVolume)
	if err != nil {
		return err
	}
	v.guestVolFS[n] = max(v.guestVolFS[n], v.guestVols[n])
	return nil
}

// volumeGuestLocked is the machine's running container VM whose guest
// answers about the volume on device, and that volume's index in its Disks,
// the call recorded in RootCalls as call. A guest answers only of a volume
// it mounts read-write, as hangar-init does. Callers hold h.mu.
func (h *Host) volumeGuestLocked(machineUID, device, call string) (*vm, int, error) {
	v, ok := h.vms[machineUID]
	n := -1
	if ok {
		n = slices.IndexFunc(v.spec.Mounts, func(m driver.Mount) bool {
			return !m.Share.IsValid() && !m.ReadOnly && "/dev/"+computeapi.VirtioDevice(computeapi.FirstContainerDisk+m.Disk) == device
		})
		if n >= 0 {
			n = v.spec.Mounts[n].Disk
		}
	}
	h.rootCalls[machineUID] = append(h.rootCalls[machineUID], fmt.Sprintf("%s %d", call, n))
	if !ok || v.State != driver.VMRunning {
		return nil, 0, errUnknownVM
	}
	if h.silent[machineUID] {
		return nil, 0, fmt.Errorf("the guest agent of machine %s does not answer", machineUID)
	}
	if h.oldVolumeGuests[machineUID] || h.oldVolumeGuests[""] {
		return nil, 0, fmt.Errorf("%w: its volumes' filesystems", driver.ErrGuestCannot)
	}
	if n < 0 || n >= len(v.guestVols) {
		return nil, 0, fmt.Errorf("machine %s's guest mounts no volume read-write from %s", machineUID, device)
	}
	return v, n, nil
}

// FillVolume has the guests mounting the Disk of diskUID use used bytes of
// its filesystem, or all of it if that is less.
func (h *Host) FillVolume(diskUID string, used uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.volumeUsed[diskUID] = used
}

// ClaimVolume has the guests mounting the Disk of diskUID answer
// VolumeUsage with u, as a guest that lies does; nil has them tell the
// truth.
func (h *Host) ClaimVolume(diskUID string, u *driver.FSUsage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if u == nil {
		delete(h.volumeClaims, diskUID)
		return
	}
	h.volumeClaims[diskUID] = *u
}

// OldVolumeGuest makes the machine's guest one that can neither say how
// full its volumes are nor grow them while it runs, as a hangar-init from
// before it could; false makes it able again. The machine uid "" is every
// machine's.
func (h *Host) OldVolumeGuest(machineUID string, old bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.oldVolumeGuests[machineUID] = old
}

// diskSizes are the sizes of vols, in their order.
func diskSizes(vols []driver.Volume) []int64 {
	var out []int64
	for _, v := range vols {
		out = append(out, v.Size)
	}
	return out
}
