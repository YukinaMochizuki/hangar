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
	if spec.Hash() != withDisk {
		t.Error("a grown volume changes the hash: the VM would be restarted to grow it")
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
	vm := func(hash string) driver.VM {
		return driver.VM{SpecHash: hash, Interfaces: started, DiskSizes: bare.DiskSizes()}
	}
	for name, hash := range map[string]string{
		"by its hash":                   now.Hash(),
		"by an older node's hash":       bare.SizedHash(),
		"by the IPv6 legacy hash, bare": bare.LegacyHash(started),
	} {
		if !now.Runs(vm(hash)) {
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
		if now.Runs(vm(hash)) {
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

// TestADiskGrownKeepsItsVM: Hash leaves out the disks' sizes, which grow
// while a VM runs; SizedHash is the hash nodes gave before, which counted
// them, so a VM such a node started runs its spec with its disks grown
// since, without a restart. Another disk, or one taken away, is another
// spec.
func TestADiskGrownKeepsItsVM(t *testing.T) {
	nic := driver.Interface{
		MachineUID: "m1", Name: "eth0", NetworkUID: "n1", MAC: "02:00:00:00:00:01",
		Address: netip.MustParsePrefix("10.0.0.2/24"), Gateway: netip.MustParseAddr("10.0.0.1"), Device: "hm0123456789abc",
	}
	ct := driver.VMSpec{
		MachineUID: "m1", Machine: "web/ct-api", VCPUs: 1, MemoryMiB: 256,
		Root:       driver.Disk{MachineUID: "m1", Digest: "sha256:aa", Path: "/dev/zvol/hangar/roots/m1", Size: 2 << 30},
		Disks:      []driver.Volume{{DiskUID: "d1", Path: "/dev/zvol/hangar/disks/d1", Size: 1 << 30}},
		Mounts:     []driver.Mount{{Disk: 0, Name: "data", Target: "/data"}},
		Interfaces: []driver.Interface{nic}, Run: driver.RunSpec{Command: []string{"nginx"}},
	}
	srv := driver.VMSpec{
		MachineUID: "s1", Machine: "web/fs-data", VCPUs: 1, MemoryMiB: 256,
		FileShareServer: &driver.ShareServer{Volume: driver.ShareVolume{ShareUID: "f1", Path: "/dev/zvol/hangar/shares/f1", Size: 1 << 30}},
	}
	// What the node before gave these specs, Hash at commit 2c787ad.
	for _, tt := range []struct {
		spec   driver.VMSpec
		before string
	}{{ct, "4c2186b58986393e3d8081f8"}, {srv, "bab8897fdc1792abfa8dca69"}} {
		s := tt.spec
		if got := s.SizedHash(); got != tt.before {
			t.Errorf("%s: SizedHash %s, the node before gave %s", s.Machine, got, tt.before)
		}
		grown, _ := s.WithDiskSizes(slices.Repeat([]int64{5 << 30}, len(s.DiskSizes())))
		if grown.Hash() != s.Hash() {
			t.Errorf("%s: growing its disks changes the hash: the VM would be restarted", s.Machine)
		}
		if s.Hash() == tt.before {
			t.Errorf("%s: Hash is SizedHash: the test sees nothing", s.Machine)
		}
		old := driver.VM{SpecHash: tt.before, Interfaces: s.Interfaces, DiskSizes: s.DiskSizes()}
		if !grown.Runs(old) {
			t.Errorf("%s: a VM an older node started does not run its spec with its disks grown: the agent would restart it", s.Machine)
		}
		if !grown.Runs(driver.VM{SpecHash: s.Hash()}) {
			t.Errorf("%s: a VM started with its disks smaller does not run its spec", s.Machine)
		}
	}
	other := ct
	other.Disks = []driver.Volume{{DiskUID: "d2", Path: "/dev/zvol/hangar/disks/d2", Size: 1 << 30}}
	less := ct
	less.Disks, less.Mounts = nil, nil
	for name, s := range map[string]driver.VMSpec{"another volume": other, "a volume less": less} {
		if s.Runs(driver.VM{SpecHash: ct.Hash(), DiskSizes: ct.DiskSizes()}) || s.Runs(driver.VM{SpecHash: ct.SizedHash(), DiskSizes: ct.DiskSizes()}) {
			t.Errorf("a VM started with %s runs the spec: the agent would not restart it", name)
		}
	}
	if d := ct.DiskSizes(); !slices.Equal(d, []int64{2 << 30, 1 << 30}) {
		t.Errorf("a container's disk sizes are %v, not its root's then its volume's", d)
	}
	if d := srv.DiskSizes(); !slices.Equal(d, []int64{1 << 30}) {
		t.Errorf("a share's server's disk sizes are %v, not its share's", d)
	}
	if _, ok := ct.WithDiskSizes([]int64{1}); ok {
		t.Error("WithDiskSizes took fewer sizes than the spec has disks")
	}
}
