package apiserver

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
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// diskAsk is what a bound Disk d with spec.grow, its status as being
// written, is asked to grow to: the size the container mounting it
// read-write as a volume asks for in its machine's status.disks_wanted,
// with d's growth. asked is false when nothing is asked. The ask only starts
// a growth: the caller grants the next step from the size the disk has, so
// an ask repeated is granted once.
func (s *Server) diskAsk(ctx context.Context, d *storagev1alpha1.Disk, status *storagev1alpha1.DiskStatus) (wanted uint64, dg computeapi.DiskGrowth, asked bool, err error) {
	dg, ok := diskGrowthOf(d, status)
	if a := status.GetAttachment(); !ok || a.GetAs() != storagev1alpha1.DiskAttachmentRole_DISK_ATTACHMENT_ROLE_VOLUME || a.GetReadOnly() {
		return 0, dg, false, nil
	}
	obj, err := s.get(ctx, registry.Machine, resource.Key{Project: d.GetMetadata().GetProject(), Name: status.GetMachine()})
	if err != nil || obj == nil {
		return 0, dg, false, err
	}
	m := obj.(*computev1alpha1.Machine)
	// Only a machine on the disk's node asks for it: a machine never moves,
	// and the disk lives on one node.
	if m.GetSpec().GetNode() != status.GetNode() || !listsDisk(m.GetSpec(), d.GetMetadata().GetName()) {
		return 0, dg, false, nil
	}
	i := slices.IndexFunc(m.GetStatus().GetDisksWanted(), func(w *computev1alpha1.MachineDiskWanted) bool {
		return w.GetDisk() == d.GetMetadata().GetName()
	})
	if i < 0 {
		return 0, dg, false, nil
	}
	// An ask that does not parse is none: the node writes canonical sizes.
	wanted, perr := optionalBytes(m.GetStatus().GetDisksWanted()[i].GetSize())
	return wanted, dg, perr == nil, nil
}

// diskGrowthOf reads the growth of a Disk d that grows by itself, bound to
// a node and attached to a machine, with status as being written. ok is
// false for any other disk, and for a size or growth that does not parse,
// which admission refuses.
func diskGrowthOf(d *storagev1alpha1.Disk, status *storagev1alpha1.DiskStatus) (dg computeapi.DiskGrowth, ok bool) {
	if status.GetMachine() == "" || status.GetNode() == "" {
		return dg, false
	}
	size, err1 := diskBytes(d.GetSpec())
	dg, ok, err2 := computeapi.ParseDiskGrow(d.GetSpec().GetGrow(), size, "spec.grow")
	return dg, ok && errors.Join(err1, err2) == nil
}

// grantDiskStep grants a bound disk d, whose status as being written is
// status, the growth to target bytes, within the tenant's quota and the
// room on its node: status.size takes it, or DiskGrowBlocked says why not.
// stored is the size d has on its node as stored, which its node's room
// counts: from it, the growth counts a growth by hand in the same status
// too. The caller holds s.capacity and then s.quota until it writes status.
func (s *Server) grantDiskStep(ctx context.Context, d *storagev1alpha1.Disk, status *storagev1alpha1.DiskStatus, stored, target uint64) error {
	have, err := diskHeld(&storagev1alpha1.Disk{Metadata: d.GetMetadata(), Status: status})
	if err != nil {
		return err
	}
	setBlocked := func(reason, msg string) {
		status.Conditions = conditions.Set(status.GetConditions(), &metav1.Condition{
			Type: DiskGrowBlocked, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: reason, Message: msg,
		}, s.clock.Now())
	}
	grown := proto.Clone(d).(*storagev1alpha1.Disk)
	grown.Status = proto.Clone(status).(*storagev1alpha1.DiskStatus)
	grown.Status.Size = quantity.FormatDisk(target)
	err = s.checkFits(ctx, registry.Disk, grown)
	if ce := new(connect.Error); errors.As(err, &ce) && ce.Code() == connect.CodeResourceExhausted {
		setBlocked(ReasonDiskGrowQuotaExceeded, fmt.Sprintf("the disk stays %s: %s", quantity.FormatDisk(have), ce.Message()))
		return nil
	}
	if err != nil {
		return err
	}
	var stuck *blocked
	err = s.growthFits(ctx, "the disk", status.GetNode(), stored, target)
	if errors.As(err, &stuck) {
		setBlocked(stuck.reason, stuck.message)
		return nil
	}
	if err != nil {
		return err
	}
	status.Size = quantity.FormatDisk(target)
	status.Conditions = conditions.Set(status.GetConditions(), diskGrowNotBlocked(), s.clock.Now())
	return nil
}

// volumeGrowthOf is a copy of the VolumeGrowth condition of machine, in
// project; nil when it has none, or there is no such machine.
func (s *Server) volumeGrowthOf(ctx context.Context, project, machine string) (*metav1.Condition, error) {
	if machine == "" {
		return nil, nil
	}
	obj, err := s.get(ctx, registry.Machine, resource.Key{Project: project, Name: machine})
	if err != nil || obj == nil {
		return nil, err
	}
	c := conditions.Find(obj.(*computev1alpha1.Machine).GetStatus().GetConditions(), computeapi.VolumeGrowth)
	if c == nil {
		return nil, nil
	}
	return proto.Clone(c).(*metav1.Condition), nil
}

func diskGrowNotBlocked() *metav1.Condition {
	return &metav1.Condition{Type: DiskGrowBlocked, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reasonDiskGrowNotBlocked}
}

// disksWaitingOnQuota maps a Tenant or a Project, whose status the usage
// controller rewrites as what it holds or may hold changes, to the disks
// under it whose growth waits on quota.
func (s *Server) disksWaitingOnQuota(ctx context.Context, o store.Object) []controller.Key {
	var keys []controller.Key
	for _, project := range s.projectsOf(ctx, o) {
		objs, err := s.listWith(ctx, registry.Disk, project, nil)
		if err != nil {
			s.log.Error("listing disks failed", "error", err)
			return nil
		}
		for _, obj := range objs {
			d := obj.(*storagev1alpha1.Disk)
			if b := conditions.Find(d.GetStatus().GetConditions(), DiskGrowBlocked); b.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE && b.GetReason() == ReasonDiskGrowQuotaExceeded {
				keys = append(keys, controller.Key{Project: project, Name: d.GetMetadata().GetName()})
			}
		}
	}
	return keys
}
