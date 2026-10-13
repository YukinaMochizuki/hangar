package node

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// diskController keeps the volumes of the Disks bound to this node: each at
// the size its status says, and gone once its deletion no longer waits for
// a machine.
func (a *Agent) diskController() *controller.Controller {
	return &controller.Controller{
		Name:       "disks",
		Source:     controller.APISource{Client: a.api},
		Reconciler: controller.ReconcilerFunc(a.reconcileDisk),
		Watches:    []controller.Watch{{Kind: registry.Disk.Kind, Map: a.diskIfMine}},
		Clock:      a.cfg.Clock,
		Log:        a.cfg.Log,
	}
}

func (a *Agent) diskIfMine(_ context.Context, o store.Object) []controller.Key {
	var d storagev1alpha1.Disk
	if proto.Unmarshal(o.Data, &d) != nil || d.GetStatus().GetNode() != a.cfg.Name {
		return nil
	}
	return []controller.Key{{Project: o.Project, Name: o.Name}}
}

// reconcileDisk brings the host in line with one disk bound to this node.
func (a *Agent) reconcileDisk(ctx context.Context, k controller.Key) error {
	a.host.Lock()
	defer a.host.Unlock()
	d, err := a.getDisk(ctx, k.Project, k.Name)
	if err != nil || d == nil || d.GetStatus().GetNode() != a.cfg.Name {
		return err
	}
	uid := d.GetMetadata().GetUid()
	if d.GetMetadata().GetDeleteTime() != nil {
		if conditions.IsTrue(d.GetStatus().GetConditions(), computeapi.DiskReleased) ||
			slices.Contains(d.GetMetadata().GetFinalizers(), computeapi.DiskInUseFinalizer) {
			return nil
		}
		if err := a.cfg.Drivers.Storage.DeleteVolume(ctx, uid); err != nil {
			return err
		}
		return a.writeDiskStatus(ctx, d, &metav1.Condition{
			Type: computeapi.DiskReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned",
			Message: "the disk's volume is gone from the host",
		})
	}
	_, err = a.ensureVolume(ctx, d)
	ready := &metav1.Condition{Type: computeapi.DiskReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Created"}
	if err != nil {
		reason := computeapi.ReasonDiskFailed
		if errors.Is(err, driver.ErrNoSpace) {
			reason = "NoSpace"
		}
		ready = &metav1.Condition{Type: computeapi.DiskReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reason, Message: err.Error()}
	}
	if werr := a.writeDiskStatus(ctx, d, ready); werr != nil {
		return werr
	}
	return err
}

// ensureVolume makes d's volume at the size its status says, which binding
// sets.
func (a *Agent) ensureVolume(ctx context.Context, d *storagev1alpha1.Disk) (driver.Volume, error) {
	size, err := quantity.Bytes(d.GetStatus().GetSize())
	if err != nil {
		return driver.Volume{}, fmt.Errorf("disk %s: status.size: %w", d.GetMetadata().GetName(), err)
	}
	if size > 1<<62 {
		return driver.Volume{}, fmt.Errorf("disk %s: status.size %s is too large", d.GetMetadata().GetName(), d.GetStatus().GetSize())
	}
	return a.cfg.Drivers.Storage.EnsureVolume(ctx, d.GetMetadata().GetUid(), int64(size))
}

// writeDiskStatus sets the node's conditions on d as read, keeping what the
// apiserver owns, and writes nothing if nothing changed. The write is
// conditional on d's resource version.
func (a *Agent) writeDiskStatus(ctx context.Context, d *storagev1alpha1.Disk, conds ...*metav1.Condition) error {
	status := &storagev1alpha1.DiskStatus{}
	if d.GetStatus() != nil {
		status = proto.Clone(d.GetStatus()).(*storagev1alpha1.DiskStatus)
	}
	for _, c := range conds {
		c.ObservedGeneration = d.GetMetadata().GetGeneration()
		status.Conditions = conditions.Set(status.GetConditions(), c, a.cfg.Clock.Now())
	}
	if proto.Equal(status, d.GetStatus()) {
		return nil
	}
	rv := d.GetMetadata().GetResourceVersion()
	_, err := a.disks.UpdateDiskStatus(ctx, connect.NewRequest(&storagev1alpha1.UpdateDiskStatusRequest{
		Project: d.GetMetadata().GetProject(), Name: d.GetMetadata().GetName(), Status: status, ExpectedResourceVersion: &rv,
	}))
	return err
}

// volumes makes the volumes of the disks m lists, in its order. A disk not
// yet bound to this node holds the machine up.
func (a *Agent) volumes(ctx context.Context, m *computev1alpha1.Machine) ([]driver.Volume, error) {
	var out []driver.Volume
	for _, md := range m.GetSpec().GetDisks() {
		d, err := a.getDisk(ctx, m.GetMetadata().GetProject(), md.GetDisk())
		if err != nil {
			return nil, err
		}
		if d == nil {
			return nil, blocked(computeapi.ReasonDiskNotReady, fmt.Errorf("disk %s does not exist", md.GetDisk()))
		}
		if d.GetStatus().GetNode() != a.cfg.Name {
			return nil, blocked(computeapi.ReasonDiskNotReady, fmt.Errorf("disk %s is not bound to this node", md.GetDisk()))
		}
		v, err := a.ensureVolume(ctx, d)
		if err != nil {
			return nil, blocked(computeapi.ReasonDiskFailed, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// machinesWithDisk maps a disk to the machines on this node that list it: a
// disk bound or grown unblocks or changes them.
func (a *Agent) machinesWithDisk(ctx context.Context, o store.Object) []controller.Key {
	objs, _, err := a.api.ListAll(ctx, registry.Machine, client.ListOptions{Project: o.Project})
	if err != nil {
		a.cfg.Log.Error("listing machines failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, obj := range objs {
		m := obj.(*computev1alpha1.Machine)
		if m.GetSpec().GetNode() != a.cfg.Name {
			continue
		}
		if slices.ContainsFunc(m.GetSpec().GetDisks(), func(d *computev1alpha1.MachineDisk) bool { return d.GetDisk() == o.Name }) {
			keys = append(keys, controller.Key{Project: o.Project, Name: m.GetMetadata().GetName()})
		}
	}
	return keys
}

func (a *Agent) getDisk(ctx context.Context, project, name string) (*storagev1alpha1.Disk, error) {
	res, err := a.disks.GetDisk(ctx, connect.NewRequest(&storagev1alpha1.GetDiskRequest{Project: project, Name: name}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return res.Msg.GetDisk(), nil
}
