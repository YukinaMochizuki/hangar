package node_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
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
	"github.com/YukinaMochizuki/hangar/internal/testutil"
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

// waitValue waits until get returns a value that satisfies ok, and returns it.
func waitValue[T proto.Message](t *testing.T, what string, get func() (T, error), ok func(T) bool) T {
	t.Helper()
	var v T
	testutil.Eventually(t, what, func() (bool, string) {
		var err error
		v, err = get()
		return err == nil && ok(v), fmt.Sprint(v, err)
	})
	return v
}

func TestTheNodeReportsItsHost(t *testing.T) {
	env := apiservertest.Start(t, nil)
	startAgent(t, env, &computev1alpha1.NodeResources{Cpu: "8", Memory: "8Gi"}, &computev1alpha1.NodeResources{Cpu: "1", Memory: "2G"})
	root := env.As(apiservertest.Root)
	n := waitValue(t, "the node reporting ready", func() (*computev1alpha1.Node, error) {
		obj, err := root.Get(t.Context(), registry.Node, client.Key{Name: apiservertest.Node})
		if err != nil {
			return nil, err
		}
		return obj.(*computev1alpha1.Node), nil
	}, func(n *computev1alpha1.Node) bool {
		return conditions.IsTrue(n.GetStatus().GetConditions(), computeapi.NodeReady)
	})

	st := n.GetStatus()
	if !proto.Equal(st.GetCapacity(), &computev1alpha1.NodeResources{Cpu: "8", Memory: "8Gi"}) {
		t.Errorf("capacity %v", st.GetCapacity())
	}
	if !proto.Equal(st.GetAllocatable(), &computev1alpha1.NodeResources{Cpu: "7", Memory: "6284Mi"}) {
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

// syncBuffer is a log the agent writes while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) count(s string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), s)
}

// TestTheNodeReportsItsPlacement: the node describes its host by what the
// agent reads of its CPU, disks, memory and network and what its
// configuration says, less a configured value a Node may not report. The
// CPU is read at each heartbeat, a read that fails keeping what it last
// read and being logged when it starts failing and when it works again;
// storage, memory and network are read once, as the agent starts.
func TestTheNodeReportsItsPlacement(t *testing.T) {
	env := apiservertest.Start(t, nil)
	root := env.As(apiservertest.Root)
	var cpuFails atomic.Bool
	var disks, network atomic.Value
	var diskReads, memoryReads, networkReads atomic.Int32
	disks.Store("2 × NVMe SSD")
	network.Store("2 × 10 GbE")
	logs := &syncBuffer{}
	startAgent(t, env, &computev1alpha1.NodeResources{Cpu: "8", Memory: "8Gi"}, nil, func(c *node.Config) {
		c.Log = slog.New(slog.NewTextHandler(logs, nil))
		c.Placement = &computev1alpha1.Placement{Region: "ap-southeast", Site: "SGP02", Chassis: "Generic 1U"}
		c.CPU = func() (*computev1alpha1.Placement, error) {
			if cpuFails.Load() {
				return nil, errors.New("no /proc/cpuinfo")
			}
			return &computev1alpha1.Placement{CpuModel: "AMD EPYC 7313", CpuGeneration: "Zen 3 (Milan)"}, nil
		}
		c.Disks = func() (string, error) {
			diskReads.Add(1)
			return disks.Load().(string), nil
		}
		c.Memory = func() (string, error) {
			memoryReads.Add(1)
			return "DDR4-3200 ECC", nil
		}
		c.Network = func() (string, error) {
			networkReads.Add(1)
			return network.Load().(string), nil
		}
	})
	want := &computev1alpha1.Placement{
		Region: "ap-southeast", Site: "SGP02", CpuModel: "AMD EPYC 7313", CpuGeneration: "Zen 3 (Milan)",
		Storage: "2 × NVMe SSD", Memory: "DDR4-3200 ECC", Network: "2 × 10 GbE",
	}
	n := waitValue(t, "the node describing its host, without the chassis that is no label", getNode(t, root), func(n *computev1alpha1.Node) bool {
		return proto.Equal(n.GetStatus().GetPlacement(), want)
	})
	if got := logs.count("reported without chassis"); got != 1 {
		t.Errorf("the dropped chassis was logged %d times, want once", got)
	}

	// The CPU's read fails, and the disks and network the host would show
	// now, a disk off the bus and a port gone, are never asked.
	cpuFails.Store(true)
	disks.Store("1 × NVMe SSD")
	network.Store("1 × 10 GbE")
	n = waitBeats(t, root, n, 3)
	if got := n.GetStatus().GetPlacement(); !proto.Equal(got, want) {
		t.Errorf("while the CPU's read fails the node reports placement %v, want %v", got, want)
	}
	if got := logs.count("reading the host's CPU failed"); got != 1 {
		t.Errorf("the CPU read failing for several heartbeats was logged %d times, want once", got)
	}
	if d, m, nw := diskReads.Load(), memoryReads.Load(), networkReads.Load(); d != 1 || m != 1 || nw != 1 {
		t.Errorf("over several heartbeats the disks were read %d times, memory %d and network %d, want once each", d, m, nw)
	}

	cpuFails.Store(false)
	waitBeats(t, root, n, 2)
	if got := logs.count("reading the host's CPU works again"); got != 1 {
		t.Errorf("the read working again was logged %d times, want once", got)
	}
}

// TestAnUnreadableHostIsDescribedAsConfigured: storage, memory and network
// that cannot be read are logged once, and are what the configuration says,
// or empty; what a read gives with an error for the rest is reported, the
// rest logged once.
func TestAnUnreadableHostIsDescribedAsConfigured(t *testing.T) {
	env := apiservertest.Start(t, nil)
	root := env.As(apiservertest.Root)
	logs := &syncBuffer{}
	startAgent(t, env, &computev1alpha1.NodeResources{Cpu: "8", Memory: "8Gi"}, nil, func(c *node.Config) {
		c.Log = slog.New(slog.NewTextHandler(logs, nil))
		c.Placement = &computev1alpha1.Placement{Memory: "DDR4-3200 ECC"}
		c.CPU = func() (*computev1alpha1.Placement, error) {
			return &computev1alpha1.Placement{CpuModel: "AMD EPYC 7313"}, nil
		}
		c.Disks = func() (string, error) { return "", errors.New("no /sys/block") }
		c.Memory = func() (string, error) { return "", errors.New("no SMBIOS table") }
		c.Network = func() (string, error) {
			return "1 × 10 GbE", errors.New("eno2: no supported link mode with a speed")
		}
	})
	n := waitValue(t, "the node reporting", getNode(t, root), func(n *computev1alpha1.Node) bool {
		return n.GetStatus().GetPlacement().GetCpuModel() != ""
	})
	n = waitBeats(t, root, n, 3)
	want := &computev1alpha1.Placement{CpuModel: "AMD EPYC 7313", Memory: "DDR4-3200 ECC", Network: "1 × 10 GbE"}
	if got := n.GetStatus().GetPlacement(); !proto.Equal(got, want) {
		t.Errorf("placement %v, want %v", got, want)
	}
	for _, log := range []string{"reading the host's disks failed", "reading the host's memory failed", "reading the host's network left some of it out"} {
		if got := logs.count(log); got != 1 {
			t.Errorf("%q was logged %d times, want once", log, got)
		}
	}
}

// waitBeats waits for the node to report beats more times since n.
func waitBeats(t *testing.T, root *client.Client, n *computev1alpha1.Node, beats int) *computev1alpha1.Node {
	t.Helper()
	for i := range beats {
		beat := n.GetStatus().GetLastHeartbeatTime().AsTime()
		n = waitValue(t, fmt.Sprintf("report %d after %v", i+1, beat), getNode(t, root), func(n *computev1alpha1.Node) bool {
			return n.GetStatus().GetLastHeartbeatTime().AsTime().After(beat)
		})
	}
	return n
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
	waitValue(t, "the Node created", getNode(t, root), func(*computev1alpha1.Node) bool { return true })
	clock := clockwork.NewFakeClockAt(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	startAgent(t, env, &computev1alpha1.NodeResources{Cpu: "1", Memory: "1Gi"}, nil, func(c *node.Config) {
		c.HeartbeatInterval = 0
		c.Clock = clock
	})
	for beat := range 3 {
		want := clock.Now()
		waitValue(t, fmt.Sprintf("report %d", beat), getNode(t, root), func(n *computev1alpha1.Node) bool {
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
	waitValue(t, "the Node created", getNode(t, root), func(*computev1alpha1.Node) bool { return true })
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
	startAgent(t, env, &computev1alpha1.NodeResources{Cpu: "1", Memory: "1Gi"}, nil, func(c *node.Config) { c.HTTP = racing })
	waitValue(t, "the node ready again", getNode(t, root), func(n *computev1alpha1.Node) bool {
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

func TestAllocatable(t *testing.T) {
	type res = computev1alpha1.NodeResources
	tests := []struct {
		name               string
		capacity, reserved *res
		want               *res
	}{
		{"nothing kept", &res{Cpu: "4", Memory: "4Gi", Disk: "100Gi"}, &res{}, &res{Cpu: "4", Memory: "4096Mi", Disk: "100Gi"}},
		{"never negative", &res{Cpu: "2", Memory: "1Gi", Disk: "1Gi"}, &res{Cpu: "4", Memory: "2Gi", Disk: "2Gi"}, &res{Cpu: "0", Memory: "0Mi", Disk: "0Gi"}},
		{"exactly all kept", &res{Cpu: "2", Memory: "1Gi"}, &res{Cpu: "2000m", Memory: "1024Mi"}, &res{Cpu: "0", Memory: "0Mi"}},
		{"rounded down", &res{Cpu: "8", Memory: "8Gi", Disk: "100Gi"}, &res{Cpu: "1", Memory: "2G", Disk: "1"}, &res{Cpu: "7", Memory: "6284Mi", Disk: "99Gi"}},
		{"what the host does not report stays empty", &res{Cpu: "8", Memory: "8Gi"}, &res{Disk: "10Gi"}, &res{Cpu: "8", Memory: "8192Mi"}},
	}
	for _, tt := range tests {
		got, err := node.Allocatable(tt.capacity, tt.reserved)
		if err != nil || !proto.Equal(got, tt.want) {
			t.Errorf("%s: allocatable %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}
	for _, bad := range []*res{{Cpu: "1.5"}, {Cpu: "-1"}, {Cpu: "x"}, {Memory: "1.5"}, {Disk: "-1Gi"}} {
		if _, err := node.Allocatable(&res{Cpu: "8", Memory: "8Gi", Disk: "8Gi"}, bad); err == nil {
			t.Errorf("reserved %v was accepted", bad)
		}
		if _, err := node.Allocatable(bad, &res{}); err == nil {
			t.Errorf("capacity %v was accepted", bad)
		}
		if _, err := node.New(node.Config{Name: "n1", Reserved: bad}); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("an agent keeping %v back: %v", bad, err)
		}
	}
	for _, cfg := range []node.Config{{Name: "n1", MaxPulls: -1}, {Name: "n1", MaxUnpackWork: -1}} {
		if _, err := node.New(cfg); err == nil {
			t.Errorf("an agent with %d pulls and %d steps of work", cfg.MaxPulls, cfg.MaxUnpackWork)
		}
	}
}

func TestVMSize(t *testing.T) {
	tests := []struct {
		res       *computev1alpha1.Resources
		vcpus     int32
		memoryMiB int64
	}{
		{&computev1alpha1.Resources{Cpu: "2", Memory: "512Mi", Disk: "10Gi"}, 2, 512},
		{&computev1alpha1.Resources{Cpu: "1", Memory: "512M"}, 1, 489},
		{&computev1alpha1.Resources{Cpu: "1", Memory: "1Mi"}, 1, 1},
	}
	for _, tt := range tests {
		vcpus, mib, err := node.VMSize(tt.res)
		if err != nil || vcpus != tt.vcpus || mib != tt.memoryMiB {
			t.Errorf("VMSize(%v) = %d, %d, %v; want %d, %d", tt.res, vcpus, mib, err, tt.vcpus, tt.memoryMiB)
		}
	}
	for _, bad := range []*computev1alpha1.Resources{{Memory: "512Mi"}, {Cpu: "500m", Memory: "512Mi"}, {Cpu: "1"}, {Cpu: "1", Memory: "-1Mi"}} {
		if _, _, err := node.VMSize(bad); err == nil {
			t.Errorf("VMSize(%v) was accepted", bad)
		}
	}
}

// TestMachinesArePlacedOnAReportingNode: the scheduler places machines on a
// node the agent reports, within what it says is allocatable.
func TestMachinesArePlacedOnAReportingNode(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	// Room for one 512 MiB container, not two.
	startAgent(t, env, &computev1alpha1.NodeResources{Cpu: "2", Memory: "1Gi"}, &computev1alpha1.NodeResources{Memory: "400Mi"})
	root := env.As(apiservertest.Root)
	for _, name := range []string{"a", "b"} {
		if _, err := root.Apply(t.Context(), registry.Container, &computev1alpha1.Container{
			Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: name},
			Spec: &computev1alpha1.ContainerSpec{
				Image: "nginx", Resources: &computev1alpha1.Resources{Cpu: "1", Memory: "512Mi"},
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
	var other string
	testutil.Eventually(t, "one machine placed", func() bool {
		switch {
		case placed(machine("ct-a")):
			other = "ct-b"
		case placed(machine("ct-b")):
			other = "ct-a"
		default:
			return false
		}
		return true
	})
	// The other machine may not exist yet; until it does, it reads as not
	// placed.
	testutil.Unchanged(t, "the node of the other machine, where the node has 624 MiB allocatable", 200*time.Millisecond, func() (string, error) {
		m, err := machine(other)()
		if connect.CodeOf(err) == connect.CodeNotFound {
			return "", nil
		}
		return m.GetSpec().GetNode(), err
	})
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
