package apiserver

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1/tenancyv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
)

// poolNode is a usageNode that keeps CPU pool acme slowed slowed of the
// time, held to milliCPU millicores, its members sampled now.
type poolNode struct {
	usageNode
	slowed   float64
	milliCPU int64
}

func (n poolNode) MachineUsage(ctx context.Context, req *connect.Request[nodev1.MachineUsageRequest]) (*connect.Response[nodev1.MachineUsageResponse], error) {
	res, err := n.usageNode.MachineUsage(ctx, req)
	if err != nil {
		return nil, err
	}
	res.Msg.CpuPoolSlowed, res.Msg.CpuPoolMilliCpu = proto.Float64(n.slowed), proto.Int64(n.milliCPU)
	return res, nil
}

func (n poolNode) CPUPoolUsage(_ context.Context, req *connect.Request[nodev1.CPUPoolUsageRequest]) (*connect.Response[nodev1.CPUPoolUsageResponse], error) {
	if req.Msg.GetPool() != "acme" {
		return connect.NewResponse(&nodev1.CPUPoolUsageResponse{}), nil
	}
	return connect.NewResponse(&nodev1.CPUPoolUsageResponse{Slowed: proto.Float64(n.slowed), MilliCpu: proto.Int64(n.milliCPU), SampleTime: timestamppb.Now()}), nil
}

// newPoolWorld is a usage world on nodes n1 and n2 whose tenant acme
// shares 4 CPUs as a pool, its container's machine on n1 in it, and bob
// owning tenant bobs, which has none.
func newPoolWorld(t *testing.T) *execWorld {
	t.Helper()
	w := newUsageWorld(t, func(c *Config) { c.Nodes = append(c.Nodes, "n2") })
	setDefaultPlan(t, w.s, cpuPooledLimits("4", 200))
	system := auth.WithPrincipal(context.Background(), auth.SystemPrincipal)
	cur, err := w.s.backend(registry.Machine).Get(system, resource.Key{Project: "web", Name: machineName("api")})
	if err != nil {
		t.Fatal(err)
	}
	m := cur.(*computev1alpha1.Machine)
	m.Spec.CpuPool = &computev1alpha1.CPUPool{Name: "acme", Cpu: "4"}
	applied, err := w.s.backend(registry.Machine).Apply(system, m)
	if err != nil {
		t.Fatal(err)
	}
	m = applied.Object.(*computev1alpha1.Machine)
	reportMachine(t, w.s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration())
	bob := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: "bob@example.com"})
	if _, err := w.s.backend(registry.Tenant).Apply(bob, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "bobs"}}); err != nil {
		t.Fatal(err)
	}
	return w
}

// pooledOn runs another machine of acme's pool, name, on node, in phase.
func (w *execWorld) pooledOn(t *testing.T, name, node string, phase computev1alpha1.MachinePhase) {
	t.Helper()
	system := auth.WithPrincipal(context.Background(), auth.SystemPrincipal)
	m := machine(name, node)
	m.Spec.Resources = &computev1alpha1.Resources{Cpu: "1", Memory: "512Mi"}
	m.Spec.CpuPool = &computev1alpha1.CPUPool{Name: "acme", Cpu: "4"}
	applied, err := w.s.backend(registry.Machine).Apply(system, m)
	if err != nil {
		t.Fatal(err)
	}
	m = applied.Object.(*computev1alpha1.Machine)
	reportMachine(t, w.s, system, m, phase, m.GetMetadata().GetGeneration())
}

func (w *execWorld) cpuPoolUsage(t *testing.T, user, tenant string) (*tenancyv1alpha1.CPUPoolUsageResponse, error) {
	t.Helper()
	req := connect.NewRequest(&tenancyv1alpha1.CPUPoolUsageRequest{Tenant: tenant})
	req.Header().Set(auth.DevUserHeader, user)
	res, err := tenancyv1alpha1connect.NewTenantCapabilityServiceClient(http.DefaultClient, w.url).CPUPoolUsage(t.Context(), req)
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// TestUsagePassesTheCPUPoolOn: a machine's usage says what its node says
// of its CPU pool, and nothing when its node says nothing.
func TestUsagePassesTheCPUPoolOn(t *testing.T) {
	t.Parallel()
	w := newUsageWorld(t)
	w.connectNodeServing(t, poolNode{usageNode: usageNode{uid: w.uid}, slowed: 0.25, milliCPU: 4000})
	res, err := w.usage(t, "dave@example.com", registry.Container.Kind, "api")
	if err != nil {
		t.Fatal(err)
	}
	if res.CpuPoolSlowed == nil || res.GetCpuPoolSlowed() != 0.25 || res.GetCpuPoolMilliCpu() != 4000 {
		t.Errorf("the usage's pool: slowed %v, %v millicores; want 0.25 of 4000", res.CpuPoolSlowed, res.CpuPoolMilliCpu)
	}

	w = newUsageWorld(t)
	w.connectNodeServing(t, usageNode{uid: w.uid})
	res, err = w.usage(t, "dave@example.com", registry.Container.Kind, "api")
	if err != nil {
		t.Fatal(err)
	}
	if res.CpuPoolSlowed != nil || res.CpuPoolMilliCpu != nil {
		t.Errorf("a machine in no pool: slowed %v, %v millicores", res.CpuPoolSlowed, res.CpuPoolMilliCpu)
	}
}

// TestCPUPoolUsageAsksOnlyNodesRunningThePool: a node whose machine of
// the pool is stopped is not asked, so not counted unread.
func TestCPUPoolUsageAsksOnlyNodesRunningThePool(t *testing.T) {
	t.Parallel()
	w := newPoolWorld(t)
	w.connectNodeServing(t, poolNode{slowed: 0.25, milliCPU: 4000})
	w.pooledOn(t, "m-other", "n2", computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED)
	res, err := w.cpuPoolUsage(t, "alice@example.com", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if res.GetNodesUnread() != 0 {
		t.Errorf("%d nodes unread, though n2 runs none of the pool", res.GetNodesUnread())
	}
}
