package node

import (
	"context"
	"errors"
	"fmt"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/client"
)

// sweep removes what no machine on this node accounts for, then networks
// and bases nothing uses (docs/decisions/node-agent.md). A list it could not
// read stops whatever depends on it: a failed list is never an empty one.
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
		if bases, err := d.Storage.Bases(ctx); keep(err) {
			for _, b := range bases {
				if !used[b] && keep(d.Storage.DeleteBase(ctx, b)) {
					a.puller.forget(b)
					keep(d.Images.Forget(ctx, b))
				}
			}
		}
	}
	return errors.Join(errs...)
}
