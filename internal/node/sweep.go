package node

import (
	"context"
	"errors"
	"fmt"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/client"
)

// sweep removes what no machine on this node accounts for, and volumes no
// Disk bound to it does, then networks nothing uses, and the container
// images nothing uses past the image cache's bounds
// (docs/decisions/node-agent.md). A list it could not read stops whatever
// depends on it: a failed list is never an empty one.
// A VM is stopped off the host lock, and what its machine has left goes at
// the first sweep after its stop.
func (a *Agent) sweep(ctx context.Context) error {
	a.host.Lock()
	defer a.host.Unlock()
	objs, _, err := a.api.ListAll(ctx, registry.Machine, client.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing machines: %w", err)
	}
	mine := map[string]bool{}
	for _, o := range objs {
		m := o.(*computev1alpha1.Machine)
		if m.GetSpec().GetNode() == a.cfg.Name {
			mine[m.GetMetadata().GetUid()] = true
		}
	}
	pulling := a.puller.keep(mine)
	for uid := range a.netChecked {
		if !mine[uid] {
			delete(a.netChecked, uid)
		}
	}
	for uid := range a.guests.slow {
		if !mine[uid] {
			a.forgetGuest(uid)
		}
	}
	for uid := range a.stops.ended {
		if !mine[uid] {
			delete(a.stops.ended, uid)
		}
	}
	// A machine being stopped is passed by: its VM still uses its disk and
	// interfaces, and its stop must be the only one.
	gone := func(uid string) bool { return !mine[uid] && !a.stops.running[uid] }

	d := a.cfg.Drivers
	var errs []error
	keep := func(err error) bool {
		errs = append(errs, err)
		return err == nil
	}
	if vms, err := d.Hypervisor.VMs(ctx); keep(err) {
		listed := map[string]bool{}
		for _, vm := range vms {
			listed[vm.MachineUID] = true
			if gone(vm.MachineUID) && !a.backingOff(vm.MachineUID) {
				a.cfg.Log.Info("stopping a VM no machine accounts for", "machine_uid", vm.MachineUID)
				a.beginStop(ctx, vm.MachineUID)
			}
		}
		for uid := range a.stops.failed {
			if !mine[uid] && !listed[uid] {
				delete(a.stops.failed, uid)
			}
		}
	}
	// A failed stop leaves the VM listed, and DeleteLogs refuses it.
	if logs, err := d.Hypervisor.Logs(ctx); keep(err) {
		for _, uid := range logs {
			if gone(uid) {
				a.cfg.Log.Info("removing a console log no machine accounts for", "machine_uid", uid)
				keep(d.Hypervisor.DeleteLogs(ctx, uid))
			}
		}
	}
	if ifaces, err := d.Network.Interfaces(ctx); keep(err) {
		done := map[string]bool{}
		for _, i := range ifaces {
			if gone(i.MachineUID) && !done[i.MachineUID] {
				done[i.MachineUID] = true
				a.cfg.Log.Info("removing interfaces no machine accounts for", "machine_uid", i.MachineUID)
				keep(d.Network.DeleteInterfaces(ctx, i.MachineUID))
			}
		}
	}
	if roots, err := d.Storage.Roots(ctx); keep(err) {
		for _, r := range roots {
			if gone(r.MachineUID) {
				a.cfg.Log.Info("removing a root disk no machine accounts for", "machine_uid", r.MachineUID)
				keep(d.Storage.DeleteRoot(ctx, r.MachineUID))
			}
		}
	}

	// A volume is a Disk's, not a machine's: it goes once its Disk is gone.
	// The disk controller removes a deleted Disk's own. A disk image's base
	// goes once no volume is cloned from it, no disk still to be made here
	// is made from it, and no Image not being deleted names its digest: a
	// catalogued image stays ready for its next disk.
	disks, _, err := a.api.ListAll(ctx, registry.Disk, client.ListOptions{})
	images, _, ierr := a.api.ListAll(ctx, registry.Image, client.ListOptions{})
	if keep(err) {
		bound := map[string]*storagev1alpha1.Disk{}
		for _, o := range disks {
			if disk := o.(*storagev1alpha1.Disk); disk.GetStatus().GetNode() == a.cfg.Name {
				bound[disk.GetMetadata().GetUid()] = disk
			}
		}
		if vols, err := d.Storage.Volumes(ctx); keep(err) {
			used := map[string]bool{}
			for _, v := range vols {
				if bound[v.DiskUID] == nil {
					a.cfg.Log.Info("removing a volume no disk accounts for", "disk_uid", v.DiskUID)
					keep(d.Storage.DeleteVolume(ctx, v.DiskUID))
				}
			}
			// Read again: a volume that would not go still holds its base.
			if vols, err := d.Storage.Volumes(ctx); keep(err) && keep(ierr) {
				made := map[string]bool{}
				for _, v := range vols {
					made[v.DiskUID] = true
					used[v.Digest] = true
				}
				digests := map[string]string{}
				for _, o := range images {
					img := o.(*storagev1alpha1.Image)
					digest := "sha256:" + img.GetSpec().GetSha256()
					digests[img.GetMetadata().GetName()] = digest
					if img.GetMetadata().GetDeleteTime() == nil {
						used[digest] = true
					}
				}
				for uid, disk := range bound {
					if name := disk.GetSpec().GetSource().GetImage().GetName(); name != "" && !made[uid] && disk.GetMetadata().GetDeleteTime() == nil {
						used[digests[name]] = true
					}
				}
				for digest := range a.images.keep(used) {
					used[digest] = true
				}
				if bases, err := d.Storage.ImageBases(ctx); keep(err) {
					for _, b := range bases {
						if !used[b] && keep(d.Storage.DeleteImageBase(ctx, b)) {
							a.images.forget(b)
						}
					}
				}
			}
		}
	}

	// What is shared goes once nothing on the host uses it, judged from
	// lists read after the removals above.
	if ifaces, err := d.Network.Interfaces(ctx); keep(err) {
		used := map[string]bool{}
		for _, i := range ifaces {
			used[i.NetworkUID] = true
		}
		if nets, err := d.Network.Networks(ctx); keep(err) {
			onHost := map[string]bool{}
			for _, n := range nets {
				onHost[n.UID] = true
				if !used[n.UID] {
					// Its services go first. One reopened before the
					// network goes is closed by the keep below.
					a.netsvc.services.Close(n.UID)
					if keep(d.Network.DeleteNetwork(ctx, n.UID)) {
						delete(onHost, n.UID)
					}
				}
			}
			a.netsvc.keep(onHost)
		}
	}
	if roots, err := d.Storage.Roots(ctx); keep(err) {
		used := map[string]bool{}
		for _, r := range roots {
			used[r.Digest] = true
		}
		for digest := range pulling {
			used[digest] = true
		}
		_, err := a.evictImages(ctx, used, nil)
		keep(err)
	}
	return errors.Join(errs...)
}
