package authz

import (
	"slices"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
)

// TestBuiltInRolesHoldExactly pins, for each built-in role, every permission
// it grants on every kind, so a change to one is a change to this table.
func TestBuiltInRolesHoldExactly(t *testing.T) {
	reach := []string{"compute.machines.logs", "compute.machines.exec", "compute.machines.ssh", "network.publicendpoints.access"}
	isRead := func(v Verb) bool { return v == Get || v == List || v == Watch }
	isWrite := func(v Verb) bool { return v == Apply || v == Delete }
	want := map[string]func(r registry.Resource, v Verb, perm string) bool{
		"platform.root": func(_ registry.Resource, v Verb, perm string) bool {
			return isRead(v) || isWrite(v) || slices.Contains(reach, perm)
		},
		"platform.sponsor": func(registry.Resource, Verb, string) bool { return false },
		"platform.member":  func(registry.Resource, Verb, string) bool { return false },
		"tenant.owner": func(_ registry.Resource, v Verb, perm string) bool {
			return isRead(v) || isWrite(v) || slices.Contains(reach, perm)
		},
		"project.owner": func(_ registry.Resource, v Verb, perm string) bool {
			return isRead(v) || isWrite(v) || slices.Contains(reach, perm)
		},
		"project.editor": func(r registry.Resource, v Verb, perm string) bool {
			if r.Kind == registry.RoleBinding.Kind || r.Kind == registry.ServiceToken.Kind || r.Kind == registry.Project.Kind {
				return isRead(v)
			}
			return isRead(v) || isWrite(v) || slices.Contains(reach, perm)
		},
		"tenant.viewer":  func(_ registry.Resource, v Verb, _ string) bool { return isRead(v) },
		"project.viewer": func(_ registry.Resource, v Verb, _ string) bool { return isRead(v) },
	}
	if len(want) != len(Roles) {
		t.Errorf("%d built-in roles, %d pinned", len(Roles), len(want))
	}
	for name, wants := range want {
		role, ok := Roles[name]
		if !ok {
			t.Fatalf("no role %s", name)
		}
		for _, r := range registry.All {
			for _, v := range verbs {
				perm := Permission(r, v)
				if perm == CreateTenant {
					continue // pinned below, as platform roles hold it alone
				}
				if got, w := role.Grants(perm), wants(r, v, perm); got != w {
					t.Errorf("%s grants %s: %t, want %t", name, perm, got, w)
				}
			}
		}
		if got, w := role.Grants(CreateTenant), strings.HasPrefix(name, "platform."); got != w {
			t.Errorf("%s grants %s: %t, want %t", name, CreateTenant, got, w)
		}
		if got, w := role.Grants(MonitoringRead), name == "platform.root"; got != w {
			t.Errorf("%s grants %s: %t, want %t", name, MonitoringRead, got, w)
		}
	}
	for name, level := range map[string]ScopeLevel{
		"tenant.owner": TenantLevel, "tenant.viewer": TenantLevel,
		"project.owner": ProjectLevel, "project.editor": ProjectLevel, "project.viewer": ProjectLevel,
	} {
		if Roles[name].Level != level || Roles[name].Member {
			t.Errorf("%s: level %d member %t", name, Roles[name].Level, Roles[name].Member)
		}
	}
}
