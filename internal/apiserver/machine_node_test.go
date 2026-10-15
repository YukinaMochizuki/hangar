package apiserver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

func asNode(ctx context.Context, name string) context.Context {
	return auth.WithPrincipal(ctx, auth.Principal{Type: auth.Node, ID: name})
}

// asRoot makes root@example.com a platform root, signed up, and acts as them.
func asRoot(t *testing.T, s *Server, system context.Context) context.Context {
	t.Helper()
	invite(t, s, "root@example.com", "platform.root")
	signUp(t, s, "root@example.com")
	return auth.WithPrincipal(system, auth.Principal{Type: auth.User, ID: "root@example.com"})
}

func machine(name, node string) *computev1alpha1.Machine {
	return &computev1alpha1.Machine{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
		Spec: &computev1alpha1.MachineSpec{
			Node:          node,
			RunState:      computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING,
			RestartPolicy: computev1alpha1.RestartPolicy_RESTART_POLICY_ALWAYS,
			Boot:          &computev1alpha1.MachineSpec_Container{Container: &computev1alpha1.ContainerBoot{Image: "nginx"}},
			Interfaces: []*computev1alpha1.MachineInterface{
				{Name: "eth0", Network: "default"},
			},
		},
	}
}

func wantCodeOf(t *testing.T, err error, code connect.Code, what string) {
	t.Helper()
	if connect.CodeOf(err) != code {
		t.Errorf("%s: got %v, want %s", what, err, code)
	}
}

func TestOnlyTheSystemWritesMachines(t *testing.T) {
	s, system, user := running(t)
	root := asRoot(t, s, system)
	b := s.backend(registry.Machine)
	key := resource.Key{Project: "web", Name: "ct-api"}

	if _, err := b.Apply(system, machine("ct-api", "")); err != nil {
		t.Fatal(err)
	}

	// The project's owner and root can read it...
	for who, ctx := range map[string]context.Context{"owner": user, "root": root} {
		if _, err := b.Get(ctx, key); err != nil {
			t.Errorf("%s reading a machine: %v", who, err)
		}
		list, err := b.List(ctx, resource.ListOptions{Project: "web"})
		if err != nil || len(list.Items) != 1 {
			t.Errorf("%s listing machines: %d items, %v", who, len(list.Items), err)
		}
	}

	// ...but no one but the system writes it, whatever their role.
	for who, ctx := range map[string]context.Context{"owner": user, "root": root, "node": asNode(system, "n1")} {
		_, err := b.Apply(ctx, machine("ct-api", "n1"))
		wantCodeOf(t, err, connect.CodePermissionDenied, who+" applying a machine")
		_, err = b.Apply(ctx, machine("ct-new", ""))
		wantCodeOf(t, err, connect.CodePermissionDenied, who+" creating a machine")
		_, err = b.Delete(ctx, key, nil)
		wantCodeOf(t, err, connect.CodePermissionDenied, who+" deleting a machine")
	}
	if _, err := b.Get(user, key); err != nil {
		t.Errorf("the machine did not survive the attempts: %v", err)
	}
}

func TestOnlyTheMachinesNodeWritesItsStatus(t *testing.T) {
	s, system, user := running(t)
	b := s.backend(registry.Machine)
	key := resource.Key{Project: "web", Name: "ct-api"}
	status := &computev1alpha1.MachineStatus{Phase: computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING}

	if _, err := b.Apply(system, machine("ct-api", "")); err != nil {
		t.Fatal(err)
	}
	_, err := b.UpdateStatus(asNode(system, "n1"), key, status, nil)
	wantCodeOf(t, err, connect.CodePermissionDenied, "a node reporting on an unplaced machine")
	_, err = b.UpdateStatus(asNode(system, ""), key, status, nil)
	wantCodeOf(t, err, connect.CodePermissionDenied, "a nameless node reporting on an unplaced machine")

	if _, err := b.Apply(system, machine("ct-api", "n1")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.UpdateStatus(asNode(system, "n1"), key, status, nil); err != nil {
		t.Errorf("the machine's node reporting: %v", err)
	}
	_, err = b.UpdateStatus(asNode(system, "n2"), key, status, nil)
	wantCodeOf(t, err, connect.CodePermissionDenied, "another node reporting")
	_, err = b.UpdateStatus(asNode(system, ""), key, status, nil)
	wantCodeOf(t, err, connect.CodePermissionDenied, "a node without a name reporting")
	_, err = b.UpdateStatus(user, key, status, nil)
	wantCodeOf(t, err, connect.CodePermissionDenied, "the owner reporting")
}

func TestMachineAdmission(t *testing.T) {
	s, system, _ := running(t)
	b := s.backend(registry.Machine)

	if _, err := b.Apply(system, machine("ct-api", "n1")); err != nil {
		t.Fatal(err)
	}
	_, err := b.Apply(system, machine("ct-api", "n2"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "moving a placed machine to another node")
	_, err = b.Apply(system, machine("ct-api", ""))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "unplacing a placed machine")

	noBoot := machine("ct-noboot", "")
	noBoot.Spec.Boot = nil
	_, err = b.Apply(system, noBoot)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a machine with nothing to boot")

	twice := machine("ct-twice", "")
	twice.Spec.Interfaces = append(twice.Spec.Interfaces, &computev1alpha1.MachineInterface{Name: "eth0", Network: "other"})
	_, err = b.Apply(system, twice)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "two interfaces with one name")

	noNet := machine("ct-nonet", "")
	noNet.Spec.Interfaces[0].Network = ""
	_, err = b.Apply(system, noNet)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "an interface without a network")

	for name, mutate := range map[string]func(*computev1alpha1.MachineSpec){
		"an unspecified run state":      func(s *computev1alpha1.MachineSpec) { s.RunState = 0 },
		"an unknown run state":          func(s *computev1alpha1.MachineSpec) { s.RunState = 99 },
		"an unspecified restart policy": func(s *computev1alpha1.MachineSpec) { s.RestartPolicy = 0 },
		"an unknown restart policy":     func(s *computev1alpha1.MachineSpec) { s.RestartPolicy = 99 },
	} {
		m := machine("ct-enum", "")
		mutate(m.Spec)
		_, err = b.Apply(system, m)
		wantCodeOf(t, err, connect.CodeInvalidArgument, name)
	}
}

// waitNode waits for the apiserver to register a node.
func waitNode(t *testing.T, s *Server, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := s.store.Get(context.Background(), store.Key{Kind: registry.Node.Kind, Name: name})
		if err == nil {
			return
		}
		if !errors.Is(err, store.ErrNotFound) || time.Now().After(deadline) {
			t.Fatalf("node %s never registered: %v", name, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNodesAreRegisteredAndKeepTheirSpec(t *testing.T) {
	s, system, user := runningWith(t, func(c *Config) { c.Nodes = []string{"n1"} })
	root := asRoot(t, s, system)
	waitNode(t, s, "n1")
	b := s.backend(registry.Node)
	key := resource.Key{Name: "n1"}

	if _, err := b.Get(root, key); err != nil {
		t.Errorf("root reading a node: %v", err)
	}
	_, err := b.Get(user, key)
	wantCodeOf(t, err, connect.CodePermissionDenied, "a tenant owner reading a node")
	cordoned := &computev1alpha1.Node{Metadata: &metav1.ObjectMeta{Name: "n1"}, Spec: &computev1alpha1.NodeSpec{Unschedulable: true}}
	_, err = b.Apply(root, cordoned)
	wantCodeOf(t, err, connect.CodePermissionDenied, "root writing a node")

	// What the system set on a node outlives a restart's registration.
	if _, err := b.Apply(system, cordoned); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureNodes(system); err != nil {
		t.Fatal(err)
	}
	got, err := b.Get(system, key)
	if err != nil {
		t.Fatal(err)
	}
	if !got.(*computev1alpha1.Node).GetSpec().GetUnschedulable() {
		t.Error("registering again undid the cordon")
	}

	status := &computev1alpha1.NodeStatus{Capacity: &computev1alpha1.NodeResources{Cpu: "4", Memory: "8192Mi"}}
	if _, err := b.UpdateStatus(asNode(system, "n1"), key, status, nil); err != nil {
		t.Errorf("a node reporting on itself: %v", err)
	}
	_, err = b.UpdateStatus(asNode(system, "n2"), key, status, nil)
	wantCodeOf(t, err, connect.CodePermissionDenied, "a node reporting on another")
}

// monitored starts a server whose node monitor never ticks on its own, so a
// test drives every check, and waits until the monitor has started.
func monitored(t *testing.T, nodes ...string) (*Server, context.Context, *clockwork.FakeClock) {
	t.Helper()
	clock := clockwork.NewFakeClock()
	s, system, _ := runningWith(t, func(c *Config) {
		c.Clock = clock
		c.Nodes = nodes
		c.NodeMonitorInterval = 1000 * time.Hour
	})
	for _, n := range nodes {
		waitNode(t, s, n)
	}
	deadline := time.Now().Add(10 * time.Second)
	for start, _ := s.heartbeats.since(""); start.IsZero(); start, _ = s.heartbeats.since("") {
		if time.Now().After(deadline) {
			t.Fatal("the node monitor never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return s, system, clock
}

// report writes a node's status as its agent does, with the agent's clock
// reading agentNow.
func report(t *testing.T, s *Server, system context.Context, node string, agentNow time.Time) {
	t.Helper()
	status := &computev1alpha1.NodeStatus{
		LastHeartbeatTime: timestamppb.New(agentNow),
		Capacity:          &computev1alpha1.NodeResources{Cpu: "4", Memory: "8192Mi"},
		Conditions: []*metav1.Condition{{
			Type: NodeReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "AgentReporting",
			LastTransitionTime: timestamppb.New(agentNow),
		}},
	}
	if _, err := s.backend(registry.Node).UpdateStatus(asNode(system, node), resource.Key{Name: node}, status, nil); err != nil {
		t.Fatal(err)
	}
}

func nodeReady(t *testing.T, s *Server, system context.Context, name string) *metav1.Condition {
	t.Helper()
	o, err := s.backend(registry.Node).Get(system, resource.Key{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return conditions.Find(o.(*computev1alpha1.Node).GetStatus().GetConditions(), NodeReady)
}

func checkNodesNow(t *testing.T, s *Server, system context.Context) {
	t.Helper()
	if err := s.checkNodes(system); err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatsCountFromTheMonitorStart(t *testing.T) {
	t0 := time.Unix(1000, 0)
	h := heartbeats{start: t0}
	h.record("old", t0.Add(-time.Hour))
	h.record("new", t0.Add(time.Minute))
	if got, heard := h.since("old"); !got.Equal(t0) || heard {
		t.Errorf("a report from before the start counts from %v (heard %t)", got, heard)
	}
	if got, heard := h.since("new"); !got.Equal(t0.Add(time.Minute)) || !heard {
		t.Errorf("a report after the start counts from %v (heard %t)", got, heard)
	}
	if got, heard := h.since("never"); !got.Equal(t0) || heard {
		t.Errorf("a node never heard from counts from %v (heard %t)", got, heard)
	}
}

func TestNodeMonitorMarksSilentNodes(t *testing.T) {
	s, system, clock := monitored(t, "talks", "silent")
	unknown := metav1.ConditionStatus_CONDITION_STATUS_UNKNOWN

	report(t, s, system, "talks", clock.Now())
	clock.Advance(s.cfg.NodeGracePeriod)
	checkNodesNow(t, s, system)
	if c := nodeReady(t, s, system, "talks"); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("a node within its grace period was marked %v", c)
	}
	if c := nodeReady(t, s, system, "silent"); c != nil {
		t.Errorf("a new node within its grace period was marked %v", c)
	}

	clock.Advance(time.Second)
	checkNodesNow(t, s, system)
	if c := nodeReady(t, s, system, "talks"); c.GetStatus() != unknown || c.GetReason() != "NodeStatusUnknown" {
		t.Errorf("a node past its grace period: %v", c)
	}
	if c := nodeReady(t, s, system, "silent"); c.GetStatus() != unknown || c.GetReason() != "NodeNeverReported" {
		t.Errorf("a node that never reported: %v", c)
	}

	// A node already marked is left alone: no write every check.
	before, err := s.store.Revision(system)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	checkNodesNow(t, s, system)
	if after, _ := s.store.Revision(system); after != before {
		t.Errorf("checking nodes already Unknown wrote %d times", after-before)
	}

	report(t, s, system, "talks", clock.Now())
	checkNodesNow(t, s, system)
	if c := nodeReady(t, s, system, "talks"); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("a node that reported again stayed %v", c)
	}
}

// TestNodeMonitorUsesItsOwnClock: an agent whose clock is far off is judged
// by when its reports arrive, not by the time it writes in them.
func TestNodeMonitorUsesItsOwnClock(t *testing.T) {
	s, system, clock := monitored(t, "behind", "ahead")
	for i := range 3 {
		report(t, s, system, "behind", clock.Now().Add(-time.Hour))
		report(t, s, system, "ahead", clock.Now().Add(time.Hour))
		checkNodesNow(t, s, system)
		if c := nodeReady(t, s, system, "behind"); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
			t.Fatalf("report %d: an agent with a slow clock was marked %v", i, c)
		}
		clock.Advance(10 * time.Second)
	}
	clock.Advance(s.cfg.NodeGracePeriod)
	checkNodesNow(t, s, system)
	if c := nodeReady(t, s, system, "ahead"); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_UNKNOWN {
		t.Errorf("an agent with a fast clock that went silent stayed %v", c)
	}
}

// TestNodeMonitorGivesGraceAfterARestart: a heartbeat stored long before the
// apiserver started does not count against the node; it gets a full grace
// period from the monitor's start to report again.
func TestNodeMonitorGivesGraceAfterARestart(t *testing.T) {
	s, system, clock := monitored(t, "n1")
	report(t, s, system, "n1", clock.Now())

	// The next server over the same store starts an hour later, when the
	// agent has not reported yet.
	clock.Advance(time.Hour)
	next := &Server{cfg: s.cfg, store: s.store, clock: clock, log: s.log, authz: s.authz, backends: map[string]*backend{}}
	for _, r := range registry.All {
		next.backends[r.Kind] = &backend{s: next, r: r, hooks: hooksFor(r)}
	}
	next.heartbeats.start = clock.Now()

	clock.Advance(next.cfg.NodeGracePeriod)
	checkNodesNow(t, next, system)
	if c := nodeReady(t, next, system, "n1"); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("a node was marked within the grace period after a restart: %v", c)
	}
	clock.Advance(time.Second)
	checkNodesNow(t, next, system)
	c := nodeReady(t, next, system, "n1")
	if c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_UNKNOWN {
		t.Errorf("a node silent past the grace period after a restart stayed %v", c)
	}
	// It did report, before the restart; the message says what is known.
	if c.GetReason() != "NodeStatusUnknown" || !strings.Contains(c.GetMessage(), "since the apiserver started") {
		t.Errorf("after a restart the node was described as %s: %s", c.GetReason(), c.GetMessage())
	}
}

// TestNodeMonitorNeverOverwritesANewerReport: the monitor decides on the node
// as it read it; a report that lands in between wins, capacity and all.
func TestNodeMonitorNeverOverwritesANewerReport(t *testing.T) {
	s, system, clock := monitored(t, "n1")
	stale, err := s.store.Get(system, store.Key{Kind: registry.Node.Kind, Name: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	report(t, s, system, "n1", clock.Now())
	if err := s.markIfSilent(system, stale, clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	o, err := s.backend(registry.Node).Get(system, resource.Key{Name: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	st := o.(*computev1alpha1.Node).GetStatus()
	if !conditions.IsTrue(st.GetConditions(), NodeReady) || st.GetCapacity().GetMemory() != "8192Mi" {
		t.Errorf("a decision on stale data overwrote a newer report: %v", st)
	}
}
