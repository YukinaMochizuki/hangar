package node

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
)

// hostResources reads what this host has: its CPUs and its memory.
func hostResources() (*computev1alpha1.NodeResources, error) {
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	mib, err := memTotalMiB(meminfo)
	if err != nil {
		return nil, err
	}
	return &computev1alpha1.NodeResources{Vcpus: int32(runtime.NumCPU()), MemoryMib: mib}, nil
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
