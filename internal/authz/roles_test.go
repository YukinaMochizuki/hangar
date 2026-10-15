package authz

import (
	"testing"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
)

// TestMachineCapabilitiesAreTheirOwnPermissions: reading a machine's output,
// running a process in it and logging in to it are permissions on the
// Machine kind, apart from one another and from reading the object; a
// tenant's owner and the platform's root hold them, a sponsor or a member
// none. No other kind has them: the kind a caller names never decides.
func TestMachineCapabilitiesAreTheirOwnPermissions(t *testing.T) {
	for perm, want := range map[string]string{
		MachineLogs: "compute.machines.logs", MachineExec: "compute.machines.exec", MachineSSH: "compute.machines.ssh",
	} {
		if perm != want {
			t.Errorf("the permission is %q, want %q", perm, want)
		}
	}
	for role, want := range map[string]bool{
		"platform.root": true, "tenant.owner": true,
		"platform.sponsor": false, "platform.member": false,
	} {
		for _, perm := range []string{MachineLogs, MachineExec, MachineSSH} {
			if got := Roles[role].Grants(perm); got != want {
				t.Errorf("%s grants %s: %t, want %t", role, perm, got, want)
			}
		}
	}
	for _, r := range registry.All {
		if r.Kind == registry.Machine.Kind {
			continue
		}
		for _, v := range []Verb{Logs, Exec, SSH} {
			if Roles["platform.root"].Grants(Permission(r, v)) {
				t.Errorf("%s is granted on %s, not a machine", v, r.Plural)
			}
		}
	}
}

// TestARoleOfOnesOwn: NewRole grants what it is given, and only that, from
// a copy; an authorizer's extra roles never replace a built-in one.
func TestARoleOfOnesOwn(t *testing.T) {
	perms := []string{MachineLogs}
	r := NewRole(TenantLevel, perms...)
	perms[0] = MachineExec
	if !r.Grants(MachineLogs) || r.Grants(MachineExec) || r.Grants(Permission(registry.Container, Get)) {
		t.Errorf("a role of logs alone grants logs %t, exec %t, get %t",
			r.Grants(MachineLogs), r.Grants(MachineExec), r.Grants(Permission(registry.Container, Get)))
	}
	a := New(nil, map[string]Role{"test.logs": r, "tenant.owner": NewRole(TenantLevel)})
	if got, ok := a.Role("test.logs"); !ok || !got.Grants(MachineLogs) {
		t.Error("an extra role is not found")
	}
	if got, _ := a.Role("tenant.owner"); !got.Grants(MachineExec) {
		t.Error("an extra role replaced a built-in one")
	}
	if _, ok := New(nil, nil).Role("test.logs"); ok {
		t.Error("an extra role leaked to another authorizer")
	}
}
