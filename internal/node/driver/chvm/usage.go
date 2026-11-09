package chvm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// Usage reads the counters of the cgroup of the VM's unit, in the machines'
// slice: its CPU time and the memory it holds.
func (h *Hypervisor) Usage(_ context.Context, machineUID string) (driver.VMUsage, error) {
	if err := checkUID(machineUID); err != nil {
		return driver.VMUsage{}, err
	}
	dir := filepath.Join(h.cfg.cgroupRoot, machinesSlice, unitName(machineUID))
	u, err := readCgroupUsage(dir)
	if errors.Is(err, os.ErrNotExist) {
		return driver.VMUsage{}, fmt.Errorf("machine %s has no running VM: %w", machineUID, err)
	}
	return u, err
}

// readCgroupUsage reads cpu.stat's usage_usec and memory.current in the
// cgroup directory dir.
func readCgroupUsage(dir string) (driver.VMUsage, error) {
	stat, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return driver.VMUsage{}, err
	}
	usec, err := cpuUsageUsec(stat)
	if err != nil {
		return driver.VMUsage{}, fmt.Errorf("%s/cpu.stat: %w", dir, err)
	}
	cur, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return driver.VMUsage{}, err
	}
	mem, err := strconv.ParseUint(strings.TrimSpace(string(cur)), 10, 64)
	if err != nil {
		return driver.VMUsage{}, fmt.Errorf("%s/memory.current is %q, not a number of bytes", dir, strings.TrimSpace(string(cur)))
	}
	return driver.VMUsage{CPU: time.Duration(usec) * time.Microsecond, Memory: mem}, nil
}

// maxUsageUsec is the most microseconds a time.Duration holds.
const maxUsageUsec = uint64(1<<63-1) / uint64(time.Microsecond)

// cpuUsageUsec is the usage_usec line of a cgroup's cpu.stat.
func cpuUsageUsec(stat []byte) (uint64, error) {
	sc := bufio.NewScanner(bytes.NewReader(stat))
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), " ")
		if !ok || name != "usage_usec" {
			continue
		}
		usec, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil || usec > maxUsageUsec {
			return 0, fmt.Errorf("usage_usec is %q, not a number of microseconds", value)
		}
		return usec, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("no usage_usec")
}
