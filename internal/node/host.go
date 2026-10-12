package node

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// hostResources reads what this host has: its CPUs and its memory. Its disk
// is the storage driver's to report.
func hostResources() (*computev1alpha1.NodeResources, error) {
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	mib, err := memTotalMiB(meminfo)
	if err != nil {
		return nil, err
	}
	return &computev1alpha1.NodeResources{
		Cpu:    quantity.FormatCPU(int32(runtime.NumCPU())),
		Memory: quantity.FormatMemory(uint64(mib) * quantity.MiB),
	}, nil
}

// memTotalMiB reads MemTotal from the contents of /proc/meminfo.
func memTotalMiB(meminfo []byte) (int64, error) {
	s := bufio.NewScanner(bytes.NewReader(meminfo))
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) == 3 && fields[0] == "MemTotal:" && fields[2] == "kB" {
			kib, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("MemTotal %q: %w", fields[1], err)
			}
			return kib / 1024, nil
		}
	}
	return 0, fmt.Errorf("no MemTotal in kB in /proc/meminfo")
}

// amounts is a NodeResources read as numbers; has says which fields were
// given.
type amounts struct {
	cpu, memory, disk          uint64
	hasCPU, hasMemory, hasDisk bool
}

// readAmounts parses each quantity r gives. CPU must be a whole count here
// too, but zero is allowed: a host may keep none back.
func readAmounts(r *computev1alpha1.NodeResources) (amounts, error) {
	var a amounts
	var err error
	if a.hasCPU = r.GetCpu() != ""; a.hasCPU {
		if a.cpu, err = cpuCount(r.GetCpu()); err != nil {
			return amounts{}, fmt.Errorf("cpu: %w", err)
		}
	}
	if a.hasMemory = r.GetMemory() != ""; a.hasMemory {
		if a.memory, err = quantity.Bytes(r.GetMemory()); err != nil {
			return amounts{}, fmt.Errorf("memory: %w", err)
		}
	}
	if a.hasDisk = r.GetDisk() != ""; a.hasDisk {
		if a.disk, err = quantity.Bytes(r.GetDisk()); err != nil {
			return amounts{}, fmt.Errorf("disk: %w", err)
		}
	}
	return a, nil
}

// allocatable is what the host has less what it keeps for itself, never
// below nothing, rounded down to the canonical units. What the host does not
// report stays empty.
func allocatable(capacity, reserved *computev1alpha1.NodeResources) (*computev1alpha1.NodeResources, error) {
	has, err := readAmounts(capacity)
	if err != nil {
		return nil, fmt.Errorf("capacity %w", err)
	}
	keeps, err := readAmounts(reserved)
	if err != nil {
		return nil, fmt.Errorf("reserved %w", err)
	}
	out := &computev1alpha1.NodeResources{}
	if has.hasCPU {
		out.Cpu = strconv.FormatUint(less(has.cpu, keeps.cpu), 10)
	}
	if has.hasMemory {
		out.Memory = quantity.FormatMemory(less(has.memory, keeps.memory) / quantity.MiB * quantity.MiB)
	}
	if has.hasDisk {
		out.Disk = quantity.FormatDisk(less(has.disk, keeps.disk) / quantity.GiB * quantity.GiB)
	}
	return out, nil
}

func cpuCount(s string) (uint64, error) {
	v, err := quantity.Parse(s)
	if err != nil {
		return 0, err
	}
	if !v.IsInt() || v.Sign() < 0 || !v.Num().IsUint64() {
		return 0, fmt.Errorf("%q is not a whole number of CPUs", s)
	}
	return v.Num().Uint64(), nil
}

func less(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

// diskSize reads the size of a machine's root disk.
func diskSize(r *computev1alpha1.Resources) (int64, error) {
	size, err := quantity.Bytes(r.GetDisk())
	if err != nil {
		return 0, fmt.Errorf("disk: %w", err)
	}
	if size == 0 || size > math.MaxInt64 {
		return 0, fmt.Errorf("disk: %q is not a size a root disk can have", r.GetDisk())
	}
	return int64(size), nil
}

// diskCapacity is the storage's size as a node reports it, in whole Gi
// rounded down, so it never claims more than there is.
func diskCapacity(c driver.Capacity) string {
	return quantity.FormatDisk(uint64(max(c.Total, 0)) / quantity.GiB * quantity.GiB)
}

// vmSize reads the vCPUs and memory a machine's VM is started with. The
// apiserver writes them canonical; memory is rounded up to whole MiB all the
// same.
func vmSize(r *computev1alpha1.Resources) (int32, int64, error) {
	vcpus, err := quantity.CPU(r.GetCpu())
	if err != nil {
		return 0, 0, err
	}
	memory, err := quantity.Bytes(r.GetMemory())
	if err != nil {
		return 0, 0, fmt.Errorf("memory: %w", err)
	}
	// At most 2^44 MiB, so it fits an int64.
	mib := memory / quantity.MiB
	if memory%quantity.MiB != 0 {
		mib++
	}
	return vcpus, int64(mib), nil
}
