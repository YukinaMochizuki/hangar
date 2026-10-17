package node

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
)

// epyc7313 is the start of /proc/cpuinfo on an EPYC 7313 host, cut short.
const epyc7313 = `processor	: 0
vendor_id	: AuthenticAMD
cpu family	: 25
model		: 1
model name	: AMD EPYC 7313 16-Core Processor
stepping	: 1

processor	: 1
vendor_id	: AuthenticAMD
cpu family	: 25
model		: 1
model name	: Something else
`

func TestReadCPU(t *testing.T) {
	for _, tt := range []struct {
		what, cpuinfo    string
		name, generation string
	}{
		{"an EPYC 7313", epyc7313, "AMD EPYC 7313", "Zen 3 (Milan)"},
		{"an EPYC 9124", "vendor_id\t: AuthenticAMD\ncpu family\t: 25\nmodel\t\t: 17\nmodel name\t: AMD EPYC 9124 16-Core Processor\n", "AMD EPYC 9124", "Zen 4 (Genoa)"},
		{"an EPYC 7302P", "vendor_id\t: AuthenticAMD\ncpu family\t: 23\nmodel\t\t: 49\nmodel name\t: AMD EPYC 7302P 16-Core Processor\n", "AMD EPYC 7302P", "Zen 2 (Rome)"},
		// A desktop part sharing Milan's family and model range.
		{"a Threadripper", "vendor_id\t: AuthenticAMD\ncpu family\t: 25\nmodel\t\t: 1\nmodel name\t: AMD Ryzen Threadripper PRO 5995WX 64-Cores\n", "AMD Ryzen Threadripper PRO 5995WX", ""},
		{"a core count without Processor", "vendor_id\t: AuthenticAMD\ncpu family\t: 25\nmodel\t\t: 33\nmodel name\t: AMD Ryzen 9 5950X 16-Core\n", "AMD Ryzen 9 5950X", ""},
		{"an EPYC the table does not know", "vendor_id\t: AuthenticAMD\ncpu family\t: 25\nmodel\t\t: 160\nmodel name\t: AMD EPYC 9754 128-Core Processor\n", "AMD EPYC 9754", ""},
		{"a Xeon", "vendor_id\t: GenuineIntel\ncpu family\t: 6\nmodel\t\t: 106\nmodel name\t: Intel(R) Xeon(R) Gold  6338 CPU @ 2.00GHz\n", "Intel(R) Xeon(R) Gold 6338 CPU @ 2.00GHz", ""},
		{"an arm64 host", "processor\t: 0\nBogoMIPS\t: 50.00\nCPU implementer\t: 0x41\n", "", ""},
	} {
		c := readCPU([]byte(tt.cpuinfo))
		if c.name != tt.name || c.generation() != tt.generation {
			t.Errorf("%s: model %q, generation %q; want %q, %q", tt.what, c.name, c.generation(), tt.name, tt.generation)
		}
	}
}

// fakeDisk is a block device in a made-up sysfs: dev names the device
// behind it, empty for none.
type fakeDisk struct {
	dev                   string
	rotational, removable bool
}

// sysBlock makes a /sys/block under a temporary directory: each disk a link
// to its directory under devices, its device a link to dev there, as sysfs
// has them.
func sysBlock(t *testing.T, disks map[string]fakeDisk) string {
	t.Helper()
	root := t.TempDir()
	block := filepath.Join(root, "block")
	mustMkdir(t, block)
	flag := func(b bool) []byte {
		if b {
			return []byte("1\n")
		}
		return []byte("0\n")
	}
	for name, d := range disks {
		dir := filepath.Join(root, "devices", "virtual", "block", name)
		mustMkdir(t, filepath.Join(dir, "queue"))
		if d.dev != "" {
			dev := filepath.Join(root, "devices", d.dev)
			mustMkdir(t, dev)
			if err := os.Symlink(dev, filepath.Join(dir, "device")); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "queue", "rotational"), flag(d.rotational), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "removable"), flag(d.removable), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dir, filepath.Join(block, name)); err != nil {
			t.Fatal(err)
		}
	}
	return block
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestStorageKinds(t *testing.T) {
	type disk = fakeDisk
	for _, tt := range []struct {
		what  string
		disks map[string]disk
		want  string
	}{
		{"two NVMe disks, one with two namespaces, beside what is not a disk", map[string]disk{
			"nvme0n1": {dev: "nvme0"}, "nvme0n2": {dev: "nvme0"}, "nvme1n1": {dev: "nvme1"},
			"loop0": {}, "md127": {}, "zd0": {}, "dm-0": {},
		}, "2 × NVMe SSD"},
		{"mixed, the most first", map[string]disk{
			"sda": {dev: "sda", rotational: true}, "sdb": {dev: "sdb", rotational: true}, "sdc": {dev: "sdc"},
			"sr0": {dev: "sr0", removable: true}, "sdd": {dev: "sdd", removable: true},
		}, "2 × HDD, 1 × SSD"},
		{"a virtual machine's", map[string]disk{"vda": {dev: "virtio1"}, "vdb": {dev: "virtio2", rotational: true}}, "2 × virtual disk"},
		{"none", map[string]disk{"loop0": {}}, ""},
	} {
		got, err := storageKinds(sysBlock(t, tt.disks))
		if err != nil {
			t.Fatalf("%s: %v", tt.what, err)
		}
		if got != tt.want {
			t.Errorf("%s: %q, want %q", tt.what, got, tt.want)
		}
	}
}

func TestPlacementMerge(t *testing.T) {
	hw := &computev1alpha1.Placement{CpuModel: "AMD EPYC 7313", CpuGeneration: "Zen 3 (Milan)", Storage: "2 × NVMe SSD",
		Memory: "DDR4-3200 ECC", Network: "1 × 10 GbE",
		// Never from the host: these are the configuration's alone.
		Region: "stale", Site: "stale", Chassis: "stale"}
	conf := &computev1alpha1.Placement{Region: "ap-southeast", Site: "SGP02", Chassis: "generic-1u-2x-u2", CpuGeneration: "Zen 3",
		Network: "2 × 10 GbE"}
	want := &computev1alpha1.Placement{Region: "ap-southeast", Site: "SGP02", Chassis: "generic-1u-2x-u2",
		CpuModel: "AMD EPYC 7313", CpuGeneration: "Zen 3", Storage: "2 × NVMe SSD",
		Memory: "DDR4-3200 ECC", Network: "2 × 10 GbE"}
	if got := placement(hw, conf); !proto.Equal(got, want) {
		t.Errorf("placement %v, want %v", got, want)
	}
	if got := placement(nil, nil); got != nil {
		t.Errorf("nothing known is placement %v, want none", got)
	}
}

func TestConfiguredPlacement(t *testing.T) {
	conf := &computev1alpha1.Placement{Region: "ap-southeast", Site: "SGP\x0002", Chassis: "Generic 1U", CpuGeneration: strings.Repeat("z", 65)}
	got := configuredPlacement(conf, slog.New(slog.DiscardHandler))
	if want := (&computev1alpha1.Placement{Region: "ap-southeast"}); !proto.Equal(got, want) {
		t.Errorf("configured placement %v, want %v", got, want)
	}
	if conf.GetChassis() != "Generic 1U" {
		t.Errorf("the configuration given was changed: %v", conf)
	}
	if got := configuredPlacement(nil, slog.New(slog.DiscardHandler)); proto.Size(got) != 0 {
		t.Errorf("no configuration is placement %v", got)
	}
}
