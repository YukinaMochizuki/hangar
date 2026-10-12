package apiserver

import (
	"testing"

	"connectrpc.com/connect"

	"github.com/YukinaMochizuki/hangar/internal/auth"
)

func person(email string) auth.Principal { return auth.Principal{Type: auth.User, ID: email} }

// TestNodeCallsAreCapped: past a tenant's cap its calls fail, others'
// do not; past a node's, every tenant's do; a slot given back, once only,
// is taken again.
func TestNodeCallsAreCapped(t *testing.T) {
	n := newNodeCalls(100, 2, 3)
	take := func(tenant, node string) func() {
		t.Helper()
		release, err := n.take(person(tenant+"@example.com"), tenant, node)
		if err != nil {
			t.Fatalf("%s on %s: %v", tenant, node, err)
		}
		return release
	}
	refused := func(tenant, node, why string) {
		t.Helper()
		if _, err := n.take(person(tenant+"@example.com"), tenant, node); connect.CodeOf(err) != connect.CodeResourceExhausted {
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

// TestACallersNodeCallsAreCappedAcrossTenants: making more tenants gives a
// caller no more calls; another caller, in the same tenants, has its own.
func TestACallersNodeCallsAreCappedAcrossTenants(t *testing.T) {
	n := newNodeCalls(2, 100, 100)
	alice, bob := person("alice@example.com"), person("bob@example.com")
	release, err := n.take(alice, "t1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.take(alice, "t2", "n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.take(alice, "t3", "n1"); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("alice's third call, in a tenant of its own: %v", err)
	}
	if _, err := n.take(bob, "t1", "n1"); err != nil {
		t.Errorf("bob's call in alice's tenant: %v", err)
	}
	// Another kind of principal of the same ID is another caller.
	if _, err := n.take(auth.Principal{Type: auth.ServiceToken, ID: alice.ID}, "t3", "n1"); err != nil {
		t.Errorf("a service token sharing alice's ID: %v", err)
	}
	release()
	if _, err := n.take(alice, "t3", "n1"); err != nil {
		t.Errorf("alice's call once one ended: %v", err)
	}
}

func TestNodeCallsDefaults(t *testing.T) {
	n := newNodeCalls(0, 0, 0)
	if n.maxPrincipal != 64 || n.maxTenant != 64 || n.maxPerNode != 900 {
		t.Errorf("caps %d per caller, %d per tenant and %d per node", n.maxPrincipal, n.maxTenant, n.maxPerNode)
	}
}
