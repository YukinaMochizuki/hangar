package apiserver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
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
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
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

// dependentsFinalizer is described in computeapi.
const dependentsFinalizer = computeapi.DependentsFinalizer

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
	// alsoOwnedBy returns a second owner a new object belongs to, if any,
	// which must exist, not be on its way out, and be one p may read: as a
	// RoleBinding naming a ServiceToken is the token's.
	alsoOwnedBy func(ctx context.Context, s *Server, p auth.Principal, obj proto.Message) (*metav1.OwnerReference, error)
	// createLock, if set, is taken from the admission of a new object to
	// its write, and returns what releases it.
	createLock func(s *Server, obj proto.Message) (unlock func())
	// cascadeLock, if set, is taken by the garbage collector for the whole
	// of a cascade of the object, from looking for what it owns to letting
	// it go, and returns what releases it.
	cascadeLock func(s *Server, key resource.Key) (unlock func())
	// validate checks rules specific to the kind; old is nil on create.
	validate func(ctx context.Context, s *Server, p auth.Principal, old, obj proto.Message) error
	// beforeDelete may refuse a delete; it runs when the delete is asked
	// for, not again while finalizers hold the object.
	beforeDelete func(ctx context.Context, s *Server, obj proto.Message) error
	// afterDelete runs once a delete is stored, whether it marked the
	// object or removed it, and again on every later delete of an object
	// being deleted, so it must be idempotent: it ends what the object
	// stands for outside the store, as a ServiceToken's secret.
	afterDelete func(ctx context.Context, s *Server, key resource.Key) error
	// beforeGone runs when a cascading object has nothing left and is about
	// to go; its failure keeps the object, being deleted, and the garbage
	// collector tries again. It ends for good what afterDelete may have
	// failed to.
	beforeGone func(ctx context.Context, s *Server, key resource.Key) error
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
	// initialStatus, if set, returns the status every new object of the
	// kind is stored with, in place of none.
	initialStatus func() proto.Message
	// finalizers are added to every new object of the kind; the controller
	// that owns each removes it.
	finalizers []string
	// nodeOf names the Node responsible for obj, the only node agent allowed
	// to write its status. Kinds without it take no status from node agents.
	nodeOf func(obj proto.Message) string
	// writeLocks, if set, are held by every apply and delete of the kind
	// from its admission checks to its write, for rules that span objects.
	// They are taken in order; whoever takes users and plans takes users
	// first.
	writeLocks func(s *Server) []*sync.Mutex
	// applyLocks, if set, are held by an apply of obj as writeLocks are,
	// after them: for rules that only some objects of a kind take part in,
	// so the others are not serialized for nothing.
	applyLocks func(s *Server, obj proto.Message) []*sync.Mutex
	// othersWriteLock, if set, is held as writeLocks are, after them, by
	// every apply and delete of the kind but the apiserver's own.
	othersWriteLock func(s *Server) *sync.Mutex
	// readableByAll lets everyone allowed to use Hangar at all get, list and
	// watch every object of the kind, whatever their grants.
	readableByAll bool
	// readableBy, if set, reports whether p holds permission, the kind's
	// get, list or watch, somewhere that lets it read obj besides obj's own
	// target. It widens those reads alone (backend.mayRead); every other
	// verb is decided at the target.
	readableBy func(s *Server, p auth.Principal, permission string, obj proto.Message) bool
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
	// nameIsEmail makes an object's name an email in lower case rather than
	// a DNS label, as an Invitation's.
	nameIsEmail bool
	// seal, if set, turns what a caller wrote into what is stored, once
	// admission has passed it and the object has its uid; old is nil on
	// create. A stored object equal to old is not written.
	seal func(s *Server, old, obj proto.Message) error
	// validateStatus, if set, checks every status written, whoever writes
	// it.
	validateStatus func(status proto.Message) error
	// fromNode, if set, is the status stored when a node agent writes
	// reported over stored: what of it the node owns, and stored's rest.
	fromNode func(stored, reported proto.Message) proto.Message
	// redact, if set, clears in place what no answer may hold, whoever
	// asks: a Secret's values. It is applied to every object answered, by
	// every call and event.
	redact func(obj proto.Message)
	// warn, if set, returns what the caller of a successful apply should
	// know about the stored object, which the answer carries as warnings.
	warn func(s *Server, obj proto.Message) []string
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
			writeLocks: only(planLock),
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
			// And one naming a service token goes with the token.
			alsoOwnedBy: bindingTokenOwner,
			// Shared with the token's cascade: see Server.tokenBindings.
			createLock: lockBindingToken,
			validate:   validateRoleBinding,
			warn:       warnRoleBinding,
			// A tenant keeps one tenant.owner binding (checkOwnerLeft).
			beforeDelete:    refuseRemovingLastOwner,
			othersWriteLock: ownersLock,
		}
	case registry.ServiceToken.Kind:
		// Deleting a token revokes its secret at once and waits for the
		// RoleBindings naming it, so no later token of its name finds them.
		// See servicetokens.go.
		return kindHooks{
			target:          projectTarget,
			owner:           ownedByProject,
			validate:        validateServiceToken,
			cascades:        true,
			hiddenFromNodes: true,
			afterDelete:     revokeServiceToken,
			beforeGone:      revokeServiceToken,
			cascadeLock:     lockTokenCascade,
		}
	case registry.User.Kind:
		// A person signs up through AccountService, which creates their User
		// as the system; otherwise only platform.root reads and changes Users,
		// as no tenant grant covers the platform. No one deletes one: one
		// email has one User for good. Its applies hold its lock: that no two
		// Users share an email is checked against the others (validateUser);
		// and Plan's, as the plan it names must exist. No node agent needs
		// one, which holds an email.
		return kindHooks{
			target:          platformTarget,
			neverReuseNames: true,
			validate:        validateUser,
			writeLocks:      usersAndPlans,
			hiddenFromNodes: true,
			neverDeleted:    "users are never deleted; to remove a member, delete their platform role binding",
		}
	case registry.Invitation.Kind:
		// Keyed by the email it invites; only platform.root, through RBAC,
		// reads and writes it. Its writes hold User's lock, as there is none
		// for an email with a User, and Plan's, as the plan it names must
		// exist. No node agent needs one. See invitations.go.
		return kindHooks{
			target:          platformTarget,
			nameIsEmail:     true,
			validate:        validateInvitation,
			writeLocks:      usersAndPlans,
			hiddenFromNodes: true,
		}
	case registry.Container.Kind:
		// Deleting a container waits for its machine to be gone from the
		// node, so the deletion a client watches is the real one. Its writes
		// hold attach: the shares it declares are checked against it, and it
		// against them.
		return kindHooks{target: projectTarget, owner: ownedByProject, validate: validateContainer, cascades: true,
			writeLocks: only(attachLock)}
	case registry.Machine.Kind:
		// A machine is owned by the object it was made from, which is
		// already owned by the project.
		return kindHooks{
			target:        projectTarget,
			validate:      validateMachine,
			systemManaged: true,
			nodeOf:        func(obj proto.Message) string { return obj.(*computev1alpha1.Machine).GetSpec().GetNode() },
			writeLocks:    only(attachLock),
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
			writeLocks:   only(attachLock),
		}
	case registry.FileShare.Kind:
		// It is Pending from its creation; its node reports on it once its
		// server is placed there, and writes only its own conditions. Its
		// writes hold attach: the networks it may leave or join are checked
		// against what declares it and the networks being deleted, and those
		// against it. Deleting it deletes its server, and then waits for its
		// node to release its volume.
		return kindHooks{
			target:       projectTarget,
			owner:        ownedByProject,
			validate:     validateFileShare,
			beforeDelete: refuseDeletingDeclaredShare,
			cascades:     true,
			finalizers:   []string{NodeCleanupFinalizer},
			fromNode:     fileShareFromNode,
			initialStatus: func() proto.Message {
				return &storagev1alpha1.FileShareStatus{Phase: storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_PENDING}
			},
			nodeOf:     func(obj proto.Message) string { return obj.(*storagev1alpha1.FileShare).GetStatus().GetNode() },
			writeLocks: only(attachLock),
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
	case registry.MachineSize.Kind:
		// Platform administrators define the sizes machines are offered in;
		// every tenant chooses among them. See docs/decisions/machine-sizes.md.
		return kindHooks{
			target:        platformTarget,
			validate:      validateMachineSize,
			readableByAll: true,
		}
	case registry.VirtualMachine.Kind:
		// Deleting a VM waits for its machine to be gone from the node, and
		// for a root disk it owns. Its applies and deletes hold attach:
		// whether its root disk is owned by it is checked against it, both
		// ways (checkRootDiskOwner, refuseDeletingUnsettledVM), and so are
		// the file shares it declares.
		return kindHooks{
			target:       projectTarget,
			owner:        ownedByProject,
			validate:     validateVirtualMachine,
			beforeDelete: refuseDeletingUnsettledVM,
			cascades:     true,
			writeLocks:   only(attachLock),
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
			writeLocks: only(addressLock),
			finalizers: []string{AddressUnbindFinalizer},
		}
	case registry.PublicEndpoint.Kind:
		// Its hostname is claimed for good when it is made. An apply with
		// spec.hostnames holds customDomains: they are checked against the
		// domains and the other endpoints. One without holds nothing of
		// another's, and a delete only lets go.
		return kindHooks{
			target:     projectTarget,
			owner:      endpointOwner,
			validate:   validatePublicEndpoint,
			claim:      endpointClaim,
			applyLocks: endpointHostnamesLock,
		}
	case registry.CustomDomain.Kind:
		// A tenant's, as a Project is; its writes hold customDomains, shared
		// with PublicEndpoint (docs/decisions/custom-domains.md).
		return kindHooks{
			target:            customDomainTarget,
			targetNeedsObject: true,
			readableBy:        readsCustomDomain,
			owner:             customDomainOwner,
			validate:          validateCustomDomain,
			beforeDelete:      refuseDeletingDomainInUse,
			writeLocks:        only(customDomainsLock),
		}
	case registry.Network.Kind:
		// Its writes hold attach: whether a share is on it, and as it is
		// made whether a share naming it is on another network of an
		// overlapping range, are checked against the shares, and a share
		// joining it against it.
		return kindHooks{
			target:       projectTarget,
			owner:        ownedByProject,
			validate:     validateNetwork,
			beforeDelete: refuseDeletingInUse,
			finalizers:   []string{NetworkInUseFinalizer},
			writeLocks:   only(attachLock),
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
			writeLocks:   only(planLock),
		}
	case registry.Node.Kind:
		return kindHooks{
			target:        platformTarget,
			systemManaged: true,
			nodeOf:        func(obj proto.Message) string { return object.Meta(obj).GetName() },
			// Its placement is copied to every tenant's machine on it.
			validateStatus: func(status proto.Message) error {
				return computeapi.CheckPlacement(status.(*computev1alpha1.NodeStatus).GetPlacement())
			},
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

// usersAndPlans are the writeLocks of a kind checked against Users and
// naming a Plan.
func usersAndPlans(s *Server) []*sync.Mutex { return []*sync.Mutex{&s.users, &s.plans} }

// only makes one lock a kind's writeLocks.
func only(lock func(s *Server) *sync.Mutex) func(s *Server) []*sync.Mutex {
	return func(s *Server) []*sync.Mutex { return []*sync.Mutex{lock(s)} }
}

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
// the whole platform can choose a tenant's plan, which must exist; but a new
// tenant a person opens gets, and may name, the plan their User names. An
// update that leaves spec.plan empty keeps the stored one, and one naming
// the default plan stores none.
func validateTenant(ctx context.Context, s *Server, p auth.Principal, old, obj proto.Message) error {
	t := obj.(*tenancyv1alpha1.Tenant)
	var oldPlan string
	if old != nil {
		oldPlan = old.(*tenancyv1alpha1.Tenant).GetSpec().GetPlan()
	}
	if old == nil && p.Type == auth.User {
		u, err := s.userOf(ctx, p)
		if err != nil {
			return err
		}
		if own := u.GetSpec().GetPlan(); own != "" && (t.GetSpec().GetPlan() == "" || t.GetSpec().GetPlan() == own) {
			// Written into the incoming object, which the create stores.
			if t.Spec == nil {
				t.Spec = &tenancyv1alpha1.TenantSpec{}
			}
			t.Spec.Plan = own
			return s.requirePlan(ctx, "", own)
		}
	}
	if t.GetSpec().GetPlan() == "" && oldPlan != "" {
		// Written into the incoming object, which the update stores, so a
		// manifest that names no plan applies again unchanged, whoever
		// applies it.
		if t.Spec == nil {
			t.Spec = &tenancyv1alpha1.TenantSpec{}
		}
		t.Spec.Plan = oldPlan
	} else if t.GetSpec().GetPlan() == DefaultPlan {
		// Naming the default plan is how a tenant goes back to it; it is
		// stored as no plan, which is what the default is.
		t.Spec.Plan = ""
	}
	plan := t.GetSpec().GetPlan()
	admin := s.authz.Allowed(p, authz.Permission(registry.Tenant, authz.Apply), authz.Target{})
	if plan != oldPlan && !admin {
		return denied("only platform administrators set spec.plan")
	}
	return s.requirePlan(ctx, oldPlan, plan)
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

func validateRoleBinding(ctx context.Context, s *Server, p auth.Principal, old, obj proto.Message) error {
	spec := obj.(*iamv1alpha1.RoleBinding).GetSpec()
	role, ok := s.authz.Role(spec.GetRole())
	if !ok {
		return invalid("spec.role %q is not a known role", spec.GetRole())
	}
	switch sub := spec.GetSubject().GetSubject().(type) {
	case *iamv1alpha1.Subject_User:
		if sub.User == "" || !auth.ASCII(sub.User) || sub.User != strings.ToLower(sub.User) || !strings.Contains(sub.User, "@") {
			return invalid("spec.subject.user must be a lowercase ASCII email address")
		}
	case *iamv1alpha1.Subject_ServiceToken:
		if _, err := parseTokenSubject(sub.ServiceToken); err != nil {
			return err
		}
	default:
		return invalid("spec.subject is required")
	}
	// A binding naming a token is owned by it from its creation, so it
	// cannot come to name another, nor a token's binding someone else.
	if old != nil {
		was := old.(*iamv1alpha1.RoleBinding).GetSpec().GetSubject()
		if (was.GetServiceToken() != "" || spec.GetSubject().GetServiceToken() != "") && !proto.Equal(was, spec.GetSubject()) {
			return invalid("spec.subject cannot change on a binding that names a service token; delete it and make another")
		}
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
	// The scope is immutable, so a binding stops making an owner of its
	// tenant only by a change of role.
	if old != nil {
		if tenant, was := ownerTenant(old.(*iamv1alpha1.RoleBinding)); was {
			if _, is := ownerTenant(obj.(*iamv1alpha1.RoleBinding)); !is {
				return s.checkOwnerLeft(ctx, p, authz.Apply, tenant, map[string]bool{object.Meta(old).GetName(): true})
			}
		}
	}
	return nil
}

// warnRoleBinding says when a binding below the platform names a person who
// is not a platform member, whose grants count for nothing until they are
// one. Whether they were invited is not said: that is the platform's.
func warnRoleBinding(s *Server, obj proto.Message) []string {
	spec := obj.(*iamv1alpha1.RoleBinding).GetSpec()
	email := spec.GetSubject().GetUser()
	if email == "" || spec.GetScope().GetPlatform() != nil {
		return nil
	}
	if s.authz.IsMember(auth.Principal{Type: auth.User, ID: email}) {
		return nil
	}
	return []string{fmt.Sprintf("%s is not a platform member yet; the binding takes effect once an administrator invites them and they sign up", email)}
}

func validateContainer(ctx context.Context, s *Server, p auth.Principal, old, obj proto.Message) error {
	c := obj.(*computev1alpha1.Container)
	spec := c.GetSpec()
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
	if err := validateContainerVolumes(spec.GetVolumes()); err != nil {
		return err
	}
	if err := s.checkShareReach(ctx, meta.GetProject(), "container "+meta.GetName(), interfaceNetworks(spec.GetInterfaces()),
		containerShares(spec.GetVolumes())); err != nil {
		return err
	}
	if err := s.validateContainerResources(ctx, old, spec.GetResources()); err != nil {
		return err
	}
	// Its resources parse: validateContainerResources read them.
	cur, _ := containerShape(c)
	var was *machineShape
	if o, ok := old.(*computev1alpha1.Container); ok {
		if shape, ok := containerShape(o); ok {
			was = &shape
		}
	}
	return s.checkMachineShape(p, was, cur, "spec.resources.disk "+cmp.Or(spec.GetResources().GetDisk(), defaultDisk))
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

// interfaceNetworks are the networks a Container's or VirtualMachine's
// interfaces are on, as its machine has them: the default network when it
// names none.
func interfaceNetworks(nics []*computev1alpha1.NetworkInterface) []string {
	var out []string
	for _, nic := range machineInterfaces(nics) {
		out = append(out, nic.GetNetwork())
	}
	return out
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
	if err := checkMaximums(sz, "spec.resources", cmp.Or(r.GetCpu(), defaultCPU), cmp.Or(r.GetMemory(), defaultMemory), cmp.Or(r.GetDisk(), defaultDisk)); err != nil {
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

// checkMaximums refuses sizes past what a machine can have; at is the field
// holding cpu, memory and disk, such as spec.resources, and they are the
// quantities as written, for the message.
func checkMaximums(sz sizes, at, cpu, memory, disk string) error {
	if sz.cpu > maxCPU {
		return invalid("%s.cpu %s is above the maximum of %d vCPUs", at, cpu, maxCPU)
	}
	if sz.memory > maxMemory {
		return invalid("%s.memory %s is above the maximum of 4Ti", at, memory)
	}
	if sz.disk > maxDisk {
		return invalid("%s.disk %s is above the maximum of 64Ti", at, disk)
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
	if err := validateBootVolumes(spec); err != nil {
		return err
	}
	if f := spec.GetFirmware(); f != nil && f.GetRootDisk() == "" {
		return invalid("spec.firmware.root_disk is required")
	}
	if err := validateServerBoot(spec); err != nil {
		return err
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
	if err := checkMaximums(sz, "spec.resources", res.GetCpu(), res.GetMemory(), res.GetDisk()); err != nil {
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
	if err := s.checkAttaching(ctx, obj.(*computev1alpha1.Machine).GetMetadata(), oldSpec, spec); err != nil {
		return err
	}
	return s.checkDeclaring(ctx, obj.(*computev1alpha1.Machine).GetMetadata(), oldSpec, spec)
}

// validateServerBoot checks the machine of a file share's server: it
// names its share, and the size of its volume in whole Gi, attaches no
// disk, declares no share, and has no public address.
func validateServerBoot(spec *computev1alpha1.MachineSpec) error {
	b := spec.GetFileShareServer()
	if b == nil {
		return nil
	}
	if err := ValidName(b.GetFileShare()); err != nil {
		return invalid("spec.file_share_server.file_share: %v", err)
	}
	if size, err := quantity.Bytes(b.GetSize()); err != nil || size == 0 || size%quantity.GiB != 0 {
		return invalid("spec.file_share_server.size %q is not a size in whole Gi", b.GetSize())
	}
	if len(spec.GetDisks()) > 0 || len(spec.GetFileShares()) > 0 {
		return invalid("a file share's server attaches no disk and declares no file share")
	}
	for _, nic := range spec.GetInterfaces() {
		if nic.GetIpv6() != "" {
			return invalid("spec.interfaces[%s].ipv6: a file share's server has no public address", nic.GetName())
		}
	}
	return nil
}

// privateIPv4 are the RFC 1918 ranges networks may use.
var privateIPv4 = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

// validateNetwork checks a network's range, and, as a network is made, that
// no share naming it is on another network of an overlapping range. Its
// range never changes once made.
func validateNetwork(ctx context.Context, s *Server, _ auth.Principal, old, obj proto.Message) error {
	n := obj.(*networkv1alpha1.Network)
	cidr := n.GetSpec().GetIpv4Cidr()
	if n.GetMetadata().GetName() == computeapi.DefaultNetwork && cidr != DefaultNetworkCIDR {
		return invalid("network %q always has spec.ipv4_cidr %s; give another range another name", computeapi.DefaultNetwork, DefaultNetworkCIDR)
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
	if !slices.ContainsFunc(privateIPv4, func(r netip.Prefix) bool { return r.Bits() <= p.Bits() && r.Contains(p.Addr()) }) {
		return invalid("spec.ipv4_cidr %q is not inside a private range (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16)", cidr)
	}
	if old != nil {
		return nil
	}
	return s.checkRangeOfShares(ctx, n.GetMetadata().GetProject(), n.GetMetadata().GetName(), p)
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
