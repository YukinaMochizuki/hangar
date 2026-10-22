package apiserver

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
)

// maxDisplayName is the longest display name a size may have, enough for a
// label such as "XL" or "Large, high memory".
const maxDisplayName = 63

// validateMachineSize checks a size as a container's resources are checked,
// though a size has no defaults: cpu, memory and disk are all required, and
// its disk is whole Gi, the unit a machine's disk is held in. A size is a
// preset of a shape, so it keeps to the shape rules a tenant's machine does.
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
	if sz.disk%quantity.GiB != 0 {
		return invalid("spec.disk %s is not a whole number of Gi", spec.GetDisk())
	}
	if err := checkMaximums(sz, "spec", spec.GetCpu(), spec.GetMemory(), spec.GetDisk()); err != nil {
		return err
	}
	return checkShape(machineShape{sizes: sz, hasDisk: true}, "spec", "spec.disk "+spec.GetDisk())
}

// The shape rules of docs/decisions/machine-sizes.md: memory in steps of
// shapeMemoryStep, at least one step; between minMemoryPerCPU and
// maxMemoryPerCPU of memory per vCPU, though one vCPU is always allowed; and
// a root disk of at least minShapeDisk.
const (
	shapeMemoryStep = 256 * quantity.MiB
	minMemoryPerCPU = 512 * quantity.MiB
	maxMemoryPerCPU = 8 * quantity.GiB
	minShapeDisk    = 2 * quantity.GiB
)

// cpuRange is the fewest and most vCPUs a machine of memory may have.
func cpuRange(memory uint64) (lo, hi uint64) {
	lo = max(1, (memory+maxMemoryPerCPU-1)/maxMemoryPerCPU)
	hi = max(1, memory/minMemoryPerCPU)
	return lo, hi
}

// checkShape refuses a shape outside the rules; at is the field holding cpu
// and memory, and disk the field holding the disk and its value as written,
// for the message.
func checkShape(cur machineShape, at, disk string) error {
	memory := quantity.FormatMemory(cur.memory)
	if cur.memory%quantity.GiB == 0 {
		memory = quantity.FormatDisk(cur.memory)
	}
	if cur.memory < shapeMemoryStep || cur.memory%shapeMemoryStep != 0 {
		return invalid("%s.memory %s is not a whole number of 256Mi steps, at least one", at, memory)
	}
	if lo, hi := cpuRange(cur.memory); uint64(cur.cpu) < lo || uint64(cur.cpu) > hi {
		return invalid("%s: a machine of %s of memory has %s, not %d; each vCPU goes with 512Mi to 8Gi of memory",
			at, memory, cpuSpan(lo, hi), cur.cpu)
	}
	if cur.hasDisk && cur.disk < minShapeDisk {
		return invalid("%s is below the minimum of 2Gi", disk)
	}
	return nil
}

// cpuSpan says lo to hi vCPUs, or lo alone where they are one.
func cpuSpan(lo, hi uint64) string {
	switch {
	case lo != hi:
		return fmt.Sprintf("%d to %d vCPUs", lo, hi)
	case lo == 1:
		return "1 vCPU"
	default:
		return fmt.Sprintf("%d vCPUs", lo)
	}
}

// machineShape is what the shape rules check of a Container or a
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

// checkMachineShape holds a machine to the shape rules; see
// docs/decisions/machine-sizes.md. old is the stored machine's shape, nil on
// create; disk is the field holding the disk and its value as written, for
// the message. Only a new shape is checked, and only for a caller who may
// not define sizes.
func (s *Server) checkMachineShape(p auth.Principal, old *machineShape, cur machineShape, disk string) error {
	if old != nil && *old == cur {
		return nil
	}
	if s.authz.Allowed(p, authz.Permission(registry.MachineSize, authz.Apply), authz.Target{}) {
		return nil
	}
	return checkShape(cur, "spec.resources", disk)
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
