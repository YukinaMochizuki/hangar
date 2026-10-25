package apiserver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/authz"
)

const testShared = "198.51.100.3"

var (
	tcp = networkv1alpha1.Protocol_PROTOCOL_TCP
	udp = networkv1alpha1.Protocol_PROTOCOL_UDP
)

// forward is a PortForward of project web to container ctr's interface
// iface, "" for eth0.
func forward(name, ctr, iface string, protocol networkv1alpha1.Protocol, port int32) *networkv1alpha1.PortForward {
	return &networkv1alpha1.PortForward{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
		Spec: &networkv1alpha1.PortForwardSpec{
			Target:   &networkv1alpha1.PortForwardTarget{Object: &networkv1alpha1.PortForwardTarget_Container{Container: ctr}, Interface: iface},
			Protocol: protocol, TargetPort: port,
		},
	}
}

func applyForward(t *testing.T, s *Server, ctx context.Context, pf *networkv1alpha1.PortForward) *networkv1alpha1.PortForward {
	t.Helper()
	res, err := s.backend(registry.PortForward).Apply(ctx, pf)
	if err != nil {
		t.Fatal(err)
	}
	return res.Object.(*networkv1alpha1.PortForward)
}

func waitForward(t *testing.T, s *Server, system context.Context, name, what string, ok func(*networkv1alpha1.PortForward) bool) *networkv1alpha1.PortForward {
	t.Helper()
	return waitObject(t, s, system, registry.PortForward, resource.Key{Project: "web", Name: name}, "port forward "+name+" "+what, ok)
}

func readyReason(pf *networkv1alpha1.PortForward) string {
	return conditions.Find(pf.GetStatus().GetConditions(), PortForwardReady).GetReason()
}

// nodeSharing reports a node ready with room and the shared IPv4 address
// shared, none if empty.
func nodeSharing(t *testing.T, s *Server, node, shared string) {
	t.Helper()
	status := &computev1alpha1.NodeStatus{
		Capacity: &computev1alpha1.NodeResources{Cpu: "8", Memory: "4096Mi"},
		Conditions: []*metav1.Condition{{
			Type: NodeReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "AgentReporting",
		}},
		SharedIpv4: shared,
	}
	if _, err := s.backend(registry.Node).UpdateStatus(asNode(t.Context(), node), resource.Key{Name: node}, status, nil); err != nil {
		t.Fatal(err)
	}
}

// drawFrom has s draw public ports from lo to hi alone.
func drawFrom(s *Server, lo, hi int32) {
	s.portForwardPorts.Lock()
	defer s.portForwardPorts.Unlock()
	s.testForwardPorts = [2]int32{lo, hi}
}

func TestPortForwardAdmission(t *testing.T) {
	t.Parallel()
	s, _, user := running(t)
	b := s.backend(registry.PortForward)
	if _, err := b.Apply(user, forward("waits", "not-made-yet", "", tcp, 80)); err != nil {
		t.Errorf("a forward to a container not made yet: %v", err)
	}
	noTarget := forward("bad", "x", "", tcp, 80)
	noTarget.Spec.Target = nil
	vmOtherProject := forward("bad", "", "", tcp, 80)
	vmOtherProject.Spec.Target = &networkv1alpha1.PortForwardTarget{Object: &networkv1alpha1.PortForwardTarget_VirtualMachine{VirtualMachine: "other/vm"}}
	for what, pf := range map[string]*networkv1alpha1.PortForward{
		"no target":                      noTarget,
		"a target naming no object":      forward("bad", "", "", tcp, 80),
		"a target in another project":    forward("bad", "other/api", "", tcp, 80),
		"a VM in another project":        vmOtherProject,
		"an interface's name not a name": forward("bad", "api", "Eth 0", tcp, 80),
		"no protocol":                    forward("bad", "api", "", networkv1alpha1.Protocol_PROTOCOL_UNSPECIFIED, 80),
		"a protocol that is none":        forward("bad", "api", "", networkv1alpha1.Protocol(7), 80),
		"no target port":                 forward("bad", "api", "", tcp, 0),
		"a target port past 65535":       forward("bad", "api", "", udp, 65536),
		"a negative target port":         forward("bad", "api", "", udp, -1),
	} {
		_, err := b.Apply(user, pf)
		wantCodeOf(t, err, connect.CodeInvalidArgument, what)
	}
	if obj, err := s.get(user, registry.PortForward, resource.Key{Project: "web", Name: "bad"}); err != nil || obj != nil {
		t.Errorf("a refused forward was stored: %v %v", obj, err)
	}
	_, err := b.Apply(user, forward("waits", "not-made-yet", "", udp, 80))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "changing the protocol")
	if err != nil && !strings.Contains(err.Error(), "spec.protocol") {
		t.Errorf("changing the protocol is refused as %v, not naming spec.protocol", err)
	}
}

// TestAPortIsDrawnAndKept: a new forward's port is of the range, unique per
// protocol, and kept whatever is applied after.
func TestAPortIsDrawnAndKept(t *testing.T) {
	t.Parallel()
	s, _, user := running(t)
	pf := applyForward(t, s, user, forward("web", "a", "", tcp, 80))
	port := pf.GetStatus().GetPort()
	if port < PortForwardPortMin || port > PortForwardPortMax {
		t.Fatalf("drawn port %d, want one from %d to %d", port, PortForwardPortMin, PortForwardPortMax)
	}
	// Retargeted, with another target port, and with a status of its own
	// sent along, it keeps its port.
	moved := forward("web", "b", "eth1", tcp, 8080)
	moved.Status = &networkv1alpha1.PortForwardStatus{Port: 1}
	if got := applyForward(t, s, user, moved).GetStatus().GetPort(); got != port {
		t.Errorf("retargeting changed the port from %d to %d", port, got)
	}
	// A status sent with a create is no port either.
	fresh := forward("fresh", "a", "", tcp, 80)
	fresh.Status = &networkv1alpha1.PortForwardStatus{Port: port}
	if got := applyForward(t, s, user, fresh).GetStatus().GetPort(); got == port || got < PortForwardPortMin {
		t.Errorf("a create sending port %d got %d", port, got)
	}

	// One port left: TCP and UDP each have it.
	drawFrom(s, 20000, 20000)
	if p := applyForward(t, s, user, forward("t", "a", "", tcp, 80)).GetStatus().GetPort(); p != 20000 {
		t.Errorf("TCP drew %d from the one port 20000", p)
	}
	if p := applyForward(t, s, user, forward("u", "a", "", udp, 80)).GetStatus().GetPort(); p != 20000 {
		t.Errorf("UDP drew %d from the one port 20000, which only TCP holds", p)
	}
	_, err := s.backend(registry.PortForward).Apply(user, forward("t2", "a", "", tcp, 80))
	wantExhausted(t, err, "every public TCP port from 20000 to 20000 is held", "a TCP forward with no port left")
}

// otherProject makes project other of the same tenant, which user owns.
func otherProject(t *testing.T, s *Server, user context.Context) {
	t.Helper()
	if _, err := s.backend(registry.Project).Apply(user, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "other"}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "acme"},
	}); err != nil {
		t.Fatal(err)
	}
}

// in moves pf to project.
func in(project string, pf *networkv1alpha1.PortForward) *networkv1alpha1.PortForward {
	pf.Metadata.Project = project
	return pf
}

// TestAPortIsUniqueAcrossProjects: a port another project's forward holds
// is no more free than one of the same project's.
func TestAPortIsUniqueAcrossProjects(t *testing.T) {
	t.Parallel()
	s, _, user := running(t)
	otherProject(t, s, user)
	drawFrom(s, 20000, 20000)
	applyForward(t, s, user, forward("web", "a", "", tcp, 80))
	_, err := s.backend(registry.PortForward).Apply(user, in("other", forward("web", "a", "", tcp, 80)))
	wantExhausted(t, err, "every public TCP port from 20000 to 20000 is held", "a forward in another project drawing a held port")
}

// TestDrawingIsOneDecision: creators racing for fewer ports than there are
// of them, in two projects, each get another, and the rest are refused.
// Half race as the system, whose writes the quota does not serialize.
func TestDrawingIsOneDecision(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	otherProject(t, s, user)
	drawFrom(s, 20000, 20003)
	var wg sync.WaitGroup
	errs := make([]error, 12)
	for i := range errs {
		ctx := user
		if i%2 == 0 {
			ctx = system
		}
		project := []string{"web", "other"}[i/2%2]
		wg.Go(func() {
			_, errs[i] = s.backend(registry.PortForward).Apply(ctx, in(project, forward(fmt.Sprintf("f%d", i), "a", "", tcp, 80)))
		})
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case connect.CodeOf(err) != connect.CodeResourceExhausted:
			t.Errorf("a creator was refused with %v", err)
		}
	}
	all, err := s.portForwards(system, "")
	if err != nil {
		t.Fatal(err)
	}
	ports := map[int32]string{}
	for _, pf := range all {
		p, name := pf.GetStatus().GetPort(), pf.GetMetadata().GetProject()+"/"+pf.GetMetadata().GetName()
		if other, ok := ports[p]; ok {
			t.Errorf("%s and %s both hold port %d", other, name, p)
		}
		ports[p] = name
	}
	if won != 4 || len(all) != 4 {
		t.Errorf("%d of 12 creators won 4 ports, %d stored", won, len(all))
	}
}

// TestPortForwardFollowsItsTarget: waiting for its target, for a node with a
// shared address, then forwarding, and waiting again for an interface it
// does not have.
func TestPortForwardFollowsItsTarget(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeSharing(t, s, "n1", "")
	applyForward(t, s, user, forward("web", "a", "", tcp, 80))
	waitForward(t, s, system, "web", "waiting for its target", func(pf *networkv1alpha1.PortForward) bool {
		return readyReason(pf) == ReasonTargetNotFound
	})
	applyCtr(t, s, user, ctr("a"))
	pf := waitForward(t, s, system, "web", "waiting for a shared address", func(pf *networkv1alpha1.PortForward) bool {
		return readyReason(pf) == ReasonNoSharedIPv4
	})
	if pf.GetStatus().GetIpv4() != "" || pf.GetStatus().GetNode() != "n1" || pf.GetStatus().GetMachine() != "ct-a" {
		t.Errorf("without a shared address the forward is at %q on %q to %q", pf.GetStatus().GetIpv4(), pf.GetStatus().GetNode(), pf.GetStatus().GetMachine())
	}
	nodeSharing(t, s, "n1", testShared)
	pf = waitForward(t, s, system, "web", "forwarding", func(pf *networkv1alpha1.PortForward) bool {
		return readyReason(pf) == ReasonForwarding
	})
	if !conditions.IsTrue(pf.GetStatus().GetConditions(), PortForwardReady) || pf.GetStatus().GetIpv4() != testShared {
		t.Errorf("forwarding, the forward is %v at %q", conditions.Find(pf.GetStatus().GetConditions(), PortForwardReady), pf.GetStatus().GetIpv4())
	}
	port := pf.GetStatus().GetPort()

	applyForward(t, s, user, forward("web", "a", "eth1", tcp, 80))
	pf = waitForward(t, s, system, "web", "missing its interface", func(pf *networkv1alpha1.PortForward) bool {
		return readyReason(pf) == ReasonInterfaceNotFound
	})
	if pf.GetStatus().GetPort() != port {
		t.Errorf("retargeting changed the port from %d to %d", port, pf.GetStatus().GetPort())
	}
	applyForward(t, s, user, forward("web", "b", "", tcp, 80))
	waitForward(t, s, system, "web", "waiting for b", func(pf *networkv1alpha1.PortForward) bool {
		return readyReason(pf) == ReasonTargetNotFound && pf.GetStatus().GetMachine() == ""
	})
	applyCtr(t, s, user, ctr("b"))
	waitForward(t, s, system, "web", "forwarding to b", func(pf *networkv1alpha1.PortForward) bool {
		return readyReason(pf) == ReasonForwarding && pf.GetStatus().GetMachine() == "ct-b" && pf.GetStatus().GetPort() == port
	})
}

// TestADeletedForwardHoldsItsPortUntilItsNodeLetsGo: the port of a forward
// being deleted is drawn by no other until the node that opened it reports
// it closed; no other node can say so.
func TestADeletedForwardHoldsItsPortUntilItsNodeLetsGo(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1", "n2")
	nodeSharing(t, s, "n1", testShared)
	nodeSharing(t, s, "n2", "")
	drawFrom(s, 20000, 20000)
	applyCtr(t, s, user, ctr("a"))
	applyForward(t, s, user, forward("old", "a", "", tcp, 80))
	pf := waitForward(t, s, system, "old", "placed", func(pf *networkv1alpha1.PortForward) bool { return pf.GetStatus().GetNode() != "" })
	node, other := pf.GetStatus().GetNode(), "n2"
	if node == "n2" {
		other = "n1"
	}
	key := resource.Key{Project: "web", Name: "old"}
	b := s.backend(registry.PortForward)
	if _, err := b.Delete(user, key, nil); err != nil {
		t.Fatal(err)
	}
	waitForward(t, s, system, "old", "being deleted", func(pf *networkv1alpha1.PortForward) bool {
		return pf.GetMetadata().GetDeleteTime() != nil
	})
	_, err := b.Apply(user, forward("new", "a", "", tcp, 80))
	wantExhausted(t, err, "every public TCP port", "a forward drawing the port of one being deleted")

	released := func(pf *networkv1alpha1.PortForward) *networkv1alpha1.PortForwardStatus {
		st := proto.Clone(pf.GetStatus()).(*networkv1alpha1.PortForwardStatus)
		st.Conditions = append(st.Conditions, &metav1.Condition{
			Type: PortForwardReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "PortClosed",
		})
		// What a node sends besides its condition is not taken.
		st.Port, st.Node = 1, other
		return st
	}
	pf = mustGet[*networkv1alpha1.PortForward](t, s, system, registry.PortForward, key)
	_, err = b.UpdateStatus(asNode(system, other), key, released(pf), nil)
	wantCodeOf(t, err, connect.CodePermissionDenied, "another node reporting the port closed")
	if _, err := b.UpdateStatus(asNode(system, node), key, released(pf), nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.PortForward, "old")
	if p := applyForward(t, s, user, forward("new", "a", "", tcp, 80)).GetStatus().GetPort(); p != 20000 {
		t.Errorf("the port let go of is not drawn again: got %d", p)
	}
}

// TestAForwardNeverPlacedGoesAtOnce: deleted before any node held it, a
// forward waits for none.
func TestAForwardNeverPlacedGoesAtOnce(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	applyForward(t, s, user, forward("web", "nothing", "", udp, 53))
	waitForward(t, s, system, "web", "waiting for its target", func(pf *networkv1alpha1.PortForward) bool {
		return readyReason(pf) == ReasonTargetNotFound
	})
	if _, err := s.backend(registry.PortForward).Delete(user, resource.Key{Project: "web", Name: "web"}, nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.PortForward, "web")
}

// TestANodeReadsOnlyItsForwards: a node reads the forwards it opens, and no
// other, nor reports on them. Get, list and watch judge an object alike
// (backend.mayRead).
func TestANodeReadsOnlyItsForwards(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1", "n2")
	nodeSharing(t, s, "n1", testShared)
	nodeSharing(t, s, "n2", testShared)
	applyCtr(t, s, user, ctr("a"))
	applyForward(t, s, user, forward("web", "a", "", tcp, 80))
	pf := waitForward(t, s, system, "web", "placed", func(pf *networkv1alpha1.PortForward) bool { return pf.GetStatus().GetNode() != "" })
	mine, other := pf.GetStatus().GetNode(), "n1"
	if mine == "n1" {
		other = "n2"
	}
	b := s.backend(registry.PortForward)
	key := resource.Key{Project: "web", Name: "web"}
	if _, err := b.Get(asNode(system, mine), key); err != nil {
		t.Errorf("its node reading the forward: %v", err)
	}
	_, err := b.Get(asNode(system, other), key)
	wantCodeOf(t, err, connect.CodePermissionDenied, "another node reading the forward")
	for node, want := range map[string]int{mine: 1, other: 0} {
		for _, project := range []string{"", "web"} {
			res, err := b.List(asNode(system, node), resource.ListOptions{Project: project})
			if err != nil || len(res.Items) != want {
				t.Errorf("node %s lists %d forwards in %q (%v), want %d", node, len(res.Items), project, err, want)
			}
		}
	}
	_, err = b.UpdateStatus(asNode(system, other), key, &networkv1alpha1.PortForwardStatus{}, nil)
	wantCodeOf(t, err, connect.CodePermissionDenied, "another node reporting on the forward")
	// Its node writes nothing but its own condition.
	if _, err := b.UpdateStatus(asNode(system, mine), key, &networkv1alpha1.PortForwardStatus{Port: 1, Ipv4: "192.0.2.9"}, nil); err != nil {
		t.Fatal(err)
	}
	got := mustGet[*networkv1alpha1.PortForward](t, s, system, registry.PortForward, key)
	if got.GetStatus().GetPort() != pf.GetStatus().GetPort() || got.GetStatus().GetNode() != mine {
		t.Errorf("a node's report changed the forward's status to %v", got.GetStatus())
	}
}

// TestPortForwardPermissions: network.portforwards.* are the kind's
// permissions; a viewer reads and writes nothing, and one with no grant
// reads nothing either.
func TestPortForwardPermissions(t *testing.T) {
	t.Parallel()
	for _, v := range []authz.Verb{authz.Get, authz.List, authz.Watch, authz.Apply, authz.Delete, authz.UpdateStatus} {
		perm := "network.portforwards." + string(v)
		if r, verb, ok := authz.LookupPermission(perm); !ok || r.Kind != registry.PortForward.Kind || verb != v {
			t.Errorf("%s is %v %v %t", perm, r.Kind, verb, ok)
		}
	}
	s, system, user := running(t)
	applyForward(t, s, user, forward("web", "a", "", tcp, 80))
	b := s.backend(registry.PortForward)
	key := resource.Key{Project: "web", Name: "web"}
	viewer := bindProjectRole(t, s, system, "carol@example.com", "project.viewer")
	if _, err := b.Get(viewer, key); err != nil {
		t.Errorf("a viewer reading a forward: %v", err)
	}
	_, err := b.Apply(viewer, forward("mine", "a", "", tcp, 80))
	wantCodeOf(t, err, connect.CodePermissionDenied, "a viewer making a forward")
	_, err = b.Delete(viewer, key, nil)
	wantCodeOf(t, err, connect.CodePermissionDenied, "a viewer deleting a forward")
	_, err = b.UpdateStatus(user, key, &networkv1alpha1.PortForwardStatus{Port: 1}, nil)
	wantCodeOf(t, err, connect.CodePermissionDenied, "the project's owner writing a forward's status")

	addMember(t, s, "dave@example.com")
	stranger := asUser("dave@example.com")
	_, err = b.Get(stranger, key)
	wantCodeOf(t, err, connect.CodePermissionDenied, "someone with no grant reading a forward")
	_, err = b.Apply(stranger, forward("theirs", "a", "", tcp, 80))
	wantCodeOf(t, err, connect.CodePermissionDenied, "someone with no grant making a forward")
}

func TestPortForwardsAreCounted(t *testing.T) {
	t.Parallel()
	s, _, user := running(t)
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{PortForwards: proto.Int32(1)})
	applyForward(t, s, user, forward("one", "a", "", tcp, 80))
	_, err := s.backend(registry.PortForward).Apply(user, forward("two", "a", "", udp, 53))
	wantExhausted(t, err, "port_forwards limit is 1", "a second forward past the plan")
	// Applying the one there is again takes nothing more.
	applyForward(t, s, user, forward("one", "b", "", tcp, 80))
}
