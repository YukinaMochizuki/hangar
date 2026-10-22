package apiserver

import (
	"context"
	"math"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
)

func withResources(name string, r *computev1alpha1.Resources) *computev1alpha1.Container {
	c := ctr(name)
	c.Spec.Resources = r
	return c
}

// nodeWithDisk reports a node ready with 4096Mi of memory and the given
// disk: capacity, and allocatable unless it is empty.
func nodeWithDisk(t *testing.T, s *Server, system context.Context, node, capacity, allocatable string) {
	t.Helper()
	status := &computev1alpha1.NodeStatus{
		Capacity: &computev1alpha1.NodeResources{Cpu: "8", Memory: "4096Mi", Disk: capacity},
		Conditions: []*metav1.Condition{{
			Type: NodeReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "AgentReporting",
		}},
	}
	if allocatable != "" {
		status.Allocatable = &computev1alpha1.NodeResources{Cpu: "8", Memory: "4096Mi", Disk: allocatable}
	}
	if _, err := s.backend(registry.Node).UpdateStatus(asNode(system, node), resource.Key{Name: node}, status, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAMachineGetsCanonicalQuantities(t *testing.T) {
	s, system, _ := controlled(t)
	tests := []struct {
		name string
		in   *computev1alpha1.Resources
		want *computev1alpha1.Resources
	}{
		{"exact", &computev1alpha1.Resources{Cpu: "2", Memory: "512Mi", Disk: "20Gi"}, &computev1alpha1.Resources{Cpu: "2", Memory: "512Mi", Disk: "20Gi"}},
		{"decimal", &computev1alpha1.Resources{Cpu: "2000m", Memory: "512M", Disk: "10G"}, &computev1alpha1.Resources{Cpu: "2", Memory: "489Mi", Disk: "10Gi"}},
		{"defaults", &computev1alpha1.Resources{}, &computev1alpha1.Resources{Cpu: "1", Memory: "512Mi", Disk: "10Gi"}},
		{"bytes", &computev1alpha1.Resources{Cpu: "1e0", Memory: "134217729", Disk: "1.5Gi"}, &computev1alpha1.Resources{Cpu: "1", Memory: "129Mi", Disk: "2Gi"}},
	}
	// As the system, which the shape rules do not hold: these test the
	// canonical forms, of quantities no tenant may ask for.
	for _, tt := range tests {
		applyCtr(t, s, system, withResources(tt.name, tt.in))
		m := waitMachine(t, s, system, "ct-"+tt.name, "created", func(*computev1alpha1.Machine) bool { return true })
		if !proto.Equal(m.GetSpec().GetResources(), tt.want) {
			t.Errorf("%s: machine resources %v, want %v", tt.name, m.GetSpec().GetResources(), tt.want)
		}
		c := waitContainer(t, s, system, tt.name, "stored", func(*computev1alpha1.Container) bool { return true })
		if got := c.GetSpec().GetResources(); got.GetCpu() != tt.in.GetCpu() || got.GetMemory() != tt.in.GetMemory() || got.GetDisk() != tt.in.GetDisk() {
			t.Errorf("%s: the container's resources were rewritten to %v", tt.name, c.GetSpec().GetResources())
		}
	}
}

func TestContainerQuantitiesAreChecked(t *testing.T) {
	s, _, user := running(t)
	b := s.backend(registry.Container)
	refused := []struct {
		res *computev1alpha1.Resources
		msg string
	}{
		{&computev1alpha1.Resources{Cpu: "500m"}, "vCPUs are whole"},
		{&computev1alpha1.Resources{Cpu: "1.5"}, "vCPUs are whole"},
		{&computev1alpha1.Resources{Cpu: "0"}, "at least 1"},
		{&computev1alpha1.Resources{Cpu: "two"}, "spec.resources.cpu"},
		{&computev1alpha1.Resources{Memory: "127Mi"}, "spec.resources.memory 127Mi is below the minimum of 128Mi"},
		{&computev1alpha1.Resources{Memory: "134217727"}, "below the minimum of 128Mi"},
		{&computev1alpha1.Resources{Memory: "1.5"}, "whole number of bytes"},
		{&computev1alpha1.Resources{Memory: "-1Gi"}, "spec.resources.memory"},
		{&computev1alpha1.Resources{Memory: "512MB"}, "unknown suffix"},
		{&computev1alpha1.Resources{Disk: "1023Mi"}, "spec.resources.disk 1023Mi is below the minimum of 1Gi"},
		{&computev1alpha1.Resources{Disk: "1G"}, "below the minimum of 1Gi"},
		{&computev1alpha1.Resources{Disk: "1073741823"}, "below the minimum of 1Gi"},
		{&computev1alpha1.Resources{Disk: "20 Gi"}, "spec.resources.disk"},
		{&computev1alpha1.Resources{Disk: "16Ei"}, "too large"},
	}
	for _, tt := range refused {
		_, err := b.Apply(user, withResources("bad", tt.res))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%v: got %v, want InvalidArgument saying %q", tt.res, err, tt.msg)
		}
	}
	// The least a tenant may ask for, under the shape rules
	// (machinesize_admission_internal_test.go has the rest).
	for i, ok := range []*computev1alpha1.Resources{
		{Cpu: "1", Memory: "256Mi", Disk: "2Gi"},
		{Cpu: "1000m", Memory: "268435456", Disk: "2147483648"},
	} {
		if _, err := b.Apply(user, withResources([]string{"least", "least-bytes"}[i], ok)); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
}

func TestADiskCanGrowButNeverShrink(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithDisk(t, s, system, "n1", "1000Gi", "")
	b := s.backend(registry.Container)
	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Disk: "20Gi"}))
	waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "n1" && m.GetSpec().GetResources().GetDisk() == "20Gi"
	})

	for _, smaller := range []string{"10Gi", "", "20G", "19Gi"} {
		_, err := b.Apply(user, withResources("api", &computev1alpha1.Resources{Disk: smaller}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "smaller than the current 20Gi; a root disk can grow but never shrink") {
			t.Errorf("shrinking the disk to %q: %v", smaller, err)
		}
	}
	// Sizes compare as the disk has them, in whole Gi rounded up: one byte
	// short of 20Gi is a 20Gi disk, so no shrink and no change.
	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Disk: "21474836479"}))
	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Disk: "30Gi"}))
	waitMachine(t, s, system, "ct-api", "grown", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetResources().GetDisk() == "30Gi" })

	// Leaving the disk out means the default, which a disk cannot shrink to.
	applyCtr(t, s, user, ctr("web"))
	waitMachine(t, s, system, "ct-web", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	if _, err := b.Apply(user, withResources("web", &computev1alpha1.Resources{Disk: "5Gi"})); connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "5Gi is smaller than the current 10Gi") {
		t.Errorf("shrinking a default disk: %v", err)
	}
	// 10G is a 10Gi disk.
	applyCtr(t, s, user, withResources("web", &computev1alpha1.Resources{Disk: "10G"}))
	applyCtr(t, s, user, withResources("web", &computev1alpha1.Resources{Disk: "10Gi"}))
	if m := waitMachine(t, s, system, "ct-web", "created", func(*computev1alpha1.Machine) bool { return true }); m.GetSpec().GetResources().GetDisk() != "10Gi" {
		t.Errorf("machine disk %s", m.GetSpec().GetResources().GetDisk())
	}
}

// TestBeforeItsMachineAContainerCannotShrinkItsDisk: without a machine of
// its own, the stored container is the floor; another object's machine of
// the same name is not.
func TestBeforeItsMachineAContainerCannotShrinkItsDisk(t *testing.T) {
	s, system, user := running(t)
	b := s.backend(registry.Container)
	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Disk: "20Gi"}))
	squatter := machine("ct-api", "")
	squatter.Spec.Resources = &computev1alpha1.Resources{Cpu: "1", Memory: "512Mi", Disk: "5Gi"}
	if _, err := s.backend(registry.Machine).Apply(system, squatter); err != nil {
		t.Fatal(err)
	}
	_, err := b.Apply(user, withResources("api", &computev1alpha1.Resources{Disk: "10Gi"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "smaller than the current 20Gi") {
		t.Errorf("shrinking with no machine of its own: %v", err)
	}
	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Disk: "21474836479"}))
}

// TestAGrowthThatDidNotFitCanBeWithdrawn: the floor is the disk the machine
// has, not what the container last asked for.
func TestAGrowthThatDidNotFitCanBeWithdrawn(t *testing.T) {
	s, system, user := controlled(t, "n1")
	b := s.backend(registry.Container)
	nodeWithDisk(t, s, system, "n1", "30Gi", "")
	applyCtr(t, s, user, ctr("api"))
	applyCtr(t, s, user, ctr("other"))
	m := waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	waitMachine(t, s, system, "ct-other", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration())
	waitContainer(t, s, system, "api", "ready", func(c *computev1alpha1.Container) bool {
		return conditions.IsTrue(c.GetStatus().GetConditions(), ContainerReady)
	})

	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Disk: "25Gi"}))
	waitContainer(t, s, system, "api", "told the growth does not fit", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == "ResizeDoesNotFit"
	})
	// Below the machine's disk is still a shrink.
	_, err := b.Apply(user, withResources("api", &computev1alpha1.Resources{Disk: "9Gi"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "9Gi is smaller than the current 10Gi") {
		t.Errorf("going below the machine's disk: %v", err)
	}
	// Back to what the machine has, and the container recovers.
	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Disk: "10G"}))
	waitContainer(t, s, system, "api", "ready again", func(c *computev1alpha1.Container) bool {
		return conditions.IsTrue(c.GetStatus().GetConditions(), ContainerReady) && c.GetStatus().GetObservedGeneration() == c.GetMetadata().GetGeneration()
	})
	if got := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"}); got.GetSpec().GetResources().GetDisk() != "10Gi" || got.GetMetadata().GetGeneration() != m.GetMetadata().GetGeneration() {
		t.Errorf("the machine changed: disk %s, generation %d, was %d", got.GetSpec().GetResources().GetDisk(), got.GetMetadata().GetGeneration(), m.GetMetadata().GetGeneration())
	}
}

// TestAMachineDiskNeverShrinks: a container asking for less disk than its
// machine has, as when a withdrawal races the growth landing, leaves the
// machine its disk and still applies the rest.
func TestAMachineDiskNeverShrinks(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithDisk(t, s, system, "n1", "100Gi", "")
	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Disk: "20Gi"}))
	waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	c := waitContainer(t, s, system, "api", "stored", func(*computev1alpha1.Container) bool { return true })

	racing := proto.Clone(c).(*computev1alpha1.Container)
	racing.Spec.Image = "nginx:2"
	racing.Spec.Resources = &computev1alpha1.Resources{Disk: "10Gi"}
	// The scheduler writes the machine too; try again on its writes.
	var got *computev1alpha1.Machine
	var err error
	for range 50 {
		m := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"})
		if got, err = s.ensureMachine(system, racing, m); connect.CodeOf(err) != connect.CodeAborted {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if got.GetSpec().GetResources().GetDisk() != "20Gi" || got.GetSpec().GetContainer().GetImage() != "nginx:2" {
		t.Errorf("machine disk %s, image %s; want 20Gi and nginx:2", got.GetSpec().GetResources().GetDisk(), got.GetSpec().GetContainer().GetImage())
	}
}

func TestTheSchedulerFitsDisk(t *testing.T) {
	s, system, user := controlled(t, "d", "plain")
	// Allocatable, not capacity, is what machines get.
	nodeWithDisk(t, s, system, "d", "1000Gi", "30Gi")

	applyCtr(t, s, user, withResources("a", &computev1alpha1.Resources{Disk: "20Gi"}))
	waitMachine(t, s, system, "ct-a", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "d" })
	// Exactly what is left fits.
	applyCtr(t, s, user, withResources("b", &computev1alpha1.Resources{Disk: "10Gi"}))
	waitMachine(t, s, system, "ct-b", "placed in what is left", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "d" })
	applyCtr(t, s, user, withResources("c", &computev1alpha1.Resources{Disk: "2Gi"}))
	waitMachine(t, s, system, "ct-c", "refused for lack of disk", func(m *computev1alpha1.Machine) bool {
		sc := scheduled(m)
		return m.GetSpec().GetNode() == "" && sc.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE &&
			sc.GetMessage() == "no ready node has 2Gi of disk free"
	})
	waitContainer(t, s, system, "c", "told why", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetMessage() == "no ready node has 2Gi of disk free"
	})

	// A node that reports no disk is not checked for it.
	nodeWithRoom(t, s, system, "plain", 1024)
	waitMachine(t, s, system, "ct-c", "placed on the node without disk", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "plain" })
	applyCtr(t, s, user, withResources("huge", &computev1alpha1.Resources{Disk: "64Ti"}))
	waitMachine(t, s, system, "ct-huge", "placed on the node without disk", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "plain" })
}

func TestUnschedulableMessagesNameTheShortfall(t *testing.T) {
	s, system, _ := controlled(t, "mem", "disk")
	// mem has memory and no disk to spare; disk has disk and no memory.
	nodeWithDisk(t, s, system, "mem", "1Gi", "")
	status := &computev1alpha1.NodeStatus{
		Capacity: &computev1alpha1.NodeResources{Memory: "256Mi", Disk: "1Ti"},
		Conditions: []*metav1.Condition{{
			Type: NodeReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "AgentReporting",
		}},
	}
	if _, err := s.backend(registry.Node).UpdateStatus(asNode(system, "disk"), resource.Key{Name: "disk"}, status, nil); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		res  *computev1alpha1.Resources
		msg  string
	}{
		{"both", &computev1alpha1.Resources{Memory: "512Mi", Disk: "20Gi"}, "no ready node has both 512Mi of memory and 20Gi of disk free"},
		{"memory", &computev1alpha1.Resources{Memory: "8Gi", Disk: "1Gi"}, "no ready node has 8192Mi of memory free"},
		{"disk", &computev1alpha1.Resources{Memory: "128Mi", Disk: "2Ti"}, "no ready node has 2048Gi of disk free"},
		{"either", &computev1alpha1.Resources{Memory: "8Gi", Disk: "2Ti"}, "no ready node has 8192Mi of memory or 2048Gi of disk free"},
	}
	// As the system, which the shape rules do not hold, for shapes as
	// lopsided as the nodes.
	for _, tt := range tests {
		applyCtr(t, s, system, withResources(tt.name, tt.res))
		waitMachine(t, s, system, "ct-"+tt.name, "refused", func(m *computev1alpha1.Machine) bool {
			return m.GetSpec().GetNode() == "" && scheduled(m).GetMessage() == tt.msg
		})
	}
}

func TestANodeWithAnUnreadableQuantityTakesNothing(t *testing.T) {
	s, system, user := controlled(t, "odd", "full")
	nodeWithDisk(t, s, system, "odd", "lots", "")
	applyCtr(t, s, user, ctr("api"))
	const unreadable = `node odd disk: "lots" is not a quantity, such as 2, 500m, 512Mi or 10Gi`
	waitMachine(t, s, system, "ct-api", "told the node cannot be read", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "" && scheduled(m).GetMessage() == "no ready node reports sizes that can be read: "+unreadable
	})
	// Beside a node that can be read, the shortfall is named and the
	// unreadable node too.
	nodeWithDisk(t, s, system, "full", "1Gi", "")
	waitMachine(t, s, system, "ct-api", "told both", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "" &&
			scheduled(m).GetMessage() == "no ready node has 10Gi of disk free; not counted, as its sizes cannot be read: "+unreadable
	})
	nodeWithDisk(t, s, system, "odd", "100Gi", "")
	waitMachine(t, s, system, "ct-api", "placed once the node reads", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "odd" })
}

// TestAResizeOnANodeWithUnreadableSizesIsBlocked: the container says why,
// and the resize goes through once the node reports sizes that read.
func TestAResizeOnANodeWithUnreadableSizesIsBlocked(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithDisk(t, s, system, "n1", "100Gi", "")
	applyCtr(t, s, user, ctr("api"))
	waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	nodeWithDisk(t, s, system, "n1", "lots", "")
	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Memory: "1Gi"}))
	waitContainer(t, s, system, "api", "told the node cannot be read", func(c *computev1alpha1.Container) bool {
		r := containerReady(c)
		return r.GetReason() == "NodeSizeUnreadable" &&
			r.GetMessage() == `node n1 disk: "lots" is not a quantity, such as 2, 500m, 512Mi or 10Gi; the change is not applied`
	})
	nodeWithDisk(t, s, system, "n1", "100Gi", "")
	waitMachine(t, s, system, "ct-api", "resized", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetResources().GetMemory() == "1024Mi" })
}

// TestAnUnplacedMachineHoldsNoDisk: a disk too large for any node can be
// lowered, as no disk exists before the machine is placed.
func TestAnUnplacedMachineHoldsNoDisk(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithDisk(t, s, system, "n1", "50Gi", "")
	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Disk: "100Gi"}))
	waitMachine(t, s, system, "ct-api", "refused", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "" && scheduled(m).GetMessage() == "no ready node has 100Gi of disk free"
	})
	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Disk: "20Gi"}))
	waitMachine(t, s, system, "ct-api", "placed with the smaller disk", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "n1" && m.GetSpec().GetResources().GetDisk() == "20Gi"
	})
}

// TestReapplyingAfterTheRacePasses: when a withdrawal raced the growth and
// the machine kept the larger disk, an apply that does not change the disk
// still passes; one that does is held to the machine's disk.
func TestReapplyingAfterTheRacePasses(t *testing.T) {
	s, system, user := controlled(t, "n1")
	b := s.backend(registry.Container)
	nodeWithDisk(t, s, system, "n1", "100Gi", "")
	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Disk: "20Gi"}))
	waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	// The growth landed after the container withdrew it. The scheduler
	// writes the machine too; try again on its writes.
	var err error
	for range 50 {
		grown := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"})
		grown.Spec.Resources.Disk = "30Gi"
		if _, err = s.backend(registry.Machine).Apply(system, grown); connect.CodeOf(err) != connect.CodeAborted {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "ct-api", "kept at 30Gi", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetResources().GetDisk() == "30Gi" })

	labelled := withResources("api", &computev1alpha1.Resources{Disk: "20Gi"})
	labelled.Metadata.Labels = map[string]string{"tier": "front"}
	applyCtr(t, s, user, labelled)
	// The same disk in whole Gi, written another way, is no change either.
	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Disk: "21474836479"}))
	if _, err := b.Apply(user, withResources("api", &computev1alpha1.Resources{Disk: "25Gi"})); connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "25Gi is smaller than the current 30Gi") {
		t.Errorf("changing the disk below the machine's: %v", err)
	}
	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Disk: "30Gi"}))
}

func TestResourcesHaveMaximums(t *testing.T) {
	s, system, user := running(t)
	b := s.backend(registry.Container)
	for _, tt := range []struct {
		res *computev1alpha1.Resources
		msg string
	}{
		{&computev1alpha1.Resources{Cpu: "8193"}, "spec.resources.cpu 8193 is above the maximum of 8192 vCPUs"},
		{&computev1alpha1.Resources{Memory: "4398046511105"}, "spec.resources.memory 4398046511105 is above the maximum of 4Ti"},
		{&computev1alpha1.Resources{Disk: "70368744177665"}, "spec.resources.disk 70368744177665 is above the maximum of 64Ti"},
		{&computev1alpha1.Resources{Disk: "15Ei"}, "above the maximum of 64Ti"},
	} {
		_, err := b.Apply(user, withResources("big", tt.res))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%v: got %v, want InvalidArgument saying %q", tt.res, err, tt.msg)
		}
	}
	applyCtr(t, s, user, withResources("big", &computev1alpha1.Resources{Cpu: "8192", Memory: "4Ti", Disk: "64Ti"}))

	mb := s.backend(registry.Machine)
	for _, bad := range []*computev1alpha1.Resources{
		{Cpu: "8193", Memory: "512Mi", Disk: "10Gi"},
		{Cpu: "1", Memory: "4097Gi", Disk: "10Gi"},
		{Cpu: "1", Memory: "512Mi", Disk: "65537Gi"},
	} {
		m := machine("big", "")
		m.Spec.Resources = bad
		_, err := mb.Apply(system, m)
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "above the maximum") {
			t.Errorf("a machine with %v: %v", bad, err)
		}
	}
	m := machine("big", "")
	m.Spec.Resources = &computev1alpha1.Resources{Cpu: "8192", Memory: "4096Gi", Disk: "65536Gi"}
	if _, err := mb.Apply(system, m); err != nil {
		t.Errorf("a machine at the maximums: %v", err)
	}
}

func TestAddSaturates(t *testing.T) {
	for _, tt := range []struct{ a, b, want uint64 }{
		{1, 2, 3},
		{math.MaxUint64 - 1, 1, math.MaxUint64},
		{math.MaxUint64, 1, math.MaxUint64},
		{1, math.MaxUint64, math.MaxUint64},
		{math.MaxUint64, math.MaxUint64, math.MaxUint64},
	} {
		if got := add(tt.a, tt.b); got != tt.want {
			t.Errorf("add(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestADiskResizeMustFitItsNode(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithDisk(t, s, system, "n1", "30Gi", "")
	applyCtr(t, s, user, ctr("api"))
	applyCtr(t, s, user, ctr("other"))
	for _, n := range []string{"ct-api", "ct-other"} {
		waitMachine(t, s, system, n, "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	}

	// Exactly what is left fits.
	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Disk: "20Gi"}))
	waitMachine(t, s, system, "ct-api", "grown", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetResources().GetDisk() == "20Gi" })

	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Disk: "21Gi"}))
	waitContainer(t, s, system, "api", "told the resize does not fit", func(c *computev1alpha1.Container) bool {
		r := containerReady(c)
		return r.GetReason() == "ResizeDoesNotFit" && r.GetMessage() == "node n1 has 20Gi of disk free for this machine, not 21Gi; the change is not applied"
	})
	if m := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-api"}); m.GetSpec().GetResources().GetDisk() != "20Gi" {
		t.Errorf("the node's disk was overcommitted: the machine now asks %s", m.GetSpec().GetResources().GetDisk())
	}
	// Disk freed on the node lets the blocked growth through.
	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "other"}, nil); err != nil {
		t.Fatal(err)
	}
	other := waitMachine(t, s, system, "ct-other", "marked", func(m *computev1alpha1.Machine) bool { return m.GetMetadata().GetDeleteTime() != nil })
	reportMachine(t, s, system, other, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, other.GetMetadata().GetGeneration(),
		&metav1.Condition{Type: MachineReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned"})
	waitMachine(t, s, system, "ct-api", "grown once there is room", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetResources().GetDisk() == "21Gi" })

	// Memory that fits is not held back by disk that does not grow, even on
	// a node whose disk is now overcommitted.
	nodeWithDisk(t, s, system, "n1", "5Gi", "")
	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Memory: "1Gi", Disk: "21Gi"}))
	waitMachine(t, s, system, "ct-api", "given more memory", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetResources().GetMemory() == "1024Mi" })
}

func TestADiskResizeOnANodeWithoutDiskIsNotChecked(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 1024)
	applyCtr(t, s, user, ctr("api"))
	waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	applyCtr(t, s, user, withResources("api", &computev1alpha1.Resources{Disk: "64Ti"}))
	waitMachine(t, s, system, "ct-api", "grown", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetResources().GetDisk() == "65536Gi" })
}

func TestMachineQuantitiesAreChecked(t *testing.T) {
	s, system, _ := running(t)
	b := s.backend(registry.Machine)
	for _, bad := range []*computev1alpha1.Resources{
		{Cpu: "500m", Memory: "512Mi", Disk: "10Gi"},
		{Cpu: "1", Memory: "lots", Disk: "10Gi"},
		{Cpu: "1", Memory: "512Mi", Disk: "-10Gi"},
	} {
		m := machine("bad", "")
		m.Spec.Resources = bad
		_, err := b.Apply(system, m)
		wantCodeOf(t, err, connect.CodeInvalidArgument, "a machine with resources "+bad.String())
	}
	m := machine("good", "")
	m.Spec.Resources = &computev1alpha1.Resources{Cpu: "1", Memory: "512Mi", Disk: "10Gi"}
	if _, err := b.Apply(system, m); err != nil {
		t.Errorf("a machine with canonical resources: %v", err)
	}
}
