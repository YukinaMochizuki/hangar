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
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/auth"
)

// defineSizes makes two made-up sizes, as the system.
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

// wantShapeRefused checks err is InvalidArgument holding want.
func wantShapeRefused(t *testing.T, err error, want, what string) {
	t.Helper()
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: got %v, want InvalidArgument with %q", what, err, want)
	}
}

func TestCPURange(t *testing.T) {
	for _, c := range []struct {
		memory uint64
		lo, hi uint64
	}{
		{256 * quantity.MiB, 1, 1},
		{512 * quantity.MiB, 1, 1},
		{768 * quantity.MiB, 1, 1},
		{1 * quantity.GiB, 1, 2},
		{4 * quantity.GiB, 1, 8},
		{8 * quantity.GiB, 1, 16},
		{8*quantity.GiB + 256*quantity.MiB, 2, 16},
		{12 * quantity.GiB, 2, 24},
		{32 * quantity.GiB, 4, 64},
	} {
		if lo, hi := cpuRange(c.memory); lo != c.lo || hi != c.hi {
			t.Errorf("cpuRange(%s) = %d, %d, want %d, %d", quantity.FormatMemory(c.memory), lo, hi, c.lo, c.hi)
		}
	}
}

// TestAMachineKeepsToTheShapeRules: a tenant's machine has memory in 256Mi
// steps, 512Mi to 8Gi of it per vCPU or one vCPU, and a root disk of at
// least 2Gi, whether or not the platform defines any size, and whatever
// disk a size has.
func TestAMachineKeepsToTheShapeRules(t *testing.T) {
	for _, withSizes := range []bool{false, true} {
		s, system, user := running(t)
		if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
			t.Fatal(err)
		}
		if withSizes {
			defineSizes(t, s, system)
		}
		ctrs := s.backend(registry.Container)
		for name, c := range map[string]struct {
			ctr  *computev1alpha1.Container
			want string
		}{
			"memory off the step":      {sizedCtr("c", "1", "300Mi", ""), "spec.resources.memory 300Mi is not a whole number of 256Mi steps"},
			"memory below one step":    {sizedCtr("c", "1", "128Mi", ""), "spec.resources.memory 128Mi is not a whole number of 256Mi steps"},
			"memory in bytes off step": {sizedCtr("c", "1", "1073741825", ""), "is not a whole number of 256Mi steps"},
			"too many vCPUs":           {sizedCtr("c", "8", "1Gi", ""), "a machine of 1Gi of memory has 1 to 2 vCPUs, not 8"},
			"two vCPUs on 768Mi":       {sizedCtr("c", "2", "768Mi", ""), "a machine of 768Mi of memory has 1 vCPU, not 2"},
			"8 vCPUs on 256Mi":         {sizedCtr("c", "8", "256Mi", ""), "has 1 vCPU, not 8"},
			"too few vCPUs":            {sizedCtr("c", "1", "32Gi", ""), "a machine of 32Gi of memory has 4 to 64 vCPUs, not 1"},
			"a disk below 2Gi":         {sizedCtr("c", "1", "1Gi", "1Gi"), "spec.resources.disk 1Gi is below the minimum of 2Gi"},
		} {
			_, err := ctrs.Apply(user, c.ctr)
			wantShapeRefused(t, err, c.want, "a container of "+name)
		}
		for name, c := range map[string]*computev1alpha1.Container{
			"the defaults":                 ctr("defaults"),
			"the smallest":                 sizedCtr("smallest", "1", "256Mi", "2Gi"),
			"two vCPUs on 1Gi":             sizedCtr("two", "2", "1Gi", ""),
			"8 vCPUs on 4Gi":               sizedCtr("eight", "8", "4Gi", ""),
			"2 vCPUs on 16Gi":              sizedCtr("wide", "2", "16Gi", ""),
			"other units":                  sizedCtr("units", "2000m", "1536Mi", "2048Mi"),
			"a size's shape":               sizedCtr("preset", "1", "2Gi", "20Gi"),
			"a size's cpu, a smaller disk": sizedCtr("preset-small", "1", "2Gi", "5Gi"),
		} {
			if _, err := ctrs.Apply(user, c); err != nil {
				t.Errorf("sizes %v: a container of %s: %v", withSizes, name, err)
			}
		}

		vms := s.backend(registry.VirtualMachine)
		_, err := vms.Apply(user, sizedVM("v", "8", "1Gi", ""))
		wantShapeRefused(t, err, "a machine of 1Gi of memory has 1 to 2 vCPUs, not 8", "a VM of 8 vCPUs and 1Gi")
		if _, err := vms.Apply(user, sizedVM("v", "", "", "")); err != nil {
			t.Errorf("sizes %v: a VM of the defaults: %v", withSizes, err)
		}
		// A VM booting a Disk it names has that Disk's size, which is not
		// checked; its cpu and memory are.
		of := vmOfDisk("of-disk", "boot")
		of.Spec.Resources = &computev1alpha1.VirtualMachineResources{Cpu: "1", Memory: "2Gi"}
		if _, err := vms.Apply(user, of); err != nil {
			t.Errorf("sizes %v: a VM of a named disk: %v", withSizes, err)
		}
		of.Spec.Resources.Cpu = "5"
		_, err = vms.Apply(user, of)
		wantShapeRefused(t, err, "has 1 to 4 vCPUs, not 5", "a VM of a named disk resized to 5 vCPUs")
	}
}

// TestASizeKeepsToTheShapeRules: a size is a preset of a shape a tenant may
// have.
func TestASizeKeepsToTheShapeRules(t *testing.T) {
	s, system, _ := running(t)
	sizes := s.backend(registry.MachineSize)
	for name, c := range map[string]struct {
		spec *computev1alpha1.MachineSizeSpec
		want string
	}{
		"lopsided":   {&computev1alpha1.MachineSizeSpec{DisplayName: "X", Cpu: "8", Memory: "1Gi", Disk: "10Gi"}, "spec: a machine of 1Gi of memory has 1 to 2 vCPUs, not 8"},
		"off step":   {&computev1alpha1.MachineSizeSpec{DisplayName: "X", Cpu: "1", Memory: "300Mi", Disk: "10Gi"}, "spec.memory 300Mi is not a whole number of 256Mi steps"},
		"a 1Gi disk": {&computev1alpha1.MachineSizeSpec{DisplayName: "X", Cpu: "1", Memory: "1Gi", Disk: "1Gi"}, "spec.disk 1Gi is below the minimum of 2Gi"},
	} {
		_, err := sizes.Apply(system, &computev1alpha1.MachineSize{Metadata: &metav1.ObjectMeta{Name: "x"}, Spec: c.spec})
		wantShapeRefused(t, err, c.want, "a size "+name)
	}
	if _, err := sizes.Apply(system, &computev1alpha1.MachineSize{Metadata: &metav1.ObjectMeta{Name: "nano"},
		Spec: &computev1alpha1.MachineSizeSpec{DisplayName: "Nano", Cpu: "1", Memory: "256Mi", Disk: "2Gi"}}); err != nil {
		t.Errorf("the smallest size: %v", err)
	}
}

// TestAnUnchangedMachineKeepsItsResources: an update is checked only when
// it changes cpu, memory or disk, so a machine outside the rules, made
// before them or by someone exempt, still starts, stops and takes labels.
func TestAnUnchangedMachineKeepsItsResources(t *testing.T) {
	s, system, user := running(t)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	ctrs := s.backend(registry.Container)
	// The system writes what it likes.
	if _, err := ctrs.Apply(system, sizedCtr("odd", "8", "1Gi", "1Gi")); err != nil {
		t.Fatalf("the system making a container outside the rules: %v", err)
	}
	same := sizedCtr("odd", "8", "1Gi", "1Gi")
	same.Metadata.Labels = map[string]string{"team": "web"}
	same.Spec.RunState = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED
	if _, err := ctrs.Apply(user, same); err != nil {
		t.Errorf("stopping and labelling a container outside the rules: %v", err)
	}
	// The same values in other units are no change.
	if _, err := ctrs.Apply(user, sizedCtr("odd", "8000m", "1024Mi", "1024Mi")); err != nil {
		t.Errorf("restating a container's resources in other units: %v", err)
	}
	for name, c := range map[string]*computev1alpha1.Container{
		"cpu":    sizedCtr("odd", "7", "1Gi", "1Gi"),
		"memory": sizedCtr("odd", "8", "2Gi", "1Gi"),
	} {
		_, err := ctrs.Apply(user, c)
		wantShapeRefused(t, err, "vCPUs, not", "changing the "+name+" of a container outside the rules")
	}
	_, err := ctrs.Apply(user, sizedCtr("odd", "2", "1Gi", "1Gi"))
	wantShapeRefused(t, err, "below the minimum of 2Gi", "fixing the cpu but keeping a 1Gi disk")
	if _, err := ctrs.Apply(user, sizedCtr("odd", "2", "1Gi", "2Gi")); err != nil {
		t.Errorf("bringing a container within the rules: %v", err)
	}

	vms := s.backend(registry.VirtualMachine)
	if _, err := vms.Apply(system, sizedVM("odd", "8", "1Gi", "")); err != nil {
		t.Fatalf("the system making a VM outside the rules: %v", err)
	}
	stopped := sizedVM("odd", "8", "1Gi", "")
	stopped.Spec.RunState = computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED
	if _, err := vms.Apply(user, stopped); err != nil {
		t.Errorf("stopping a VM outside the rules: %v", err)
	}
	// Stating the default root size is no change.
	if _, err := vms.Apply(user, sizedVM("odd", "8", "1Gi", "10Gi")); err != nil {
		t.Errorf("stating a VM's default root size: %v", err)
	}
	_, err = vms.Apply(user, sizedVM("odd", "8", "1Gi", "20Gi"))
	wantShapeRefused(t, err, "has 1 to 2 vCPUs, not 8", "growing the root disk of a VM outside the rules")
}

// TestWhoMayDefineSizesIsNotChecked: the exemption follows the permission
// to define sizes, held at the platform: platform.root, whether a person or
// a service token bound it, and the system. A tenant owner and a token of a
// project owner are checked.
func TestWhoMayDefineSizesIsNotChecked(t *testing.T) {
	s, system, user := running(t)
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
		wantShapeRefused(t, err, "has 1 to 2 vCPUs, not 8", who+" making a container outside the rules")
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
			t.Errorf("%s making a container outside the rules: %v", w.who, err)
		}
	}
}
