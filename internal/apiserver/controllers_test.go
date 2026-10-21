package apiserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// controlled starts a server with its controllers and the given nodes.
func controlled(t *testing.T, nodes ...string) (*Server, context.Context, context.Context) {
	t.Helper()
	s, system, user := runningWith(t, func(c *Config) {
		c.DisableControllers = false
		c.Nodes = nodes
		c.NodeMonitorInterval = 1000 * time.Hour
	})
	for _, n := range nodes {
		waitNode(t, s, n)
	}
	return s, system, user
}

// nodeWithRoom reports a node ready with memMiB of memory.
func nodeWithRoom(t *testing.T, s *Server, system context.Context, node string, memMiB int64) {
	t.Helper()
	status := &computev1alpha1.NodeStatus{
		Capacity: &computev1alpha1.NodeResources{Cpu: "8", Memory: fmt.Sprintf("%dMi", memMiB)},
		Conditions: []*metav1.Condition{{
			Type: NodeReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "AgentReporting",
		}},
	}
	if _, err := s.backend(registry.Node).UpdateStatus(asNode(system, node), resource.Key{Name: node}, status, nil); err != nil {
		t.Fatal(err)
	}
}

func applyCtr(t *testing.T, s *Server, user context.Context, c *computev1alpha1.Container) {
	t.Helper()
	if _, err := s.backend(registry.Container).Apply(user, c); err != nil {
		t.Fatal(err)
	}
}

func sized(name string, memMiB int64) *computev1alpha1.Container {
	c := ctr(name)
	c.Spec.Resources = &computev1alpha1.Resources{Memory: fmt.Sprintf("%dMi", memMiB)}
	return c
}

// waitObject waits until the object of kind r at key exists and satisfies
// ok, and returns it.
func waitObject[T proto.Message](t *testing.T, s *Server, system context.Context, r registry.Resource, key resource.Key, what string, ok func(T) bool) T {
	t.Helper()
	var last T
	testutil.Eventually(t, what, func() (bool, string) {
		obj, err := s.get(system, r, key)
		if err != nil {
			t.Fatal(err)
		}
		if obj == nil {
			return false, "none"
		}
		last = obj.(T)
		return ok(last), fmt.Sprint(last)
	})
	return last
}

// versionOf reads the resourceVersion of the object of kind r at key, for
// testutil.Unchanged, which then fails if the object is gone.
func versionOf(s *Server, system context.Context, r registry.Resource, key resource.Key) func() (string, error) {
	return func() (string, error) {
		obj, err := s.get(system, r, key)
		if err != nil {
			return "", err
		}
		if obj == nil {
			return "", errors.New("gone")
		}
		return fmt.Sprint(obj.(interface{ GetMetadata() *metav1.ObjectMeta }).GetMetadata().GetResourceVersion()), nil
	}
}

// waitMachine waits until the machine exists and satisfies ok.
func waitMachine(t *testing.T, s *Server, system context.Context, name string, what string, ok func(*computev1alpha1.Machine) bool) *computev1alpha1.Machine {
	t.Helper()
	return waitObject(t, s, system, registry.Machine, resource.Key{Project: "web", Name: name}, "machine "+name+" "+what, ok)
}

func waitContainer(t *testing.T, s *Server, system context.Context, name, what string, ok func(*computev1alpha1.Container) bool) *computev1alpha1.Container {
	t.Helper()
	return waitObject(t, s, system, registry.Container, resource.Key{Project: "web", Name: name}, "container "+name+" "+what, ok)
}

func waitGone(t *testing.T, s *Server, system context.Context, r registry.Resource, name string) {
	t.Helper()
	testutil.Eventually(t, r.Kind+" "+name+" gone", func() (bool, string) {
		obj, err := s.get(system, r, resource.Key{Project: "web", Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return obj == nil, fmt.Sprint(obj)
	})
}

// reportMachine writes a machine's status as its node does, keeping the
// scheduler's condition.
func reportMachine(t *testing.T, s *Server, system context.Context, m *computev1alpha1.Machine, phase computev1alpha1.MachinePhase, observed int64, conds ...*metav1.Condition) {
	t.Helper()
	status := &computev1alpha1.MachineStatus{Phase: phase, ObservedGeneration: observed}
	if c := conditions.Find(m.GetStatus().GetConditions(), MachineScheduled); c != nil {
		status.Conditions = append(status.Conditions, c)
	}
	status.Conditions = append(status.Conditions, conds...)
	key := resource.Key{Project: "web", Name: m.GetMetadata().GetName()}
	if _, err := s.backend(registry.Machine).UpdateStatus(asNode(system, m.GetSpec().GetNode()), key, status, nil); err != nil {
		t.Fatal(err)
	}
}

func scheduled(m *computev1alpha1.Machine) *metav1.Condition {
	return conditions.Find(m.GetStatus().GetConditions(), MachineScheduled)
}

func containerReady(c *computev1alpha1.Container) *metav1.Condition {
	return conditions.Find(c.GetStatus().GetConditions(), ContainerReady)
}

func TestAContainerGetsAMachine(t *testing.T) {
	s, system, user := controlled(t)
	applyCtr(t, s, user, ctr("api"))
	c := waitContainer(t, s, system, "api", "created", func(*computev1alpha1.Container) bool { return true })

	m := waitMachine(t, s, system, "ct-api", "reported unschedulable", func(m *computev1alpha1.Machine) bool {
		return scheduled(m).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE
	})
	if !ownedBy(m.GetMetadata(), c.GetMetadata().GetUid()) {
		t.Errorf("the machine is not controlled by its container: %v", m.GetMetadata().GetOwnerReferences())
	}
	if m.GetMetadata().GetFinalizers()[0] != NodeCleanupFinalizer {
		t.Errorf("finalizers %v", m.GetMetadata().GetFinalizers())
	}
	want := &computev1alpha1.MachineSpec{
		Resources:     &computev1alpha1.Resources{Cpu: defaultCPU, Memory: defaultMemory, Disk: defaultDisk},
		RunState:      computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING,
		RestartPolicy: computev1alpha1.RestartPolicy_RESTART_POLICY_ALWAYS,
		Boot:          &computev1alpha1.MachineSpec_Container{Container: &computev1alpha1.ContainerBoot{Image: "nginx"}},
		Interfaces:    []*computev1alpha1.MachineInterface{{Name: "eth0", Network: computeapi.DefaultNetwork, Ipv4: "10.0.0.2/24", SecurityGroups: []string{"default"}}},
	}
	if !proto.Equal(m.GetSpec(), want) {
		t.Errorf("machine spec\n got %v\nwant %v", m.GetSpec(), want)
	}
	if sc := scheduled(m); sc.GetReason() != "NoNodeAvailable" || sc.GetMessage() != "no nodes are registered" {
		t.Errorf("Scheduled = %v", sc)
	}
	if m.GetStatus().GetPhase() != computev1alpha1.MachinePhase_MACHINE_PHASE_PENDING {
		t.Errorf("phase %v", m.GetStatus().GetPhase())
	}
	waitContainer(t, s, system, "api", "reported unschedulable", func(c *computev1alpha1.Container) bool {
		r := containerReady(c)
		return r.GetReason() == "Unschedulable" && r.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE &&
			c.GetStatus().GetObservedGeneration() == 1
	})
}

func TestSchedulerPicksAReadyNodeWithRoom(t *testing.T) {
	s, system, user := controlled(t, "big", "small", "silent")

	applyCtr(t, s, user, ctr("first"))
	waitMachine(t, s, system, "ct-first", "told no node is ready", func(m *computev1alpha1.Machine) bool {
		return scheduled(m).GetMessage() == "no node is ready"
	})

	// The smaller node is ready first, so the machine already waiting goes
	// there; the next one goes where there is most room.
	nodeWithRoom(t, s, system, "small", 1024)
	waitMachine(t, s, system, "ct-first", "placed on the only ready node", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "small" && scheduled(m).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE
	})
	nodeWithRoom(t, s, system, "big", 4096)
	applyCtr(t, s, user, ctr("second"))
	waitMachine(t, s, system, "ct-second", "placed on the node with most room", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "big"
	})

	// big has 3584 MiB left, small 1024: 3800 fits nowhere.
	applyCtr(t, s, user, sized("huge", 3800))
	waitMachine(t, s, system, "ct-huge", "refused for lack of memory", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "" && scheduled(m).GetMessage() == "no ready node has 3800Mi of memory free"
	})

	// A cordoned node takes nothing new.
	if _, err := s.backend(registry.Node).Apply(system, &computev1alpha1.Node{
		Metadata: &metav1.ObjectMeta{Name: "big"}, Spec: &computev1alpha1.NodeSpec{Unschedulable: true},
	}); err != nil {
		t.Fatal(err)
	}
	applyCtr(t, s, user, ctr("third"))
	waitMachine(t, s, system, "ct-third", "placed on the uncordoned node", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "small"
	})
	if m := waitMachine(t, s, system, "ct-huge", "still waiting", func(*computev1alpha1.Machine) bool { return true }); m.GetSpec().GetNode() != "" {
		t.Errorf("the huge machine was placed on %s", m.GetSpec().GetNode())
	}
}

func TestAContainerFollowsItsMachine(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyCtr(t, s, user, ctr("api"))
	m := waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })

	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration())
	waitContainer(t, s, system, "api", "ready", func(c *computev1alpha1.Container) bool {
		return conditions.IsTrue(c.GetStatus().GetConditions(), ContainerReady) && containerReady(c).GetObservedGeneration() == 1
	})

	// A new image is a new machine generation on the same node.
	before := m.GetMetadata().GetGeneration()
	changed := ctr("api")
	changed.Spec.Image = "nginx:2"
	applyCtr(t, s, user, changed)
	m = waitMachine(t, s, system, "ct-api", "updated", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetContainer().GetImage() == "nginx:2"
	})
	if m.GetSpec().GetNode() != "n1" || m.GetMetadata().GetGeneration() != before+1 {
		t.Errorf("after the change: node %q generation %d, was %d", m.GetSpec().GetNode(), m.GetMetadata().GetGeneration(), before)
	}
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, before)
	waitContainer(t, s, system, "api", "waiting for the node", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == "Updating" && c.GetStatus().GetObservedGeneration() == 2
	})
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, before+1)
	waitContainer(t, s, system, "api", "ready again", func(c *computev1alpha1.Container) bool {
		return conditions.IsTrue(c.GetStatus().GetConditions(), ContainerReady) && containerReady(c).GetObservedGeneration() == 2
	})

	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_FAILED, before+1)
	waitContainer(t, s, system, "api", "reported failed", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == "Failed"
	})
}

// TestASettledSystemStaysQuiet: once everything matches, the controllers
// write nothing more.
func TestASettledSystemStaysQuiet(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyCtr(t, s, user, ctr("api"))
	applyCtr(t, s, user, sized("huge", 1<<20))
	m := waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration())
	api := waitContainer(t, s, system, "api", "ready", func(c *computev1alpha1.Container) bool {
		return conditions.IsTrue(c.GetStatus().GetConditions(), ContainerReady)
	})
	huge := waitContainer(t, s, system, "huge", "reported unschedulable", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == "Unschedulable"
	})

	// A write loop never settles; a write after the match shows below.
	waitSettled(t, s)
	for _, c := range []*computev1alpha1.Container{api, huge} {
		key := resource.Key{Project: "web", Name: c.GetMetadata().GetName()}
		if now, _ := versionOf(s, system, registry.Container, key)(); now != fmt.Sprint(c.GetMetadata().GetResourceVersion()) {
			t.Errorf("container %s was written again once it matched: at %d, then at %s", key.Name, c.GetMetadata().GetResourceVersion(), now)
		}
	}
}

func TestDeletingAContainerWaitsForItsNode(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeWithRoom(t, s, system, "n1", 4096)
	applyCtr(t, s, user, ctr("api"))
	waitMachine(t, s, system, "ct-api", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })

	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "api"}, nil); err != nil {
		t.Fatal(err)
	}
	m := waitMachine(t, s, system, "ct-api", "marked for deletion", func(m *computev1alpha1.Machine) bool {
		return m.GetMetadata().GetDeleteTime() != nil
	})
	testutil.Unchanged(t, "container api, before its node released the machine", 100*time.Millisecond,
		versionOf(s, system, registry.Container, resource.Key{Project: "web", Name: "api"}))

	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration(),
		&metav1.Condition{Type: MachineReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned"})
	waitGone(t, s, system, registry.Machine, "ct-api")
	waitGone(t, s, system, registry.Container, "api")

	// A machine no node ever took has nothing to release.
	applyCtr(t, s, user, sized("huge", 1<<20))
	waitMachine(t, s, system, "ct-huge", "refused", func(m *computev1alpha1.Machine) bool { return scheduled(m) != nil })
	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "huge"}, nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.Machine, "ct-huge")
	waitGone(t, s, system, registry.Container, "huge")
}

func TestControllersCatchUpOnStart(t *testing.T) {
	s, system, user := running(t)
	applyCtr(t, s, user, ctr("api"))

	// A server with controllers, over the same store, finds the container
	// that was written while none were running.
	cfg := s.cfg
	cfg.DisableControllers = false
	next := New(cfg)
	ctx, cancel := context.WithCancel(system)
	done := make(chan error, 1)
	go func() { done <- next.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitMachine(t, s, system, "ct-api", "created", func(*computev1alpha1.Machine) bool { return true })
}

func TestMachineNamesStayValid(t *testing.T) {
	long := strings.Repeat("a", 63)
	other := strings.Repeat("a", 62) + "b"
	got, gotOther := machineName(long), machineName(other)
	if len(got) > 63 || ValidName(got) != nil {
		t.Errorf("machine name %q is not a valid name", got)
	}
	if got == gotOther {
		t.Errorf("two containers share machine name %q", got)
	}
	if machineName(long) != got {
		t.Error("machine names are not stable")
	}
	if machineName("api") != "ct-api" {
		t.Errorf("short name became %q", machineName("api"))
	}
}
