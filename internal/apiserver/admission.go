package apiserver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidName checks that a name is a DNS-1123 label, the rule for every object
// name, project and node name.
func ValidName(name string) error {
	if len(name) == 0 || len(name) > 63 || !dns1123Label.MatchString(name) {
		return fmt.Errorf("%q must be 1-63 lowercase letters, digits or '-', starting and ending with a letter or digit", name)
	}
	return nil
}

// reservedKey reports whether a label or annotation key is reserved for the
// system: its prefix is the API domain or a subdomain of it.
func reservedKey(key string) bool {
	prefix, _, ok := strings.Cut(key, "/")
	return ok && (prefix == registry.Domain || strings.HasSuffix(prefix, "."+registry.Domain))
}

// checkMetaKeys validates labels and annotations. A user may not set a
// reserved key, but may send back one the stored object already has with the
// same value, so reading an object and applying it again works.
func checkMetaKeys(p auth.Principal, meta, stored *metav1.ObjectMeta) error {
	for _, kind := range []struct {
		what         string
		user, stored map[string]string
	}{
		{"label", meta.GetLabels(), stored.GetLabels()},
		{"annotation", meta.GetAnnotations(), stored.GetAnnotations()},
	} {
		for k, v := range kind.user {
			if k == "" || len(k) > 317 {
				return invalid("%s key %q must be 1-317 characters", kind.what, k)
			}
			if kind.what == "label" && len(v) > 63 {
				return invalid("label %q: value longer than 63 characters", k)
			}
			if reservedKey(k) && p.Type != auth.System {
				if sv, ok := kind.stored[k]; !ok || sv != v {
					return invalid("%s %q uses a prefix reserved for the system", kind.what, k)
				}
			}
		}
	}
	return nil
}

// dependentsFinalizer keeps a tenant or project in place, marked as being
// deleted, until everything it owns is gone.
const dependentsFinalizer = "hangar.dev/dependents"

// kindHooks is what differs between kinds in the generic machinery.
type kindHooks struct {
	// target is where an action on obj applies. When creating, obj is the
	// incoming object; otherwise it is the stored one, or nil when the key
	// alone decides.
	target func(s *Server, key resource.Key, obj proto.Message, creating bool) authz.Target
	// targetNeedsObject means target cannot be computed from the key alone.
	targetNeedsObject bool
	// selfServiceCreate makes creating the object need only the platform
	// level Create permission instead of a grant where it lands.
	selfServiceCreate bool
	// neverReuseNames keeps a name claimed after its object is deleted, so a
	// global name can never come to mean something else; see
	// docs/decisions/tenancy-and-iam.md.
	neverReuseNames bool
	// cascades makes deleting the object delete everything it owns first.
	cascades bool
	// owner returns the object a new one belongs to, if any: deleting the
	// owner deletes it.
	owner func(ctx context.Context, s *Server, obj proto.Message) (registry.Resource, resource.Key, bool)
	// validate checks rules specific to the kind; old is nil on create.
	validate func(ctx context.Context, s *Server, p auth.Principal, old, obj proto.Message) error
	// afterCreate runs once a new object is stored.
	afterCreate func(ctx context.Context, s *Server, p auth.Principal, obj proto.Message) error
	// systemManaged keeps every write but status updates to the apiserver
	// itself: no role, however broad, lets anyone else apply or delete such
	// an object.
	systemManaged bool
	// nodeOf names the Node responsible for obj, the only node agent allowed
	// to write its status. Kinds without it take no status from node agents.
	nodeOf func(obj proto.Message) string
}

func projectTarget(_ *Server, key resource.Key, _ proto.Message, _ bool) authz.Target {
	return authz.Target{Project: key.Project}
}

// ownedByProject makes every project-scoped object belong to its project.
func ownedByProject(_ context.Context, _ *Server, obj proto.Message) (registry.Resource, resource.Key, bool) {
	return registry.Project, resource.Key{Name: object.Meta(obj).GetProject()}, true
}

func hooksFor(r registry.Resource) kindHooks {
	switch r.Kind {
	case registry.Tenant.Kind:
		return kindHooks{
			target: func(_ *Server, key resource.Key, _ proto.Message, _ bool) authz.Target {
				return authz.Target{Tenant: key.Name}
			},
			// Platform members may open tenants and become their owners; see
			// docs/decisions/tenancy-and-iam.md.
			selfServiceCreate: true,
			neverReuseNames:   true,
			cascades:          true,
			validate:          validateTenant,
			afterCreate:       grantTenantOwner,
		}
	case registry.Project.Kind:
		return kindHooks{
			target: func(_ *Server, key resource.Key, obj proto.Message, creating bool) authz.Target {
				p, _ := obj.(*tenancyv1alpha1.Project)
				if creating {
					return authz.Target{Tenant: p.GetSpec().GetTenant()}
				}
				// The stored project names its tenant (immutably), which still
				// holds when the project is already gone from the view.
				return authz.Target{Project: key.Name, Tenant: p.GetSpec().GetTenant()}
			},
			neverReuseNames: true,
			cascades:        true,
			owner: func(_ context.Context, _ *Server, obj proto.Message) (registry.Resource, resource.Key, bool) {
				return registry.Tenant, resource.Key{Name: obj.(*tenancyv1alpha1.Project).GetSpec().GetTenant()}, true
			},
			validate: validateProject,
		}
	case registry.RoleBinding.Kind:
		return kindHooks{
			target: func(_ *Server, _ resource.Key, obj proto.Message, _ bool) authz.Target {
				rb, _ := obj.(*iamv1alpha1.RoleBinding)
				switch t := rb.GetSpec().GetScope().GetTarget().(type) {
				case *iamv1alpha1.BindingScope_Tenant:
					return authz.Target{Tenant: t.Tenant}
				case *iamv1alpha1.BindingScope_Project:
					return authz.Target{Project: t.Project}
				}
				return authz.Target{}
			},
			targetNeedsObject: true,
			// A binding scoped to a tenant or project goes with it.
			owner: func(_ context.Context, _ *Server, obj proto.Message) (registry.Resource, resource.Key, bool) {
				switch t := obj.(*iamv1alpha1.RoleBinding).GetSpec().GetScope().GetTarget().(type) {
				case *iamv1alpha1.BindingScope_Tenant:
					return registry.Tenant, resource.Key{Name: t.Tenant}, true
				case *iamv1alpha1.BindingScope_Project:
					return registry.Project, resource.Key{Name: t.Project}, true
				}
				return registry.Resource{}, resource.Key{}, false
			},
			validate: validateRoleBinding,
		}
	case registry.Container.Kind:
		// Deleting a container waits for its machine to be gone from the
		// node, so the deletion a client watches is the real one.
		return kindHooks{target: projectTarget, owner: ownedByProject, validate: validateContainer, cascades: true}
	case registry.Machine.Kind:
		// A machine is owned by the object it was made from, which is
		// already owned by the project.
		return kindHooks{
			target:        projectTarget,
			validate:      validateMachine,
			systemManaged: true,
			nodeOf:        func(obj proto.Message) string { return obj.(*computev1alpha1.Machine).GetSpec().GetNode() },
		}
	case registry.Node.Kind:
		return kindHooks{
			target:        platformTarget,
			systemManaged: true,
			nodeOf:        func(obj proto.Message) string { return object.Meta(obj).GetName() },
		}
	}
	if r.ProjectScoped() {
		return kindHooks{target: projectTarget, owner: ownedByProject}
	}
	return kindHooks{target: platformTarget}
}

func platformTarget(*Server, resource.Key, proto.Message, bool) authz.Target { return authz.Target{} }

// admit runs the checks every write of an object goes through; old is nil on
// create.
func (s *Server) admit(ctx context.Context, b *backend, p auth.Principal, old, obj proto.Message) error {
	if b.r.ProjectScoped() {
		if err := s.requireLive(ctx, registry.Project, "", object.Meta(obj).GetProject()); err != nil {
			return err
		}
	}
	if b.hooks.validate != nil {
		return b.hooks.validate(ctx, s, p, old, obj)
	}
	return nil
}

// ownerRef returns a reference to the object a new one belongs to, which
// must exist and not be on its way out.
func (s *Server) ownerRef(ctx context.Context, b *backend, obj proto.Message) (*metav1.OwnerReference, error) {
	if b.hooks.owner == nil {
		return nil, nil
	}
	r, key, ok := b.hooks.owner(ctx, s, obj)
	if !ok {
		return nil, nil
	}
	o, err := s.store.Get(ctx, s.backend(r).storeKey(key))
	if errors.Is(err, store.ErrNotFound) {
		return nil, failedPrecondition("%s %q does not exist", strings.ToLower(r.Kind), key.Name)
	}
	if err != nil {
		return nil, err
	}
	owner, err := s.backend(r).decode(o)
	if err != nil {
		return nil, err
	}
	meta := object.Meta(owner)
	if meta.GetDeleteTime() != nil {
		return nil, failedPrecondition("%s %q is being deleted", strings.ToLower(r.Kind), key.Name)
	}
	return &metav1.OwnerReference{
		ApiVersion: r.APIVersion(), Kind: r.Kind, Name: key.Name, Uid: meta.GetUid(), Controller: true,
	}, nil
}

// validateTenant keeps quota a platform decision: only someone who may manage
// the whole platform can choose a tenant's plan.
func validateTenant(_ context.Context, s *Server, p auth.Principal, old, obj proto.Message) error {
	plan := obj.(*tenancyv1alpha1.Tenant).GetSpec().GetPlan()
	var oldPlan string
	if old != nil {
		oldPlan = old.(*tenancyv1alpha1.Tenant).GetSpec().GetPlan()
	}
	if plan != oldPlan && !s.authz.Allowed(p, authz.Permission(registry.Tenant, authz.Apply), authz.Target{}) {
		return denied("only platform administrators set spec.plan")
	}
	return nil
}

// requireLive checks that an object an incoming one depends on exists and is
// not being deleted.
func (s *Server) requireLive(ctx context.Context, r registry.Resource, project, name string) error {
	o, err := s.store.Get(ctx, store.Key{Kind: r.Kind, Project: project, Name: name})
	if errors.Is(err, store.ErrNotFound) {
		return failedPrecondition("%s %q does not exist", strings.ToLower(r.Kind), name)
	}
	if err != nil {
		return err
	}
	obj, err := s.backend(r).decode(o)
	if err != nil {
		return err
	}
	if object.Meta(obj).GetDeleteTime() != nil {
		return failedPrecondition("%s %q is being deleted", strings.ToLower(r.Kind), name)
	}
	return nil
}

func validateProject(ctx context.Context, s *Server, _ auth.Principal, _, obj proto.Message) error {
	tenant := obj.(*tenancyv1alpha1.Project).GetSpec().GetTenant()
	if tenant == "" {
		return invalid("spec.tenant is required")
	}
	return s.requireLive(ctx, registry.Tenant, "", tenant)
}

func validateRoleBinding(ctx context.Context, s *Server, _ auth.Principal, _, obj proto.Message) error {
	spec := obj.(*iamv1alpha1.RoleBinding).GetSpec()
	role, ok := authz.Roles[spec.GetRole()]
	if !ok {
		return invalid("spec.role %q is not a known role", spec.GetRole())
	}
	switch sub := spec.GetSubject().GetSubject().(type) {
	case *iamv1alpha1.Subject_User:
		if sub.User == "" || sub.User != strings.ToLower(sub.User) || !strings.Contains(sub.User, "@") {
			return invalid("spec.subject.user must be a lowercase email address")
		}
	case *iamv1alpha1.Subject_ServiceToken:
		if sub.ServiceToken == "" {
			return invalid("spec.subject.service_token must not be empty")
		}
	default:
		return invalid("spec.subject is required")
	}
	var level authz.ScopeLevel
	switch t := spec.GetScope().GetTarget().(type) {
	case *iamv1alpha1.BindingScope_Platform:
		level = authz.PlatformLevel
	case *iamv1alpha1.BindingScope_Tenant:
		level = authz.TenantLevel
		if err := s.requireLive(ctx, registry.Tenant, "", t.Tenant); err != nil {
			return err
		}
	case *iamv1alpha1.BindingScope_Project:
		level = authz.ProjectLevel
		if err := s.requireLive(ctx, registry.Project, "", t.Project); err != nil {
			return err
		}
	default:
		return invalid("spec.scope is required")
	}
	if role.Level != level {
		return invalid("role %q cannot be bound at this scope", spec.GetRole())
	}
	return nil
}

func validateContainer(_ context.Context, _ *Server, _ auth.Principal, _, obj proto.Message) error {
	spec := obj.(*computev1alpha1.Container).GetSpec()
	if spec.GetImage() == "" {
		return invalid("spec.image is required")
	}
	seen := map[string]bool{}
	for _, port := range spec.GetPorts() {
		if err := ValidName(port.GetName()); err != nil {
			return invalid("spec.ports: %v", err)
		}
		if seen[port.GetName()] {
			return invalid("spec.ports: duplicate name %q", port.GetName())
		}
		seen[port.GetName()] = true
		if port.GetPort() < 1 || port.GetPort() > 65535 {
			return invalid("spec.ports[%s]: port %d out of range", port.GetName(), port.GetPort())
		}
	}
	if spec.GetResources().GetVcpus() < 0 || spec.GetResources().GetMemoryMib() < 0 {
		return invalid("spec.resources must not be negative")
	}
	return nil
}

// validateMachine checks a machine the controllers wrote. The node, once
// chosen, is permanent: moving a machine means a new one, never a rewrite.
func validateMachine(_ context.Context, _ *Server, _ auth.Principal, old, obj proto.Message) error {
	spec := obj.(*computev1alpha1.Machine).GetSpec()
	if old != nil {
		if was := old.(*computev1alpha1.Machine).GetSpec().GetNode(); was != "" && was != spec.GetNode() {
			return invalid("spec.node is %q and cannot change", was)
		}
	}
	if spec.GetNode() != "" {
		if err := ValidName(spec.GetNode()); err != nil {
			return invalid("spec.node: %v", err)
		}
	}
	// Only controllers write machines, and they always say what they mean:
	// no enum is left for the node to default.
	switch spec.GetRunState() {
	case computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING, computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED:
	default:
		return invalid("spec.run_state %v is not RUNNING or STOPPED", spec.GetRunState())
	}
	switch spec.GetRestartPolicy() {
	case computev1alpha1.RestartPolicy_RESTART_POLICY_ALWAYS, computev1alpha1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
		computev1alpha1.RestartPolicy_RESTART_POLICY_NEVER:
	default:
		return invalid("spec.restart_policy %v is not ALWAYS, ON_FAILURE or NEVER", spec.GetRestartPolicy())
	}
	if spec.GetBoot() == nil {
		return invalid("spec.boot is required")
	}
	if c := spec.GetContainer(); c != nil && c.GetImage() == "" {
		return invalid("spec.container.image is required")
	}
	if spec.GetResources().GetVcpus() < 0 || spec.GetResources().GetMemoryMib() < 0 {
		return invalid("spec.resources must not be negative")
	}
	seen := map[string]bool{}
	for _, nic := range spec.GetInterfaces() {
		if err := ValidName(nic.GetName()); err != nil {
			return invalid("spec.interfaces: %v", err)
		}
		if seen[nic.GetName()] {
			return invalid("spec.interfaces: duplicate name %q", nic.GetName())
		}
		seen[nic.GetName()] = true
		if err := ValidName(nic.GetNetwork()); err != nil {
			return invalid("spec.interfaces[%s].network: %v", nic.GetName(), err)
		}
	}
	return nil
}

// grantTenantOwner binds the tenant's creator as its owner. The binding is
// owned by the tenant, so it goes away with it.
func grantTenantOwner(ctx context.Context, s *Server, p auth.Principal, obj proto.Message) error {
	t := obj.(*tenancyv1alpha1.Tenant)
	var sub *iamv1alpha1.Subject
	switch p.Type {
	case auth.User:
		sub = &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: p.ID}}
	case auth.ServiceToken:
		sub = &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_ServiceToken{ServiceToken: p.ID}}
	default:
		return nil
	}
	meta := t.GetMetadata()
	uid := meta.GetUid()
	rb := &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{
			Name: ownerBindingName(meta.GetName(), uid),
			OwnerReferences: []*metav1.OwnerReference{{
				ApiVersion: registry.Tenant.APIVersion(), Kind: registry.Tenant.Kind,
				Name: meta.GetName(), Uid: uid, Controller: true,
			}},
		},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: sub,
			Role:    "tenant.owner",
			Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: meta.GetName()}},
		},
	}
	_, err := s.backend(registry.RoleBinding).Apply(auth.WithPrincipal(ctx, auth.SystemPrincipal), rb)
	if err != nil {
		return fmt.Errorf("granting its owner: %w", err)
	}
	return nil
}

func ownerBindingName(tenant, uid string) string {
	const suffix = "-owner-"
	tail := strings.ReplaceAll(uid, "-", "")
	tail = tail[max(0, len(tail)-8):]
	return tenant[:min(len(tenant), 63-len(suffix)-len(tail))] + suffix + tail
}
