package apiserver

import (
	"cmp"
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1/nodev1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
)

// Roles of the usage tests: one that may get containers alone, and one
// with every capability of machines but none of getting containers.
const (
	getterRole       = "test.getter"
	capabilitiesRole = "test.capabilities"
)

// usageNode answers MachineUsage as a node does, for machine uid alone,
// its memory counted as source says, the guest's own count if unset.
type usageNode struct {
	nodev1connect.UnimplementedNodeServiceHandler
	uid    string
	source nodev1.MemorySource
}

// usageAt is the minute the node's one point is of.
var usageAt = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func (n usageNode) MachineUsage(_ context.Context, req *connect.Request[nodev1.MachineUsageRequest]) (*connect.Response[nodev1.MachineUsageResponse], error) {
	if req.Msg.GetMachineUid() != n.uid {
		return nil, connect.NewError(connect.CodeFailedPrecondition, nil)
	}
	mem := uint64(300 << 20)
	return connect.NewResponse(&nodev1.MachineUsageResponse{
		MemorySource: cmp.Or(n.source, nodev1.MemorySource_MEMORY_SOURCE_GUEST_USED),
		Points: []*nodev1.UsagePoint{
			{Time: timestamppb.New(usageAt), Cpu: 0.25, MemoryBytes: &mem},
			{Time: timestamppb.New(usageAt.Add(time.Minute)), Cpu: 0.5},
		},
		Latest: &nodev1.UsagePoint{Time: timestamppb.New(usageAt.Add(90 * time.Second)), Cpu: 0.75, MemoryBytes: &mem},
	}), nil
}

// newUsageWorld is an execWorld whose container's machine has 2 vCPUs and
// 1Gi, with dave, who may get containers alone, and erin, who may use every
// capability of machines but get no container.
func newUsageWorld(t *testing.T) *execWorld {
	t.Helper()
	w := newExecWorld(t, func(c *Config) {
		c.ExtraRoles[getterRole] = authz.NewRole(authz.TenantLevel, authz.Permission(registry.Container, authz.Get))
		c.ExtraRoles[capabilitiesRole] = authz.NewRole(authz.TenantLevel, authz.MachineLogs, authz.MachineExec, authz.MachineSSH, authz.MachinePortForward)
	})
	system := auth.WithPrincipal(context.Background(), auth.SystemPrincipal)
	tenant := &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: "acme"}}
	for user, role := range map[string]string{"dave@example.com": getterRole, "erin@example.com": capabilitiesRole} {
		addMember(t, w.s, user)
		if _, err := w.s.backend(registry.RoleBinding).Apply(system, binding(role[len("test."):]+"-"+usernameOf(user), user, role, tenant)); err != nil {
			t.Fatal(err)
		}
	}
	cur, err := w.s.backend(registry.Machine).Get(system, resource.Key{Project: "web", Name: machineName("api")})
	if err != nil {
		t.Fatal(err)
	}
	m := machine(machineName("api"), "n1")
	m.Metadata.OwnerReferences = cur.(*computev1alpha1.Machine).GetMetadata().GetOwnerReferences()
	m.Spec.Resources = &computev1alpha1.Resources{Cpu: "2", Memory: "1Gi"}
	applied, err := w.s.backend(registry.Machine).Apply(system, m)
	if err != nil {
		t.Fatal(err)
	}
	reportMachine(t, w.s, system, applied.Object.(*computev1alpha1.Machine), computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, 2)
	return w
}

func (w *execWorld) usage(t *testing.T, user, kind, name string) (*computev1alpha1.UsageResponse, error) {
	t.Helper()
	req := connect.NewRequest(&computev1alpha1.UsageRequest{Project: "web", Kind: kind, Name: name})
	req.Header().Set(auth.DevUserHeader, user)
	res, err := computev1alpha1connect.NewMachineCapabilityServiceClient(http.DefaultClient, w.url).Usage(t.Context(), req)
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// TestUsageNeedsTheObjectsGet: getting the container alone suffices to read
// its machine's usage, and no capability of machines stands in for it.
func TestUsageNeedsTheObjectsGet(t *testing.T) {
	t.Parallel()
	w := newUsageWorld(t)
	w.connectNodeServing(t, usageNode{uid: w.uid})
	res, err := w.usage(t, "dave@example.com", registry.Container.Kind, "api")
	if err != nil {
		t.Fatalf("one who may get the container: %v", err)
	}
	if res.GetVcpus() != 2 || res.GetMemoryLimitBytes() != 1<<30 ||
		res.GetMemoryKind() != computev1alpha1.MachineMemoryKind_MACHINE_MEMORY_KIND_GUEST_USED {
		t.Errorf("the usage is %v, want 2 vCPUs, 1Gi and the guest's own count", res)
	}
	pts := res.GetPoints()
	if len(pts) != 2 || !pts[0].GetTime().AsTime().Equal(usageAt) || pts[0].GetCpu() != 0.25 || pts[0].GetMemoryBytes() != 300<<20 ||
		pts[1].GetCpu() != 0.5 || pts[1].MemoryBytes != nil {
		t.Errorf("the points are %v, as the node gave them", pts)
	}
	if l := res.GetLatest(); l.GetCpu() != 0.75 || !l.GetTime().AsTime().Equal(usageAt.Add(90*time.Second)) {
		t.Errorf("the latest is %v, as the node gave it", l)
	}
	for _, user := range []string{"erin@example.com", "bob@example.com"} {
		_, err := w.usage(t, user, registry.Container.Kind, "api")
		wantCodeOf(t, err, connect.CodePermissionDenied, user+" reading the usage of a container they may not get")
	}
	// Refused before anything is told of the object, as of one absent.
	_, err = w.usage(t, "bob@example.com", registry.Container.Kind, "absent")
	wantCodeOf(t, err, connect.CodePermissionDenied, "reading the usage of an absent container one may not get")
}

// TestUsageOfWhatHasNoRunningMachine: a kind that runs as no machine is
// refused; an object not on a node, or not running, has no usage; one whose
// node is not connected has none yet.
func TestUsageOfWhatHasNoRunningMachine(t *testing.T) {
	t.Parallel()
	w := newUsageWorld(t)
	alice := "alice@example.com"
	for _, kind := range []string{registry.Disk.Kind, "Nothing", ""} {
		_, err := w.usage(t, alice, kind, "api")
		wantCodeOf(t, err, connect.CodeInvalidArgument, "the usage of kind "+kind)
	}
	_, err := w.usage(t, alice, registry.Container.Kind, "api")
	wantCodeOf(t, err, connect.CodeUnavailable, "the usage of a container whose node is not connected")

	system := auth.WithPrincipal(context.Background(), auth.SystemPrincipal)
	if _, err := w.s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	user := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: alice})
	if _, err := w.s.backend(registry.VirtualMachine).Apply(user, vmFromImage("web", "noble")); err != nil {
		t.Fatal(err)
	}
	_, err = w.usage(t, alice, registry.VirtualMachine.Kind, "web")
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "the usage of a virtual machine not on a node")

	cur, err := w.s.backend(registry.Machine).Get(system, resource.Key{Project: "web", Name: machineName("api")})
	if err != nil {
		t.Fatal(err)
	}
	reportMachine(t, w.s, system, cur.(*computev1alpha1.Machine), computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, 2,
		&metav1.Condition{Type: "Ready", Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "Stopped"})
	_, err = w.usage(t, alice, registry.Container.Kind, "api")
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "the usage of a stopped container")
}

// TestUsageSaysWhatItsMemoryCounts: the memory a node says is what the
// host holds is answered as such, not as the guest's own count.
func TestUsageSaysWhatItsMemoryCounts(t *testing.T) {
	t.Parallel()
	w := newUsageWorld(t)
	w.connectNodeServing(t, usageNode{uid: w.uid, source: nodev1.MemorySource_MEMORY_SOURCE_HOST_HELD})
	res, err := w.usage(t, "alice@example.com", registry.Container.Kind, "api")
	if err != nil {
		t.Fatal(err)
	}
	if res.GetMemoryKind() != computev1alpha1.MachineMemoryKind_MACHINE_MEMORY_KIND_HOST_HELD {
		t.Errorf("memory the host holds is answered as %v", res.GetMemoryKind())
	}
}
