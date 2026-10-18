package apiserver_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
)

// The permissions the table tests, among them those whose real check is not
// the bare role table: a system-managed kind's writes, a kind never deleted,
// a kind every member reads, kinds that live at the platform, and the
// machine capabilities and endpoint access, which are no write to their kind.
const (
	cget    = "compute.containers.get"
	capply  = "compute.containers.apply"
	rbapply = "iam.rolebindings.apply"
	exec    = "compute.machines.exec"
	logs    = "compute.machines.logs"
	ssh     = "compute.machines.ssh"
	access  = "network.publicendpoints.access"
	pdelete = "tenancy.projects.delete"
	tapply  = "tenancy.tenants.apply"
	tcreate = "tenancy.tenants.create"
	mapply  = "compute.machines.apply"
	iget    = "storage.images.get"
	iapply  = "storage.images.apply"
	msget   = "compute.machinesizes.get"
	msapply = "compute.machinesizes.apply"
	msdel   = "compute.machinesizes.delete"
	msstat  = "compute.machinesizes.updateStatus"
	udelete = "iam.users.delete"
	cstatus = "compute.containers.updateStatus"
	stapply = "iam.servicetokens.apply"
)

var tested = []string{cget, capply, rbapply, exec, logs, ssh, access, pdelete, tapply, tcreate, mapply, iget, iapply, msget, msapply, msdel, msstat, udelete, cstatus, stapply}

func testPermissions(c clients, scope *iamv1alpha1.BindingScope, perms ...string) ([]string, error) {
	res, err := c.accounts.TestPermissions(context.Background(), connect.NewRequest(&iamv1alpha1.TestPermissionsRequest{Scope: scope, Permissions: perms}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetPermissions(), nil
}

// permEnv is a tenant acme, with projects web and db, and someone holding
// each built-in role, and a service token, around it; and a tenant other,
// with project hidden, that only bob reaches.
type permEnv struct {
	*env
	callers map[string]clients
}

func newPermEnv(t *testing.T) permEnv {
	e := newEnv(t)
	ctx := t.Context()
	const (
		sam   = "sam@example.com"
		carol = "carol@example.com"
		dave  = "dave@example.com"
		erin  = "erin@example.com"
		frank = "frank@example.com"
	)
	e.invite(sam, "platform.sponsor")
	e.signUp(sam)
	for _, p := range []string{carol, dave, erin, frank} {
		e.addMember(p)
	}
	e.setupTenant(alice, "acme", "web")
	e.setupTenant(bob, "other", "hidden")
	a := e.as(alice)
	must[tenancyv1alpha1.ApplyProjectResponse](t)(a.projects.ApplyProject(ctx, connect.NewRequest(project("db", "acme"))))
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("db", "pg", "postgres"))))
	must[computev1alpha1.ApplyContainerResponse](t)(a.containers.ApplyContainer(ctx, applyContainer(container("web", "logged", "nginx"))))
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("acme-carol", carol, "tenant.viewer", tenantScope("acme"))))
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("web-dave", dave, "project.owner", projectScope("web"))))
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("web-erin", erin, "project.editor", projectScope("web"))))
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("web-frank", frank, "project.viewer", projectScope("web"))))
	tok := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("web", "deployer", "project.editor")))
	return permEnv{env: e, callers: map[string]clients{
		"platform.root":    e.as(root),
		"platform.sponsor": e.as(sam),
		"platform.member":  e.as(bob),
		"tenant.owner":     e.as(alice),
		"tenant.viewer":    e.as(carol),
		"project.owner":    e.as(dave),
		"project.editor":   e.as(erin),
		"project.viewer":   e.as(frank),
		"serviceToken":     e.withToken(tok.GetSecret()),
	}}
}

// TestTestPermissionsByRole pins, for each role, what TestPermissions
// answers at each scope: the subset in the order asked, from the role's
// grants and their inheritance, with nothing at a scope the caller cannot
// see, alike whether it exists.
func TestTestPermissionsByRole(t *testing.T) {
	e := newPermEnv(t)
	var (
		rootAll   = []string{cget, capply, rbapply, exec, logs, ssh, access, pdelete, tapply, tcreate, iget, iapply, msget, msapply, msdel, stapply}
		owner     = []string{cget, capply, rbapply, exec, logs, ssh, access, pdelete, tapply, tcreate, iget, msget, stapply}
		member    = []string{tcreate, iget, msget}
		viewer    = []string{cget, tcreate, iget, msget}
		projOwner = []string{cget, capply, rbapply, exec, logs, ssh, access, pdelete, tcreate, iget, msget, stapply}
		editor    = []string{cget, capply, exec, logs, ssh, access, tcreate, iget, msget}
		token     = []string{cget, capply, exec, logs, ssh, access, iget, msget}
		none      []string
	)
	scopes := []struct {
		name  string
		scope *iamv1alpha1.BindingScope
	}{
		{"platform", platformScope()},
		{"tenant acme", tenantScope("acme")},
		{"project web", projectScope("web")},
		{"project db", projectScope("db")},
		{"tenant other", tenantScope("other")},
		{"project hidden", projectScope("hidden")},
		{"tenant nosuch", tenantScope("nosuch")},
		{"project nosuch", projectScope("nosuch")},
	}
	// Columns in the order of scopes.
	want := map[string][8][]string{
		"platform.root":    {rootAll, rootAll, rootAll, rootAll, rootAll, rootAll, none, none},
		"platform.sponsor": {member, none, none, none, none, none, none, none},
		"platform.member":  {member, none, none, none, owner, owner, none, none},
		"tenant.owner":     {member, owner, owner, owner, none, none, none, none},
		"tenant.viewer":    {member, viewer, viewer, viewer, none, none, none, none},
		"project.owner":    {member, none, projOwner, none, none, none, none, none},
		"project.editor":   {member, none, editor, none, none, none, none, none},
		"project.viewer":   {member, none, viewer, none, none, none, none, none},
		"serviceToken":     {{iget, msget}, none, token, none, none, none, none, none},
	}
	if len(want) != len(e.callers) {
		t.Fatalf("%d callers, %d pinned", len(e.callers), len(want))
	}
	for role, row := range want {
		for i, sc := range scopes {
			got, err := testPermissions(e.callers[role], sc.scope, tested...)
			if err != nil {
				t.Errorf("%s at %s: %v", role, sc.name, err)
				continue
			}
			if !slices.Equal(got, row[i]) {
				t.Errorf("%s at %s:\n got %v\nwant %v", role, sc.name, got, row[i])
			}
		}
	}
}

// TestTestPermissionsAgreesWithCalls: for each role, what TestPermissions
// answers is what the call it names does: the call gets past authorization
// when the permission is held, and is refused with PermissionDenied when not.
func TestTestPermissionsAgreesWithCalls(t *testing.T) {
	e := newPermEnv(t)
	ctx := t.Context()
	e.addMember("probe@example.com")
	must[computev1alpha1.ApplyMachineSizeResponse](t)(e.as(root).sizes.ApplyMachineSize(ctx, applySize(machineSize("probe", "Probe", "1", "1Gi", "5Gi", 0))))
	i := 0
	for role, c := range e.callers {
		i++
		calls := []struct {
			scope *iamv1alpha1.BindingScope
			perm  string
			call  func() error
			// authorized is the code a call that got past authorization
			// fails with for want of anything else, besides success.
			authorized connect.Code
		}{
			{projectScope("web"), capply, func() error {
				// Of the probe size, which admission holds a tenant's machine to.
				ct := container("web", fmt.Sprintf("probe-%d", i), "nginx")
				ct.Spec.Resources = &computev1alpha1.Resources{Cpu: "1", Memory: "1Gi", Disk: "5Gi"}
				_, err := c.containers.ApplyContainer(ctx, applyContainer(ct))
				return err
			}, 0},
			{projectScope("web"), rbapply, func() error {
				_, err := c.bindings.ApplyRoleBinding(ctx, binding(fmt.Sprintf("web-probe-%d", i), "probe@example.com", "project.viewer", projectScope("web")))
				return err
			}, 0},
			{projectScope("db"), cget, func() error {
				_, err := c.containers.GetContainer(ctx, getContainer("db", "pg"))
				return err
			}, 0},
			{tenantScope("acme"), tapply, func() error {
				_, err := c.tenants.ApplyTenant(ctx, connect.NewRequest(tenant("acme")))
				return err
			}, 0},
			{projectScope("web"), mapply, func() error {
				_, err := c.machines.ApplyMachine(ctx, connect.NewRequest(&computev1alpha1.ApplyMachineRequest{Machine: &computev1alpha1.Machine{
					Metadata: container("web", fmt.Sprintf("probe-%d", i), "").GetMetadata(),
				}}))
				return err
			}, 0},
			{platformScope(), msapply, func() error {
				_, err := c.sizes.ApplyMachineSize(ctx, applySize(machineSize(fmt.Sprintf("probe-%d", i), "P", "1", "1Gi", "5Gi", 0)))
				return err
			}, 0},
			{platformScope(), msget, func() error {
				_, err := c.sizes.GetMachineSize(ctx, connect.NewRequest(&computev1alpha1.GetMachineSizeRequest{Name: "probe"}))
				return err
			}, 0},
			// No role grants a size's status, root's included.
			{platformScope(), msstat, func() error {
				_, err := c.sizes.UpdateMachineSizeStatus(ctx, connect.NewRequest(&computev1alpha1.UpdateMachineSizeStatusRequest{
					Name: "probe", Status: &computev1alpha1.MachineSizeStatus{ObservedGeneration: 1},
				}))
				return err
			}, 0},
			// The container has no machine, as the controllers do not run here,
			// so a caller who may read its logs is told so.
			{projectScope("web"), logs, func() error {
				stream, err := c.capability.Logs(ctx, connect.NewRequest(&computev1alpha1.LogsRequest{Project: "web", Kind: registry.Container.Kind, Name: "logged"}))
				if err != nil {
					return err
				}
				defer func() { _ = stream.Close() }()
				for stream.Receive() {
				}
				return stream.Err()
			}, connect.CodeFailedPrecondition},
		}
		for _, call := range calls {
			got, err := testPermissions(c, call.scope, call.perm)
			if err != nil {
				t.Fatalf("%s: %v", role, err)
			}
			held := slices.Contains(got, call.perm)
			err = call.call()
			passed := err == nil || (call.authorized != 0 && connect.CodeOf(err) == call.authorized)
			refused := connect.CodeOf(err) == connect.CodePermissionDenied
			switch {
			case !passed && !refused:
				t.Errorf("%s: %s: the call failed other than by authorization: %v", role, call.perm, err)
			case passed && !held:
				t.Errorf("%s: TestPermissions says no %s, and the call got past authorization (%v)", role, call.perm, err)
			case refused && held:
				t.Errorf("%s: TestPermissions says %s, and the call was refused: %v", role, call.perm, err)
			}
		}
	}
}

// TestTestPermissionsChecksItsRequest: unknown names and too many are
// refused, duplicates are answered once in the order first asked, and only
// someone who may use Hangar may ask. Asking writes nothing.
func TestTestPermissionsChecksItsRequest(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	a := e.as(alice)

	var all []string
	for _, r := range registry.All {
		for _, v := range []string{"get", "list", "watch", "apply", "delete", "updateStatus"} {
			all = append(all, r.Group+"."+r.Plural+"."+v)
		}
	}
	all = append(all, tcreate, "compute.machines.logs", exec, "compute.machines.ssh", "network.publicendpoints.access")
	for chunk := range slices.Chunk(all, 100) {
		if _, err := testPermissions(e.as(root), platformScope(), chunk...); err != nil {
			t.Errorf("asking for known permissions: %v", err)
		}
	}

	_, err := testPermissions(a, projectScope("web"), cget, "compute.containers.aply")
	wantCode(t, err, connect.CodeInvalidArgument, "a misspelt permission")
	_, err = testPermissions(a, projectScope("web"), "compute.containers.logs")
	wantCode(t, err, connect.CodeInvalidArgument, "a verb on the wrong kind")
	_, err = testPermissions(a, projectScope("web"), all[:101]...)
	wantCode(t, err, connect.CodeInvalidArgument, "101 permissions")
	_, err = testPermissions(a, projectScope("web"))
	wantCode(t, err, connect.CodeInvalidArgument, "no permissions")
	_, err = testPermissions(a, nil, cget)
	wantCode(t, err, connect.CodeInvalidArgument, "no scope")
	_, err = testPermissions(a, projectScope("Not A Name"), cget)
	wantCode(t, err, connect.CodeInvalidArgument, "an invalid project name")

	got, err := testPermissions(a, projectScope("web"), iget, udelete, cget, iget, cget)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{iget, cget}; !slices.Equal(got, want) {
		t.Errorf("duplicates: got %v, want %v", got, want)
	}

	const gus = "gus@example.com"
	e.invite(gus, "platform.member")
	_, err = testPermissions(e.as(gus), platformScope(), tcreate)
	wantCode(t, err, connect.CodePermissionDenied, "someone invited who has not signed up")
	_, err = testPermissions(e.as("stranger@example.com"), platformScope(), tcreate)
	wantCode(t, err, connect.CodePermissionDenied, "someone never invited")
	_, err = testPermissions(e.as(""), platformScope(), tcreate)
	wantCode(t, err, connect.CodeUnauthenticated, "no credentials")

	before, err := e.store.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range []*iamv1alpha1.BindingScope{platformScope(), tenantScope("acme"), projectScope("web"), projectScope("nosuch")} {
		if _, err := testPermissions(a, sc, tested...); err != nil {
			t.Fatal(err)
		}
	}
	if after, err := e.store.Revision(ctx); err != nil || after != before {
		t.Errorf("store revision %d before asking, %d after (%v)", before, after, err)
	}
}
