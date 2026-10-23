package apiserver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// CustomImageSnapshotted and CustomImageReady are described in computeapi.
const (
	CustomImageSnapshotted = computeapi.CustomImageSnapshotted
	CustomImageReady       = computeapi.CustomImageReady
)

func decodeCustomImage(o store.Object) (*storagev1alpha1.CustomImage, bool) {
	var ci storagev1alpha1.CustomImage
	return &ci, proto.Unmarshal(o.Data, &ci) == nil
}

// validateCustomImage checks a custom image's spec and, as it is made, its
// source: a live Disk made on a node, which no machine that is not stopped
// has, and no VM or container asks to run with. The caller holds attach,
// which every write of a machine, VM, container and disk holds too, so
// none of them changes between the check and the write.
func validateCustomImage(ctx context.Context, s *Server, _ auth.Principal, old, obj proto.Message) error {
	ci := obj.(*storagev1alpha1.CustomImage)
	spec := ci.GetSpec()
	if u := spec.GetDefaultUser(); u != "" && !unixUser.MatchString(u) {
		return invalid("spec.default_user %q is not a login name, such as ubuntu", u)
	}
	if m := spec.GetMinDiskSize(); m != "" {
		b, err := quantity.Bytes(m)
		if err != nil {
			return invalid("spec.min_disk_size: %v", err)
		}
		if b < minDisk || b > maxDisk {
			return invalid("spec.min_disk_size %s is not between 1Gi and 64Ti", m)
		}
	}
	src := spec.GetSource().GetDisk()
	if src == nil {
		return invalid("spec.source.disk is required")
	}
	if err := ValidName(src.GetName()); err != nil {
		return invalid("spec.source.disk.name: %v", err)
	}
	if old != nil {
		return nil
	}
	_, err := s.captureSource(ctx, ci.GetMetadata().GetProject(), src.GetName())
	return err
}

// captureSource returns the disk a new image of project is captured from,
// refusing one that is not there, not made yet, or in use by a machine
// that is not stopped.
func (s *Server) captureSource(ctx context.Context, project, name string) (*storagev1alpha1.Disk, error) {
	obj, err := s.get(ctx, registry.Disk, resource.Key{Project: project, Name: name})
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, failedPrecondition("disk %q does not exist", name)
	}
	d := obj.(*storagev1alpha1.Disk)
	if d.GetMetadata().GetDeleteTime() != nil {
		return nil, failedPrecondition("disk %q is being deleted", name)
	}
	if d.GetStatus().GetNode() == "" || d.GetStatus().GetSize() == "" {
		return nil, failedPrecondition("disk %q has not been made on a node yet; it is made when a machine it is attached to first runs", name)
	}
	if err := s.runningWith(ctx, project, name); err != nil {
		return nil, err
	}
	return d, nil
}

// runningWith refuses capturing the disk of project while anything with
// it may run: a machine listing it that its node has not reported stopped
// at its current spec, or a VM or container listing it that asks to run.
func (s *Server) runningWith(ctx context.Context, project, disk string) error {
	var running []string
	machines, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind, Project: project})
	if err != nil {
		return err
	}
	for _, o := range machines {
		m, ok := decodeMachine(o)
		if !ok {
			return fmt.Errorf("decode machine %s/%s", o.Project, o.Name)
		}
		if listsDisk(m.GetSpec(), disk) && !machineHasStopped(m) {
			running = append(running, "machine "+o.Name)
		}
	}
	vms, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.VirtualMachine.Kind, Project: project})
	if err != nil {
		return err
	}
	for _, o := range vms {
		vm, ok := decodeVM(o)
		if !ok {
			return fmt.Errorf("decode virtual machine %s/%s", o.Project, o.Name)
		}
		if slices.Contains(vmDisks(vm), disk) && vmRuns(vm.GetSpec()) {
			running = append(running, "virtual machine "+o.Name)
		}
	}
	containers, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Container.Kind, Project: project})
	if err != nil {
		return err
	}
	for _, o := range containers {
		c, ok := decodeContainer(o)
		if !ok {
			return fmt.Errorf("decode container %s/%s", o.Project, o.Name)
		}
		if slices.Contains(containerDisks(c.GetSpec()), disk) && containerRuns(c.GetSpec()) {
			running = append(running, "container "+o.Name)
		}
	}
	if len(running) == 0 {
		return nil
	}
	return failedPrecondition("disk %q is attached to %s, which is not stopped; stop it first, and wait until it is",
		disk, strings.Join(running, ", "))
}

// machineHasStopped says m's node has stopped it at its current spec, which
// asks it to stay stopped.
func machineHasStopped(m *computev1alpha1.Machine) bool {
	if m.GetSpec().GetRunState() != computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED ||
		m.GetStatus().GetObservedGeneration() != m.GetMetadata().GetGeneration() {
		return false
	}
	switch m.GetStatus().GetPhase() {
	case computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, computev1alpha1.MachinePhase_MACHINE_PHASE_FAILED:
		return true
	}
	return false
}

// vmDisks are the disks a VM lists: its root disk, then spec.disks.
func vmDisks(vm *computev1alpha1.VirtualMachine) []string {
	out := []string{vmRootDisk(vm)}
	for _, d := range vm.GetSpec().GetDisks() {
		out = append(out, d.GetDisk())
	}
	return out
}

func vmRuns(spec *computev1alpha1.VirtualMachineSpec) bool {
	return spec.GetRunState() != computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED
}

// containerDisks are the disks a container's volumes mount.
func containerDisks(spec *computev1alpha1.ContainerSpec) []string {
	disks, _, _ := machineVolumes(spec.GetVolumes())
	var out []string
	for _, d := range disks {
		out = append(out, d.GetDisk())
	}
	return out
}

func decodeContainer(o store.Object) (*computev1alpha1.Container, bool) {
	var c computev1alpha1.Container
	return &c, proto.Unmarshal(o.Data, &c) == nil
}

func containerRuns(spec *computev1alpha1.ContainerSpec) bool {
	return spec.GetRunState() != computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED
}

// heldSources are the disks of project that custom images still hold
// (computeapi.CustomImageHoldsSource), each with an image holding it.
func (s *Server) heldSources(ctx context.Context, project string) (map[string]string, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.CustomImage.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	held := map[string]string{}
	for _, o := range objs {
		ci, ok := decodeCustomImage(o)
		if !ok {
			return nil, fmt.Errorf("decode custom image %s/%s", o.Project, o.Name)
		}
		if computeapi.CustomImageHoldsSource(ci) {
			held[ci.GetSpec().GetSource().GetDisk().GetName()] = o.Name
		}
	}
	return held, nil
}

// checkHeldDisks refuses, while a custom image holds one of the disks
// listed, attaching it, which listing it where was did not, and starting
// what has it, which runs where was did not (was is nil on create). what
// names the writer, such as "virtual machine db". The caller holds attach.
func (s *Server) checkHeldDisks(ctx context.Context, project, what string, disks, was []string, runs, ran bool) error {
	if len(disks) == 0 {
		return nil
	}
	held, err := s.heldSources(ctx, project)
	if err != nil || len(held) == 0 {
		return err
	}
	for _, d := range disks {
		image, ok := held[d]
		if !ok {
			continue
		}
		if !slices.Contains(was, d) {
			return failedPrecondition("disk %q is being captured by custom image %q; %s can attach it once the image is Snapshotted", d, image, what)
		}
		if runs && !ran {
			return failedPrecondition("disk %q is being captured by custom image %q; %s can start once the image is Snapshotted", d, image, what)
		}
	}
	return nil
}

// refuseDeletingHeldDisk keeps a disk a custom image still holds: its node
// has yet to snapshot it.
func (s *Server) refuseDeletingHeldDisk(ctx context.Context, d *storagev1alpha1.Disk) error {
	held, err := s.heldSources(ctx, d.GetMetadata().GetProject())
	if err != nil {
		return err
	}
	if image, ok := held[d.GetMetadata().GetName()]; ok {
		return failedPrecondition("disk %q is being captured by custom image %q; delete it once the image is Snapshotted, or delete the image",
			d.GetMetadata().GetName(), image)
	}
	return nil
}

// drawCustomImage is the status a new custom image is stored with: its
// source disk's node, where it is captured, and size. Admission found the
// disk made, under attach, which the create still holds.
func drawCustomImage(ctx context.Context, s *Server, obj proto.Message) (proto.Message, error) {
	ci := obj.(*storagev1alpha1.CustomImage)
	name := ci.GetSpec().GetSource().GetDisk().GetName()
	dobj, err := s.get(ctx, registry.Disk, resource.Key{Project: ci.GetMetadata().GetProject(), Name: name})
	if err != nil {
		return nil, err
	}
	d, ok := dobj.(*storagev1alpha1.Disk)
	if !ok {
		return nil, failedPrecondition("disk %q does not exist", name)
	}
	size, err := diskHeld(d)
	if err != nil {
		return nil, err
	}
	return &storagev1alpha1.CustomImageStatus{Node: d.GetStatus().GetNode(), DiskSize: quantity.FormatDisk(wholeGi(size) * quantity.GiB)}, nil
}

// customImageFromNode is what a node's status report of a custom image
// stores: the node's conditions, used size and observed generation, and
// the apiserver's node and disk size as stored.
func customImageFromNode(stored, reported proto.Message) proto.Message {
	out := proto.Clone(reported).(*storagev1alpha1.CustomImageStatus)
	st, _ := stored.(*storagev1alpha1.CustomImageStatus)
	out.Node, out.DiskSize = st.GetNode(), st.GetDiskSize()
	return out
}

// customImageNode is the node an image lives on, the only one that
// reports on it.
func customImageNode(obj proto.Message) string {
	return obj.(*storagev1alpha1.CustomImage).GetStatus().GetNode()
}

// customImageFloor is the smallest disk, in whole Gi, made from ci: its
// min_disk_size and its disk_size.
func customImageFloor(ci *storagev1alpha1.CustomImage) uint64 {
	var floor uint64
	for _, q := range []string{ci.GetSpec().GetMinDiskSize(), ci.GetStatus().GetDiskSize()} {
		if b, err := quantity.Bytes(q); err == nil {
			floor = max(floor, wholeGi(b))
		}
	}
	return floor
}

// customImageAmounts is a custom image and the disk it takes: its source
// disk's size while it is captured, read from the disk in until the image
// has its own status, and what it holds once Ready. One that failed takes
// nothing.
func customImageAmounts(obj proto.Message, in projectObjects) amounts {
	ci := obj.(*storagev1alpha1.CustomImage)
	var a amounts
	if computeapi.CustomImageFailed(ci) {
		return a
	}
	a[dimCustomImages] = 1
	size := ci.GetStatus().GetDiskSize()
	if computeapi.CustomImageReadyNow(ci) {
		size = ci.GetStatus().GetUsedSize()
	} else if size == "" {
		// As drawCustomImage will store it.
		name := ci.GetSpec().GetSource().GetDisk().GetName()
		for _, o := range in[registry.Disk.Kind] {
			if d := o.(*storagev1alpha1.Disk); d.GetMetadata().GetName() == name {
				size = d.GetStatus().GetSize()
			}
		}
	}
	if b, err := quantity.Bytes(size); err == nil {
		a[dimDisk] = int64(wholeGi(b) * quantity.GiB)
	}
	return a
}
