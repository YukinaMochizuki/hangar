package node

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// PortForwardReleased is the PortForward condition the node reports once it
// has closed the port of a PortForward being deleted, as the apiserver
// waits for (docs/decisions/networking.md, "Port forwards").
const PortForwardReleased = "Released"

// forwardsKey is the forward controller's one key: the host's forwards are
// ensured as one set.
var forwardsKey = controller.Key{Name: "forwards"}

// forwardController keeps the host's port forwards those of the
// PortForwards the apiserver gives this node, which are the only ones it
// reads, on changes to them, to machines and to networks, and when the
// network driver notices the host changed.
func (a *Agent) forwardController() *controller.Controller {
	all := func(context.Context, store.Object) []controller.Key { return []controller.Key{forwardsKey} }
	return &controller.Controller{
		Name:       "port forwards",
		Source:     controller.APISource{Client: a.api},
		Reconciler: controller.ReconcilerFunc(a.reconcileForwards),
		Watches: []controller.Watch{
			{Kind: registry.PortForward.Kind, Map: all},
			// A machine moving, going or given its address; a network
			// getting its transit link.
			{Kind: registry.Machine.Kind, Map: all, SkipResync: true},
			{Kind: registry.Network.Kind, Map: all, SkipResync: true},
		},
		Trigger: a.forwardTrigger,
		Clock:   a.cfg.Clock,
		Log:     a.cfg.Log,
	}
}

// reconcileForwards hands the network driver the forward of every
// PortForward this node opens that leads to a machine on it, then reports
// on each of them being deleted that its port is closed. Without a shared
// address the host forwards nothing, and still reports.
func (a *Agent) reconcileForwards(ctx context.Context, _ controller.Key) error {
	objs, _, err := a.api.ListAll(ctx, registry.PortForward, client.ListOptions{})
	if err != nil {
		return err
	}
	var want []driver.Forward
	var released []*networkv1alpha1.PortForward
	for _, obj := range objs {
		pf := obj.(*networkv1alpha1.PortForward)
		if pf.GetStatus().GetNode() != a.cfg.Name {
			continue
		}
		if pf.GetMetadata().GetDeleteTime() != nil {
			released = append(released, pf)
			continue
		}
		if !a.cfg.SharedIPv4.IsValid() {
			continue
		}
		f, ok, err := a.forwardOf(ctx, pf)
		if err != nil {
			return err
		}
		if ok {
			want = append(want, f)
		}
	}
	slices.SortFunc(want, func(x, y driver.Forward) int {
		return cmp.Or(cmp.Compare(x.Protocol, y.Protocol), cmp.Compare(x.Port, y.Port))
	})
	a.host.Lock()
	err = a.cfg.Drivers.Network.EnsureForwards(ctx, want)
	a.host.Unlock()
	if err != nil {
		return fmt.Errorf("forwarding ports: %w", err)
	}
	for _, pf := range released {
		if err := a.reportReleased(ctx, pf); err != nil {
			return err
		}
	}
	return nil
}

// forwardOf is the forward a PortForward makes on this host: none while it
// has no port, or leads to no interface with an address of a machine here.
func (a *Agent) forwardOf(ctx context.Context, pf *networkv1alpha1.PortForward) (driver.Forward, bool, error) {
	project := pf.GetMetadata().GetProject()
	port, name := pf.GetStatus().GetPort(), pf.GetStatus().GetMachine()
	if port <= 0 || port > 65535 || name == "" {
		return driver.Forward{}, false, nil
	}
	var protocol driver.ForwardProtocol
	switch pf.GetSpec().GetProtocol() {
	case networkv1alpha1.Protocol_PROTOCOL_TCP:
		protocol = driver.TCP
	case networkv1alpha1.Protocol_PROTOCOL_UDP:
		protocol = driver.UDP
	default:
		return driver.Forward{}, false, nil
	}
	m, err := a.getMachine(ctx, controller.Key{Project: project, Name: name})
	if err != nil || m == nil || m.GetSpec().GetNode() != a.cfg.Name || m.GetMetadata().GetDeleteTime() != nil {
		return driver.Forward{}, false, err
	}
	iface := cmp.Or(pf.GetSpec().GetTarget().GetInterface(), "eth0")
	i := slices.IndexFunc(m.GetSpec().GetInterfaces(), func(nic *computev1alpha1.MachineInterface) bool { return nic.GetName() == iface })
	if i < 0 {
		return driver.Forward{}, false, nil
	}
	nic := m.GetSpec().GetInterfaces()[i]
	addr, ok := parsePrefix(nic.GetIpv4())
	if !ok {
		return driver.Forward{}, false, nil
	}
	n, err := a.networks.GetNetwork(ctx, connect.NewRequest(&networkv1alpha1.GetNetworkRequest{Project: project, Name: nic.GetNetwork()}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return driver.Forward{}, false, nil
	}
	if err != nil {
		return driver.Forward{}, false, err
	}
	uid := n.Msg.GetNetwork().GetMetadata().GetUid()
	target := pf.GetSpec().GetTargetPort()
	if uid == "" || target <= 0 || target > 65535 {
		return driver.Forward{}, false, nil
	}
	return driver.Forward{
		Protocol: protocol, Port: uint16(port), NetworkUID: uid, To: netip.AddrPortFrom(addr.Addr(), uint16(target)),
	}, true, nil
}

// reportReleased writes on a PortForward being deleted, whose port the host
// no longer forwards, that it has let go of it.
func (a *Agent) reportReleased(ctx context.Context, pf *networkv1alpha1.PortForward) error {
	if conditions.IsTrue(pf.GetStatus().GetConditions(), PortForwardReleased) {
		return nil
	}
	status := &networkv1alpha1.PortForwardStatus{}
	if pf.GetStatus() != nil {
		status = proto.Clone(pf.GetStatus()).(*networkv1alpha1.PortForwardStatus)
	}
	status.Conditions = conditions.Set(status.GetConditions(), &metav1.Condition{
		Type: PortForwardReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "PortClosed",
		Message: fmt.Sprintf("node %s no longer forwards port %d", a.cfg.Name, pf.GetStatus().GetPort()),
	}, a.cfg.Clock.Now())
	rv := pf.GetMetadata().GetResourceVersion()
	_, err := a.portForwards.UpdatePortForwardStatus(ctx, connect.NewRequest(&networkv1alpha1.UpdatePortForwardStatusRequest{
		Project: pf.GetMetadata().GetProject(), Name: pf.GetMetadata().GetName(), Status: status, ExpectedResourceVersion: &rv,
	}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil
	}
	return err
}
