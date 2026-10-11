package apiserver_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
)

// TestNodeOverTheWire covers what the node agent and people see of nodes
// through the real listeners: the agent on the socket reports on its own Node,
// only platform administrators read nodes, and nobody writes their spec.
func TestNodeOverTheWire(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	var node *computev1alpha1.Node
	deadline := time.Now().Add(10 * time.Second)
	for node == nil {
		res, err := e.as(root).nodes.GetNode(ctx, connect.NewRequest(&computev1alpha1.GetNodeRequest{Name: testNode}))
		switch {
		case err == nil:
			node = res.Msg.GetNode()
		case connect.CodeOf(err) != connect.CodeNotFound || time.Now().After(deadline):
			t.Fatalf("node %s never registered: %v", testNode, err)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	_, err := e.as(alice).nodes.GetNode(ctx, connect.NewRequest(&computev1alpha1.GetNodeRequest{Name: testNode}))
	wantCode(t, err, connect.CodePermissionDenied, "a member reading a node")
	list := must[computev1alpha1.ListNodesResponse](t)(e.as(alice).nodes.ListNodes(ctx, connect.NewRequest(&computev1alpha1.ListNodesRequest{})))
	if len(list.GetItems()) != 0 {
		t.Errorf("a member lists %d nodes", len(list.GetItems()))
	}

	cordon := &computev1alpha1.Node{Metadata: &metav1.ObjectMeta{Name: testNode}, Spec: &computev1alpha1.NodeSpec{Unschedulable: true}}
	_, err = e.as(root).nodes.ApplyNode(ctx, connect.NewRequest(&computev1alpha1.ApplyNodeRequest{Node: cordon}))
	wantCode(t, err, connect.CodePermissionDenied, "root applying a node")
	_, err = e.asNode().nodes.ApplyNode(ctx, connect.NewRequest(&computev1alpha1.ApplyNodeRequest{Node: cordon}))
	wantCode(t, err, connect.CodePermissionDenied, "the agent applying its node")
	_, err = e.as(root).nodes.DeleteNode(ctx, connect.NewRequest(&computev1alpha1.DeleteNodeRequest{Name: testNode}))
	wantCode(t, err, connect.CodePermissionDenied, "root deleting a node")

	status := &computev1alpha1.NodeStatus{
		Capacity:     &computev1alpha1.NodeResources{Vcpus: 32, MemoryMib: 131072},
		AgentVersion: "test",
	}
	got := must[computev1alpha1.UpdateNodeStatusResponse](t)(e.asNode().nodes.UpdateNodeStatus(ctx,
		connect.NewRequest(&computev1alpha1.UpdateNodeStatusRequest{Name: testNode, Status: status})))
	if got.GetNode().GetStatus().GetCapacity().GetMemoryMib() != 131072 {
		t.Errorf("the agent's report was not stored: %v", got.GetNode().GetStatus())
	}
	_, err = e.as(root).nodes.UpdateNodeStatus(ctx,
		connect.NewRequest(&computev1alpha1.UpdateNodeStatusRequest{Name: testNode, Status: status}))
	wantCode(t, err, connect.CodePermissionDenied, "root writing a node's status")

	machines := must[computev1alpha1.ListMachinesResponse](t)(e.asNode().machines.ListMachines(ctx, connect.NewRequest(&computev1alpha1.ListMachinesRequest{})))
	if len(machines.GetItems()) != 0 {
		t.Errorf("a fresh server has %d machines", len(machines.GetItems()))
	}
}
