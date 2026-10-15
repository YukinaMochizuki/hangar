// Package authz decides whether a principal may perform an action on a
// target. Permissions are "<group>.<plural>.<verb>", built-in roles bundle
// them, and RoleBindings grant a role to a subject at a scope. See
// docs/decisions/tenancy-and-iam.md.
package authz

import (
	"slices"
	"strings"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
)

// Verb is an action on a resource kind.
type Verb string

const (
	// Get reads one object.
	Get Verb = "get"
	// List reads objects in bulk.
	List Verb = "list"
	// Watch follows changes to objects.
	Watch Verb = "watch"
	// Apply creates or replaces an object's spec and metadata.
	Apply Verb = "apply"
	// Delete deletes an object.
	Delete Verb = "delete"
	// UpdateStatus writes an object's status.
	UpdateStatus Verb = "updateStatus"
	// Create opens a new tenant without any grant inside it; only tenants
	// use it.
	Create Verb = "create"
	// Logs reads what a container or a virtual machine wrote; only they use
	// it. It is not Get: output can hold what the spec does not.
	Logs Verb = "logs"
	// Exec runs a process in a container; only containers use it. It is
	// neither Get nor Logs: it reaches everything the container can.
	Exec Verb = "exec"
	// SSH logs in to a virtual machine; only they use it. It reaches
	// everything the VM can, as Exec does a container.
	SSH Verb = "ssh"
)

// verbs lists every Verb, for tests that must see each one.
var verbs = []Verb{Get, List, Watch, Apply, Delete, UpdateStatus, Create, Logs, Exec, SSH}

// CreateTenant is the permission to open a tenant of one's own.
const CreateTenant = "tenancy.tenants.create"

// ContainerLogs is the permission to read a container's output.
var ContainerLogs = Permission(registry.Container, Logs)

// VirtualMachineLogs is the permission to read a virtual machine's serial
// console.
var VirtualMachineLogs = Permission(registry.VirtualMachine, Logs)

// ContainerExec is the permission to run a process in a container.
var ContainerExec = Permission(registry.Container, Exec)

// VirtualMachineSSH is the permission to log in to a virtual machine: to
// have a key signed for it, and to reach its port 22.
var VirtualMachineSSH = Permission(registry.VirtualMachine, SSH)

// userVerbs are what roles may grant. UpdateStatus is reserved for the node
// and the apiserver itself; see docs/decisions/apply-semantics.md.
var userVerbs = []Verb{Get, List, Watch, Apply, Delete}

// Permission names a verb on a kind.
func Permission(r registry.Resource, v Verb) string {
	return r.Group + "." + r.Plural + "." + string(v)
}

func verbOf(permission string) Verb {
	return Verb(permission[strings.LastIndexByte(permission, '.')+1:])
}

// ScopeLevel is the level a role can be bound at.
type ScopeLevel int

const (
	PlatformLevel ScopeLevel = iota + 1
	TenantLevel
	ProjectLevel
)

// Role is a built-in bundle of permissions.
type Role struct {
	// Level is the only scope level the role can be bound at.
	Level ScopeLevel
	// verbs are granted on every kind but those in except.
	verbs []Verb
	// except are kinds verbs are not granted on, as "<group>.<plural>".
	except []string
	// permissions are granted one by one.
	permissions []string
	// Member makes whoever holds the role, at platform level, a platform
	// member: someone allowed to use Hangar at all.
	Member bool
}

// NewRole is a role at level granting permissions one by one, for roles
// beyond the built-in ones, such as a test's.
func NewRole(level ScopeLevel, permissions ...string) Role {
	return Role{Level: level, permissions: slices.Clone(permissions)}
}

// Grants reports whether the role includes the permission.
func (r Role) Grants(permission string) bool {
	if slices.Contains(r.permissions, permission) {
		return true
	}
	return slices.Contains(r.verbs, verbOf(permission)) && !slices.Contains(r.except, permissionKind(permission))
}

// permissionKind is the "<group>.<plural>" a permission is on.
func permissionKind(permission string) string {
	return permission[:strings.LastIndexByte(permission, '.')]
}

func kindOfResource(r registry.Resource) string { return r.Group + "." + r.Plural }

// readVerbs are what viewers hold.
var readVerbs = []Verb{Get, List, Watch}

// reachPermissions reach into what a machine holds or says: its output, a
// process in it, a login to it. Root, owners and editors hold them; viewers
// do not, since output can hold what the spec does not. A new permission of
// that sort, such as reaching a private PublicEndpoint, belongs here.
var reachPermissions = []string{ContainerLogs, ContainerExec, VirtualMachineLogs, VirtualMachineSSH}

// Roles are the built-in roles by name. Platform roles nest: root holds
// everything a sponsor does, and a sponsor everything a member does.
var Roles = map[string]Role{
	"platform.root": {Level: PlatformLevel, verbs: userVerbs, permissions: append([]string{CreateTenant}, reachPermissions...), Member: true},
	// Sponsors will also see aggregated platform data, never other tenants'
	// resources; that data has no API yet.
	"platform.sponsor": {Level: PlatformLevel, permissions: []string{CreateTenant}, Member: true},
	"platform.member":  {Level: PlatformLevel, permissions: []string{CreateTenant}, Member: true},
	"tenant.owner":     {Level: TenantLevel, verbs: userVerbs, permissions: reachPermissions},
	"tenant.viewer":    {Level: TenantLevel, verbs: readVerbs},
	"project.owner":    {Level: ProjectLevel, verbs: userVerbs, permissions: reachPermissions},
	// An editor changes what runs in the project, not who may: it writes no
	// RoleBinding. Nor does it write the Project itself: deleting it takes
	// everything in it and its name for good.
	"project.editor": {Level: ProjectLevel, verbs: userVerbs, except: []string{kindOfResource(registry.RoleBinding), kindOfResource(registry.Project)},
		permissions: append(slices.Clone(reachPermissions), readPermissions(registry.RoleBinding, registry.Project)...)},
	"project.viewer": {Level: ProjectLevel, verbs: readVerbs},
}

// readPermissions are the read verbs on the kinds given.
func readPermissions(rs ...registry.Resource) []string {
	var out []string
	for _, r := range rs {
		for _, v := range readVerbs {
			out = append(out, Permission(r, v))
		}
	}
	return out
}
