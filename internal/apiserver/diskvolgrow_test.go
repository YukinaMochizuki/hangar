package apiserver

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

// growingDisk is a disk of size that grows by itself up to max.
func growingDisk(name, size, max string) *storagev1alpha1.Disk {
	d := disk(name, size)
	d.Spec.Grow = &storagev1alpha1.DiskGrow{Max: max}
	return d
}

// mountedOn applies disk d and a container's machine ct-a on node n1
// mounting it read-write at /data, with the disk bound there at its size.
func mountedOn(t *testing.T, s *Server, system, user context.Context, d *storagev1alpha1.Disk) {
	t.Helper()
	mountedAs(t, s, system, user, d, false)
}

// mountedAs is mountedOn, the volume read-only if readOnly.
func mountedAs(t *testing.T, s *Server, system, user context.Context, d *storagev1alpha1.Disk, readOnly bool) {
	t.Helper()
	if _, err := s.backend(registry.Disk).Apply(user, d); err != nil {
		t.Fatal(err)
	}
	name := d.GetMetadata().GetName()
	bindDisk(t, s, system, name, "n1", d.GetSpec().GetSize())
	m := machine("ct-a", "n1")
	m.Spec.Resources = &computev1alpha1.Resources{Memory: "512Mi", Disk: "2Gi"}
	m.GetSpec().GetContainer().Volumes = []*computev1alpha1.ContainerBootVolume{{Disk: name, Target: "/data", ReadOnly: readOnly}}
	if _, err := s.backend(registry.Machine).Apply(system, withDisks(m, name)); err != nil {
		t.Fatal(err)
	}
	waitDisk(t, s, system, name, "mounted", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetAttachment().GetAs() == storagev1alpha1.DiskAttachmentRole_DISK_ATTACHMENT_ROLE_VOLUME
	})
}

// askDiskGrow writes machine ct-a's status as its node does when the
// guest finds the volume of disk past its threshold: asking for size.
func askDiskGrow(t *testing.T, s *Server, system context.Context, disk, size string) {
	t.Helper()
	m := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-a"})
	status := &computev1alpha1.MachineStatus{}
	if m.GetStatus() != nil {
		status = proto.Clone(m.GetStatus()).(*computev1alpha1.MachineStatus)
	}
	status.DisksWanted = []*computev1alpha1.MachineDiskWanted{{Disk: disk, Size: size}}
	if _, err := s.backend(registry.Machine).UpdateStatus(asNode(system, "n1"), resource.Key{Project: "web", Name: "ct-a"}, status, nil); err != nil {
		t.Fatal(err)
	}
}

func diskGrowCond(d *storagev1alpha1.Disk) *metav1.Condition {
	return conditions.Find(d.GetStatus().GetConditions(), DiskGrowBlocked)
}

func diskSized(size string) func(*storagev1alpha1.Disk) bool {
	return func(d *storagev1alpha1.Disk) bool { return d.GetStatus().GetSize() == size }
}

// TestADiskGrowsWithinQuota: its machine's ask grows a disk one step at a
// time into status.size, never its spec; a step past the tenant's limit
// waits, the disk saying why, and goes through once the limit is raised.
func TestADiskGrowsWithinQuota(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Disk: proto.String("12Gi")})
	nodeSized(t, s, system, "n1", 4096, 100)
	mountedOn(t, s, system, user, growingDisk("data", "2Gi", "20Gi"))

	askDiskGrow(t, s, system, "data", "4Gi")
	d := waitDisk(t, s, system, "data", "grown to 4Gi", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetSize() == "4Gi" && diskGrowCond(d).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE
	})
	if d.GetSpec().GetSize() != "2Gi" {
		t.Errorf("the spec was rewritten: %s", d.GetSpec().GetSize())
	}
	// The same ask again grows nothing more.
	askDiskGrow(t, s, system, "data", "4Gi")
	waitSettled(t, s)
	if got := getDisk(t, s, system, "data").GetStatus().GetSize(); got != "4Gi" {
		t.Errorf("an ask repeated grew the disk to %s", got)
	}

	askDiskGrow(t, s, system, "data", "8Gi")
	waitDisk(t, s, system, "data", "grown to 8Gi", diskSized("8Gi"))
	// 16Gi passes the 12Gi limit.
	askDiskGrow(t, s, system, "data", "16Gi")
	waitDisk(t, s, system, "data", "blocked by quota", func(d *storagev1alpha1.Disk) bool {
		c := diskGrowCond(d)
		return c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE && c.GetReason() == ReasonDiskGrowQuotaExceeded &&
			strings.Contains(c.GetMessage(), "the disk stays 8Gi") && d.GetStatus().GetSize() == "8Gi"
	})
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Disk: proto.String("30Gi")})
	waitDisk(t, s, system, "data", "grown once the limit is raised", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetSize() == "16Gi" && diskGrowCond(d).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE
	})
}

// TestADiskGrowthMustFitItsNode: a step its node has no room for waits,
// said on the disk, and goes through once the node has room.
func TestADiskGrowthMustFitItsNode(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	// The machine's 2Gi and the disk's 2Gi of 5Gi: a step to 4Gi takes 2Gi
	// more, and 1Gi is left.
	nodeSized(t, s, system, "n1", 4096, 5)
	mountedOn(t, s, system, user, growingDisk("data", "2Gi", "20Gi"))
	askDiskGrow(t, s, system, "data", "4Gi")
	waitDisk(t, s, system, "data", "blocked by its node", func(d *storagev1alpha1.Disk) bool {
		c := diskGrowCond(d)
		return c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE && c.GetReason() == "ResizeDoesNotFit" && d.GetStatus().GetSize() == "2Gi"
	})
	nodeSized(t, s, system, "n1", 4096, 50)
	waitDisk(t, s, system, "data", "grown once there is room", diskSized("4Gi"))
}

// TestAGrownDiskKeepsItsSize: once grown, the disk's spec applied again,
// or a smaller spec.size, is not refused and shrinks nothing; quota counts
// the grown size; growth turned off keeps the disk and drops the condition.
func TestAGrownDiskKeepsItsSize(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 4096, 100)
	mountedOn(t, s, system, user, growingDisk("data", "2Gi", "20Gi"))
	askDiskGrow(t, s, system, "data", "4Gi")
	waitDisk(t, s, system, "data", "grown", diskSized("4Gi"))
	askDiskGrow(t, s, system, "data", "8Gi")
	waitDisk(t, s, system, "data", "grown", diskSized("8Gi"))

	for _, d := range []*storagev1alpha1.Disk{growingDisk("data", "2Gi", "20Gi"), growingDisk("data", "3Gi", "20Gi")} {
		if _, err := s.backend(registry.Disk).Apply(user, d); err != nil {
			t.Fatalf("applying %s again: %v", d.GetSpec().GetSize(), err)
		}
	}
	// The machine, owned by nothing, counts nothing: the disk alone, grown.
	if got := diskUsage(t, s); got != "8Gi" {
		t.Errorf("the tenant uses %s of disk, want the grown 8Gi", got)
	}
	if _, err := s.backend(registry.Disk).Apply(user, disk("data", "3Gi")); err != nil {
		t.Fatalf("turning growth off: %v", err)
	}
	waitDisk(t, s, system, "data", "no longer reporting growth", func(d *storagev1alpha1.Disk) bool {
		return diskGrowCond(d) == nil && d.GetStatus().GetSize() == "8Gi"
	})
}

// TestOnlyItsContainerOnItsNodeGrowsADisk: an ask grows a disk only from
// the machine it is attached to, on the disk's node, mounting it read-write
// as a volume.
func TestOnlyItsContainerOnItsNodeGrowsADisk(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 4096, 100)
	mountedOn(t, s, system, user, growingDisk("data", "2Gi", "20Gi"))
	askDiskGrow(t, s, system, "data", "4Gi")
	d := getDisk(t, s, system, "data")
	ask := func(status *storagev1alpha1.DiskStatus) bool {
		_, _, asked, err := s.diskAsk(system, d, status)
		if err != nil {
			t.Fatal(err)
		}
		return asked
	}
	if !ask(d.GetStatus()) {
		t.Fatal("its own container's ask is not one")
	}
	for what, change := range map[string]func(*storagev1alpha1.DiskStatus){
		"bound to another node": func(st *storagev1alpha1.DiskStatus) { st.Node = "n2" },
		"attached to another":   func(st *storagev1alpha1.DiskStatus) { st.Machine = "ct-other" },
		"mounted read-only":     func(st *storagev1alpha1.DiskStatus) { st.Attachment.ReadOnly = true },
		"a VM's data disk": func(st *storagev1alpha1.DiskStatus) {
			st.Attachment.As = storagev1alpha1.DiskAttachmentRole_DISK_ATTACHMENT_ROLE_DATA
		},
		"attached to no machine": func(st *storagev1alpha1.DiskStatus) { st.Machine, st.Attachment = "", nil },
	} {
		st := proto.Clone(d.GetStatus()).(*storagev1alpha1.DiskStatus)
		change(st)
		if ask(st) {
			t.Errorf("a disk %s is asked to grow", what)
		}
	}
	// A machine of that name that does not list the disk asks nothing.
	other := machine("ct-other", "n1")
	other.Spec.Resources = &computev1alpha1.Resources{Memory: "512Mi", Disk: "2Gi"}
	if _, err := s.backend(registry.Machine).Apply(system, other); err != nil {
		t.Fatal(err)
	}
	st := proto.Clone(d.GetStatus()).(*storagev1alpha1.DiskStatus)
	st.Machine = "ct-other"
	if ask(st) {
		t.Error("a machine not listing the disk asks for it")
	}
}

// TestAReadOnlyVolumeDoesNotGrow: a disk mounted read-only is not grown,
// whatever is asked.
func TestAReadOnlyVolumeDoesNotGrow(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 4096, 100)
	mountedAs(t, s, system, user, growingDisk("data", "2Gi", "20Gi"), true)
	askDiskGrow(t, s, system, "data", "4Gi")
	waitSettled(t, s)
	if got := getDisk(t, s, system, "data").GetStatus().GetSize(); got != "2Gi" {
		t.Errorf("a read-only volume grew to %s", got)
	}
}

// TestAGrowthByHandAndByItselfFitTogether: a spec.size raised by hand and
// an ask in the same reconcile take the node's room together, and an ask
// the growth by hand met grows nothing more.
func TestAGrowthByHandAndByItselfFitTogether(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	// The machine's 2Gi and the disk's 2Gi fill the node.
	nodeSized(t, s, system, "n1", 4096, 4)
	mountedOn(t, s, system, user, growingDisk("data", "2Gi", "40Gi"))
	askDiskGrow(t, s, system, "data", "4Gi")
	if _, err := s.backend(registry.Disk).Apply(user, growingDisk("data", "4Gi", "40Gi")); err != nil {
		t.Fatal(err)
	}
	waitDisk(t, s, system, "data", "held back", func(d *storagev1alpha1.Disk) bool {
		return conditions.Find(d.GetStatus().GetConditions(), DiskAllocated).GetReason() == "ResizeDoesNotFit"
	})
	// Room for the growth by hand, 4Gi free, which meets the ask.
	nodeSized(t, s, system, "n1", 4096, 8)
	waitDisk(t, s, system, "data", "grown by hand", diskSized("4Gi"))
	waitSettled(t, s)
	if got := getDisk(t, s, system, "data").GetStatus().GetSize(); got != "4Gi" {
		t.Errorf("an 8Gi node holding a 2Gi machine has a disk of %s; the ask for 4Gi was met by hand", got)
	}

	// An ask past it, with a growth by hand beside it: 2Gi free takes the
	// growth by hand to 5Gi, and the step from it, to 10Gi, does not fit.
	askDiskGrow(t, s, system, "data", "8Gi")
	if _, err := s.backend(registry.Disk).Apply(user, growingDisk("data", "5Gi", "40Gi")); err != nil {
		t.Fatal(err)
	}
	d := waitDisk(t, s, system, "data", "grown by hand, the step held back", func(d *storagev1alpha1.Disk) bool {
		c := diskGrowCond(d)
		return d.GetStatus().GetSize() == "5Gi" && c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE && c.GetReason() == "ResizeDoesNotFit"
	})
	waitSettled(t, s)
	if got := getDisk(t, s, system, "data").GetStatus().GetSize(); got != "5Gi" {
		t.Errorf("an 8Gi node holding a 2Gi machine has a disk of %s, after %v", got, d.GetStatus())
	}
}

// TestAGrowthByHandAndAStepCountTheRoomTogether: a growth by hand and a
// step granted in the same reconcile are measured against the node's room
// as one, from the size the disk has on the node as stored.
func TestAGrowthByHandAndAStepCountTheRoomTogether(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	// The machine's 2Gi and the disk's 2Gi fill a 4Gi node: both wait.
	nodeSized(t, s, system, "n1", 4096, 4)
	d := growingDisk("data", "2Gi", "40Gi")
	d.Spec.Grow.Step = "3Gi"
	mountedOn(t, s, system, user, d)
	askDiskGrow(t, s, system, "data", "5Gi")
	raised := proto.Clone(d).(*storagev1alpha1.Disk)
	raised.Spec.Size = "3Gi"
	if _, err := s.backend(registry.Disk).Apply(user, raised); err != nil {
		t.Fatal(err)
	}
	waitDisk(t, s, system, "data", "both held back", func(d *storagev1alpha1.Disk) bool {
		return conditions.Find(d.GetStatus().GetConditions(), DiskAllocated).GetReason() == "ResizeDoesNotFit"
	})
	// 3Gi free: the growth by hand (1Gi) fits, and the step from it, to
	// 6Gi, would take 4Gi from the 2Gi stored: it waits, the disk at 3Gi.
	// A reconcile that reads the node's room before and after it grows may
	// instead find the growth by hand not fitting and the step from 2Gi, to
	// 5Gi, fitting, which meets both. Never more than the 5Gi left.
	nodeSized(t, s, system, "n1", 4096, 7)
	waitDisk(t, s, system, "data", "grown", func(d *storagev1alpha1.Disk) bool { return d.GetStatus().GetSize() != "2Gi" })
	waitSettled(t, s)
	if size := getDisk(t, s, system, "data").GetStatus().GetSize(); size != "3Gi" && size != "5Gi" {
		t.Errorf("a 7Gi node holding a 2Gi machine has a disk of %s", size)
	}
}

// TestOnlyItsMachineOnItsNodeGrowsADisk: an ask from a machine that does
// not list the disk grows nothing, nor does one for a disk without grow.
func TestOnlyItsMachineOnItsNodeGrowsADisk(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 4096, 100)
	mountedOn(t, s, system, user, disk("plain", "2Gi"))
	askDiskGrow(t, s, system, "plain", "4Gi")
	waitSettled(t, s)
	if d := getDisk(t, s, system, "plain"); d.GetStatus().GetSize() != "2Gi" || diskGrowCond(d) != nil {
		t.Errorf("a disk without grow grew: %v", d.GetStatus())
	}
	if _, err := s.backend(registry.Disk).Apply(user, growingDisk("other", "2Gi", "20Gi")); err != nil {
		t.Fatal(err)
	}
	bindDisk(t, s, system, "other", "n1", "2Gi")
	askDiskGrow(t, s, system, "other", "4Gi")
	waitSettled(t, s)
	if got := getDisk(t, s, system, "other").GetStatus().GetSize(); got != "2Gi" {
		t.Errorf("a machine not listing the disk grew it to %s", got)
	}
}

// TestADiskSaysItsVolumeCannotGrow: its machine's VolumeGrowth reaches the
// disk as it is, and goes when it goes.
func TestADiskSaysItsVolumeCannotGrow(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 4096, 100)
	mountedOn(t, s, system, user, growingDisk("data", "2Gi", "20Gi"))
	m := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-a"})
	cannot := &metav1.Condition{Type: computeapi.VolumeGrowth, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE,
		Reason: computeapi.ReasonGuestCannotGrow, Message: "an old guest"}
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration(), cannot)
	waitDisk(t, s, system, "data", "saying its volume cannot grow", func(d *storagev1alpha1.Disk) bool {
		g := conditions.Find(d.GetStatus().GetConditions(), computeapi.VolumeGrowth)
		return g.GetReason() == computeapi.ReasonGuestCannotGrow && g.GetMessage() == "an old guest"
	})
	m = getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-a"})
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration())
	waitDisk(t, s, system, "data", "no longer saying so", func(d *storagev1alpha1.Disk) bool {
		return conditions.Find(d.GetStatus().GetConditions(), computeapi.VolumeGrowth) == nil
	})
}

// TestOnlyAGrownDiskMayAskForLess: adding grow does not let a disk that
// never grew ask below what it has, and grow.max may not be below it.
func TestOnlyAGrownDiskMayAskForLess(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 4096, 100)
	mountedOn(t, s, system, user, disk("data", "8Gi"))
	if _, err := s.backend(registry.Disk).Apply(user, growingDisk("data", "2Gi", "20Gi")); err == nil || !strings.Contains(err.Error(), "never shrink") {
		t.Errorf("a disk that never grew, given grow, asked below its 8Gi: %v", err)
	}
	// A max at least spec.size but below the size on the node.
	if _, err := s.backend(registry.Disk).Apply(user, growingDisk("data", "2Gi", "4Gi")); err == nil || !strings.Contains(err.Error(), "spec.grow.max 4Gi is less than the 8Gi the disk has") {
		t.Errorf("a grow.max below the disk's 8Gi: %v", err)
	}
}

// TestDiskGrowAdmission: admission refuses a grow out of bounds, naming
// the field.
func TestDiskGrowAdmission(t *testing.T) {
	t.Parallel()
	s, _, user := controlled(t)
	for _, g := range []*storagev1alpha1.DiskGrow{{}, {Max: "1Gi"}, {Max: "65Ti"}, {Max: "20Gi", Threshold: "99%"}, {Max: "20Gi", Step: "1x"}} {
		d := disk("data", "2Gi")
		d.Spec.Grow = g
		if _, err := s.backend(registry.Disk).Apply(user, d); err == nil || !strings.Contains(err.Error(), "spec.grow") {
			t.Errorf("%v: %v", g, err)
		}
	}
}
