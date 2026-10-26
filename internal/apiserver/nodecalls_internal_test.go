package apiserver

import (
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/auth"
)

func person(email string) auth.Principal { return auth.Principal{Type: auth.User, ID: email} }

// tenantFull is what only the tenant's cap refuses a call with.
func tenantFull(tenant string, max int) string {
	return fmt.Sprintf("tenant %q has %d calls open to nodes, the most its plan allows", tenant, max)
}

// TestNodeCallsAreCapped: past a tenant's cap its calls fail, others'
// do not; past a node's, every tenant's do; a slot given back, once only,
// is taken again. Every call is a caller's first, so no caller's cap
// refuses one.
func TestNodeCallsAreCapped(t *testing.T) {
	t.Parallel()
	n := newNodeCalls(100, 3)
	callers := 0
	caller := func(tenant string) auth.Principal {
		callers++
		return person(fmt.Sprintf("%s%d@example.com", tenant, callers))
	}
	take := func(tenant, node string) func() {
		t.Helper()
		release, err := n.take(caller(tenant), tenant, 2, node)
		if err != nil {
			t.Fatalf("%s on %s: %v", tenant, node, err)
		}
		return release
	}
	refused := func(tenant, node, want string) {
		t.Helper()
		_, err := n.take(caller(tenant), tenant, 2, node)
		if connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), want) {
			t.Errorf("%s on %s: %v, want resource exhausted saying %q", tenant, node, err, want)
		}
	}
	nodeFull := "node n1 has 3 calls open, the most it takes"
	a1 := take("a", "n1")
	take("a", "n2")
	refused("a", "n3", tenantFull("a", 2))
	b1 := take("b", "n1")
	take("c", "n1")
	refused("d", "n1", nodeFull)
	take("d", "n2")

	a1()
	a1()
	take("a", "n2")
	refused("a", "n2", tenantFull("a", 2))
	b1()
	take("e", "n1")
	take("f", "n1")
	refused("g", "n1", nodeFull)
}

// TestATenantsCapIsEachOfItsCallers: one caller may hold all its tenant's
// calls, and more than DefaultMachineCalls where the tenant's plan allows
// them; two callers share the tenant's.
func TestATenantsCapIsEachOfItsCallers(t *testing.T) {
	t.Parallel()
	n := newNodeCalls(0, 1000)
	token := auth.Principal{Type: auth.ServiceToken, ID: "tower"}
	for i := range 100 {
		if _, err := n.take(token, "t1", 100, "n1"); err != nil {
			t.Fatalf("call %d of a tenant allowing 100: %v", i+1, err)
		}
	}
	_, err := n.take(token, "t1", 100, "n1")
	if connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "serviceToken:tower") {
		t.Errorf("call 101 of a tenant allowing 100: %v, want the caller over its cap", err)
	}

	alice, bob := person("alice@example.com"), person("bob@example.com")
	release, err := n.take(alice, "t2", 2, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.take(bob, "t2", 2, "n1"); err != nil {
		t.Fatal(err)
	}
	_, err = n.take(alice, "t2", 2, "n1")
	if connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), tenantFull("t2", 2)) {
		t.Errorf("a third call of a tenant allowing 2: %v, want the tenant over its cap", err)
	}
	release()
	if _, err := n.take(bob, "t2", 2, "n1"); err != nil {
		t.Errorf("bob's call once alice's ended: %v", err)
	}
}

// TestACallersNodeCallsAreCappedAcrossTenants: making more tenants gives a
// caller no more calls; another caller, in the same tenants, has its own.
func TestACallersNodeCallsAreCappedAcrossTenants(t *testing.T) {
	t.Parallel()
	n := newNodeCalls(100, 100)
	alice, bob := person("alice@example.com"), person("bob@example.com")
	release, err := n.take(alice, "t1", 2, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.take(alice, "t2", 2, "n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.take(alice, "t3", 2, "n1"); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("alice's third call, in a tenant of its own: %v", err)
	}
	if _, err := n.take(alice, "t4", 3, "n1"); err != nil {
		t.Errorf("alice's third call, in a tenant allowing 3: %v", err)
	}
	if _, err := n.take(bob, "t1", 2, "n1"); err != nil {
		t.Errorf("bob's call in alice's tenant: %v", err)
	}
	// Another kind of principal of the same ID is another caller.
	if _, err := n.take(auth.Principal{Type: auth.ServiceToken, ID: alice.ID}, "t3", 2, "n1"); err != nil {
		t.Errorf("a service token sharing alice's ID: %v", err)
	}
	release()
	if _, err := n.take(alice, "t3", 3, "n1"); err != nil {
		t.Errorf("alice's call once one ended: %v", err)
	}
}

// TestACallerOfNoTenantHasItsOwnCap: root on the host is capped at
// maxNoTenant whatever the tenant allows, and still counts toward the
// tenant's.
func TestACallerOfNoTenantHasItsOwnCap(t *testing.T) {
	t.Parallel()
	n := newNodeCalls(2, 100)
	root := auth.Principal{Type: auth.Local, ID: "root"}
	for range 2 {
		if _, err := n.take(root, "t1", 100, "n1"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := n.take(root, "t1", 100, "n1"); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("root's third call: %v, want resource exhausted", err)
	}
	if _, err := n.take(person("alice@example.com"), "t1", 2, "n1"); connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), tenantFull("t1", 2)) {
		t.Errorf("a call of a tenant allowing 2 with root's 2 open: %v, want the tenant over its cap", err)
	}
	if _, err := n.take(person("alice@example.com"), "t2", 2, "n1"); err != nil {
		t.Errorf("a call of another tenant: %v", err)
	}
}

// TestMachineCallsOfATenant: a tenant whose Plan is missing has the
// default; a Plan that cannot be read, or a tenant that does not exist,
// fails the call, naming it.
func TestMachineCallsOfATenant(t *testing.T) {
	t.Parallel()
	s, system, _ := running(t)
	// Past admission, which refuses a tenant naming a Plan that does not
	// exist, or a Plan that does not parse.
	put(t, s, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "lost"}, Spec: &tenancyv1alpha1.TenantSpec{Plan: "gone"}})
	put(t, s, &tenancyv1alpha1.Plan{Metadata: &metav1.ObjectMeta{Name: "broken"},
		Spec: &tenancyv1alpha1.PlanSpec{Limits: &tenancyv1alpha1.Limits{Cpu: proto.String("1x"), MachineCalls: proto.Int32(3)}}})
	put(t, s, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "odd"}, Spec: &tenancyv1alpha1.TenantSpec{Plan: "broken"}})

	if n, err := s.machineCallsOf(system, "lost", `container "api"`); err != nil || n != DefaultMachineCalls {
		t.Errorf("a tenant whose plan is missing: %d, %v; want %d", n, err, DefaultMachineCalls)
	}
	if n, err := s.machineCallsOf(system, "odd", `container "api"`); err == nil {
		t.Errorf("a tenant whose plan does not parse: %d, want an error", n)
	}
	// Only a missing Plan is let through, not any other precondition.
	if _, _, err := s.planOf(system, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "lost"}, Spec: &tenancyv1alpha1.TenantSpec{Plan: "gone"}}); !isNoPlan(err) {
		t.Errorf("planOf of a missing plan: %v, not told apart", err)
	}
	if isNoPlan(failedPrecondition("tenant %q does not exist", "lost")) {
		t.Error("another failed precondition taken for a missing plan")
	}
	_, err := s.machineCallsOf(system, "nobody", `container "api"`)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), `container "api" is in tenant "nobody", which does not exist`) {
		t.Errorf("a tenant that does not exist: %v", err)
	}
}

func TestNodeCallsDefaults(t *testing.T) {
	t.Parallel()
	n := newNodeCalls(0, 0)
	if n.maxNoTenant != 64 || n.maxPerNode != 900 {
		t.Errorf("caps %d per caller of no tenant and %d per node", n.maxNoTenant, n.maxPerNode)
	}
}
