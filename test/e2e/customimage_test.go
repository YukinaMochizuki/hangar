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
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

const (
	vmStopped = computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED
	vmRunning = computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_RUNNING
)

// captureImage makes the CustomImage name of disk.
func (w *world) captureImage(name, disk string) error {
	w.t.Helper()
	_, err := w.root.Apply(context.Background(), registry.CustomImage, &storagev1alpha1.CustomImage{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec: &storagev1alpha1.CustomImageSpec{
			Source: &storagev1alpha1.CustomImageSource{Source: &storagev1alpha1.CustomImageSource_Disk{
				Disk: &storagev1alpha1.CustomImageDiskSource{Name: disk},
			}},
			DefaultUser: "ubuntu",
		},
	})
	return err
}

func (w *world) waitCustomImage(name, what string, ok func(*storagev1alpha1.CustomImage) bool) *storagev1alpha1.CustomImage {
	w.t.Helper()
	var ci *storagev1alpha1.CustomImage
	testutil.Eventually(w.t, "custom image "+name+" "+what, func() (bool, string) {
		var found bool
		ci, found = get[*storagev1alpha1.CustomImage](w, registry.CustomImage, name)
		return found && ok(ci), fmt.Sprintf("%v", ci.GetStatus())
	})
	return ci
}

func imageCondition(ci *storagev1alpha1.CustomImage, typ string) *metav1.Condition {
	return conditions.Find(ci.GetStatus().GetConditions(), typ)
}

// applyVMOfCustomImage applies the VM name, its root disk of size made from
// the project's CustomImage img.
func (w *world) applyVMOfCustomImage(name, img, size string, runState computev1alpha1.VirtualMachineRunState) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.VirtualMachine, &computev1alpha1.VirtualMachine{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec: &computev1alpha1.VirtualMachineSpec{
			RootDisk: &computev1alpha1.VirtualMachineRootDisk{Source: &computev1alpha1.VirtualMachineRootDisk_FromImage{
				FromImage: &computev1alpha1.VirtualMachineImageDisk{Image: &storagev1alpha1.ImageSource{Name: img, Project: project}, Size: size},
			}},
			RunState: runState,
		},
	}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) deleteVM(name string) {
	w.t.Helper()
	if _, err := w.root.Delete(context.Background(), registry.VirtualMachine, client.Key{Project: project, Name: name}); err != nil {
		w.t.Fatal(err)
	}
	testutil.Eventually(w.t, "virtual machine "+name+", its machine and its root disk gone", func() (bool, string) {
		_, v := get[*computev1alpha1.VirtualMachine](w, registry.VirtualMachine, name)
		_, m := get[*computev1alpha1.Machine](w, registry.Machine, vmMachineOf(name))
		_, d := get[*storagev1alpha1.Disk](w, registry.Disk, name+"-root")
		return !v && !m && !d, fmt.Sprintf("vm %t machine %t disk %t", v, m, d)
	})
}

// projectUsage is the usage the project's status shows.
func (w *world) projectUsage() *tenancyv1alpha1.Usage {
	w.t.Helper()
	obj, err := w.root.Get(context.Background(), registry.Project, client.Key{Name: project})
	if err != nil {
		w.t.Fatal(err)
	}
	return obj.(*tenancyv1alpha1.Project).GetStatus().GetUsage()
}

// TestACustomImagesLife: a stopped VM's root disk is captured; the VM
// starts again as soon as the disk is snapshotted, while the copy goes on;
// the image, Ready, makes a VM of its own, which outlives the source VM
// and the image; the image's base goes only once the last disk of it does.
func TestACustomImagesLife(t *testing.T) {
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	w.applyVM("src", 0)
	w.waitVMReason("src", "Running")
	if err := w.captureImage("golden", "src-root"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("capturing a running VM's disk: %v, want failed_precondition", err)
	}

	w.applyVM("src", vmStopped)
	w.waitVMReason("src", computeapi.ReasonStopped)
	release := w.host.HoldCaptures()
	if err := w.captureImage("golden", "src-root"); err != nil {
		t.Fatalf("capturing a stopped VM's disk: %v", err)
	}
	ci := w.waitCustomImage("golden", "snapshotted, its copy held", func(ci *storagev1alpha1.CustomImage) bool {
		return conditions.IsTrue(ci.GetStatus().GetConditions(), computeapi.CustomImageSnapshotted) &&
			imageCondition(ci, computeapi.CustomImageReady).GetReason() == computeapi.ReasonCapturing
	})
	uid := ci.GetMetadata().GetUid()
	if s := w.host.Snapshot(); !slices.Equal(s.Captures, []string{uid}) {
		t.Errorf("snapshots for custom images %v, want %s's", s.Captures, uid)
	}
	if st := ci.GetStatus(); st.GetNode() == "" || st.GetDiskSize() != "10Gi" {
		t.Errorf("status %v, want a node and disk_size 10Gi", st)
	}
	// Snapshotted, its source is free again, whatever the copy does.
	w.applyVM("src", vmRunning)
	w.waitVMReason("src", "Running")
	release()

	ci = w.waitCustomImage("golden", "Ready", computeapi.CustomImageReadyNow)
	key := driver.CustomImageBase(uid)
	if ci.GetStatus().GetUsedSize() != "5Gi" {
		t.Errorf("used_size %q, want 5Gi, half the source as the fake holds it", ci.GetStatus().GetUsedSize())
	}
	if s := w.host.Snapshot(); len(s.Captures) != 0 || !slices.Contains(s.ImageBases, key) {
		t.Errorf("snapshots %v and image bases %v once Ready", s.Captures, s.ImageBases)
	}
	testutil.Eventually(w.t, "the project's usage to count the image as what it holds", func() (bool, string) {
		u := w.projectUsage()
		// The source VM's 10Gi root disk and the image's 5Gi.
		return u.GetCustomImages() == 1 && u.GetDisk() == "15Gi", fmt.Sprintf("%v", u)
	})

	// A VM of the image boots from a clone of its base.
	w.applyVMOfCustomImage("copy", "golden", "12Gi", 0)
	w.waitVMReason("copy", "Running")
	copyRoot, _ := get[*storagev1alpha1.Disk](w, registry.Disk, "copy-root")
	if vol := w.host.Snapshot().Volumes[copyRoot.GetMetadata().GetUid()]; vol.Digest != key || vol.Size < 12*gib {
		t.Errorf("the copy's root volume %+v, want a clone of %s", vol, key)
	}

	// The source goes; the image and its VM stay, which stops and starts.
	w.deleteVM("src")
	w.applyVMOfCustomImage("copy", "golden", "12Gi", vmStopped)
	w.waitVMReason("copy", computeapi.ReasonStopped)
	w.applyVMOfCustomImage("copy", "golden", "12Gi", vmRunning)
	w.waitVMReason("copy", "Running")

	// The image goes: no disk is made of it any more, and its base stays
	// for the disk made of it.
	if _, err := w.root.Delete(context.Background(), registry.CustomImage, client.Key{Project: project, Name: "golden"}); err != nil {
		t.Fatal(err)
	}
	_, err := w.root.Apply(context.Background(), registry.Disk, &storagev1alpha1.Disk{
		Metadata: &metav1.ObjectMeta{Project: project, Name: "again"},
		Spec: &storagev1alpha1.DiskSpec{Size: "12Gi", Source: &storagev1alpha1.DiskSource{Source: &storagev1alpha1.DiskSource_Image{
			Image: &storagev1alpha1.ImageSource{Name: "golden", Project: project},
		}}},
	})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a disk of a deleted custom image: %v, want failed_precondition", err)
	}
	w.waitSettled()
	w.sweeps(3)
	if bases := w.host.Snapshot().ImageBases; !slices.Contains(bases, key) {
		t.Fatalf("image bases %v: the base went while a disk is cloned from it", bases)
	}
	if u := w.projectUsage(); u.GetCustomImages() != 0 {
		t.Errorf("a deleted image still counts: %v", u)
	}

	// The last disk of it gone, so is its base.
	w.deleteVM("copy")
	testutil.Eventually(w.t, "the custom image's base gone", func() (bool, string) {
		bases := w.host.Snapshot().ImageBases
		return !slices.Contains(bases, key), fmt.Sprint(bases)
	})
	w.deleteImage("noble")
	w.waitHostEmpty()
}

// TestACaptureThatFails fails for good: what it made goes, its source is
// free, it holds no quota, and it is not tried again.
func TestACaptureThatFails(t *testing.T) {
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	w.applyVM("src", 0)
	w.waitVMReason("src", "Running")
	w.host.FailCapture(driver.ErrNoSpace)
	w.applyVM("src", vmStopped)
	w.waitVMReason("src", computeapi.ReasonStopped)
	if err := w.captureImage("golden", "src-root"); err != nil {
		t.Fatal(err)
	}
	ci := w.waitCustomImage("golden", "failed", computeapi.CustomImageFailed)
	if msg := imageCondition(ci, computeapi.CustomImageReady).GetMessage(); !strings.Contains(msg, driver.ErrNoSpace.Error()) {
		t.Errorf("the failure does not say what failed: %v", ci.GetStatus())
	}
	if s := w.host.Snapshot(); len(s.Captures) != 0 || slices.Contains(s.ImageBases, driver.CustomImageBase(ci.GetMetadata().GetUid())) {
		t.Errorf("a failed capture left snapshots %v and image bases %v", s.Captures, s.ImageBases)
	}
	testutil.Eventually(w.t, "the project's usage to count the failed image as nothing", func() (bool, string) {
		u := w.projectUsage()
		return u.GetCustomImages() == 0 && u.GetDisk() == "10Gi", fmt.Sprintf("%v", u)
	})
	w.host.FailCapture(nil)
	w.waitSettled()
	w.sweeps(2)
	if got := w.waitCustomImage("golden", "still failed", computeapi.CustomImageFailed); !sameVersion(got, ci) {
		t.Errorf("a failed capture was tried again: %v", got.GetStatus())
	}
	w.applyVM("src", vmRunning)
	w.waitVMReason("src", "Running")
	if _, err := w.root.Delete(context.Background(), registry.CustomImage, client.Key{Project: project, Name: "golden"}); err != nil {
		t.Fatal(err)
	}
	w.deleteVM("src")
	w.deleteImage("noble")
	w.waitHostEmpty()
}

// TestACaptureThatStallsFails: a copy that takes longer than the agent's
// capture timeout fails for good, saying so, and leaves nothing.
func TestACaptureThatStallsFails(t *testing.T) {
	w := newIdleWorld(t)
	w.captureTimeout = time.Second
	w.startAgent()
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	w.applyVM("src", 0)
	w.waitVMReason("src", "Running")
	w.applyVM("src", vmStopped)
	w.waitVMReason("src", computeapi.ReasonStopped)
	release := w.host.HoldCaptures()
	defer release()
	if err := w.captureImage("golden", "src-root"); err != nil {
		t.Fatal(err)
	}
	ci := w.waitCustomImage("golden", "failed", computeapi.CustomImageFailed)
	if msg := imageCondition(ci, computeapi.CustomImageReady).GetMessage(); !strings.Contains(msg, "took longer than 1s") {
		t.Errorf("the failure does not say the copy took too long: %q", msg)
	}
	testutil.Eventually(w.t, "the stalled copy ended and its snapshot gone", func() (bool, string) {
		s, n := w.host.Snapshot(), w.host.CapturesUnderWay()
		return n == 0 && len(s.Captures) == 0 && !slices.Contains(s.ImageBases, driver.CustomImageBase(ci.GetMetadata().GetUid())),
			fmt.Sprintf("%d under way, %v %v", n, s.Captures, s.ImageBases)
	})
	if _, err := w.root.Delete(context.Background(), registry.CustomImage, client.Key{Project: project, Name: "golden"}); err != nil {
		t.Fatal(err)
	}
	w.deleteVM("src")
	w.deleteImage("noble")
	w.waitHostEmpty()
}

// sameVersion says a and b are at the same resource version.
func sameVersion(a, b *storagev1alpha1.CustomImage) bool {
	return a.GetMetadata().GetResourceVersion() == b.GetMetadata().GetResourceVersion()
}

// TestACaptureOutlivesItsAgent: an agent stopped mid-copy copies again from
// the snapshot it kept once it is back; and a capture whose image is
// deleted mid-copy leaves nothing behind.
func TestACaptureOutlivesItsAgent(t *testing.T) {
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	w.applyVM("src", 0)
	w.waitVMReason("src", "Running")
	w.applyVM("src", vmStopped)
	w.waitVMReason("src", computeapi.ReasonStopped)

	release := w.host.HoldCaptures()
	if err := w.captureImage("golden", "src-root"); err != nil {
		t.Fatal(err)
	}
	w.waitCustomImage("golden", "snapshotted", func(ci *storagev1alpha1.CustomImage) bool {
		return conditions.IsTrue(ci.GetStatus().GetConditions(), computeapi.CustomImageSnapshotted)
	})
	w.stopAgent()
	if s := w.host.Snapshot(); len(s.Captures) != 1 {
		t.Fatalf("snapshots for custom images %v once the agent stopped mid-copy, want the one", s.Captures)
	}
	// The source changes meanwhile; the image is what was snapshotted.
	w.applyVM("src", vmRunning)
	release()
	w.startAgent()
	ci := w.waitCustomImage("golden", "Ready", computeapi.CustomImageReadyNow)
	if s := w.host.Snapshot(); len(s.Captures) != 0 || !slices.Contains(s.ImageBases, driver.CustomImageBase(ci.GetMetadata().GetUid())) {
		t.Errorf("snapshots %v and image bases %v once Ready", s.Captures, s.ImageBases)
	}
	w.waitVMReason("src", "Running")

	// A capture whose image is deleted mid-copy is let go.
	w.applyVM("src", vmStopped)
	w.waitVMReason("src", computeapi.ReasonStopped)
	release = w.host.HoldCaptures()
	defer release()
	if err := w.captureImage("doomed", "src-root"); err != nil {
		t.Fatal(err)
	}
	doomed := w.waitCustomImage("doomed", "snapshotted", func(ci *storagev1alpha1.CustomImage) bool {
		return conditions.IsTrue(ci.GetStatus().GetConditions(), computeapi.CustomImageSnapshotted)
	})
	if _, err := w.root.Delete(context.Background(), registry.CustomImage, client.Key{Project: project, Name: "doomed"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(w.t, "the deleted image's copy ended, its snapshot and base gone", func() (bool, string) {
		s := w.host.Snapshot()
		n := w.host.CapturesUnderWay()
		return n == 0 && len(s.Captures) == 0 && !slices.Contains(s.ImageBases, driver.CustomImageBase(doomed.GetMetadata().GetUid())),
			fmt.Sprintf("%d under way, %v %v", n, s.Captures, s.ImageBases)
	})

	if _, err := w.root.Delete(context.Background(), registry.CustomImage, client.Key{Project: project, Name: "golden"}); err != nil {
		t.Fatal(err)
	}
	w.deleteVM("src")
	w.deleteImage("noble")
	w.waitHostEmpty()
}
