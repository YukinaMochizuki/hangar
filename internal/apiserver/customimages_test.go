package apiserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

func customImage(name, disk string) *storagev1alpha1.CustomImage {
	return &storagev1alpha1.CustomImage{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
		Spec: &storagev1alpha1.CustomImageSpec{
			Source: &storagev1alpha1.CustomImageSource{Source: &storagev1alpha1.CustomImageSource_Disk{
				Disk: &storagev1alpha1.CustomImageDiskSource{Name: disk},
			}},
			Os: "ubuntu", Version: "24.04", DefaultUser: "ubuntu",
		},
	}
}

func getCustomImage(t *testing.T, s *Server, system context.Context, name string) *storagev1alpha1.CustomImage {
	t.Helper()
	obj, err := s.get(system, registry.CustomImage, resource.Key{Project: "web", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	ci, _ := obj.(*storagev1alpha1.CustomImage)
	return ci
}

// reportCustomImage writes a custom image's status as its node does: c, at
// the image's generation, and usedSize.
func reportCustomImage(t *testing.T, s *Server, system context.Context, name, usedSize string, cs ...*metav1.Condition) {
	t.Helper()
	ci := getCustomImage(t, s, system, name)
	status := proto.Clone(ci.GetStatus()).(*storagev1alpha1.CustomImageStatus)
	for _, c := range cs {
		c.ObservedGeneration = ci.GetMetadata().GetGeneration()
		status.Conditions = conditions.Set(status.GetConditions(), c, time.Now())
	}
	status.UsedSize = usedSize
	if _, err := s.backend(registry.CustomImage).UpdateStatus(asNode(system, ci.GetStatus().GetNode()),
		resource.Key{Project: "web", Name: name}, status, nil); err != nil {
		t.Fatal(err)
	}
}

func snapshotted() *metav1.Condition {
	return &metav1.Condition{Type: CustomImageSnapshotted, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: computeapi.ReasonCaptured}
}

func imageReady() *metav1.Condition {
	return &metav1.Condition{Type: CustomImageReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: computeapi.ReasonCaptured}
}

func captureFailed() *metav1.Condition {
	return &metav1.Condition{Type: CustomImageReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: computeapi.ReasonCaptureFailed, Message: "out of space"}
}

// madeDisk makes a disk of size bound to node, as one a machine there has
// run with.
func madeDisk(t *testing.T, s *Server, system, user context.Context, name, node, size string) {
	t.Helper()
	if _, err := s.backend(registry.Disk).Apply(user, disk(name, size)); err != nil {
		t.Fatal(err)
	}
	bindDisk(t, s, system, name, node, size)
}

// vmMachine is the machine of VM vm booting rootDisk on node, as its
// controller writes it, in state.
func vmMachine(vm, rootDisk, node string, state computev1alpha1.MachineRunState) *computev1alpha1.Machine {
	m := machine(vmMachineName(vm), node)
	m.Spec.RunState = state
	m.Spec.RestartPolicy = computev1alpha1.RestartPolicy_RESTART_POLICY_ON_FAILURE
	m.Spec.Boot = &computev1alpha1.MachineSpec_Firmware{Firmware: &computev1alpha1.FirmwareBoot{RootDisk: rootDisk}}
	return m
}

// stoppedVM sets up what capturing a stopped VM's root disk starts from:
// the disk db-root, 10Gi, made on n1, the VM db booting it, asked to stop,
// and its machine, which n1 has stopped.
func stoppedVM(t *testing.T, s *Server, system, user context.Context) {
	t.Helper()
	madeDisk(t, s, system, user, "db-root", "n1", "10Gi")
	vm := vmOfDisk("db", "db-root")
	vm.Spec.RunState = computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Fatal(err)
	}
	res, err := s.backend(registry.Machine).Apply(system, vmMachine("db", "db-root", "n1", computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED))
	if err != nil {
		t.Fatal(err)
	}
	m := res.Object.(*computev1alpha1.Machine)
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration())
}

// mounting is c with disk mounted as a volume at /data.
func mounting(c *computev1alpha1.Container, disk string) *computev1alpha1.Container {
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{{Type: "volume", Source: disk, Target: "/data"}}
	return c
}

func vmWithRunState(vm *computev1alpha1.VirtualMachine, state computev1alpha1.VirtualMachineRunState) *computev1alpha1.VirtualMachine {
	vm = proto.Clone(vm).(*computev1alpha1.VirtualMachine)
	vm.Spec.RunState = state
	return vm
}

const (
	vmRunning = computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_RUNNING
	vmStopped = computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED
)

func TestCustomImageAdmission(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	b := s.backend(registry.CustomImage)
	for name, mutate := range map[string]func(*storagev1alpha1.CustomImageSpec){
		"no source":                  func(s *storagev1alpha1.CustomImageSpec) { s.Source = nil },
		"a source disk by bad name":  func(s *storagev1alpha1.CustomImageSpec) { s.GetSource().GetDisk().Name = "Bad_Name" },
		"a default user not a login": func(s *storagev1alpha1.CustomImageSpec) { s.DefaultUser = "root;rm" },
		"a minimum below 1Gi":        func(s *storagev1alpha1.CustomImageSpec) { s.MinDiskSize = "512Mi" },
		"a minimum unreadable":       func(s *storagev1alpha1.CustomImageSpec) { s.MinDiskSize = "big" },
	} {
		ci := customImage("bad", "db-root")
		mutate(ci.Spec)
		_, err := b.Apply(user, ci)
		wantCodeOf(t, err, connect.CodeInvalidArgument, name)
	}
	_, err := b.Apply(user, customImage("img", "nope"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a source disk that does not exist")
	if _, err := s.backend(registry.Disk).Apply(user, disk("blank", "5Gi")); err != nil {
		t.Fatal(err)
	}
	_, err = b.Apply(user, customImage("img", "blank"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a source disk not made on a node yet")

	// A disk made and attached to nothing is captured; the image lives on
	// its node and is as large as it.
	madeDisk(t, s, system, user, "data", "n1", "8Gi")
	res, err := b.Apply(user, customImage("data-img", "data"))
	if err != nil {
		t.Fatalf("capturing a disk attached to nothing: %v", err)
	}
	if st := res.Object.(*storagev1alpha1.CustomImage).GetStatus(); st.GetNode() != "n1" || st.GetDiskSize() != "8Gi" {
		t.Errorf("status %v, want node n1 and disk_size 8Gi", st)
	}
	// The spec's metadata can change; its source cannot.
	changed := customImage("data-img", "data")
	changed.Spec.Version = "24.04.1"
	if _, err := b.Apply(user, changed); err != nil {
		t.Errorf("changing the image's version: %v", err)
	}
	changed.Spec.GetSource().GetDisk().Name = "blank"
	_, err = b.Apply(user, changed)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "changing the image's source")

	// A stopped VM's root disk is captured once its node has stopped it,
	// at its current spec, and the VM itself asks to stay stopped.
	stoppedVM(t, s, system, user)
	mkey := resource.Key{Project: "web", Name: vmMachineName("db")}
	m := getMachine(t, s, system, mkey)
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration())
	_, err = b.Apply(user, customImage("db-img", "db-root"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "capturing a disk whose machine its node still runs")
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration()-1)
	_, err = b.Apply(user, customImage("db-img", "db-root"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "capturing a disk whose machine is stopped at an older spec")
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration())
	vms := s.backend(registry.VirtualMachine)
	if _, err := vms.Apply(user, vmWithRunState(getVM(t, s, system, "db"), vmRunning)); err != nil {
		t.Fatal(err)
	}
	_, err = b.Apply(user, customImage("db-img", "db-root"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "capturing a disk whose VM asks to run, before its machine does")
	if _, err := vms.Apply(user, vmWithRunState(getVM(t, s, system, "db"), vmStopped)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply(user, customImage("db-img", "db-root")); err != nil {
		t.Fatalf("capturing a stopped VM's root disk: %v", err)
	}

	// A container mounting a disk that asks to run holds it too.
	madeDisk(t, s, system, user, "vol", "n1", "4Gi")
	c := ctr("app")
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{{Type: "volume", Source: "vol", Target: "/data"}}
	if _, err := s.backend(registry.Container).Apply(user, c); err != nil {
		t.Fatal(err)
	}
	_, err = b.Apply(user, customImage("vol-img", "vol"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "capturing a disk a running container mounts")
	if _, err := s.backend(registry.Container).Apply(user, stopped(c)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply(user, customImage("vol-img", "vol")); err != nil {
		t.Errorf("capturing a disk a stopped container mounts: %v", err)
	}
}

// TestASourceDiskIsHeldUntilSnapshotted: until its node has snapshotted
// it, nothing with the source disk starts, nothing attaches it, and it is
// not deleted; then all of that is allowed again.
func TestASourceDiskIsHeldUntilSnapshotted(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	images, vms, machines, disks := s.backend(registry.CustomImage), s.backend(registry.VirtualMachine), s.backend(registry.Machine), s.backend(registry.Disk)
	containers := s.backend(registry.Container)
	stoppedVM(t, s, system, user)
	madeDisk(t, s, system, user, "data", "n1", "4Gi")
	// A stopped container mounting a disk, captured too.
	madeDisk(t, s, system, user, "vol", "n1", "4Gi")
	app := stopped(mounting(ctr("app"), "vol"))
	if _, err := containers.Apply(user, app); err != nil {
		t.Fatal(err)
	}
	for _, ci := range []*storagev1alpha1.CustomImage{customImage("db-img", "db-root"), customImage("data-img", "data"), customImage("vol-img", "vol")} {
		if _, err := images.Apply(user, ci); err != nil {
			t.Fatal(err)
		}
	}

	_, err := containers.Apply(user, mounting(ctr("app"), "vol"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "starting a container mounting a disk being captured")
	_, err = containers.Apply(user, mounting(ctr("new"), "data"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a running container attaching a disk being captured")
	_, err = containers.Apply(user, stopped(mounting(ctr("new"), "data")))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a stopped container attaching a disk being captured")
	app.Spec.Env = []*computev1alpha1.EnvVar{{Name: "MODE", Value: "golden"}}
	if _, err := containers.Apply(user, app); err != nil {
		t.Errorf("changing a stopped container mounting a disk being captured: %v", err)
	}

	_, err = vms.Apply(user, vmWithRunState(getVM(t, s, system, "db"), vmRunning))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "starting a VM whose root disk is being captured")
	_, err = machines.Apply(system, vmMachine("db", "db-root", "n1", computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "starting a machine whose root disk is being captured")
	other := vmOfDisk("other", "boot")
	other.Spec.RunState = vmStopped
	other.Spec.Disks = []*computev1alpha1.VirtualMachineDisk{{Disk: "data"}}
	_, err = vms.Apply(user, other)
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a VM attaching a disk being captured")
	_, err = machines.Apply(system, withDisks(machine("ct-x", "n1"), "data"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a machine attaching a disk being captured")
	_, err = disks.Delete(user, resource.Key{Project: "web", Name: "data"}, nil)
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "deleting a disk being captured")
	// What leaves it be still changes.
	vm := getVM(t, s, system, "db")
	vm.Spec.UserData = "#cloud-config\n"
	if _, err := vms.Apply(user, vm); err != nil {
		t.Errorf("changing a stopped VM whose root disk is being captured: %v", err)
	}

	// Only the image's node reports on it, and only on its own part.
	_, err = images.UpdateStatus(asNode(system, "n2"), resource.Key{Project: "web", Name: "db-img"},
		&storagev1alpha1.CustomImageStatus{Conditions: []*metav1.Condition{snapshotted()}}, nil)
	wantCodeOf(t, err, connect.CodePermissionDenied, "another node reporting on the image")
	_, err = images.UpdateStatus(user, resource.Key{Project: "web", Name: "db-img"},
		&storagev1alpha1.CustomImageStatus{Conditions: []*metav1.Condition{snapshotted()}}, nil)
	wantCodeOf(t, err, connect.CodePermissionDenied, "the project's owner reporting on the image")
	if _, err := images.UpdateStatus(asNode(system, "n1"), resource.Key{Project: "web", Name: "db-img"},
		&storagev1alpha1.CustomImageStatus{Node: "n2", DiskSize: "1Gi", Conditions: []*metav1.Condition{snapshotted()}}, nil); err != nil {
		t.Fatal(err)
	}
	if st := getCustomImage(t, s, system, "db-img").GetStatus(); st.GetNode() != "n1" || st.GetDiskSize() != "10Gi" {
		t.Errorf("the node rewrote the apiserver's part: %v", st)
	}

	// Snapshotted, the VM and the container start.
	if _, err := vms.Apply(user, vmWithRunState(getVM(t, s, system, "db"), vmRunning)); err != nil {
		t.Errorf("starting a VM whose root disk is snapshotted: %v", err)
	}
	reportCustomImage(t, s, system, "vol-img", "", snapshotted())
	if _, err := containers.Apply(user, mounting(ctr("app"), "vol")); err != nil {
		t.Errorf("starting a container mounting a disk snapshotted: %v", err)
	}
	if _, err := machines.Apply(system, vmMachine("db", "db-root", "n1", computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING)); err != nil {
		t.Errorf("starting a machine whose root disk is snapshotted: %v", err)
	}
	// A capture that failed lets its disk go too.
	reportCustomImage(t, s, system, "data-img", "", captureFailed())
	if _, err := vms.Apply(user, other); err != nil {
		t.Errorf("attaching a disk whose capture failed: %v", err)
	}
	other.Spec.Disks = nil
	if _, err := vms.Apply(user, other); err != nil {
		t.Fatal(err)
	}
	if _, err := disks.Delete(user, resource.Key{Project: "web", Name: "data"}, nil); err != nil {
		t.Errorf("deleting a disk whose capture failed: %v", err)
	}
}

// TestADeletedImageLetsItsSourceGo: deleting an image still holding its
// disk ends the hold.
func TestADeletedImageLetsItsSourceGo(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	stoppedVM(t, s, system, user)
	if _, err := s.backend(registry.CustomImage).Apply(user, customImage("db-img", "db-root")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.CustomImage).Delete(user, resource.Key{Project: "web", Name: "db-img"}, nil); err != nil {
		t.Fatal(err)
	}
	if getCustomImage(t, s, system, "db-img") != nil {
		t.Fatal("the image is still there: it needs no finalizer")
	}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vmWithRunState(getVM(t, s, system, "db"), vmRunning)); err != nil {
		t.Errorf("starting a VM whose image was deleted: %v", err)
	}
}

// raceCaptures runs write and n captures of disk at once, and fails
// unless exactly one side wins: write, or one capture or more, as several
// images of one disk may be made. It then undoes what won: undo for write,
// a delete for each capture.
func raceCaptures(t *testing.T, s *Server, user context.Context, round int, disk, what string, write func() error, undo func()) {
	t.Helper()
	const n = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	var writeErr error
	captureErrs := make([]error, n)
	wg.Go(func() { <-start; writeErr = write() })
	for i := range n {
		wg.Go(func() {
			<-start
			_, captureErrs[i] = s.backend(registry.CustomImage).Apply(user, customImage(fmt.Sprintf("%s-%d-%d", disk, round, i), disk))
		})
	}
	close(start)
	wg.Wait()
	captured := false
	for i, err := range captureErrs {
		if err != nil {
			continue
		}
		captured = true
		if _, err := s.backend(registry.CustomImage).Delete(user, resource.Key{Project: "web", Name: fmt.Sprintf("%s-%d-%d", disk, round, i)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if (writeErr == nil) == captured {
		t.Fatalf("round %d: %s %v, captures %v; exactly one side must win", round, what, writeErr, captureErrs)
	}
	if writeErr == nil {
		undo()
	}
}

// TestCaptureAndStartAreOneDecision: captures racing a start of what has
// their source disk, or a running machine attaching it, only one side wins;
// for machines, which the apiserver writes, the quota serializes nothing.
func TestCaptureAndStartAreOneDecision(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	vms, machines := s.backend(registry.VirtualMachine), s.backend(registry.Machine)
	stoppedVM(t, s, system, user)
	madeDisk(t, s, system, user, "data", "n1", "4Gi")
	attacher := vmOfDisk("attacher", "boot")
	must := func(_ resource.ApplyResult, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := range 10 {
		raceCaptures(t, s, user, i, "db-root", "starting the VM",
			func() error {
				_, err := vms.Apply(user, vmWithRunState(getVM(t, s, system, "db"), vmRunning))
				return err
			},
			func() { must(vms.Apply(user, vmWithRunState(getVM(t, s, system, "db"), vmStopped))) })
		raceCaptures(t, s, user, i, "db-root", "starting the machine",
			func() error {
				_, err := machines.Apply(system, vmMachine("db", "db-root", "n1", computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING))
				return err
			},
			func() {
				res, err := machines.Apply(system, vmMachine("db", "db-root", "n1", computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED))
				must(res, err)
				m := res.Object.(*computev1alpha1.Machine)
				reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration())
			})
		raceCaptures(t, s, user, i, "data", "a running VM attaching it",
			func() error {
				a := proto.Clone(attacher).(*computev1alpha1.VirtualMachine)
				a.Spec.Disks = []*computev1alpha1.VirtualMachineDisk{{Disk: "data"}}
				_, err := vms.Apply(user, a)
				return err
			},
			func() { must(vms.Apply(user, attacher)) })
		raceCaptures(t, s, user, i, "data", "a running machine attaching it",
			func() error { _, err := machines.Apply(system, withDisks(machine("ct-x", "n1"), "data")); return err },
			func() { must(machines.Apply(system, machine("ct-x", "n1"))) })
		raceCaptures(t, s, user, i, "data", "a running container attaching it",
			func() error {
				_, err := s.backend(registry.Container).Apply(user, mounting(ctr("app"), "data"))
				return err
			},
			func() { must(s.backend(registry.Container).Apply(user, ctr("app"))) })
	}
}

func TestADiskFromACustomImage(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	disks, images := s.backend(registry.Disk), s.backend(registry.CustomImage)
	stoppedVM(t, s, system, user)
	ci := customImage("db-img", "db-root")
	ci.Spec.MinDiskSize = "12Gi"
	if _, err := images.Apply(user, ci); err != nil {
		t.Fatal(err)
	}
	fromImage := func(name, size, project, img string) *storagev1alpha1.Disk {
		d := diskOf(name, size, img)
		d.Spec.Source.GetImage().Project = project
		return d
	}

	_, err := disks.Apply(user, fromImage("copy", "20Gi", "web", "db-img"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a disk of an image being captured")
	reportCustomImage(t, s, system, "db-img", "", snapshotted())
	_, err = disks.Apply(user, fromImage("copy", "20Gi", "web", "db-img"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a disk of an image snapshotted but not Ready")
	reportCustomImage(t, s, system, "db-img", "3Gi", imageReady())

	_, err = disks.Apply(user, fromImage("copy", "20Gi", "other", "db-img"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a disk of another project's image")
	if err == nil || !strings.Contains(err.Error(), "its own project") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	_, err = disks.Apply(user, fromImage("copy", "20Gi", "web", "nope"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a disk of a custom image that does not exist")
	_, err = disks.Apply(user, fromImage("copy", "11Gi", "web", "db-img"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a disk below the image's min_disk_size")
	// A global image of the same name is another image.
	_, err = disks.Apply(user, fromImage("copy", "20Gi", "", "db-img"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a disk of a global image named as the custom one")

	forged := fromImage("copy", "12Gi", "web", "db-img")
	forged.Metadata.Annotations = map[string]string{computeapi.CustomImageOf: "another-image"}
	_, err = disks.Apply(user, forged)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a disk naming its custom image's uid itself")
	if _, err := disks.Apply(user, fromImage("copy", "12Gi", "web", "db-img")); err != nil {
		t.Fatalf("a disk of a Ready custom image: %v", err)
	}
	// It carries the image's uid, which names its base even once the
	// image is gone, and which no apply drops.
	uid := getCustomImage(t, s, system, "db-img").GetMetadata().GetUid()
	if got := getDisk(t, s, system, "copy").GetMetadata().GetAnnotations()[computeapi.CustomImageOf]; got != uid {
		t.Errorf("the disk names custom image %q, want %q", got, uid)
	}
	relabelled := fromImage("copy", "12Gi", "web", "db-img")
	relabelled.Metadata.Labels = map[string]string{"tier": "db"}
	if _, err := disks.Apply(user, relabelled); err != nil {
		t.Fatal(err)
	}
	if got := getDisk(t, s, system, "copy").GetMetadata().GetAnnotations()[computeapi.CustomImageOf]; got != uid {
		t.Errorf("an apply without it left the disk naming %q, want %q", got, uid)
	}
	vm := vmFromImage("clone", "db-img")
	vm.Spec.RootDisk.GetFromImage().GetImage().Project = "web"
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Fatalf("a VM of a Ready custom image: %v", err)
	}
	if u, err := s.defaultUser(system, getVM(t, s, system, "clone")); err != nil || u != "ubuntu" {
		t.Errorf("the default user of a VM of a custom image: %q, %v", u, err)
	}

	// Deleted, no new disk is made of it; one made of it still grows.
	if _, err := images.Delete(user, resource.Key{Project: "web", Name: "db-img"}, nil); err != nil {
		t.Fatal(err)
	}
	_, err = disks.Apply(user, fromImage("again", "20Gi", "web", "db-img"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a disk of a deleted custom image")
	if _, err := disks.Apply(user, fromImage("copy", "30Gi", "web", "db-img")); err != nil {
		t.Errorf("growing a disk of a deleted custom image: %v", err)
	}
}

// TestADiskOfACustomImageAndItsDeleteAreOneDecision: disks of a Ready
// image created while the image is deleted are each stored before the
// delete, or refused; none is stored once the image is gone, which the
// node's sweep would take as no disk of the image, and remove its base.
func TestADiskOfACustomImageAndItsDeleteAreOneDecision(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	disks, images := s.backend(registry.Disk), s.backend(registry.CustomImage)
	stoppedVM(t, s, system, user)
	errFound := errors.New("found")
	for round := range 20 {
		name := fmt.Sprintf("img-%d", round)
		if _, err := images.Apply(user, customImage(name, "db-root")); err != nil {
			t.Fatal(err)
		}
		reportCustomImage(t, s, system, name, "3Gi", snapshotted(), imageReady())
		uid := getCustomImage(t, s, system, name).GetMetadata().GetUid()
		since, err := s.store.Revision(system)
		if err != nil {
			t.Fatal(err)
		}

		const n = 4
		var wg sync.WaitGroup
		start := make(chan struct{})
		var deleteErr error
		made := make([]*storagev1alpha1.Disk, n)
		createErrs := make([]error, n)
		wg.Go(func() {
			<-start
			_, deleteErr = images.Delete(user, resource.Key{Project: "web", Name: name}, nil)
		})
		for i := range n {
			wg.Go(func() {
				<-start
				d := diskOf(fmt.Sprintf("%s-%d", name, i), "12Gi", name)
				d.Spec.Source.GetImage().Project = "web"
				res, err := disks.Apply(user, d)
				createErrs[i] = err
				if err == nil {
					made[i] = res.Object.(*storagev1alpha1.Disk)
				}
			})
		}
		close(start)
		wg.Wait()
		if deleteErr != nil {
			t.Fatal(deleteErr)
		}

		// The revision the image's delete was stored at.
		var deleted int64
		err = s.store.Watch(system, store.WatchOptions{Kind: registry.CustomImage.Kind, Project: "web", SinceRevision: since}, func(e store.Event) error {
			if e.Type == store.EventDeleted && e.Object.Name == name {
				deleted = e.Revision
				return errFound
			}
			return nil
		})
		if !errors.Is(err, errFound) {
			t.Fatal(err)
		}
		for i, d := range made {
			if d == nil {
				wantCodeOf(t, createErrs[i], connect.CodeFailedPrecondition, "a disk of an image being deleted")
				continue
			}
			if got := d.GetMetadata().GetAnnotations()[computeapi.CustomImageOf]; got != uid {
				t.Errorf("round %d: disk %s names custom image %q, want %q", round, d.GetMetadata().GetName(), got, uid)
			}
			if rv := d.GetMetadata().GetResourceVersion(); rv > deleted {
				t.Fatalf("round %d: disk %s of image %s stored at revision %d, after the image's delete at %d",
					round, d.GetMetadata().GetName(), name, rv, deleted)
			}
			if _, err := disks.Delete(user, resource.Key{Project: "web", Name: d.GetMetadata().GetName()}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestCustomImageQuota: an image counts its source disk's size while it
// is captured, what it holds once Ready, nothing once failed or deleted,
// and toward custom_images.
func TestCustomImageQuota(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	images := s.backend(registry.CustomImage)
	madeDisk(t, s, system, user, "a", "n1", "10Gi")
	madeDisk(t, s, system, user, "b", "n1", "5Gi")
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Disk: proto.String("25Gi"), CustomImages: proto.Int32(2)})
	used := func(what string, disk, count int64) {
		t.Helper()
		got, err := s.tally(system, "acme", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.tenant[dimDisk] != disk*int64(quantity.GiB) || got.tenant[dimCustomImages] != count {
			t.Errorf("%s: %s of disk and %d images, want %dGi and %d", what,
				formatAmount(dimDisk, got.tenant[dimDisk]), got.tenant[dimCustomImages], disk, count)
		}
	}
	used("two disks", 15, 0)

	if _, err := images.Apply(user, customImage("ia", "a")); err != nil {
		t.Fatal(err)
	}
	used("an image being captured, reserving its source's size", 25, 1)
	_, err := images.Apply(user, customImage("ib", "b"))
	wantExhausted(t, err, "disk limit", "a second capture past the disk limit")

	reportCustomImage(t, s, system, "ia", "2Gi", snapshotted(), imageReady())
	used("the image Ready, settled at what it holds", 17, 1)
	if _, err := images.Apply(user, customImage("ib", "b")); err != nil {
		t.Fatalf("a second capture once the first settled: %v", err)
	}
	used("two images", 22, 2)
	reportCustomImage(t, s, system, "ib", "1Gi", snapshotted(), imageReady())
	used("two images Ready", 18, 2)
	_, err = images.Apply(user, customImage("ic", "b"))
	wantExhausted(t, err, "custom_images limit", "a third image past the count limit")

	reportCustomImage(t, s, system, "ia", "", captureFailed())
	used("an image failed, released", 16, 1)
	if _, err := images.Apply(user, customImage("ic", "b")); err != nil {
		t.Fatalf("an image once another failed: %v", err)
	}
	used("a new capture", 21, 2)
	if _, err := images.Delete(user, resource.Key{Project: "web", Name: "ic"}, nil); err != nil {
		t.Fatal(err)
	}
	used("the capture deleted, released", 16, 1)
}
