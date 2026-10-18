package apiserver

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// MaxVolumes bounds the volumes of a container.
const MaxVolumes = 8

// maxTarget bounds a volume's target and subpath, well inside PATH_MAX
// once joined.
const maxTarget = 1024

// Volume types and source kinds, as Compose and spec.volumes name them. See
// docs/decisions/container-volumes.md.
const (
	volumeTypeVolume = "volume"
	sourceKindDisk   = "Disk"
)

// platformMounts are where the guest mounts the platform's filesystems,
// over anything a volume would put there.
var platformMounts = []string{"/proc", "/sys", "/dev", "/run"}

// validateContainerVolumes checks a container's volumes: each a Disk, once,
// at a target of its own.
func validateContainerVolumes(volumes []*computev1alpha1.ContainerVolume) error {
	if len(volumes) > MaxVolumes {
		return invalid("spec.volumes: %d volumes, at most %d", len(volumes), MaxVolumes)
	}
	disks, targets := map[string]bool{}, map[string]bool{}
	for i, v := range volumes {
		switch v.GetType() {
		case "", volumeTypeVolume:
		case "bind":
			return invalid("spec.volumes[%d].type bind is not supported: a container has no host path to mount", i)
		case "tmpfs":
			return invalid("spec.volumes[%d].type tmpfs is not supported yet", i)
		default:
			return invalid("spec.volumes[%d].type %q is not volume", i, v.GetType())
		}
		switch v.GetSourceKind() {
		case "", sourceKindDisk:
		default:
			return invalid("spec.volumes[%d].source_kind %q is not Disk", i, v.GetSourceKind())
		}
		if err := ValidName(v.GetSource()); err != nil {
			return invalid("spec.volumes[%d].source: %v", i, err)
		}
		if disks[v.GetSource()] {
			return invalid("spec.volumes: disk %q is mounted twice", v.GetSource())
		}
		disks[v.GetSource()] = true
		if err := checkTarget(v.GetTarget()); err != nil {
			return invalid("spec.volumes[%d].target: %v", i, err)
		}
		if targets[v.GetTarget()] {
			return invalid("spec.volumes: two volumes are mounted at %s", v.GetTarget())
		}
		targets[v.GetTarget()] = true
		if err := checkSubpath(v.GetVolume().GetSubpath()); err != nil {
			return invalid("spec.volumes[%d].volume.subpath: %v", i, err)
		}
	}
	return nil
}

// checkTarget checks where a volume is mounted: a clean absolute path, not
// / and not where the platform mounts its own filesystems.
func checkTarget(target string) error {
	switch {
	case target == "":
		return fmt.Errorf("is required")
	case !path.IsAbs(target) || path.Clean(target) != target:
		return fmt.Errorf("%q is not a clean absolute path", target)
	case target == "/":
		return fmt.Errorf("a volume cannot be mounted at /")
	case len(target) > maxTarget:
		return fmt.Errorf("%d bytes, at most %d", len(target), maxTarget)
	case strings.ContainsRune(target, 0):
		return fmt.Errorf("%q holds a NUL", target)
	}
	for _, p := range platformMounts {
		if target == p || strings.HasPrefix(target, p+"/") {
			return fmt.Errorf("%s is under %s, which the platform mounts", target, p)
		}
	}
	return nil
}

// checkSubpath checks a directory of a volume: empty, or a clean relative
// path that stays inside it.
func checkSubpath(sub string) error {
	switch {
	case sub == "":
		return nil
	case len(sub) > maxTarget:
		return fmt.Errorf("%d bytes, at most %d", len(sub), maxTarget)
	case path.IsAbs(sub) || path.Clean(sub) != sub || sub == "." || sub == ".." || strings.HasPrefix(sub, "../"):
		return fmt.Errorf("%q is not a clean relative path inside the volume", sub)
	case strings.ContainsRune(sub, 0):
		return fmt.Errorf("%q holds a NUL", sub)
	}
	return nil
}

// machineVolumes are a container's volumes as its machine lists them: its
// disks, in order, and what its boot mounts of them.
func machineVolumes(volumes []*computev1alpha1.ContainerVolume) ([]*computev1alpha1.MachineDisk, []*computev1alpha1.ContainerBootVolume) {
	var disks []*computev1alpha1.MachineDisk
	var mounts []*computev1alpha1.ContainerBootVolume
	for _, v := range volumes {
		disks = append(disks, &computev1alpha1.MachineDisk{Disk: v.GetSource()})
		mounts = append(mounts, &computev1alpha1.ContainerBootVolume{
			Disk: v.GetSource(), Target: v.GetTarget(), ReadOnly: v.GetReadOnly(),
			Nocopy: v.GetVolume().GetNocopy(), Subpath: v.GetVolume().GetSubpath(),
		})
	}
	return disks, mounts
}

// validateBootVolumes checks what a container machine's boot mounts: each
// one of its disks, at most once, at a target of its own.
func validateBootVolumes(spec *computev1alpha1.MachineSpec) error {
	listed := map[string]bool{}
	for _, d := range spec.GetDisks() {
		listed[d.GetDisk()] = true
	}
	mounted, targets := map[string]bool{}, map[string]bool{}
	for i, v := range spec.GetContainer().GetVolumes() {
		if !listed[v.GetDisk()] {
			return invalid("spec.container.volumes[%d]: disk %q is not one of spec.disks", i, v.GetDisk())
		}
		if mounted[v.GetDisk()] {
			return invalid("spec.container.volumes: disk %q is mounted twice", v.GetDisk())
		}
		mounted[v.GetDisk()] = true
		if err := checkTarget(v.GetTarget()); err != nil {
			return invalid("spec.container.volumes[%d].target: %v", i, err)
		}
		if targets[v.GetTarget()] {
			return invalid("spec.container.volumes: two volumes are mounted at %s", v.GetTarget())
		}
		targets[v.GetTarget()] = true
		if err := checkSubpath(v.GetSubpath()); err != nil {
			return invalid("spec.container.volumes[%d].subpath: %v", i, err)
		}
	}
	return nil
}

// diskAttachment is how machine m uses disk, as its spec says, its device
// named in the node's order (docs/decisions/container-volumes.md); nil if m
// does not list disk.
func diskAttachment(m *computev1alpha1.Machine, disk string) *storagev1alpha1.DiskAttachment {
	spec := m.GetSpec()
	a := &storagev1alpha1.DiskAttachment{Machine: m.GetMetadata().GetName()}
	for _, r := range m.GetMetadata().GetOwnerReferences() {
		if r.GetController() {
			a.Kind, a.Name = r.GetKind(), r.GetName()
		}
	}
	first := computeapi.FirstContainerDisk
	if f := spec.GetFirmware(); f != nil {
		first = 1
		if f.GetRootDisk() == disk {
			a.As, a.Device = storagev1alpha1.DiskAttachmentRole_DISK_ATTACHMENT_ROLE_BOOT, computeapi.VirtioDevice(0)
			return a
		}
	}
	for i, d := range spec.GetDisks() {
		if d.GetDisk() != disk {
			continue
		}
		a.As, a.Device = storagev1alpha1.DiskAttachmentRole_DISK_ATTACHMENT_ROLE_DATA, computeapi.VirtioDevice(first+i)
		for _, v := range spec.GetContainer().GetVolumes() {
			if v.GetDisk() == disk {
				a.As, a.Path, a.ReadOnly = storagev1alpha1.DiskAttachmentRole_DISK_ATTACHMENT_ROLE_VOLUME, v.GetTarget(), v.GetReadOnly()
			}
		}
		return a
	}
	return nil
}

// containersMountingDisk maps a disk to the containers whose volumes name
// it: one held back for it goes on once it is made, or let go elsewhere.
func (s *Server) containersMountingDisk(ctx context.Context, o store.Object) []controller.Key {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Container.Kind, Project: o.Project})
	if err != nil {
		s.log.Error("listing containers failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, co := range objs {
		obj, err := s.backend(registry.Container).decode(co)
		if err != nil {
			continue
		}
		if slices.ContainsFunc(obj.(*computev1alpha1.Container).GetSpec().GetVolumes(), func(v *computev1alpha1.ContainerVolume) bool {
			return v.GetSource() == o.Name
		}) {
			keys = append(keys, controller.Key{Project: co.Project, Name: co.Name})
		}
	}
	return keys
}

// attachmentOf reads the machine named, the first that lists the disk, and
// says how it uses the disk and the node the machine is placed on; nothing
// when none is named or it is gone.
func (s *Server) attachmentOf(ctx context.Context, project, machine, disk string) (*storagev1alpha1.DiskAttachment, string, error) {
	if machine == "" {
		return nil, "", nil
	}
	obj, err := s.get(ctx, registry.Machine, resource.Key{Project: project, Name: machine})
	if err != nil || obj == nil {
		return nil, "", err
	}
	m := obj.(*computev1alpha1.Machine)
	return diskAttachment(m, disk), m.GetSpec().GetNode(), nil
}
