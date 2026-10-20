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
	// Access uses a private public endpoint: reaches what it serves through
	// the edge. It is not Get, which reads the endpoint object, nor Apply,
	// which edits it.
	Access Verb = "access"
)

// verbs lists every Verb, for tests that must see each one.
var verbs = []Verb{Get, List, Watch, Apply, Delete, UpdateStatus, Create, Logs, Exec, SSH, Access}

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

// PublicEndpointAccess is the permission to use a private PublicEndpoint
// of a project through the edge.
var PublicEndpointAccess = Permission(registry.PublicEndpoint, Access)

// MonitoringRead is the permission to read the platform's monitoring
// (MonitoringService). It is on no kind, and held at the platform alone. See
// docs/decisions/monitoring.md.
const MonitoringRead = "platform.monitoring.read"

// platformPermissions are on no kind: they are held at the platform, and
// checked there whatever the scope.
var platformPermissions = []string{MonitoringRead}

// IsPlatform reports whether a permission is on no kind, and so is held and
// checked at the platform alone.
func IsPlatform(permission string) bool { return slices.Contains(platformPermissions, permission) }

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
// do not, since output can hold what the spec does not. Reaching what a
// private PublicEndpoint serves is of that sort too.
var reachPermissions = []string{MachineLogs, MachineExec, MachineSSH, PublicEndpointAccess}

// Roles are the built-in roles by name. Platform roles nest: root holds
// everything a sponsor does, and a sponsor everything a member does.
var Roles = map[string]Role{
	"platform.root": {Level: PlatformLevel, verbs: userVerbs, permissions: append(append([]string{CreateTenant}, reachPermissions...), platformPermissions...), Member: true},
	// Sponsors will also see aggregated platform data, never other tenants'
	// resources; that data has no API yet.
	"platform.sponsor": {Level: PlatformLevel, permissions: []string{CreateTenant}, Member: true},
	"platform.member":  {Level: PlatformLevel, permissions: []string{CreateTenant}, Member: true},
	"tenant.owner":     {Level: TenantLevel, verbs: userVerbs, permissions: reachPermissions},
	"tenant.viewer":    {Level: TenantLevel, verbs: readVerbs},
	"project.owner":    {Level: ProjectLevel, verbs: userVerbs, permissions: reachPermissions},
	// An editor changes what runs in the project, not who may: it writes no
	// RoleBinding, and no ServiceToken, since creating or rotating one hands
	// out its secret, and with it whatever the token's bindings grant, an
	// owner's included. Nor does it write the Project itself: deleting it
	// takes everything in it and its name for good.
	"project.editor": {Level: ProjectLevel, verbs: userVerbs,
		except:      []string{kindOfResource(registry.RoleBinding), kindOfResource(registry.ServiceToken), kindOfResource(registry.Project)},
		permissions: append(slices.Clone(reachPermissions), readPermissions(registry.RoleBinding, registry.ServiceToken, registry.Project)...)},
	"project.viewer": {Level: ProjectLevel, verbs: readVerbs},
}

// kindVerbs are the verbs every kind has a permission for: those roles may
// grant, and UpdateStatus, which none does.
var kindVerbs = append(slices.Clone(userVerbs), UpdateStatus)

// knownPermissions maps every permission there is to its kind and verb: each
// kind's kindVerbs, the special actions roles name one by one, and the
// platform permissions, whose kind is the zero Resource.
var knownPermissions = func() map[string]knownPermission {
	out := map[string]knownPermission{}
	for _, r := range registry.All {
		for _, v := range kindVerbs {
			out[Permission(r, v)] = knownPermission{r, v}
		}
	}
	for _, perm := range append([]string{CreateTenant}, reachPermissions...) {
		for _, r := range registry.All {
			if kindOfResource(r) == permissionKind(perm) {
				out[perm] = knownPermission{r, verbOf(perm)}
			}
		}
	}
	for _, perm := range platformPermissions {
		out[perm] = knownPermission{verb: verbOf(perm)}
	}
	return out
}()

type knownPermission struct {
	resource registry.Resource
	verb     Verb
}

// LookupPermission returns the kind and verb of a permission, and false for
// a name that is no permission.
func LookupPermission(permission string) (registry.Resource, Verb, bool) {
	k, ok := knownPermissions[permission]
	return k.resource, k.verb, ok
}

// KnownPermission reports whether a name is a permission.
func KnownPermission(permission string) bool {
	_, ok := knownPermissions[permission]
	return ok
}

// IsReach reports whether a permission reaches into what a machine or an
// endpoint holds (logs, exec, ssh, endpoint access), rather than acting on
// an object of its kind.
func IsReach(permission string) bool { return slices.Contains(reachPermissions, permission) }

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
