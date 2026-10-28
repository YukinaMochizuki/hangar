package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// startingSmall is a container whose 2Gi root disk grows by itself up to
// max.
func startingSmall(name, max string) *computev1alpha1.Container {
	c := container(name, "nginx")
	c.Spec.Resources = &computev1alpha1.Resources{Disk: "2Gi", DiskGrow: &computev1alpha1.DiskGrow{Max: max}}
	return c
}

// rootStarted waits for the container's root disk to have been made of
// size bytes and the container to run, its status saying disk, and returns
// it.
func (w *world) rootStarted(name string, size int64, disk string) *computev1alpha1.Container {
	w.t.Helper()
	c := w.waitReason(name, "Running")
	uid := w.machine(name).GetMetadata().GetUid()
	testutil.Eventually(w.t, fmt.Sprintf("the root disk made at %d bytes, the status saying %q", size, disk), func() (bool, string) {
		got, _ := get[*computev1alpha1.Container](w, registry.Container, name)
		c = got
		r := w.host.Snapshot().Roots[uid]
		return r.Size == size && c.GetStatus().GetDisk() == disk && w.machine(name).GetStatus().GetDiskNeeded() == "",
			fmt.Sprintf("root %d bytes, status %q, the machine still needing %q", r.Size, c.GetStatus().GetDisk(), w.machine(name).GetStatus().GetDiskNeeded())
	})
	return c
}

// TestARootDiskStartsAsLargeAsItsImage: a container whose disk grows, asking
// for less than its image's base, starts at the base's size in whole Gi, as
// its status says and its project's quota counts, and runs; its spec is
// not rewritten.
func TestARootDiskStartsAsLargeAsItsImage(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.host.SetBaseSize(4 << 30)
	w.apply(startingSmall("api", "16Gi"))
	c := w.rootStarted("api", 4<<30, "4Gi")
	if d := c.GetSpec().GetResources().GetDisk(); d != "2Gi" {
		t.Errorf("the spec was rewritten: %s", d)
	}
	testutil.Eventually(t, "the project counting 4Gi of disk", func() (bool, string) {
		u := w.projectUsage().GetDisk()
		return u == "4Gi", u
	})
}

// TestARootDiskLargerThanItsImageStartsAsAsked: an image smaller than the
// disk asked for starts the disk at the size asked.
func TestARootDiskLargerThanItsImageStartsAsAsked(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.host.SetBaseSize(1 << 30)
	w.apply(startingSmall("api", "16Gi"))
	w.rootStarted("api", 2<<30, "")
}

// TestAnImageLargerThanTheDiskCanGrowSaysSo: an image whose base is past
// the disk's disk_grow.max is too small a disk, as without growth.
func TestAnImageLargerThanTheDiskCanGrowSaysSo(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.host.SetBaseSize(20 << 30)
	w.apply(startingSmall("api", "16Gi"))
	c := w.waitReason("api", computeapi.ReasonDiskTooSmall)
	if msg := ready(c).GetMessage(); !strings.Contains(msg, "disk smaller than its image") {
		t.Errorf("message %q", msg)
	}
	if roots := w.host.Snapshot().Roots; len(roots) != 0 {
		t.Errorf("roots made: %v", roots)
	}
}

// TestARootDiskStartsOnlyWithinQuota: the size an image needs, past the
// project's disk limit, makes no root disk: the container waits, saying
// why, and starts once the limit is raised.
func TestARootDiskStartsOnlyWithinQuota(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.setProjectDisk("3Gi")
	w.host.SetBaseSize(4 << 30)
	w.apply(startingSmall("api", "16Gi"))
	c := w.waitReason("api", computeapi.ReasonDiskGrowing)
	testutil.Eventually(t, "the container saying quota holds its disk back", func() (bool, string) {
		c, _ = get[*computev1alpha1.Container](w, registry.Container, "api")
		b := conditions.Find(c.GetStatus().GetConditions(), "DiskGrowBlocked")
		return b.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE && b.GetReason() == "QuotaExceeded", fmt.Sprint(b)
	})
	if roots := w.host.Snapshot().Roots; len(roots) != 0 {
		t.Errorf("roots made: %v", roots)
	}
	w.setProjectDisk("10Gi")
	w.rootStarted("api", 4<<30, "4Gi")
}

// TestANewImageDropsTheOldRootsAsk: a growth its old root disk asked for,
// held back by quota, is not granted to the root disk a larger image
// rebuilds, which starts at what the image needs and asks nothing until it
// fills, even once quota lets both through.
func TestANewImageDropsTheOldRootsAsk(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.setProjectDisk("2Gi")
	w.host.SetBaseSize(1 << 30)
	c := startingSmall("api", "16Gi")
	c.Spec.Resources.DiskGrow.Step = "3Gi"
	w.apply(c)
	w.rootStarted("api", 2<<30, "")
	uid := w.machine("api").GetMetadata().GetUid()
	w.host.FillRoot(uid, 2<<30-1<<20)
	testutil.Eventually(t, "the old root's growth to 5Gi held back by quota", func() (bool, string) {
		c, _ := get[*computev1alpha1.Container](w, registry.Container, "api")
		b := conditions.Find(c.GetStatus().GetConditions(), "DiskGrowBlocked")
		return w.machine("api").GetStatus().GetDiskWanted() == "5Gi" && b.GetReason() == "QuotaExceeded",
			fmt.Sprintf("asks %q, %v", w.machine("api").GetStatus().GetDiskWanted(), b)
	})

	// The new image needs 3Gi, which quota holds back too, until it is
	// raised past every step.
	w.host.SetBaseSize(3 << 30)
	w.host.AddImage("larger", "sha256:larger-1", nginx)
	c.Spec.Image = "larger"
	w.apply(c)
	w.waitReason("api", computeapi.ReasonDiskGrowing)
	w.setProjectDisk("20Gi")
	w.rootStarted("api", 3<<30, "3Gi")
	testutil.Unchanged(t, "the new root, asking nothing", 300*time.Millisecond, func() (string, error) {
		c, _ := get[*computev1alpha1.Container](w, registry.Container, "api")
		return fmt.Sprintf("machine disk %s, status %q, asks %q", w.machine("api").GetSpec().GetResources().GetDisk(),
			c.GetStatus().GetDisk(), w.machine("api").GetStatus().GetDiskWanted()), nil
	})
	if d := w.machine("api").GetSpec().GetResources().GetDisk(); d != "3Gi" {
		t.Errorf("the new root disk grew to %s, asking nothing", d)
	}
}

// setProjectDisk caps the project's disk.
func (w *world) setProjectDisk(disk string) {
	w.t.Helper()
	obj, err := w.root.Get(context.Background(), registry.Project, client.Key{Name: project})
	if err != nil {
		w.t.Fatal(err)
	}
	p := obj.(*tenancyv1alpha1.Project)
	p.Spec.Limits = &tenancyv1alpha1.Limits{Disk: proto.String(disk)}
	p.Metadata.ResourceVersion = 0
	if _, err := w.root.Apply(context.Background(), registry.Project, p); err != nil {
		w.t.Fatal(err)
	}
}
