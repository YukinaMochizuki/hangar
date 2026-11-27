package apiserver

import (
	"context"
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
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
)

// cpuPoolNode is a node that keeps CPU pool acme as given; with sampled
// zero, its members have no sample lately.
type cpuPoolNode struct {
	nodev1connect.UnimplementedNodeServiceHandler
	slowed   float64
	milliCPU int64
	minutes  []*nodev1.CPUPoolMinute
	sampled  time.Time
	members  []*nodev1.CPUPoolMember
}

func (n cpuPoolNode) CPUPoolUsage(_ context.Context, req *connect.Request[nodev1.CPUPoolUsageRequest]) (*connect.Response[nodev1.CPUPoolUsageResponse], error) {
	if req.Msg.GetPool() != "acme" {
		return connect.NewResponse(&nodev1.CPUPoolUsageResponse{}), nil
	}
	res := &nodev1.CPUPoolUsageResponse{Slowed: proto.Float64(n.slowed), MilliCpu: proto.Int64(n.milliCPU), Minutes: n.minutes, Members: n.members}
	if !n.sampled.IsZero() {
		res.SampleTime = timestamppb.New(n.sampled)
	}
	return connect.NewResponse(res), nil
}

// newCPUPoolWorld is a usage world whose tenant acme shares 4 CPUs as a
// pool, frank viewing its project web alone, with, running in the pool:
// web's container api, 1 vCPU, on n1; web's VM vm1, 2 vCPUs, on n1; and
// project db's container pg, 1 vCPU, on n2. Web's stopped container old
// and its FileShare's server, in no pool, are not in it. Bob owns tenant
// bobs, which has none. The uids are by name.
func newCPUPoolWorld(t *testing.T) *memoryPoolWorld {
	t.Helper()
	w := &memoryPoolWorld{execWorld: newUsageWorld(t, func(c *Config) { c.Nodes = append(c.Nodes, "n2") }), uids: map[string]string{}}
	setDefaultPlan(t, w.s, cpuPooledLimits("4", 200))
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
	bob := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: "bob@example.com"})
	if _, err := w.s.backend(registry.Tenant).Apply(bob, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "bobs"}}); err != nil {
		t.Fatal(err)
	}
	pool := &computev1alpha1.CPUPool{Name: "acme", Cpu: "4"}
	cur, err := w.s.backend(registry.Machine).Get(system, resource.Key{Project: "web", Name: machineName("api")})
	if err != nil {
		t.Fatal(err)
	}
	api := cur.(*computev1alpha1.Machine)
	api.Spec.Resources.Cpu = "1"
	api.Spec.CpuPool = pool
	w.put(t, "api", api, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING)
	if _, err := w.s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	vm1 := vmFromImage("vm1", "noble")
	pg, old := ctr("pg"), ctr("old")
	pg.Metadata.Project = "db"
	w.put(t, "vm1", cpuPoolMachineOf("web", w.owner(t, registry.VirtualMachine, vm1), "n1", "2", pool), computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING)
	w.put(t, "pg", cpuPoolMachineOf("db", w.owner(t, registry.Container, pg), "n2", "1", pool), computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING)
	w.put(t, "old", cpuPoolMachineOf("web", w.owner(t, registry.Container, old), "n1", "1", pool), computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED)
	w.put(t, "share", cpuPoolMachineOf("web", w.owner(t, registry.Container, ctr("files")), "n1", "1", nil), computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING)
	return w
}

// cpuPoolMachineOf is a Machine in project run by owner, of cpu vCPUs, in
// pool if set.
func cpuPoolMachineOf(project string, owner *metav1.OwnerReference, node, cpu string, pool *computev1alpha1.CPUPool) *computev1alpha1.Machine {
	m := machine(owner.GetName()+"-m", node)
	m.Metadata.Project = project
	m.Metadata.OwnerReferences = []*metav1.OwnerReference{owner}
	m.Spec.Resources = &computev1alpha1.Resources{Cpu: cpu, Memory: "1Gi"}
	m.Spec.CpuPool = pool
	return m
}

var cpuSampled = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

// minute is the minute i minutes after cpuSampled's, slowed share.
func minute(i int, share float64) *nodev1.CPUPoolMinute {
	return &nodev1.CPUPoolMinute{Time: timestamppb.New(cpuSampled.Add(time.Duration(i) * time.Minute)), Slowed: share}
}

// connectBothCPU serves n1, sampled a second after n2, slowed 0.25 of
// 4000 millicores, with api using 300 millicores and vm1 1500, and old,
// though stopped; and n2 slowed 0.5 of 2000, with pg using 800.
func (w *memoryPoolWorld) connectBothCPU(t *testing.T) {
	t.Helper()
	w.connectNodeAs(t, "n1", cpuPoolNode{slowed: 0.25, milliCPU: 4000, sampled: cpuSampled.Add(time.Second),
		minutes: []*nodev1.CPUPoolMinute{minute(-2, 0.1), minute(-1, 0.6)},
		members: []*nodev1.CPUPoolMember{
			{MachineUid: w.uids["api"], MilliCpu: proto.Int64(300)},
			{MachineUid: w.uids["vm1"], MilliCpu: proto.Int64(1500)},
			{MachineUid: w.uids["old"], MilliCpu: proto.Int64(900)},
		}})
	w.connectNodeAs(t, "n2", cpuPoolNode{slowed: 0.5, milliCPU: 2000, sampled: cpuSampled,
		minutes: []*nodev1.CPUPoolMinute{minute(-3, 0.2), minute(-2, 0.3), minute(-1, 0.4)},
		members: []*nodev1.CPUPoolMember{{MachineUid: w.uids["pg"], MilliCpu: proto.Int64(800)}}})
}

func cpuPoolNames(res *tenancyv1alpha1.CPUPoolUsageResponse) []string {
	var out []string
	for _, m := range res.GetMachines() {
		out = append(out, m.GetProject()+"/"+m.GetName())
	}
	return out
}

// TestCPUPoolUsageNamesAllToTheTenantsOwner: the tenant's owner sees every
// running machine in the pool by what it uses now, largest first, with
// its kind and vCPUs; the pool's share and CPUs are the largest of its
// nodes', and each minute's the largest of the nodes that counted it; the
// oldest of the nodes' samples is when it was sampled.
func TestCPUPoolUsageNamesAllToTheTenantsOwner(t *testing.T) {
	t.Parallel()
	w := newCPUPoolWorld(t)
	w.connectBothCPU(t)
	res, err := w.cpuPoolUsage(t, "alice@example.com", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if res.GetSlowed() != 0.5 || res.GetMilliCpu() != 4000 || res.GetNodesUnread() != 0 || res.GetOther() != nil || !res.GetSampleTime().AsTime().Equal(cpuSampled) {
		t.Errorf("the pool: %v; want n2's 0.5, n1's 4000 millicores, every node read, nothing other, sampled %v", res, cpuSampled)
	}
	want := []struct {
		at     int
		slowed float64
	}{{-3, 0.2}, {-2, 0.3}, {-1, 0.6}}
	if got := res.GetMinutes(); len(got) != len(want) {
		t.Errorf("minutes %v, want %v", got, want)
	} else {
		for i, m := range want {
			if !got[i].GetTime().AsTime().Equal(cpuSampled.Add(time.Duration(m.at)*time.Minute)) || got[i].GetSlowed() != m.slowed {
				t.Errorf("minute %d: %v, want %+v", i, got[i], m)
			}
		}
	}
	if got := cpuPoolNames(res); len(got) != 3 || got[0] != "web/vm1" || got[1] != "db/pg" || got[2] != "web/api" {
		t.Fatalf("machines %v, want web/vm1, db/pg, web/api", got)
	}
	vm1, api := res.GetMachines()[0], res.GetMachines()[2]
	if vm1.GetKind() != "VirtualMachine" || vm1.GetVcpus() != 2 || vm1.GetCpuNowMilli() != 1500 || vm1.GetPhase() != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING {
		t.Errorf("vm1: %v, want a running VirtualMachine of 2 vCPUs at 1500 millicores", vm1)
	}
	if api.GetKind() != "Container" || api.GetVcpus() != 1 || api.GetCpuNowMilli() != 300 {
		t.Errorf("api: %v, want a Container of 1 vCPU at 300 millicores", api)
	}

	// The largest share, whichever node is asked last.
	w.connectNodeAs(t, "n2", cpuPoolNode{slowed: 0.1, milliCPU: 2000, sampled: cpuSampled})
	res, err = w.cpuPoolUsage(t, "alice@example.com", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if res.GetSlowed() != 0.25 {
		t.Errorf("slowed %v with n2 at 0.1, want n1's 0.25", res.GetSlowed())
	}
}

// TestCPUPoolUsageFoldsWhatTheCallerMayNotRead: one who views a single
// project sees its machines by name and the rest of the pool's summed into
// other, and the pool's share and minutes whole; its sample time and nodes
// unread are of the nodes its named machines are on.
func TestCPUPoolUsageFoldsWhatTheCallerMayNotRead(t *testing.T) {
	t.Parallel()
	w := newCPUPoolWorld(t)
	w.connectBothCPU(t)
	res, err := w.cpuPoolUsage(t, "frank@example.com", "acme")
	if err != nil {
		t.Fatalf("one project's viewer: %v", err)
	}
	if got := cpuPoolNames(res); len(got) != 2 || got[0] != "web/vm1" || got[1] != "web/api" {
		t.Errorf("machines %v, want web's alone", got)
	}
	if res.GetOther().GetCpuNowMilli() != 800 {
		t.Errorf("other %v, want pg's 800 millicores", res.GetOther())
	}
	if res.GetSlowed() != 0.5 || len(res.GetMinutes()) != 3 || res.GetMilliCpu() != 4000 {
		t.Errorf("the pool: slowed %v, %d minutes, %d millicores; want the whole pool's", res.GetSlowed(), len(res.GetMinutes()), res.GetMilliCpu())
	}
	// Of n1 alone, which web's machines are on.
	if !res.GetSampleTime().AsTime().Equal(cpuSampled.Add(time.Second)) || res.GetNodesUnread() != 0 {
		t.Errorf("sampled %v, %d unread; want n1's %v, none", res.GetSampleTime().AsTime(), res.GetNodesUnread(), cpuSampled.Add(time.Second))
	}

	// Another of db's machines: other sums them.
	cron := ctr("cron")
	cron.Metadata.Project = "db"
	w.put(t, "cron", cpuPoolMachineOf("db", w.owner(t, registry.Container, cron), "n2", "1", &computev1alpha1.CPUPool{Name: "acme", Cpu: "4"}), computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING)
	w.connectNodeAs(t, "n2", cpuPoolNode{slowed: 0.5, milliCPU: 2000, sampled: cpuSampled, members: []*nodev1.CPUPoolMember{
		{MachineUid: w.uids["pg"], MilliCpu: proto.Int64(800)}, {MachineUid: w.uids["cron"], MilliCpu: proto.Int64(200)},
	}})
	res, err = w.cpuPoolUsage(t, "frank@example.com", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if res.GetOther().GetCpuNowMilli() != 1000 {
		t.Errorf("other %v, want pg's and cron's 1000 millicores", res.GetOther())
	}

	// n2 with no sample lately: db's machines count nothing, and n2 is frank's to
	// count as unread no more than it is to know.
	w.connectNodeAs(t, "n2", cpuPoolNode{slowed: 0.5, milliCPU: 2000, members: []*nodev1.CPUPoolMember{{MachineUid: w.uids["pg"]}}})
	res, err = w.cpuPoolUsage(t, "frank@example.com", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if res.GetOther() == nil || res.GetOther().GetCpuNowMilli() != 0 || res.GetNodesUnread() != 0 {
		t.Errorf("with n2 stale: other %v, %d unread; want pg counted at nothing, none unread", res.GetOther(), res.GetNodesUnread())
	}
	res, err = w.cpuPoolUsage(t, "alice@example.com", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if res.GetNodesUnread() != 1 || !res.GetSampleTime().AsTime().Equal(cpuSampled.Add(time.Second)) || res.GetMachines()[2].CpuNowMilli != nil {
		t.Errorf("the owner, with n2 stale: %v; want one unread, n1's time, pg with nothing of now", res)
	}
}

// TestCPUPoolUsageIsAskedAsTheMemoryPools: one who may get the tenant's
// containers in a single project, or across it without the tenant's own
// get, may ask; one who may get none of them may not, nor anyone of
// another tenant, nor anyone of a tenant that does not exist but one over
// every tenant, who is told it does not exist. A tenant with no CPU pool
// answers nothing.
func TestCPUPoolUsageIsAskedAsTheMemoryPools(t *testing.T) {
	t.Parallel()
	w := newCPUPoolWorld(t)
	w.connectBothCPU(t)
	for _, user := range []string{"alice@example.com", "frank@example.com", "dave@example.com"} {
		res, err := w.cpuPoolUsage(t, user, "acme")
		if err != nil {
			t.Errorf("%s: %v", user, err)
			continue
		}
		if res.GetSlowed() != 0.5 {
			t.Errorf("%s: slowed %v, want 0.5", user, res.GetSlowed())
		}
	}
	for _, user := range []string{"bob@example.com", "erin@example.com"} {
		_, err := w.cpuPoolUsage(t, user, "acme")
		wantCodeOf(t, err, connect.CodePermissionDenied, user+" reading acme's CPU pool")
	}
	_, err := w.cpuPoolUsage(t, "alice@example.com", "bobs")
	wantCodeOf(t, err, connect.CodePermissionDenied, "alice reading another tenant's pool")
	_, err = w.cpuPoolUsage(t, "frank@example.com", "absent")
	wantCodeOf(t, err, connect.CodePermissionDenied, "reading the pool of a tenant that does not exist")
	asRoot(t, w.s, auth.WithPrincipal(context.Background(), auth.SystemPrincipal))
	_, err = w.cpuPoolUsage(t, "root@example.com", "absent")
	wantCodeOf(t, err, connect.CodeNotFound, "root reading the pool of a tenant that does not exist")

	setDefaultPlan(t, w.s, &tenancyv1alpha1.Limits{Cpu: proto.String("4")})
	res, err := w.cpuPoolUsage(t, "alice@example.com", "acme")
	if err != nil {
		t.Fatalf("a tenant with no pool: %v", err)
	}
	if res.Slowed != nil || res.MilliCpu != nil || len(res.GetMachines()) != 0 || len(res.GetMinutes()) != 0 || res.GetOther() != nil || res.GetNodesUnread() != 0 {
		t.Errorf("a tenant with no pool: %v, want nothing", res)
	}
}

// TestCPUPoolUsageCountsNodesUnreadAndKeepsAnHour: a node not connected
// is counted, its machines listed with nothing of now; of more than 60
// minutes across the nodes, the last 60 are answered.
func TestCPUPoolUsageCountsNodesUnreadAndKeepsAnHour(t *testing.T) {
	t.Parallel()
	w := newCPUPoolWorld(t)
	var n1, n2 []*nodev1.CPUPoolMinute
	for i := range 60 {
		n1 = append(n1, minute(i-70, 0.1))
		n2 = append(n2, minute(i-60, 0.2))
	}
	w.connectNodeAs(t, "n1", cpuPoolNode{slowed: 0.1, milliCPU: 4000, sampled: cpuSampled, minutes: n1})
	res, err := w.cpuPoolUsage(t, "alice@example.com", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if res.GetNodesUnread() != 1 || len(res.GetMachines()) != 3 || res.GetMachines()[2].CpuNowMilli != nil {
		t.Errorf("with n2 not connected: %v, want one node unread and pg with nothing of now", res)
	}
	w.connectNodeAs(t, "n2", cpuPoolNode{slowed: 0.2, milliCPU: 4000, sampled: cpuSampled, minutes: n2})
	res, err = w.cpuPoolUsage(t, "alice@example.com", "acme")
	if err != nil {
		t.Fatal(err)
	}
	got := res.GetMinutes()
	if len(got) != 60 || !got[0].GetTime().AsTime().Equal(cpuSampled.Add(-60*time.Minute)) || !got[59].GetTime().AsTime().Equal(cpuSampled.Add(-time.Minute)) {
		t.Fatalf("%d minutes, want the last 60, 11:00 to 11:59", len(got))
	}
	if got[0].GetSlowed() != 0.2 {
		t.Errorf("11:00 slowed %v, want n2's 0.2 over n1's 0.1", got[0].GetSlowed())
	}
}
