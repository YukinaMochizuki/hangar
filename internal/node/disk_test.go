package node_test

import (
	"errors"
	"testing"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
)

// TestTheNodeReportsItsDisk: the node's disk is its storage's, less what it
// keeps back, and a storage that cannot be read for a while does not take
// it away.
func TestTheNodeReportsItsDisk(t *testing.T) {
	env := apiservertest.Start(t, nil)
	host := fake.New()
	startAgent(t, env, &computev1alpha1.NodeResources{Cpu: "8", Memory: "8Gi"},
		&computev1alpha1.NodeResources{Disk: "24Gi"},
		func(c *node.Config) {
			c.Drivers = &node.Drivers{
				Images: host.Images(), Storage: host.Storage(), Network: host.Network(),
				Hypervisor: host.Hypervisor(), Guest: host.Guest(),
			}
		})
	root := env.As(apiservertest.Root)
	get := func() (*computev1alpha1.Node, error) {
		obj, err := root.Get(t.Context(), registry.Node, client.Key{Name: apiservertest.Node})
		if err != nil {
			return nil, err
		}
		return obj.(*computev1alpha1.Node), nil
	}
	reports := func(n *computev1alpha1.Node) bool {
		// fake.Capacity is 1 TiB.
		return n.GetStatus().GetCapacity().GetDisk() == "1024Gi" && n.GetStatus().GetAllocatable().GetDisk() == "1000Gi"
	}
	n := waitValue(t, "the node reporting its disk", get, reports)

	host.FailCapacity(errors.New("zfs is busy"))
	seen := n.GetStatus().GetLastHeartbeatTime().AsTime()
	n = waitValue(t, "two more heartbeats", get, func(n *computev1alpha1.Node) bool {
		return n.GetStatus().GetLastHeartbeatTime().AsTime().After(seen.Add(30 * time.Millisecond))
	})
	if !reports(n) {
		t.Errorf("with the storage unreadable, the node reports %v", n.GetStatus())
	}
}

// TestDiskCapacity: the node never reports more disk than its storage has.
func TestDiskCapacity(t *testing.T) {
	for total, want := range map[int64]string{
		10<<30 + 512<<20: "10Gi",
		10 << 30:         "10Gi",
		1<<30 - 1:        "0Gi",
		-1:               "0Gi",
	} {
		if got := node.DiskCapacity(driver.Capacity{Total: total}); got != want {
			t.Errorf("%d bytes: %q, want %q", total, got, want)
		}
	}
}

// TestStorageNeverReadIsNoDisk: a node whose storage could not be read yet
// reports no disk, so no machine with a disk is placed on it, until it can.
func TestStorageNeverReadIsNoDisk(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	host := fake.New()
	host.FailCapacity(errors.New("zfs is busy"))
	startAgent(t, env, &computev1alpha1.NodeResources{Cpu: "8", Memory: "8Gi"}, nil, func(c *node.Config) {
		c.Drivers = &node.Drivers{
			Images: host.Images(), Storage: host.Storage(), Network: host.Network(),
			Hypervisor: host.Hypervisor(), Guest: host.Guest(),
		}
	})
	root := env.As(apiservertest.Root)
	waitValue(t, "the node reporting no disk", func() (*computev1alpha1.Node, error) {
		obj, err := root.Get(t.Context(), registry.Node, client.Key{Name: apiservertest.Node})
		if err != nil {
			return nil, err
		}
		return obj.(*computev1alpha1.Node), nil
	}, func(n *computev1alpha1.Node) bool { return n.GetStatus().GetCapacity().GetDisk() == "0Gi" })
	if _, err := root.Apply(t.Context(), registry.Container, &computev1alpha1.Container{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: "a"},
		Spec:     &computev1alpha1.ContainerSpec{Image: "nginx", Resources: &computev1alpha1.Resources{Disk: "100Gi"}},
	}); err != nil {
		t.Fatal(err)
	}
	machine := func() (*computev1alpha1.Machine, error) {
		obj, err := root.Get(t.Context(), registry.Machine, client.Key{Project: apiservertest.Project, Name: "ct-a"})
		if err != nil {
			return nil, err
		}
		return obj.(*computev1alpha1.Machine), nil
	}
	m := waitValue(t, "the machine found no node", machine, func(m *computev1alpha1.Machine) bool {
		c := conditions.Find(m.GetStatus().GetConditions(), computeapi.MachineScheduled)
		return c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE
	})
	if m.GetSpec().GetNode() != "" {
		t.Fatalf("placed on %s, which has no disk", m.GetSpec().GetNode())
	}
	host.FailCapacity(nil)
	waitValue(t, "the machine placed once the storage is read", machine, func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == apiservertest.Node
	})
}
