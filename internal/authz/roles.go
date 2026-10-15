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
	// Logs reads what a machine wrote; only machines use it, whatever kind
	// runs as one. It is not Get: output can hold what the spec does not.
	Logs Verb = "logs"
	// Exec runs a process in a machine; only machines use it. It is neither
	// Get nor Logs: it reaches everything the machine can.
	Exec Verb = "exec"
	// SSH logs in to a machine; only machines use it. It reaches everything
	// the machine can, as Exec does.
	SSH Verb = "ssh"
)

// verbs lists every Verb, for tests that must see each one.
var verbs = []Verb{Get, List, Watch, Apply, Delete, UpdateStatus, Create, Logs, Exec, SSH}

// CreateTenant is the permission to open a tenant of one's own.
const CreateTenant = "tenancy.tenants.create"

// A machine's capabilities are permissions on the Machine kind, whatever
// kind the caller names: a container's logs and a virtual machine's are one
// permission. See docs/decisions/machine-model.md.
var (
	// MachineLogs is the permission to read what a machine wrote: a
	// container's output, a virtual machine's serial console.
	MachineLogs = Permission(registry.Machine, Logs)
	// MachineExec is the permission to run a process in a machine.
	MachineExec = Permission(registry.Machine, Exec)
	// MachineSSH is the permission to log in to a machine: to have a key
	// signed for it, and to reach its port 22.
	MachineSSH = Permission(registry.Machine, SSH)
)

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
var reachPermissions = []string{MachineLogs, MachineExec, MachineSSH}

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
