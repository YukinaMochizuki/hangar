package chvm

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	sd "github.com/coreos/go-systemd/v22/dbus"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// machinesSlice is the slice every VM's unit runs in, so the VMs share the
// CPUs it is given among themselves alone, by their memory, never with the
// host's services; docs/decisions/node-agent.md says how the CPUs are
// shared. It has no dash: systemd reads hangar-vms.slice as a slice inside
// hangar.slice, whose weight would count against nothing but its siblings,
// and no longer against system.slice's.
const machinesSlice = "hangar.slice"

// machinesSliceWeight is the slice's CPUWeight, against the 100 of
// system.slice and user.slice: under full load the VMs take most of the CPUs
// they may use, and the host's services still get some of them.
const machinesSliceWeight = 1000

// machinesSliceDescription is the slice's description, as systemctl shows it.
const machinesSliceDescription = "Hangar's virtual machines"

// shareServerWeight is a FileShare server's CPUWeight, that of a 25 GiB
// machine: a server of 256 MiB serves every client of its share, which
// would wait on it at the weight of its memory.
const shareServerWeight = 100

// cpuWeight is a VM's CPUWeight: one per 256 MiB of its memory, within what
// systemd takes, so under full load each VM's share of the slice's CPUs
// follows its memory; shareServerWeight for a FileShare's server.
func cpuWeight(spec driver.VMSpec) uint64 {
	if spec.FileShareServer != nil {
		return shareServerWeight
	}
	return uint64(min(max(spec.MemoryMiB/256, 1), 10000))
}

// sliceProps are the machines' slice's properties: its weight, and the CPUs
// it may use. cpus is never empty: systemd takes an empty AllowedCPUs as no
// limit, but at runtime it then leaves the cgroup's cpuset as an earlier
// run of the agent set it, so every CPU is given by name.
func sliceProps(cpus []int) []sd.Property {
	return []sd.Property{
		property("CPUWeight", uint64(machinesSliceWeight)),
		property("AllowedCPUs", cpuMask(cpus)),
	}
}

// cpuMask is cpus as systemd takes a CPU set over D-Bus: bit j of byte i is
// CPU 8i+j.
func cpuMask(cpus []int) []byte {
	var mask []byte
	for _, c := range cpus {
		for len(mask) <= c/8 {
			mask = append(mask, 0)
		}
		mask[c/8] |= 1 << (c % 8)
	}
	return mask
}

// onlineCPUs reads the host's online CPUs under sysfs (/sys normally).
func onlineCPUs(sysfs string) ([]int, error) {
	b, err := os.ReadFile(filepath.Join(sysfs, "devices/system/cpu/online"))
	if err != nil {
		return nil, err
	}
	online, err := parseCPUList(string(b))
	if err != nil {
		return nil, fmt.Errorf("online CPUs: %w", err)
	}
	return online, nil
}

// MachineCPUs reads the host's CPU topology under sysfs (/sys normally) and
// says which CPUs the VMs may use once reserved CPUs are kept for the host:
// the first taken whole core by whole core, a core's SMT siblings together,
// so no vCPU shares a core with the host's services, unless reserved is odd
// on a host with two threads a core. Zero keeps none back.
func MachineCPUs(sysfs string, reserved int) ([]int, error) {
	online, err := onlineCPUs(sysfs)
	if err != nil {
		return nil, err
	}
	if reserved <= 0 {
		return online, nil
	}
	base := filepath.Join(sysfs, "devices/system/cpu")
	if reserved >= len(online) {
		return nil, fmt.Errorf("%d CPUs kept for the host leave none of the %d online for machines", reserved, len(online))
	}
	isOnline := map[int]bool{}
	for _, c := range online {
		isOnline[c] = true
	}
	kept := map[int]bool{}
	for _, c := range online {
		if len(kept) == reserved {
			break
		}
		if kept[c] {
			continue
		}
		b, err := os.ReadFile(filepath.Join(base, fmt.Sprintf("cpu%d/topology/thread_siblings_list", c)))
		if err != nil {
			return nil, err
		}
		siblings, err := parseCPUList(string(b))
		if err != nil {
			return nil, fmt.Errorf("CPU %d's siblings: %w", c, err)
		}
		if !slices.Contains(siblings, c) {
			siblings = append(siblings, c)
		}
		slices.Sort(siblings)
		for _, s := range siblings {
			if len(kept) < reserved && isOnline[s] {
				kept[s] = true
			}
		}
	}
	var out []int
	for _, c := range online {
		if !kept[c] {
			out = append(out, c)
		}
	}
	return out, nil
}

// parseCPUList reads the kernel's CPU list format, such as 0-3,8,10-11.
func parseCPUList(s string) ([]int, error) {
	var out []int
	for part := range strings.SplitSeq(strings.TrimSpace(s), ",") {
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("%q is not a CPU list", s)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return nil, fmt.Errorf("%q is not a CPU list", s)
			}
		}
		if a < 0 {
			return nil, fmt.Errorf("%q is not a CPU list", s)
		}
		for c := a; c <= b; c++ {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%q lists no CPU", s)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
