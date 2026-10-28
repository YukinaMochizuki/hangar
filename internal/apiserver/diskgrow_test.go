package apiserver

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

// growing is a container whose 2Gi root disk grows by itself up to max.
func growing(name, max string) *computev1alpha1.Container {
	return withResources(name, &computev1alpha1.Resources{Disk: "2Gi", DiskGrow: &computev1alpha1.DiskGrow{Max: max}})
}

// askToGrow writes the machine's status as its node does when its guest is
// past the threshold: asking for wanted.
func askToGrow(t *testing.T, s *Server, system context.Context, m *computev1alpha1.Machine, wanted string) {
	t.Helper()
	status := &computev1alpha1.MachineStatus{}
	if m.GetStatus() != nil {
		status = proto.Clone(m.GetStatus()).(*computev1alpha1.MachineStatus)
	}
	status.DiskWanted = wanted
	key := resource.Key{Project: "web", Name: m.GetMetadata().GetName()}
	if _, err := s.backend(registry.Machine).UpdateStatus(asNode(system, m.GetSpec().GetNode()), key, status, nil); err != nil {
		t.Fatal(err)
	}
}

// askToStart writes the machine's status as its node does when its image's
// base needs more than the machine's disk: asking for needed.
func askToStart(t *testing.T, s *Server, system context.Context, m *computev1alpha1.Machine, needed string) {
	t.Helper()
	status := &computev1alpha1.MachineStatus{}
	if m.GetStatus() != nil {
		status = proto.Clone(m.GetStatus()).(*computev1alpha1.MachineStatus)
	}
	status.DiskNeeded = needed
	key := resource.Key{Project: "web", Name: m.GetMetadata().GetName()}
	if _, err := s.backend(registry.Machine).UpdateStatus(asNode(system, m.GetSpec().GetNode()), key, status, nil); err != nil {
		t.Fatal(err)
	}
}

func diskGrowBlocked(c *computev1alpha1.Container) *metav1.Condition {
	return conditions.Find(c.GetStatus().GetConditions(), ContainerDiskGrowBlocked)
}

func machineDisk(name string) func(*computev1alpha1.Machine) bool {
	return func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetResources().GetDisk() == name }
}

// TestARootDiskGrowsWithinQuota: a node's ask grows the disk one step at a
// time, recorded in the container's status and followed by its machine; a
// step past the tenant's disk limit waits, the container saying why, and
// goes through once the limit is raised.
func TestARootDiskGrowsWithinQuota(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Disk: proto.String("12Gi")})
	nodeWithDisk(t, s, system, "n1", "100Gi", "")
	applyCtr(t, s, user, growing("api", "20Gi"))
	applyCtr(t, s, user, withResources("other", &computev1alpha1.Resources{Disk: "2Gi"}))
	m := waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	if g := m.GetSpec().GetResources().GetDiskGrow().GetMax(); g != "20Gi" {
		t.Errorf("the machine's disk_grow.max is %q", g)
	}

	askToGrow(t, s, system, m, "4Gi")
	m = waitMachine(t, s, system, "ct-api", "grown to 4Gi", machineDisk("4Gi"))
	c := waitContainer(t, s, system, "api", "recording 4Gi", func(c *computev1alpha1.Container) bool {
		return c.GetStatus().GetDisk() == "4Gi" && diskGrowBlocked(c).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE
	})
	if c.GetSpec().GetResources().GetDisk() != "2Gi" {
		t.Errorf("the spec was rewritten: %s", c.GetSpec().GetResources().GetDisk())
	}
	if got := diskUsage(t, s); got != "6Gi" {
		t.Errorf("the tenant uses %s of disk, want 6Gi", got)
	}

	// The same ask again grows nothing more.
	askToGrow(t, s, system, m, "4Gi")
	waitSettled(t, s)
	if m := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"}); m.GetSpec().GetResources().GetDisk() != "4Gi" {
		t.Errorf("an ask repeated grew the disk to %s", m.GetSpec().GetResources().GetDisk())
	}

	askToGrow(t, s, system, m, "8Gi")
	m = waitMachine(t, s, system, "ct-api", "grown to 8Gi", machineDisk("8Gi"))

	// 16Gi and the other's 2Gi pass the 12Gi limit.
	askToGrow(t, s, system, m, "16Gi")
	waitContainer(t, s, system, "api", "blocked by quota", func(c *computev1alpha1.Container) bool {
		b := diskGrowBlocked(c)
		return b.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE && b.GetReason() == ReasonDiskGrowQuotaExceeded &&
			strings.Contains(b.GetMessage(), "the root disk stays 8Gi")
	})
	if m := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"}); m.GetSpec().GetResources().GetDisk() != "8Gi" {
		t.Errorf("a growth past quota reached the machine: %s", m.GetSpec().GetResources().GetDisk())
	}
	if r := containerReady(waitContainer(t, s, system, "api", "read", func(*computev1alpha1.Container) bool { return true })); r.GetReason() == ReasonDiskGrowQuotaExceeded {
		t.Error("a blocked growth made the container not ready")
	}

	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Disk: proto.String("20Gi")})
	waitMachine(t, s, system, "ct-api", "grown to 16Gi once the limit is raised", machineDisk("16Gi"))
	waitContainer(t, s, system, "api", "no longer blocked", func(c *computev1alpha1.Container) bool {
		return c.GetStatus().GetDisk() == "16Gi" && diskGrowBlocked(c).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE
	})
}

// TestARootDiskStartsAsLargeAsItsImageNeeds: the node asking for the size
// its image needs before it makes the root disk is granted that size, not
// the next step, in the container's status and its machine, and quota
// counts it; asked again, nothing more is granted.
func TestARootDiskStartsAsLargeAsItsImageNeeds(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithDisk(t, s, system, "n1", "100Gi", "")
	applyCtr(t, s, user, growing("api", "20Gi"))
	m := waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })

	// The step from 2Gi is 4Gi; the image needs 3Gi.
	askToStart(t, s, system, m, "3Gi")
	m = waitMachine(t, s, system, "ct-api", "started at 3Gi", machineDisk("3Gi"))
	c := waitContainer(t, s, system, "api", "recording 3Gi", func(c *computev1alpha1.Container) bool {
		return c.GetStatus().GetDisk() == "3Gi" && diskGrowBlocked(c).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE
	})
	if c.GetSpec().GetResources().GetDisk() != "2Gi" {
		t.Errorf("the spec was rewritten: %s", c.GetSpec().GetResources().GetDisk())
	}
	if got := diskUsage(t, s); got != "3Gi" {
		t.Errorf("the tenant uses %s of disk, want 3Gi", got)
	}

	askToStart(t, s, system, m, "3Gi")
	waitSettled(t, s)
	if m := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"}); m.GetSpec().GetResources().GetDisk() != "3Gi" {
		t.Errorf("an ask repeated grew the disk to %s", m.GetSpec().GetResources().GetDisk())
	}
}

// TestAStartTheImageNeedsWaitsOnQuota: the size an image needs, past the
// tenant's disk limit, is not granted, the container saying why, and is
// once the limit is raised.
func TestAStartTheImageNeedsWaitsOnQuota(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Disk: proto.String("4Gi")})
	nodeWithDisk(t, s, system, "n1", "100Gi", "")
	applyCtr(t, s, user, growing("api", "20Gi"))
	m := waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })

	askToStart(t, s, system, m, "5Gi")
	waitContainer(t, s, system, "api", "blocked by quota", func(c *computev1alpha1.Container) bool {
		b := diskGrowBlocked(c)
		return b.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE && b.GetReason() == ReasonDiskGrowQuotaExceeded
	})
	if m := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"}); m.GetSpec().GetResources().GetDisk() != "2Gi" {
		t.Errorf("a start past quota reached the machine: %s", m.GetSpec().GetResources().GetDisk())
	}

	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Disk: proto.String("10Gi")})
	waitMachine(t, s, system, "ct-api", "started at 5Gi once the limit is raised", machineDisk("5Gi"))
}

// TestAStartPastMaxIsNotGranted: a size the image needs past
// disk_grow.max, which the node does not ask for, is not granted if asked.
func TestAStartPastMaxIsNotGranted(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithDisk(t, s, system, "n1", "100Gi", "")
	applyCtr(t, s, user, growing("api", "4Gi"))
	m := waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })

	askToStart(t, s, system, m, "5Gi")
	waitContainer(t, s, system, "api", "not blocked", func(c *computev1alpha1.Container) bool {
		return diskGrowBlocked(c).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE
	})
	waitSettled(t, s)
	if m := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"}); m.GetSpec().GetResources().GetDisk() != "2Gi" {
		t.Errorf("a start past max reached the machine: %s", m.GetSpec().GetResources().GetDisk())
	}
}

// setProjectDisk caps project web's disk, as its tenant's owner does.
func setProjectDisk(t *testing.T, s *Server, user context.Context, disk string) {
	t.Helper()
	if _, err := s.backend(registry.Project).Apply(user, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "web"},
		Spec:     &tenancyv1alpha1.ProjectSpec{Tenant: "acme", Limits: &tenancyv1alpha1.Limits{Disk: proto.String(disk)}},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAGrowthWaitsOnItsProjectsLimit: a project's own disk limit holds a
// growth back too, and raising it lets the growth through.
func TestAGrowthWaitsOnItsProjectsLimit(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithDisk(t, s, system, "n1", "100Gi", "")
	setProjectDisk(t, s, user, "3Gi")
	applyCtr(t, s, user, growing("api", "20Gi"))
	m := waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })

	askToGrow(t, s, system, m, "4Gi")
	waitContainer(t, s, system, "api", "blocked by its project", func(c *computev1alpha1.Container) bool {
		b := diskGrowBlocked(c)
		return b.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE && b.GetReason() == ReasonDiskGrowQuotaExceeded &&
			strings.Contains(b.GetMessage(), `project "web"`)
	})
	setProjectDisk(t, s, user, "10Gi")
	waitMachine(t, s, system, "ct-api", "grown once its project's limit is raised", machineDisk("4Gi"))
}

// TestTwoGrowthsShareOneLimit: two containers asking at once against room
// for one step grow one of them, and the other waits.
func TestTwoGrowthsShareOneLimit(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Disk: proto.String("6Gi")})
	nodeWithDisk(t, s, system, "n1", "100Gi", "")
	applyCtr(t, s, user, growing("a", "20Gi"))
	applyCtr(t, s, user, growing("b", "20Gi"))
	ma := waitMachine(t, s, system, "ct-a", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	mb := waitMachine(t, s, system, "ct-b", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })

	askToGrow(t, s, system, ma, "4Gi")
	askToGrow(t, s, system, mb, "4Gi")
	for _, n := range []string{"a", "b"} {
		waitContainer(t, s, system, n, "decided", func(c *computev1alpha1.Container) bool {
			return c.GetStatus().GetDisk() == "4Gi" || diskGrowBlocked(c).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE
		})
	}
	waitSettled(t, s)
	if got := diskUsage(t, s); got != "6Gi" {
		t.Errorf("the tenant uses %s of disk, want the 6Gi limit and no more", got)
	}
}

// TestAContainerSaysItsRootCannotGrow: the node's RootGrowth condition on
// the machine reaches the container as it is, and goes when it goes.
func TestAContainerSaysItsRootCannotGrow(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithDisk(t, s, system, "n1", "100Gi", "")
	applyCtr(t, s, user, growing("api", "20Gi"))
	m := waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	cannot := &metav1.Condition{Type: computeapi.RootGrowth, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE,
		Reason: computeapi.ReasonGuestCannotGrow, Message: "an old guest"}
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration(), cannot)
	waitContainer(t, s, system, "api", "saying its root cannot grow", func(c *computev1alpha1.Container) bool {
		g := conditions.Find(c.GetStatus().GetConditions(), computeapi.RootGrowth)
		return g.GetReason() == computeapi.ReasonGuestCannotGrow && g.GetMessage() == "an old guest" && containerReady(c).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE
	})
	m = getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"})
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration())
	waitContainer(t, s, system, "api", "no longer saying so", func(c *computev1alpha1.Container) bool {
		return conditions.Find(c.GetStatus().GetConditions(), computeapi.RootGrowth) == nil
	})
}

// TestARootDiskGrowthMustFitItsNode: a step the node has no room for
// waits, as a resize asked for in the spec does.
func TestARootDiskGrowthMustFitItsNode(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	// The 2Gi disk and 1Gi free: a step to 4Gi takes 2Gi more.
	nodeWithDisk(t, s, system, "n1", "3Gi", "")
	applyCtr(t, s, user, growing("api", "20Gi"))
	m := waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })

	askToGrow(t, s, system, m, "4Gi")
	waitContainer(t, s, system, "api", "blocked by the node", func(c *computev1alpha1.Container) bool {
		b := diskGrowBlocked(c)
		return b.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE && b.GetReason() == "ResizeDoesNotFit"
	})
	if c := waitContainer(t, s, system, "api", "read", func(*computev1alpha1.Container) bool { return true }); c.GetStatus().GetDisk() != "" {
		t.Errorf("a growth that does not fit was recorded: %s", c.GetStatus().GetDisk())
	}

	nodeWithDisk(t, s, system, "n1", "50Gi", "")
	waitMachine(t, s, system, "ct-api", "grown once there is room", machineDisk("4Gi"))
}

// TestAGrownDiskIsKeptWhateverTheSpecSays: once grown, a smaller
// spec.disk is no shrink, the manifest applied again passes, and quota
// counts the grown size; growth turned off keeps the disk.
func TestAGrownDiskIsKeptWhateverTheSpecSays(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeWithDisk(t, s, system, "n1", "100Gi", "")
	applyCtr(t, s, user, growing("api", "20Gi"))
	m := waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	askToGrow(t, s, system, m, "4Gi")
	m = waitMachine(t, s, system, "ct-api", "grown", machineDisk("4Gi"))
	askToGrow(t, s, system, m, "8Gi")
	waitMachine(t, s, system, "ct-api", "grown", machineDisk("8Gi"))

	applyCtr(t, s, user, growing("api", "20Gi"))
	c := withResources("api", &computev1alpha1.Resources{Disk: "3Gi", DiskGrow: &computev1alpha1.DiskGrow{Max: "20Gi"}})
	applyCtr(t, s, user, c)
	if got := diskUsage(t, s); got != "8Gi" {
		t.Errorf("the tenant uses %s of disk, want the grown 8Gi", got)
	}

	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Disk: "3Gi"}))
	waitContainer(t, s, system, "api", "no longer reporting growth", func(c *computev1alpha1.Container) bool {
		return diskGrowBlocked(c) == nil && c.GetStatus().GetDisk() == "8Gi"
	})
	waitSettled(t, s)
	if m := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"}); m.GetSpec().GetResources().GetDisk() != "8Gi" || m.GetSpec().GetResources().GetDiskGrow() != nil {
		t.Errorf("growth turned off left the machine %v", m.GetSpec().GetResources())
	}
}

// TestDiskGrowIsValidated: admission refuses a disk_grow out of bounds,
// naming the field.
func TestDiskGrowIsValidated(t *testing.T) {
	t.Parallel()
	s, _, user := controlled(t)
	for _, g := range []*computev1alpha1.DiskGrow{
		{},
		{Max: "1Gi"},
		{Max: "65Ti"},
		{Max: "20Gi", Threshold: "99%"},
		{Max: "20Gi", Step: "1x"},
	} {
		_, err := s.backend(registry.Container).Apply(user, withResources("api", &computev1alpha1.Resources{Disk: "2Gi", DiskGrow: g}))
		if err == nil || !strings.Contains(err.Error(), "spec.resources.disk_grow") {
			t.Errorf("%v: %v", g, err)
		}
	}
}
