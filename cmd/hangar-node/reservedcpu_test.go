package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestReservedCPUIsReadAsTheNodeReadsIt: --reserved-cpu takes any quantity
// of whole CPUs, as the node's allocatable cpu does, and refuses the rest
// before the agent starts.
func TestReservedCPUIsReadAsTheNodeReadsIt(t *testing.T) {
	for in, want := range map[string]int{"": 0, "0": 0, "2": 2, "2000m": 2, "1e1": 10} {
		if got, err := reservedCount(in); err != nil || got != want {
			t.Errorf("--reserved-cpu %q = %d, %v, want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"1.5", "500m", "-1", "two", "1Gi"} {
		if got, err := reservedCount(in); err == nil || !strings.Contains(err.Error(), "--reserved-cpu") {
			t.Errorf("--reserved-cpu %q = %d, %v, want a refusal naming the flag", in, got, err)
		}
	}
}

// TestMachineCPUsKeepTheReservedForTheHost: the VMs get every online CPU
// but those kept, and none kept leaves them every CPU; keeping them all is
// refused.
func TestMachineCPUsKeepTheReservedForTheHost(t *testing.T) {
	sysfs := t.TempDir()
	base := filepath.Join(sysfs, "devices/system/cpu")
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Four CPUs, two cores of two threads: 0 and 2 share one.
	write("online", "0-3\n")
	for cpu, siblings := range []string{"0,2", "1,3", "0,2", "1,3"} {
		write(fmt.Sprintf("cpu%d/topology/thread_siblings_list", cpu), siblings+"\n")
	}
	for reserved, want := range map[string][]int{"": {0, 1, 2, 3}, "2": {1, 3}} {
		if got, err := machineCPUs(sysfs, reserved); err != nil || !slices.Equal(got, want) {
			t.Errorf("--reserved-cpu %q: %v, %v, want %v", reserved, got, err, want)
		}
	}
	if got, err := machineCPUs(sysfs, "4"); err == nil {
		t.Errorf("--reserved-cpu 4 of 4 CPUs: %v, want a refusal", got)
	}
}
