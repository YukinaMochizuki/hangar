package apiserver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
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
const dependentsFinalizer = registry.Domain + "/dependents"

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
	// beforeDelete may refuse a delete; it runs when the delete is asked
	// for, not again while finalizers hold the object.
	beforeDelete func(ctx context.Context, s *Server, obj proto.Message) error
	// onCreate, if set, records on a new object what the apiserver keeps
	// about who creates it; it runs once the caller's metadata is checked,
	// before admission.
	onCreate func(p auth.Principal, obj proto.Message)
	// afterCreate runs once a new object is stored.
	afterCreate func(ctx context.Context, s *Server, p auth.Principal, obj proto.Message) error
	// systemManaged keeps every write but status updates to the apiserver
	// itself: no role, however broad, lets anyone else apply or delete such
	// an object.
	systemManaged bool
	// neverDeleted, if set, refuses every delete of the kind but the
	// apiserver's own, whatever the caller's roles, before anything else is
	// done; it is what the refusal tells the caller.
	neverDeleted string
	// finalizers are added to every new object of the kind; the controller
	// that owns each removes it.
	finalizers []string
	// nodeOf names the Node responsible for obj, the only node agent allowed
	// to write its status. Kinds without it take no status from node agents.
	nodeOf func(obj proto.Message) string
	// writeLock, if set, is held by every apply and delete of the kind from
	// its admission checks to its write, for rules that span objects.
	writeLock func(s *Server) *sync.Mutex
	// readableByAll lets everyone allowed to use Hangar at all get, list and
	// watch every object of the kind, whatever their grants.
	readableByAll bool
	// anyNodeReports lets every node agent write the status of any object
	// of the kind, for what is the same on every node.
	anyNodeReports bool
	// claim, if set, is a claim every new object takes, besides its name,
	// and never gives back once the object is stored, such as an endpoint's
	// hostname. It is made from the key alone: a create that finds an object
	// of its key made meanwhile keeps the claim, which only holds if that
	// object would have taken the same one.
	claim func(key resource.Key) globalClaim
	// forNode, if set, clears in place what of obj the node agent of node
	// may not read, though it reads the rest: what only the node an object
	// runs on needs, such as a VM's user-data. It is applied to every
	// object answered to a node: by get, list, watch and status update.
	// Nodes may not create, apply or delete.
	forNode func(node string, obj proto.Message)
	// hiddenFromNodes refuses node agents every read of the kind, which
	// they otherwise have on every kind.
	hiddenFromNodes bool
	// seal, if set, turns what a caller wrote into what is stored, once
	// admission has passed it and the object has its uid; old is nil on
	// create. A stored object equal to old is not written.
	seal func(s *Server, old, obj proto.Message) error
	// redact, if set, clears in place what no answer may hold, whoever
	// asks: a Secret's values. It is applied to every object answered, by
	// every call and event.
	redact func(obj proto.Message)
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
			onCreate:          stampOpener,
			afterCreate:       grantTenantOwner,
			// Shared with Plan: that a tenant's plan exists is checked against
			// the plans, and deleting a plan against the tenants.
			writeLock: planLock,
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
	case registry.User.Kind:
		// A person signs up through AccountService, which creates their User
		// as the system; otherwise only platform.root reads and changes Users,
		// as no tenant grant covers the platform. No one deletes one: one
		// email has one User for good. Its applies hold its lock: that no two
		// Users share an email is checked against the others (validateUser).
		return kindHooks{
			target:          platformTarget,
			neverReuseNames: true,
			validate:        validateUser,
			writeLock:       userLock,
			neverDeleted:    "users are never deleted; to remove a member, delete their platform role binding",
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
			writeLock:     attachLock,
			forNode:       machineForNode,
		}
	case registry.Disk.Kind:
		// A disk's node reports on it once the scheduler has bound it there.
		return kindHooks{
			target:       projectTarget,
			owner:        ownedByProject,
			validate:     validateDisk,
			beforeDelete: refuseDeletingAttachedDisk,
			finalizers:   []string{DiskInUseFinalizer, NodeCleanupFinalizer},
			nodeOf:       func(obj proto.Message) string { return obj.(*storagev1alpha1.Disk).GetStatus().GetNode() },
			writeLock:    attachLock,
		}
	case registry.Image.Kind:
		// Platform administrators keep the catalogue that every tenant
		// makes disks from.
		return kindHooks{
			target:         platformTarget,
			validate:       validateImage,
			readableByAll:  true,
			anyNodeReports: true,
		}
	case registry.VirtualMachine.Kind:
		// Deleting a VM waits for its machine to be gone from the node, and
		// for a root disk it owns. Its applies and deletes hold Disk's lock:
		// whether its root disk is owned by it is checked against it, both
		// ways (checkRootDiskOwner, refuseDeletingUnsettledVM).
		return kindHooks{
			target:       projectTarget,
			owner:        ownedByProject,
			validate:     validateVirtualMachine,
			beforeDelete: refuseDeletingUnsettledVM,
			cascades:     true,
			writeLock:    attachLock,
			// Its machine carries what its guest is given, to its node.
			forNode: func(_ string, obj proto.Message) {
				spec := obj.(*computev1alpha1.VirtualMachine).GetSpec()
				spec.UserData, spec.SshAuthorizedKeys = "", nil
			},
		}
	case registry.Address.Kind:
		// Its applies hold its lock: that no two Addresses name one
		// interface is checked against the others (validateAddress).
		return kindHooks{
			target:     projectTarget,
			owner:      ownedByProject,
			validate:   validateAddress,
			writeLock:  addressLock,
			finalizers: []string{AddressUnbindFinalizer},
		}
	case registry.PublicEndpoint.Kind:
		// Its hostname is claimed for good when it is made, and it is kept,
		// deleted, until no Access application of it can be left.
		return kindHooks{
			target:     projectTarget,
			owner:      endpointOwner,
			validate:   validatePublicEndpoint,
			claim:      endpointClaim,
			finalizers: []string{AccessApplicationFinalizer},
		}
	case registry.Network.Kind:
		return kindHooks{
			target:       projectTarget,
			owner:        ownedByProject,
			validate:     validateNetwork,
			beforeDelete: refuseDeletingInUse,
			finalizers:   []string{NetworkInUseFinalizer},
		}
	case registry.SecurityGroup.Kind:
		return kindHooks{
			target:       projectTarget,
			owner:        ownedByProject,
			validate:     validateSecurityGroup,
			beforeDelete: refuseDeletingGroupInUse,
		}
	case registry.Secret.Kind:
		// Values are write-only and stored sealed; nodes get a login only
		// through ImagePullCredentialsService. See docs/decisions/secrets.md.
		return kindHooks{
			target:          projectTarget,
			owner:           ownedByProject,
			validate:        validateSecret,
			hiddenFromNodes: true,
			seal:            sealSecret,
			redact:          redactSecret,
		}
	case registry.Plan.Kind:
		// Platform administrators keep the plans; a tenant reads its own
		// plan's limits in its status.
		return kindHooks{
			target:       platformTarget,
			validate:     validatePlan,
			beforeDelete: refuseDeletingPlanInUse,
			writeLock:    planLock,
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

func attachLock(s *Server) *sync.Mutex { return &s.attach }

func planLock(s *Server) *sync.Mutex { return &s.plans }

// machineForNode keeps a machine's cloud-init data, its guest's user-data
// and keys, from every node but the one it is on, which serves it.
func machineForNode(node string, obj proto.Message) {
	spec := obj.(*computev1alpha1.Machine).GetSpec()
	if f := spec.GetFirmware(); f != nil && spec.GetNode() != node {
		f.CloudInit = nil
	}
}

// admit runs the checks every write of an object goes through; old is nil on
// create.
func (s *Server) admit(ctx context.Context, b *backend, p auth.Principal, old, obj proto.Message) error {
	if b.r.ProjectScoped() {
		if err := s.requireLive(ctx, registry.Project, "", object.Meta(obj).GetProject()); err != nil {
			return err
		}
	}
	if b.hooks.validate != nil {
		if err := b.hooks.validate(ctx, s, p, old, obj); err != nil {
			return err
		}
	}
	return s.checkQuota(ctx, b.r, p, old, obj)
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
// the whole platform can choose a tenant's plan, which must exist.
func validateTenant(ctx context.Context, s *Server, p auth.Principal, old, obj proto.Message) error {
	plan := obj.(*tenancyv1alpha1.Tenant).GetSpec().GetPlan()
	var oldPlan string
	if old != nil {
		oldPlan = old.(*tenancyv1alpha1.Tenant).GetSpec().GetPlan()
	}
	if plan != oldPlan && !s.authz.Allowed(p, authz.Permission(registry.Tenant, authz.Apply), authz.Target{}) {
		return denied("only platform administrators set spec.plan")
	}
	if plan != oldPlan && plan != "" {
		if o, err := s.get(ctx, registry.Plan, resource.Key{Name: plan}); err != nil {
			return err
		} else if o == nil {
			return failedPrecondition("spec.plan: plan %q does not exist", plan)
		}
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

func validateProject(ctx context.Context, s *Server, p auth.Principal, old, obj proto.Message) error {
	tenant := obj.(*tenancyv1alpha1.Project).GetSpec().GetTenant()
	if tenant == "" {
		return invalid("spec.tenant is required")
	}
	if err := s.requireLive(ctx, registry.Tenant, "", tenant); err != nil {
		return err
	}
	return s.validateProjectLimits(ctx, p, old, obj)
}

func validateRoleBinding(ctx context.Context, s *Server, _ auth.Principal, _, obj proto.Message) error {
	spec := obj.(*iamv1alpha1.RoleBinding).GetSpec()
	role, ok := s.authz.Role(spec.GetRole())
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

func validateContainer(ctx context.Context, s *Server, _ auth.Principal, old, obj proto.Message) error {
	spec := obj.(*computev1alpha1.Container).GetSpec()
	if spec.GetImage() == "" {
		return invalid("spec.image is required")
	}
	switch spec.GetRunState() {
	case computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_UNSPECIFIED, computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_RUNNING,
		computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED:
	default:
		return invalid("spec.run_state %v is not RUNNING or STOPPED", spec.GetRunState())
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
	meta := obj.(*computev1alpha1.Container).GetMetadata()
	if err := validateContainerPorts(meta.GetProject(), meta.GetName(), spec.GetPorts()); err != nil {
		return err
	}
	if err := s.validateInterfaces(ctx, meta.GetProject(), spec.GetInterfaces()); err != nil {
		return err
	}
	if err := validateImagePullSecrets(spec.GetImagePullSecrets()); err != nil {
		return err
	}
	return s.validateContainerResources(ctx, old, spec.GetResources())
}

// MaxInterfaces bounds the interfaces of a machine.
const MaxInterfaces = 8

// validateInterfaces checks the interfaces a Container or VirtualMachine
// asks for in project. A network or group that does not exist yet is the
// controller's or the node's to wait for, not admission's to refuse.
func (s *Server) validateInterfaces(ctx context.Context, project string, nics []*computev1alpha1.NetworkInterface) error {
	if len(nics) > MaxInterfaces {
		return invalid("spec.interfaces: %d interfaces, at most %d", len(nics), MaxInterfaces)
	}
	seen := map[string]bool{}
	for i, nic := range nics {
		if err := ValidName(nic.GetNetwork()); err != nil {
			return invalid("spec.interfaces[%d].network: %v", i, err)
		}
		if seen[nic.GetNetwork()] {
			return invalid("spec.interfaces: network %q twice; a machine has one interface on a network", nic.GetNetwork())
		}
		seen[nic.GetNetwork()] = true
		if err := validateSecurityGroupNames(i, nic.GetSecurityGroups()); err != nil {
			return err
		}
		if err := s.refuseGroupsOfOtherNetworks(ctx, project, nic.GetNetwork(), nic.GetSecurityGroups(),
			fmt.Sprintf("spec.interfaces[%d].security_groups", i)); err != nil {
			return err
		}
	}
	return nil
}

// validateContainerResources checks the quantities a container asks for,
// as written; old is the stored container, nil on create. The defaults
// count as written, so a container that never named a disk has the default.
func (s *Server) validateContainerResources(ctx context.Context, old proto.Message, r *computev1alpha1.Resources) error {
	sz, err := containerSizes(r)
	if err != nil {
		return invalid("%v", err)
	}
	if sz.memory < minMemory {
		return invalid("spec.resources.memory %s is below the minimum of 128Mi", cmp.Or(r.GetMemory(), defaultMemory))
	}
	if sz.disk < minDisk {
		return invalid("spec.resources.disk %s is below the minimum of 1Gi", cmp.Or(r.GetDisk(), defaultDisk))
	}
	if err := checkMaximums(sz, cmp.Or(r.GetCpu(), defaultCPU), cmp.Or(r.GetMemory(), defaultMemory), cmp.Or(r.GetDisk(), defaultDisk)); err != nil {
		return err
	}
	if old == nil {
		return nil
	}
	// Only a change of disk is checked, so an apply that leaves the disk as
	// it was always passes, even when the machine's disk is larger.
	was := old.(*computev1alpha1.Container)
	if stored, _ := containerSizes(was.GetSpec().GetResources()); wholeGi(sz.disk) == wholeGi(stored.disk) {
		return nil
	}
	floor, err := s.diskFloor(ctx, was)
	if err != nil {
		return err
	}
	if wholeGi(sz.disk) < floor {
		return invalid("spec.resources.disk %s is smaller than the current %dGi; a root disk can grow but never shrink",
			cmp.Or(r.GetDisk(), defaultDisk), floor)
	}
	return nil
}

// diskFloor is the smallest disk, in whole Gi, an update of c may ask for:
// the disk its placed machine has, so a growth that never took effect can be
// withdrawn; nothing while the machine is unplaced, as no disk exists yet;
// and before there is a machine, the disk c asks for.
func (s *Server) diskFloor(ctx context.Context, c *computev1alpha1.Container) (uint64, error) {
	// One Get of the machine by name, not a list or a watch: the machine may
	// still change after this read, and the container controller keeps a
	// machine's disk from shrinking if it does.
	obj, err := s.get(ctx, registry.Machine, resource.Key{Project: c.GetMetadata().GetProject(), Name: machineName(c.GetMetadata().GetName())})
	if err != nil {
		return 0, err
	}
	if m, ok := obj.(*computev1alpha1.Machine); ok && ownedBy(m.GetMetadata(), c.GetMetadata().GetUid()) {
		if m.GetSpec().GetNode() == "" {
			return 0, nil
		}
		if h, err := heldBy(m.GetSpec().GetResources()); err == nil {
			return wholeGi(h.disk), nil
		}
	}
	// A stored disk that does not parse reads as zero and holds nothing back.
	stored, _ := containerSizes(c.GetSpec().GetResources())
	return wholeGi(stored.disk), nil
}

// checkMaximums refuses sizes past what a machine can have; cpu, memory and
// disk are the quantities as written, for the message.
func checkMaximums(sz sizes, cpu, memory, disk string) error {
	if sz.cpu > maxCPU {
		return invalid("spec.resources.cpu %s is above the maximum of %d vCPUs", cpu, maxCPU)
	}
	if sz.memory > maxMemory {
		return invalid("spec.resources.memory %s is above the maximum of 4Ti", memory)
	}
	if sz.disk > maxDisk {
		return invalid("spec.resources.disk %s is above the maximum of 64Ti", disk)
	}
	return nil
}

// validateMachine checks a machine the controllers wrote. The node, once
// chosen, is permanent: moving a machine means a new one, never a rewrite.
func validateMachine(ctx context.Context, s *Server, _ auth.Principal, old, obj proto.Message) error {
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
	if f := spec.GetFirmware(); f != nil && f.GetRootDisk() == "" {
		return invalid("spec.firmware.root_disk is required")
	}
	if ci := spec.GetFirmware().GetCloudInit(); len(ci.GetUserData()) > maxUserData || len(ci.GetVendorData()) > maxVendorData {
		return invalid("spec.firmware.cloud_init: user_data of %d bytes, at most %d; vendor_data of %d, at most %d",
			len(ci.GetUserData()), maxUserData, len(ci.GetVendorData()), maxVendorData)
	}
	res := spec.GetResources()
	var sz sizes
	if cpu := res.GetCpu(); cpu != "" {
		var err error
		if sz.cpu, err = quantity.CPU(cpu); err != nil {
			return invalid("spec.resources.cpu: %v", err)
		}
	}
	h, err := heldBy(res)
	if err != nil {
		return invalid("spec.resources.%v", err)
	}
	sz.memory, sz.disk = h.memory, h.disk
	if err := checkMaximums(sz, res.GetCpu(), res.GetMemory(), res.GetDisk()); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, nic := range spec.GetInterfaces() {
		if err := ValidName(nic.GetName()); err != nil {
			return invalid("spec.interfaces: %v", err)
		}
		if a := nic.GetIpv4(); a != "" {
			if p, err := netip.ParsePrefix(a); err != nil || !p.Addr().Is4() {
				return invalid("spec.interfaces[%s].ipv4 %q is not an IPv4 address with a prefix length", nic.GetName(), a)
			}
		}
		if a := nic.GetIpv6(); a != "" {
			if p, err := netip.ParsePrefix(a); err != nil || !p.Addr().Is6() || p.Addr().Is4In6() || p.Addr().Zone() != "" ||
				p.Bits() != 128 || !p.Addr().IsGlobalUnicast() {
				return invalid("spec.interfaces[%s].ipv6 %q is not a global IPv6 address with /128", nic.GetName(), a)
			}
			if spec.GetNode() == "" {
				return invalid("spec.interfaces[%s].ipv6 is set on a machine on no node", nic.GetName())
			}
		}
		if seen[nic.GetName()] {
			return invalid("spec.interfaces: duplicate name %q", nic.GetName())
		}
		seen[nic.GetName()] = true
		if err := ValidName(nic.GetNetwork()); err != nil {
			return invalid("spec.interfaces[%s].network: %v", nic.GetName(), err)
		}
	}
	var oldSpec *computev1alpha1.MachineSpec
	if old != nil {
		oldSpec = old.(*computev1alpha1.Machine).GetSpec()
	}
	return s.checkAttaching(ctx, obj.(*computev1alpha1.Machine).GetMetadata(), oldSpec, spec)
}

// privateIPv4 are the RFC 1918 ranges networks may use.
var privateIPv4 = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

func validateNetwork(_ context.Context, _ *Server, _ auth.Principal, _, obj proto.Message) error {
	n := obj.(*networkv1alpha1.Network)
	cidr := n.GetSpec().GetIpv4Cidr()
	if n.GetMetadata().GetName() == DefaultNetwork && cidr != DefaultNetworkCIDR {
		return invalid("network %q always has spec.ipv4_cidr %s; give another range another name", DefaultNetwork, DefaultNetworkCIDR)
	}
	p, err := netip.ParsePrefix(cidr)
	if err != nil || !p.Addr().Is4() {
		return invalid("spec.ipv4_cidr %q is not an IPv4 range such as 10.0.0.0/24", cidr)
	}
	if p != p.Masked() {
		return invalid("spec.ipv4_cidr %q has host bits set; did you mean %s?", cidr, p.Masked())
	}
	if p.Bits() < 16 || p.Bits() > 28 {
		return invalid("spec.ipv4_cidr %q must be between /16 and /28", cidr)
	}
	for _, r := range privateIPv4 {
		if r.Bits() <= p.Bits() && r.Contains(p.Addr()) {
			return nil
		}
	}
	return invalid("spec.ipv4_cidr %q is not inside a private range (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16)", cidr)
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
