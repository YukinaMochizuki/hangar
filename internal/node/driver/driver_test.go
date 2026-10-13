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
}
