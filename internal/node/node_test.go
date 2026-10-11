package node_test

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// startAgent runs a node agent for env's node until the test ends; adjust
// may change its config.
func startAgent(t *testing.T, env *apiservertest.Env, host, reserved *computev1alpha1.NodeResources, adjust ...func(*node.Config)) {
	t.Helper()
	cfg := node.Config{
		Name:    apiservertest.Node,
		HTTP:    client.UnixSocket(env.Socket),
		BaseURL: client.UnixSocketURL,
		Host: func() (*computev1alpha1.NodeResources, error) {
			return proto.Clone(host).(*computev1alpha1.NodeResources), nil
		},
		Reserved:          reserved,
		HeartbeatInterval: 20 * time.Millisecond,
	}
	for _, f := range adjust {
		f(&cfg)
	}
	agent, err := node.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("agent: %v", err)
		}
	})
}

func waitFor[T proto.Message](t *testing.T, what string, get func() (T, error), ok func(T) bool) T {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, err := get()
		if err == nil && ok(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s (last: %v, %v)", what, v, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTheNodeReportsItsHost(t *testing.T) {
	env := apiservertest.Start(t, nil)
	startAgent(t, env, &computev1alpha1.NodeResources{Vcpus: 8, MemoryMib: 8192}, &computev1alpha1.NodeResources{Vcpus: 1, MemoryMib: 2048})
	root := env.As(apiservertest.Root)
	n := waitFor(t, "the node reporting ready", func() (*computev1alpha1.Node, error) {
		obj, err := root.Get(t.Context(), registry.Node, client.Key{Name: apiservertest.Node})
		if err != nil {
			return nil, err
		}
		return obj.(*computev1alpha1.Node), nil
	}, func(n *computev1alpha1.Node) bool {
		return conditions.IsTrue(n.GetStatus().GetConditions(), computeapi.NodeReady)
	})

	st := n.GetStatus()
	if !proto.Equal(st.GetCapacity(), &computev1alpha1.NodeResources{Vcpus: 8, MemoryMib: 8192}) {
		t.Errorf("capacity %v", st.GetCapacity())
	}
	if !proto.Equal(st.GetAllocatable(), &computev1alpha1.NodeResources{Vcpus: 7, MemoryMib: 6144}) {
		t.Errorf("allocatable %v, want the capacity less what the host keeps", st.GetAllocatable())
	}
	if st.GetAgentVersion() == "" || st.GetLastHeartbeatTime() == nil {
		t.Errorf("version %q, heartbeat %v", st.GetAgentVersion(), st.GetLastHeartbeatTime())
	}
	if st.GetObservedGeneration() != n.GetMetadata().GetGeneration() {
		t.Errorf("observed generation %d of %d", st.GetObservedGeneration(), n.GetMetadata().GetGeneration())
	}
	ready := conditions.Find(st.GetConditions(), computeapi.NodeReady)
	if ready.GetReason() != "AgentReporting" || ready.GetObservedGeneration() != n.GetMetadata().GetGeneration() {
		t.Errorf("Ready %v", ready)
	}
}

func getNode(t *testing.T, root *client.Client) func() (*computev1alpha1.Node, error) {
	return func() (*computev1alpha1.Node, error) {
		obj, err := root.Get(t.Context(), registry.Node, client.Key{Name: apiservertest.Node})
		if err != nil {
			return nil, err
		}
		return obj.(*computev1alpha1.Node), nil
	}
}

// TestTheAgentKeepsReporting: the agent reports at every beat of its
// default interval, timed by its clock.
func TestTheAgentKeepsReporting(t *testing.T) {
	env := apiservertest.Start(t, nil)
	root := env.As(apiservertest.Root)
	// The apiserver creates the Node in the background; a first report
	// before that would fail and wait for the next beat.
	waitFor(t, "the Node created", getNode(t, root), func(*computev1alpha1.Node) bool { return true })
	clock := clockwork.NewFakeClockAt(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	startAgent(t, env, &computev1alpha1.NodeResources{Vcpus: 1, MemoryMib: 1024}, nil, func(c *node.Config) {
		c.HeartbeatInterval = 0
		c.Clock = clock
	})
	for beat := range 3 {
		want := clock.Now()
		waitFor(t, fmt.Sprintf("report %d", beat), getNode(t, root), func(n *computev1alpha1.Node) bool {
			return n.GetStatus().GetLastHeartbeatTime().AsTime().Equal(want)
		})
		if err := clock.BlockUntilContext(t.Context(), 1); err != nil { // the agent waits for its next beat
			t.Fatal(err)
		}
		clock.Advance(10 * time.Second)
	}
}

// TestALiveAgentWinsOverTheMonitor: a report is written whatever the
// apiserver's node monitor wrote since the agent read the Node, so a node
// marked Unknown comes back as soon as its agent reports.
func TestALiveAgentWinsOverTheMonitor(t *testing.T) {
	env := apiservertest.Start(t, nil)
	root := env.As(apiservertest.Root)
	waitFor(t, "the Node created", getNode(t, root), func(*computev1alpha1.Node) bool { return true })
	// The node is marked Unknown in the store, as the apiserver's monitor
	// does after a silence: before the agent starts, and between each of its
	// reads and writes.
	key := store.Key{Kind: registry.Node.Kind, Name: apiservertest.Node}
	markUnknown := func() {
		ctx := context.Background() // the agent's request may end first
		obj, err := env.Store.Get(ctx, key)
		if err != nil {
			t.Errorf("reading the node: %v", err)
			return
		}
		var n computev1alpha1.Node
		if err := proto.Unmarshal(obj.Data, &n); err != nil {
			t.Error(err)
			return
		}
		if n.Status == nil {
			n.Status = &computev1alpha1.NodeStatus{}
		}
		n.Status.Conditions = conditions.Set(n.Status.Conditions, &metav1.Condition{
			Type: computeapi.NodeReady, Status: metav1.ConditionStatus_CONDITION_STATUS_UNKNOWN, Reason: "NodeStatusUnknown",
		}, time.Now())
		data, err := proto.Marshal(&n)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := env.Store.Update(ctx, key, data, obj.Revision); err != nil {
			t.Errorf("marking the node Unknown: %v", err)
		}
	}
	markUnknown()
	racing := &http.Client{Transport: afterGetNode{next: client.UnixSocket(env.Socket).Transport, then: markUnknown}}
	startAgent(t, env, &computev1alpha1.NodeResources{Vcpus: 1, MemoryMib: 1024}, nil, func(c *node.Config) { c.HTTP = racing })
	waitFor(t, "the node ready again", getNode(t, root), func(n *computev1alpha1.Node) bool {
		return conditions.IsTrue(n.GetStatus().GetConditions(), computeapi.NodeReady)
	})
}

// afterGetNode calls then after each successful GetNode the agent makes,
// before the agent sees the answer.
type afterGetNode struct {
	next http.RoundTripper
	then func()
}

func (a afterGetNode) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := a.next.RoundTrip(r)
	if err == nil && res.StatusCode == http.StatusOK && strings.HasSuffix(r.URL.Path, "/GetNode") {
		a.then()
	}
	return res, err
}

func TestAllocatableIsNeverNegative(t *testing.T) {
	got := node.Allocatable(&computev1alpha1.NodeResources{Vcpus: 2, MemoryMib: 1024}, &computev1alpha1.NodeResources{Vcpus: 4, MemoryMib: 2048})
	if got.GetVcpus() != 0 || got.GetMemoryMib() != 0 {
		t.Errorf("allocatable %v when the host keeps more than it has", got)
	}
}

func TestVersion(t *testing.T) {
	for _, tt := range []struct {
		settings []debug.BuildSetting
		want     string
	}{
		{[]debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "false"}}, "abc123"},
		{[]debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "true"}}, "abc123-dirty"},
		{nil, "v1.2.3"},
	} {
		info := &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}, Settings: tt.settings}
		if got := node.VersionOf(info); got != tt.want {
			t.Errorf("version of %v: %q, want %q", tt.settings, got, tt.want)
		}
	}
}

// TestMachinesArePlacedOnAReportingNode: the scheduler places machines on a
// node the agent reports, within what it says is allocatable.
func TestMachinesArePlacedOnAReportingNode(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	// Room for one 512 MiB container, not two.
	startAgent(t, env, &computev1alpha1.NodeResources{Vcpus: 2, MemoryMib: 1024}, &computev1alpha1.NodeResources{MemoryMib: 400})
	root := env.As(apiservertest.Root)
	for _, name := range []string{"a", "b"} {
		if _, err := root.Apply(t.Context(), registry.Container, &computev1alpha1.Container{
			Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: name},
			Spec: &computev1alpha1.ContainerSpec{
				Image: "nginx", Resources: &computev1alpha1.Resources{Vcpus: 1, MemoryMib: 512},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	machine := func(name string) func() (*computev1alpha1.Machine, error) {
		return func() (*computev1alpha1.Machine, error) {
			obj, err := root.Get(t.Context(), registry.Machine, client.Key{Project: apiservertest.Project, Name: name})
			if err != nil {
				return nil, err
			}
			return obj.(*computev1alpha1.Machine), nil
		}
	}
	placed := func(get func() (*computev1alpha1.Machine, error)) bool {
		m, err := get()
		return err == nil && m.GetSpec().GetNode() == apiservertest.Node
	}
	deadline := time.Now().Add(10 * time.Second)
	for !placed(machine("ct-a")) && !placed(machine("ct-b")) {
		if time.Now().After(deadline) {
			t.Fatal("neither machine was placed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if placed(machine("ct-a")) && placed(machine("ct-b")) {
		t.Error("two 512 MiB machines placed where the node has 624 MiB allocatable")
	}
}

func TestMemTotal(t *testing.T) {
	got, err := node.MemTotalMiB([]byte("MemTotal:       65756148 kB\nMemFree:         1234 kB\n"))
	if err != nil || got != 64214 {
		t.Errorf("MemTotal: %d, %v", got, err)
	}
	if _, err := node.MemTotalMiB([]byte("MemFree: 1 kB\n")); err == nil {
		t.Error("no MemTotal read as something")
	}
}
