package authz

import (
	"testing"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
)

// TestReadingLogsIsItsOwnPermission: reading a container's output is not
// reading the container; a tenant's owner and the platform's root may do
// both, a sponsor or a member neither.
func TestReadingLogsIsItsOwnPermission(t *testing.T) {
	if ContainerLogs != "compute.containers.logs" {
		t.Errorf("the permission is %q", ContainerLogs)
	}
	for role, want := range map[string]bool{
		"platform.root": true, "tenant.owner": true,
		"platform.sponsor": false, "platform.member": false,
	} {
		if got := Roles[role].Grants(ContainerLogs); got != want {
			t.Errorf("%s grants reading logs: %t, want %t", role, got, want)
		}
	}
	if Roles["tenant.owner"].Grants(Permission(registry.Machine, Logs)) {
		t.Error("the logs verb is granted on a kind other than containers")
	}
}

// TestExecIsItsOwnPermission: running a process in a container is neither
// reading it nor reading its output; a tenant's owner and the platform's
// root may, a sponsor or a member not.
func TestExecIsItsOwnPermission(t *testing.T) {
	if ContainerExec != "compute.containers.exec" {
		t.Errorf("the permission is %q", ContainerExec)
	}
	for role, want := range map[string]bool{
		"platform.root": true, "tenant.owner": true,
		"platform.sponsor": false, "platform.member": false,
	} {
		if got := Roles[role].Grants(ContainerExec); got != want {
			t.Errorf("%s grants exec: %t, want %t", role, got, want)
		}
	}
	if Roles["tenant.owner"].Grants(Permission(registry.Machine, Exec)) {
		t.Error("the exec verb is granted on a kind other than containers")
	}
}

// TestARoleOfOnesOwn: NewRole grants what it is given, and only that, from
// a copy; an authorizer's extra roles never replace a built-in one.
func TestARoleOfOnesOwn(t *testing.T) {
	perms := []string{ContainerLogs}
	r := NewRole(TenantLevel, perms...)
	perms[0] = ContainerExec
	if !r.Grants(ContainerLogs) || r.Grants(ContainerExec) || r.Grants(Permission(registry.Container, Get)) {
		t.Errorf("a role of logs alone grants logs %t, exec %t, get %t",
			r.Grants(ContainerLogs), r.Grants(ContainerExec), r.Grants(Permission(registry.Container, Get)))
	}
	a := New(nil, map[string]Role{"test.logs": r, "tenant.owner": NewRole(TenantLevel)})
	if got, ok := a.Role("test.logs"); !ok || !got.Grants(ContainerLogs) {
		t.Error("an extra role is not found")
	}
	if got, _ := a.Role("tenant.owner"); !got.Grants(ContainerExec) {
		t.Error("an extra role replaced a built-in one")
	}
	if _, ok := New(nil, nil).Role("test.logs"); ok {
		t.Error("an extra role leaked to another authorizer")
	}
}
