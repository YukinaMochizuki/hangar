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
)

// CreateTenant is the permission to open a tenant of one's own.
const CreateTenant = "tenancy.tenants.create"

// ContainerLogs is the permission to read a container's output.
var ContainerLogs = Permission(registry.Container, Logs)

// VirtualMachineLogs is the permission to read a virtual machine's serial
// console.
var VirtualMachineLogs = Permission(registry.VirtualMachine, Logs)

// ContainerExec is the permission to run a process in a container.
var ContainerExec = Permission(registry.Container, Exec)

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
	// verbs are granted on every kind.
	verbs []Verb
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
	return slices.Contains(r.verbs, verbOf(permission)) || slices.Contains(r.permissions, permission)
}

// Roles are the built-in roles by name. Platform roles nest: root holds
// everything a sponsor does, and a sponsor everything a member does.
var Roles = map[string]Role{
	"platform.root": {Level: PlatformLevel, verbs: userVerbs, permissions: []string{CreateTenant, ContainerLogs, ContainerExec, VirtualMachineLogs}, Member: true},
	// Sponsors will also see aggregated platform data, never other tenants'
	// resources; that data has no API yet.
	"platform.sponsor": {Level: PlatformLevel, permissions: []string{CreateTenant}, Member: true},
	"platform.member":  {Level: PlatformLevel, permissions: []string{CreateTenant}, Member: true},
	"tenant.owner":     {Level: TenantLevel, verbs: userVerbs, permissions: []string{ContainerLogs, ContainerExec, VirtualMachineLogs}},
}
