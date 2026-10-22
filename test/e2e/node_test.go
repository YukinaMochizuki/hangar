// Package e2e holds the hermetic full-stack tests (L2 in
// docs/decisions/testing-strategy.md): a real apiserver and the real node
// agent, with a simulated host under the agent.
package e2e

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/controller/controllertest"
	"github.com/YukinaMochizuki/hangar/internal/node"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
	"github.com/YukinaMochizuki/hangar/internal/node/metadata"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

const project = apiservertest.Project

var nginx = driver.ImageConfig{Entrypoint: []string{"nginx"}, Cmd: []string{"-g", "daemon off;"}, Env: []string{"PATH=/usr/bin"}}

// world is an apiserver, a simulated host, and the node agent between them.
type world struct {
	t      *testing.T
	env    *apiservertest.Env
	host   *fake.Host
	root   *client.Client
	system *client.Client
	// wrap, if set before the agent starts, wraps its transport to the
	// apiserver.
	wrap func(http.RoundTripper) http.RoundTripper
	// pullTimeout, if set before the agent starts, bounds preparing an
	// image.
	pullTimeout time.Duration
	// dnsUpstreams, if set before the agent starts, are what the networks'
	// resolvers forward to.
	dnsUpstreams []netip.AddrPort
	// metadata is the agent's metadata service, which a test asks as a
	// guest would, from the guest's address (see askMetadata).
	metadata *metadata.Server
	// ipv6Prefix, if set before the agent starts, is the host's.
	ipv6Prefix netip.Prefix
	// sharedIPv4, if set before the agent starts, is the host's.
	sharedIPv4 netip.Addr
	// edgeDial, if set before the agent starts, is where it takes
	// hangar-edge's dials, from this process.
	edgeDial net.Listener
	// imageCacheHigh and imageCacheLow, if set before the agent starts,
	// bound the images no machine uses.
	imageCacheHigh, imageCacheLow int64
	// sweepInterval, if set before the agent starts, is how often it
	// sweeps; 100ms if not.
	sweepInterval time.Duration

	stop func()
	// agent is the running node agent; nil while it is stopped.
	agent *node.Agent
}

// sweeps waits until the running agent has finished n sweeps of the host
// that began after the call: one more than n, as one may be under way.
func (w *world) sweeps(n int) {
	w.t.Helper()
	from := w.agent.Activity().Sweeps
	testutil.Eventually(w.t, fmt.Sprint(n, " sweeps"), func() (bool, string) {
		done := w.agent.Activity().Sweeps - from
		return done > int64(n), fmt.Sprint(done, " finished")
	})
}

// reconciled waits until the running agent's controller has finished n
// reconciles of key that began after the call, for a key whose every
// reconcile fails or waits, which WaitSettled would wait on for good. Its
// retry counts them; one more than n is waited for, as one may be under
// way.
func (w *world) reconciled(ctrl string, key controller.Key, n int) {
	w.t.Helper()
	attempts := func() int {
		for _, s := range w.agent.ControllerStatus() {
			if s.Name != ctrl {
				continue
			}
			for _, r := range s.Retries {
				if r.Key == key {
					return r.Attempts
				}
			}
		}
		return 0
	}
	want := attempts() + n + 1
	testutil.Eventually(w.t, fmt.Sprintf("%s reconciling %s %d more times", ctrl, key, n), func() (bool, string) {
		got := attempts()
		return got >= want, fmt.Sprintf("%d attempts in a row, want %d", got, want)
	})
}

// networkChangeTaken waits until the running agent has taken in a network
// change signalled after from, its count of them at the time.
func (w *world) networkChangeTaken(from int64) {
	w.t.Helper()
	testutil.Eventually(w.t, "the network change taken in", func() bool { return w.agent.Activity().NetworkChanges > from })
}

// packetsTaken waits until the network's service of the kind has answered,
// or dropped, every packet sent to it.
func (w *world) packetsTaken(networkUID string, kind driver.PacketKind) {
	w.t.Helper()
	testutil.Eventually(w.t, "the packets sent taken", func() bool { return w.host.PacketsTaken(networkUID, kind) })
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := newIdleWorld(t)
	w.startAgent()
	return w
}

// newIdleWorld is a world whose agent is not started yet.
func newIdleWorld(t *testing.T) *world {
	t.Helper()
	return newIdleWorldWith(t, nil)
}

// newIdleWorldWith is newIdleWorld with the apiserver's config adjusted by
// configure.
func newIdleWorldWith(t *testing.T, configure func(*apiserver.Config)) *world {
	t.Helper()
	env := apiservertest.Start(t, configure)
	env.Setup()
	w := &world{
		t: t, env: env, host: fake.New(), root: env.As(apiservertest.Root), system: env.AsSystem(),
		metadata: metadata.New(metadata.Config{}),
	}
	w.host.AddImage("nginx", "sha256:nginx-1", nginx)
	t.Cleanup(func() { w.stopAgent() })
	return w
}

// startAgent runs a node agent over the world's host.
func (w *world) startAgent() {
	w.t.Helper()
	w.host.SetIPv6Prefix(w.ipv6Prefix)
	w.host.SetSharedIPv4(w.sharedIPv4)
	hc := client.UnixSocket(w.env.Socket)
	if w.wrap != nil {
		hc.Transport = w.wrap(hc.Transport)
	}
	agent, err := node.New(node.Config{
		Name:    apiservertest.Node,
		HTTP:    hc,
		BaseURL: client.UnixSocketURL,
		Tunnel: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", w.env.Socket)
		},
		LogPoll: 20 * time.Millisecond,
		Host: func() (*computev1alpha1.NodeResources, error) {
			return &computev1alpha1.NodeResources{Cpu: "8", Memory: "16Gi"}, nil
		},
		HeartbeatInterval: 50 * time.Millisecond,
		SweepInterval:     cmp.Or(w.sweepInterval, 100*time.Millisecond),
		PullTimeout:       w.pullTimeout,
		DNSUpstreams:      w.dnsUpstreams,
		Metadata:          w.metadata,
		IPv6Prefix:        w.ipv6Prefix,
		SharedIPv4:        w.sharedIPv4,
		EdgeDial:          w.edgeDial,
		EdgeUIDs:          []uint32{uint32(os.Getuid())},
		ImageCacheHigh:    w.imageCacheHigh,
		ImageCacheLow:     w.imageCacheLow,
		Drivers: &node.Drivers{
			Images: w.host.Images(), DiskImages: w.host.DiskImages(), Storage: w.host.Storage(), Network: w.host.Network(),
			Hypervisor: w.host.Hypervisor(), Guest: w.host.Guest(),
		},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	w.agent = agent
	w.stop = func() {
		cancel()
		if err := <-done; err != nil {
			w.t.Errorf("agent: %v", err)
		}
	}
}

func (w *world) stopAgent() {
	if w.stop != nil {
		w.stop()
		w.stop, w.agent = nil, nil
	}
}

// waitSettled waits until the apiserver's controllers and the running
// agent's have finished reacting to every write so far, and returns the keys
// still waiting on something outside them (see controllertest.WaitSettled).
// What the host does reaches the agent outside the API: wait for its effect
// first.
func (w *world) waitSettled() []controllertest.Waiting {
	w.t.Helper()
	if w.agent == nil {
		return w.env.WaitSettled()
	}
	return w.env.WaitSettled(w.agent.ControllerStatus)
}

func (w *world) apply(c *computev1alpha1.Container) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.Container, c); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) delete(name string) {
	w.t.Helper()
	if _, err := w.root.Delete(context.Background(), registry.Container, client.Key{Project: project, Name: name}); err != nil {
		w.t.Fatal(err)
	}
}

func container(name, image string) *computev1alpha1.Container {
	return &computev1alpha1.Container{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec:     &computev1alpha1.ContainerSpec{Image: image},
	}
}

func get[T any](w *world, r registry.Resource, name string) (T, bool) {
	var zero T
	obj, err := w.root.Get(context.Background(), r, client.Key{Project: project, Name: name})
	if connect.CodeOf(err) == connect.CodeNotFound {
		return zero, false
	}
	if err != nil {
		w.t.Fatal(err)
	}
	return obj.(T), true
}

func (w *world) machine(name string) *computev1alpha1.Machine {
	w.t.Helper()
	m, ok := get[*computev1alpha1.Machine](w, registry.Machine, "ct-"+name)
	if !ok {
		w.t.Fatalf("container %s has no machine", name)
	}
	return m
}

// vmVersion reads how often the host started machine uid's VM and how it
// runs, for testutil.Unchanged, which then fails if the host has no VM.
func (w *world) vmVersion(uid string) func() (string, error) {
	return func() (string, error) {
		v, ok := w.host.Snapshot().VMs[uid]
		if !ok {
			return "", errors.New("no VM")
		}
		return fmt.Sprintf("boots %d, started %v, %v", v.Boots, v.Started, v.State), nil
	}
}

// machineVersion reads the resourceVersion of the machine named name, for
// testutil.Unchanged, which then fails if the machine is gone.
func (w *world) machineVersion(name string) func() (string, error) {
	return func() (string, error) {
		m, ok := get[*computev1alpha1.Machine](w, registry.Machine, name)
		if !ok {
			return "", errors.New("gone")
		}
		return fmt.Sprint(m.GetMetadata().GetResourceVersion()), nil
	}
}

func ready(c *computev1alpha1.Container) *metav1.Condition {
	return conditions.Find(c.GetStatus().GetConditions(), "Ready")
}

// waitReason waits for the container to say Ready with the reason, at its
// current generation.
func (w *world) waitReason(name, reason string) *computev1alpha1.Container {
	w.t.Helper()
	var c *computev1alpha1.Container
	testutil.Eventually(w.t, fmt.Sprintf("container %s %s", name, reason), func() (bool, string) {
		var ok bool
		c, ok = get[*computev1alpha1.Container](w, registry.Container, name)
		r := ready(c)
		return ok && r.GetReason() == reason && r.GetObservedGeneration() == c.GetMetadata().GetGeneration(),
			fmt.Sprintf("%v", r)
	})
	return c
}

// settle waits until the container's machine has gone unwritten for a
// while: the agent's own reports have come back to it and nothing is
// left to do. A fault injected after this is noticed only by what is meant
// to notice it.
func (w *world) settle(name string) {
	w.t.Helper()
	last, since := int64(-1), time.Now()
	testutil.Eventually(w.t, "machine of "+name+" quiet", func() (bool, string) {
		rv := w.machine(name).GetMetadata().GetResourceVersion()
		if rv != last {
			last, since = rv, time.Now()
		}
		return time.Since(since) > 300*time.Millisecond, fmt.Sprintf("resource version %d", rv)
	})
}

func (w *world) waitGone(name string) {
	w.t.Helper()
	testutil.Eventually(w.t, "container "+name+" and its machine gone", func() (bool, string) {
		_, c := get[*computev1alpha1.Container](w, registry.Container, name)
		_, m := get[*computev1alpha1.Machine](w, registry.Machine, "ct-"+name)
		return !c && !m, fmt.Sprintf("container left: %t, machine left: %t", c, m)
	})
}

func (w *world) waitHostEmpty() {
	w.t.Helper()
	testutil.Eventually(w.t, "the host to hold nothing but images", func() (bool, string) {
		s := w.host.Snapshot()
		return s.EmptyButImages(), fmt.Sprintf("%+v", s)
	})
}

// TestAContainerRuns follows a container from apply to a running VM and back
// to an empty host.
func TestAContainerRuns(t *testing.T) {
	w := newWorld(t)
	c := container("api", "nginx")
	c.Spec.Env = []*computev1alpha1.EnvVar{{Name: "PATH", Value: "/bin"}, {Name: "MODE", Value: "prod"}}
	w.apply(c)
	w.waitReason("api", "Running")

	m := w.machine("api")
	uid := m.GetMetadata().GetUid()
	st := m.GetStatus()
	if st.GetPhase() != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING || st.GetImageDigest() != "sha256:nginx-1" ||
		st.GetBootTime() == nil || st.GetObservedGeneration() != m.GetMetadata().GetGeneration() {
		t.Errorf("machine status %v", st)
	}
	if !conditions.IsTrue(st.GetConditions(), computeapi.MachineScheduled) {
		t.Error("the node's report dropped the scheduler's condition")
	}
	// Once running, nothing rewrites the machine.
	w.settle("api")
	rv := w.machine("api").GetMetadata().GetResourceVersion()
	testutil.Unchanged(w.t, "a running machine", 500*time.Millisecond, w.machineVersion("ct-api"))
	if after := w.machine("api").GetMetadata().GetResourceVersion(); after != rv {
		t.Errorf("a running machine kept being written: resource version %d, then %d", rv, after)
	}

	net, _ := get[*networkv1alpha1.Network](w, registry.Network, "default")
	vm := w.host.Snapshot().VMs[uid]
	if vm.State != driver.VMRunning || vm.Spec.Root.Digest != "sha256:nginx-1" {
		t.Errorf("VM %+v", vm)
	}
	if got, want := fmt.Sprint(vm.Spec.Run.Command), "[nginx -g daemon off;]"; got != want {
		t.Errorf("runs %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(vm.Spec.Run.Env), "[PATH=/bin MODE=prod]"; got != want {
		t.Errorf("environment %s, want %s", got, want)
	}
	if len(vm.Spec.Interfaces) != 1 {
		t.Fatalf("interfaces %+v", vm.Spec.Interfaces)
	}
	eth0 := vm.Spec.Interfaces[0]
	if eth0.Name != "eth0" || eth0.Address.String() != "10.0.0.2/24" || eth0.Gateway.String() != "10.0.0.1" ||
		eth0.NetworkUID != net.GetMetadata().GetUid() {
		t.Errorf("eth0 %+v", eth0)
	}
	if n := w.host.Snapshot().Networks[net.GetMetadata().GetUid()]; n.Transit.String() != net.GetStatus().GetTransitIpv4() {
		t.Errorf("host network %+v for %v", n, net.GetStatus())
	}

	// A command given without arguments drops the image's arguments.
	sh := container("sh", "nginx")
	sh.Spec.Command = []string{"sh", "-c", "sleep 1d"}
	w.apply(sh)
	w.waitReason("sh", "Running")
	if got := fmt.Sprint(w.host.Snapshot().VMs[w.machine("sh").GetMetadata().GetUid()].Spec.Run.Command); got != "[sh -c sleep 1d]" {
		t.Errorf("runs %s", got)
	}

	w.delete("api")
	w.delete("sh")
	w.waitGone("api")
	w.waitGone("sh")
	w.waitHostEmpty()
}

// TestChangesReachTheHost: a new image rebuilds the root disk, anything
// else restarts the VM on the disk it has, and a tag moving upstream
// changes nothing.
func TestChangesReachTheHost(t *testing.T) {
	w := newWorld(t)
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	uid := w.machine("api").GetMetadata().GetUid()

	c := container("api", "nginx")
	c.Spec.Args = []string{"-c", "/etc/other.conf"}
	w.apply(c)
	testutil.Eventually(w.t, "the VM restarted with the new arguments", func() (bool, string) {
		vm := w.host.Snapshot().VMs[uid]
		return fmt.Sprint(vm.Spec.Run.Command) == "[nginx -c /etc/other.conf]" && vm.State == driver.VMRunning, fmt.Sprintf("%+v", vm)
	})
	w.waitReason("api", "Running")
	if root := w.host.Snapshot().Roots[uid]; root.Digest != "sha256:nginx-1" {
		t.Errorf("a new command rebuilt the disk: %+v", root)
	}

	// The tag moves; the running container keeps its image, even when
	// its VM restarts and the agent looks at it again.
	w.settle("api")
	w.host.AddImage("nginx", "sha256:nginx-2", nginx)
	before := w.machine("api").GetStatus().GetBootTime().AsTime()
	if err := w.host.Exit(uid, 0); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(w.t, "the restart reported", func() (bool, string) {
		boot := w.machine("api").GetStatus().GetBootTime().AsTime()
		return boot.After(before), fmt.Sprintf("boot time %v", boot)
	})
	if root := w.host.Snapshot().Roots[uid]; root.Digest != "sha256:nginx-1" {
		t.Errorf("a tag moving upstream rebuilt the disk: %+v", root)
	}
	w.waitReason("api", "Running")

	// Nor does any other change: the reference is resolved once.
	c = container("api", "nginx")
	c.Spec.Env = []*computev1alpha1.EnvVar{{Name: "MODE", Value: "debug"}}
	w.apply(c)
	testutil.Eventually(w.t, "the VM restarted with the new environment", func() (bool, string) {
		vm := w.host.Snapshot().VMs[uid]
		return slices.Contains(vm.Spec.Run.Env, "MODE=debug") && vm.State == driver.VMRunning, fmt.Sprintf("%+v", vm)
	})
	w.waitReason("api", "Running")
	if root := w.host.Snapshot().Roots[uid]; root.Digest != "sha256:nginx-1" {
		t.Errorf("a change to the environment rebuilt the disk from the moved tag: %+v", root)
	}
	if st := w.machine("api").GetStatus(); st.GetImage() != "nginx" || st.GetImageDigest() != "sha256:nginx-1" {
		t.Errorf("status image %q at %q", st.GetImage(), st.GetImageDigest())
	}

	w.host.AddImage("nginx:2", "sha256:nginx-3", nginx)
	w.apply(container("api", "nginx:2"))
	testutil.Eventually(w.t, "the disk rebuilt from the new image", func() (bool, string) {
		s := w.host.Snapshot()
		return s.Roots[uid].Digest == "sha256:nginx-3" && s.VMs[uid].Spec.Root.Digest == "sha256:nginx-3", fmt.Sprintf("%+v", s.Roots)
	})
	w.waitReason("api", "Running")
	// The old image is kept for its next use.
	w.sweeps(1)
	if b := w.host.Snapshot().Bases; fmt.Sprint(b) != "[sha256:nginx-1 sha256:nginx-3]" || !w.host.Cached("sha256:nginx-1") {
		t.Errorf("bases %v, the old image cached: %t", b, w.host.Cached("sha256:nginx-1"))
	}
}

// TestFailuresAreReportedAndOutlived: each way the host can fail shows on
// the container, with the host's own words, until it clears.
func TestFailuresAreReportedAndOutlived(t *testing.T) {
	w := newWorld(t)

	w.host.FailPull("nginx", errors.New("registry unreachable"))
	w.apply(container("api", "nginx"))
	c := w.waitReason("api", computeapi.ReasonImagePullFailed)
	if msg := ready(c).GetMessage(); msg != "registry unreachable" {
		t.Errorf("message %q", msg)
	}
	w.host.FailPull("nginx", nil)
	w.waitReason("api", "Running")

	w.host.SetFull(true)
	w.apply(container("web", "nginx"))
	w.waitReason("web", computeapi.ReasonDiskFailed)
	w.host.SetFull(false)
	w.waitReason("web", "Running")

	w.host.FailBoot("", errors.New("KVM unavailable"))
	w.apply(container("db", "nginx"))
	w.waitReason("db", computeapi.ReasonBootFailed)
	w.host.FailBoot("", nil)
	w.waitReason("db", "Running")

	w.apply(container("slow", "nginx"))
	testutil.Eventually(w.t, "slow's machine placed", func() (bool, string) {
		m, ok := get[*computev1alpha1.Machine](w, registry.Machine, "ct-slow")
		return ok && m.GetMetadata().GetUid() != "", ""
	})
	slow := w.machine("slow").GetMetadata().GetUid()
	w.host.Silence(slow, true)
	w.waitReason("slow", computeapi.ReasonBooting)
	w.host.Silence(slow, false)
	w.waitReason("slow", "Running")
}

// TestAnExitedContainerSaysSo: a container whose restart policy keeps it
// down after its process exits reports how it ended.
func TestAnExitedContainerSaysSo(t *testing.T) {
	w := newWorld(t)
	c := container("job", "nginx")
	c.Spec.RestartPolicy = computev1alpha1.RestartPolicy_RESTART_POLICY_NEVER
	w.apply(c)
	w.waitReason("job", "Running")
	uid := w.machine("job").GetMetadata().GetUid()
	w.settle("job")

	if err := w.host.Exit(uid, 3); err != nil {
		t.Fatal(err)
	}
	c = w.waitReason("job", computeapi.ReasonExited)
	if msg := ready(c).GetMessage(); msg != "the container exited with code 3" {
		t.Errorf("message %q", msg)
	}
	if m := w.machine("job"); m.GetStatus().GetPhase() != computev1alpha1.MachinePhase_MACHINE_PHASE_FAILED {
		t.Errorf("phase %v", m.GetStatus().GetPhase())
	}
	testutil.Unchanged(w.t, "a VM its policy keeps down", 300*time.Millisecond, w.vmVersion(uid))
	if vm := w.host.Snapshot().VMs[uid]; vm.Boots != 1 {
		t.Errorf("a VM its policy keeps down was started again: %+v", vm)
	}
}

// TestTheNodePicksUpWhereItLeftOff: a machine deleted while the agent is
// down is released when it returns, and whatever the host holds that no
// machine accounts for is removed, but the images kept for their next use.
func TestTheNodePicksUpWhereItLeftOff(t *testing.T) {
	w := newWorld(t)
	w.apply(container("api", "nginx"))
	w.apply(container("web", "nginx"))
	w.waitReason("api", "Running")
	w.waitReason("web", "Running")
	web := w.machine("web").GetMetadata().GetUid()
	w.settle("web")
	before := w.machine("web").GetMetadata().GetResourceVersion()

	w.stopAgent()
	w.delete("api")
	testutil.Unchanged(w.t, "the presence of a machine its node has not released", 200*time.Millisecond, func() (string, error) {
		if _, ok := get[*computev1alpha1.Machine](w, registry.Machine, "ct-api"); !ok {
			return "", errors.New("gone")
		}
		return "present", nil
	})
	w.host.Leave("crashed-machine", "crashed-network")
	w.host.LeaveLog("long-gone-machine")

	w.startAgent()
	w.waitGone("api")
	testutil.Eventually(w.t, "only web left on the host", func() (bool, string) {
		s := w.host.Snapshot()
		_, vm := s.VMs[web]
		return len(s.VMs) == 1 && vm && len(s.Roots) == 1 && len(s.Interfaces) == 1 && len(s.Networks) == 1 &&
			slices.Contains(s.Bases, "sha256:nginx-1") && slices.Equal(s.Logs, []string{web}), fmt.Sprintf("%+v", s)
	})
	w.waitReason("web", "Running")
	if vm := w.host.Snapshot().VMs[web]; vm.Boots != 1 {
		t.Errorf("a restarted agent restarted a running VM: %+v", vm)
	}
	w.settle("web")
	if after := w.machine("web").GetMetadata().GetResourceVersion(); after != before {
		t.Errorf("a restarted agent rewrote a running machine's status: resource version %d, then %d", before, after)
	}
}

// TestLostNetworkingIsRebuilt: networking removed behind the agent's back,
// as networkd can, comes back.
func TestLostNetworkingIsRebuilt(t *testing.T) {
	w := newWorld(t)
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	net, _ := get[*networkv1alpha1.Network](w, registry.Network, "default")
	uid := w.machine("api").GetMetadata().GetUid()
	w.settle("api")

	before := w.host.Snapshot().VMs[uid].Started
	w.host.LoseNetwork(net.GetMetadata().GetUid())
	// A tap made again is not the one the VM holds: the VM is started
	// again on the new one.
	testutil.Eventually(w.t, "the network, the interface and a VM holding it back", func() (bool, string) {
		s := w.host.Snapshot()
		_, n := s.Networks[net.GetMetadata().GetUid()]
		vm := s.VMs[uid]
		return n && len(s.Interfaces[uid]) == 1 && vm.Started.After(before) && !vm.DeadNICs && vm.State == driver.VMRunning,
			fmt.Sprintf("%+v", s)
	})
	w.waitReason("api", "Running")
}
