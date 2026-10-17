package apiserver_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/authz"
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
	// The name is made from the scope and the email alone, and must not
	// change between releases: an add of the same person must collide.
	if want := "member-65b55fe97b92018bc224b631"; bobBinding != want {
		t.Errorf("bob's binding in web is named %s, want %s", bobBinding, want)
	}
	must[computev1alpha1.ApplyContainerResponse](t)(b.containers.ApplyContainer(ctx, applyContainer(container("web", "by-bob", "nginx"))))

	// Someone not on the platform is bound all the same, and told about.
	add = must[iamv1alpha1.AddMemberResponse](t)(a.members.AddMember(ctx, addMember(projectScope("web"), carol, "project.viewer")))
	if m := add.GetMember(); m.GetPlatformMember() || m.GetUsername() != "" {
		t.Errorf("adding carol, not a platform member, answered %v", m)
	}
	if len(add.GetWarnings()) != 1 || !strings.Contains(add.GetWarnings()[0], "not a platform member yet") {
		t.Errorf("adding carol warned %q, want that she is not a platform member yet", add.GetWarnings())
	}

	// Someone invited who has not signed up is told about in the same
	// words: whether an email was invited is not the tenant's to learn.
	const frank = "frank@example.com"
	e.invite(frank, "platform.member")
	invited := must[iamv1alpha1.AddMemberResponse](t)(a.members.AddMember(ctx, addMember(projectScope("web"), frank, "project.viewer")))
	if invited.GetMember().GetPlatformMember() || len(invited.GetWarnings()) != 1 ||
		strings.ReplaceAll(invited.GetWarnings()[0], frank, "") != strings.ReplaceAll(add.GetWarnings()[0], carol, "") {
		t.Errorf("adding frank, invited but not signed up, answered %v with warnings %q; carol's were %q",
			invited.GetMember(), invited.GetWarnings(), add.GetWarnings())
	}
	must[iamv1alpha1.RemoveMemberResponse](t)(a.members.RemoveMember(ctx, removeMember(projectScope("web"), frank)))

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
	_, err = a.members.SetMemberRole(ctx, setMemberRole(tenantScope("acme"), bob, "platform.root"))
	wantCode(t, err, connect.CodeInvalidArgument, "setting a platform role at a tenant")
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
		frank = "frank@example.com"
		grace = "grace@example.com"
	)
	for _, m := range []string{carol, dave, erin, frank, grace} {
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

	// Owning another tenant reaches none of this one.
	e.setupTenant(frank, "initech", "app")
	other := e.as(frank)
	_, err = other.members.ListMembers(ctx, listMembers(projectScope("web")))
	wantCode(t, err, connect.CodePermissionDenied, "another tenant's owner listing the members of web")
	_, err = other.members.AddMember(ctx, addMember(projectScope("web"), frank, "project.owner"))
	wantCode(t, err, connect.CodePermissionDenied, "another tenant's owner adding to web")
	_, err = other.members.RemoveMember(ctx, removeMember(projectScope("web"), bob))
	wantCode(t, err, connect.CodePermissionDenied, "another tenant's owner removing from web")

	// A tenant viewer reads the tenant's members but changes none.
	must[iamv1alpha1.AddMemberResponse](t)(a.members.AddMember(ctx, addMember(tenantScope("acme"), grace, "tenant.viewer")))
	tv := e.as(grace)
	must[iamv1alpha1.ListMembersResponse](t)(tv.members.ListMembers(ctx, listMembers(tenantScope("acme"))))
	_, err = tv.members.AddMember(ctx, addMember(tenantScope("acme"), erin, "tenant.viewer"))
	wantCode(t, err, connect.CodePermissionDenied, "a tenant viewer adding a member")
	_, err = tv.members.SetMemberRole(ctx, setMemberRole(tenantScope("acme"), grace, "tenant.owner"))
	wantCode(t, err, connect.CodePermissionDenied, "a tenant viewer raising themselves")
	_, err = tv.members.RemoveMember(ctx, removeMember(tenantScope("acme"), alice))
	wantCode(t, err, connect.CodePermissionDenied, "a tenant viewer removing the owner")
	_, err = tv.members.AddMember(ctx, addMember(projectScope("web"), erin, "project.viewer"))
	wantCode(t, err, connect.CodePermissionDenied, "a tenant viewer adding to a project")

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
// tenants and projects they hold a role in, and no others: the tenant and
// project signing up opened for them, and what they were granted; a project
// role lists the project, not its tenant.
func TestListingReachesOnlyGrantedTenantsAndProjects(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	const (
		carol = "carol@example.com"
		erin  = "erin@example.com"
	)
	e.addMember(carol)
	e.addMember(erin)
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

	// Signing up opened a tenant, with one project, for each person: as
	// platform.root, tell whose each is from the tenant's opener.
	r := e.as(root)
	personalTenant, personalProject := map[string]string{}, map[string]string{}
	for _, x := range must[tenancyv1alpha1.ListTenantsResponse](t)(r.tenants.ListTenants(ctx, connect.NewRequest(&tenancyv1alpha1.ListTenantsRequest{}))).GetItems() {
		if opener, ok := strings.CutPrefix(x.GetMetadata().GetAnnotations()[apiserver.OpenedBy], "user:"); ok && x.GetMetadata().GetName() != "acme" && x.GetMetadata().GetName() != "globex" {
			personalTenant[opener] = x.GetMetadata().GetName()
		}
	}
	for _, x := range must[tenancyv1alpha1.ListProjectsResponse](t)(r.projects.ListProjects(ctx, connect.NewRequest(&tenancyv1alpha1.ListProjectsRequest{}))).GetItems() {
		for who, tenant := range personalTenant {
			if x.GetSpec().GetTenant() == tenant {
				personalProject[who] = x.GetMetadata().GetName()
			}
		}
	}
	people := []string{root, alice, bob, carol, erin}
	for _, who := range people {
		if personalTenant[who] == "" || personalProject[who] == "" {
			t.Fatalf("%s has no tenant and project of their own: tenants %v, projects %v", who, personalTenant, personalProject)
		}
	}
	// sorted is a person's own tenant or project with what else they reach.
	sorted := func(own string, granted ...string) []string {
		out := append([]string{own}, granted...)
		slices.Sort(out)
		return out
	}

	for _, tc := range []struct {
		who               string
		tenants, projects []string
	}{
		{alice, []string{"acme"}, []string{"db", "web"}},
		{bob, []string{"globex"}, []string{"shop"}},
		{carol, nil, []string{"web"}},
		// platform.member reaches nothing beyond their own.
		{erin, nil, nil},
	} {
		tenants, projects := names(e.as(tc.who))
		wantTenants, wantProjects := sorted(personalTenant[tc.who], tc.tenants...), sorted(personalProject[tc.who], tc.projects...)
		if !slices.Equal(tenants, wantTenants) || !slices.Equal(projects, wantProjects) {
			t.Errorf("%s lists tenants %q and projects %q, want %q and %q", tc.who, tenants, projects, wantTenants, wantProjects)
		}
	}
	// platform.root reaches everything: every person's own and the two
	// opened by hand.
	if tenants, projects := names(r); len(tenants) != len(people)+2 || len(projects) != len(people)+3 {
		t.Errorf("platform root lists tenants %q and projects %q", tenants, projects)
	}
}

// TestATenantKeepsAnOwner: no one but platform.root leaves a tenant without
// a tenant.owner binding, through MemberService or RoleBindings; a call that
// writes several bindings is refused before it writes any; and deleting the
// tenant takes its owners with it.
func TestATenantKeepsAnOwner(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	acme := tenantScope("acme")
	owners := must[iamv1alpha1.ListMembersResponse](t)(a.members.ListMembers(ctx, listMembers(acme))).GetMembers()
	if len(owners) != 1 {
		t.Fatalf("acme's members: %v", owners)
	}
	aliceBinding := owners[0].GetRoleBinding()
	getBinding := func(name string) *iamv1alpha1.RoleBinding {
		t.Helper()
		return must[iamv1alpha1.GetRoleBindingResponse](t)(e.as(root).bindings.GetRoleBinding(ctx,
			connect.NewRequest(&iamv1alpha1.GetRoleBindingRequest{Name: name}))).GetRoleBinding()
	}

	// The last owner stays, by every path.
	_, err := a.members.RemoveMember(ctx, removeMember(acme, alice))
	wantCode(t, err, connect.CodeFailedPrecondition, "removing the last owner")
	_, err = a.members.SetMemberRole(ctx, setMemberRole(acme, alice, "tenant.viewer"))
	wantCode(t, err, connect.CodeFailedPrecondition, "lowering the last owner")
	_, err = a.bindings.DeleteRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: aliceBinding}))
	wantCode(t, err, connect.CodeFailedPrecondition, "deleting the last owner's binding")
	lowered := getBinding(aliceBinding)
	lowered.Spec.Role = "tenant.viewer"
	_, err = a.bindings.ApplyRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.ApplyRoleBindingRequest{RoleBinding: lowered}))
	wantCode(t, err, connect.CodeFailedPrecondition, "applying the last owner's binding with another role")
	if r := getBinding(aliceBinding).GetSpec().GetRole(); r != "tenant.owner" {
		t.Errorf("the last owner's binding has role %s", r)
	}

	// With another owner, either may step down.
	must[iamv1alpha1.AddMemberResponse](t)(a.members.AddMember(ctx, addMember(acme, bob, "tenant.owner")))
	must[iamv1alpha1.SetMemberRoleResponse](t)(a.members.SetMemberRole(ctx, setMemberRole(acme, alice, "tenant.viewer")))
	b := e.as(bob)
	_, err = b.members.RemoveMember(ctx, removeMember(acme, bob))
	wantCode(t, err, connect.CodeFailedPrecondition, "the new last owner removing themselves")

	// Several bindings of the last owner: refused before any is written.
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(b.bindings.ApplyRoleBinding(ctx, binding("acme-bob-2", bob, "tenant.owner", acme)))
	held := must[iamv1alpha1.ListMembersResponse](t)(b.members.ListMembers(ctx, listMembers(acme))).GetMembers()
	_, err = b.members.SetMemberRole(ctx, setMemberRole(acme, bob, "tenant.viewer"))
	wantCode(t, err, connect.CodeFailedPrecondition, "lowering the last owner, who holds two bindings")
	_, err = b.members.RemoveMember(ctx, removeMember(acme, bob))
	wantCode(t, err, connect.CodeFailedPrecondition, "removing the last owner, who holds two bindings")
	for _, m := range held {
		if got := getBinding(m.GetRoleBinding()).GetSpec().GetRole(); got != m.GetRole() {
			t.Errorf("binding %s has role %s after refused calls, want %s", m.GetRoleBinding(), got, m.GetRole())
		}
	}
	// Dropping one of the two leaves an owner.
	must[iamv1alpha1.DeleteRoleBindingResponse](t)(b.bindings.DeleteRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: "acme-bob-2"})))

	// platform.root may leave a tenant without an owner.
	must[iamv1alpha1.RemoveMemberResponse](t)(e.as(root).members.RemoveMember(ctx, removeMember(acme, bob)))

	// Deleting a tenant takes its last owner with it.
	e.setupTenant(alice, "globex", "shop")
	globexOwner := must[iamv1alpha1.ListMembersResponse](t)(a.members.ListMembers(ctx, listMembers(tenantScope("globex")))).GetMembers()[0].GetRoleBinding()
	must[tenancyv1alpha1.DeleteTenantResponse](t)(a.tenants.DeleteTenant(ctx, connect.NewRequest(&tenancyv1alpha1.DeleteTenantRequest{Name: "globex"})))
	e.waitSettled()
	_, err = e.as(root).bindings.GetRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.GetRoleBindingRequest{Name: globexOwner}))
	wantCode(t, err, connect.CodeNotFound, "the deleted tenant's owner binding")
	_, err = e.as(root).tenants.GetTenant(ctx, connect.NewRequest(&tenancyv1alpha1.GetTenantRequest{Name: "globex"}))
	wantCode(t, err, connect.CodeNotFound, "the deleted tenant")
}

// tokenOwner binds a service token tenant.owner of a tenant.
func tokenOwner(name, token, tenant string) *connect.Request[iamv1alpha1.ApplyRoleBindingRequest] {
	return connect.NewRequest(&iamv1alpha1.ApplyRoleBindingRequest{RoleBinding: &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_ServiceToken{ServiceToken: token}},
			Role:    "tenant.owner",
			Scope:   tenantScope(tenant),
		},
	}})
}

// TestAServiceTokenIsNoOwner: a service token bound tenant.owner does not
// count as the tenant's owner, so the last person who owns it stays, and
// deleting the token, or the project it is in, leaves them owner.
func TestAServiceTokenIsNoOwner(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	acme := tenantScope("acme")
	must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, connect.NewRequest(project("db", "acme"))))
	for _, tok := range []struct{ project, name, binding string }{{"web", "deployer", "acme-deployer"}, {"db", "ci", "acme-ci"}} {
		must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, connect.NewRequest(&iamv1alpha1.CreateServiceTokenRequest{Project: tok.project, Name: tok.name})))
		must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, tokenOwner(tok.binding, tok.project+"/"+tok.name, "acme")))
	}
	owners := must[iamv1alpha1.ListMembersResponse](t)(a.members.ListMembers(ctx, listMembers(acme))).GetMembers()
	if len(owners) != 1 || owners[0].GetEmail() != alice {
		t.Fatalf("acme's members: %v, want alice alone", owners)
	}
	aliceBinding := owners[0].GetRoleBinding()

	// Alice is the last person who owns acme, whatever the tokens hold.
	_, err := a.members.RemoveMember(ctx, removeMember(acme, alice))
	wantCode(t, err, connect.CodeFailedPrecondition, "removing the last person owner beside token owners")
	_, err = a.members.SetMemberRole(ctx, setMemberRole(acme, alice, "tenant.viewer"))
	wantCode(t, err, connect.CodeFailedPrecondition, "lowering the last person owner beside token owners")
	_, err = a.bindings.DeleteRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: aliceBinding}))
	wantCode(t, err, connect.CodeFailedPrecondition, "deleting the last person owner's binding beside token owners")

	// A token's owner binding is deleted with it, and with its project.
	must[iamv1alpha1.DeleteServiceTokenResponse](t)(a.tokens.DeleteServiceToken(ctx, connect.NewRequest(&iamv1alpha1.DeleteServiceTokenRequest{Project: "web", Name: "deployer"})))
	must[tenancyv1alpha1.DeleteProjectResponse](t)(a.projects.DeleteProject(ctx, connect.NewRequest(&tenancyv1alpha1.DeleteProjectRequest{Name: "db"})))
	e.waitSettled()
	r := e.as(root)
	for _, name := range []string{"acme-deployer", "acme-ci"} {
		_, err := r.bindings.GetRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.GetRoleBindingRequest{Name: name}))
		wantCode(t, err, connect.CodeNotFound, "the deleted token's binding "+name)
	}
	if got := must[iamv1alpha1.GetRoleBindingResponse](t)(r.bindings.GetRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.GetRoleBindingRequest{Name: aliceBinding}))); got.GetRoleBinding().GetSpec().GetRole() != "tenant.owner" {
		t.Errorf("alice's binding after the tokens went: %v", got.GetRoleBinding())
	}
	must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, connect.NewRequest(project("more", "acme"))))
}

// binderRole may write and read role bindings, but not delete them.
const binderRole = "test.binder"

// TestSetMemberRoleNeedsDeleteUpFront: changing the role of someone who holds
// several bindings deletes all but one, so a caller who may apply bindings
// but not delete them is refused before anything is written.
func TestSetMemberRoleNeedsDeleteUpFront(t *testing.T) {
	e := newEnv(t, func(c *apiserver.Config) {
		c.ExtraRoles = map[string]authz.Role{binderRole: authz.NewRole(authz.TenantLevel,
			authz.Permission(registry.RoleBinding, authz.Apply), authz.Permission(registry.RoleBinding, authz.List),
			authz.Permission(registry.RoleBinding, authz.Get))}
	})
	ctx := t.Context()
	const carol = "carol@example.com"
	e.addMember(carol)
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("acme-carol", carol, binderRole, tenantScope("acme"))))
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("web-bob-1", bob, "project.editor", projectScope("web"))))
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("web-bob-2", bob, "project.viewer", projectScope("web"))))

	_, err := e.as(carol).members.SetMemberRole(ctx, setMemberRole(projectScope("web"), bob, "project.owner"))
	wantCode(t, err, connect.CodePermissionDenied, "changing the role of bob, who holds two bindings, without delete")
	list := must[iamv1alpha1.ListMembersResponse](t)(a.members.ListMembers(ctx, listMembers(projectScope("web"))))
	if got, want := memberRows(list.GetMembers()), []string{
		bob + " " + usernameOf(bob) + " project.editor", bob + " " + usernameOf(bob) + " project.viewer",
	}; !slices.Equal(got, want) {
		t.Errorf("bob's bindings after a refused change: %q, want %q", got, want)
	}
}

// TestOwnersCannotRemoveEachOtherAtOnce: two tenant owners each taking the
// other's ownership at the same moment, through MemberService or the
// RoleBinding calls, leave one owner: exactly one of the two calls succeeds.
func TestOwnersCannotRemoveEachOtherAtOnce(t *testing.T) {
	acme := tenantScope("acme")
	people := []string{alice, bob}
	for _, tc := range []struct {
		how string
		// take has c take away other's ownership, whose binding is named rb.
		take func(ctx context.Context, c clients, other, rb string) error
	}{
		{"RemoveMember", func(ctx context.Context, c clients, other, _ string) error {
			_, err := c.members.RemoveMember(ctx, removeMember(acme, other))
			return err
		}},
		{"DeleteRoleBinding", func(ctx context.Context, c clients, _, rb string) error {
			_, err := c.bindings.DeleteRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: rb}))
			return err
		}},
		{"ApplyRoleBinding", func(ctx context.Context, c clients, other, rb string) error {
			_, err := c.bindings.ApplyRoleBinding(ctx, binding(rb, other, "tenant.viewer", acme))
			return err
		}},
	} {
		t.Run(tc.how, func(t *testing.T) {
			e := newEnv(t)
			ctx := t.Context()
			e.setupTenant(alice, "acme", "web")
			must[iamv1alpha1.AddMemberResponse](t)(e.as(alice).members.AddMember(ctx, addMember(acme, bob, "tenant.owner")))
			// bindingOf returns the name of a person's binding at acme.
			bindingOf := func(email string) string {
				t.Helper()
				for _, m := range must[iamv1alpha1.ListMembersResponse](t)(e.as(root).members.ListMembers(ctx, listMembers(acme))).GetMembers() {
					if m.GetEmail() == email {
						return m.GetRoleBinding()
					}
				}
				t.Fatalf("%s holds no role in acme", email)
				return ""
			}
			for round := range 30 {
				names := []string{bindingOf(alice), bindingOf(bob)}
				var wg sync.WaitGroup
				start := make(chan struct{})
				errs := make([]error, 2)
				for i, who := range people {
					wg.Go(func() {
						<-start
						errs[i] = tc.take(ctx, e.as(who), people[1-i], names[1-i])
					})
				}
				close(start)
				wg.Wait()
				switch {
				case errs[0] == nil && errs[1] == nil:
					t.Fatalf("round %d: both owners took the other's ownership", round)
				case errs[0] != nil && errs[1] != nil:
					t.Fatalf("round %d: neither owner took the other's: %v; %v", round, errs[0], errs[1])
				}
				// The owner left makes the other owner again.
				left := 0
				if errs[0] != nil {
					left = 1
				}
				c := e.as(people[left])
				if _, err := c.members.SetMemberRole(ctx, setMemberRole(acme, people[1-left], "tenant.owner")); connect.CodeOf(err) == connect.CodeNotFound {
					must[iamv1alpha1.AddMemberResponse](t)(c.members.AddMember(ctx, addMember(acme, people[1-left], "tenant.owner")))
				} else if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
