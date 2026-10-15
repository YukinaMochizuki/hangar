package apiserver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/cloudflare"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// DefaultEndpointDomain is the domain endpoint hostnames are in when the
// config names none: user content, apart from the control plane's domain.
const DefaultEndpointDomain = "hangarlab.dev"

// PublicEndpointsConfig configures PublicEndpoints
// (docs/decisions/public-endpoints.md).
type PublicEndpointsConfig struct {
	// Domain holds every endpoint's hostname; empty is
	// DefaultEndpointDomain.
	Domain string
	// Cloudflare makes and deletes the Access application that lets
	// everyone through to a PUBLIC endpoint, in AccountID, with the
	// reusable bypass policy BypassPolicyID that hangar-cloudflare made.
	// Nil leaves PUBLIC endpoints not Ready (CloudflareNotConfigured).
	Cloudflare     *cloudflare.Client
	AccountID      string
	BypassPolicyID string
}

// PublicEndpoint condition types and reasons, the public endpoint
// controller's alone.
const (
	// PublicEndpointReady says whether the endpoint serves: its target's
	// machine runs, and a PUBLIC endpoint's Access application is in place.
	PublicEndpointReady = "Ready"
	// PublicEndpointAccessApplication says where a PUBLIC endpoint's Access
	// application stands. Its reason Creating is written before the
	// application is made, so an application made by a reconcile that
	// stopped before recording it is still looked for and deleted.
	PublicEndpointAccessApplication = "AccessApplication"

	ReasonEndpointReady           = "Ready"
	ReasonMachineNotReady         = "MachineNotReady"
	ReasonCloudflareNotConfigured = "CloudflareNotConfigured"
	ReasonAccessApplicationFailed = "AccessApplicationFailed"
	ReasonAccessCreating          = "Creating"
	ReasonAccessCreated           = "Created"
	ReasonAccessNotNeeded         = "NotNeeded"
)

// AccessApplicationFinalizer keeps a deleted endpoint until no Access
// application of it may be left in Cloudflare; the public endpoint
// controller removes it then.
const AccessApplicationFinalizer = registry.Domain + "/access-application"

// endpointHostnameClaim is the claim kind every endpoint's hostname label
// is held under, forever, by its project and name.
const endpointHostnameClaim = "PublicEndpointHostname"

// accessAppPrefix names every Access application the apiserver makes, so
// one can be found again by its hostname alone.
const accessAppPrefix = cloudflare.EndpointAppPrefix

// endpointLabel is an endpoint's hostname label, the first and only label
// before the endpoint domain.
func endpointLabel(project, name string) string { return name + "-" + project }

// reservedLabelPrefixes are hostname labels kept for the platform's own
// names under the endpoint domain.
var reservedLabelPrefixes = []string{"hangar-"}

// checkEndpointLabel refuses a hostname label that is not one DNS label or
// that the platform keeps.
func checkEndpointLabel(label string) error {
	if len(label) > 63 {
		return invalid("hostname label %q is %d characters, more than 63: shorten the endpoint's name", label, len(label))
	}
	// RFC 5891 keeps labels with "--" in the third and fourth places for
	// internationalized names (xn--).
	if len(label) >= 4 && label[2:4] == "--" {
		return invalid("hostname label %q has \"--\" in its third and fourth characters, which DNS keeps for internationalized names", label)
	}
	for _, p := range reservedLabelPrefixes {
		if strings.HasPrefix(label, p) {
			return invalid("hostname label %q starts with %q, which the platform keeps", label, p)
		}
	}
	return nil
}

// endpointDomain is the domain endpoint hostnames are in.
func (s *Server) endpointDomain() string {
	if d := s.cfg.PublicEndpoints.Domain; d != "" {
		return d
	}
	return DefaultEndpointDomain
}

// endpointHostname is an endpoint's full hostname.
func (s *Server) endpointHostname(ep *networkv1alpha1.PublicEndpoint) string {
	return endpointLabel(ep.GetMetadata().GetProject(), ep.GetMetadata().GetName()) + "." + s.endpointDomain()
}

// endpointTargetOf is the kind and name of the object an endpoint reaches.
func endpointTargetOf(spec *networkv1alpha1.PublicEndpointSpec) (kind, name string, ok bool) {
	switch o := spec.GetTarget().GetObject().(type) {
	case *networkv1alpha1.PublicEndpointTarget_VirtualMachine:
		return registry.VirtualMachine.Kind, o.VirtualMachine, true
	case *networkv1alpha1.PublicEndpointTarget_Container:
		return registry.Container.Kind, o.Container, true
	}
	return "", "", false
}

// IsPublic reports whether an endpoint is open to everyone.
func IsPublic(ep *networkv1alpha1.PublicEndpoint) bool {
	return ep.GetSpec().GetAccess() == networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PUBLIC
}

// validatePublicEndpoint checks an endpoint's spec and its hostname label.
// That no other endpoint holds the hostname is the claim's to decide, on
// create (endpointClaim).
func validatePublicEndpoint(_ context.Context, _ *Server, _ auth.Principal, _, obj proto.Message) error {
	ep := obj.(*networkv1alpha1.PublicEndpoint)
	spec := ep.GetSpec()
	_, name, ok := endpointTargetOf(spec)
	if !ok {
		return invalid("spec.target needs a virtualMachine or a container")
	}
	if err := ValidName(name); err != nil {
		return invalid("spec.target: %v", err)
	}
	if p := spec.GetPort(); p < 1 || p > 65535 {
		return invalid("spec.port %d out of range 1-65535", p)
	}
	switch spec.GetAccess() {
	case networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_UNSPECIFIED,
		networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PRIVATE,
		networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PUBLIC:
	default:
		return invalid("spec.access %v is not PRIVATE or PUBLIC", spec.GetAccess())
	}
	// How many endpoints a tenant may have is its quota's (quotaCounters).
	return checkEndpointLabel(endpointLabel(ep.GetMetadata().GetProject(), ep.GetMetadata().GetName()))
}

// endpointClaim is the claim an endpoint takes on its hostname label when
// it is made, held by its project and name: the same endpoint made again
// takes it back, and no other endpoint ever does.
func endpointClaim(key resource.Key) globalClaim {
	label := endpointLabel(key.Project, key.Name)
	return globalClaim{
		key:    store.Key{Kind: nameClaimKind, Name: endpointHostnameClaim + "/" + label},
		holder: key.Project + "/" + key.Name,
		what:   fmt.Sprintf("hostname label %q", label),
	}
}

func decodeEndpoint(o store.Object) (*networkv1alpha1.PublicEndpoint, bool) {
	var ep networkv1alpha1.PublicEndpoint
	return &ep, proto.Unmarshal(o.Data, &ep) == nil
}

// endpoints lists the PublicEndpoints of a project, or every project's for
// "", each with its resource version.
func (s *Server) endpoints(ctx context.Context, project string) ([]*networkv1alpha1.PublicEndpoint, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.PublicEndpoint.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	out := make([]*networkv1alpha1.PublicEndpoint, 0, len(objs))
	for _, o := range objs {
		obj, err := s.backend(registry.PublicEndpoint).decode(o)
		if err != nil {
			return nil, fmt.Errorf("decode public endpoint %s/%s: %w", o.Project, o.Name, err)
		}
		out = append(out, obj.(*networkv1alpha1.PublicEndpoint))
	}
	return out, nil
}

// endpointOwner makes an endpoint belong to its project, unless the system
// made it for an object that owns it already, such as a container's public
// port: one object has one controlling owner.
func endpointOwner(ctx context.Context, s *Server, obj proto.Message) (registry.Resource, resource.Key, bool) {
	if slices.ContainsFunc(object.Meta(obj).GetOwnerReferences(), (*metav1.OwnerReference).GetController) {
		return registry.Resource{}, resource.Key{}, false
	}
	return ownedByProject(ctx, s, obj)
}

// reconcilePublicEndpoint reports where an endpoint's target runs, keeps a
// PUBLIC endpoint's Access application in Cloudflare and a PRIVATE one
// without, and lets a deleted endpoint go once no application of it can be
// left.
func (s *Server) reconcilePublicEndpoint(ctx context.Context, k controller.Key) error {
	key := resource.Key{Project: k.Project, Name: k.Name}
	obj, err := s.get(ctx, registry.PublicEndpoint, key)
	if err != nil || obj == nil {
		return err
	}
	ep := obj.(*networkv1alpha1.PublicEndpoint)
	if ep.GetMetadata().GetDeleteTime() != nil {
		if mayHaveAccessApp(ep) {
			if err := s.deleteAccessApps(ctx, ep); err != nil {
				return err
			}
		}
		return s.RemoveFinalizer(ctx, registry.PublicEndpoint, key, AccessApplicationFinalizer)
	}
	gen := ep.GetMetadata().GetGeneration()
	status := &networkv1alpha1.PublicEndpointStatus{}
	if ep.GetStatus() != nil {
		status = proto.Clone(ep.GetStatus()).(*networkv1alpha1.PublicEndpointStatus)
	}
	status.ObservedGeneration = gen
	status.Hostname = s.endpointHostname(ep)
	status.Url = "https://" + status.Hostname
	machine, node, running, err := s.endpointMachine(ctx, ep)
	if err != nil {
		return err
	}
	status.Machine, status.Node = machine, node

	access, appErr := s.convergeAccessApp(ctx, ep, status)
	var intent bool
	if access != nil {
		access.ObservedGeneration = gen
		intent = access.GetReason() == ReasonAccessCreating && !creatingStored(ep)
		status.Conditions = conditions.Set(status.GetConditions(), access, s.clock.Now())
	}
	ready := &metav1.Condition{Type: PublicEndpointReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, ObservedGeneration: gen}
	switch {
	case running.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE:
		ready.Reason, ready.Message = running.GetReason(), running.GetMessage()
	case IsPublic(ep) && access.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE:
		ready.Reason, ready.Message = access.GetReason(), access.GetMessage()
	default:
		ready.Status, ready.Reason = metav1.ConditionStatus_CONDITION_STATUS_TRUE, ReasonEndpointReady
		ready.Message = status.GetUrl() + " reaches port " + fmt.Sprint(ep.GetSpec().GetPort()) + " of " + machine
	}
	status.Conditions = conditions.Set(status.GetConditions(), ready, s.clock.Now())
	if !proto.Equal(status, ep.GetStatus()) {
		rv := ep.GetMetadata().GetResourceVersion()
		if _, err := s.backend(registry.PublicEndpoint).UpdateStatus(s.system(ctx), key, status, &rv); err != nil {
			return err
		}
		if intent {
			// The application is made once the intent is stored; the
			// status written queues the endpoint again.
			return nil
		}
	}
	return appErr
}

// endpointMachine finds the machine an endpoint reaches and says whether it
// runs: a Ready condition whose reason says why not.
func (s *Server) endpointMachine(ctx context.Context, ep *networkv1alpha1.PublicEndpoint) (machine, node string, running *metav1.Condition, err error) {
	kind, name, _ := endpointTargetOf(ep.GetSpec())
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind, Project: ep.GetMetadata().GetProject()})
	if err != nil {
		return "", "", nil, err
	}
	for _, o := range objs {
		m, ok := decodeMachine(o)
		if !ok {
			return "", "", nil, fmt.Errorf("decode machine %s/%s", o.Project, o.Name)
		}
		owner := ownerOfMachine(m)
		if owner.kind != kind || owner.name != name || m.GetMetadata().GetDeleteTime() != nil {
			continue
		}
		var n *computev1alpha1.Node
		if nn := m.GetSpec().GetNode(); nn != "" {
			nobj, err := s.get(ctx, registry.Node, resource.Key{Name: nn})
			if err != nil {
				return "", "", nil, err
			}
			if nobj != nil {
				n = nobj.(*computev1alpha1.Node)
			}
		}
		running = machineReadiness(m, n)
		if running.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
			running.Reason, running.Message = ReasonMachineNotReady, fmt.Sprintf("%s %s is not running: %s %s", kind, name, running.GetReason(), running.GetMessage())
		}
		return o.Name, m.GetSpec().GetNode(), running, nil
	}
	return "", "", &metav1.Condition{Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: ReasonTargetNotFound,
		Message: fmt.Sprintf("%s %s does not exist, or has no machine yet", kind, name)}, nil
}

// mayHaveAccessApp reports whether an Access application of ep may exist:
// one is recorded, or one was about to be made.
func mayHaveAccessApp(ep *networkv1alpha1.PublicEndpoint) bool {
	return ep.GetStatus().GetAccessApplicationId() != "" || creatingStored(ep)
}

// creatingStored reports whether the stored endpoint records that its Access
// application is being made.
func creatingStored(ep *networkv1alpha1.PublicEndpoint) bool {
	return conditions.Find(ep.GetStatus().GetConditions(), PublicEndpointAccessApplication).GetReason() == ReasonAccessCreating
}

// accessAppName is the name of the Access application of hostname: fixed,
// so an application made but not recorded is found again.
func accessAppName(hostname string) string { return accessAppPrefix + hostname }

// convergeAccessApp makes a PUBLIC endpoint's Access application, adopting
// one made before, or deletes a PRIVATE endpoint's, and records it in
// status. It returns the AccessApplication condition, nil for an endpoint
// that never had one, and an error to retry with.
func (s *Server) convergeAccessApp(ctx context.Context, ep *networkv1alpha1.PublicEndpoint, status *networkv1alpha1.PublicEndpointStatus) (*metav1.Condition, error) {
	cond := func(st metav1.ConditionStatus, reason, msg string) *metav1.Condition {
		return &metav1.Condition{Type: PublicEndpointAccessApplication, Status: st, Reason: reason, Message: msg}
	}
	cf := s.cfg.PublicEndpoints
	if !IsPublic(ep) {
		if !mayHaveAccessApp(ep) {
			if conditions.Find(status.GetConditions(), PublicEndpointAccessApplication) == nil {
				return nil, nil
			}
			return cond(metav1.ConditionStatus_CONDITION_STATUS_FALSE, ReasonAccessNotNeeded, "a private endpoint needs no Access application"), nil
		}
		if err := s.deleteAccessApps(ctx, ep); err != nil {
			return cond(metav1.ConditionStatus_CONDITION_STATUS_FALSE, ReasonAccessApplicationFailed, err.Error()), err
		}
		status.AccessApplicationId = ""
		return cond(metav1.ConditionStatus_CONDITION_STATUS_FALSE, ReasonAccessNotNeeded, "a private endpoint needs no Access application"), nil
	}
	if cf.Cloudflare == nil {
		return cond(metav1.ConditionStatus_CONDITION_STATUS_FALSE, ReasonCloudflareNotConfigured,
			"the apiserver has no Cloudflare API token, so it cannot open a public endpoint"), nil
	}
	host := s.endpointHostname(ep)
	if id := status.GetAccessApplicationId(); id != "" {
		app, err := cf.Cloudflare.GetAccessApp(ctx, cf.AccountID, id)
		switch {
		case err == nil && strings.EqualFold(app.Domain, host):
			return cond(metav1.ConditionStatus_CONDITION_STATUS_TRUE, ReasonAccessCreated, "Access application "+id+" lets everyone through"), nil
		case err == nil || cloudflare.IsNotFound(err):
			// Gone, or no longer this hostname's (changed in the dashboard,
			// which nobody should do): made again below.
			status.AccessApplicationId = ""
		default:
			return cond(metav1.ConditionStatus_CONDITION_STATUS_FALSE, ReasonAccessApplicationFailed, err.Error()), err
		}
	}
	if !creatingStored(ep) {
		// Recorded before anything is made: a reconcile that stops between
		// making the application and recording it leaves it findable.
		return cond(metav1.ConditionStatus_CONDITION_STATUS_FALSE, ReasonAccessCreating, "the Access application is being made"), nil
	}
	apps, err := cf.Cloudflare.FindAccessApps(ctx, cf.AccountID, accessAppName(host))
	if err != nil {
		return cond(metav1.ConditionStatus_CONDITION_STATUS_FALSE, ReasonAccessCreating, err.Error()), err
	}
	var app cloudflare.AccessApp
	if i := slices.IndexFunc(apps, func(a cloudflare.AccessApp) bool { return strings.EqualFold(a.Domain, host) }); i >= 0 {
		app = apps[i]
		s.log.Info("adopted an Access application made before", "hostname", host, "id", app.ID)
	} else {
		if app, err = cf.Cloudflare.CreateAccessApp(ctx, cf.AccountID, s.bypassApp(host)); err != nil {
			return cond(metav1.ConditionStatus_CONDITION_STATUS_FALSE, ReasonAccessCreating, err.Error()), err
		}
		s.log.Info("made an Access application for a public endpoint", "hostname", host, "id", app.ID)
	}
	status.AccessApplicationId = app.ID
	return cond(metav1.ConditionStatus_CONDITION_STATUS_TRUE, ReasonAccessCreated, "Access application "+app.ID+" lets everyone through"), nil
}

// bypassApp is the Access application that lets everyone through to
// hostname: more specific than the wildcard application every other
// endpoint is behind, so Access applies it instead.
func (s *Server) bypassApp(hostname string) cloudflare.AccessAppInput {
	return cloudflare.AccessAppInput{
		Name:                    accessAppName(hostname),
		Domain:                  hostname,
		Type:                    "self_hosted",
		SessionDuration:         "24h",
		AllowedIdPs:             []string{},
		Destinations:            []cloudflare.AppDestination{{Type: "public", URI: hostname}},
		Policies:                []cloudflare.PolicyLink{{ID: s.cfg.PublicEndpoints.BypassPolicyID, Precedence: 1}},
		HTTPOnlyCookieAttribute: true,
		SameSiteCookieAttribute: "lax",
	}
}

// deleteAccessApps deletes every Access application of ep's hostname: the
// one recorded and any made but not recorded.
func (s *Server) deleteAccessApps(ctx context.Context, ep *networkv1alpha1.PublicEndpoint) error {
	cf := s.cfg.PublicEndpoints
	if cf.Cloudflare == nil {
		return fmt.Errorf("%w: public endpoint %s/%s may have an Access application, and the apiserver has no Cloudflare API token to delete it with",
			controller.ErrWaiting, ep.GetMetadata().GetProject(), ep.GetMetadata().GetName())
	}
	host := s.endpointHostname(ep)
	ids := map[string]bool{}
	if id := ep.GetStatus().GetAccessApplicationId(); id != "" {
		ids[id] = true
	}
	apps, err := cf.Cloudflare.FindAccessApps(ctx, cf.AccountID, accessAppName(host))
	if err != nil {
		return err
	}
	for _, a := range apps {
		if strings.EqualFold(a.Domain, host) {
			ids[a.ID] = true
		}
	}
	for id := range ids {
		if err := cf.Cloudflare.DeleteAccessApp(ctx, cf.AccountID, id); err != nil && !cloudflare.IsNotFound(err) {
			return err
		}
		s.log.Info("deleted a public endpoint's Access application", "hostname", host, "id", id)
	}
	return nil
}

// endpointsOfMachine maps a machine to the endpoints that reach its owner,
// or that report it.
func (s *Server) endpointsOfMachine(ctx context.Context, o store.Object) []controller.Key {
	m, ok := decodeMachine(o)
	if !ok {
		return nil
	}
	owner := ownerOfMachine(m)
	all, err := s.endpoints(ctx, o.Project)
	if err != nil {
		s.log.Error("listing public endpoints failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, ep := range all {
		kind, name, _ := endpointTargetOf(ep.GetSpec())
		if (kind == owner.kind && name == owner.name) || ep.GetStatus().GetMachine() == o.Name {
			keys = append(keys, controller.Key{Project: o.Project, Name: ep.GetMetadata().GetName()})
		}
	}
	return keys
}

// endpointsOnNode maps a node to the endpoints whose machines it runs: a
// node going quiet changes whether they serve.
func (s *Server) endpointsOnNode(ctx context.Context, n store.Object) []controller.Key {
	all, err := s.endpoints(ctx, "")
	if err != nil {
		s.log.Error("listing public endpoints failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, ep := range all {
		if ep.GetStatus().GetNode() == n.Name {
			keys = append(keys, controller.Key{Project: ep.GetMetadata().GetProject(), Name: ep.GetMetadata().GetName()})
		}
	}
	return keys
}

// ContainerPublicEndpoints is the Container condition saying whether the
// endpoints of its public ports are in place; the container endpoint
// controller's alone, and set only on a container with public ports.
const ContainerPublicEndpoints = "PublicEndpoints"

// containerEndpointName is the endpoint a container's public port gets.
func containerEndpointName(container, port string) string { return container + "-" + port }

// validateContainerPorts checks what a container's public ports make: a TCP
// endpoint whose name and hostname label fit.
func validateContainerPorts(project, container string, ports []*computev1alpha1.ContainerPort) error {
	for _, port := range ports {
		if !port.GetPublic() {
			continue
		}
		if port.GetProtocol() == computev1alpha1.Protocol_PROTOCOL_UDP {
			return invalid("spec.ports[%s]: a public port is TCP; HTTPS does not reach UDP", port.GetName())
		}
		name := containerEndpointName(container, port.GetName())
		if err := ValidName(name); err != nil {
			return invalid("spec.ports[%s]: its public endpoint's name: %v", port.GetName(), err)
		}
		if err := checkEndpointLabel(endpointLabel(project, name)); err != nil {
			return invalid("spec.ports[%s]: %v", port.GetName(), messageOf(err))
		}
	}
	return nil
}

// reconcileContainerEndpoints keeps one PRIVATE endpoint, owned by the
// container, for each of its public ports, and none for the others. It
// never touches an endpoint the container does not own; one of the name it
// wants is reported on the container.
func (s *Server) reconcileContainerEndpoints(ctx context.Context, k controller.Key) error {
	key := resource.Key{Project: k.Project, Name: k.Name}
	obj, err := s.get(ctx, registry.Container, key)
	if err != nil || obj == nil {
		return err
	}
	c := obj.(*computev1alpha1.Container)
	if c.GetMetadata().GetDeleteTime() != nil {
		// Its endpoints go with it, through their owner reference.
		return nil
	}
	want := map[string]int32{}
	for _, p := range c.GetSpec().GetPorts() {
		if p.GetPublic() {
			want[containerEndpointName(k.Name, p.GetName())] = p.GetPort()
		}
	}
	all, err := s.endpoints(ctx, k.Project)
	if err != nil {
		return err
	}
	uid := c.GetMetadata().GetUid()
	var taken []string
	b := s.backend(registry.PublicEndpoint)
	for _, ep := range all {
		name := ep.GetMetadata().GetName()
		port, wanted := want[name]
		if !ownedBy(ep.GetMetadata(), uid) {
			if wanted {
				taken = append(taken, name)
				delete(want, name)
			}
			continue
		}
		delete(want, name)
		if ep.GetMetadata().GetDeleteTime() != nil {
			continue
		}
		epKey := resource.Key{Project: k.Project, Name: name}
		if !wanted {
			rv := ep.GetMetadata().GetResourceVersion()
			if _, err := b.Delete(s.system(ctx), epKey, &rv); err != nil {
				return err
			}
			continue
		}
		if spec := containerEndpointSpec(k.Name, port); !proto.Equal(spec, ep.GetSpec()) {
			updated := proto.Clone(ep).(*networkv1alpha1.PublicEndpoint)
			updated.Spec = spec
			if _, err := b.Apply(s.system(ctx), updated); err != nil {
				return err
			}
		}
	}
	for name, port := range want {
		created := &networkv1alpha1.PublicEndpoint{
			Metadata: &metav1.ObjectMeta{
				Project: k.Project, Name: name,
				OwnerReferences: []*metav1.OwnerReference{{
					ApiVersion: registry.Container.APIVersion(), Kind: registry.Container.Kind,
					Name: k.Name, Uid: uid, Controller: true,
				}},
			},
			Spec: containerEndpointSpec(k.Name, port),
		}
		if _, err := b.Create(s.system(ctx), created); err != nil {
			switch connect.CodeOf(err) {
			case connect.CodeAlreadyExists:
				// Made since the list; the next reconcile reads it.
			case connect.CodeInvalidArgument, connect.CodeFailedPrecondition:
				taken = append(taken, name+" ("+messageOf(err)+")")
			default:
				return err
			}
		}
	}
	return s.reportContainerEndpoints(ctx, c, taken)
}

func containerEndpointSpec(container string, port int32) *networkv1alpha1.PublicEndpointSpec {
	return &networkv1alpha1.PublicEndpointSpec{
		Target: &networkv1alpha1.PublicEndpointTarget{Object: &networkv1alpha1.PublicEndpointTarget_Container{Container: container}},
		Port:   port,
		Access: networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PRIVATE,
	}
}

// reportContainerEndpoints writes the container's PublicEndpoints
// condition, on the latest copy of its status, which the container
// controller writes too: only on a container with public ports, and taken
// from one that no longer has any.
func (s *Server) reportContainerEndpoints(ctx context.Context, c *computev1alpha1.Container, taken []string) error {
	public := slices.ContainsFunc(c.GetSpec().GetPorts(), (*computev1alpha1.ContainerPort).GetPublic)
	if !public && conditions.Find(c.GetStatus().GetConditions(), ContainerPublicEndpoints) == nil {
		// Nothing of this controller's to write: the container controller's
		// status, nil or not, is left as it is.
		return nil
	}
	status := &computev1alpha1.ContainerStatus{}
	if c.GetStatus() != nil {
		status = proto.Clone(c.GetStatus()).(*computev1alpha1.ContainerStatus)
	}
	if !public {
		status.Conditions = slices.DeleteFunc(status.Conditions, func(x *metav1.Condition) bool { return x.GetType() == ContainerPublicEndpoints })
	} else {
		cond := &metav1.Condition{Type: ContainerPublicEndpoints, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Created",
			ObservedGeneration: c.GetMetadata().GetGeneration()}
		if len(taken) > 0 {
			slices.Sort(taken)
			cond.Status, cond.Reason = metav1.ConditionStatus_CONDITION_STATUS_FALSE, "NameTaken"
			cond.Message = "public endpoints the container does not own hold these names: " + strings.Join(taken, ", ")
		}
		status.Conditions = conditions.Set(status.GetConditions(), cond, s.clock.Now())
	}
	if proto.Equal(status, c.GetStatus()) {
		return nil
	}
	rv := c.GetMetadata().GetResourceVersion()
	key := resource.Key{Project: c.GetMetadata().GetProject(), Name: c.GetMetadata().GetName()}
	_, err := s.backend(registry.Container).UpdateStatus(s.system(ctx), key, status, &rv)
	return err
}

// containersOfEndpoint maps an endpoint to the container that owns it, and
// to those of its project that want its name for a public port: one held
// by another endpoint is theirs to take once that one goes.
func (s *Server) containersOfEndpoint(ctx context.Context, o store.Object) []controller.Key {
	ep, ok := decodeEndpoint(o)
	if !ok {
		return nil
	}
	var keys []controller.Key
	for _, ref := range ep.GetMetadata().GetOwnerReferences() {
		if ref.GetController() && ref.GetKind() == registry.Container.Kind {
			keys = append(keys, controller.Key{Project: o.Project, Name: ref.GetName()})
		}
	}
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Container.Kind, Project: o.Project})
	if err != nil {
		s.log.Error("listing containers failed", "error", err)
		return keys
	}
	for _, co := range objs {
		var c computev1alpha1.Container
		if proto.Unmarshal(co.Data, &c) != nil {
			continue
		}
		for _, p := range c.GetSpec().GetPorts() {
			if p.GetPublic() && containerEndpointName(co.Name, p.GetName()) == o.Name {
				keys = append(keys, controller.Key{Project: o.Project, Name: co.Name})
			}
		}
	}
	return keys
}
