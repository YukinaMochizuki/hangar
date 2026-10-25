package apiserver

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// vmWithRootDisk has the user apply a VM that makes its root disk of size
// from an image, and makes that disk, bound at the size, as the VM
// controller and the scheduler do.
func vmWithRootDisk(t *testing.T, s *Server, system, user context.Context, size string) *computev1alpha1.VirtualMachine {
	t.Helper()
	if _, err := s.backend(registry.Image).Apply(system, image("ubuntu")); err != nil {
		t.Fatal(err)
	}
	res, err := s.backend(registry.VirtualMachine).Apply(user, quotaVM("vm", "", size))
	if err != nil {
		t.Fatal(err)
	}
	vm := res.Object.(*computev1alpha1.VirtualMachine)
	root := imageDisk(vm)
	root.Metadata.Annotations = map[string]string{RootDiskOf: vm.GetMetadata().GetUid()}
	root.Metadata.OwnerReferences = []*metav1.OwnerReference{vmRef(vm)}
	if _, err := s.backend(registry.Disk).Create(system, root); err != nil {
		t.Fatal(err)
	}
	bindDisk(t, s, system, rootDiskName("vm"), "n1", size)
	return vm
}

func diskUsage(t *testing.T, s *Server) string {
	t.Helper()
	tl, err := s.tally(context.Background(), "acme", nil)
	if err != nil {
		t.Fatal(err)
	}
	return tl.tenant.usage().GetDisk()
}

// TestAVMRootDiskCountsWhatItHolds: once a VM's root disk exists, the VM
// counts the larger of its own size and the disk's, so shrinking the VM's
// size frees nothing the disk still holds, and growing the disk itself is
// checked.
func TestAVMRootDiskCountsWhatItHolds(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Disk: proto.String("15Gi")})
	vm := vmWithRootDisk(t, s, system, user, "14Gi")
	if got := diskUsage(t, s); got != "14Gi" {
		t.Fatalf("a VM with a 14Gi root disk uses %s", got)
	}

	// Whatever admission says of a smaller size, the disk holds 14Gi.
	_, _ = s.backend(registry.VirtualMachine).Apply(user, quotaVM("vm", vm.GetMetadata().GetUid(), "4Gi"))
	if got := diskUsage(t, s); got != "14Gi" {
		t.Errorf("after asking the VM for a 4Gi root disk, acme uses %s, want the 14Gi the disk holds", got)
	}
	_, err := s.backend(registry.Disk).Apply(user, quotaDisk("data", "10Gi", nil))
	wantExhausted(t, err, `disk limit is 15Gi (plan "default") and it uses 14Gi; this would bring it to 24Gi`, "a data disk past what the root disk holds")

	// Growing the root disk itself, sending back what get returned.
	obj, err := s.backend(registry.Disk).Get(user, resource.Key{Project: "web", Name: rootDiskName("vm")})
	if err != nil {
		t.Fatal(err)
	}
	d := obj.(*storagev1alpha1.Disk)
	d.Spec.Size = "100Gi"
	_, err = s.backend(registry.Disk).Apply(user, d)
	wantExhausted(t, err, "this would bring it to 100Gi", "growing a VM's root disk directly")
	// And from a manifest that carries no uid or annotation.
	_, err = s.backend(registry.Disk).Apply(user, diskOf(rootDiskName("vm"), "100Gi", "ubuntu"))
	wantExhausted(t, err, "this would bring it to 100Gi", "growing a VM's root disk from a manifest")
	// Growing the VM's own size is checked as well.
	_, err = s.backend(registry.VirtualMachine).Apply(user, quotaVM("vm", "", "16Gi"))
	wantExhausted(t, err, "this would bring it to 16Gi", "growing a VM's root disk through the VM")
	if got := diskUsage(t, s); got != "14Gi" {
		t.Errorf("after the refusals acme uses %s", got)
	}
}

// TestABorrowedUIDCountsForNothing: a new VM carrying the uid a root disk
// names, whether a retained disk's gone VM or a live VM that now boots from
// a named disk, counts as created, with a uid of its own, so the disk still
// counts and the VM is refused past the limit.
func TestABorrowedUIDCountsForNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// setup stores the disk whose RootDiskOf names uid, and what else
		// goes with it.
		setup func(t *testing.T, s *Server, uid string)
	}{
		{"a retained disk's", func(t *testing.T, s *Server, uid string) {
			put(t, s, diskOfRoot("kept", "20Gi", uid))
		}},
		{"a live VM's", func(t *testing.T, s *Server, uid string) {
			put(t, s, diskOfRoot("a-data", "20Gi", uid))
			live := &computev1alpha1.VirtualMachine{
				Metadata: &metav1.ObjectMeta{Project: "web", Name: "a", Uid: uid},
				Spec: &computev1alpha1.VirtualMachineSpec{
					RootDisk: &computev1alpha1.VirtualMachineRootDisk{Source: &computev1alpha1.VirtualMachineRootDisk_Disk{Disk: "a-data"}},
					RunState: computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED,
				},
			}
			put(t, s, live)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, system, user := running(t)
			if _, err := s.backend(registry.Image).Apply(system, image("ubuntu")); err != nil {
				t.Fatal(err)
			}
			setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Disk: proto.String("25Gi")})
			const uid = "01a13f45-0000-7000-8000-000000000001"
			tc.setup(t, s, uid)
			if got := diskUsage(t, s); got != "20Gi" {
				t.Fatalf("acme uses %s before, want 20Gi", got)
			}
			vb := s.backend(registry.VirtualMachine)
			if _, err := vb.Apply(user, quotaVM("b1", uid, "5Gi")); err != nil {
				t.Fatalf("a VM within the limit: %v", err)
			}
			for _, name := range []string{"b2", "b3"} {
				_, err := vb.Apply(user, quotaVM(name, uid, "5Gi"))
				wantExhausted(t, err, `disk limit is 25Gi (plan "default") and it uses 25Gi; this would bring it to 30Gi`, "a VM with a borrowed uid past the limit")
			}
			if got := diskUsage(t, s); got != "25Gi" {
				t.Errorf("acme uses %s, want 25Gi", got)
			}
		})
	}
}

// diskOfRoot is a root disk made from an image for the VM with uid.
func diskOfRoot(name, size, uid string) *storagev1alpha1.Disk {
	d := diskOf(name, size, "ubuntu")
	d.Metadata.Annotations = map[string]string{RootDiskOf: uid}
	return d
}

// TestApplyingAManifestAgainIsNotRefused: a VM and its root disk applied
// again, unchanged, from manifests without a uid or the reserved
// annotation, count as they are stored, so a tenant at its limit is not
// refused.
func TestApplyingAManifestAgainIsNotRefused(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Disk: proto.String("10Gi")})
	vmWithRootDisk(t, s, system, user, "10Gi")
	if _, err := s.backend(registry.VirtualMachine).Apply(user, quotaVM("vm", "", "10Gi")); err != nil {
		t.Errorf("applying the VM's manifest again: %v", err)
	}
	if _, err := s.backend(registry.Disk).Apply(user, diskOf(rootDiskName("vm"), "10Gi", "ubuntu")); err != nil {
		t.Errorf("applying the root disk's manifest again: %v", err)
	}
	if got := diskUsage(t, s); got != "10Gi" {
		t.Errorf("acme uses %s, want 10Gi", got)
	}
}

// TestVMsAndDisksAreCheckedThroughApply: a VM's root disk, its cpu when it
// starts, and a Disk are each checked when applied.
func TestVMsAndDisksAreCheckedThroughApply(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	if _, err := s.backend(registry.Image).Apply(system, image("ubuntu")); err != nil {
		t.Fatal(err)
	}
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Disk: proto.String("20Gi"), Cpu: proto.String("1"), Machines: proto.Int32(2)})
	vb, db := s.backend(registry.VirtualMachine), s.backend(registry.Disk)
	_, err := vb.Apply(user, quotaVM("big", "", "21Gi"))
	wantExhausted(t, err, "disk limit is 20Gi", "a VM whose root disk passes the limit")
	// Without a size, a root disk made from an image is 10Gi.
	if _, err := vb.Apply(user, quotaVM("vm1", "", "")); err != nil {
		t.Fatal(err)
	}
	stoppedVM := quotaVM("vm2", "", "5Gi")
	stoppedVM.Spec.RunState = computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED
	if _, err := vb.Apply(user, stoppedVM); err != nil {
		t.Fatalf("a stopped VM within the disk limit: %v", err)
	}
	_, err = vb.Apply(user, quotaVM("vm2", "", "5Gi"))
	wantExhausted(t, err, `cpu limit is 1 (plan "default") and it uses 1; this would bring it to 2`, "starting a VM past the cpu limit")
	_, err = db.Apply(user, quotaDisk("data", "6Gi", nil))
	wantExhausted(t, err, "disk limit is 20Gi (plan \"default\") and it uses 15Gi; this would bring it to 21Gi", "a disk past the limit")
	if _, err := db.Apply(user, quotaDisk("data", "5Gi", nil)); err != nil {
		t.Fatalf("a disk within the limit: %v", err)
	}
	_, err = db.Apply(user, quotaDisk("data", "6Gi", nil))
	wantExhausted(t, err, "this would bring it to 21Gi", "growing a disk past the limit")
}

// TestTheTenantCapIgnoresBindings: the cap counts the tenants someone
// opened, as recorded on each tenant, so changing or deleting the owner
// binding made with it frees nothing; and no one but the apiserver can
// write that record.
func TestTheTenantCapIgnoresBindings(t *testing.T) {
	t.Parallel()
	// alice opens two: the tenant signing up opened, and acme.
	s, system, user := runningWith(t, func(c *Config) { c.DefaultMaxOwnedTenants = 2 })
	tb, rbb := s.backend(registry.Tenant), s.backend(registry.RoleBinding)
	newTenant := func(name string) error {
		_, err := tb.Apply(user, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: name}})
		return err
	}
	ownerAgain := func(name, tenant string) {
		t.Helper()
		if _, err := rbb.Apply(user, &iamv1alpha1.RoleBinding{
			Metadata: &metav1.ObjectMeta{Name: name},
			Spec: &iamv1alpha1.RoleBindingSpec{
				Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: "alice@example.com"}},
				Role:    "tenant.owner",
				Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: tenant}},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	autoBinding := func(tenant string) *iamv1alpha1.RoleBinding {
		t.Helper()
		obj, err := tb.Get(user, resource.Key{Name: tenant})
		if err != nil {
			t.Fatal(err)
		}
		o, err := rbb.Get(user, resource.Key{Name: ownerBindingName(tenant, obj.(*tenancyv1alpha1.Tenant).GetMetadata().GetUid())})
		if err != nil {
			t.Fatal(err)
		}
		return o.(*iamv1alpha1.RoleBinding)
	}
	wantExhausted(t, newTenant("acme2"), "owns 2 tenants and may own 2", "a third tenant")

	// Re-subjecting the owner binding made with acme.
	ownerAgain("alice-again", "acme")
	rb := autoBinding("acme")
	rb.Spec.Subject = &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: "nobody@example.com"}}
	if _, err := rbb.Apply(user, rb); err != nil {
		t.Fatal(err)
	}
	wantExhausted(t, newTenant("acme2"), "owns 2 tenants and may own 2", "a third tenant after re-subjecting the owner binding")

	// Deleting it.
	ownerAgain("alice-again", "acme")
	if _, err := rbb.Delete(user, resource.Key{Name: rb.GetMetadata().GetName()}, nil); err != nil {
		t.Fatal(err)
	}
	wantExhausted(t, newTenant("acme2"), "owns 2 tenants and may own 2", "a third tenant after deleting the owner binding")

	// The record is the apiserver's alone.
	obj, err := tb.Get(user, resource.Key{Name: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	acme := obj.(*tenancyv1alpha1.Tenant)
	if got := acme.GetMetadata().GetAnnotations()[OpenedBy]; got != "user:alice@example.com" {
		t.Fatalf("acme's %s is %q", OpenedBy, got)
	}
	forged := proto.Clone(acme).(*tenancyv1alpha1.Tenant)
	forged.Metadata.Annotations[OpenedBy] = "user:nobody@example.com"
	_, err = tb.Apply(user, forged)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "changing who opened a tenant")
	dropped := proto.Clone(acme).(*tenancyv1alpha1.Tenant)
	dropped.Metadata.Annotations = nil
	dropped.Spec = &tenancyv1alpha1.TenantSpec{DisplayName: "Acme"}
	res, err := tb.Apply(user, dropped)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Object.(*tenancyv1alpha1.Tenant).GetMetadata().GetAnnotations()[OpenedBy]; got != "user:alice@example.com" {
		t.Errorf("an apply without annotations left %s %q", OpenedBy, got)
	}
	_, err = tb.Apply(user, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "preset", Annotations: map[string]string{OpenedBy: "user:nobody@example.com"}}})
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a new tenant naming who opened it")
	// A tenant the apiserver opens is no one's.
	if _, err := tb.Apply(system, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "sys"}}); err != nil {
		t.Fatal(err)
	}
	if o := getTenant(t, s, "sys"); o.GetMetadata().GetAnnotations()[OpenedBy] != "" {
		t.Errorf("a tenant the apiserver opened records %q", o.GetMetadata().GetAnnotations()[OpenedBy])
	}
}

func getTenant(t *testing.T, s *Server, name string) *tenancyv1alpha1.Tenant {
	t.Helper()
	obj, err := s.get(context.Background(), registry.Tenant, resource.Key{Name: name})
	if err != nil || obj == nil {
		t.Fatalf("tenant %s: %v", name, err)
	}
	return obj.(*tenancyv1alpha1.Tenant)
}

// TestAPlanAndATenantNamingItDoNotRace races deleting a plan against a
// tenant coming to name it: the tenant never names a plan that is gone.
func TestAPlanAndATenantNamingItDoNotRace(t *testing.T) {
	t.Parallel()
	s, system, _ := running(t)
	pb, tb := s.backend(registry.Plan), s.backend(registry.Tenant)
	for i := range 20 {
		name := fmt.Sprintf("p%d", i)
		if _, err := pb.Apply(system, &tenancyv1alpha1.Plan{Metadata: &metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
		acme := getTenant(t, s, "acme")
		acme.Spec = &tenancyv1alpha1.TenantSpec{Plan: name}
		acme.Metadata.ResourceVersion = 0
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = pb.Delete(system, resource.Key{Name: name}, nil) }()
		go func() { defer wg.Done(); _, _ = tb.Apply(system, acme) }()
		wg.Wait()
		planObj, err := s.get(context.Background(), registry.Plan, resource.Key{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if got := getTenant(t, s, "acme").GetSpec().GetPlan(); got == name && planObj == nil {
			t.Fatalf("round %d: acme names plan %s, which is gone", i, name)
		}
		acme = getTenant(t, s, "acme")
		acme.Spec = &tenancyv1alpha1.TenantSpec{}
		if _, err := tb.Apply(system, acme); err != nil {
			t.Fatal(err)
		}
	}
}

// TestProjectLimitsCompareByValue: the same limits written another way are
// no change, so a project admin can apply them.
func TestProjectLimitsCompareByValue(t *testing.T) {
	t.Parallel()
	s, system, user := runningWith(t, withProjectAdminRole)
	pb := s.backend(registry.Project)
	web := func(l *tenancyv1alpha1.Limits) *tenancyv1alpha1.Project {
		return &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: "web"}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "acme", Limits: l}}
	}
	if _, err := pb.Apply(user, web(&tenancyv1alpha1.Limits{Memory: proto.String("4Gi")})); err != nil {
		t.Fatal(err)
	}
	bob := projectAdmin(t, s, system)
	if _, err := pb.Apply(bob, web(&tenancyv1alpha1.Limits{Memory: proto.String("4096Mi")})); err != nil {
		t.Errorf("the same memory limit in Mi: %v", err)
	}
	if _, err := pb.Apply(user, web(nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := pb.Apply(bob, web(&tenancyv1alpha1.Limits{})); err != nil {
		t.Errorf("empty limits where there were none: %v", err)
	}
}

// TestTheUsageControllerCatchesUpOnStart: a store written while no usage
// controller ran has its usage written once one starts.
func TestTheUsageControllerCatchesUpOnStart(t *testing.T) {
	t.Parallel()
	s, _, user := running(t)
	if _, err := s.backend(registry.Container).Apply(user, ctr("c1")); err != nil {
		t.Fatal(err)
	}
	if u := getTenant(t, s, "acme").GetStatus().GetUsage(); u != nil {
		t.Fatalf("usage %v without a controller", u)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- New(Config{Store: s.store, DefaultPlanLimits: &tenancyv1alpha1.Limits{}, DefaultMaxOwnedTenants: 1000}).Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	testutil.Eventually(t, "usage of one machine of one CPU", func() (bool, string) {
		u := getTenant(t, s, "acme").GetStatus().GetUsage()
		return u.GetMachines() == 1 && u.GetCpu() == "1", fmt.Sprint(u)
	})
}

// withProjectAdminRole adds test.project-admin, which may get and apply a
// project where it is bound, and nothing at the tenant.
func withProjectAdminRole(c *Config) {
	c.ExtraRoles = map[string]authz.Role{"test.project-admin": authz.NewRole(authz.ProjectLevel,
		authz.Permission(registry.Project, authz.Get), authz.Permission(registry.Project, authz.Apply))}
}

// projectAdmin makes bob a member holding test.project-admin on project
// web, and returns a context acting as him.
func projectAdmin(t *testing.T, s *Server, system context.Context) context.Context {
	t.Helper()
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
	return auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: "bob@example.com"})
}
