package driver_test

import (
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// TestAVMWithoutDisksKeepsItsHash: a VM a node started before VMSpec had
// disks hashes as it did, so the agent does not restart it on upgrade.
func TestAVMWithoutDisksKeepsItsHash(t *testing.T) {
	// VMSpec as it was before it had disks.
	type before struct {
		MachineUID  string
		Machine     string
		VCPUs       int32
		MemoryMiB   int64
		Root        driver.Disk
		Interfaces  []driver.Interface
		Nameservers []string
		Search      []string
		Run         driver.RunSpec
		Restart     driver.RestartPolicy
	}
	nic := driver.Interface{MachineUID: "m1", Name: "eth0", Address: netip.MustParsePrefix("10.0.0.2/24"), Device: "hn1"}
	spec := driver.VMSpec{
		MachineUID: "m1", Machine: "p/m1", VCPUs: 2, MemoryMiB: 512, Root: driver.Disk{MachineUID: "m1", Path: "/dev/zvol/r"},
		Interfaces: []driver.Interface{nic}, Run: driver.RunSpec{Command: []string{"true"}}, Restart: driver.RestartNever,
	}
	now, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	was, err := json.Marshal(before{spec.MachineUID, spec.Machine, spec.VCPUs, spec.MemoryMiB, spec.Root, spec.Interfaces, nil, nil, spec.Run, spec.Restart})
	if err != nil {
		t.Fatal(err)
	}
	if string(now) != string(was) {
		t.Errorf("a spec without disks encodes as\n%s\nnot as before\n%s", now, was)
	}
	spec.Disks = []driver.Volume{{DiskUID: "d1", Path: "/dev/zvol/d", Size: 1 << 30}}
	withDisk := spec.Hash()
	spec.Disks[0].Size = 2 << 30
	if spec.Hash() == withDisk {
		t.Error("a grown volume leaves the hash as it was: the VM would not boot to see it")
	}
	grown := spec.Hash()
	spec.Mounts = []driver.Mount{{Disk: 0, Name: "data", Target: "/data"}}
	mounted := spec.Hash()
	spec.Mounts[0].Target = "/srv"
	if mounted == grown || spec.Hash() == mounted {
		t.Error("mounting a volume, or moving it, leaves the hash as it was: the guest would not mount it")
	}
}

// TestAVMStartedBeforeIPv6ChangedLiveKeepsItsHash: Hash leaves out the
// interfaces' IPv6 addresses, which change while a VM runs; LegacyHash is
// the hash nodes gave before, which counted them, so a VM such a node
// started is not restarted on upgrade.
func TestAVMStartedBeforeIPv6ChangedLiveKeepsItsHash(t *testing.T) {
	started := driver.Interface{
		MachineUID: "m1", Name: "eth0", NetworkUID: "n1", MAC: "02:00:00:00:00:01",
		Address: netip.MustParsePrefix("10.0.0.2/24"), Gateway: netip.MustParseAddr("10.0.0.1"),
		IPv6: netip.MustParseAddr("2001:db8:0:1::a"), Device: "hm0123456789abc",
	}
	spec := driver.VMSpec{
		MachineUID: "m1", Machine: "web/ct-api", VCPUs: 1, MemoryMiB: 256,
		Interfaces: []driver.Interface{started},
		Run:        driver.RunSpec{Command: []string{"nginx"}},
	}
	// What the node before gave this spec, Hash at commit 2d5e442.
	const before = "f8fa417cbf321d091aadc724"
	moved := spec
	moved.Interfaces = []driver.Interface{started}
	moved.Interfaces[0].IPv6 = netip.MustParseAddr("2001:db8:0:1::b")
	for name, s := range map[string]driver.VMSpec{"as started": spec, "with its address moved since": moved} {
		if got := s.LegacyHash([]driver.Interface{started}); got != before {
			t.Errorf("%s: LegacyHash %s, the node before gave %s", name, got, before)
		}
	}
	if spec.Hash() != moved.Hash() {
		t.Error("an interface's IPv6 address changes the hash: the VM would start again")
	}
	if spec.Hash() == before {
		t.Error("Hash is LegacyHash: the test sees nothing")
	}
}
