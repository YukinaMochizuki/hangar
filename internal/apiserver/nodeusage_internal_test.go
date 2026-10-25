package apiserver

import (
	"testing"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	monitoringv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/monitoring/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
)

// TestNodeUsageSummary: each node counts the machines placed on it and sums
// their vCPUs and memory; its disk is theirs and the Disks' placed there, as
// the scheduler counts it. A node with nothing on it answers zeros.
func TestNodeUsageSummary(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1", "n2")
	nodeSized(t, s, system, "n1", 8192, 100)
	nodeWithRoom(t, s, system, "n2", 0)
	applyCtr(t, s, user, withResources("a", &computev1alpha1.Resources{Cpu: "2", Memory: "1Gi", Disk: "10Gi"}))
	applyCtr(t, s, user, withResources("b", &computev1alpha1.Resources{Cpu: "1", Memory: "512Mi", Disk: "5Gi"}))
	for _, c := range []string{"a", "b"} {
		waitMachine(t, s, system, machineName(c), "placed on n1", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	}
	if _, err := s.backend(registry.Disk).Apply(user, disk("data", "20Gi")); err != nil {
		t.Fatal(err)
	}
	bindDisk(t, s, system, "data", "n1", "20Gi")

	got, err := s.nodeUsageSummary(system)
	if err != nil {
		t.Fatal(err)
	}
	want := []*monitoringv1alpha1.NodeUsage{
		{Node: "n1", Machines: 2, Cpu: "3", Memory: "1536Mi", Disk: "35Gi"},
		{Node: "n2", Cpu: "0", Memory: "0", Disk: "0"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("node %d: got %v, want %v", i, got[i], want[i])
		}
	}
}
