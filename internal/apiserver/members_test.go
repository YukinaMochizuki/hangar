package apiserver_test

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
)

func listMembers(scope *iamv1alpha1.BindingScope) *connect.Request[iamv1alpha1.ListMembersRequest] {
	return connect.NewRequest(&iamv1alpha1.ListMembersRequest{Scope: scope})
}

func addMember(scope *iamv1alpha1.BindingScope, email, role string) *connect.Request[iamv1alpha1.AddMemberRequest] {
	return connect.NewRequest(&iamv1alpha1.AddMemberRequest{Scope: scope, Email: email, Role: role})
}

func setMemberRole(scope *iamv1alpha1.BindingScope, email, role string) *connect.Request[iamv1alpha1.SetMemberRoleRequest] {
	return connect.NewRequest(&iamv1alpha1.SetMemberRoleRequest{Scope: scope, Email: email, Role: role})
}

func removeMember(scope *iamv1alpha1.BindingScope, email string) *connect.Request[iamv1alpha1.RemoveMemberRequest] {
	return connect.NewRequest(&iamv1alpha1.RemoveMemberRequest{Scope: scope, Email: email})
}

// memberRows renders members as "email username role" lines, for comparing
// whole listings.
func memberRows(ms []*iamv1alpha1.Member) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.GetEmail()+" "+m.GetUsername()+" "+m.GetRole())
	}
	return out
}

// TestTenantOwnerManagesMembers: a tenant owner adds, lists, changes and
// removes members of the tenant and of its projects; each change holds for
// the next call; and adding someone who is not a platform member binds them
// and says so.
func TestTenantOwnerManagesMembers(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	const carol = "carol@example.com"
	e.setupTenant(alice, "acme", "web")
	a, b := e.as(alice), e.as(bob)

	add := must[iamv1alpha1.AddMemberResponse](t)(a.members.AddMember(ctx, addMember(projectScope("web"), "Bob@Example.com", "project.editor")))
	if m := add.GetMember(); m.GetEmail() != bob || m.GetUsername() != usernameOf(bob) || !m.GetPlatformMember() || m.GetRole() != "project.editor" {
		t.Errorf("adding bob answered %v", m)
	}
	if len(add.GetWarnings()) != 0 {
		t.Errorf("adding a platform member warned %q", add.GetWarnings())
	}
	bobBinding := add.GetMember().GetRoleBinding()
	must[computev1alpha1.ApplyContainerResponse](t)(b.containers.ApplyContainer(ctx, applyContainer(container("web", "by-bob", "nginx"))))

	// Someone not on the platform is bound all the same, and told about.
	add = must[iamv1alpha1.AddMemberResponse](t)(a.members.AddMember(ctx, addMember(projectScope("web"), carol, "project.viewer")))
	if m := add.GetMember(); m.GetPlatformMember() || m.GetUsername() != "" {
		t.Errorf("adding carol, not a platform member, answered %v", m)
	}
	if len(add.GetWarnings()) != 1 || !strings.Contains(add.GetWarnings()[0], "not a platform member yet") {
		t.Errorf("adding carol warned %q, want that she is not a platform member yet", add.GetWarnings())
	}

	_, err := a.members.AddMember(ctx, addMember(projectScope("web"), bob, "project.viewer"))
	wantCode(t, err, connect.CodeAlreadyExists, "adding bob again")

	list := must[iamv1alpha1.ListMembersResponse](t)(a.members.ListMembers(ctx, listMembers(projectScope("web"))))
	want := []string{bob + " " + usernameOf(bob) + " project.editor", carol + "  project.viewer"}
	if got := memberRows(list.GetMembers()); !slices.Equal(got, want) {
		t.Errorf("members of web: %q, want %q", got, want)
	}
	if s := list.GetMembers()[0].GetScope().GetProject(); s != "web" {
		t.Errorf("a member of web listed at scope %v", list.GetMembers()[0].GetScope())
	}
	list = must[iamv1alpha1.ListMembersResponse](t)(a.members.ListMembers(ctx, listMembers(tenantScope("acme"))))
	if got, want := memberRows(list.GetMembers()), []string{alice + " " + usernameOf(alice) + " tenant.owner"}; !slices.Equal(got, want) {
		t.Errorf("members of acme: %q, want %q", got, want)
	}

	// A change of role is the same binding with another role, and holds for
	// bob's very next call.
	set := must[iamv1alpha1.SetMemberRoleResponse](t)(a.members.SetMemberRole(ctx, setMemberRole(projectScope("web"), bob, "project.viewer")))
	if set.GetMember().GetRoleBinding() != bobBinding || set.GetMember().GetRole() != "project.viewer" {
		t.Errorf("changing bob's role answered %v, want binding %s with project.viewer", set.GetMember(), bobBinding)
	}
	_, err = b.containers.ApplyContainer(ctx, applyContainer(container("web", "by-bob-2", "nginx")))
	wantCode(t, err, connect.CodePermissionDenied, "bob writing as a viewer")
	must[computev1alpha1.GetContainerResponse](t)(b.containers.GetContainer(ctx, getContainer("web", "by-bob")))
	_, err = a.members.SetMemberRole(ctx, setMemberRole(projectScope("web"), bob, "tenant.owner"))
	wantCode(t, err, connect.CodeInvalidArgument, "binding a tenant role in a project")

	// Removal holds for the next call too.
	rm := must[iamv1alpha1.RemoveMemberResponse](t)(a.members.RemoveMember(ctx, removeMember(projectScope("web"), bob)))
	if len(rm.GetRemoved()) != 1 || rm.GetRemoved()[0].GetRoleBinding() != bobBinding {
		t.Errorf("removing bob removed %v", rm.GetRemoved())
	}
	_, err = b.containers.GetContainer(ctx, getContainer("web", "by-bob"))
	wantCode(t, err, connect.CodePermissionDenied, "bob reading once removed")
	_, err = a.members.RemoveMember(ctx, removeMember(projectScope("web"), bob))
	wantCode(t, err, connect.CodeNotFound, "removing bob twice")
	_, err = a.members.SetMemberRole(ctx, setMemberRole(projectScope("web"), bob, "project.owner"))
	wantCode(t, err, connect.CodeNotFound, "changing the role of someone who holds none")

	// In the tenant as well.
	must[iamv1alpha1.AddMemberResponse](t)(a.members.AddMember(ctx, addMember(tenantScope("acme"), bob, "tenant.viewer")))
	must[computev1alpha1.GetContainerResponse](t)(b.containers.GetContainer(ctx, getContainer("web", "by-bob")))
	must[iamv1alpha1.SetMemberRoleResponse](t)(a.members.SetMemberRole(ctx, setMemberRole(tenantScope("acme"), bob, "tenant.owner")))
	must[computev1alpha1.ApplyContainerResponse](t)(b.containers.ApplyContainer(ctx, applyContainer(container("web", "by-bob-3", "nginx"))))
	must[iamv1alpha1.RemoveMemberResponse](t)(a.members.RemoveMember(ctx, removeMember(tenantScope("acme"), bob)))
	_, err = b.containers.GetContainer(ctx, getContainer("web", "by-bob"))
	wantCode(t, err, connect.CodePermissionDenied, "bob reading once removed from the tenant")
}

// TestMembersGrantNoMoreThanBindings: the member calls are authorized as the
// RoleBinding calls they make. A project owner manages their project's
// members and nothing above or beside it; an editor and a viewer change
// none; outsiders learn nothing; the platform is out of scope.
func TestMembersGrantNoMoreThanBindings(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	const (
		carol = "carol@example.com"
		dave  = "dave@example.com"
		erin  = "erin@example.com"
	)
	for _, m := range []string{carol, dave, erin} {
		e.addMember(m)
	}
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, connect.NewRequest(project("db", "acme"))))
	must[iamv1alpha1.AddMemberResponse](t)(a.members.AddMember(ctx, addMember(projectScope("web"), bob, "project.owner")))
	must[iamv1alpha1.AddMemberResponse](t)(a.members.AddMember(ctx, addMember(projectScope("web"), carol, "project.editor")))
	must[iamv1alpha1.AddMemberResponse](t)(a.members.AddMember(ctx, addMember(projectScope("web"), dave, "project.viewer")))
	owner, editor, viewer, outsider := e.as(bob), e.as(carol), e.as(dave), e.as(erin)

	// The project owner manages the project.
	must[iamv1alpha1.AddMemberResponse](t)(owner.members.AddMember(ctx, addMember(projectScope("web"), erin, "project.owner")))
	must[iamv1alpha1.SetMemberRoleResponse](t)(owner.members.SetMemberRole(ctx, setMemberRole(projectScope("web"), erin, "project.viewer")))
	must[iamv1alpha1.RemoveMemberResponse](t)(owner.members.RemoveMember(ctx, removeMember(projectScope("web"), erin)))
	must[iamv1alpha1.ListMembersResponse](t)(owner.members.ListMembers(ctx, listMembers(projectScope("web"))))

	// And nothing above or beside it.
	_, err := owner.members.AddMember(ctx, addMember(tenantScope("acme"), erin, "tenant.viewer"))
	wantCode(t, err, connect.CodePermissionDenied, "a project owner adding to the tenant")
	_, err = owner.members.AddMember(ctx, addMember(projectScope("web"), erin, "tenant.owner"))
	wantCode(t, err, connect.CodeInvalidArgument, "a project owner binding a tenant role in the project")
	_, err = owner.members.AddMember(ctx, addMember(projectScope("db"), erin, "project.owner"))
	wantCode(t, err, connect.CodePermissionDenied, "a project owner adding to another project")
	_, err = owner.members.ListMembers(ctx, listMembers(tenantScope("acme")))
	wantCode(t, err, connect.CodePermissionDenied, "a project owner listing the tenant's members")
	_, err = owner.members.RemoveMember(ctx, removeMember(tenantScope("acme"), alice))
	wantCode(t, err, connect.CodePermissionDenied, "a project owner removing the tenant owner")
	for _, scope := range []*iamv1alpha1.BindingScope{platformScope(), nil} {
		_, err = a.members.AddMember(ctx, addMember(scope, erin, "platform.root"))
		wantCode(t, err, connect.CodeInvalidArgument, "adding at the platform or no scope")
	}

	// An editor and a viewer read the members but change none.
	for who, c := range map[string]clients{"editor": editor, "viewer": viewer} {
		must[iamv1alpha1.ListMembersResponse](t)(c.members.ListMembers(ctx, listMembers(projectScope("web"))))
		_, err = c.members.AddMember(ctx, addMember(projectScope("web"), erin, "project.viewer"))
		wantCode(t, err, connect.CodePermissionDenied, "a project "+who+" adding a member")
		_, err = c.members.SetMemberRole(ctx, setMemberRole(projectScope("web"), dave, "project.owner"))
		wantCode(t, err, connect.CodePermissionDenied, "a project "+who+" raising a member")
		_, err = c.members.RemoveMember(ctx, removeMember(projectScope("web"), bob))
		wantCode(t, err, connect.CodePermissionDenied, "a project "+who+" removing the owner")
	}

	// An outsider is refused alike whether or not the project, or the
	// person they name, is there.
	for _, p := range []string{"web", "nowhere"} {
		_, err = outsider.members.ListMembers(ctx, listMembers(projectScope(p)))
		wantCode(t, err, connect.CodePermissionDenied, "an outsider listing the members of "+p)
		for _, email := range []string{bob, "nobody@example.com"} {
			_, err = outsider.members.RemoveMember(ctx, removeMember(projectScope(p), email))
			wantCode(t, err, connect.CodePermissionDenied, "an outsider removing "+email+" from "+p)
		}
	}
}

// TestMembersAndBindingsAreOneThing: a RoleBinding applied by hand is a
// member, and is warned about in the same words; setting a role leaves a
// person who held several bindings at a scope with the one.
func TestMembersAndBindingsAreOneThing(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	const carol = "carol@example.com"
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)

	res := must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("web-carol", carol, "project.viewer", projectScope("web"))))
	if len(res.GetWarnings()) != 1 || !strings.Contains(res.GetWarnings()[0], "not a platform member yet") {
		t.Errorf("applying a binding for carol warned %q", res.GetWarnings())
	}
	res = must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("web-bob-1", bob, "project.viewer", projectScope("web"))))
	if len(res.GetWarnings()) != 0 {
		t.Errorf("applying a binding for bob, a platform member, warned %q", res.GetWarnings())
	}
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("web-bob-2", bob, "project.editor", projectScope("web"))))

	list := must[iamv1alpha1.ListMembersResponse](t)(a.members.ListMembers(ctx, listMembers(projectScope("web"))))
	if got, want := memberRows(list.GetMembers()), []string{
		bob + " " + usernameOf(bob) + " project.editor", bob + " " + usernameOf(bob) + " project.viewer", carol + "  project.viewer",
	}; !slices.Equal(got, want) {
		t.Errorf("members of web: %q, want %q", got, want)
	}

	set := must[iamv1alpha1.SetMemberRoleResponse](t)(a.members.SetMemberRole(ctx, setMemberRole(projectScope("web"), bob, "project.editor")))
	if set.GetMember().GetRoleBinding() != "web-bob-2" {
		t.Errorf("setting bob's role kept %s, want web-bob-2, which already had it", set.GetMember().GetRoleBinding())
	}
	// As root: to anyone else a missing binding reads as a refusal.
	_, err := e.as(root).bindings.GetRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.GetRoleBindingRequest{Name: "web-bob-1"}))
	wantCode(t, err, connect.CodeNotFound, "bob's other binding at the project")
	list = must[iamv1alpha1.ListMembersResponse](t)(a.members.ListMembers(ctx, listMembers(projectScope("web"))))
	if got, want := memberRows(list.GetMembers()), []string{bob + " " + usernameOf(bob) + " project.editor", carol + "  project.viewer"}; !slices.Equal(got, want) {
		t.Errorf("members of web after setting bob's role: %q, want %q", got, want)
	}

	// Once carol is a platform member, her binding counts, and the listing
	// shows her username.
	e.addMember(carol)
	list = must[iamv1alpha1.ListMembersResponse](t)(a.members.ListMembers(ctx, listMembers(projectScope("web"))))
	if m := list.GetMembers()[1]; m.GetUsername() != usernameOf(carol) || !m.GetPlatformMember() {
		t.Errorf("carol, signed up, listed as %v", m)
	}
	must[tenancyv1alpha1.GetProjectResponse](t)(e.as(carol).projects.GetProject(ctx, connect.NewRequest(&tenancyv1alpha1.GetProjectRequest{Name: "web"})))
}

// TestListingReachesOnlyGrantedTenantsAndProjects: a person lists the
// tenants and projects they hold a role in, and no others; a project role
// lists the project, not its tenant.
func TestListingReachesOnlyGrantedTenantsAndProjects(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	const carol = "carol@example.com"
	e.addMember(carol)
	e.setupTenant(alice, "acme", "web")
	e.setupTenant(bob, "globex", "shop")
	a := e.as(alice)
	must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, connect.NewRequest(project("db", "acme"))))
	must[iamv1alpha1.AddMemberResponse](t)(a.members.AddMember(ctx, addMember(projectScope("web"), carol, "project.viewer")))

	names := func(c clients) (tenants, projects []string) {
		t.Helper()
		for _, x := range must[tenancyv1alpha1.ListTenantsResponse](t)(c.tenants.ListTenants(ctx, connect.NewRequest(&tenancyv1alpha1.ListTenantsRequest{}))).GetItems() {
			tenants = append(tenants, x.GetMetadata().GetName())
		}
		for _, x := range must[tenancyv1alpha1.ListProjectsResponse](t)(c.projects.ListProjects(ctx, connect.NewRequest(&tenancyv1alpha1.ListProjectsRequest{}))).GetItems() {
			projects = append(projects, x.GetMetadata().GetName())
		}
		slices.Sort(tenants)
		slices.Sort(projects)
		return tenants, projects
	}
	for _, tc := range []struct {
		who               string
		tenants, projects []string
	}{
		{alice, []string{"acme"}, []string{"db", "web"}},
		{bob, []string{"globex"}, []string{"shop"}},
		{carol, nil, []string{"web"}},
	} {
		tenants, projects := names(e.as(tc.who))
		if !slices.Equal(tenants, tc.tenants) || !slices.Equal(projects, tc.projects) {
			t.Errorf("%s lists tenants %q and projects %q, want %q and %q", tc.who, tenants, projects, tc.tenants, tc.projects)
		}
	}
	// Platform roles reach everything.
	if tenants, projects := names(e.as(root)); len(tenants) != 2 || len(projects) != 3 {
		t.Errorf("platform root lists tenants %q and projects %q", tenants, projects)
	}
}
