package apiserver

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/sshws"
)

// machineKind is a kind whose objects run as machines, and what their
// machines can do for a caller.
type machineKind struct {
	// what is the kind in words, such as "virtual machine".
	what string
	// machine is the name of the machine the object name runs as.
	machine func(name string) string
	// capabilities are what its machines can be asked; any other capability
	// fails with Unimplemented.
	capabilities []authz.Verb
}

// machineKinds maps every kind whose objects run as machines to them: a
// kind added here gets the capabilities it lists, under the machines'
// permissions, with no permission of its own; any other capability fails
// with Unimplemented. See docs/decisions/machine-model.md.
var machineKinds = map[string]machineKind{
	registry.Container.Kind:      {what: "container", machine: machineName, capabilities: []authz.Verb{authz.Logs, authz.Exec}},
	registry.VirtualMachine.Kind: {what: "virtual machine", machine: vmMachineName, capabilities: []authz.Verb{authz.Logs, authz.SSH}},
}

// kindNamed is the kind a capability's request names, by kind, as
// "Container".
func kindNamed(kind string) (registry.Resource, error) {
	for _, r := range registry.All {
		if r.Kind == kind {
			return r, nil
		}
	}
	if kind == "" {
		return registry.Resource{}, invalid("kind is required")
	}
	return registry.Resource{}, invalid("unknown kind %q", kind)
}

// kindOfPlural is the kind a capability's path names, by plural, as
// "containers".
func kindOfPlural(plural string) (registry.Resource, error) {
	for _, r := range registry.All {
		if r.Plural == plural {
			return r, nil
		}
	}
	return registry.Resource{}, invalid("unknown kind %q", plural)
}

// reached is an object whose machine a caller may use a capability of.
type reached struct {
	principal auth.Principal
	kind      registry.Resource
	key       resource.Key
	obj       proto.Message
}

// String is the object in words, such as `container "api"`.
func (r *reached) String() string {
	return fmt.Sprintf("%s %q", machineKinds[r.kind.Kind].what, r.key.Name)
}

// reach authorizes the capability verb of the machine of the object of kind
// at key and reads the object. A kind without the capability is refused
// first, from the kind alone; then check, if any, checks the rest of the
// request. Nothing about the object is told before the caller is
// authorized, as the permission is on machines in the object's project:
// the machine an object runs as is in its project. A stream that reaches
// the machine, a log followed, an exec or an SSH tunnel, stands only as long
// as the caller may still use the capability (auth.Recheck).
func (s *Server) reach(ctx context.Context, kind registry.Resource, key resource.Key, verb authz.Verb, check func() error) (*reached, error) {
	machines := s.backend(registry.Machine)
	p, err := machines.begin(ctx)
	if err != nil {
		return nil, err
	}
	mk, ok := machineKinds[kind.Kind]
	if !ok {
		return nil, invalid("%s do not run as machines", kind.Plural)
	}
	if !slices.Contains(mk.capabilities, verb) {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("%s is not supported on %ss yet", verb, mk.what))
	}
	b := s.backend(kind)
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	if check != nil {
		if err := check(); err != nil {
			return nil, err
		}
	}
	if err := s.mayUse(p, b, key, verb); err != nil {
		return nil, err
	}
	auth.Recheck(ctx, func(ctx context.Context) error {
		p, err := machines.begin(ctx)
		if err != nil {
			return err
		}
		return s.mayUse(p, b, key, verb)
	})
	cur, err := s.store.Get(ctx, b.storeKey(key))
	if err != nil {
		return nil, apiError(err)
	}
	obj, err := b.decode(cur)
	if err != nil {
		return nil, apiError(err)
	}
	return &reached{principal: p, kind: kind, key: key, obj: obj}, nil
}

// mayUse authorizes the capability verb of the machine of the object of b's
// kind at key.
func (s *Server) mayUse(p auth.Principal, b *backend, key resource.Key, verb authz.Verb) error {
	// Not machines.allowed: a Machine is system-managed, and that refuses
	// every caller any verb but reads. The capabilities are no write to it.
	if !s.authz.Allowed(p, authz.Permission(registry.Machine, verb), s.backend(registry.Machine).target(key, nil)) {
		return denied("%s may not %s %s", p, verb, b.asked(key))
	}
	return nil
}

// machineOf is the machine the object r runs as, once it is placed on a
// node. A machine is the object's only if the object owns it: one left by
// an object deleted and made again under its name is not.
func (s *Server) machineOf(ctx context.Context, r *reached) (*computev1alpha1.Machine, error) {
	name := machineKinds[r.kind.Kind].machine(r.key.Name)
	mobj, err := s.get(ctx, registry.Machine, resource.Key{Project: r.key.Project, Name: name})
	if err != nil {
		return nil, apiError(err)
	}
	machine, _ := mobj.(*computev1alpha1.Machine)
	if machine == nil || !ownedBy(machine.GetMetadata(), object.Meta(r.obj).GetUid()) {
		return nil, failedPrecondition("%s has no machine yet", r)
	}
	if machine.GetSpec().GetNode() == "" {
		return nil, failedPrecondition("%s is not placed on a node yet", r)
	}
	return machine, nil
}

// checkRunning fails unless the machine of r is running.
func checkRunning(r *reached, machine *computev1alpha1.Machine) error {
	if phase := machine.GetStatus().GetPhase(); phase != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING {
		return failedPrecondition("%s is not running: its machine is %s", r,
			strings.ToLower(strings.TrimPrefix(phase.String(), "MACHINE_PHASE_")))
	}
	return nil
}

// callMachine begins a call to the node of the machine r runs as.
func (s *Server) callMachine(ctx context.Context, r *reached, machine *computev1alpha1.Machine) (*nodeCall, error) {
	project, err := s.get(ctx, registry.Project, resource.Key{Name: r.key.Project})
	if err != nil {
		return nil, apiError(err)
	}
	if project == nil {
		return nil, failedPrecondition("project %q is gone", r.key.Project)
	}
	return s.callNode(r.principal, project.(*tenancyv1alpha1.Project).GetSpec().GetTenant(), machine.GetSpec().GetNode(), r.String())
}

// machineTarget is a running machine a caller reached, and the call to its
// node.
type machineTarget struct {
	principal auth.Principal
	// client is the caller's address, as the listener established it
	// (auth.ClientIP); empty on a unix socket.
	client  string
	kind    registry.Resource
	key     resource.Key
	machine string // uid
	call    *nodeCall
}

// machineTarget reaches the running machine of the object of kind at key
// for verb and begins the call to its node; fits, if any, refuses a machine
// that cannot serve it.
func (s *Server) machineTarget(ctx context.Context, kind registry.Resource, key resource.Key, verb authz.Verb,
	fits func(*reached, *computev1alpha1.Machine) error) (*machineTarget, error) {
	r, err := s.reach(ctx, kind, key, verb, nil)
	if err != nil {
		return nil, err
	}
	machine, err := s.machineOf(ctx, r)
	if err != nil {
		return nil, err
	}
	if fits != nil {
		if err := fits(r, machine); err != nil {
			return nil, err
		}
	}
	if err := checkRunning(r, machine); err != nil {
		return nil, err
	}
	call, err := s.callMachine(ctx, r, machine)
	if err != nil {
		return nil, err
	}
	return &machineTarget{principal: r.principal, client: clientIP(ctx), kind: kind, key: key, machine: machine.GetMetadata().GetUid(), call: call}, nil
}

// auditAttrs are what every audit line of a capability on x says of it.
func (x *machineTarget) auditAttrs() []any {
	return []any{"principal", x.principal.String(), "client_ip", x.client, "project", x.key.Project, "kind", x.kind.Kind, "name", x.key.Name, "machine", x.machine}
}

// clientIP is the address the request came from, as its listener
// established it (auth.ClientIP), for the audit lines; empty when the
// listener has none, a unix socket's.
func clientIP(ctx context.Context) string {
	if ip, ok := auth.ClientIP(ctx); ok {
		return ip.String()
	}
	return ""
}

// machineSocket serves the WebSockets of the machines' capabilities, an
// exec or an SSH tunnel, on the public listener; see
// proto/hangar/compute/v1alpha1/machine_capability.proto.
func (s *Server) machineSocket(chain auth.Chain) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		route, ok := execws.RouteOf(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch route.Action {
		case execws.Action:
			s.serveExec(chain, w, r, route)
		case sshws.Action:
			s.serveSSH(chain, w, r, route)
		default:
			http.NotFound(w, r)
		}
	}
}

// machineCapabilities serves MachineCapabilityService.
type machineCapabilities struct{ s *Server }
