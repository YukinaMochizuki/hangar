package apiserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
)

// keysQuotedTo are two keys, under every limit on keys themselves, whose
// vendor-data is size bytes: each < in their comments is six once quoted.
func keysQuotedTo(t *testing.T, size int) []string {
	t.Helper()
	base := sshKey("ssh-ed25519") + " "
	fixed := len(keysConfig(&computev1alpha1.VirtualMachineSpec{SshAuthorizedKeys: []string{base, base}}))
	lt, c := (size-fixed)/6, (size-fixed)%6
	keys := []string{base + strings.Repeat("<", lt/2) + strings.Repeat("c", c), base + strings.Repeat("<", lt-lt/2)}
	if n := len(keysConfig(&computev1alpha1.VirtualMachineSpec{SshAuthorizedKeys: keys})); n != size {
		t.Fatalf("keys of %d bytes of vendor-data, want %d", n, size)
	}
	if n := len(keys[0]) + len(keys[1]); n > maxSSHKeysLen || len(keys[1]) > maxSSHKeyLen {
		t.Fatalf("keys of %d bytes, past the limit on keys", n)
	}
	return keys
}

// TestKeysAreBoundedAsTheirMachineHoldsThem: a VM's keys are refused when
// their vendor-data is past what its machine may hold, though the keys are
// within their own limits, so a VM admitted always gets its machine.
func TestKeysAreBoundedAsTheirMachineHoldsThem(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 8192)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	vm := vmFromImage("web", "noble")
	vm.Spec.SshAuthorizedKeys = keysQuotedTo(t, maxVendorData+1)
	_, err := s.backend(registry.VirtualMachine).Apply(user, vm)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "keys of one byte too many once quoted")

	vm.Spec.SshAuthorizedKeys = keysQuotedTo(t, maxVendorData)
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Fatalf("keys of %d bytes once quoted: %v", maxVendorData, err)
	}
	waitMachine(t, s, system, "vm-web", "given the keys", func(m *computev1alpha1.Machine) bool {
		return len(m.GetSpec().GetFirmware().GetCloudInit().GetVendorData()) == maxVendorData
	})
}

// TestAMachinesCloudInitIsBounded: a machine's user-data and vendor-data
// are refused past their limits, at one byte past each.
func TestAMachinesCloudInitIsBounded(t *testing.T) {
	s, system, user := runningWith(t, func(c *Config) { c.Nodes = []string{"n1"} })
	if _, err := s.backend(registry.Disk).Apply(user, disk("web-root", "10Gi")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		what         string
		user, vendor int
		refused      bool
	}{
		{"user-data past its limit", maxUserData + 1, 0, true},
		{"vendor-data past its limit", 0, maxVendorData + 1, true},
		{"both at their limits", maxUserData, maxVendorData, false},
	} {
		m := machine("vm-web", "n1")
		m.Spec.Boot = &computev1alpha1.MachineSpec_Firmware{Firmware: &computev1alpha1.FirmwareBoot{
			RootDisk: "web-root",
			CloudInit: &computev1alpha1.MachineCloudInit{
				InstanceId: "i-1", UserData: strings.Repeat("u", tc.user), VendorData: strings.Repeat("v", tc.vendor),
			},
		}}
		_, err := s.backend(registry.Machine).Apply(system, m)
		if tc.refused {
			wantCodeOf(t, err, connect.CodeInvalidArgument, tc.what)
		} else if err != nil {
			t.Errorf("%s: %v", tc.what, err)
		}
	}
}

// TestAMachinesSerialNumberIsChecked: a firmware machine's system serial
// number is refused past 64 bytes, or holding other than printable ASCII.
func TestAMachinesSerialNumberIsChecked(t *testing.T) {
	s, system, user := runningWith(t, func(c *Config) { c.Nodes = []string{"n1"} })
	if _, err := s.backend(registry.Disk).Apply(user, disk("web-root", "10Gi")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		serial  string
		refused bool
	}{
		{strings.Repeat("s", 65), true},
		{"ARX\x0001", true},
		{"ARX-\x7f", true},
		{strings.Repeat(`~ ,"`, 16), false},
	} {
		m := machine("vm-web", "n1")
		m.Spec.Boot = &computev1alpha1.MachineSpec_Firmware{Firmware: &computev1alpha1.FirmwareBoot{RootDisk: "web-root", SystemSerialNumber: tc.serial}}
		_, err := s.backend(registry.Machine).Apply(system, m)
		if tc.refused {
			wantCodeOf(t, err, connect.CodeInvalidArgument, fmt.Sprintf("serial number %q", tc.serial))
		} else if err != nil {
			t.Errorf("serial number %q: %v", tc.serial, err)
		}
	}
}

// TestAMachineRefusedIsReported: admission refusing the machine its owner
// asks for, made or changed, is what the owner reports, not an error
// retried.
func TestAMachineRefusedIsReported(t *testing.T) {
	s, system, user := runningWith(t, func(c *Config) { c.Nodes = []string{"n1"} })
	if _, err := s.backend(registry.Disk).Apply(user, disk("web-root", "10Gi")); err != nil {
		t.Fatal(err)
	}
	// An owner that exists: garbage collection deletes a machine whose
	// owner is gone, and the change below would then race that delete.
	res, err := s.backend(registry.VirtualMachine).Apply(user, vmOfDisk("web", "web-root"))
	if err != nil {
		t.Fatal(err)
	}
	owner := res.Object.(*computev1alpha1.VirtualMachine).GetMetadata()
	spec := func(userData string) *computev1alpha1.MachineSpec {
		return &computev1alpha1.MachineSpec{
			RunState:      computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING,
			RestartPolicy: computev1alpha1.RestartPolicy_RESTART_POLICY_ALWAYS,
			Boot: &computev1alpha1.MachineSpec_Firmware{Firmware: &computev1alpha1.FirmwareBoot{
				RootDisk: "web-root", CloudInit: &computev1alpha1.MachineCloudInit{UserData: userData},
			}},
		}
	}
	tooMuch := strings.Repeat("u", maxUserData+1)
	var stuck *blocked
	if _, err := s.ensureMachineOf(system, registry.VirtualMachine, owner, "vm-web", spec(tooMuch), nil); !errors.As(err, &stuck) || stuck.reason != "MachineRefused" {
		t.Fatalf("a machine refused as made: %v, want MachineRefused", err)
	}
	m, err := s.ensureMachineOf(system, registry.VirtualMachine, owner, "vm-web", spec(""), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ensureMachineOf(system, registry.VirtualMachine, owner, "vm-web", spec(tooMuch), m)
	if !errors.As(err, &stuck) || stuck.reason != "MachineRefused" {
		t.Fatalf("a machine refused as changed: %v, want MachineRefused", err)
	}
	if got != m {
		t.Errorf("a machine refused as changed is reported as %v, not as it is", got)
	}
}

// TestVendorDataGivesTheKeys: a VM's keys are a cloud-config list, each
// quoted, quotes and colons in its comment too; a VM with none has no
// vendor-data.
func TestVendorDataGivesTheKeys(t *testing.T) {
	key := sshKey("ssh-ed25519")
	odd := strings.TrimSuffix(sshKey("ssh-rsa"), "alice@laptop") + `bob "the" builder: #1`
	got := keysConfig(&computev1alpha1.VirtualMachineSpec{SshAuthorizedKeys: []string{key, odd}})
	want := "#cloud-config\nssh_authorized_keys:\n" +
		"  - \"" + key + "\"\n" +
		"  - \"" + strings.ReplaceAll(odd, `"`, `\"`) + "\"\n"
	if got != want {
		t.Errorf("vendor-data\n%s\nwant\n%s", got, want)
	}
	if got := keysConfig(&computev1alpha1.VirtualMachineSpec{}); got != "" {
		t.Errorf("a VM without keys has vendor-data %q", got)
	}
}

// TestAVMsMachineCarriesItsCloudInit: a VM's machine carries its user-data
// as written, its keys as vendor-data, and the VM's uid as the instance
// id; changing the user-data changes them, and nothing else of the
// machine.
func TestAVMsMachineCarriesItsCloudInit(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 8192)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	vm := vmFromImage("web", "noble")
	vm.Spec.SshAuthorizedKeys = []string{sshKey("ssh-ed25519")}
	vm.Spec.UserData = "#cloud-config\nruncmd: [touch, /hello]\n"
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Fatal(err)
	}
	uid := getVM(t, s, system, "web").GetMetadata().GetUid()
	m := waitMachine(t, s, system, "vm-web", "placed with its cloud-init", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "n1" && m.GetSpec().GetFirmware().GetCloudInit() != nil
	})
	want := &computev1alpha1.MachineCloudInit{InstanceId: uid, UserData: vm.Spec.UserData, VendorData: keysConfig(vm.Spec)}
	if got := m.GetSpec().GetFirmware().GetCloudInit(); !proto.Equal(got, want) {
		t.Errorf("cloud-init %v, want %v", got, want)
	}

	vm.Spec.UserData = "#!/bin/sh\necho changed\n"
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Fatal(err)
	}
	after := waitMachine(t, s, system, "vm-web", "given the new user-data", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetFirmware().GetCloudInit().GetUserData() == vm.Spec.UserData
	})
	was, now := proto.Clone(m.GetSpec()).(*computev1alpha1.MachineSpec), proto.Clone(after.GetSpec()).(*computev1alpha1.MachineSpec)
	was.GetFirmware().CloudInit, now.GetFirmware().CloudInit = nil, nil
	if !proto.Equal(was, now) {
		t.Errorf("new user-data changed the machine's spec beyond its cloud-init:\n%v\n%v", was, now)
	}
}

// TestOnlyItsNodeReadsAGuestsData: a node agent reads a machine's cloud-init
// data only if the machine is on it, and no node reads a VM's user-data or
// keys, by get, list or watch; a user reads them all.
func TestOnlyItsNodeReadsAGuestsData(t *testing.T) {
	s, system, user := runningWith(t, func(c *Config) { c.Nodes = []string{"n1", "n2"} })
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	vm := vmFromImage("web", "noble")
	vm.Spec.SshAuthorizedKeys = []string{sshKey("ssh-ed25519")}
	vm.Spec.UserData = "#cloud-config\n{}\n"
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Disk).Apply(user, disk("web-root", "10Gi")); err != nil {
		t.Fatal(err)
	}
	m := machine("vm-web", "n1")
	m.Spec.Boot = &computev1alpha1.MachineSpec_Firmware{Firmware: &computev1alpha1.FirmwareBoot{
		RootDisk:  "web-root",
		CloudInit: &computev1alpha1.MachineCloudInit{InstanceId: "i-1", UserData: "secret", VendorData: "keys"},
	}}
	if _, err := s.backend(registry.Machine).Apply(system, m); err != nil {
		t.Fatal(err)
	}
	key := resource.Key{Project: "web", Name: "vm-web"}
	vmKey := resource.Key{Project: "web", Name: "web"}
	machineData := func(o proto.Message) string {
		return o.(*computev1alpha1.Machine).GetSpec().GetFirmware().GetCloudInit().GetUserData()
	}
	vmData := func(o proto.Message) string {
		spec := o.(*computev1alpha1.VirtualMachine).GetSpec()
		return spec.GetUserData() + strings.Join(spec.GetSshAuthorizedKeys(), "")
	}
	for _, tc := range []struct {
		who         string
		ctx         context.Context
		machine, vm bool
	}{
		{"n1, the machine's node", asNode(system, "n1"), true, false},
		{"n2", asNode(system, "n2"), false, false},
		{"the owner", user, true, true},
	} {
		got, err := s.backend(registry.Machine).Get(tc.ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if (machineData(got) != "") != tc.machine {
			t.Errorf("%s gets the machine's user-data %q", tc.who, machineData(got))
		}
		if got.(*computev1alpha1.Machine).GetSpec().GetFirmware().GetRootDisk() != "web-root" {
			t.Errorf("%s gets the machine without its root disk", tc.who)
		}
		list, err := s.backend(registry.Machine).List(tc.ctx, resource.ListOptions{Project: "web"})
		if err != nil || len(list.Items) != 1 || (machineData(list.Items[0]) != "") != tc.machine {
			t.Errorf("%s lists %v %v", tc.who, list.Items, err)
		}
		v, err := s.backend(registry.VirtualMachine).Get(tc.ctx, vmKey)
		if err != nil {
			t.Fatal(err)
		}
		if (vmData(v) != "") != tc.vm {
			t.Errorf("%s gets the VM's user-data and keys %q", tc.who, vmData(v))
		}
		vms, err := s.backend(registry.VirtualMachine).List(tc.ctx, resource.ListOptions{Project: "web"})
		if err != nil || len(vms.Items) != 1 || (vmData(vms.Items[0]) != "") != tc.vm {
			t.Errorf("%s lists the VMs %v %v", tc.who, vms.Items, err)
		}

		// A watch sees a change as a get does.
		ctx, cancel := context.WithTimeout(tc.ctx, 5*time.Second)
		events := make(chan proto.Message, 4)
		// The opening bookmark says from where the watch sees changes.
		started := make(chan struct{})
		go func() {
			begun := false
			_ = s.backend(registry.Machine).Watch(ctx, resource.WatchOptions{Project: "web"}, func(ev resource.Event) error {
				if ev.Object == nil {
					if !begun {
						begun = true
						close(started)
					}
					return nil
				}
				events <- ev.Object
				return nil
			})
		}()
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatalf("%s: the watch never began", tc.who)
		}
		m.Spec.GetFirmware().CloudInit.UserData = "secret, " + tc.who
		if _, err := s.backend(registry.Machine).Apply(system, m); err != nil {
			t.Fatal(err)
		}
		select {
		case o := <-events:
			if (machineData(o) != "") != tc.machine {
				t.Errorf("%s watches the machine's user-data %q", tc.who, machineData(o))
			}
		case <-ctx.Done():
			t.Errorf("%s saw no change", tc.who)
		}
		cancel()
	}

	// A status update answers as a get does: here a kind any node reports
	// on, which none is that holds such data today.
	reporting := *s.backend(registry.Machine)
	reporting.hooks.anyNodeReports = true
	for node, want := range map[string]bool{"n1": true, "n2": false} {
		got, err := reporting.UpdateStatus(asNode(system, node), key, &computev1alpha1.MachineStatus{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if (machineData(got) != "") != want {
			t.Errorf("%s updating the status is answered the machine's user-data %q", node, machineData(got))
		}
	}
}
