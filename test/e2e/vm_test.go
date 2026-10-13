package e2e

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

func (w *world) applyVM(name string, runState computev1alpha1.VirtualMachineRunState) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.VirtualMachine, &computev1alpha1.VirtualMachine{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec: &computev1alpha1.VirtualMachineSpec{
			RootDisk: &computev1alpha1.VirtualMachineRootDisk{Source: &computev1alpha1.VirtualMachineRootDisk_FromImage{
				FromImage: &computev1alpha1.VirtualMachineImageDisk{Image: &storagev1alpha1.ImageSource{Name: "noble"}, Size: "10Gi"},
			}},
			RunState: runState,
		},
	}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) waitVMReason(name, reason string) *computev1alpha1.VirtualMachine {
	w.t.Helper()
	var vm *computev1alpha1.VirtualMachine
	w.eventually(fmt.Sprintf("virtual machine %s %s", name, reason), func() (bool, string) {
		var ok bool
		vm, ok = get[*computev1alpha1.VirtualMachine](w, registry.VirtualMachine, name)
		r := conditions.Find(vm.GetStatus().GetConditions(), "Ready")
		return ok && r.GetReason() == reason && r.GetObservedGeneration() == vm.GetMetadata().GetGeneration(), fmt.Sprintf("%v", r)
	})
	return vm
}

// vmLogs reads a VM's serial console to its end, as boot:data lines and
// boot:end:code, with :stopped for a boot the platform ended.
func (w *world) vmLogs(name string) []string {
	w.t.Helper()
	c := computev1alpha1connect.NewVirtualMachineLogsServiceClient(apiservertest.DevUser(apiservertest.Root), w.env.URL)
	stream, err := c.VirtualMachineLogs(context.Background(), connect.NewRequest(&computev1alpha1.VirtualMachineLogsRequest{Project: project, Name: name}))
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	var out []string
	for stream.Receive() {
		for _, r := range stream.Msg().GetRecords() {
			if end := r.GetEnd(); end != nil {
				line := fmt.Sprintf("%d:end:%d", r.GetBoot(), end.GetCode())
				if end.GetStopped() {
					line += ":stopped"
				}
				out = append(out, line)
				continue
			}
			out = append(out, fmt.Sprintf("%d:%s", r.GetBoot(), strings.TrimSuffix(string(r.GetData()), "\n")))
		}
	}
	if err := stream.Err(); err != nil {
		w.t.Fatal(err)
	}
	return out
}

// TestAVirtualMachineRuns follows a VM from apply, through its root disk
// made from an image, booting through firmware, its serial console, its
// guest powering off and a stop and start, to its deletion and an empty
// host.
func TestAVirtualMachineRuns(t *testing.T) {
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	w.applyVM("web", 0)
	vm := w.waitVMReason("web", "Running")
	if st := vm.GetStatus(); st.GetRootDisk() != "web-root" || len(st.GetInterfaces()) != 1 || st.GetInterfaces()[0].GetIpv4() == "" {
		t.Errorf("status %v", st)
	}

	m := w.machine2(vmMachineOf("web"))
	uid := m.GetMetadata().GetUid()
	root, _ := get[*storagev1alpha1.Disk](w, registry.Disk, "web-root")
	got := w.host.Snapshot().VMs[uid]
	if !got.Spec.Firmware || got.Spec.Root != (driver.Disk{}) || len(got.Spec.Disks) != 1 ||
		got.Spec.Disks[0].DiskUID != root.GetMetadata().GetUid() || got.Spec.Disks[0].Digest != "sha256:"+cloudSHA ||
		got.Spec.Restart != driver.RestartOnFailure || len(got.Spec.Interfaces) != 1 || len(got.Spec.Run.Command) != 0 {
		t.Errorf("the VM on the host: %+v", got.Spec)
	}
	// No guest agent is asked: running is ready.
	if n := w.host.GuestReadies(uid); n != 0 {
		t.Errorf("the guest agent of a VM was asked %d times", n)
	}
	if lines := w.vmLogs("web"); len(lines) == 0 || lines[0] != "1:Booting from the root disk" {
		t.Errorf("logs %q", lines)
	}

	// Its guest powering off leaves it off, for good.
	if err := w.host.Exit(uid, 0); err != nil {
		t.Fatal(err)
	}
	w.waitVMReason("web", computeapi.ReasonPoweredOff)
	if vm := w.host.Snapshot().VMs[uid]; vm.State != driver.VMExited || vm.Boots != 1 {
		t.Errorf("a powered-off VM: %+v", vm)
	}
	if lines := w.vmLogs("web"); lines[len(lines)-1] != "1:end:0" {
		t.Errorf("logs after the power-off %q", lines)
	}

	// Stopped and started, it boots again from its disk.
	w.applyVM("web", computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED)
	w.waitVMReason("web", computeapi.ReasonStopped)
	w.applyVM("web", computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_RUNNING)
	w.waitVMReason("web", "Running")
	if vm := w.host.Snapshot().VMs[uid]; vm.State != driver.VMRunning || vm.Spec.Disks[0].DiskUID != root.GetMetadata().GetUid() {
		t.Errorf("the VM started again: %+v", vm)
	}

	// Deleted, it takes its machine and its root disk with it.
	if _, err := w.root.Delete(context.Background(), registry.VirtualMachine, client.Key{Project: project, Name: "web"}); err != nil {
		t.Fatal(err)
	}
	w.eventually("the VM, its machine and its disk gone", func() (bool, string) {
		_, v := get[*computev1alpha1.VirtualMachine](w, registry.VirtualMachine, "web")
		_, m := get[*computev1alpha1.Machine](w, registry.Machine, vmMachineOf("web"))
		_, d := get[*storagev1alpha1.Disk](w, registry.Disk, "web-root")
		return !v && !m && !d, fmt.Sprintf("vm %t machine %t disk %t", v, m, d)
	})
	w.waitHostEmpty()
}

// TestOnlyItsGuestKeepsAVirtualMachineOff: a VM its host's reboot ended,
// its power button pressed, boots again once the host is back, and its log
// says the platform stopped it; so does a VM that crashed. Only its guest
// powering off on its own keeps it off, through a reboot too; and a stop
// says it was stopped.
func TestOnlyItsGuestKeepsAVirtualMachineOff(t *testing.T) {
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	w.applyVM("web", 0)
	w.waitVMReason("web", "Running")
	uid := w.machine2(vmMachineOf("web")).GetMetadata().GetUid()
	// bootedAgain waits for the VM's boot, as its log numbers it.
	bootedAgain := func(boot int) {
		t.Helper()
		w.eventually(fmt.Sprintf("boot %d", boot), func() (bool, string) {
			lines := w.vmLogs("web")
			vm := w.host.Snapshot().VMs[uid]
			return vm.State == driver.VMRunning && len(lines) > 0 && lines[len(lines)-1] == fmt.Sprintf("%d:Booting from the root disk", boot),
				fmt.Sprintf("%+v, logs %q", vm, lines)
		})
		w.waitVMReason("web", "Running")
	}

	// The host reboots: the agent goes, the VMs are stopped, and it is back.
	w.stopAgent()
	w.host.Reboot()
	w.startAgent()
	bootedAgain(2)
	if lines := w.vmLogs("web"); !slices.Contains(lines, "1:end:0:stopped") || lines[len(lines)-1] != "2:Booting from the root disk" {
		t.Errorf("logs after the reboot %q", lines)
	}

	// It crashes: it boots again.
	if err := w.host.Exit(uid, 253); err != nil {
		t.Fatal(err)
	}
	bootedAgain(3)
	if lines := w.vmLogs("web"); !slices.Contains(lines, "2:end:253") {
		t.Errorf("logs after the crash %q", lines)
	}

	// Its guest powers off: it stays off, and through a reboot.
	if err := w.host.Exit(uid, 0); err != nil {
		t.Fatal(err)
	}
	w.waitVMReason("web", computeapi.ReasonPoweredOff)
	w.stopAgent()
	w.host.Reboot()
	w.startAgent()
	w.waitVMReason("web", computeapi.ReasonPoweredOff)
	time.Sleep(200 * time.Millisecond)
	if vm := w.host.Snapshot().VMs[uid]; vm.State != driver.VMExited {
		t.Errorf("a VM whose guest powered off, after a reboot: %+v", vm)
	}
	if lines := w.vmLogs("web"); lines[len(lines)-1] != "3:end:0" {
		t.Errorf("logs after the power-off and the reboot %q", lines)
	}

	// Stopped and started, it boots again; stopped running, its log says
	// the platform ended the boot.
	w.applyVM("web", computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED)
	w.waitVMReason("web", computeapi.ReasonStopped)
	w.applyVM("web", computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_RUNNING)
	bootedAgain(4)
	w.applyVM("web", computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED)
	w.waitVMReason("web", computeapi.ReasonStopped)
	if lines := w.vmLogs("web"); lines[len(lines)-1] != "4:end:0:stopped" {
		t.Errorf("logs after the stop %q", lines)
	}
}

func vmMachineOf(vm string) string { return "vm-" + vm }
