package chvm

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// TestUsageReadsTheUnitsCgroup: a VM's usage is its unit's cgroup's, in the
// machines' slice or its pool's, read as cgroup v2 writes it; a VM with no
// cgroup has none, and a file not as the kernel writes it is an error.
func TestUsageReadsTheUnitsCgroup(t *testing.T) {
	root := t.TempDir()
	h := newDriverWith(t, shortDir(t), newFakeSystemd(), &fakeGuest{}, func(c *Config) { c.cgroupRoot = root })
	ctx := t.Context()
	pooled := testSpec("m3", driver.RestartAlways)
	pooled.MemoryPool = &driver.MemoryPool{Name: "acme-corp", SizeMiB: 4096}
	for _, spec := range []driver.VMSpec{testSpec("m1", driver.RestartAlways), pooled} {
		if err := h.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
	}
	unit := filepath.Join(root, "hangar.slice", "hangar-vm-m1.service")
	write := func(name, content string) {
		t.Helper()
		if err := os.MkdirAll(unit, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(unit, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if u, err := h.Usage(ctx, "m1"); err == nil {
		t.Errorf("a VM with no cgroup used %+v", u)
	}

	write("cpu.stat", "usage_usec 81234567\nuser_usec 60000000\nsystem_usec 21234567\nnr_periods 0\n")
	write("memory.current", "536870912\n")
	u, err := h.Usage(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if want := (driver.VMUsage{CPU: 81234567 * time.Microsecond, Memory: 512 << 20}); u != want {
		t.Errorf("used %+v, want %+v", u, want)
	}
	// A pool's VM's unit is in its pool's slice, its dashes underscores.
	unit = filepath.Join(root, "hangar.slice", "hangar-pool_acme_corp.slice", "hangar-vm-m3.service")
	write("cpu.stat", "usage_usec 5\n")
	write("memory.current", "1048576\n")
	if u, err := h.Usage(ctx, "m3"); err != nil || u != (driver.VMUsage{CPU: 5 * time.Microsecond, Memory: 1 << 20}) {
		t.Errorf("the pool's VM used %+v (%v), want its pool's slice's unit's", u, err)
	}
	unit = filepath.Join(root, "hangar.slice", "hangar-vm-m1.service")
	// Another VM's cgroup is not this one's.
	if u, err := h.Usage(ctx, "m2"); err == nil {
		t.Errorf("m2, with no cgroup, used %+v", u)
	}
	if _, err := h.Usage(ctx, "../m1"); err == nil {
		t.Error("read the usage of a uid that is a path")
	}

	for name, files := range map[string][2]string{
		"no usage_usec":          {"user_usec 1\n", "1\n"},
		"usage_usec not a count": {"usage_usec lots\n", "1\n"},
		"overflowing usage_usec": {"usage_usec 18446744073709551615\n", "1\n"},
		"memory.current max":     {"usage_usec 1\n", "max\n"},
	} {
		write("cpu.stat", files[0])
		write("memory.current", files[1])
		if u, err := h.Usage(ctx, "m1"); err == nil {
			t.Errorf("%s: used %+v", name, u)
		}
	}
}

// TestVMsSayTheirShape: a VM listed says its vCPUs and what it runs, as it
// was started.
func TestVMsSayTheirShape(t *testing.T) {
	s, g := newFakeSystemd(), &fakeGuest{}
	h := newDriver(t, shortDir(t), s, g)
	spec := testSpec("m1", driver.RestartAlways)
	if err := h.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	vm, ok := listed(t, h, "m1")
	if !ok || vm.VCPUs != spec.VCPUs || vm.Firmware || vm.FileShareServer {
		t.Errorf("listed %+v (%t), started with %d vCPUs as a container", vm, ok, spec.VCPUs)
	}
}
