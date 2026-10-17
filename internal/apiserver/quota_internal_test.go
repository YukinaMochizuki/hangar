package apiserver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// setDefaultPlan gives the default plan limits, as an administrator does.
func setDefaultPlan(t *testing.T, s *Server, l *tenancyv1alpha1.Limits) {
	t.Helper()
	if _, err := s.backend(registry.Plan).Apply(s.system(context.Background()), &tenancyv1alpha1.Plan{
		Metadata: &metav1.ObjectMeta{Name: DefaultPlan}, Spec: &tenancyv1alpha1.PlanSpec{Limits: l},
	}); err != nil {
		t.Fatal(err)
	}
}

// wantExhausted checks err refuses for quota, with a message holding want.
func wantExhausted(t *testing.T, err error, want, what string) {
	t.Helper()
	if connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: got %v, want resource_exhausted saying %q", what, err, want)
	}
}

func quotaCtr(name, cpu, memory, disk string) *computev1alpha1.Container {
	c := ctr(name)
	c.Spec.Resources = &computev1alpha1.Resources{Cpu: cpu, Memory: memory, Disk: disk}
	return c
}

func stopped(c *computev1alpha1.Container) *computev1alpha1.Container {
	c = proto.Clone(c).(*computev1alpha1.Container)
	c.Spec.RunState = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED
	return c
}

// askedFor is what a machine with resources r is asked for, counted.
func askedFor(t *testing.T, r *computev1alpha1.Resources) amounts {
	t.Helper()
	var a amounts
	a[dimMachines] = 1
	cpu, err := milliCPU(r.GetCpu())
	if err != nil {
		t.Fatal(err)
	}
	h, err := heldBy(r)
	if err != nil {
		t.Fatal(err)
	}
	a[dimCPU], a[dimMemory], a[dimDisk] = cpu, int64(h.memory), int64(h.disk)
	return a
}

// put stores obj as it is, past admission, as a test of what is counted
// needs.
func put(t *testing.T, s *Server, obj proto.Message) {
	t.Helper()
	data, err := encode(obj)
	if err != nil {
		t.Fatal(err)
	}
	meta := obj.ProtoReflect().Descriptor().Name()
	key := store.Key{Kind: string(meta), Project: metaOf(obj).GetProject(), Name: metaOf(obj).GetName()}
	if _, err := s.store.Create(context.Background(), key, data); err != nil {
		t.Fatal(err)
	}
}

func metaOf(obj proto.Message) *metav1.ObjectMeta {
	return obj.ProtoReflect().Get(obj.ProtoReflect().Descriptor().Fields().ByName("metadata")).Message().Interface().(*metav1.ObjectMeta)
}

func quotaVM(name, uid, size string) *computev1alpha1.VirtualMachine {
	return &computev1alpha1.VirtualMachine{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name, Uid: uid},
		Spec: &computev1alpha1.VirtualMachineSpec{RootDisk: &computev1alpha1.VirtualMachineRootDisk{
			Source: &computev1alpha1.VirtualMachineRootDisk_FromImage{FromImage: &computev1alpha1.VirtualMachineImageDisk{
				Image: &storagev1alpha1.ImageSource{Name: "ubuntu"}, Size: size,
			}},
		}},
	}
}

func quotaDisk(name, size string, annotations map[string]string) *storagev1alpha1.Disk {
	return &storagev1alpha1.Disk{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name, Annotations: annotations},
		Spec:     &storagev1alpha1.DiskSpec{Size: size},
	}
}

// TestQuotaCountsWhatTheNodeIsAskedFor: a machine counts the sizes its
// controller gives it, defaults and rounding included, and a VM's root disk
// made from an image counts once.
func TestQuotaCountsWhatTheNodeIsAskedFor(t *testing.T) {
	for _, c := range []*computev1alpha1.Container{ctr("defaults"), quotaCtr("odd", "2", "1G", "1.5Gi")} {
		want, err := machineSpec(c.GetSpec())
		if err != nil {
			t.Fatal(err)
		}
		got := containerAmounts(c, nil)
		if got != askedFor(t, want.GetResources()) {
			t.Errorf("container %s counts %v, its machine is asked for %v", c.GetMetadata().GetName(), got, want.GetResources())
		}
		if s := containerAmounts(stopped(c), nil); s[dimCPU] != 0 || s[dimMemory] != 0 || s[dimDisk] != got[dimDisk] || s[dimMachines] != 1 {
			t.Errorf("stopped container %s counts %v, want its disk and machine only", c.GetMetadata().GetName(), s)
		}
	}
	if got := containerAmounts(ctr("d"), nil); got[dimCPU] != 1000 || got[dimMemory] != int64(512*quantity.MiB) || got[dimDisk] != int64(10*quantity.GiB) {
		t.Errorf("a container with defaults counts %v, want 1 cpu, 512Mi, 10Gi", got)
	}

	vm := quotaVM("vm", "vm-uid", "")
	want, err := vmMachineSpec(vm, "")
	if err != nil {
		t.Fatal(err)
	}
	got := vmAmounts(vm, nil)
	asked := askedFor(t, want.GetResources())
	asked[dimDisk] = int64(10 * quantity.GiB)
	if got != asked {
		t.Errorf("a VM with defaults counts %v, its machine is asked for %v and a 10Gi root disk", got, want.GetResources())
	}

	s, _, _ := running(t)
	put(t, s, vm)
	put(t, s, quotaDisk(rootDiskName("vm"), "10Gi", map[string]string{RootDiskOf: "vm-uid"}))
	put(t, s, quotaDisk("data", "5Gi", nil))
	// A root disk retained by a VM that is gone is a disk like any other.
	put(t, s, quotaDisk("kept", "3Gi", map[string]string{RootDiskOf: "gone-uid"}))
	put(t, s, stopped(quotaCtr("c", "2", "1Gi", "2Gi")))
	tl, err := s.tally(context.Background(), "acme", nil)
	if err != nil {
		t.Fatal(err)
	}
	u := tl.tenant.usage()
	if u.GetCpu() != "1" || u.GetMemory() != "1Gi" || u.GetDisk() != "20Gi" || u.GetMachines() != 2 || u.GetProjects() != 1 || u.GetNetworks() != 1 {
		t.Errorf("acme uses %v, want cpu 1, memory 1Gi, disk 10+5+3+2Gi, 2 machines, 1 project, 1 network", u)
	}
}

func TestQuotaRefusesWhatWouldPassALimit(t *testing.T) {
	s, _, user := running(t)
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Cpu: proto.String("2500m"), Disk: proto.String("35Gi"), Machines: proto.Int32(3)})
	b := s.backend(registry.Container)
	for _, name := range []string{"c1", "c2"} {
		if _, err := b.Apply(user, ctr(name)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := b.Apply(user, ctr("c3"))
	wantExhausted(t, err, `tenant "acme"'s cpu limit is 2500m (plan "default") and it uses 2; this would bring it to 3`, "a third running container")

	// A stopped machine holds no cpu, so starting it is what is checked.
	if _, err := b.Apply(user, stopped(ctr("c2"))); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply(user, ctr("c3")); err != nil {
		t.Fatalf("a container in the cpu a stopped one freed: %v", err)
	}
	_, err = b.Apply(user, ctr("c2"))
	wantExhausted(t, err, "cpu limit is 2500m", "starting a stopped container past the cpu limit")

	_, err = b.Apply(user, stopped(quotaCtr("c4", "", "", "1Gi")))
	wantExhausted(t, err, `machines limit is 3 (plan "default") and it uses 3; this would bring it to 4`, "a fourth machine, even stopped")
	_, err = b.Apply(user, quotaCtr("c1", "", "", "16Gi"))
	wantExhausted(t, err, `disk limit is 35Gi (plan "default") and it uses 30Gi; this would bring it to 36Gi`, "growing a disk past the limit")

	// Over a lowered limit, what raises nothing still goes through.
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Cpu: proto.String("1"), Machines: proto.Int32(1)})
	if _, err := b.Apply(user, ctr("c1")); err != nil {
		t.Errorf("applying a container unchanged over a lowered limit: %v", err)
	}
	other := ctr("c1")
	other.Spec.Image = "redis"
	if _, err := b.Apply(user, other); err != nil {
		t.Errorf("changing a container's image over a lowered limit: %v", err)
	}
	if _, err := b.Apply(user, stopped(ctr("c3"))); err != nil {
		t.Errorf("stopping a container over a lowered limit: %v", err)
	}
	// The apiserver's own writes are counted but never refused.
	if _, err := s.backend(registry.Container).Apply(s.system(context.Background()), ctr("c5")); err != nil {
		t.Errorf("the system's write: %v", err)
	}
}

func TestQuotaCountsProjectsAndTheirDefaultNetworks(t *testing.T) {
	s, system, user := running(t)
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Projects: proto.Int32(2), Networks: proto.Int32(3), Addresses: proto.Int32(0)})
	proj := func(name string) *tenancyv1alpha1.Project {
		return &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: name}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "acme"}}
	}
	if _, err := s.backend(registry.Project).Apply(user, proj("web2")); err != nil {
		t.Fatal(err)
	}
	_, err := s.backend(registry.Project).Apply(user, proj("web3"))
	wantExhausted(t, err, `projects limit is 2 (plan "default") and it uses 2`, "a third project")
	// web's default network and web2's, which its controller is yet to make.
	if _, err := s.backend(registry.Network).Apply(user, &networkv1alpha1.Network{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: "back"}, Spec: &networkv1alpha1.NetworkSpec{Ipv4Cidr: "10.9.0.0/24"},
	}); err != nil {
		t.Fatal(err)
	}
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Projects: proto.Int32(5), Networks: proto.Int32(3), Addresses: proto.Int32(0)})
	_, err = s.backend(registry.Project).Apply(user, proj("web3"))
	wantExhausted(t, err, `networks limit is 3 (plan "default") and it uses 3; this would bring it to 4`, "a project whose default network would pass the limit")
	_, err = s.backend(registry.Address).Apply(user, &networkv1alpha1.Address{Metadata: &metav1.ObjectMeta{Project: "web", Name: "ip"}})
	wantExhausted(t, err, "addresses limit is 0", "an address")
	_ = system
}

// TestProjectLimits: a tenant owner caps a project within the tenant's
// limits, and a role on the project alone cannot change the cap.
func TestProjectLimits(t *testing.T) {
	s, system, user := runningWith(t, func(c *Config) {
		c.ExtraRoles = map[string]authz.Role{"test.project-admin": authz.NewRole(authz.ProjectLevel,
			authz.Permission(registry.Project, authz.Get), authz.Permission(registry.Project, authz.Apply))}
	})
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Machines: proto.Int32(5), Cpu: proto.String("4")})
	pb := s.backend(registry.Project)
	web := func(l *tenancyv1alpha1.Limits, display string) *tenancyv1alpha1.Project {
		return &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: "web"}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "acme", DisplayName: display, Limits: l}}
	}
	_, err := pb.Apply(user, web(&tenancyv1alpha1.Limits{Machines: proto.Int32(6)}, ""))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), `spec.limits.machines 6 is above tenant "acme"'s limit of 5`) {
		t.Errorf("a project limit above the tenant's: %v", err)
	}
	_, err = pb.Apply(user, web(&tenancyv1alpha1.Limits{Projects: proto.Int32(1)}, ""))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a project setting projects")
	_, err = pb.Apply(user, web(&tenancyv1alpha1.Limits{Cpu: proto.String("1x")}, ""))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a project limit that is not a quantity")
	if _, err := pb.Apply(user, web(&tenancyv1alpha1.Limits{Machines: proto.Int32(1)}, "")); err != nil {
		t.Fatalf("the tenant owner capping a project: %v", err)
	}

	cb := s.backend(registry.Container)
	if _, err := cb.Apply(user, ctr("c1")); err != nil {
		t.Fatal(err)
	}
	_, err = cb.Apply(user, ctr("c2"))
	wantExhausted(t, err, `project "web"'s machines limit is 1 (its spec.limits) and it uses 1; this would bring it to 2`, "a second machine in a capped project")

	addMember(t, s, "bob@example.com")
	if _, err := s.backend(registry.RoleBinding).Apply(system, &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: "bob-web"},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: "bob@example.com"}},
			Role:    "test.project-admin",
			Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Project{Project: "web"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	bob := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: "bob@example.com"})
	if _, err := pb.Apply(bob, web(&tenancyv1alpha1.Limits{Machines: proto.Int32(1)}, "Web")); err != nil {
		t.Errorf("a project admin applying the project with its limits unchanged: %v", err)
	}
	_, err = pb.Apply(bob, web(&tenancyv1alpha1.Limits{Machines: proto.Int32(2)}, "Web"))
	wantCodeOf(t, err, connect.CodePermissionDenied, "a project admin raising the project's own limit")
	_, err = pb.Apply(bob, web(nil, "Web"))
	wantCodeOf(t, err, connect.CodePermissionDenied, "a project admin clearing the project's own limit")
}

// TestProjectOwnerCannotRaiseItsLimits: the built-in project.owner writes
// its project but, holding nothing at the tenant, cannot change its limits.
func TestProjectOwnerCannotRaiseItsLimits(t *testing.T) {
	s, system, user := running(t)
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Machines: proto.Int32(5)})
	pb := s.backend(registry.Project)
	web := func(l *tenancyv1alpha1.Limits, display string) *tenancyv1alpha1.Project {
		return &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: "web"}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "acme", DisplayName: display, Limits: l}}
	}
	if _, err := pb.Apply(user, web(&tenancyv1alpha1.Limits{Machines: proto.Int32(1)}, "")); err != nil {
		t.Fatalf("the tenant owner capping a project: %v", err)
	}
	addMember(t, s, "carol@example.com")
	if _, err := s.backend(registry.RoleBinding).Apply(system, &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: "carol-web"},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: "carol@example.com"}},
			Role:    "project.owner",
			Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Project{Project: "web"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	carol := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: "carol@example.com"})
	if _, err := pb.Apply(carol, web(&tenancyv1alpha1.Limits{Machines: proto.Int32(1)}, "Web")); err != nil {
		t.Errorf("a project owner applying the project with its limits unchanged: %v", err)
	}
	_, err := pb.Apply(carol, web(&tenancyv1alpha1.Limits{Machines: proto.Int32(2)}, "Web"))
	wantCodeOf(t, err, connect.CodePermissionDenied, "a project owner raising the project's own limit")
	_, err = pb.Apply(carol, web(nil, "Web"))
	wantCodeOf(t, err, connect.CodePermissionDenied, "a project owner clearing the project's own limit")
}

// TestPublicEndpointsAreCounted: an endpoint counts one, and a container
// one for each public port, in place of the endpoint made for it, so a
// create past the limit is refused whichever way it would come.
func TestPublicEndpointsAreCounted(t *testing.T) {
	s, system, user := running(t)
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{PublicEndpoints: proto.Int32(2)})
	eb, cb := s.backend(registry.PublicEndpoint), s.backend(registry.Container)
	if _, err := eb.Apply(user, endpoint("site", "api", 80, private)); err != nil {
		t.Fatal(err)
	}
	api := ctr("api")
	api.Spec.Ports = []*computev1alpha1.ContainerPort{{Name: "http", Port: 8080, Protocol: computev1alpha1.Protocol_PROTOCOL_TCP, Public: true}}
	if _, err := cb.Apply(user, api); err != nil {
		t.Fatalf("a container with a public port, within the limit: %v", err)
	}
	// What the container endpoint controller does, as the system.
	if err := s.reconcileContainerEndpoints(system, controller.Key{Project: "web", Name: "api"}); err != nil {
		t.Fatal(err)
	}
	if obj, err := s.get(context.Background(), registry.PublicEndpoint, resource.Key{Project: "web", Name: "api-http"}); err != nil || obj == nil {
		t.Fatalf("the container's endpoint: %v, %v", obj, err)
	}
	tl, err := s.tally(context.Background(), "acme", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tl.tenant[dimPublicEndpoints]; got != 2 {
		t.Errorf("public endpoints counted %d, want 2: the container's endpoint once", got)
	}

	_, err = eb.Apply(user, endpoint("extra", "api", 81, public))
	wantExhausted(t, err, `public_endpoints limit is 2 (plan "default") and it uses 2; this would bring it to 3`, "an endpoint past the limit")
	more := proto.Clone(api).(*computev1alpha1.Container)
	more.Spec.Ports = append(more.Spec.Ports, &computev1alpha1.ContainerPort{Name: "admin", Port: 9090, Protocol: computev1alpha1.Protocol_PROTOCOL_TCP, Public: true})
	_, err = cb.Apply(user, more)
	wantExhausted(t, err, `public_endpoints limit is 2 (plan "default") and it uses 2; this would bring it to 3`, "a public port past the limit")
}

// TestEveryMachineKindCounts: every kind that runs as a machine counts
// toward quota, so a machine kind added to machineKinds cannot run free.
func TestEveryMachineKindCounts(t *testing.T) {
	for kind := range machineKinds {
		if _, ok := quotaCounters[kind]; !ok {
			t.Errorf("%s runs as machines but has no quota counter", kind)
		}
	}
}

// TestConcurrentCreatesCannotOvershootAQuota races creates against one
// limit: exactly as many win as it allows.
func TestConcurrentCreatesCannotOvershootAQuota(t *testing.T) {
	s, _, user := running(t)
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Machines: proto.Int32(3)})
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.backend(registry.Container).Apply(user, ctr(fmt.Sprintf("c%d", i)))
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch connect.CodeOf(err) {
		case connect.CodeResourceExhausted:
		default:
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				continue
			}
			won++
		}
	}
	if won != 3 {
		t.Errorf("%d of 10 concurrent creates won against a limit of 3", won)
	}
}

// TestConcurrentTenantsCannotOvershootTheCap races tenant creates by one
// person against their cap.
func TestConcurrentTenantsCannotOvershootTheCap(t *testing.T) {
	s, _, user := runningWith(t, func(c *Config) { c.DefaultMaxOwnedTenants = 4 })
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.backend(registry.Tenant).Apply(user, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: fmt.Sprintf("t%d", i)}})
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		if err == nil {
			won++
		} else if connect.CodeOf(err) != connect.CodeResourceExhausted {
			t.Errorf("unexpected error: %v", err)
		}
	}
	// alice already owns acme and the tenant signing up opened.
	if won != 2 {
		t.Errorf("%d of 8 concurrent tenants won against a cap of 4 with two owned", won)
	}
}

func TestOwnedTenantsAreCapped(t *testing.T) {
	s, system, user := runningWith(t, func(c *Config) { c.DefaultMaxOwnedTenants = 3 })
	tb := s.backend(registry.Tenant)
	newTenant := func(ctx context.Context, name string) error {
		_, err := tb.Apply(ctx, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: name}})
		return err
	}
	// alice already owns acme and the tenant signing up opened, which
	// counts as one she opened.
	if err := newTenant(user, "acme2"); err != nil {
		t.Fatal(err)
	}
	wantExhausted(t, newTenant(user, "acme3"), "user:alice@example.com owns 3 tenants and may own 3 (the platform default)", "a fourth tenant")

	// Her User can raise her own cap.
	ub := s.backend(registry.User)
	obj, err := ub.Get(system, resource.Key{Name: usernameOf("alice@example.com")})
	if err != nil {
		t.Fatal(err)
	}
	u := obj.(*iamv1alpha1.User)
	u.Spec.MaxOwnedTenants = proto.Int32(4)
	if _, err := ub.Apply(system, u); err != nil {
		t.Fatal(err)
	}
	if err := newTenant(user, "acme3"); err != nil {
		t.Fatalf("a fourth tenant under a raised cap: %v", err)
	}
	wantExhausted(t, newTenant(user, "acme4"), "owns 4 tenants and may own 4 (set on their User)", "a fifth tenant")
	// A tenant being deleted no longer counts.
	if _, err := tb.Delete(user, resource.Key{Name: "acme3"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := newTenant(user, "acme4"); err != nil {
		t.Errorf("a tenant in place of one being deleted: %v", err)
	}
	// Owning a tenant through a binding someone else made is not opening it.
	addMember(t, s, "bob@example.com")
	bob := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: "bob@example.com"})
	if _, err := s.backend(registry.RoleBinding).Apply(system, &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: "bob-acme"},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: "bob@example.com"}},
			Role:    "tenant.owner",
			Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: "acme"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bob1", "bob2"} {
		if err := newTenant(bob, name); err != nil {
			t.Fatalf("bob's tenant %s: %v", name, err)
		}
	}
	// The third is the one signing up opened.
	wantExhausted(t, newTenant(bob, "bob3"), "owns 3 tenants", "bob's fourth tenant")

	// A service token is capped by the platform default.
	token := serviceToken(t, s, system, "web", "tok")
	if _, err := s.backend(registry.RoleBinding).Apply(system, &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: "member-token"},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_ServiceToken{ServiceToken: "web/tok"}},
			Role:    "platform.member",
			Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tok1", "tok2", "tok3"} {
		if err := newTenant(token, name); err != nil {
			t.Fatalf("the token's tenant %s: %v", name, err)
		}
	}
	wantExhausted(t, newTenant(token, "tok4"), "owns 3 tenants and may own 3 (the platform default)", "the token's fourth tenant")

	// Platform root is not capped.
	invite(t, s, "root@example.com", "platform.root")
	signUp(t, s, "root@example.com")
	root := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: "root@example.com"})
	for _, name := range []string{"r1", "r2", "r3"} {
		if err := newTenant(root, name); err != nil {
			t.Errorf("root's tenant %s: %v", name, err)
		}
	}
}

func TestPlans(t *testing.T) {
	s, system, user := running(t)
	pb := s.backend(registry.Plan)
	plan := func(name string, l *tenancyv1alpha1.Limits) *tenancyv1alpha1.Plan {
		return &tenancyv1alpha1.Plan{Metadata: &metav1.ObjectMeta{Name: name}, Spec: &tenancyv1alpha1.PlanSpec{Limits: l}}
	}
	obj, err := pb.Get(system, resource.Key{Name: DefaultPlan})
	if err != nil {
		t.Fatal(err)
	}
	if got := obj.(*tenancyv1alpha1.Plan).GetSpec().GetLimits(); !proto.Equal(got, &tenancyv1alpha1.Limits{}) {
		t.Errorf("the default plan has %v, want the test config's", got)
	}
	_, err = pb.Apply(user, plan("mine", nil))
	wantCodeOf(t, err, connect.CodePermissionDenied, "a tenant owner making a plan")
	_, err = pb.Get(user, resource.Key{Name: DefaultPlan})
	wantCodeOf(t, err, connect.CodePermissionDenied, "a tenant owner reading a plan")
	for _, l := range []*tenancyv1alpha1.Limits{
		{Cpu: proto.String("1x")}, {Cpu: proto.String("-1")}, {Cpu: proto.String("0.0001")}, {Memory: proto.String("1.5")}, {Machines: proto.Int32(-1)},
	} {
		_, err := pb.Apply(system, plan("bad", l))
		wantCodeOf(t, err, connect.CodeInvalidArgument, fmt.Sprintf("a plan with %v", l))
	}
	if _, err := pb.Apply(system, plan("big", &tenancyv1alpha1.Limits{Cpu: proto.String("64")})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Tenant).Apply(system, &tenancyv1alpha1.Tenant{
		Metadata: &metav1.ObjectMeta{Name: "acme"}, Spec: &tenancyv1alpha1.TenantSpec{Plan: "big"},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = pb.Delete(system, resource.Key{Name: "big"}, nil)
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "deleting a plan a tenant has")
	_, err = pb.Delete(system, resource.Key{Name: DefaultPlan}, nil)
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "deleting the default plan")
	if _, err := pb.Apply(system, plan("spare", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := pb.Delete(system, resource.Key{Name: "spare"}, nil); err != nil {
		t.Errorf("deleting a plan no one has: %v", err)
	}
}

// waitUsage waits until the status of tenant acme and project web, as
// their owner reads them, satisfy ok.
func waitUsage(t *testing.T, s *Server, user context.Context, what string, ok func(*tenancyv1alpha1.TenantStatus, *tenancyv1alpha1.ProjectStatus) bool) {
	t.Helper()
	testutil.Eventually(t, what, func() (bool, string) {
		tobj, err := s.backend(registry.Tenant).Get(user, resource.Key{Name: "acme"})
		if err != nil {
			t.Fatal(err)
		}
		pobj, err := s.backend(registry.Project).Get(user, resource.Key{Name: "web"})
		if err != nil {
			t.Fatal(err)
		}
		ts, ps := tobj.(*tenancyv1alpha1.Tenant).GetStatus(), pobj.(*tenancyv1alpha1.Project).GetStatus()
		return ok(ts, ps), fmt.Sprintf("tenant status %v, project status %v", ts, ps)
	})
}

// TestUsageIsReported runs the controllers: the tenant's and project's
// status follow what they hold and the limits that hold for them, and the
// tenant's owner reads them.
func TestUsageIsReported(t *testing.T) {
	s, _, user := runningWith(t, func(c *Config) { c.DisableControllers = false })
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Cpu: proto.String("4"), Machines: proto.Int32(5)})
	if _, err := s.backend(registry.Container).Apply(user, ctr("c1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Network).Apply(user, &networkv1alpha1.Network{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: "back"}, Spec: &networkv1alpha1.NetworkSpec{Ipv4Cidr: "10.9.0.0/24"},
	}); err != nil {
		t.Fatal(err)
	}
	wantUsage := &tenancyv1alpha1.Usage{Cpu: "1", Memory: "512Mi", Disk: "10Gi", Machines: 1, Projects: 1, Networks: 2}
	planLimits := &tenancyv1alpha1.Limits{Cpu: proto.String("4"), Machines: proto.Int32(5)}
	waitUsage(t, s, user, "a new container and network", func(ts *tenancyv1alpha1.TenantStatus, ps *tenancyv1alpha1.ProjectStatus) bool {
		return ts.GetPlan() == DefaultPlan && proto.Equal(ts.GetLimits(), planLimits) && proto.Equal(ts.GetUsage(), wantUsage) &&
			proto.Equal(ps.GetLimits(), planLimits) && proto.Equal(ps.GetUsage(), wantUsage)
	})

	// A spec change: stopped, the container holds no cpu or memory.
	if _, err := s.backend(registry.Container).Apply(user, stopped(ctr("c1"))); err != nil {
		t.Fatal(err)
	}
	waitUsage(t, s, user, "a stopped container", func(ts *tenancyv1alpha1.TenantStatus, _ *tenancyv1alpha1.ProjectStatus) bool {
		return ts.GetUsage().GetCpu() == "0" && ts.GetUsage().GetMemory() == "0" && ts.GetUsage().GetDisk() == "10Gi"
	})

	// The project's own limits, and the plan's changing.
	if _, err := s.backend(registry.Project).Apply(user, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "web"},
		Spec:     &tenancyv1alpha1.ProjectSpec{Tenant: "acme", Limits: &tenancyv1alpha1.Limits{Machines: proto.Int32(2), Cpu: proto.String("1500m")}},
	}); err != nil {
		t.Fatal(err)
	}
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Cpu: proto.String("8"), Machines: proto.Int32(1), Memory: proto.String("2Gi")})
	wantProject := &tenancyv1alpha1.Limits{Cpu: proto.String("1500m"), Machines: proto.Int32(1), Memory: proto.String("2Gi")}
	waitUsage(t, s, user, "project limits and a changed plan", func(ts *tenancyv1alpha1.TenantStatus, ps *tenancyv1alpha1.ProjectStatus) bool {
		return ts.GetLimits().GetCpu() == "8" && proto.Equal(ps.GetLimits(), wantProject)
	})

	// A Disk, which only the Disk watch sees.
	if _, err := s.backend(registry.Disk).Apply(user, quotaDisk("data", "5Gi", nil)); err != nil {
		t.Fatal(err)
	}
	waitUsage(t, s, user, "a new disk", func(ts *tenancyv1alpha1.TenantStatus, _ *tenancyv1alpha1.ProjectStatus) bool {
		return ts.GetUsage().GetDisk() == "15Gi"
	})

	// A deletion.
	if _, err := s.backend(registry.Network).Delete(user, resource.Key{Project: "web", Name: "back"}, nil); err != nil {
		t.Fatal(err)
	}
	waitUsage(t, s, user, "a deleted network", func(ts *tenancyv1alpha1.TenantStatus, ps *tenancyv1alpha1.ProjectStatus) bool {
		return ts.GetUsage().GetNetworks() == 1 && ps.GetUsage().GetNetworks() == 1
	})
}
