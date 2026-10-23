package apiserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
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
// has, and no VM or container asks to run with; or a file to upload of at
// most the bound. The caller holds attach, which every write of a machine,
// VM, container and disk holds too, so none of them changes between the
// check and the write.
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
	if up := spec.GetSource().GetUpload(); up != nil {
		return s.validateUpload(up)
	}
	src := spec.GetSource().GetDisk()
	if src == nil {
		return invalid("spec.source needs disk or upload")
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

// validateUpload checks the file an image is uploaded from: its size, up
// to the bound, and its sha256, if any.
func (s *Server) validateUpload(up *storagev1alpha1.CustomImageUploadSource) error {
	if up.GetSize() <= 0 {
		return invalid("spec.source.upload.size is required: the file's size in bytes")
	}
	if bound := s.maxUploadSize(); up.GetSize() > bound {
		return invalid("spec.source.upload.size %d is more than %d bytes (%s), the most an uploaded file may be",
			up.GetSize(), bound, quantity.FormatDisk(uint64(bound)))
	}
	if h := up.GetSha256(); h != "" && !sha256Hex.MatchString(h) {
		return invalid("spec.source.upload.sha256 %q is not 64 lowercase hex digits", h)
	}
	return nil
}

// maxUploadSize is the most bytes a file uploaded for a custom image holds.
func (s *Server) maxUploadSize() int64 {
	if s.cfg.MaxUploadSize > 0 {
		return s.cfg.MaxUploadSize
	}
	return DefaultMaxUploadSize
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
		if disk := ci.GetSpec().GetSource().GetDisk(); disk != nil && computeapi.CustomImageHoldsSource(ci) {
			held[disk.GetName()] = o.Name
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
// source disk's node, where it is captured, and size, admission having
// found the disk made, under attach, which the create still holds; or for
// an upload the node picked to take the file, as a machine's is.
func drawCustomImage(ctx context.Context, s *Server, obj proto.Message) (proto.Message, error) {
	ci := obj.(*storagev1alpha1.CustomImage)
	if ci.GetSpec().GetSource().GetUpload() != nil {
		node, why, err := s.pickNode(ctx, &computev1alpha1.Machine{}, "", 0)
		if err != nil {
			return nil, err
		}
		if node == "" {
			return nil, failedPrecondition("no node can take the upload: %s", why)
		}
		return &storagev1alpha1.CustomImageStatus{Node: node}, nil
	}
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
// stores: all the node reports but the apiserver's node and disk size,
// once stored. An upload's disk size and virtual size are the node's to
// set, once: the size of the disk the file holds, which only its
// converter reads. The apiserver's QuotaExceeded stays.
func customImageFromNode(stored, reported proto.Message) proto.Message {
	out := proto.Clone(reported).(*storagev1alpha1.CustomImageStatus)
	st, _ := stored.(*storagev1alpha1.CustomImageStatus)
	out.Node = st.GetNode()
	if st.GetDiskSize() != "" {
		out.DiskSize = st.GetDiskSize()
	}
	if st.GetVirtualSize() != 0 {
		out.VirtualSize = st.GetVirtualSize()
	}
	if c := conditions.Find(st.GetConditions(), CustomImageReady); c.GetReason() == computeapi.ReasonQuotaExceeded {
		out.Conditions = slices.DeleteFunc(out.Conditions, func(o *metav1.Condition) bool { return o.GetType() == CustomImageReady })
		out.Conditions = append(out.Conditions, c)
	}
	return out
}

// customImageGrows says a node's report on an upload, stored as obj over
// stored, gives it its virtual size, which it then counts toward the disk
// quota, rounded up to whole Gi, if more than its file's size.
func customImageGrows(stored, obj proto.Message) bool {
	ci := obj.(*storagev1alpha1.CustomImage)
	return ci.GetSpec().GetSource().GetUpload() != nil &&
		stored.(*storagev1alpha1.CustomImage).GetStatus().GetVirtualSize() == 0 &&
		ci.GetStatus().GetVirtualSize() != 0 && !computeapi.CustomImageFailed(ci)
}

// customImageOverQuota is what a node's report of an upload's virtual
// size, status, stores when it does not fit the disk quota, as err says:
// the image failed for good, as QuotaExceeded, its virtual size kept to
// tell. The node, seeing it, makes no base and removes the file.
func customImageOverQuota(s *Server, status proto.Message, err error) proto.Message {
	out := proto.Clone(status).(*storagev1alpha1.CustomImageStatus)
	msg := err.Error()
	if ce := new(connect.Error); errors.As(err, &ce) {
		msg = ce.Message()
	}
	out.Conditions = conditions.Set(out.GetConditions(), &metav1.Condition{
		Type: CustomImageReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: computeapi.ReasonQuotaExceeded,
		Message:            fmt.Sprintf("the file holds a disk of %s, which the disk quota has no room for: %s", quantity.FormatDisk(wholeGi(uint64(out.GetVirtualSize()))*quantity.GiB), msg),
		ObservedGeneration: out.GetObservedGeneration(),
	}, s.clock.Now())
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
// has its own status, or until an upload is Ready the larger of its file's
// size and its virtual size, once kept, and what it holds once Ready. One
// that failed takes nothing.
func customImageAmounts(obj proto.Message, in projectObjects) amounts {
	ci := obj.(*storagev1alpha1.CustomImage)
	var a amounts
	if computeapi.CustomImageFailed(ci) {
		return a
	}
	a[dimCustomImages] = 1
	size := ci.GetStatus().GetDiskSize()
	up := ci.GetSpec().GetSource().GetUpload()
	switch {
	case computeapi.CustomImageReadyNow(ci):
		size = ci.GetStatus().GetUsedSize()
	case up != nil:
		bytes := max(up.GetSize(), ci.GetStatus().GetVirtualSize(), 0)
		a[dimDisk] = int64(wholeGi(uint64(bytes)) * quantity.GiB)
		return a
	case size == "":
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
