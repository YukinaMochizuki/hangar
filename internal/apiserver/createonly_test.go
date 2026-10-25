package apiserver_test

import (
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
)

func createContainer(c *computev1alpha1.Container) *connect.Request[computev1alpha1.ApplyContainerRequest] {
	return connect.NewRequest(&computev1alpha1.ApplyContainerRequest{Container: c, CreateOnly: true})
}

func TestCreateOnlyApplyNeverReplaces(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)

	res := must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, createContainer(container("web", "api", "nginx"))))
	if !res.GetCreated() {
		t.Error("a create-only apply of a new name did not report created")
	}
	before := res.GetContainer()

	_, err := a.containers.ApplyContainer(ctx, createContainer(container("web", "api", "nginx:2")))
	wantCode(t, err, connect.CodeAlreadyExists, "a create-only apply of an existing container")
	after := must[computev1alpha1.GetContainerResponse](t)(a.containers.GetContainer(ctx, getContainer("web", "api"))).GetContainer()
	if !proto.Equal(before, after) {
		t.Errorf("a refused create-only apply changed the object: %v, then %v", before, after)
	}

	versioned := container("web", "new", "nginx")
	versioned.Metadata.ResourceVersion = after.GetMetadata().GetResourceVersion()
	_, err = a.containers.ApplyContainer(ctx, createContainer(versioned))
	wantCode(t, err, connect.CodeInvalidArgument, "a create-only apply with a resource version")

	created := tenant("acme")
	created.CreateOnly = true
	_, err = a.tenants.ApplyTenant(ctx, connect.NewRequest(created))
	wantCode(t, err, connect.CodeAlreadyExists, "the owner creating their own tenant again")
}

// A caller who may not write an object is refused for that, whether or not it
// exists, and never learns that the name is taken. The containers are refused
// at the project check, before anything is read; the tenant and the binding
// are global, and cover the authorization against an existing object (with
// TestARacedCreateIsAuthorizedAgainstTheObjectFound for an object made since
// the read).
func TestCreateOnlyDeniesBeforeSayingAnObjectExists(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	must[computev1alpha1.ApplyContainerResponse](t)(e.as(alice).containers.ApplyContainer(ctx, applyContainer(container("web", "api", "nginx"))))
	b := e.as(bob)

	for _, name := range []string{"api", "missing"} {
		_, err := b.containers.ApplyContainer(ctx, createContainer(container("web", name, "x")))
		wantCode(t, err, connect.CodePermissionDenied, "bob creating container "+name)
	}

	taken := tenant("acme")
	taken.CreateOnly = true
	_, err := b.tenants.ApplyTenant(ctx, connect.NewRequest(taken))
	wantCode(t, err, connect.CodePermissionDenied, "bob creating alice's tenant")

	rb := binding("bootstrap-root", bob, "platform.root", platformScope())
	rb.Msg.CreateOnly = true
	_, err = b.bindings.ApplyRoleBinding(ctx, rb)
	wantCode(t, err, connect.CodePermissionDenied, "bob creating the bootstrap binding")
}
