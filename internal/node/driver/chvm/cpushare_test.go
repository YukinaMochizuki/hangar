package chvm

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

func TestCPUWeightFollowsMemory(t *testing.T) {
	for mib, want := range map[int64]uint64{
		128: 1, 256: 1, 512: 2, 1024: 4, 4096: 16, 16384: 64, 2560 * 1024: 10000, 4 << 20: 10000,
	} {
		if got := cpuWeight(driver.VMSpec{MemoryMiB: mib}); got != want {
			t.Errorf("cpuWeight(%d MiB) = %d, want %d", mib, got, want)
		}
	}
	// A FileShare's server, 256 MiB, serves every client of its share.
	if got := cpuWeight(driver.VMSpec{MemoryMiB: 256, FileShareServer: &driver.ShareServer{}}); got != shareServerWeight {
		t.Errorf("a share server's cpuWeight = %d, want %d", got, shareServerWeight)
	}
}

func TestCPUMask(t *testing.T) {
	for _, c := range []struct {
		cpus []int
		want []byte
	}{
		{nil, nil},
		{[]int{0}, []byte{0x01}},
		{[]int{1, 2, 3}, []byte{0x0e}},
		{[]int{8}, []byte{0x00, 0x01}},
		{[]int{1, 15, 17, 31}, []byte{0x02, 0x80, 0x02, 0x80}},
	} {
		if got := cpuMask(c.cpus); !bytes.Equal(got, c.want) {
			t.Errorf("cpuMask(%v) = %x, want %x", c.cpus, got, c.want)
		}
	}
}

func TestParseCPUList(t *testing.T) {
	for s, want := range map[string][]int{
		"0\n":           {0},
		"0-3":           {0, 1, 2, 3},
		"0-1,8,10-11\n": {0, 1, 8, 10, 11},
		"16,0":          {0, 16},
	} {
		got, err := parseCPUList(s)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("parseCPUList(%q) = %v, %v, want %v", s, got, err, want)
		}
	}
	for _, s := range []string{"", "\n", "a", "3-1", "-1", "1-", "0,,x"} {
		if got, err := parseCPUList(s); err == nil {
			t.Errorf("parseCPUList(%q) = %v, want an error", s, got)
		}
	}
}

// fakeSysfs writes a topology of cores cores with threads threads each,
// numbered as Linux numbers an EPYC's: the first thread of every core, then
// the second.
func fakeSysfs(t *testing.T, cores, threads int) string {
	t.Helper()
	root := t.TempDir()
	base := filepath.Join(root, "devices/system/cpu")
	n := cores * threads
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(base, "online"), fmt.Sprintf("0-%d\n", n-1))
	for c := range n {
		core := c % cores
		var siblings []byte
		for th := range threads {
			if th > 0 {
				siblings = append(siblings, ',')
			}
			siblings = fmt.Appendf(siblings, "%d", core+th*cores)
		}
		write(filepath.Join(base, fmt.Sprintf("cpu%d/topology/thread_siblings_list", c)), string(siblings)+"\n")
	}
	return root
}

func TestMachineCPUsKeepWholeCoresForTheHost(t *testing.T) {
	smt := fakeSysfs(t, 16, 2)
	all := make([]int, 32)
	for i := range all {
		all[i] = i
	}
	without := func(kept ...int) []int {
		return slices.DeleteFunc(slices.Clone(all), func(c int) bool { return slices.Contains(kept, c) })
	}
	for _, c := range []struct {
		sysfs    string
		reserved int
		want     []int
	}{
		{smt, 0, all},
		{smt, 2, without(0, 16)},
		{smt, 4, without(0, 16, 1, 17)},
		// Half a core: the host has cpu0 and machines its sibling.
		{smt, 1, without(0)},
		{smt, 3, without(0, 16, 1)},
		{fakeSysfs(t, 8, 1), 2, []int{2, 3, 4, 5, 6, 7}},
	} {
		got, err := MachineCPUs(c.sysfs, c.reserved)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("MachineCPUs(%d reserved) = %v, %v, want %v", c.reserved, got, err, c.want)
		}
	}
	if got, err := MachineCPUs(smt, 32); err == nil {
		t.Errorf("keeping every CPU for the host: %v, want an error", got)
	}
	if got, err := MachineCPUs(t.TempDir(), 2); err == nil {
		t.Errorf("a host with no topology: %v, want an error", got)
	}
}

// TestTheVMsShareTheirSlice: New sets the machines' slice's weight and CPUs,
// and every VM's unit runs in it, weighted by its memory.
func TestTheVMsShareTheirSlice(t *testing.T) {
	s := newFakeSystemd()
	h := newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) { c.MachineCPUs = []int{1, 2, 3} })
	slice := s.props[machinesSlice]
	if got := prop(slice, "CPUWeight"); got != uint64(machinesSliceWeight) {
		t.Errorf("the slice's CPUWeight=%v, want %d", got, machinesSliceWeight)
	}
	if got, _ := prop(slice, "AllowedCPUs").([]byte); !bytes.Equal(got, []byte{0x0e}) {
		t.Errorf("the slice's AllowedCPUs=%x, want 0e", got)
	}
	spec := testSpec("m1", driver.RestartAlways)
	spec.MemoryMiB = 4096
	if err := h.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	unit := s.props["hangar-vm-m1.service"]
	if got := prop(unit, "Slice"); got != machinesSlice {
		t.Errorf("Slice=%v, want %s", got, machinesSlice)
	}
	if got := prop(unit, "CPUWeight"); got != uint64(16) {
		t.Errorf("CPUWeight=%v for 4096 MiB, want 16", got)
	}

	// No CPUs given: every online one, by name, so a slice an earlier run
	// limited is let go, which an empty set would not do at runtime.
	s = newFakeSystemd()
	newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) { c.sysfs = fakeSysfs(t, 4, 1) })
	if got, _ := prop(s.props[machinesSlice], "AllowedCPUs").([]byte); !bytes.Equal(got, []byte{0x0f}) {
		t.Errorf("AllowedCPUs=%x with no CPUs given, want 0f, the four online", got)
	}
}
