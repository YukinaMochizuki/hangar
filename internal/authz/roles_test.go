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
