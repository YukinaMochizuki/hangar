package apiserver

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// maxDisplayName is the longest display name a size may have, enough for a
// label such as "XL" or "Large, high memory".
const maxDisplayName = 63

// validateMachineSize checks a size as a container's resources are checked,
// though a size has no defaults: cpu, memory and disk are all required, and
// its disk is whole Gi, the unit a machine's disk is held in.
func validateMachineSize(_ context.Context, _ *Server, _ auth.Principal, _, obj proto.Message) error {
	ms := obj.(*computev1alpha1.MachineSize)
	if path := unknownField(ms.ProtoReflect(), "machine_size"); path != "" {
		return invalid("%s holds a field this apiserver does not know", path)
	}
	spec := ms.GetSpec()
	if n := spec.GetDisplayName(); strings.TrimSpace(n) == "" || len(n) > maxDisplayName {
		return invalid("spec.display_name %q must be 1 to %d characters, not only spaces", n, maxDisplayName)
	}
	if spec.GetOrder() < 0 {
		return invalid("spec.order %d is negative", spec.GetOrder())
	}
	var sz sizes
	var err error
	if sz.cpu, err = quantity.CPU(spec.GetCpu()); err != nil {
		return invalid("spec.cpu: %v", err)
	}
	if sz.memory, err = quantity.Bytes(spec.GetMemory()); err != nil {
		return invalid("spec.memory: %v", err)
	}
	if sz.disk, err = quantity.Bytes(spec.GetDisk()); err != nil {
		return invalid("spec.disk: %v", err)
	}
	switch {
	case sz.memory < minMemory:
		return invalid("spec.memory %s is below the minimum of 128Mi", spec.GetMemory())
	case sz.disk < minDisk:
		return invalid("spec.disk %s is below the minimum of 1Gi", spec.GetDisk())
	case sz.disk%quantity.GiB != 0:
		return invalid("spec.disk %s is not a whole number of Gi", spec.GetDisk())
	}
	return checkMaximums(sz, "spec", spec.GetCpu(), spec.GetMemory(), spec.GetDisk())
}

// machineShape is what a MachineSize fixes of a Container or a
// VirtualMachine, with the defaults for what it leaves empty. hasDisk is
// false for a VM that boots a Disk it names, whose size is the Disk's.
type machineShape struct {
	sizes
	hasDisk bool
}

// containerShape is c's shape; ok is false if its resources do not parse.
func containerShape(c *computev1alpha1.Container) (shape machineShape, ok bool) {
	sz, err := containerSizes(c.GetSpec().GetResources())
	return machineShape{sizes: sz, hasDisk: true}, err == nil
}

// vmShape is vm's shape, its disk the root disk made from an image; ok is
// false if its resources do not parse.
func vmShape(vm *computev1alpha1.VirtualMachine) (shape machineShape, ok bool) {
	sz, err := vmSizes(vm.GetSpec().GetResources())
	if err != nil {
		return machineShape{}, false
	}
	shape.sizes = sz
	if vm.GetSpec().GetRootDisk().GetFromImage() != nil {
		if shape.disk, err = diskBytes(imageDisk(vm).GetSpec()); err != nil {
			return machineShape{}, false
		}
		shape.hasDisk = true
	}
	return shape, true
}

// definedSize is a MachineSize read as numbers, with its spec as written for
// messages.
type definedSize struct {
	sizes
	name string
	spec *computev1alpha1.MachineSizeSpec
}

// checkMachineSize holds a machine to the sizes; see
// docs/decisions/machine-sizes.md. old is the stored machine's shape, nil on
// create; disk is the field holding the disk and its value as written, for
// the message.
func (s *Server) checkMachineSize(ctx context.Context, p auth.Principal, old *machineShape, cur machineShape, disk string) error {
	if old != nil && *old == cur {
		return nil
	}
	if s.authz.Allowed(p, authz.Permission(registry.MachineSize, authz.Apply), authz.Target{}) {
		return nil
	}
	defined, err := s.definedSizes(ctx)
	if err != nil || len(defined) == 0 {
		return err
	}
	var short *definedSize
	for i, d := range defined {
		if d.cpu != cur.cpu || d.memory != cur.memory {
			continue
		}
		if !cur.hasDisk || cur.disk >= d.disk {
			return nil
		}
		// The smallest disk is the least the machine would have to grow to.
		if short == nil || d.disk < short.disk {
			short = &defined[i]
		}
	}
	list := make([]string, len(defined))
	for i, d := range defined {
		list[i] = fmt.Sprintf("%s: %s / %s / %s", d.name, d.spec.GetCpu(), d.spec.GetMemory(), d.spec.GetDisk())
	}
	if short != nil {
		return invalid("%s is below the %s of machine size %q; the machine sizes (cpu / memory / disk) are %s",
			disk, short.spec.GetDisk(), short.name, strings.Join(list, ", "))
	}
	return invalid("spec.resources: cpu %s and memory %s are those of no machine size; the machine sizes (cpu / memory / disk) are %s",
		quantity.FormatCPU(cur.cpu), quantity.FormatMemory(cur.memory), strings.Join(list, ", "))
}

// definedSizes reads every MachineSize, in the order they are listed by. It
// is one read of the store, made only for a machine whose shape is new and
// whose writer is checked; the sizes are a handful.
func (s *Server) definedSizes(ctx context.Context) ([]definedSize, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.MachineSize.Kind})
	if err != nil {
		return nil, err
	}
	var out []definedSize
	for _, o := range objs {
		obj, err := s.backend(registry.MachineSize).decode(o)
		if err != nil {
			return nil, err
		}
		ms := obj.(*computev1alpha1.MachineSize)
		spec := ms.GetSpec()
		d := definedSize{name: ms.GetMetadata().GetName(), spec: spec}
		var cpuErr, memErr, diskErr error
		d.cpu, cpuErr = quantity.CPU(spec.GetCpu())
		d.memory, memErr = quantity.Bytes(spec.GetMemory())
		d.disk, diskErr = quantity.Bytes(spec.GetDisk())
		// Admission let in only sizes that parse.
		if cpuErr != nil || memErr != nil || diskErr != nil {
			continue
		}
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b definedSize) int {
		return cmp.Or(cmp.Compare(a.spec.GetOrder(), b.spec.GetOrder()), strings.Compare(a.name, b.name))
	})
	return out, nil
}

// unknownField names the first message under m, m itself included, that
// holds fields its schema does not have, or is empty if none does. Only the
// binary codec keeps such fields; the JSON codec drops them before a handler
// sees the request.
func unknownField(m protoreflect.Message, path string) string {
	if len(m.GetUnknown()) > 0 {
		return path
	}
	var found string
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Message() == nil {
			return true
		}
		at := path + "." + string(fd.Name())
		switch {
		case fd.IsList():
			for i := range v.List().Len() {
				if found = unknownField(v.List().Get(i).Message(), at); found != "" {
					return false
				}
			}
		case fd.IsMap():
			if fd.MapValue().Message() == nil {
				return true
			}
			v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
				found = unknownField(mv.Message(), at)
				return found == ""
			})
		default:
			found = unknownField(v.Message(), at)
		}
		return found == ""
	})
	return found
}
