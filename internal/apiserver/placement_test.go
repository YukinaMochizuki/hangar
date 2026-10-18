package apiserver

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
)

// described is a host as its node describes it.
var described = &computev1alpha1.Placement{
	Region: "region-1", Site: "site-a", CpuModel: "Example CPU 1000", CpuGeneration: "Example Gen 3",
	Storage: "2 × NVMe SSD", Chassis: "generic-1u-2x-u2", Memory: "DDR4-3200 ECC", Network: "2 × 10 GbE",
}

// nodeDescribed reports a node ready with room, an IPv6 prefix and the given
// placement, nil for none.
func nodeDescribed(t *testing.T, s *Server, node string, p *computev1alpha1.Placement) {
	t.Helper()
	status := &computev1alpha1.NodeStatus{
		Capacity:    &computev1alpha1.NodeResources{Cpu: "8", Memory: "8192Mi", Disk: "100Gi"},
		Allocatable: &computev1alpha1.NodeResources{Cpu: "7", Memory: "6144Mi", Disk: "90Gi"},
		Conditions: []*metav1.Condition{{
			Type: NodeReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "AgentReporting",
		}},
		AgentVersion:      "v1.2.3",
		Ipv6Prefix:        testPrefix,
		LastHeartbeatTime: timestamppb.Now(),
		Placement:         p,
	}
	if _, err := s.backend(registry.Node).UpdateStatus(asNode(t.Context(), node), resource.Key{Name: node}, status, nil); err != nil {
		t.Fatal(err)
	}
}

// userContainer reads a container as its project's user does.
func userContainer(t *testing.T, s *Server, user context.Context, name string) *computev1alpha1.Container {
	t.Helper()
	obj, err := s.backend(registry.Container).Get(user, resource.Key{Project: "web", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return obj.(*computev1alpha1.Container)
}

// TestAContainerReportsWhereItRuns: a container's status carries the
// placement of its machine's node from when it is placed, follows the node's
// description, and is all its user reads of the node.
func TestAContainerReportsWhereItRuns(t *testing.T) {
	s, system, user := controlled(t, "n1")
	applyCtr(t, s, user, ctr("a"))
	c := waitContainer(t, s, system, "a", "reported unplaced", func(c *computev1alpha1.Container) bool {
		return c.GetStatus().GetObservedGeneration() > 0
	})
	if p := c.GetStatus().GetPlacement(); p != nil {
		t.Errorf("a container on no node reports placement %v", p)
	}

	nodeDescribed(t, s, "n1", described)
	waitMachine(t, s, system, "ct-a", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	waitContainer(t, s, system, "a", "reporting its node's placement", func(c *computev1alpha1.Container) bool {
		return proto.Equal(c.GetStatus().GetPlacement(), described)
	})
	if got := userContainer(t, s, user, "a").GetStatus().GetPlacement(); !proto.Equal(got, described) {
		t.Errorf("its user reads placement %v, want %v", got, described)
	}

	// The node describing itself anew is what the container says.
	moved := proto.Clone(described).(*computev1alpha1.Placement)
	moved.Site, moved.Chassis = "site-b", ""
	nodeDescribed(t, s, "n1", moved)
	waitContainer(t, s, system, "a", "following its node's description", func(c *computev1alpha1.Container) bool {
		return proto.Equal(c.GetStatus().GetPlacement(), moved)
	})
	nodeDescribed(t, s, "n1", nil)
	waitContainer(t, s, system, "a", "without placement once its node has none", func(c *computev1alpha1.Container) bool {
		return c.GetStatus().GetPlacement() == nil
	})
}

// TestAVirtualMachineReportsWhereItRuns: a VM's status carries its node's
// placement as a container's does.
func TestAVirtualMachineReportsWhereItRuns(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeDescribed(t, s, "n1", described)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vmFromImage("web", "noble")); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "vm-web", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	waitVM(t, s, system, "web", "reporting its node's placement", func(vm *computev1alpha1.VirtualMachine) bool {
		return proto.Equal(vm.GetStatus().GetPlacement(), described)
	})
	obj, err := s.backend(registry.VirtualMachine).Get(user, resource.Key{Project: "web", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if got := obj.(*computev1alpha1.VirtualMachine).GetStatus().GetPlacement(); !proto.Equal(got, described) {
		t.Errorf("its user reads placement %v, want %v", got, described)
	}

	moved := proto.Clone(described).(*computev1alpha1.Placement)
	moved.CpuModel = "Example CPU 2000"
	nodeDescribed(t, s, "n1", moved)
	waitVM(t, s, system, "web", "following its node's description", func(vm *computev1alpha1.VirtualMachine) bool {
		return proto.Equal(vm.GetStatus().GetPlacement(), moved)
	})
}

// TestPlacementOf: only a machine its owner owns, on the node given, gives
// the owner a placement, and a copy of it.
func TestPlacementOf(t *testing.T) {
	node := &computev1alpha1.Node{
		Metadata: &metav1.ObjectMeta{Name: "n1"},
		Status:   &computev1alpha1.NodeStatus{Ipv6Prefix: testPrefix, Placement: described},
	}
	m := func(owner, node string) *computev1alpha1.Machine {
		return &computev1alpha1.Machine{
			Metadata: &metav1.ObjectMeta{OwnerReferences: []*metav1.OwnerReference{{Uid: owner, Controller: true}}},
			Spec:     &computev1alpha1.MachineSpec{Node: node},
		}
	}
	got := placementOf("u1", m("u1", "n1"), node)
	if !proto.Equal(got, described) {
		t.Errorf("placement %v, want %v", got, described)
	}
	if got == described {
		t.Error("the placement is the node's own message, not a copy")
	}
	for _, tt := range []struct {
		what string
		m    *computev1alpha1.Machine
		node *computev1alpha1.Node
	}{
		{"no machine", nil, node},
		{"another owner's machine", m("u2", "n1"), node},
		{"an unplaced machine", m("u1", ""), node},
		{"a machine on another node", m("u1", "n2"), node},
		{"a machine whose node is gone", m("u1", "n1"), nil},
	} {
		if p := placementOf("u1", tt.m, tt.node); p != nil {
			t.Errorf("%s: placement %v", tt.what, p)
		}
	}
}

// TestAStuckReportKeepsWhereItRuns: a container or VM whose change cannot be
// applied still runs on its node, and still says so.
func TestAStuckReportKeepsWhereItRuns(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeDescribed(t, s, "n1", described)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	applyCtr(t, s, user, ctr("a"))
	vm := vmFromImage("web", "noble")
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Fatal(err)
	}
	waitContainer(t, s, system, "a", "reporting its node's placement", func(c *computev1alpha1.Container) bool {
		return proto.Equal(c.GetStatus().GetPlacement(), described)
	})
	waitVM(t, s, system, "web", "reporting its node's placement", func(vm *computev1alpha1.VirtualMachine) bool {
		return proto.Equal(vm.GetStatus().GetPlacement(), described)
	})

	// More memory than the node has: the change waits, the machine stays.
	applyCtr(t, s, user, sized("a", 64*1024))
	big := proto.Clone(vm).(*computev1alpha1.VirtualMachine)
	big.Spec.Resources = &computev1alpha1.VirtualMachineResources{Memory: "64Gi"}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, big); err != nil {
		t.Fatal(err)
	}
	c := waitContainer(t, s, system, "a", "stuck on its resize", func(c *computev1alpha1.Container) bool {
		return conditions.Find(c.GetStatus().GetConditions(), ContainerReady).GetReason() == "ResizeDoesNotFit"
	})
	if got := c.GetStatus().GetPlacement(); !proto.Equal(got, described) {
		t.Errorf("a stuck container reports placement %v, want %v", got, described)
	}
	got := waitVM(t, s, system, "web", "stuck on its resize", func(vm *computev1alpha1.VirtualMachine) bool {
		return vmReady(vm).GetReason() == "ResizeDoesNotFit"
	})
	if p := got.GetStatus().GetPlacement(); !proto.Equal(p, described) {
		t.Errorf("a stuck VM reports placement %v, want %v", p, described)
	}
}

// TestAHeartbeatWritesNoMachineOwner: a node reporting again with the same
// placement leaves its containers and VMs as they are.
func TestAHeartbeatWritesNoMachineOwner(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeDescribed(t, s, "n1", described)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	applyCtr(t, s, user, ctr("a"))
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vmFromImage("web", "noble")); err != nil {
		t.Fatal(err)
	}
	waitContainer(t, s, system, "a", "reporting its node's placement", func(c *computev1alpha1.Container) bool {
		return proto.Equal(c.GetStatus().GetPlacement(), described)
	})
	waitVM(t, s, system, "web", "reporting its node's placement", func(vm *computev1alpha1.VirtualMachine) bool {
		return proto.Equal(vm.GetStatus().GetPlacement(), described)
	})
	waitSettled(t, s)
	ctrKey, vmKey := resource.Key{Project: "web", Name: "a"}, resource.Key{Project: "web", Name: "web"}
	ctrRV, err := versionOf(s, system, registry.Container, ctrKey)()
	if err != nil {
		t.Fatal(err)
	}
	vmRV, err := versionOf(s, system, registry.VirtualMachine, vmKey)()
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		nodeDescribed(t, s, "n1", proto.Clone(described).(*computev1alpha1.Placement))
		waitSettled(t, s)
	}
	if now, _ := versionOf(s, system, registry.Container, ctrKey)(); now != ctrRV {
		t.Errorf("heartbeats wrote the container: at %s, then at %s", ctrRV, now)
	}
	if now, _ := versionOf(s, system, registry.VirtualMachine, vmKey)(); now != vmRV {
		t.Errorf("heartbeats wrote the VM: at %s, then at %s", vmRV, now)
	}
}

// TestANodesPlacementIsBounded: what a node may say of its host is short,
// printable text, and a chassis is a label, since every tenant on the node
// reads it.
func TestANodesPlacementIsBounded(t *testing.T) {
	s, _, _ := controlled(t, "n1")
	ok := func(p *computev1alpha1.Placement) error {
		_, err := s.backend(registry.Node).UpdateStatus(asNode(t.Context(), "n1"), resource.Key{Name: "n1"},
			&computev1alpha1.NodeStatus{LastHeartbeatTime: timestamppb.New(time.Now()), Placement: p}, nil)
		return err
	}
	if err := ok(described); err != nil {
		t.Fatalf("a valid placement: %v", err)
	}
	for _, p := range []*computev1alpha1.Placement{
		{Region: strings.Repeat("r", 65)},
		{Site: "site\na"},
		{CpuModel: "EPYC\x1b[31m"},
		{Storage: fmt.Sprint("2 ", "\u0000", " NVMe")},
		{Chassis: "Generic 1U"},
		{Memory: "DDR4-3200 ECC " + strings.Repeat("x", 51)},
		{Network: "2 × 10 GbE\r"},
	} {
		if err := ok(p); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("placement %v: %v, want InvalidArgument", p, err)
		}
	}
}
