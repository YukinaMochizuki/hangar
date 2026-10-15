package node_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/edgedial"
	"github.com/YukinaMochizuki/hangar/internal/node"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
)

// otherNode is a node no agent here runs, reported ready by the test.
const otherNode = "n2"

// dialWorld is an apiserver with nodes n1, whose agent runs on a fake host
// and takes dials on its edge dial socket, and n2, which the test reports
// ready when it needs a machine elsewhere.
type dialWorld struct {
	t      *testing.T
	root   *client.Client
	system *client.Client
	// systemURL serves every caller as the System principal.
	systemURL string
	host      *fake.Host
	sock      string
}

func newDialWorld(t *testing.T) *dialWorld {
	t.Helper()
	env := apiservertest.Start(t, func(c *apiserver.Config) { c.Nodes = append(c.Nodes, otherNode) })
	env.Setup()
	host := fake.New()
	host.AddImage("nginx", "sha256:nginx-1", driver.ImageConfig{Entrypoint: []string{"nginx"}})
	dir, err := os.MkdirTemp("", "dial")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "dial.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	// n1 has room for small machines alone, n2 for big ones.
	startAgent(t, env, &computev1alpha1.NodeResources{Cpu: "4", Memory: "2Gi"}, nil, func(c *node.Config) {
		c.Drivers = &node.Drivers{Images: host.Images(), DiskImages: host.DiskImages(), Storage: host.Storage(),
			Network: host.Network(), Hypervisor: host.Hypervisor(), Guest: host.Guest()}
		c.SweepInterval = 100 * time.Millisecond
		c.EdgeDial = ln
		c.EdgeUIDs = []uint32{uint32(os.Getuid())}
	})
	return &dialWorld{t: t, root: env.As(apiservertest.Root), system: env.AsSystem(), systemURL: env.SystemURL, host: host, sock: sock}
}

// otherNodeReady reports n2 ready, with room for anything.
func (w *dialWorld) otherNodeReady() {
	w.t.Helper()
	nodes := computev1alpha1connect.NewNodeServiceClient(http.DefaultClient, w.systemURL)
	if _, err := nodes.UpdateNodeStatus(w.t.Context(), connect.NewRequest(&computev1alpha1.UpdateNodeStatusRequest{
		Name: otherNode,
		Status: &computev1alpha1.NodeStatus{
			Capacity:   &computev1alpha1.NodeResources{Cpu: "16", Memory: "64Gi", Disk: "1Ti"},
			Conditions: []*metav1.Condition{{Type: apiserver.NodeReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "AgentReporting"}},
		},
	})); err != nil {
		w.t.Fatal(err)
	}
}

// container applies a container of memory, run or not, with an endpoint
// of its name on port 8080, and returns the endpoint once it names the
// machine.
func (w *dialWorld) container(name, memory string, stopped bool) *networkv1alpha1.PublicEndpoint {
	w.t.Helper()
	c := &computev1alpha1.Container{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: name},
		Spec:     &computev1alpha1.ContainerSpec{Image: "nginx", Resources: &computev1alpha1.Resources{Cpu: "1", Memory: memory}},
	}
	if stopped {
		c.Spec.RunState = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED
	}
	if _, err := w.root.Apply(w.t.Context(), registry.Container, c); err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.root.Apply(w.t.Context(), registry.PublicEndpoint, &networkv1alpha1.PublicEndpoint{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: name},
		Spec: &networkv1alpha1.PublicEndpointSpec{
			Target: &networkv1alpha1.PublicEndpointTarget{Object: &networkv1alpha1.PublicEndpointTarget_Container{Container: name}},
			Port:   8080,
		},
	}); err != nil {
		w.t.Fatal(err)
	}
	return waitFor(w.t, "endpoint "+name+" naming its machine", func() (*networkv1alpha1.PublicEndpoint, error) {
		obj, err := w.root.Get(w.t.Context(), registry.PublicEndpoint, client.Key{Project: apiservertest.Project, Name: name})
		if err != nil {
			return nil, err
		}
		return obj.(*networkv1alpha1.PublicEndpoint), nil
	}, func(ep *networkv1alpha1.PublicEndpoint) bool {
		return ep.GetStatus().GetMachine() != "" && ep.GetStatus().GetNode() != ""
	})
}

func (w *dialWorld) dial(ep *networkv1alpha1.PublicEndpoint, uid string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(w.t.Context(), 5*time.Second)
	defer cancel()
	return edgedial.Dial(ctx, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", w.sock)
	}, edgedial.Target{Project: apiservertest.Project, Name: ep.GetMetadata().GetName(), UID: uid})
}

// refused checks the agent refused a dial with status, saying why.
func refused(t *testing.T, what string, err error, status int, why string) {
	t.Helper()
	var de *edgedial.Error
	if !errors.As(err, &de) || de.Status != status || !strings.Contains(de.Message, why) {
		t.Errorf("%s: %v, want a refusal with %d saying %q", what, err, status, why)
	}
}

// TestTheAgentDialsOnlyALiveEndpointsRunningMachine: the agent reaches an
// endpoint whose machine runs on its node, and refuses one named with
// another uid, one being deleted, one whose machine runs on another node,
// and one whose machine is not running.
func TestTheAgentDialsOnlyALiveEndpointsRunningMachine(t *testing.T) {
	w := newDialWorld(t)
	ep := w.container("api", "256Mi", false)
	if ep.GetStatus().GetNode() != apiservertest.Node {
		t.Fatalf("api is on %q", ep.GetStatus().GetNode())
	}
	waitFor(t, "api running", func() (*networkv1alpha1.PublicEndpoint, error) {
		obj, err := w.root.Get(t.Context(), registry.PublicEndpoint, client.Key{Project: apiservertest.Project, Name: "api"})
		if err != nil {
			return nil, err
		}
		return obj.(*networkv1alpha1.PublicEndpoint), nil
	}, func(ep *networkv1alpha1.PublicEndpoint) bool {
		return conditions.IsTrue(ep.GetStatus().GetConditions(), apiserver.PublicEndpointReady)
	})
	m, err := w.root.Get(t.Context(), registry.Machine, client.Key{Project: apiservertest.Project, Name: ep.GetStatus().GetMachine()})
	if err != nil {
		t.Fatal(err)
	}
	addr := netip.MustParsePrefix(m.(*computev1alpha1.Machine).GetSpec().GetInterfaces()[0].GetIpv4()).Addr()
	var netUID string
	for _, n := range w.host.Snapshot().Interfaces[m.(*computev1alpha1.Machine).GetMetadata().GetUid()] {
		if n.Name == "eth0" {
			netUID = n.NetworkUID
		}
	}
	w.host.ServeGuest(netUID, netip.AddrPortFrom(addr, 8080), func(c net.Conn) {
		defer func() { _ = c.Close() }()
		_, _ = io.WriteString(c, "hello")
	})
	uid := ep.GetMetadata().GetUid()

	// Reached, once the firewall lets the edge to its port.
	var c net.Conn
	waitFor(t, "a dial to api", func() (*networkv1alpha1.PublicEndpoint, error) {
		c, err = w.dial(ep, uid)
		return ep, err
	}, func(*networkv1alpha1.PublicEndpoint) bool { return true })
	b, _ := io.ReadAll(c)
	_ = c.Close()
	if string(b) != "hello" {
		t.Fatalf("the guest said %q", b)
	}

	t.Run("another uid", func(t *testing.T) {
		_, err := w.dial(ep, "0000-1111")
		refused(t, "a dial with another uid", err, http.StatusNotFound, "with uid 0000-1111 does not exist")
	})
	t.Run("not running", func(t *testing.T) {
		idle := w.container("idle", "256Mi", true)
		if idle.GetStatus().GetNode() != apiservertest.Node {
			t.Fatalf("idle is on %q", idle.GetStatus().GetNode())
		}
		waitFor(t, "idle's machine stopped", func() (*computev1alpha1.Machine, error) {
			obj, err := w.root.Get(t.Context(), registry.Machine, client.Key{Project: apiservertest.Project, Name: idle.GetStatus().GetMachine()})
			if err != nil {
				return nil, err
			}
			return obj.(*computev1alpha1.Machine), nil
		}, func(m *computev1alpha1.Machine) bool {
			return m.GetStatus().GetPhase() == computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED
		})
		_, err := w.dial(idle, idle.GetMetadata().GetUid())
		refused(t, "a dial to a stopped machine", err, http.StatusConflict, "is not running")
	})
	t.Run("being deleted", func(t *testing.T) {
		// Held, deleted, by a finalizer of the test's, which the system
		// alone may add.
		obj, err := w.system.Get(t.Context(), registry.PublicEndpoint, client.Key{Project: apiservertest.Project, Name: "api"})
		if err != nil {
			t.Fatal(err)
		}
		held := obj.(*networkv1alpha1.PublicEndpoint)
		held.Metadata.Finalizers = append(held.Metadata.Finalizers, "test.invalid/hold")
		if _, err := w.system.Apply(t.Context(), registry.PublicEndpoint, held); err != nil {
			t.Fatal(err)
		}
		if _, err := w.root.Delete(t.Context(), registry.PublicEndpoint, client.Key{Project: apiservertest.Project, Name: "api"}); err != nil {
			t.Fatal(err)
		}
		_, err = w.dial(ep, uid)
		refused(t, "a dial to an endpoint being deleted", err, http.StatusNotFound, "is being deleted")
	})
	t.Run("on another node", func(t *testing.T) {
		w.otherNodeReady()
		far := w.container("far", "8Gi", false)
		if far.GetStatus().GetNode() != otherNode {
			t.Fatalf("far is on %q", far.GetStatus().GetNode())
		}
		_, err := w.dial(far, far.GetMetadata().GetUid())
		refused(t, "a dial to a machine on another node", err, http.StatusConflict, `runs on node "n2"`)
	})
}
