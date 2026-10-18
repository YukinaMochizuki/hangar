package apiserver

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/auth"
)

// sizeList is how a refusal lists the sizes defineSizes makes.
const sizeList = "small: 1 / 2Gi / 20Gi, large: 2 / 4Gi / 40Gi"

// defineSizes makes two made-up sizes, as the system. The larger is applied
// first, so the refusal's order is the sizes' own.
func defineSizes(t *testing.T, s *Server, system context.Context) {
	t.Helper()
	for _, ms := range []*computev1alpha1.MachineSize{
		{Metadata: &metav1.ObjectMeta{Name: "large"}, Spec: &computev1alpha1.MachineSizeSpec{DisplayName: "L", Cpu: "2", Memory: "4Gi", Disk: "40Gi", Order: 20}},
		{Metadata: &metav1.ObjectMeta{Name: "small"}, Spec: &computev1alpha1.MachineSizeSpec{DisplayName: "S", Cpu: "1", Memory: "2Gi", Disk: "20Gi", Order: 10}},
	} {
		if _, err := s.backend(registry.MachineSize).Apply(system, ms); err != nil {
			t.Fatal(err)
		}
	}
}

func sizedCtr(name, cpu, memory, disk string) *computev1alpha1.Container {
	c := ctr(name)
	c.Spec.Resources = &computev1alpha1.Resources{Cpu: cpu, Memory: memory, Disk: disk}
	return c
}

func sizedVM(name, cpu, memory, rootSize string) *computev1alpha1.VirtualMachine {
	vm := vmFromImage(name, "noble")
	vm.Spec.Resources = &computev1alpha1.VirtualMachineResources{Cpu: cpu, Memory: memory}
	vm.Spec.RootDisk.GetFromImage().Size = rootSize
	return vm
}

// wantSizeRefused checks err is the refusal of a machine of no size, which
// lists the sizes, and holds want.
func wantSizeRefused(t *testing.T, err error, want, what string) {
	t.Helper()
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), sizeList) || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: got %v, want InvalidArgument with %q listing %q", what, err, want, sizeList)
	}
}

// TestNoMachineSizeRefusesNothing: until the platform defines a size, a
// machine may have any resources.
func TestNoMachineSizeRefusesNothing(t *testing.T) {
	s, system, user := running(t)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Container).Apply(user, sizedCtr("odd", "8", "1Gi", "")); err != nil {
		t.Errorf("a container of 8 vCPUs and 1Gi with no size defined: %v", err)
	}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, sizedVM("odd", "8", "1Gi", "")); err != nil {
		t.Errorf("a VM of 8 vCPUs and 1Gi with no size defined: %v", err)
	}
}

// TestAMachineMustMatchASize: a tenant's machine has the cpu and memory of
// a size, compared by value, and at least its disk.
func TestAMachineMustMatchASize(t *testing.T) {
	s, system, user := running(t)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	defineSizes(t, s, system)
	ctrs := s.backend(registry.Container)
	for name, c := range map[string]*computev1alpha1.Container{
		"cpu of no size":                      sizedCtr("c", "3", "4Gi", "40Gi"),
		"memory of no size":                   sizedCtr("c", "2", "3Gi", "40Gi"),
		"a lopsided ratio":                    sizedCtr("c", "8", "1Gi", "40Gi"),
		"one size's cpu and another's memory": sizedCtr("c", "2", "2Gi", "40Gi"),
		"the defaults":                        ctr("c"),
	} {
		_, err := ctrs.Apply(user, c)
		wantSizeRefused(t, err, "are those of no machine size", "a container of "+name)
	}
	_, err := ctrs.Apply(user, sizedCtr("c", "1", "2Gi", ""))
	wantSizeRefused(t, err, `spec.resources.disk 10Gi is below the 20Gi of machine size "small"`, "a small container on the default disk")
	_, err = ctrs.Apply(user, sizedCtr("c", "1", "2Gi", "19Gi"))
	wantSizeRefused(t, err, `spec.resources.disk 19Gi is below the 20Gi of machine size "small"`, "a small container on 19Gi")
	for name, c := range map[string]*computev1alpha1.Container{
		"a size as written":         sizedCtr("exact", "1", "2Gi", "20Gi"),
		"a size in other units":     sizedCtr("units", "2000m", "4096Mi", "40960Mi"),
		"a size with a larger disk": sizedCtr("roomy", "1", "2Gi", "30Gi"),
		"a size's memory in bytes":  sizedCtr("bytes", "1", "2147483648", "20Gi"),
	} {
		if _, err := ctrs.Apply(user, c); err != nil {
			t.Errorf("a container of %s: %v", name, err)
		}
	}

	vms := s.backend(registry.VirtualMachine)
	_, err = vms.Apply(user, sizedVM("v", "8", "1Gi", "40Gi"))
	wantSizeRefused(t, err, "are those of no machine size", "a VM of a lopsided ratio")
	_, err = vms.Apply(user, sizedVM("v", "", "", "40Gi"))
	wantSizeRefused(t, err, "cpu 1 and memory 1024Mi are those of no machine size", "a VM of the defaults")
	// An unstated root size is its default, 10Gi, and checked as one.
	_, err = vms.Apply(user, sizedVM("v", "1", "2Gi", ""))
	wantSizeRefused(t, err, `spec.root_disk.from_image.size 10Gi is below the 20Gi of machine size "small"`, "a small VM with no root size")
	_, err = vms.Apply(user, sizedVM("v", "2", "4Gi", "39Gi"))
	wantSizeRefused(t, err, `spec.root_disk.from_image.size 39Gi is below the 40Gi of machine size "large"`, "a large VM on 39Gi")
	if _, err := vms.Apply(user, sizedVM("v", "2", "4096Mi", "40Gi")); err != nil {
		t.Errorf("a VM of a size in other units: %v", err)
	}
	// A VM booting a Disk it names has that Disk's size, which is not
	// checked; its cpu and memory are.
	of := vmOfDisk("of-disk", "boot")
	of.Spec.Resources = &computev1alpha1.VirtualMachineResources{Cpu: "1", Memory: "2Gi"}
	if _, err := vms.Apply(user, of); err != nil {
		t.Errorf("a small VM of a named disk: %v", err)
	}
	of.Spec.Resources.Cpu = "3"
	_, err = vms.Apply(user, of)
	wantSizeRefused(t, err, "are those of no machine size", "a VM of a named disk resized to 3 vCPUs")
}

// TestADiskRefusalNamesTheSmallestDisk: of the sizes whose cpu and memory a
// machine has, the refusal names the one of the smallest disk, whatever the
// order the sizes are listed in.
func TestADiskRefusalNamesTheSmallestDisk(t *testing.T) {
	s, system, user := running(t)
	for _, ms := range []*computev1alpha1.MachineSize{
		{Metadata: &metav1.ObjectMeta{Name: "a"}, Spec: &computev1alpha1.MachineSizeSpec{DisplayName: "A", Cpu: "1", Memory: "2Gi", Disk: "40Gi", Order: 1}},
		{Metadata: &metav1.ObjectMeta{Name: "b"}, Spec: &computev1alpha1.MachineSizeSpec{DisplayName: "B", Cpu: "1", Memory: "2Gi", Disk: "20Gi", Order: 2}},
	} {
		if _, err := s.backend(registry.MachineSize).Apply(system, ms); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.backend(registry.Container).Apply(user, sizedCtr("c", "1", "2Gi", "10Gi"))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), `spec.resources.disk 10Gi is below the 20Gi of machine size "b"`) {
		t.Errorf("a 10Gi container between sizes of 40Gi and 20Gi: got %v, want the refusal naming b", err)
	}
}

// TestAnUnchangedMachineKeepsItsResources: an update is checked only when
// it changes cpu, memory or disk, so a machine of no size, made before the
// sizes or by someone exempt, still starts, stops and takes labels.
func TestAnUnchangedMachineKeepsItsResources(t *testing.T) {
	s, system, user := running(t)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	defineSizes(t, s, system)
	ctrs := s.backend(registry.Container)
	// The system writes what it likes.
	if _, err := ctrs.Apply(system, sizedCtr("odd", "8", "1Gi", "10Gi")); err != nil {
		t.Fatalf("the system making a container of no size: %v", err)
	}
	same := sizedCtr("odd", "8", "1Gi", "10Gi")
	same.Metadata.Labels = map[string]string{"team": "web"}
	same.Spec.RunState = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED
	if _, err := ctrs.Apply(user, same); err != nil {
		t.Errorf("stopping and labelling a container of no size: %v", err)
	}
	// The same values in other units are no change.
	if _, err := ctrs.Apply(user, sizedCtr("odd", "8000m", "1024Mi", "10240Mi")); err != nil {
		t.Errorf("restating a container's resources in other units: %v", err)
	}
	for name, c := range map[string]*computev1alpha1.Container{
		"cpu":    sizedCtr("odd", "7", "1Gi", "10Gi"),
		"memory": sizedCtr("odd", "8", "2Gi", "10Gi"),
		"disk":   sizedCtr("odd", "8", "1Gi", "20Gi"),
	} {
		_, err := ctrs.Apply(user, c)
		wantSizeRefused(t, err, "are those of no machine size", "changing the "+name+" of a container of no size")
	}
	if _, err := ctrs.Apply(user, sizedCtr("odd", "1", "2Gi", "20Gi")); err != nil {
		t.Errorf("resizing a container of no size to a size: %v", err)
	}
	_, err := ctrs.Apply(user, sizedCtr("odd", "8", "1Gi", "20Gi"))
	wantSizeRefused(t, err, "are those of no machine size", "resizing a container back to no size")

	vms := s.backend(registry.VirtualMachine)
	if _, err := vms.Apply(system, sizedVM("odd", "8", "1Gi", "")); err != nil {
		t.Fatalf("the system making a VM of no size: %v", err)
	}
	stopped := sizedVM("odd", "8", "1Gi", "")
	stopped.Spec.RunState = computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED
	if _, err := vms.Apply(user, stopped); err != nil {
		t.Errorf("stopping a VM of no size: %v", err)
	}
	// Stating the default root size is no change.
	if _, err := vms.Apply(user, sizedVM("odd", "8", "1Gi", "10Gi")); err != nil {
		t.Errorf("stating a VM's default root size: %v", err)
	}
	_, err = vms.Apply(user, sizedVM("odd", "8", "1Gi", "20Gi"))
	wantSizeRefused(t, err, "are those of no machine size", "growing the root disk of a VM of no size")
}

// TestWhoMayDefineSizesIsNotChecked: the exemption follows the permission
// to define sizes, held at the platform: platform.root, whether a person or
// a service token bound it, and the system. A tenant owner and a token of a
// project owner are checked.
func TestWhoMayDefineSizesIsNotChecked(t *testing.T) {
	s, system, user := running(t)
	defineSizes(t, s, system)
	invite(t, s, "root@example.com", "platform.root")
	signUp(t, s, "root@example.com")
	root := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: "root@example.com"})

	bind := func(token, role string, scope *iamv1alpha1.BindingScope) {
		t.Helper()
		if _, err := s.backend(registry.RoleBinding).Apply(root, &iamv1alpha1.RoleBinding{
			Metadata: &metav1.ObjectMeta{Name: strings.ReplaceAll(token, "/", "-") + "-" + strings.ReplaceAll(role, ".", "-")},
			Spec: &iamv1alpha1.RoleBindingSpec{
				Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_ServiceToken{ServiceToken: token}},
				Role:    role,
				Scope:   scope,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	owner := serviceToken(t, s, system, "web", "ci")
	bind("web/ci", "project.owner", &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Project{Project: "web"}})
	admin := serviceToken(t, s, system, "web", "admin")
	bind("web/admin", "platform.root", &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}})

	ctrs := s.backend(registry.Container)
	for who, ctx := range map[string]context.Context{"a tenant owner": user, "a project owner's service token": owner} {
		_, err := ctrs.Apply(ctx, sizedCtr("odd", "8", "1Gi", ""))
		wantSizeRefused(t, err, "are those of no machine size", who+" making a container of no size")
	}
	for _, w := range []struct {
		who, name string
		ctx       context.Context
	}{
		{"platform.root", "by-root", root},
		{"a service token bound platform.root", "by-admin-token", admin},
		{"the system", "by-system", system},
	} {
		if _, err := ctrs.Apply(w.ctx, sizedCtr(w.name, "8", "1Gi", "")); err != nil {
			t.Errorf("%s making a container of no size: %v", w.who, err)
		}
	}
}
