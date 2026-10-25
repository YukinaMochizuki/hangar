package e2e

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

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
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
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
	testutil.Eventually(w.t, fmt.Sprintf("virtual machine %s %s", name, reason), func() (bool, string) {
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
	out, err := w.readVMLogs(name)
	if err != nil {
		w.t.Fatal(err)
	}
	return out
}

// vmLogsOnceConnected is vmLogs once the node is connected: an agent just
// started dials the apiserver in the background, and until it has, a call
// to its node is Unavailable.
func (w *world) vmLogsOnceConnected(name string) []string {
	w.t.Helper()
	var out []string
	testutil.Eventually(w.t, "the logs of "+name, func() (bool, string) {
		var err error
		out, err = w.readVMLogs(name)
		if connect.CodeOf(err) == connect.CodeUnavailable {
			return false, err.Error()
		}
		if err != nil {
			w.t.Fatal(err)
		}
		return true, ""
	})
	return out
}

func (w *world) readVMLogs(name string) ([]string, error) {
	c := computev1alpha1connect.NewMachineCapabilityServiceClient(apiservertest.DevUser(apiservertest.Root), w.env.URL)
	stream, err := c.Logs(context.Background(), connect.NewRequest(&computev1alpha1.LogsRequest{
		Project: project, Kind: registry.VirtualMachine.Kind, Name: name,
	}))
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	var out []string
	for stream.Receive() {
		for _, r := range stream.Msg().GetRecords() {
			if end := r.GetExit(); end != nil {
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
		return nil, err
	}
	return out, nil
}

// TestAVirtualMachineRuns follows a VM from apply, through its root disk
// made from an image, booting through firmware, its serial console, its
// guest powering off and a stop and start, to its deletion and an empty
// host.
func TestAVirtualMachineRuns(t *testing.T) {
	t.Parallel()
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
	testutil.Eventually(w.t, "the VM, its machine and its disk gone", func() (bool, string) {
		_, v := get[*computev1alpha1.VirtualMachine](w, registry.VirtualMachine, "web")
		_, m := get[*computev1alpha1.Machine](w, registry.Machine, vmMachineOf("web"))
		_, d := get[*storagev1alpha1.Disk](w, registry.Disk, "web-root")
		return !v && !m && !d, fmt.Sprintf("vm %t machine %t disk %t", v, m, d)
	})
	w.deleteImage("noble")
	w.waitHostEmpty()
}

// TestAVirtualMachinesSerialNumber: a VM's spec.platform.serial_number
// reaches its host's VM spec, with a warning to whoever applied it; changing
// it boots the VM again with the new one, and clearing it gives the VM the
// platform's back.
func TestAVirtualMachinesSerialNumber(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	applySerial := func(serial string) []string {
		t.Helper()
		res, err := w.root.Apply(context.Background(), registry.VirtualMachine, &computev1alpha1.VirtualMachine{
			Metadata: &metav1.ObjectMeta{Project: project, Name: "arx"},
			Spec: &computev1alpha1.VirtualMachineSpec{
				RootDisk: &computev1alpha1.VirtualMachineRootDisk{Source: &computev1alpha1.VirtualMachineRootDisk_FromImage{
					FromImage: &computev1alpha1.VirtualMachineImageDisk{Image: &storagev1alpha1.ImageSource{Name: "noble"}, Size: "10Gi"},
				}},
				Platform: &computev1alpha1.VirtualMachinePlatform{SerialNumber: serial},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return res.Warnings
	}
	onHost := func(want string) fake.VMView {
		t.Helper()
		var vm fake.VMView
		testutil.Eventually(t, "the VM on its host with serial number "+want, func() (bool, string) {
			m, ok := get[*computev1alpha1.Machine](w, registry.Machine, vmMachineOf("arx"))
			if !ok {
				return false, "no machine"
			}
			vm = w.host.Snapshot().VMs[m.GetMetadata().GetUid()]
			return vm.State == driver.VMRunning && vm.Spec.SystemSerialNumber == want, fmt.Sprintf("%+v", vm)
		})
		return vm
	}

	if warned := applySerial(`ARX,"01"`); len(warned) != 1 || !strings.Contains(warned[0], "hangar ssh") {
		t.Errorf("applying a VM with a serial number warned %q", warned)
	}
	w.waitVMReason("arx", "Running")
	onHost(`ARX,"01"`)

	// The host runs one spec per VM: running with the new serial number,
	// it was stopped and started again.
	applySerial("ARX-02")
	w.waitVMReason("arx", "Running")
	onHost("ARX-02")

	if warned := applySerial(""); len(warned) != 0 {
		t.Errorf("applying a VM without a serial number warned %q", warned)
	}
	w.waitVMReason("arx", "Running")
	onHost("")
}

// TestAVirtualMachinesDisksHaveSerials: a VM's disks reach its host with
// their serials, a name of up to 20 bytes as itself and a longer one as the
// end of its disk's uid. A VM a node started before disks had serials runs
// on after the agent is upgraded; started again, its disks have them.
func TestAVirtualMachinesDisksHaveSerials(t *testing.T) {
	t.Parallel()
	w := newIdleWorld(t)
	w.startAgent()
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	const long = "a-data-disk-of-a-long-name"
	w.applyDisk(long, "1Gi")
	w.applyVMDisks("web", 0, long)
	w.waitVMReason("web", "Running")
	uid := w.machine2(vmMachineOf("web")).GetMetadata().GetUid()
	data, _ := get[*storagev1alpha1.Disk](w, registry.Disk, long)
	tail := strings.ReplaceAll(data.GetMetadata().GetUid(), "-", "")
	tail = tail[len(tail)-20:]
	serials := func() []string {
		var out []string
		for _, v := range w.host.Snapshot().VMs[uid].Spec.Disks {
			out = append(out, v.Serial)
		}
		return out
	}
	if got := serials(); !slices.Equal(got, []string{"web-root", tail}) {
		t.Errorf("the VM's disks have serials %q, want web-root and %s", got, tail)
	}

	w.waitSettled()
	started := w.started(uid)
	w.stopAgent()
	w.host.StartedWithoutDiskSerials(uid)
	w.startAgent()
	w.waitVMReason("web", "Running")
	w.waitSettled()
	w.checkNotRestarted(started)
	if got := serials(); !slices.Equal(got, []string{"", ""}) {
		t.Fatalf("the VM an older node started has serials %q: the test sees nothing", got)
	}

	w.applyVMDisks("web", computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED, long)
	w.waitVMReason("web", computeapi.ReasonStopped)
	w.applyVMDisks("web", computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_RUNNING, long)
	w.waitVMReason("web", "Running")
	testutil.Eventually(t, "the VM started again with its disks' serials", func() (bool, string) {
		got := serials()
		return slices.Equal(got, []string{"web-root", tail}), fmt.Sprintf("%q", got)
	})
}

// applyVMDisks applies a VM of the image noble with the data disks named,
// in the run state given.
func (w *world) applyVMDisks(name string, runState computev1alpha1.VirtualMachineRunState, disks ...string) {
	w.t.Helper()
	spec := &computev1alpha1.VirtualMachineSpec{
		RootDisk: &computev1alpha1.VirtualMachineRootDisk{Source: &computev1alpha1.VirtualMachineRootDisk_FromImage{
			FromImage: &computev1alpha1.VirtualMachineImageDisk{Image: &storagev1alpha1.ImageSource{Name: "noble"}, Size: "10Gi"},
		}},
		RunState: runState,
	}
	for _, d := range disks {
		spec.Disks = append(spec.Disks, &computev1alpha1.VirtualMachineDisk{Disk: d})
	}
	if _, err := w.root.Apply(context.Background(), registry.VirtualMachine, &computev1alpha1.VirtualMachine{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name}, Spec: spec,
	}); err != nil {
		w.t.Fatal(err)
	}
}

// TestOnlyItsGuestKeepsAVirtualMachineOff: a VM its host's reboot ended,
// its power button pressed, boots again once the host is back, and its log
// says the platform stopped it; so does a VM that crashed. Only its guest
// powering off on its own keeps it off, through a reboot too; and a stop
// says it was stopped.
func TestOnlyItsGuestKeepsAVirtualMachineOff(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	w.applyVM("web", 0)
	w.waitVMReason("web", "Running")
	uid := w.machine2(vmMachineOf("web")).GetMetadata().GetUid()
	// bootedAgain waits for the VM's boot, as its log numbers it.
	bootedAgain := func(boot int) {
		t.Helper()
		testutil.Eventually(w.t, fmt.Sprintf("boot %d", boot), func() (bool, string) {
			lines := w.vmLogsOnceConnected("web")
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
	// The status says PoweredOff from before the reboot; once the agent back
	// has looked at everything, nothing may have booted the VM.
	if waiting := w.waitSettled(); len(waiting) > 0 {
		t.Errorf("after the reboot, still waiting on %v", waiting)
	}
	if vm := w.host.Snapshot().VMs[uid]; vm.State != driver.VMExited {
		t.Errorf("a VM whose guest powered off, after a reboot: %+v", vm)
	}
	lines := w.vmLogsOnceConnected("web")
	if lines[len(lines)-1] != "3:end:0" {
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
