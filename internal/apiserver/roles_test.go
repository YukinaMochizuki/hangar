package apiserver_test

import (
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
)

func projectScope(p string) *iamv1alpha1.BindingScope {
	return &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Project{Project: p}}
}

// TestProjectRoles: each project-level role reaches its project only, an
// editor changes what runs there but not who may or the project itself, a
// viewer only reads, and none of them can grant past the project.
func TestProjectRoles(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	const (
		carol = "carol@example.com"
		dave  = "dave@example.com"
	)
	e.addMember(carol)
	e.addMember(dave)
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, connect.NewRequest(project("db", "acme"))))
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("db", "pg", "postgres"))))
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("web-bob", bob, "project.owner", projectScope("web"))))
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("web-carol", carol, "project.editor", projectScope("web"))))
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("web-dave", dave, "project.viewer", projectScope("web"))))
	owner, editor, viewer := e.as(bob), e.as(carol), e.as(dave)

	// Each writes what it may in its project.
	must[computev1alpha1.ApplyContainerResponse](t)(owner.containers.ApplyContainer(ctx, applyContainer(container("web", "by-owner", "nginx"))))
	must[computev1alpha1.ApplyContainerResponse](t)(editor.containers.ApplyContainer(ctx, applyContainer(container("web", "by-editor", "nginx"))))
	_, err := viewer.containers.ApplyContainer(ctx, applyContainer(container("web", "by-viewer", "nginx")))
	wantCode(t, err, connect.CodePermissionDenied, "a viewer applying a container")
	must[computev1alpha1.GetContainerResponse](t)(viewer.containers.GetContainer(ctx, getContainer("web", "by-editor")))
	_, err = viewer.containers.DeleteContainer(ctx, connect.NewRequest(&computev1alpha1.DeleteContainerRequest{Project: "web", Name: "by-editor"}))
	wantCode(t, err, connect.CodePermissionDenied, "a viewer deleting a container")

	// Nothing reaches the tenant's other project.
	for who, c := range map[string]clients{"owner": owner, "editor": editor, "viewer": viewer} {
		_, err := c.containers.GetContainer(ctx, getContainer("db", "pg"))
		wantCode(t, err, connect.CodePermissionDenied, "a project "+who+" reading another project")
		_, err = c.projects.ApplyProject(ctx, connect.NewRequest(project("more", "acme")))
		wantCode(t, err, connect.CodePermissionDenied, "a project "+who+" opening a project in the tenant")
	}

	// Who may change who may: the owner, inside the project, at project level.
	const erin = "erin@example.com"
	e.addMember(erin)
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(owner.bindings.ApplyRoleBinding(ctx, binding("web-erin", erin, "project.viewer", projectScope("web"))))
	_, err = owner.bindings.ApplyRoleBinding(ctx, binding("acme-erin", erin, "tenant.owner", tenantScope("acme")))
	wantCode(t, err, connect.CodePermissionDenied, "a project owner granting a tenant role")
	_, err = owner.bindings.ApplyRoleBinding(ctx, binding("web-erin-2", erin, "tenant.owner", projectScope("web")))
	wantCode(t, err, connect.CodeInvalidArgument, "a project owner binding a tenant role to the project")
	// A subject that is not ASCII: the Kelvin sign lower-cases to k.
	for i, email := range []string{"\u212aate@example.com", "gil@ex\u00e4mple.com"} {
		_, err = owner.bindings.ApplyRoleBinding(ctx, binding(fmt.Sprintf("web-ascii-%d", i), email, "project.viewer", projectScope("web")))
		wantCode(t, err, connect.CodeInvalidArgument, "binding "+email)
		if err == nil || !strings.Contains(err.Error(), "lowercase ASCII email address") {
			t.Errorf("binding %q: %v, want it refused as not ASCII", email, err)
		}
	}
	_, err = owner.bindings.ApplyRoleBinding(ctx, binding("db-erin", erin, "project.owner", projectScope("db")))
	wantCode(t, err, connect.CodePermissionDenied, "a project owner granting in another project")
	_, err = owner.bindings.ApplyRoleBinding(ctx, binding("root-erin", erin, "platform.root", platformScope()))
	wantCode(t, err, connect.CodePermissionDenied, "a project owner granting a platform role")
	_, err = editor.bindings.ApplyRoleBinding(ctx, binding("web-erin-3", erin, "project.editor", projectScope("web")))
	wantCode(t, err, connect.CodePermissionDenied, "an editor granting a role")
	_, err = editor.bindings.DeleteRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: "web-dave"}))
	wantCode(t, err, connect.CodePermissionDenied, "an editor removing a binding")
	must[iamv1alpha1.GetRoleBindingResponse](t)(editor.bindings.GetRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.GetRoleBindingRequest{Name: "web-dave"})))

	// Bindings that already exist above the project stay out of reach, by
	// name as by scope: rewriting one, even into the project, or deleting it.
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("acme-erin", erin, "tenant.viewer", tenantScope("acme"))))
	_, err = owner.bindings.ApplyRoleBinding(ctx, binding("acme-erin", erin, "tenant.owner", tenantScope("acme")))
	wantCode(t, err, connect.CodePermissionDenied, "a project owner raising a tenant binding")
	_, err = owner.bindings.ApplyRoleBinding(ctx, binding("acme-erin", bob, "project.owner", projectScope("web")))
	wantCode(t, err, connect.CodePermissionDenied, "a project owner taking over a tenant binding by its name")
	_, err = owner.bindings.DeleteRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: "acme-erin"}))
	wantCode(t, err, connect.CodePermissionDenied, "a project owner deleting a tenant binding")
	_, err = owner.bindings.ApplyRoleBinding(ctx, binding("member-erin-example-com", erin, "platform.root", platformScope()))
	wantCode(t, err, connect.CodePermissionDenied, "a project owner raising a platform binding")
	_, err = owner.bindings.DeleteRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: "member-erin-example-com"}))
	wantCode(t, err, connect.CodePermissionDenied, "a project owner deleting a platform binding")
	// Inside the project, the owner changes a binding the tenant's owner made.
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(owner.bindings.ApplyRoleBinding(ctx, binding("web-dave", dave, "project.editor", projectScope("web"))))
	_, err = owner.tenants.DeleteTenant(ctx, connect.NewRequest(&tenancyv1alpha1.DeleteTenantRequest{Name: "acme"}))
	wantCode(t, err, connect.CodePermissionDenied, "a project owner deleting the tenant")

	// The project itself: its owner changes it; an editor reads it only.
	p := project("web", "acme")
	p.Project.Spec.DisplayName = "Web"
	_, err = editor.projects.ApplyProject(ctx, connect.NewRequest(p))
	wantCode(t, err, connect.CodePermissionDenied, "an editor changing the project")
	must[tenancyv1alpha1.GetProjectResponse](t)(editor.projects.GetProject(ctx, connect.NewRequest(&tenancyv1alpha1.GetProjectRequest{Name: "web"})))
	must[tenancyv1alpha1.ApplyProjectResponse](t)(owner.projects.ApplyProject(ctx, connect.NewRequest(p)))

	// Deleting it is the owner's too, and an editor's never.
	deleteWeb := connect.NewRequest(&tenancyv1alpha1.DeleteProjectRequest{Name: "web"})
	_, err = editor.projects.DeleteProject(ctx, deleteWeb)
	wantCode(t, err, connect.CodePermissionDenied, "an editor deleting the project")
	must[tenancyv1alpha1.DeleteProjectResponse](t)(owner.projects.DeleteProject(ctx, deleteWeb))
}

// TestTenantViewer reads everything in the tenant and changes nothing.
func TestTenantViewer(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "api", "nginx"))))
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("acme-bob", bob, "tenant.viewer", tenantScope("acme"))))
	b := e.as(bob)
	must[computev1alpha1.GetContainerResponse](t)(b.containers.GetContainer(ctx, getContainer("web", "api")))
	must[tenancyv1alpha1.GetTenantResponse](t)(b.tenants.GetTenant(ctx, connect.NewRequest(&tenancyv1alpha1.GetTenantRequest{Name: "acme"})))
	_, err := b.containers.ApplyContainer(ctx, applyContainer(container("web", "more", "nginx")))
	wantCode(t, err, connect.CodePermissionDenied, "a tenant viewer applying a container")
	_, err = b.projects.ApplyProject(ctx, connect.NewRequest(project("more", "acme")))
	wantCode(t, err, connect.CodePermissionDenied, "a tenant viewer opening a project")
	_, err = b.bindings.ApplyRoleBinding(ctx, binding("acme-bob-2", bob, "tenant.owner", tenantScope("acme")))
	wantCode(t, err, connect.CodePermissionDenied, "a tenant viewer granting itself owner")
}
