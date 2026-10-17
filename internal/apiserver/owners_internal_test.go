package apiserver

import (
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// TestDeletingATenantFreesItsLastOwner: once a tenant is being deleted, a
// person may delete its last owner binding, which goes with the tenant
// anyway; before, they may not.
func TestDeletingATenantFreesItsLastOwner(t *testing.T) {
	s, system, user := running(t)
	// No controller runs here to take a disk's finalizers off, so the disk
	// keeps project web, and with it acme, being deleted.
	if _, err := s.backend(registry.Disk).Apply(system, quotaDisk("data", "1Gi", nil)); err != nil {
		t.Fatal(err)
	}
	objs, _, err := s.store.List(t.Context(), store.ListOptions{Kind: registry.RoleBinding.Kind})
	if err != nil {
		t.Fatal(err)
	}
	var owner *iamv1alpha1.RoleBinding
	for _, o := range objs {
		var rb iamv1alpha1.RoleBinding
		if err := proto.Unmarshal(o.Data, &rb); err != nil {
			t.Fatal(err)
		}
		if tenant, ok := ownerTenant(&rb); ok && tenant == "acme" {
			owner = &rb
		}
	}
	if owner == nil {
		t.Fatal("acme has no owner binding")
	}

	if err := refuseRemovingLastOwner(user, s, owner); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("deleting acme's last owner binding while acme is live: %v, want failed_precondition", err)
	}
	if _, err := s.backend(registry.Tenant).Delete(user, resource.Key{Name: "acme"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := refuseRemovingLastOwner(user, s, owner); err != nil {
		t.Errorf("deleting acme's last owner binding while acme is being deleted: %v", err)
	}
	got, err := s.backend(registry.Tenant).Get(system, resource.Key{Name: "acme"})
	if err != nil || got.(*tenancyv1alpha1.Tenant).GetMetadata().GetDeleteTime() == nil {
		t.Errorf("acme should still be there, being deleted: %v, %v", got, err)
	}
}
