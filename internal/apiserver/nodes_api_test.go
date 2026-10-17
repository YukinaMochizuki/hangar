package apiserver_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// TestNodeOverTheWire covers what the node agent and people see of nodes
// through the real listeners: the agent on the socket reports on its own Node,
// only platform administrators read nodes, and nobody writes their spec.
func TestNodeOverTheWire(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	var node *computev1alpha1.Node
	testutil.Eventually(t, "node "+testNode+" registered", func() (bool, string) {
		res, err := e.as(root).nodes.GetNode(ctx, connect.NewRequest(&computev1alpha1.GetNodeRequest{Name: testNode}))
		if err != nil {
			if connect.CodeOf(err) != connect.CodeNotFound {
				t.Fatalf("node %s: %v", testNode, err)
			}
			return false, err.Error()
		}
		node = res.Msg.GetNode()
		return node != nil, "no node"
	})

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
		Capacity:     &computev1alpha1.NodeResources{Cpu: "32", Memory: "128Gi"},
		AgentVersion: "test",
	}
	got := must[computev1alpha1.UpdateNodeStatusResponse](t)(e.asNode().nodes.UpdateNodeStatus(ctx,
		connect.NewRequest(&computev1alpha1.UpdateNodeStatusRequest{Name: testNode, Status: status})))
	if got.GetNode().GetStatus().GetCapacity().GetMemory() != "128Gi" {
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
