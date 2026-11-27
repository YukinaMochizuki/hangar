package apiserver

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1/nodev1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1/tenancyv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
)

const gib = int64(1) << 30

// memoryPoolNode is a node whose squeeze last read pool acme at sampled,
// its members as given; with sampled zero, one with no read lately, which
// answers nothing.
type memoryPoolNode struct {
	nodev1connect.UnimplementedNodeServiceHandler
	sampled time.Time
	members []*nodev1.MemoryPoolMember
}

func (n memoryPoolNode) MemoryPoolUsage(_ context.Context, req *connect.Request[nodev1.MemoryPoolUsageRequest]) (*connect.Response[nodev1.MemoryPoolUsageResponse], error) {
	if req.Msg.GetPool() != "acme" || n.sampled.IsZero() {
		return connect.NewResponse(&nodev1.MemoryPoolUsageResponse{}), nil
	}
	return connect.NewResponse(&nodev1.MemoryPoolUsageResponse{SampleTime: timestamppb.New(n.sampled), Members: n.members}), nil
}

// memoryPoolWorld is a usage world whose tenant acme shares 8Gi as a
// memory pool, frank viewing its project web alone, with, running in the
// pool: web's container api, Dedicated 1Gi, on n1; web's VM vm1, 4Gi at
// 1Gi, on n1; and project db's container pg, 2Gi at 512Mi, on n2. Web's
// stopped container old and its FileShare's server, in no pool, are not
// in it. The uids are by name.
type memoryPoolWorld struct {
	*execWorld
	uids map[string]string
}

func newMemoryPoolWorld(t *testing.T) *memoryPoolWorld {
	t.Helper()
	w := &memoryPoolWorld{execWorld: newUsageWorld(t, func(c *Config) { c.Nodes = append(c.Nodes, "n2") }), uids: map[string]string{}}
	setDefaultPlan(t, w.s, pooledLimits("8Gi", 200))
	system := auth.WithPrincipal(context.Background(), auth.SystemPrincipal)
	alice := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: "alice@example.com"})
	if _, err := w.s.backend(registry.Project).Apply(alice, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "db"}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "acme"},
	}); err != nil {
		t.Fatal(err)
	}
	addMember(t, w.s, "frank@example.com")
	if _, err := w.s.backend(registry.RoleBinding).Apply(system, binding("viewer-frank", "frank@example.com", "project.viewer",
		&iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Project{Project: "web"}})); err != nil {
		t.Fatal(err)
	}
	pool := &computev1alpha1.MemoryPool{Name: "acme", Size: "8Gi"}
	cur, err := w.s.backend(registry.Machine).Get(system, resource.Key{Project: "web", Name: machineName("api")})
	if err != nil {
		t.Fatal(err)
	}
	api := cur.(*computev1alpha1.Machine)
	api.Spec.MemoryPool = pool
	w.put(t, "api", api, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING)
	if _, err := w.s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	vm1 := vmFromImage("vm1", "noble")
	pg, old := ctr("pg"), ctr("old")
	pg.Metadata.Project = "db"
	w.put(t, "vm1", poolMachineOf("web", w.owner(t, registry.VirtualMachine, vm1), "n1", "4Gi", "1Gi", pool), computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING)
	w.put(t, "pg", poolMachineOf("db", w.owner(t, registry.Container, pg), "n2", "2Gi", "512Mi", pool), computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING)
	w.put(t, "old", poolMachineOf("web", w.owner(t, registry.Container, old), "n1", "2Gi", "", pool), computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED)
	w.put(t, "share", poolMachineOf("web", w.owner(t, registry.Container, ctr("files")), "n1", "1Gi", "", nil), computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING)
	return w
}

// owner stores obj, of kind r, as the system, and is a controller's
// reference to it.
func (w *memoryPoolWorld) owner(t *testing.T, r registry.Resource, obj proto.Message) *metav1.OwnerReference {
	t.Helper()
	system := auth.WithPrincipal(context.Background(), auth.SystemPrincipal)
	applied, err := w.s.backend(r).Apply(system, obj)
	if err != nil {
		t.Fatal(err)
	}
	meta := metaOf(applied.Object)
	return &metav1.OwnerReference{ApiVersion: r.APIVersion(), Kind: r.Kind, Name: meta.GetName(), Uid: meta.GetUid(), Controller: true}
}

// poolMachineOf is a Machine in project run by owner, of
// memory reserved reserved ("" for all), in pool if set.
func poolMachineOf(project string, owner *metav1.OwnerReference, node, memory, reserved string, pool *computev1alpha1.MemoryPool) *computev1alpha1.Machine {
	m := machine(owner.GetName()+"-m", node)
	m.Metadata.Project = project
	m.Metadata.OwnerReferences = []*metav1.OwnerReference{owner}
	m.Spec.Resources = &computev1alpha1.Resources{Cpu: "1", Memory: memory, MemoryReserved: reserved}
	m.Spec.MemoryPool = pool
	return m
}

// put stores m and reports it in phase, keeping its uid as name's.
func (w *memoryPoolWorld) put(t *testing.T, name string, m *computev1alpha1.Machine, phase computev1alpha1.MachinePhase) {
	t.Helper()
	system := auth.WithPrincipal(context.Background(), auth.SystemPrincipal)
	applied, err := w.s.backend(registry.Machine).Apply(system, m)
	if err != nil {
		t.Fatal(err)
	}
	m = applied.Object.(*computev1alpha1.Machine)
	key := resource.Key{Project: m.GetMetadata().GetProject(), Name: m.GetMetadata().GetName()}
	status := &computev1alpha1.MachineStatus{Phase: phase, ObservedGeneration: m.GetMetadata().GetGeneration()}
	if _, err := w.s.backend(registry.Machine).UpdateStatus(asNode(system, m.GetSpec().GetNode()), key, status, nil); err != nil {
		t.Fatal(err)
	}
	w.uids[name] = m.GetMetadata().GetUid()
}

func (w *execWorld) memoryPoolUsage(t *testing.T, user, tenant string) (*tenancyv1alpha1.MemoryPoolUsageResponse, error) {
	t.Helper()
	req := connect.NewRequest(&tenancyv1alpha1.MemoryPoolUsageRequest{Tenant: tenant})
	req.Header().Set(auth.DevUserHeader, user)
	res, err := tenancyv1alpha1connect.NewTenantCapabilityServiceClient(http.DefaultClient, w.url).MemoryPoolUsage(t.Context(), req)
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

var (
	poolSampled = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	poolKilled  = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
)

// connectBoth serves n1, read a second after n2, with api at 1Gi using
// 768Mi, vm1 at 2.75Gi using 256Mi, killed at poolKilled, and old, though
// stopped; and n2 with pg's balloon unread and unresponsive since
// poolSampled.
func (w *memoryPoolWorld) connectBoth(t *testing.T) {
	t.Helper()
	w.connectNodeAs(t, "n1", memoryPoolNode{sampled: poolSampled.Add(time.Second), members: []*nodev1.MemoryPoolMember{
		{MachineUid: w.uids["api"], SizeBytes: proto.Uint64(uint64(gib)), InUseBytes: proto.Uint64(uint64(gib * 3 / 4))},
		{MachineUid: w.uids["vm1"], SizeBytes: proto.Uint64(uint64(gib * 11 / 4)), InUseBytes: proto.Uint64(uint64(gib / 4)), LastOomKillTime: timestamppb.New(poolKilled)},
		{MachineUid: w.uids["old"], SizeBytes: proto.Uint64(uint64(gib))},
	}})
	w.connectNodeAs(t, "n2", memoryPoolNode{sampled: poolSampled, members: []*nodev1.MemoryPoolMember{
		{MachineUid: w.uids["pg"], BalloonUnresponsiveSince: timestamppb.New(poolSampled)},
	}})
}

func poolNames(res *tenancyv1alpha1.MemoryPoolUsageResponse) []string {
	var out []string
	for _, m := range res.GetMachines() {
		out = append(out, m.GetProject()+"/"+m.GetName())
	}
	return out
}

// TestMemoryPoolUsageNamesAllToTheTenantsOwner: the tenant's owner sees
// every running machine in the pool, Dedicated first, then by what it
// uses, largest first, one whose guest does not say by what it has, or
// its reservation: pg's 512Mi before vm1's 256Mi in use, though vm1 has
// 2.75Gi. Each is as its node read it, nothing summed; the oldest of the
// nodes' reads is when it was sampled.
func TestMemoryPoolUsageNamesAllToTheTenantsOwner(t *testing.T) {
	t.Parallel()
	w := newMemoryPoolWorld(t)
	w.connectBoth(t)
	res, err := w.memoryPoolUsage(t, "alice@example.com", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if res.GetSizeBytes() != 8*gib || !res.GetSampleTime().AsTime().Equal(poolSampled) || res.GetNodesUnread() != 0 || res.GetOther() != nil {
		t.Errorf("the pool: %d bytes, sampled %v, %d unread, other %v; want 8Gi, %v, none, none",
			res.GetSizeBytes(), res.GetSampleTime().AsTime(), res.GetNodesUnread(), res.GetOther(), poolSampled)
	}
	if got := poolNames(res); len(got) != 3 || got[0] != "web/api" || got[1] != "db/pg" || got[2] != "web/vm1" {
		t.Fatalf("machines %v, want web/api, db/pg, web/vm1", got)
	}
	api, pg, vm1 := res.GetMachines()[0], res.GetMachines()[1], res.GetMachines()[2]
	if api.GetKind() != "Container" || api.GetMemoryBytes() != gib || api.GetReservedBytes() != gib || api.GetCurrentBytes() != gib || api.GetInUseBytes() != gib*3/4 ||
		api.GetPhase() != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING {
		t.Errorf("api: %v, want a running Dedicated Container at 1Gi", api)
	}
	if vm1.GetKind() != "VirtualMachine" || vm1.GetMemoryBytes() != 4*gib || vm1.GetReservedBytes() != gib || vm1.GetCurrentBytes() != gib*11/4 || vm1.GetInUseBytes() != gib/4 ||
		!vm1.GetLastOomKillTime().AsTime().Equal(poolKilled) || vm1.GetBalloonUnresponsiveSince() != nil {
		t.Errorf("vm1: %v, want a VirtualMachine of 4Gi at 1Gi, 2.75Gi now, killed at %v", vm1, poolKilled)
	}
	if pg.CurrentBytes != nil || pg.InUseBytes != nil || pg.GetReservedBytes() != gib/2 || !pg.GetBalloonUnresponsiveSince().AsTime().Equal(poolSampled) {
		t.Errorf("pg: %v, want no size now, 512Mi reserved, unresponsive since %v", pg, poolSampled)
	}
}

// TestMemoryPoolUsageFoldsWhatTheCallerMayNotRead: one who views a single
// project sees its machines by name and the rest of the pool's summed
// into other, a machine its node could not tell counted at its
// reservation; one who may get no container or VM of the tenant may not
// ask, nor anyone ask of a tenant that does not exist.
func TestMemoryPoolUsageFoldsWhatTheCallerMayNotRead(t *testing.T) {
	t.Parallel()
	w := newMemoryPoolWorld(t)
	w.connectBoth(t)
	res, err := w.memoryPoolUsage(t, "frank@example.com", "acme")
	if err != nil {
		t.Fatalf("one project's viewer: %v", err)
	}
	if got := poolNames(res); len(got) != 2 || got[0] != "web/api" || got[1] != "web/vm1" {
		t.Errorf("machines %v, want web's alone", got)
	}
	// Of n1 alone, which web's machines are on: n2, read earlier, has only
	// pg, which frank may not read.
	if !res.GetSampleTime().AsTime().Equal(poolSampled.Add(time.Second)) || res.GetNodesUnread() != 0 {
		t.Errorf("sampled %v, %d unread; want n1's %v, none", res.GetSampleTime().AsTime(), res.GetNodesUnread(), poolSampled.Add(time.Second))
	}
	o := res.GetOther()
	if o.GetMemoryBytes() != 2*gib || o.GetReservedBytes() != gib/2 || o.GetCurrentBytes() != gib/2 || o.GetInUseBytes() != gib/2 || res.GetSizeBytes() != 8*gib {
		t.Errorf("other %v, size %d; want pg's 2Gi at 512Mi, 512Mi now and in use, of 8Gi", o, res.GetSizeBytes())
	}

	// pg read: other sums what its guest has and what it uses.
	w.connectNodeAs(t, "n2", memoryPoolNode{sampled: poolSampled, members: []*nodev1.MemoryPoolMember{
		{MachineUid: w.uids["pg"], SizeBytes: proto.Uint64(uint64(gib * 3 / 2)), InUseBytes: proto.Uint64(uint64(gib * 3 / 4))},
	}})
	res, err = w.memoryPoolUsage(t, "frank@example.com", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if o := res.GetOther(); o.GetCurrentBytes() != gib*3/2 || o.GetInUseBytes() != gib*3/4 {
		t.Errorf("other %v, want pg's 1.5Gi now, 768Mi in use", o)
	}

	for _, user := range []string{"bob@example.com", "erin@example.com"} {
		_, err := w.memoryPoolUsage(t, user, "acme")
		wantCodeOf(t, err, connect.CodePermissionDenied, user+" reading acme's memory pool")
	}
	_, err = w.memoryPoolUsage(t, "frank@example.com", "absent")
	wantCodeOf(t, err, connect.CodePermissionDenied, "reading the pool of a tenant that does not exist")
	// One over every tenant is told it does not exist, as by CPUPoolUsage.
	asRoot(t, w.s, auth.WithPrincipal(context.Background(), auth.SystemPrincipal))
	_, err = w.memoryPoolUsage(t, "root@example.com", "absent")
	wantCodeOf(t, err, connect.CodeNotFound, "root reading the pool of a tenant that does not exist")
}

// TestMemoryPoolUsageLeavesOutAMalformedMachine: a machine whose memory
// does not parse is left out, and fails nobody's answer.
func TestMemoryPoolUsageLeavesOutAMalformedMachine(t *testing.T) {
	t.Parallel()
	w := newMemoryPoolWorld(t)
	w.connectBoth(t)
	owner := ctr("bad")
	owner.Metadata.Project = "db"
	w.put(t, "bad", poolMachineOf("db", w.owner(t, registry.Container, owner), "n2", "2Gi", "", &computev1alpha1.MemoryPool{Name: "acme", Size: "8Gi"}), computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING)
	// Admission refuses such memory, so it is written to the store as data
	// from before a rule, or a bug, would be.
	b := w.s.backend(registry.Machine)
	key := b.storeKey(resource.Key{Project: "db", Name: "bad-m"})
	stored, err := w.s.store.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := b.decode(stored)
	if err != nil {
		t.Fatal(err)
	}
	obj.(*computev1alpha1.Machine).Spec.Resources.Memory = "lots"
	data, err := encode(obj)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.store.Update(t.Context(), key, data, stored.Revision); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"alice@example.com", "frank@example.com"} {
		res, err := w.memoryPoolUsage(t, user, "acme")
		if err != nil {
			t.Fatalf("%s with a malformed machine: %v", user, err)
		}
		if slices.Contains(poolNames(res), "db/bad") {
			t.Errorf("%s sees the malformed machine: %v", user, poolNames(res))
		}
	}
}

// TestMemoryPoolUsageCountsNodesUnread: a node not connected is counted,
// its machines listed with nothing of now; a tenant with no memory pool
// answers nothing.
func TestMemoryPoolUsageCountsNodesUnread(t *testing.T) {
	t.Parallel()
	w := newMemoryPoolWorld(t)
	w.connectNodeAs(t, "n1", memoryPoolNode{sampled: poolSampled})
	res, err := w.memoryPoolUsage(t, "alice@example.com", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if res.GetNodesUnread() != 1 || len(res.GetMachines()) != 3 || res.GetMachines()[2].CurrentBytes != nil {
		t.Errorf("with n2 not connected: %v, want one node unread and pg with no size", res)
	}
	// n2 has only pg, which frank may not read: not his to count.
	res, err = w.memoryPoolUsage(t, "frank@example.com", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if res.GetNodesUnread() != 0 || !res.GetSampleTime().AsTime().Equal(poolSampled) {
		t.Errorf("frank, with n2 not connected: %d unread, sampled %v; want none, n1's %v", res.GetNodesUnread(), res.GetSampleTime().AsTime(), poolSampled)
	}
	// A node with no read lately is unread too.
	w.connectNodeAs(t, "n2", memoryPoolNode{})
	res, err = w.memoryPoolUsage(t, "alice@example.com", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if res.GetNodesUnread() != 1 || !res.GetSampleTime().AsTime().Equal(poolSampled) {
		t.Errorf("with n2 read but stale: %d unread, sampled %v; want one, n1's", res.GetNodesUnread(), res.GetSampleTime().AsTime())
	}

	setDefaultPlan(t, w.s, &tenancyv1alpha1.Limits{Memory: proto.String("8Gi")})
	res, err = w.memoryPoolUsage(t, "alice@example.com", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if res.SizeBytes != nil || len(res.GetMachines()) != 0 || res.GetOther() != nil || res.GetNodesUnread() != 0 {
		t.Errorf("a tenant with no pool: %v, want nothing", res)
	}
}
