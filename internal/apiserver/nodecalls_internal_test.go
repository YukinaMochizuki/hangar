package apiserver

import (
	"testing"

	"connectrpc.com/connect"
)

// TestNodeCallsAreCapped: past a tenant's cap its calls fail, others'
// do not; past a node's, every tenant's do; a slot given back, once only,
// is taken again.
func TestNodeCallsAreCapped(t *testing.T) {
	n := newNodeCalls(2, 3)
	take := func(tenant, node string) func() {
		t.Helper()
		release, err := n.take(tenant, node)
		if err != nil {
			t.Fatalf("%s on %s: %v", tenant, node, err)
		}
		return release
	}
	refused := func(tenant, node, why string) {
		t.Helper()
		if _, err := n.take(tenant, node); connect.CodeOf(err) != connect.CodeResourceExhausted {
			t.Errorf("%s on %s past %s's cap: %v", tenant, node, why, err)
		}
	}
	a1 := take("a", "n1")
	take("a", "n2")
	refused("a", "n3", "the tenant")
	b1 := take("b", "n1")
	take("c", "n1")
	refused("d", "n1", "the node")
	take("d", "n2")

	a1()
	a1()
	take("a", "n2")
	refused("a", "n2", "the tenant")
	b1()
	take("e", "n1")
	take("f", "n1")
	refused("g", "n1", "the node")
}

func TestNodeCallsDefaults(t *testing.T) {
	n := newNodeCalls(0, 0)
	if n.maxTenant != 64 || n.maxPerNode != 900 {
		t.Errorf("caps %d per tenant and %d per node", n.maxTenant, n.maxPerNode)
	}
}
