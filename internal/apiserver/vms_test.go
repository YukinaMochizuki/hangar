package apiserver

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// sshKey is an authorized_keys line of type typ, its key made up but
// shaped as one.
func sshKey(typ string) string {
	blob := binary.BigEndian.AppendUint32(nil, uint32(len(typ)))
	blob = append(append(blob, typ...), make([]byte, 32)...)
	return typ + " " + base64.StdEncoding.EncodeToString(blob) + " alice@laptop"
}

func vmFromImage(name, img string) *computev1alpha1.VirtualMachine {
	return &computev1alpha1.VirtualMachine{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
		Spec: &computev1alpha1.VirtualMachineSpec{
			RootDisk: &computev1alpha1.VirtualMachineRootDisk{Source: &computev1alpha1.VirtualMachineRootDisk_FromImage{
				FromImage: &computev1alpha1.VirtualMachineImageDisk{Image: &storagev1alpha1.ImageSource{Name: img}, Size: "20Gi"},
			}},
		},
	}
}

func vmOfDisk(name, disk string) *computev1alpha1.VirtualMachine {
	return &computev1alpha1.VirtualMachine{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
		Spec: &computev1alpha1.VirtualMachineSpec{
			RootDisk: &computev1alpha1.VirtualMachineRootDisk{Source: &computev1alpha1.VirtualMachineRootDisk_Disk{Disk: disk}},
		},
	}
}

func getVM(t *testing.T, s *Server, system context.Context, name string) *computev1alpha1.VirtualMachine {
	t.Helper()
	obj, err := s.get(system, registry.VirtualMachine, resource.Key{Project: "web", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	vm, _ := obj.(*computev1alpha1.VirtualMachine)
	return vm
}

func waitVM(t *testing.T, s *Server, system context.Context, name, what string, ok func(*computev1alpha1.VirtualMachine) bool) *computev1alpha1.VirtualMachine {
	t.Helper()
	return waitObject(t, s, system, registry.VirtualMachine, resource.Key{Project: "web", Name: name}, "virtual machine "+name+" "+what, ok)
}

func vmReady(vm *computev1alpha1.VirtualMachine) *metav1.Condition {
	return conditions.Find(vm.GetStatus().GetConditions(), VMReady)
}

func TestVirtualMachineAdmission(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	b := s.backend(registry.VirtualMachine)
	if _, err := b.Apply(user, vmFromImage("web", "noble")); err != nil {
		t.Fatalf("a VM of an image: %v", err)
	}
	if _, err := b.Apply(user, vmOfDisk("db", "db-root")); err != nil {
		t.Fatalf("a VM of a disk: %v", err)
	}
	for name, mutate := range map[string]func(*computev1alpha1.VirtualMachineSpec){
		"no root disk":           func(s *computev1alpha1.VirtualMachineSpec) { s.RootDisk = nil },
		"an image that is none":  func(s *computev1alpha1.VirtualMachineSpec) { s.RootDisk.GetFromImage().Image.Name = "nope" },
		"no image":               func(s *computev1alpha1.VirtualMachineSpec) { s.RootDisk.GetFromImage().Image = nil },
		"a disk below the image": func(s *computev1alpha1.VirtualMachineSpec) { s.RootDisk.GetFromImage().Size = "2Gi" },
		"a root size unreadable": func(s *computev1alpha1.VirtualMachineSpec) { s.RootDisk.GetFromImage().Size = "big" },
		"an unknown delete policy": func(s *computev1alpha1.VirtualMachineSpec) {
			s.RootDisk.GetFromImage().DeletePolicy = 9
		},
		"too little memory": func(s *computev1alpha1.VirtualMachineSpec) {
			s.Resources = &computev1alpha1.VirtualMachineResources{Memory: "128Mi"}
		},
		"half a vCPU": func(s *computev1alpha1.VirtualMachineSpec) {
			s.Resources = &computev1alpha1.VirtualMachineResources{Cpu: "500m"}
		},
		"an unknown run state": func(s *computev1alpha1.VirtualMachineSpec) { s.RunState = 7 },
		"the root disk as a data disk": func(s *computev1alpha1.VirtualMachineSpec) {
			s.Disks = []*computev1alpha1.VirtualMachineDisk{{Disk: "bad-root"}}
		},
		"a data disk twice": func(s *computev1alpha1.VirtualMachineSpec) {
			s.Disks = []*computev1alpha1.VirtualMachineDisk{{Disk: "data"}, {Disk: "data"}}
		},
		"an invalid network": func(s *computev1alpha1.VirtualMachineSpec) {
			s.Interfaces = []*computev1alpha1.NetworkInterface{{Network: "Bad_Net"}}
		},
		"nine interfaces": func(s *computev1alpha1.VirtualMachineSpec) {
			for range 9 {
				s.Interfaces = append(s.Interfaces, &computev1alpha1.NetworkInterface{Network: "default"})
			}
		},
		"a key with options": func(s *computev1alpha1.VirtualMachineSpec) {
			s.SshAuthorizedKeys = []string{`command="rm -rf /" ` + sshKey("ssh-ed25519")}
		},
		"a key of another type than it says": func(s *computev1alpha1.VirtualMachineSpec) {
			s.SshAuthorizedKeys = []string{"ssh-rsa " + strings.Fields(sshKey("ssh-ed25519"))[1]}
		},
		"a key not base64": func(s *computev1alpha1.VirtualMachineSpec) { s.SshAuthorizedKeys = []string{"ssh-ed25519 !!!"} },
		"two keys on a line": func(s *computev1alpha1.VirtualMachineSpec) {
			s.SshAuthorizedKeys = []string{sshKey("ssh-ed25519") + "\n" + sshKey("ssh-ed25519")}
		},
		"keys of more than 32 KiB in all": func(s *computev1alpha1.VirtualMachineSpec) {
			long := sshKey("ssh-ed25519") + " " + strings.Repeat("c", 12<<10)
			s.SshAuthorizedKeys = []string{long, long, long}
		},
		"user-data past 16 KiB": func(s *computev1alpha1.VirtualMachineSpec) { s.UserData = strings.Repeat("x", 16<<10+1) },
		"a serial number of 65 bytes": func(s *computev1alpha1.VirtualMachineSpec) {
			s.Platform = &computev1alpha1.VirtualMachinePlatform{SerialNumber: strings.Repeat("s", 65)}
		},
		"a serial number with a tab": func(s *computev1alpha1.VirtualMachineSpec) {
			s.Platform = &computev1alpha1.VirtualMachinePlatform{SerialNumber: "ARX\t01"}
		},
		"a serial number not ASCII": func(s *computev1alpha1.VirtualMachineSpec) {
			s.Platform = &computev1alpha1.VirtualMachinePlatform{SerialNumber: "ARX-é"}
		},
		"a key comment with DEL": func(s *computev1alpha1.VirtualMachineSpec) {
			s.SshAuthorizedKeys = []string{sshKey("ssh-ed25519") + "\x7f"}
		},
		"a key comment with NEL": func(s *computev1alpha1.VirtualMachineSpec) {
			s.SshAuthorizedKeys = []string{sshKey("ssh-ed25519") + "\u0085"}
		},
		"a key comment with U+FFFE": func(s *computev1alpha1.VirtualMachineSpec) {
			s.SshAuthorizedKeys = []string{sshKey("ssh-ed25519") + "\ufffe"}
		},
		"a key comment not UTF-8": func(s *computev1alpha1.VirtualMachineSpec) {
			s.SshAuthorizedKeys = []string{sshKey("ssh-ed25519") + "\xff"}
		},
	} {
		vm := vmFromImage("bad", "noble")
		mutate(vm.Spec)
		_, err := b.Apply(user, vm)
		if c := connect.CodeOf(err); c != connect.CodeInvalidArgument && c != connect.CodeFailedPrecondition {
			t.Errorf("%s: %v, want it refused", name, err)
		}
	}
	ok := vmFromImage("keys", "noble")
	ok.Spec.SshAuthorizedKeys = []string{sshKey("ssh-ed25519"), sshKey("ecdsa-sha2-nistp256") + "\tbob \"<é>\" 🔑"}
	ok.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: "default"}, {Network: "other"}}
	ok.Spec.UserData = "#!/bin/sh\n" + strings.Repeat("#", 16<<10-10)
	if _, err := b.Apply(user, ok); err != nil {
		t.Errorf("a VM with keys, two interfaces and 16 KiB of user-data: %v", err)
	}
	serial := vmFromImage("serial", "noble")
	serial.Spec.Platform = &computev1alpha1.VirtualMachinePlatform{SerialNumber: " " + strings.Repeat(`,"[]=~`, 10) + "abc"}
	if _, err := b.Apply(user, serial); err != nil {
		t.Errorf("a VM with a serial number of 64 bytes of printable ASCII: %v", err)
	}

	// The root disk is what it was made as; its size may grow.
	other := vmFromImage("web", "noble")
	other.Spec.RootDisk.GetFromImage().Image.Name = "jammy"
	_, err := b.Apply(user, other)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "another image for a VM's root disk")
	_, err = b.Apply(user, vmOfDisk("web", "web-root"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a VM's root disk made a disk it names")
	_, err = b.Apply(user, vmOfDisk("db", "other"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "another disk as a VM's root disk")
	grown := vmFromImage("web", "noble")
	grown.Spec.RootDisk.GetFromImage().Size = "30Gi"
	grown.Spec.RootDisk.GetFromImage().DeletePolicy = computev1alpha1.VirtualMachineDeletePolicy_VIRTUAL_MACHINE_DELETE_POLICY_RETAIN
	if _, err := b.Apply(user, grown); err != nil {
		t.Errorf("growing a VM's root disk and retaining it: %v", err)
	}
}

func TestAVirtualMachineRuns(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 8192, 100)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Disk).Apply(user, disk("data", "5Gi")); err != nil {
		t.Fatal(err)
	}
	vm := vmFromImage("web", "noble")
	vm.Spec.Disks = []*computev1alpha1.VirtualMachineDisk{{Disk: "data"}}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Fatal(err)
	}
	vmUID := getVM(t, s, system, "web").GetMetadata().GetUid()

	// Its root disk is made for it, owned by it.
	root := waitDisk(t, s, system, "web-root", "made", func(*storagev1alpha1.Disk) bool { return true })
	if root.GetSpec().GetSize() != "20Gi" || root.GetSpec().GetSource().GetImage().GetName() != "noble" ||
		root.GetMetadata().GetAnnotations()[RootDiskOf] != vmUID || !ownsDisk(root, getVM(t, s, system, "web")) {
		t.Errorf("root disk %v", root)
	}
	// Its machine boots the root disk through firmware, with the data disk,
	// one interface on default in the default group, restarted only if its
	// boot fails, and no disk of its own.
	m := waitMachine(t, s, system, "vm-web", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	spec := m.GetSpec()
	if spec.GetFirmware().GetRootDisk() != "web-root" || spec.GetContainer() != nil || len(spec.GetDisks()) != 1 || spec.GetDisks()[0].GetDisk() != "data" ||
		spec.GetRestartPolicy() != computev1alpha1.RestartPolicy_RESTART_POLICY_ON_FAILURE ||
		spec.GetResources().GetCpu() != "1" || spec.GetResources().GetMemory() != "1024Mi" || spec.GetResources().GetDisk() != "" ||
		len(spec.GetInterfaces()) != 1 || spec.GetInterfaces()[0].GetName() != "eth0" || spec.GetInterfaces()[0].GetNetwork() != computeapi.DefaultNetwork ||
		!slices.Equal(spec.GetInterfaces()[0].GetSecurityGroups(), []string{secgroup.Default}) {
		t.Errorf("machine spec %v", spec)
	}
	waitDisk(t, s, system, "web-root", "bound with it", func(d *storagev1alpha1.Disk) bool { return d.GetStatus().GetNode() == "n1" })
	got := waitVM(t, s, system, "web", "reporting its interfaces", func(vm *computev1alpha1.VirtualMachine) bool {
		return len(vm.GetStatus().GetInterfaces()) == 1 && vm.GetStatus().GetInterfaces()[0].GetIpv4() != ""
	})
	if st := got.GetStatus(); st.GetRootDisk() != "web-root" || st.GetInterfaces()[0].GetName() != "eth0" {
		t.Errorf("status %v", st)
	}
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration(),
		&metav1.Condition{Type: computeapi.MachineReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: computeapi.ReasonRunning})
	waitVM(t, s, system, "web", "ready", func(vm *computev1alpha1.VirtualMachine) bool {
		return conditions.IsTrue(vm.GetStatus().GetConditions(), VMReady)
	})

	// A second interface: eth1, an address of its own network, and the VM
	// restarted on the node.
	if _, err := s.backend(registry.Network).Apply(user, &networkv1alpha1.Network{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: "back"}, Spec: &networkv1alpha1.NetworkSpec{Ipv4Cidr: "10.9.0.0/24"},
	}); err != nil {
		t.Fatal(err)
	}
	two := proto.Clone(vm).(*computev1alpha1.VirtualMachine)
	two.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: computeapi.DefaultNetwork}, {Network: "back"}}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, two); err != nil {
		t.Fatal(err)
	}
	m = waitMachine(t, s, system, "vm-web", "given eth1", func(m *computev1alpha1.Machine) bool {
		ifs := m.GetSpec().GetInterfaces()
		return len(ifs) == 2 && ifs[1].GetName() == "eth1" && strings.HasPrefix(ifs[1].GetIpv4(), "10.9.0.")
	})
	if ip := m.GetSpec().GetInterfaces()[0].GetIpv4(); ip != got.GetStatus().GetInterfaces()[0].GetIpv4() {
		t.Errorf("eth0 moved from %s to %s", got.GetStatus().GetInterfaces()[0].GetIpv4(), ip)
	}
	waitVM(t, s, system, "web", "updating", func(vm *computev1alpha1.VirtualMachine) bool { return vmReady(vm).GetReason() == "Updating" })

	// Stopped, its machine is asked to stop.
	two.Spec.RunState = computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED
	if _, err := s.backend(registry.VirtualMachine).Apply(user, two); err != nil {
		t.Fatal(err)
	}
	m = waitMachine(t, s, system, "vm-web", "asked to stop", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetRunState() == computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	})
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration())
	waitVM(t, s, system, "web", "stopped", func(vm *computev1alpha1.VirtualMachine) bool {
		return conditions.IsTrue(vm.GetStatus().GetConditions(), VMStopped)
	})
}

// TestARootDiskFollowsItsDeletePolicy: a root disk made for a VM goes with it
// unless it is retained; retained, it outlives the VM, and a VM of the
// same name takes it only by naming it.
func TestARootDiskFollowsItsDeletePolicy(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	vms := s.backend(registry.VirtualMachine)
	key := resource.Key{Project: "web", Name: "web"}

	// Deleted, it goes with the VM. No node took the machine, so nothing
	// waits for a node.
	if _, err := vms.Apply(user, vmFromImage("web", "noble")); err != nil {
		t.Fatal(err)
	}
	waitDisk(t, s, system, "web-root", "made", func(*storagev1alpha1.Disk) bool { return true })
	waitMachine(t, s, system, "vm-web", "made", func(*computev1alpha1.Machine) bool { return true })
	if _, err := vms.Delete(user, key, nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.VirtualMachine, "web")
	waitGone(t, s, system, registry.Machine, "vm-web")
	waitGone(t, s, system, registry.Disk, "web-root")

	// Retained, it stays.
	retained := vmFromImage("web", "noble")
	retained.Spec.RootDisk.GetFromImage().DeletePolicy = computev1alpha1.VirtualMachineDeletePolicy_VIRTUAL_MACHINE_DELETE_POLICY_RETAIN
	if _, err := vms.Apply(user, retained); err != nil {
		t.Fatal(err)
	}
	vm := getVM(t, s, system, "web")
	waitDisk(t, s, system, "web-root", "made, not owned by the VM", func(d *storagev1alpha1.Disk) bool { return !ownsDisk(d, vm) })
	// Made to go with it, then retained again before it goes.
	if _, err := vms.Apply(user, vmFromImage("web", "noble")); err != nil {
		t.Fatal(err)
	}
	waitDisk(t, s, system, "web-root", "owned by the VM", func(d *storagev1alpha1.Disk) bool { return ownsDisk(d, vm) })
	if _, err := vms.Apply(user, retained); err != nil {
		t.Fatal(err)
	}
	waitDisk(t, s, system, "web-root", "given back", func(d *storagev1alpha1.Disk) bool { return !ownsDisk(d, vm) })
	if _, err := vms.Delete(user, key, nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.VirtualMachine, "web")
	waitGone(t, s, system, registry.Machine, "vm-web")
	// The disk's own controllers may still be writing what the VM's going
	// changed; quiet is what comes after them.
	waitSettled(t, s)
	testutil.Unchanged(t, "the retained root disk, after its VM went", 100*time.Millisecond,
		versionOf(s, system, registry.Disk, resource.Key{Project: "web", Name: "web-root"}))

	// A new VM of the name does not take it unasked; named, it boots it.
	if _, err := vms.Apply(user, vmFromImage("web", "noble")); err != nil {
		t.Fatal(err)
	}
	waitVM(t, s, system, "web", "told the disk is not its own", func(vm *computev1alpha1.VirtualMachine) bool {
		return vmReady(vm).GetReason() == "RootDiskNameInUse"
	})
	if _, err := vms.Delete(user, key, nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.VirtualMachine, "web")
	if _, err := vms.Apply(user, vmOfDisk("web", "web-root")); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "vm-web", "booting the retained disk", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetFirmware().GetRootDisk() == "web-root"
	})
}

// TestAVMIsNotDeletedBeforeItsPolicyHolds: a VM whose root disk's owner
// reference does not yet say what its delete policy does is not deleted.
func TestAVMIsNotDeletedBeforeItsPolicyHolds(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vmFromImage("web", "noble")); err != nil {
		t.Fatal(err)
	}
	// The controller made it while the policy was DELETE, and has not seen
	// the policy change since.
	if err := s.ensureRootDisk(system, getVM(t, s, system, "web")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, retainedVM("web")); err != nil {
		t.Fatal(err)
	}
	_, err := s.backend(registry.VirtualMachine).Delete(user, resource.Key{Project: "web", Name: "web"}, nil)
	wantCodeOf(t, err, connect.CodeAborted, "deleting a VM whose retained disk is still owned by it")
}

func retainedVM(name string) *computev1alpha1.VirtualMachine {
	vm := vmFromImage(name, "noble")
	vm.Spec.RootDisk.GetFromImage().DeletePolicy = computev1alpha1.VirtualMachineDeletePolicy_VIRTUAL_MACHINE_DELETE_POLICY_RETAIN
	return vm
}

// TestAStaleReconcileDoesNotUndoADelete: a reconcile that read a VM before
// its delete policy changed and the VM was deleted, the delete allowed on
// the disk's owner reference as the new policy wants it, cannot then make
// the reference say the old policy: a retained disk would go with the VM,
// or a disk to go with it stay behind.
func TestAStaleReconcileDoesNotUndoADelete(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		before, stale, last *computev1alpha1.VirtualMachine
		wantOwned           bool
	}{
		"retained":            {before: retainedVM("web"), stale: vmFromImage("web", "noble"), last: retainedVM("web")},
		"deleted with the VM": {before: vmFromImage("web", "noble"), stale: retainedVM("web"), last: vmFromImage("web", "noble"), wantOwned: true},
	} {
		t.Run(name, func(t *testing.T) {
			s, system, user := running(t)
			if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
				t.Fatal(err)
			}
			vms := s.backend(registry.VirtualMachine)
			if _, err := vms.Apply(user, tt.before); err != nil {
				t.Fatal(err)
			}
			if err := s.ensureRootDisk(system, getVM(t, s, system, "web")); err != nil {
				t.Fatal(err)
			}
			// The reconcile reads the VM with the policy changed ...
			if _, err := vms.Apply(user, tt.stale); err != nil {
				t.Fatal(err)
			}
			stale := getVM(t, s, system, "web")
			// ... which changes back, and the VM is deleted, as the disk is.
			if _, err := vms.Apply(user, tt.last); err != nil {
				t.Fatal(err)
			}
			if _, err := vms.Delete(user, resource.Key{Project: "web", Name: "web"}, nil); err != nil {
				t.Fatal(err)
			}
			err := s.ensureRootDisk(system, stale)
			wantCodeOf(t, err, connect.CodeAborted, "a stale reconcile changing the root disk's owner reference after the delete")
			d := getDisk(t, s, system, "web-root")
			if got := d != nil && ownsDisk(d, stale); got != tt.wantOwned {
				t.Errorf("the disk is owned by the deleted VM: %t, want %t", got, tt.wantOwned)
			}
		})
	}
}

// TestTheDeleteAndTheOwnerReferenceRace: a VM's delete, with its retain
// policy just applied, racing a reconcile that read its earlier delete
// policy: one wins, and a deleted VM never leaves its retained disk owned,
// to go with it.
func TestTheDeleteAndTheOwnerReferenceRace(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	vms := s.backend(registry.VirtualMachine)
	deletes, refs := 0, 0
	for i := range 40 {
		name := fmt.Sprintf("r%d", i)
		key := resource.Key{Project: "web", Name: name}
		if _, err := vms.Apply(user, retainedVM(name)); err != nil {
			t.Fatal(err)
		}
		if err := s.ensureRootDisk(system, getVM(t, s, system, name)); err != nil {
			t.Fatal(err)
		}
		if _, err := vms.Apply(user, vmFromImage(name, "noble")); err != nil {
			t.Fatal(err)
		}
		stale := getVM(t, s, system, name)
		var delErr, refErr error
		var wg sync.WaitGroup
		wg.Go(func() {
			if _, delErr = vms.Apply(user, retainedVM(name)); delErr == nil {
				_, delErr = vms.Delete(user, key, nil)
			}
		})
		wg.Go(func() { refErr = s.ensureRootDisk(system, stale) })
		wg.Wait()
		if delErr == nil {
			deletes++
		} else {
			wantCodeOf(t, delErr, connect.CodeAborted, "the losing delete")
		}
		if refErr == nil {
			refs++
		}
		d := getDisk(t, s, system, rootDiskName(name))
		if delErr == nil && (d == nil || ownsDisk(d, stale)) {
			t.Fatalf("round %d: the VM was deleted, its retained disk owned by it (%v), the stale reconcile said %v", i, d, refErr)
		}
	}
	t.Logf("the delete won %d times, the stale owner reference %d", deletes, refs)
}
