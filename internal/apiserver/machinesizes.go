package apiserver

import (
	"context"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/auth"
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
