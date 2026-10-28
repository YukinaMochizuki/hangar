package driver_test

import (
	"encoding/json"
	"net/netip"
	"slices"
	"strings"
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

// TestAVMWithoutASerialNumberKeepsItsHash: a firmware VM a node started
// before VMSpec had a serial number hashes as it did, so the agent does not
// restart it on upgrade; giving it one, or changing it, restarts it.
func TestAVMWithoutASerialNumberKeepsItsHash(t *testing.T) {
	spec := driver.VMSpec{
		MachineUID: "m1", Machine: "p/vm-m1", VCPUs: 1, MemoryMiB: 1024, Firmware: true,
		Disks: []driver.Volume{{DiskUID: "d1", Path: "/dev/zvol/r", Size: 10 << 30}}, Restart: driver.RestartOnFailure,
	}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "SystemSerialNumber") {
		t.Errorf("a spec without a serial number encodes as %s, which changes its hash", b)
	}
	before := spec.Hash()
	spec.SystemSerialNumber = "ARX-01"
	given := spec.Hash()
	spec.SystemSerialNumber = "ARX-02"
	if given == before || spec.Hash() == given {
		t.Error("giving a VM a serial number, or changing it, leaves the hash as it was: the guest would not see it")
	}
}

// TestAVMWithoutATPMKeepsItsHash: a firmware VM a node started before
// VMSpec had a TPM hashes as it did, so the agent does not restart it on
// upgrade; giving it one restarts it.
func TestAVMWithoutATPMKeepsItsHash(t *testing.T) {
	spec := driver.VMSpec{
		MachineUID: "m1", Machine: "p/vm-m1", VCPUs: 1, MemoryMiB: 1024, Firmware: true,
		Disks: []driver.Volume{{DiskUID: "d1", Path: "/dev/zvol/r", Size: 10 << 30}}, Restart: driver.RestartOnFailure,
	}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "TPM") {
		t.Errorf("a spec without a TPM encodes as %s, which changes its hash", b)
	}
	before := spec.Hash()
	spec.TPM = true
	if spec.Hash() == before {
		t.Error("giving a VM a TPM leaves the hash as it was: the guest would not see it")
	}
}

// TestAVMStartedBeforeDiskSerialsRunsItsSpec: a VM a node started before
// volumes had serials, by Hash or by LegacyHash, runs the spec that now
// gives them; one started with other serials, or other volumes, does not.
func TestAVMStartedBeforeDiskSerialsRunsItsSpec(t *testing.T) {
	nic := driver.Interface{
		MachineUID: "m1", Name: "eth0", Address: netip.MustParsePrefix("10.0.0.2/24"),
		IPv6: netip.MustParseAddr("2001:db8:0:1::a"), Device: "hn1",
	}
	bare := driver.VMSpec{
		MachineUID: "m1", Machine: "p/vm-web", VCPUs: 1, MemoryMiB: 1024, Firmware: true,
		Disks:      []driver.Volume{{DiskUID: "d1", Path: "/dev/zvol/r", Size: 10 << 30}, {DiskUID: "d2", Path: "/dev/zvol/d", Size: 1 << 30}},
		Interfaces: []driver.Interface{nic}, Restart: driver.RestartOnFailure,
	}
	b, err := json.Marshal(bare)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "Serial") {
		t.Errorf("volumes without serials encode as %s, which changes their VM's hash", b)
	}
	serials := func(s driver.VMSpec, a, b string) driver.VMSpec {
		s.Disks = slices.Clone(s.Disks)
		s.Disks[0].Serial, s.Disks[1].Serial = a, b
		return s
	}
	now := serials(bare, "web-root", "data")
	started := []driver.Interface{nic}
	for name, hash := range map[string]string{
		"by its hash":                   now.Hash(),
		"by an older node's hash":       bare.Hash(),
		"by the IPv6 legacy hash, bare": bare.LegacyHash(started),
	} {
		if !now.Runs(driver.VM{SpecHash: hash, Interfaces: started}) {
			t.Errorf("a VM started %s does not run its spec: the agent would restart it", name)
		}
	}
	other := bare
	other.Disks = bare.Disks[:1]
	for name, hash := range map[string]string{
		"with other serials": serials(bare, "web-root", "logs").Hash(),
		"with a serial less": serials(bare, "web-root", "").Hash(),
		"with a volume less": other.Hash(),
	} {
		if now.Runs(driver.VM{SpecHash: hash, Interfaces: started}) {
			t.Errorf("a VM started %s runs the spec: the agent would not restart it", name)
		}
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

// TestAVMWhoseRootGrewRunsItsSpec: Hash leaves out the root disk's size,
// which grows while a VM runs, so a VM runs its spec whatever its root has
// grown to since it started. One an older node started has the hash that
// counted the size, as it started with: it runs the spec too, grown since
// or not, so neither an upgrade nor a growth restarts it. Anything else
// changed still does.
func TestAVMWhoseRootGrewRunsItsSpec(t *testing.T) {
	nic := driver.Interface{
		MachineUID: "m1", Name: "eth0", NetworkUID: "n1", MAC: "02:00:00:00:00:01",
		Address: netip.MustParsePrefix("10.0.0.2/24"), Gateway: netip.MustParseAddr("10.0.0.1"),
		IPv6: netip.MustParseAddr("2001:db8:0:1::a"), Device: "hm0123456789abc",
	}
	spec := driver.VMSpec{
		MachineUID: "m1", Machine: "web/ct-api", VCPUs: 1, MemoryMiB: 256,
		Root:       driver.Disk{MachineUID: "m1", Digest: "sha256:app", Path: "/dev/zvol/hangar/roots/m1", Size: 1 << 30},
		Interfaces: []driver.Interface{nic},
		Run:        driver.RunSpec{Command: []string{"nginx"}},
	}
	// What the node before gave this spec, Hash and LegacyHash at commit
	// 2c787ad.
	const sized, legacy = "cfb3f3cc89d159e5000591b8", "bb459d17f79af878d30a68ea"
	grown := spec
	grown.Root.Size = 3 << 30
	started := []driver.Interface{nic}
	for name, tt := range map[string]struct {
		spec driver.VMSpec
		vm   driver.VM
	}{
		"by its hash":                          {spec, driver.VM{SpecHash: spec.Hash(), RootSize: 1 << 30}},
		"by its hash, its root grown since":    {grown, driver.VM{SpecHash: spec.Hash(), RootSize: 1 << 30}},
		"by an older node's hash":              {spec, driver.VM{SpecHash: sized, RootSize: 1 << 30}},
		"by an older node's hash, grown since": {grown, driver.VM{SpecHash: sized, RootSize: 1 << 30}},
		"by the IPv6 legacy hash":              {spec, driver.VM{SpecHash: legacy, Interfaces: started, RootSize: 1 << 30}},
		"by the IPv6 legacy hash, grown since": {grown, driver.VM{SpecHash: legacy, Interfaces: started, RootSize: 1 << 30}},
	} {
		if !tt.spec.Runs(tt.vm) {
			t.Errorf("a VM started %s does not run its spec: the agent would restart it", name)
		}
	}
	if grown.Hash() != spec.Hash() {
		t.Error("the root disk's size changes the hash: a VM would start again for its root to grow")
	}
	bigger := spec
	bigger.MemoryMiB = 512
	for name, tt := range map[string]struct {
		spec driver.VMSpec
		vm   driver.VM
	}{
		"with other memory":                      {bigger, driver.VM{SpecHash: spec.Hash(), RootSize: 1 << 30}},
		"by an older node, with other memory":    {bigger, driver.VM{SpecHash: sized, RootSize: 1 << 30}},
		"by an older node, at another root size": {spec, driver.VM{SpecHash: sized, RootSize: 2 << 30}},
	} {
		if tt.spec.Runs(tt.vm) {
			t.Errorf("a VM started %s runs the spec: the agent would not restart it", name)
		}
	}
}
