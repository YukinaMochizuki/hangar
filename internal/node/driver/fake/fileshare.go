package fake

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"sync"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

func (d storage) EnsureShareVolume(_ context.Context, shareUID string, size int64) (driver.ShareVolume, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if v, ok := d.h.shareVols[shareUID]; ok {
		if v.Size < size {
			v.Size = size
			d.h.shareVols[shareUID] = v
		}
		return v, nil
	}
	if d.h.cloneErr != nil {
		return driver.ShareVolume{}, d.h.cloneErr
	}
	if d.h.full {
		return driver.ShareVolume{}, driver.ErrNoSpace
	}
	v := driver.ShareVolume{ShareUID: shareUID, Path: "/dev/fake/shares/" + shareUID, Size: max(size, 1)}
	d.h.shareVols[shareUID] = v
	return v, nil
}

func (d storage) ShareVolumes(context.Context) ([]driver.ShareVolume, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if err := d.h.listErr[ListShares]; err != nil {
		return nil, err
	}
	return slices.Collect(maps.Values(d.h.shareVols)), nil
}

func (d storage) DeleteShareVolume(_ context.Context, shareUID string) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.removalErr != nil {
		return d.h.removalErr
	}
	if d.h.heldShares[shareUID] > 0 {
		return fmt.Errorf("the volume of file share %s is in use", shareUID)
	}
	for _, v := range d.h.vms {
		if v.State == driver.VMRunning && v.spec.FileShareServer != nil && v.spec.FileShareServer.Volume.ShareUID == shareUID {
			return fmt.Errorf("the volume of file share %s is in use by machine %s", shareUID, v.MachineUID)
		}
	}
	delete(d.h.shareVols, shareUID)
	return nil
}

// HoldShareVolume keeps a FileShare's volume in use, as a running server
// does, until the returned function lets it go.
func (h *Host) HoldShareVolume(shareUID string) (release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.heldShares[shareUID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.heldShares[shareUID]--
		})
	}
}

// LeaveShareVolume puts a FileShare's volume on the host, as a node that
// served it leaves it.
func (h *Host) LeaveShareVolume(shareUID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.shareVols[shareUID] = driver.ShareVolume{ShareUID: shareUID, Path: "/dev/fake/shares/" + shareUID, Size: 1 << 30}
}

func (d guest) SetExport(_ context.Context, machineUID string, export driver.ShareExport) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	d.h.exportCalls[machineUID]++
	d.h.exportLog[machineUID] = append(d.h.exportLog[machineUID], "tell")
	v, ok := d.h.vms[machineUID]
	if !ok || v.State != driver.VMRunning {
		return errUnknownVM
	}
	if d.h.silent[machineUID] {
		return fmt.Errorf("the guest agent of machine %s does not answer", machineUID)
	}
	if v.spec.FileShareServer == nil {
		return fmt.Errorf("%w: machine %s is no file share's server", driver.ErrGuestCannot, machineUID)
	}
	if err := d.h.exportErr[machineUID]; err != nil {
		return err
	}
	if err := d.h.exportErr[""]; err != nil {
		return err
	}
	if err := checkExport(export); err != nil {
		return err
	}
	d.h.exports[machineUID] = cloneExport(export)
	return nil
}

// checkExport refuses what the server's hangar-init refuses
// (internal/guest/fileshare): no squash said, an anonymous id of
// (uid_t)-1, or a client that is not one host's address (unspecified,
// loopback, multicast, the broadcast, a zone), or is listed twice.
func checkExport(e driver.ShareExport) error {
	if e.Squash < driver.SquashNone || e.Squash > driver.SquashAll {
		return fmt.Errorf("the export's squash %d is none the server knows", e.Squash)
	}
	if e.AnonUID == 1<<32-1 || e.AnonGID == 1<<32-1 {
		return fmt.Errorf("the export's anonymous ids %d:%d", e.AnonUID, e.AnonGID)
	}
	seen := map[netip.Addr]bool{}
	for _, c := range e.Clients {
		a := c.Address
		if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() || a == netip.AddrFrom4([4]byte{255, 255, 255, 255}) || a.Zone() != "" || seen[a] {
			return fmt.Errorf("the export's client %s", a)
		}
		seen[a] = true
	}
	return nil
}

func cloneExport(e driver.ShareExport) driver.ShareExport {
	e.Clients = slices.Clone(e.Clients)
	return e
}

// SetBootExport rewrites a listed server's boot disk, which each boot by
// its restart policy takes its export from (bootGuest).
func (d hypervisor) SetBootExport(_ context.Context, machineUID string, export driver.ShareExport) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	d.h.exportLog[machineUID] = append(d.h.exportLog[machineUID], "boot")
	if err := d.h.bootExportErr; err != nil {
		return err
	}
	v, ok := d.h.vms[machineUID]
	if !ok {
		return errUnknownVM
	}
	if v.spec.FileShareServer == nil {
		return fmt.Errorf("machine %s's VM is no file share's server", machineUID)
	}
	srv := *v.spec.FileShareServer
	srv.Export = cloneExport(export)
	v.spec.FileShareServer = &srv
	return nil
}

// BootExport is the export the boot disk of the machine's server's VM
// holds; false for no server listed.
func (h *Host) BootExport(machineUID string) (driver.ShareExport, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.vms[machineUID]
	if !ok || v.spec.FileShareServer == nil {
		return driver.ShareExport{}, false
	}
	return cloneExport(v.spec.FileShareServer.Export), true
}

// ExportLog is what the machine's server's export was asked, in order:
// "boot" for each SetBootExport, "tell" for each SetExport.
func (h *Host) ExportLog(machineUID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.exportLog[machineUID])
}

// FailBootExport makes SetBootExport fail with err; nil clears it.
func (h *Host) FailBootExport(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bootExportErr = err
}

// FailExport makes the machine's server refuse SetExport with err, or
// every server's for ""; nil clears it.
func (h *Host) FailExport(machineUID string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.exportErr[machineUID] = err
}

// Export is what the machine's file share server holds: its boot disk's
// export, or what it was told since; false for no server that ever booted.
func (h *Host) Export(machineUID string) (driver.ShareExport, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.exports[machineUID]
	return cloneExport(e), ok
}

// ExportCalls counts SetExport asked of the machine's guest, failed calls
// included.
func (h *Host) ExportCalls(machineUID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exportCalls[machineUID]
}
