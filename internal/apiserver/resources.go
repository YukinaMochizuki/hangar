package apiserver

import (
	"cmp"
	"fmt"
	"math"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
)

// Sizes a container gets when its spec leaves them out.
const (
	defaultCPU    = "1"
	defaultMemory = "512Mi"
	defaultDisk   = "10Gi"
)

// The smallest sizes a container may ask for.
const (
	minMemory = 128 * quantity.MiB
	minDisk   = 1 * quantity.GiB
)

// The largest sizes a machine may have, which also keep every canonical form
// and every node's sum of them within range. maxCPU is the limit with KVM on
// x86_64 (MAX_SUPPORTED_CPUS in vmm/src/config.rs) of the Cloud Hypervisor
// pinned in deploy/host/versions.env; check it again when that changes.
// Memory and disk are far past any host Hangar runs on.
const (
	maxCPU    = 8192
	maxMemory = 4 << 40
	maxDisk   = 64 << 40
)

// sizes is a Resources read as numbers.
type sizes struct {
	cpu          int32
	memory, disk uint64
}

// containerSizes reads a container's resources, with the defaults for what
// it leaves empty.
func containerSizes(r *computev1alpha1.Resources) (sizes, error) {
	var s sizes
	var err error
	if s.cpu, err = quantity.CPU(cmp.Or(r.GetCpu(), defaultCPU)); err != nil {
		return s, fmt.Errorf("spec.resources.cpu: %w", err)
	}
	if s.memory, err = quantity.Bytes(cmp.Or(r.GetMemory(), defaultMemory)); err != nil {
		return s, fmt.Errorf("spec.resources.memory: %w", err)
	}
	if s.disk, err = quantity.Bytes(cmp.Or(r.GetDisk(), defaultDisk)); err != nil {
		return s, fmt.Errorf("spec.resources.disk: %w", err)
	}
	return s, nil
}

// canonical is s as a machine carries it.
func (s sizes) canonical() *computev1alpha1.Resources {
	return &computev1alpha1.Resources{
		Cpu:    quantity.FormatCPU(s.cpu),
		Memory: quantity.FormatMemory(s.memory),
		Disk:   quantity.FormatDisk(s.disk),
	}
}

// held is the memory and disk a machine holds on its node.
type held struct {
	memory, disk uint64
}

// heldBy reads a machine's resources; an empty quantity holds nothing.
func heldBy(r *computev1alpha1.Resources) (held, error) {
	var h held
	var err error
	if h.memory, err = optionalBytes(r.GetMemory()); err != nil {
		return held{}, fmt.Errorf("memory: %w", err)
	}
	if h.disk, err = optionalBytes(r.GetDisk()); err != nil {
		return held{}, fmt.Errorf("disk: %w", err)
	}
	return h, nil
}

func optionalBytes(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	return quantity.Bytes(s)
}

// nodeRoom is what a node offers machines: its allocatable amounts, or its
// capacity where it reports no allocatable one.
type nodeRoom struct {
	memory uint64
	disk   uint64
	// hasDisk is false for a node that reports no disk, whose disk is not
	// checked.
	hasDisk bool
}

// roomOf reads a node's room. A quantity that does not parse is an error;
// the scheduler then treats the node as full.
func roomOf(n *computev1alpha1.Node) (nodeRoom, error) {
	st := n.GetStatus()
	var room nodeRoom
	var err error
	if room.memory, err = optionalBytes(cmp.Or(st.GetAllocatable().GetMemory(), st.GetCapacity().GetMemory())); err != nil {
		return nodeRoom{}, fmt.Errorf("node %s memory: %w", n.GetMetadata().GetName(), err)
	}
	disk := cmp.Or(st.GetAllocatable().GetDisk(), st.GetCapacity().GetDisk())
	if room.hasDisk = disk != ""; room.hasDisk {
		if room.disk, err = quantity.Bytes(disk); err != nil {
			return nodeRoom{}, fmt.Errorf("node %s disk: %w", n.GetMetadata().GetName(), err)
		}
	}
	return room, nil
}

// wholeGi is a disk size in its canonical unit, whole Gi rounded up: the
// size the disk actually has.
func wholeGi(bytes uint64) uint64 {
	gi := bytes / quantity.GiB
	if bytes%quantity.GiB != 0 {
		gi++
	}
	return gi
}

// add is a + b, held at the largest uint64 rather than wrapping around.
func add(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

// free is what is left of room once used is taken, never below nothing.
func free(room, used uint64) uint64 {
	if used >= room {
		return 0
	}
	return room - used
}
