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
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// ContainerDiskGrowBlocked is the condition a container with
// resources.disk_grow carries: True while its node asks for its root disk
// to grow and the growth does not fit the tenant's quota or the node
// (docs/decisions/disk-autogrow.md).
const ContainerDiskGrowBlocked = "DiskGrowBlocked"

// Reasons of ContainerDiskGrowBlocked.
const (
	ReasonDiskGrowQuotaExceeded = "QuotaExceeded"
	reasonDiskGrowNotBlocked    = "NotBlocked"
)

// grownDisk is the disk, in whole Gi, a container's root disk has grown to
// by itself; 0 if it has not, or its status does not parse.
func grownDisk(st *computev1alpha1.ContainerStatus) uint64 {
	b, err := optionalBytes(st.GetDisk())
	if err != nil {
		return 0
	}
	return wholeGi(b)
}

// growContainerDisk grants c's root disk the growth its machine m asks for,
// within the tenant's quota and the room on m's node, by recording the new
// size in c's status; the machine follows from there (machineSpec). It
// returns c as stored after. A growth that does not fit is reported on c
// and tried again when what blocks it changes: the tenant's or project's
// usage or limits (containersWaitingOnQuota), or the node's room, as for a
// resize in the spec.
func (s *Server) growContainerDisk(ctx context.Context, c *computev1alpha1.Container, m *computev1alpha1.Machine) (*computev1alpha1.Container, error) {
	res := c.GetSpec().GetResources()
	sz, err := containerSizes(res)
	if err != nil {
		return c, nil
	}
	dg, ok, err := computeapi.ParseDiskGrow(res.GetDiskGrow(), sz.disk, "spec.resources.disk_grow")
	status := proto.Clone(c.GetStatus()).(*computev1alpha1.ContainerStatus)
	if status == nil {
		status = &computev1alpha1.ContainerStatus{}
	}
	if err != nil || !ok {
		// Growth turned off keeps what grew, and no longer reports on it.
		status.Conditions = slices.DeleteFunc(slices.Clone(status.GetConditions()), func(cond *metav1.Condition) bool {
			return cond.GetType() == ContainerDiskGrowBlocked
		})
		return s.writeContainerStatus(ctx, c, status)
	}
	node := m.GetSpec().GetNode()
	have, err := heldBy(m.GetSpec().GetResources())
	if err != nil || node == "" {
		return c, nil
	}
	notBlocked := &metav1.Condition{Type: ContainerDiskGrowBlocked, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reasonDiskGrowNotBlocked}
	wanted, err := optionalBytes(m.GetStatus().GetDiskWanted())
	// The node's ask is only what starts a growth; its size is the spec's
	// next step from the disk the machine has, so a growth the node asked
	// for twice is granted once.
	target := dg.Next(have.disk)
	if grownDisk(status) > wholeGi(have.disk) {
		// Granted, and not yet on the machine: ensureMachine grows it, or
		// says on Ready why the node has no room for it after all, the room
		// checked here having been taken before the machine was written.
		return c, nil
	}
	if err != nil || wholeGi(wanted) <= wholeGi(have.disk) || wholeGi(target) <= wholeGi(have.disk) {
		status.Conditions = conditions.Set(status.GetConditions(), notBlocked, s.clock.Now())
		return s.writeContainerStatus(ctx, c, status)
	}

	grown := proto.Clone(c).(*computev1alpha1.Container)
	grown.Status = proto.Clone(status).(*computev1alpha1.ContainerStatus)
	grown.Status.Disk = quantity.FormatDisk(target)
	// Capacity before quota: placing a machine holds capacity while its
	// apply takes attach, and a tenant's apply holds attach while it takes
	// quota. The status written under both takes neither.
	s.capacity.Lock()
	defer s.capacity.Unlock()
	s.quota.Lock()
	defer s.quota.Unlock()
	blockedBy := func(reason, msg string) (*computev1alpha1.Container, error) {
		status.Conditions = conditions.Set(status.GetConditions(), &metav1.Condition{
			Type: ContainerDiskGrowBlocked, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: reason, Message: msg,
		}, s.clock.Now())
		return s.writeContainerStatus(ctx, c, status)
	}
	err = s.checkFits(ctx, registry.Container, grown)
	if ce := new(connect.Error); errors.As(err, &ce) && ce.Code() == connect.CodeResourceExhausted {
		return blockedBy(ReasonDiskGrowQuotaExceeded, fmt.Sprintf("the root disk stays %s: %s", quantity.FormatDisk(have.disk), ce.Message()))
	}
	if err != nil {
		return c, err
	}
	var stuck *blocked
	err = s.growthFits(ctx, "the root disk", node, have.disk, target)
	if errors.As(err, &stuck) {
		return blockedBy(stuck.reason, stuck.message)
	}
	if err != nil {
		return c, err
	}
	grown.Status.Conditions = conditions.Set(grown.Status.GetConditions(), notBlocked, s.clock.Now())
	return s.writeContainerStatus(ctx, c, grown.GetStatus())
}

// containersWaitingOnQuota maps a Tenant or a Project, whose status the
// usage controller rewrites as what it holds or may hold changes, to the
// containers under it whose root disk's growth waits on quota.
func (s *Server) containersWaitingOnQuota(ctx context.Context, o store.Object) []controller.Key {
	var projects []string
	if o.Kind == registry.Project.Kind {
		projects = []string{o.Name}
	} else {
		objs, err := s.listWith(ctx, registry.Project, "", nil)
		if err != nil {
			s.log.Error("listing projects failed", "error", err)
			return nil
		}
		for _, obj := range objs {
			if p := obj.(*tenancyv1alpha1.Project); p.GetSpec().GetTenant() == o.Name {
				projects = append(projects, p.GetMetadata().GetName())
			}
		}
	}
	var keys []controller.Key
	for _, project := range projects {
		objs, err := s.listWith(ctx, registry.Container, project, nil)
		if err != nil {
			s.log.Error("listing containers failed", "error", err)
			return nil
		}
		for _, obj := range objs {
			c := obj.(*computev1alpha1.Container)
			if b := conditions.Find(c.GetStatus().GetConditions(), ContainerDiskGrowBlocked); b.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE && b.GetReason() == ReasonDiskGrowQuotaExceeded {
				keys = append(keys, controller.Key{Project: project, Name: c.GetMetadata().GetName()})
			}
		}
	}
	return keys
}

// writeContainerStatus stores status as c's, conditional on c as read, and
// returns c as then stored; a status unchanged is not written.
func (s *Server) writeContainerStatus(ctx context.Context, c *computev1alpha1.Container, status *computev1alpha1.ContainerStatus) (*computev1alpha1.Container, error) {
	if proto.Equal(status, c.GetStatus()) {
		return c, nil
	}
	rv := c.GetMetadata().GetResourceVersion()
	key := resource.Key{Project: c.GetMetadata().GetProject(), Name: c.GetMetadata().GetName()}
	obj, err := s.backend(registry.Container).UpdateStatus(s.system(ctx), key, status, &rv)
	if err != nil {
		return c, err
	}
	return obj.(*computev1alpha1.Container), nil
}
