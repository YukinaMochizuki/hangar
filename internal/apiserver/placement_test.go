package apiserver

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
)

// milan is a host as its node describes it.
var milan = &computev1alpha1.Placement{
	Region: "ap-southeast", Site: "SGP02", CpuModel: "AMD EPYC 7313", CpuGeneration: "Zen 3 (Milan)",
	Storage: "2 × NVMe SSD", Chassis: "generic-1u-2x-u2",
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
		AgentVersion: "v1.2.3",
		Ipv6Prefix:   testPrefix,
		Placement:    p,
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

	nodeDescribed(t, s, "n1", milan)
	waitMachine(t, s, system, "ct-a", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	waitContainer(t, s, system, "a", "reporting its node's placement", func(c *computev1alpha1.Container) bool {
		return proto.Equal(c.GetStatus().GetPlacement(), milan)
	})
	if got := userContainer(t, s, user, "a").GetStatus().GetPlacement(); !proto.Equal(got, milan) {
		t.Errorf("its user reads placement %v, want %v", got, milan)
	}

	// The node describing itself anew is what the container says.
	moved := proto.Clone(milan).(*computev1alpha1.Placement)
	moved.Site, moved.Chassis = "SGP03", ""
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
	nodeDescribed(t, s, "n1", milan)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vmFromImage("web", "noble")); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "vm-web", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	waitVM(t, s, system, "web", "reporting its node's placement", func(vm *computev1alpha1.VirtualMachine) bool {
		return proto.Equal(vm.GetStatus().GetPlacement(), milan)
	})
	obj, err := s.backend(registry.VirtualMachine).Get(user, resource.Key{Project: "web", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if got := obj.(*computev1alpha1.VirtualMachine).GetStatus().GetPlacement(); !proto.Equal(got, milan) {
		t.Errorf("its user reads placement %v, want %v", got, milan)
	}

	moved := proto.Clone(milan).(*computev1alpha1.Placement)
	moved.CpuModel = "AMD EPYC 7413"
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
		Status:   &computev1alpha1.NodeStatus{Ipv6Prefix: testPrefix, Placement: milan},
	}
	m := func(owner, node string) *computev1alpha1.Machine {
		return &computev1alpha1.Machine{
			Metadata: &metav1.ObjectMeta{OwnerReferences: []*metav1.OwnerReference{{Uid: owner, Controller: true}}},
			Spec:     &computev1alpha1.MachineSpec{Node: node},
		}
	}
	got := placementOf("u1", m("u1", "n1"), node)
	if !proto.Equal(got, milan) {
		t.Errorf("placement %v, want %v", got, milan)
	}
	if got == milan {
		t.Error("the placement is the node's own message, not a copy")
	}
	for _, tt := range []struct {
		what string
		m    *computev1alpha1.Machine
		node *computev1alpha1.Node
	}{
		{"no machine", nil, node},
		{"another owner's machine", m("u2", "n1"), node},
		{"an unplaced machine", m("u1", ""), nil},
		{"a machine on another node", m("u1", "n2"), node},
		{"a machine whose node is gone", m("u1", "n1"), nil},
	} {
		if p := placementOf("u1", tt.m, tt.node); p != nil {
			t.Errorf("%s: placement %v", tt.what, p)
		}
	}
}
