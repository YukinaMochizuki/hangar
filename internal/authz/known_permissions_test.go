package authz

import (
	"testing"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
)

// TestRolesNameOnlyKnownPermissions: every permission a built-in role (or
// the edge) can hold is one KnownPermission knows, so TestPermissions never
// refuses a name a role grants.
func TestRolesNameOnlyKnownPermissions(t *testing.T) {
	for name, role := range Roles {
		for _, perm := range role.permissions {
			if !KnownPermission(perm) {
				t.Errorf("%s names %q, which KnownPermission does not know", name, perm)
			}
		}
		for _, v := range role.verbs {
			for _, r := range registry.All {
				if perm := Permission(r, v); !KnownPermission(perm) {
					t.Errorf("%s grants %q, which KnownPermission does not know", name, perm)
				}
			}
		}
	}
	for _, perm := range EdgeReads {
		if !KnownPermission(perm) {
			t.Errorf("the edge reads by %q, which KnownPermission does not know", perm)
		}
	}
}

// TestKnownPermissions: each kind has its generic verbs, and the special
// actions only on the kind they belong to.
func TestKnownPermissions(t *testing.T) {
	for _, perm := range []string{
		"compute.containers.apply", "iam.rolebindings.apply", "compute.machines.updateStatus",
		"tenancy.tenants.create", "compute.machines.logs", "compute.machines.exec",
		"compute.machines.ssh", "network.publicendpoints.access",
	} {
		if !KnownPermission(perm) {
			t.Errorf("%q is not known", perm)
		}
	}
	for _, perm := range []string{
		"", "compute.containers", "compute.containers.aply", "compute.containers.logs",
		"tenancy.projects.create", "compute.containers.access", "nosuch.things.get", "compute.containers.apply.x",
	} {
		if KnownPermission(perm) {
			t.Errorf("%q is known", perm)
		}
	}
	r, v, ok := LookupPermission("compute.machines.ssh")
	if !ok || r.Kind != registry.Machine.Kind || v != SSH {
		t.Errorf("compute.machines.ssh: %s %s %t", r.Kind, v, ok)
	}
}
