package e2e

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

const cloudURL = "https://images.example/noble.img"

var (
	cloudSHA   = strings.Repeat("1a", 32)
	cloudFixed = strings.Repeat("2b", 32)
)

func (w *world) applyImage(name, sha string) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.Image, &storagev1alpha1.Image{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec:     &storagev1alpha1.ImageSpec{Url: cloudURL, Sha256: sha, Os: "ubuntu", Version: "24.04"},
	}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) applyDiskOf(name, size, img string) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.Disk, &storagev1alpha1.Disk{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec: &storagev1alpha1.DiskSpec{Size: size, Source: &storagev1alpha1.DiskSource{
			Source: &storagev1alpha1.DiskSource_Image{Image: &storagev1alpha1.ImageSource{Name: img}},
		}},
	}); err != nil {
		w.t.Fatal(err)
	}
}

// deleteImage deletes an Image, which lets the node remove its base.
func (w *world) deleteImage(name string) {
	w.t.Helper()
	if _, err := w.root.Delete(context.Background(), registry.Image, client.Key{Name: name}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) image(name string) *storagev1alpha1.Image {
	w.t.Helper()
	obj, err := w.root.Get(context.Background(), registry.Image, client.Key{Name: name})
	if err != nil {
		w.t.Fatal(err)
	}
	return obj.(*storagev1alpha1.Image)
}

func diskReady(d *storagev1alpha1.Disk) *metav1.Condition {
	return conditions.Find(d.GetStatus().GetConditions(), computeapi.DiskReady)
}

// TestADiskMadeFromAnImage: the image is fetched once, made a base of, and
// cloned for each disk of it; the machine with the disk waits for it; the
// base stays while its Image does, so a disk made after every other is gone
// needs no fetch, and goes once the Image names another digest or is
// deleted and no disk is made of it.
func TestADiskMadeFromAnImage(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	w.applyDiskOf("root", "10Gi", "noble")
	w.putMachine(diskMachine("vm-a", "root"))

	d := w.waitDisk("root", "in use", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetPhase() == storagev1alpha1.DiskPhase_DISK_PHASE_IN_USE
	})
	vol := w.host.Snapshot().Volumes[d.GetMetadata().GetUid()]
	if vol.Digest != "sha256:"+cloudSHA || vol.Size < 10*gib {
		t.Errorf("volume %+v", vol)
	}
	if bases := w.host.Snapshot().ImageBases; !slices.Equal(bases, []string{"sha256:" + cloudSHA}) {
		t.Errorf("image bases %v", bases)
	}
	testutil.Eventually(w.t, "the VM running with the disk", func() (bool, string) {
		_, spec, running := w.vmOf("vm-a")
		return running && len(spec.Disks) == 1 && spec.Disks[0].Digest == "sha256:"+cloudSHA, fmt.Sprintf("%+v", spec.Disks)
	})
	img := w.image("noble")
	if v := conditions.Find(img.GetStatus().GetConditions(), computeapi.ImageVerified); v.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE ||
		img.GetStatus().GetDiskSize() != "3Gi" {
		t.Errorf("image status %v", img.GetStatus())
	}

	// Another disk of it clones the same base.
	w.applyDiskOf("other", "4Gi", "noble")
	w.putMachine(diskMachine("vm-b", "other"))
	w.waitDisk("other", "made", func(d *storagev1alpha1.Disk) bool {
		return conditions.IsTrue(d.GetStatus().GetConditions(), computeapi.DiskReady)
	})
	if n := w.host.Fetches(cloudURL); n != 1 {
		t.Errorf("the image was fetched %d times", n)
	}

	// The disks gone, the base stays for the Image.
	for _, m := range []string{"vm-a", "vm-b"} {
		if _, err := w.system.Delete(context.Background(), registry.Machine, client.Key{Project: project, Name: m}); err != nil {
			t.Fatal(err)
		}
	}
	testutil.Eventually(w.t, "the machines gone", func() (bool, string) {
		_, a := get[*computev1alpha1.Machine](w, registry.Machine, "vm-a")
		_, b := get[*computev1alpha1.Machine](w, registry.Machine, "vm-b")
		return !a && !b, ""
	})
	for _, name := range []string{"root", "other"} {
		if _, err := w.root.Delete(context.Background(), registry.Disk, client.Key{Project: project, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	testutil.Eventually(w.t, "no volume left", func() (bool, string) {
		s := w.host.Snapshot()
		return len(s.Volumes) == 0, fmt.Sprintf("%+v", s.Volumes)
	})
	// Several sweeps, once the disks' own work is done.
	w.waitSettled()
	w.sweeps(3)
	if bases := w.host.Snapshot().ImageBases; !slices.Equal(bases, []string{"sha256:" + cloudSHA}) {
		t.Fatalf("image bases with no disk of the image: %v", bases)
	}

	// A disk made after them all clones the kept base: no fetch.
	w.applyDiskOf("again", "4Gi", "noble")
	w.putMachine(diskMachine("vm-c", "again"))
	w.waitDisk("again", "made", func(d *storagev1alpha1.Disk) bool {
		return conditions.IsTrue(d.GetStatus().GetConditions(), computeapi.DiskReady)
	})
	if n := w.host.Fetches(cloudURL); n != 1 {
		t.Errorf("the image was fetched %d times", n)
	}

	// A new digest for the Image: the old base goes once no disk is
	// cloned from it.
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudFixed, 3*gib)
	w.applyImage("noble", cloudFixed)
	w.deleteMachineAndDisk("vm-c", "again")
	testutil.Eventually(w.t, "the old digest's base gone", func() (bool, string) {
		bases := w.host.Snapshot().ImageBases
		return len(bases) == 0, fmt.Sprint(bases)
	})

	// A base of the new digest stays until the Image is deleted.
	w.applyDiskOf("fixed", "4Gi", "noble")
	w.putMachine(diskMachine("vm-d", "fixed"))
	w.waitDisk("fixed", "made", func(d *storagev1alpha1.Disk) bool {
		return conditions.IsTrue(d.GetStatus().GetConditions(), computeapi.DiskReady)
	})
	w.deleteMachineAndDisk("vm-d", "fixed")
	testutil.Eventually(w.t, "no volume left", func() (bool, string) {
		s := w.host.Snapshot()
		return len(s.Volumes) == 0, fmt.Sprintf("%+v", s.Volumes)
	})
	// Several sweeps, once the disks' own work is done.
	w.waitSettled()
	w.sweeps(3)
	if bases := w.host.Snapshot().ImageBases; !slices.Equal(bases, []string{"sha256:" + cloudFixed}) {
		t.Fatalf("image bases with no disk of the image: %v", bases)
	}
	w.deleteImage("noble")
	w.waitHostEmpty()
}

// deleteMachineAndDisk deletes a machine, waits for it to be gone, then
// deletes its disk.
func (w *world) deleteMachineAndDisk(machine, disk string) {
	w.t.Helper()
	if _, err := w.system.Delete(context.Background(), registry.Machine, client.Key{Project: project, Name: machine}); err != nil {
		w.t.Fatal(err)
	}
	testutil.Eventually(w.t, "the machine gone", func() (bool, string) {
		_, ok := get[*computev1alpha1.Machine](w, registry.Machine, machine)
		return !ok, ""
	})
	if _, err := w.root.Delete(context.Background(), registry.Disk, client.Key{Project: project, Name: disk}); err != nil {
		w.t.Fatal(err)
	}
}

// TestAnImageAtFault is never fetched again until its spec changes; a
// failure that may pass is tried again.
func TestAnImageAtFault(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudFixed, gib)
	w.applyImage("noble", cloudSHA) // not what the server has
	w.applyDiskOf("root", "10Gi", "noble")
	w.putMachine(diskMachine("vm-a", "root"))

	w.waitDisk("root", "told the image is at fault", func(d *storagev1alpha1.Disk) bool {
		return diskReady(d).GetReason() == computeapi.ReasonImagePullFailed && strings.Contains(diskReady(d).GetMessage(), "sha256")
	})
	testutil.Eventually(w.t, "the image marked at fault", func() (bool, string) {
		v := conditions.Find(w.image("noble").GetStatus().GetConditions(), computeapi.ImageVerified)
		return v.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE && v.GetReason() == computeapi.ReasonHashMismatch, fmt.Sprintf("%v", v)
	})
	testutil.Eventually(w.t, "the machine told its disk is not made", func() (bool, string) {
		mc := w.machine2("vm-a")
		r := conditions.Find(mc.GetStatus().GetConditions(), computeapi.MachineReady)
		return r.GetReason() == computeapi.ReasonDiskNotReady, fmt.Sprintf("%v", r)
	})
	fetched := w.host.Fetches(cloudURL)
	// Its disk, failing, is tried again: the image is not fetched for it.
	w.reconciled("disks", controller.Key{Project: project, Name: "root"}, 2)
	if n := w.host.Fetches(cloudURL); n != fetched || n != 1 {
		t.Errorf("an image at fault was fetched %d times, then %d", fetched, n)
	}

	// Fixed, it is fetched again, after a failure that passes.
	w.host.FailDiskImage(cloudURL, errors.New("connection reset"))
	w.applyImage("noble", cloudFixed)
	testutil.Eventually(w.t, "the fixed image fetched", func() (bool, string) {
		n := w.host.Fetches(cloudURL)
		return n >= 2, fmt.Sprint(n)
	})
	w.host.FailDiskImage(cloudURL, nil)
	w.waitDisk("root", "made", func(d *storagev1alpha1.Disk) bool {
		return conditions.IsTrue(d.GetStatus().GetConditions(), computeapi.DiskReady)
	})
	testutil.Eventually(w.t, "the VM running with the disk", func() (bool, string) {
		_, spec, running := w.vmOf("vm-a")
		return running && len(spec.Disks) == 1, fmt.Sprintf("%+v", spec.Disks)
	})
	if v := conditions.Find(w.image("noble").GetStatus().GetConditions(), computeapi.ImageVerified); v.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("the fixed image: %v", v)
	}
}

// TestADiskSmallerThanItsImage, admitted before any node knew the image's
// size, says so, and is made once grown.
func TestADiskSmallerThanItsImage(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	w.applyDiskOf("root", "2Gi", "noble")
	w.putMachine(diskMachine("vm-a", "root"))
	w.waitDisk("root", "told it is too small", func(d *storagev1alpha1.Disk) bool {
		return diskReady(d).GetReason() == computeapi.ReasonDiskTooSmall
	})
	w.applyDiskOf("root", "3Gi", "noble")
	w.waitDisk("root", "made once grown", func(d *storagev1alpha1.Disk) bool {
		return conditions.IsTrue(d.GetStatus().GetConditions(), computeapi.DiskReady)
	})
}
