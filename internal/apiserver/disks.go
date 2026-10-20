package apiserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

const (
	// DiskInUseFinalizer keeps a disk until no machine lists it; the disk
	// controller removes it then. NodeCleanupFinalizer then keeps it until
	// its node reports it Released, or goes at once for a disk never bound.
	DiskInUseFinalizer = computeapi.DiskInUseFinalizer
	// DiskAllocated, DiskReady and DiskReleased are described in computeapi.
	DiskAllocated = computeapi.DiskAllocated
	DiskReady     = computeapi.DiskReady
	DiskReleased  = computeapi.DiskReleased
)

func decodeDisk(o store.Object) (*storagev1alpha1.Disk, bool) {
	var d storagev1alpha1.Disk
	return &d, proto.Unmarshal(o.Data, &d) == nil
}

// diskBytes reads a disk's spec.size.
func diskBytes(spec *storagev1alpha1.DiskSpec) (uint64, error) {
	if spec.GetSize() == "" {
		return 0, errors.New("spec.size is required, such as 10Gi")
	}
	b, err := quantity.Bytes(spec.GetSize())
	if err != nil {
		return 0, fmt.Errorf("spec.size: %w", err)
	}
	return b, nil
}

// diskHeld is what a bound disk holds on its node: status.size, which
// binding sets.
func diskHeld(d *storagev1alpha1.Disk) (uint64, error) {
	b, err := quantity.Bytes(d.GetStatus().GetSize())
	if err != nil {
		return 0, fmt.Errorf("disk %s/%s status.size: %w", d.GetMetadata().GetProject(), d.GetMetadata().GetName(), err)
	}
	return b, nil
}

// validateDisk checks a disk's size. Only a change of size is checked against
// the disk as it is: it may not go below what the disk has on its node, so a
// growth that never took effect can be withdrawn, and a disk not yet made can
// still shrink.
func validateDisk(ctx context.Context, s *Server, _ auth.Principal, old, obj proto.Message) error {
	d := obj.(*storagev1alpha1.Disk)
	size, err := diskBytes(d.GetSpec())
	if err != nil {
		return invalid("%v", err)
	}
	if size < minDisk {
		return invalid("spec.size %s is below the minimum of 1Gi", d.GetSpec().GetSize())
	}
	if size > maxDisk {
		return invalid("spec.size %s is above the maximum of 64Ti", d.GetSpec().GetSize())
	}
	var was *storagev1alpha1.Disk
	if old != nil {
		was = old.(*storagev1alpha1.Disk)
	}
	if err := s.validateDiskSource(ctx, was, d, size); err != nil {
		return err
	}
	if err := checkRootDiskOwner(ctx, s, was, d); err != nil {
		return err
	}
	if was == nil {
		return nil
	}
	if stored, err := diskBytes(was.GetSpec()); err == nil && wholeGi(stored) == wholeGi(size) {
		return nil
	}
	if have := was.GetStatus().GetSize(); have != "" {
		if b, err := quantity.Bytes(have); err == nil && wholeGi(size) < wholeGi(b) {
			return invalid("spec.size %s is smaller than the current %s; a disk can grow but never shrink", d.GetSpec().GetSize(), have)
		}
	}
	return nil
}

// machinesUsing returns the machines, live or being deleted, that list the
// disk, by name.
func (s *Server) machinesUsing(ctx context.Context, project, disk string) ([]string, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, o := range objs {
		m, ok := decodeMachine(o)
		if !ok {
			return nil, fmt.Errorf("decode machine %s/%s", o.Project, o.Name)
		}
		if listsDisk(m.GetSpec(), disk) {
			names = append(names, o.Name)
		}
	}
	return names, nil
}

func listsDisk(spec *computev1alpha1.MachineSpec, disk string) bool {
	return slices.Contains(computeapi.MachineDisks(spec), disk)
}

// refuseDeletingAttachedDisk keeps users from deleting a disk a live machine
// has: the machine could not boot without it. A project being deleted takes its
// disks with it, and they wait for their machines under DiskInUseFinalizer.
func refuseDeletingAttachedDisk(ctx context.Context, s *Server, obj proto.Message) error {
	d := obj.(*storagev1alpha1.Disk)
	project, err := s.get(ctx, registry.Project, resource.Key{Name: d.GetMetadata().GetProject()})
	if err != nil {
		return err
	}
	if project == nil || project.(*tenancyv1alpha1.Project).GetMetadata().GetDeleteTime() != nil {
		return nil
	}
	machines, err := s.machinesUsing(ctx, d.GetMetadata().GetProject(), d.GetMetadata().GetName())
	if err != nil {
		return err
	}
	// A disk whose machines are all going, as a VM's root disk deleted with
	// it, may go too; it waits for them under DiskInUseFinalizer.
	var live []string
	for _, name := range machines {
		obj, err := s.get(ctx, registry.Machine, resource.Key{Project: d.GetMetadata().GetProject(), Name: name})
		if err != nil {
			return err
		}
		if m, ok := obj.(*computev1alpha1.Machine); ok && m.GetMetadata().GetDeleteTime() == nil {
			live = append(live, name)
		}
	}
	if len(live) == 0 {
		return nil
	}
	return failedPrecondition("disk %q is attached to machine %s; detach it first",
		d.GetMetadata().GetName(), strings.Join(live, ", "))
}

// checkAttaching checks the disks a machine lists: each at most once, and
// each one it did not list before (old is nil on create) a live disk no
// other machine lists. The caller holds s.attach until the machine is
// written.
func (s *Server) checkAttaching(ctx context.Context, meta *metav1.ObjectMeta, old, spec *computev1alpha1.MachineSpec) error {
	seen := map[string]bool{}
	names := computeapi.MachineDisks(spec)
	for i, name := range names {
		if err := ValidName(name); err != nil {
			return invalid("disk %d of spec.firmware.root_disk and spec.disks: %v", i, err)
		}
		if seen[name] {
			return invalid("disk %q is attached twice", name)
		}
		seen[name] = true
	}
	for _, name := range names {
		if listsDisk(old, name) {
			continue
		}
		if err := s.requireLive(ctx, registry.Disk, meta.GetProject(), name); err != nil {
			return err
		}
		if err := s.checkSameNode(ctx, meta, spec.GetNode(), name); err != nil {
			return err
		}
		users, err := s.machinesUsing(ctx, meta.GetProject(), name)
		if err != nil {
			return err
		}
		users = slices.DeleteFunc(users, func(u string) bool { return u == meta.GetName() })
		if len(users) > 0 {
			return failedPrecondition("disk %q is attached to machine %s; a disk is attached to one machine at a time",
				name, strings.Join(users, ", "))
		}
	}
	return nil
}

// reasonDoesNotFitMachineNode is a disk's Allocated reason while the node of
// the placed machine that lists it has no room to make it.
const reasonDoesNotFitMachineNode = "DoesNotFitMachineNode"

// checkSameNode refuses a disk bound to a node other than node, the one a
// placed machine runs on: a machine never moves. A disk bound to no node
// goes to the machine's (reportDisk).
func (s *Server) checkSameNode(ctx context.Context, meta *metav1.ObjectMeta, node, disk string) error {
	if node == "" {
		return nil
	}
	obj, err := s.get(ctx, registry.Disk, resource.Key{Project: meta.GetProject(), Name: disk})
	if err != nil || obj == nil {
		return err
	}
	if on := obj.(*storagev1alpha1.Disk).GetStatus().GetNode(); on != "" && on != node {
		return invalid("disk %q lives on node %s, and machine %s runs on node %s; a machine never moves: "+
			"it takes a disk of its node or one not made yet, and is made again to take another", disk, on, meta.GetName(), node)
	}
	return nil
}

// disksOfMachine maps a machine to the disks it lists and those reported
// attached to it, which it may no longer list: a machine changing, coming or
// going changes what they are attached to.
func (s *Server) disksOfMachine(ctx context.Context, o store.Object) []controller.Key {
	m, ok := decodeMachine(o)
	if !ok {
		return nil
	}
	var keys []controller.Key
	for _, d := range computeapi.MachineDisks(m.GetSpec()) {
		keys = append(keys, controller.Key{Project: o.Project, Name: d})
	}
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Disk.Kind, Project: o.Project})
	if err != nil {
		s.log.Error("listing disks failed", "error", err)
		return keys
	}
	for _, do := range objs {
		if d, ok := decodeDisk(do); ok && d.GetStatus().GetMachine() == o.Name {
			keys = append(keys, controller.Key{Project: do.Project, Name: do.Name})
		}
	}
	return keys
}

// disksOfMachineAndWaiting maps a machine to its disks and, when it sits on
// a node, to every disk waiting for room to grow: it shrinking or going may
// make some.
func (s *Server) disksOfMachineAndWaiting(ctx context.Context, o store.Object) []controller.Key {
	keys := s.disksOfMachine(ctx, o)
	if m, ok := decodeMachine(o); ok && m.GetSpec().GetNode() != "" {
		keys = append(keys, s.disksWaitingForRoom(ctx, o)...)
	}
	return keys
}

// disksWaitingForRoom maps to every disk whose growth did not fit its node.
func (s *Server) disksWaitingForRoom(ctx context.Context, _ store.Object) []controller.Key {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Disk.Kind})
	if err != nil {
		s.log.Error("listing disks failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, o := range objs {
		d, ok := decodeDisk(o)
		if !ok {
			continue
		}
		if c := conditions.Find(d.GetStatus().GetConditions(), DiskAllocated); c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE &&
			(d.GetStatus().GetNode() != "" || c.GetReason() == reasonDoesNotFitMachineNode) {
			keys = append(keys, controller.Key{Project: o.Project, Name: o.Name})
		}
	}
	return keys
}

// reconcileDisk reports what a disk is attached to and grants its growth on
// its node; a deleted disk is let go once no machine lists it and its node
// has removed it.
func (s *Server) reconcileDisk(ctx context.Context, k controller.Key) error {
	key := resource.Key{Project: k.Project, Name: k.Name}
	obj, err := s.get(ctx, registry.Disk, key)
	if err != nil || obj == nil {
		return err
	}
	d := obj.(*storagev1alpha1.Disk)
	users, err := s.machinesUsing(ctx, k.Project, k.Name)
	if err != nil {
		return err
	}
	if d.GetMetadata().GetDeleteTime() != nil {
		// A machine attaching it after its deletion began is refused, so no
		// machine lists it again once none does.
		if len(users) > 0 {
			return nil
		}
		if err := s.RemoveFinalizer(ctx, registry.Disk, key, DiskInUseFinalizer); err != nil {
			return err
		}
		if d.GetStatus().GetNode() == "" || conditions.IsTrue(d.GetStatus().GetConditions(), DiskReleased) {
			return s.RemoveFinalizer(ctx, registry.Disk, key, NodeCleanupFinalizer)
		}
		return nil
	}
	return s.reportDisk(ctx, key, d, users)
}

// reportDisk writes the apiserver's part of a disk's status, conditional on d
// as read, and nothing if nothing changed. A growth is granted under
// s.capacity, as one decision with the node's other reservations.
func (s *Server) reportDisk(ctx context.Context, key resource.Key, d *storagev1alpha1.Disk, users []string) error {
	gen := d.GetMetadata().GetGeneration()
	status := &storagev1alpha1.DiskStatus{}
	if d.GetStatus() != nil {
		status = proto.Clone(d.GetStatus()).(*storagev1alpha1.DiskStatus)
	}
	status.Machine = ""
	if len(users) > 0 {
		status.Machine = users[0]
	}
	attachment, placedOn, err := s.attachmentOf(ctx, key.Project, status.GetMachine(), key.Name)
	if err != nil {
		return err
	}
	status.Attachment = attachment
	allocated := &metav1.Condition{Type: DiskAllocated, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Allocated"}
	if node := status.GetNode(); node == "" && placedOn != "" {
		// A disk added to a machine already placed goes to the machine's
		// node, a growth from nothing there: a machine never moves.
		want, err := diskBytes(d.GetSpec())
		if err != nil {
			return err
		}
		s.capacity.Lock()
		defer s.capacity.Unlock()
		err = s.growthFits(ctx, "the disk", placedOn, 0, want)
		var stuck *blocked
		switch {
		case errors.As(err, &stuck):
			allocated = &metav1.Condition{
				Type: DiskAllocated, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reasonDoesNotFitMachineNode,
				Message: fmt.Sprintf("machine %s runs on node %s, which has no room for the disk: %s", status.GetMachine(), placedOn, stuck.message),
			}
		case err != nil:
			return err
		default:
			status.Node, status.Size = placedOn, quantity.FormatDisk(want)
		}
	} else if node == "" {
		allocated = &metav1.Condition{
			Type: DiskAllocated, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "WaitingForFirstMachine",
			Message: "the disk is made on the node of the first machine it is attached to",
		}
	} else {
		want, err := diskBytes(d.GetSpec())
		if err != nil {
			return err
		}
		have, err := diskHeld(d)
		if err != nil {
			return err
		}
		if wholeGi(want) > wholeGi(have) {
			s.capacity.Lock()
			defer s.capacity.Unlock()
			err := s.growthFits(ctx, "the disk", node, have, want)
			var stuck *blocked
			switch {
			case errors.As(err, &stuck):
				allocated = &metav1.Condition{
					Type: DiskAllocated, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: stuck.reason, Message: stuck.message,
				}
			case err != nil:
				return err
			default:
				status.Size = quantity.FormatDisk(want)
			}
		}
	}
	allocated.ObservedGeneration = gen
	status.Conditions = conditions.Set(status.GetConditions(), allocated, s.clock.Now())
	switch {
	case status.GetNode() == "" || !conditions.IsTrue(status.GetConditions(), DiskReady):
		status.Phase = storagev1alpha1.DiskPhase_DISK_PHASE_PENDING
	case status.GetMachine() != "":
		status.Phase = storagev1alpha1.DiskPhase_DISK_PHASE_IN_USE
	default:
		status.Phase = storagev1alpha1.DiskPhase_DISK_PHASE_AVAILABLE
	}
	status.ObservedGeneration = gen
	if proto.Equal(status, d.GetStatus()) {
		return nil
	}
	rv := d.GetMetadata().GetResourceVersion()
	_, err = s.backend(registry.Disk).UpdateStatus(s.system(ctx), key, status, &rv)
	return err
}

// growthFits says whether what, a volume on node such as "the disk", can
// grow from have to want bytes: the growth must fit in what the node has
// left. A node that reports no disk is not checked. The caller holds
// s.capacity.
func (s *Server) growthFits(ctx context.Context, what, node string, have, want uint64) error {
	nodes, used, err := s.nodeUsage(ctx, resource.Key{})
	if err != nil {
		return err
	}
	var room nodeRoom
	for _, n := range nodes {
		if n.GetMetadata().GetName() == node {
			if room, err = roomOf(n); err != nil {
				return &blocked{"NodeSizeUnreadable", err.Error() + "; " + what + " keeps its size"}
			}
		}
	}
	growth := wholeGi(want)*quantity.GiB - wholeGi(have)*quantity.GiB
	if left := free(room.disk, used[node].disk); room.hasDisk && growth > left {
		return &blocked{"ResizeDoesNotFit", fmt.Sprintf(
			"node %s has %s of disk free, and growing %s to %s takes %s more; it keeps %s",
			node, quantity.FormatDisk(left), what, quantity.FormatDisk(want), quantity.FormatDisk(growth), quantity.FormatDisk(have))}
	}
	return nil
}

// machineDisks reads the disks m lists: the node the bound ones live on, to
// which m must go, and the unbound ones, which go with it to whatever node
// it goes to. why, if not empty, says what keeps m from being placed at all.
func (s *Server) machineDisks(ctx context.Context, m *computev1alpha1.Machine) (node string, unbound []*storagev1alpha1.Disk, why string, err error) {
	project := m.GetMetadata().GetProject()
	bound := ""
	for _, name := range computeapi.MachineDisks(m.GetSpec()) {
		obj, err := s.get(ctx, registry.Disk, resource.Key{Project: project, Name: name})
		if err != nil {
			return "", nil, "", err
		}
		if obj == nil {
			return "", nil, fmt.Sprintf("disk %s does not exist", name), nil
		}
		d := obj.(*storagev1alpha1.Disk)
		if d.GetMetadata().GetDeleteTime() != nil {
			return "", nil, fmt.Sprintf("disk %s is being deleted", name), nil
		}
		switch n := d.GetStatus().GetNode(); {
		case n == "":
			unbound = append(unbound, d)
		case node == "":
			node, bound = n, name
		case n != node:
			return "", nil, fmt.Sprintf("disk %s lives on node %s and disk %s on node %s", bound, node, name, n), nil
		}
	}
	return node, unbound, "", nil
}

// bindDisks binds each disk to node at its spec.size, each write conditional
// on the disk as read. The caller holds s.capacity, having found the room.
func (s *Server) bindDisks(ctx context.Context, node string, disks []*storagev1alpha1.Disk) error {
	for _, d := range disks {
		size, err := diskBytes(d.GetSpec())
		if err != nil {
			return err
		}
		status := &storagev1alpha1.DiskStatus{}
		if d.GetStatus() != nil {
			status = proto.Clone(d.GetStatus()).(*storagev1alpha1.DiskStatus)
		}
		status.Node = node
		status.Size = quantity.FormatDisk(size)
		rv := d.GetMetadata().GetResourceVersion()
		key := resource.Key{Project: d.GetMetadata().GetProject(), Name: d.GetMetadata().GetName()}
		if _, err := s.backend(registry.Disk).UpdateStatus(s.system(ctx), key, status, &rv); err != nil {
			return fmt.Errorf("binding disk %s to node %s: %w", key.Name, node, err)
		}
	}
	return nil
}

// containersOnDiskNode maps a disk to the containers on its node: a disk
// shrinking or going may make room for a resize one is waiting on.
func (s *Server) containersOnDiskNode(ctx context.Context, o store.Object) []controller.Key {
	d, ok := decodeDisk(o)
	if !ok || d.GetStatus().GetNode() == "" {
		return nil
	}
	return s.containersOnNode(ctx, store.Object{Key: store.Key{Name: d.GetStatus().GetNode()}})
}
