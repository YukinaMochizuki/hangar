package node

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/edgedial"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// serveEdgeDial serves hangar-edge's dials on the listener the config
// names, until ctx is done.
func (a *Agent) serveEdgeDial(ctx context.Context) {
	srv := &edgedial.Server{UIDs: a.cfg.EdgeUIDs, Open: a.openEndpoint, Log: a.cfg.Log.With("component", "edge dial")}
	if err := srv.Serve(ctx, a.cfg.EdgeDial); err != nil {
		a.cfg.Log.Error("serving the edge's dials stopped", "error", err)
	}
}

// openEndpoint connects to the port of a public endpoint whose machine runs
// on this node, from its first interface's network's gateway. It reaches
// nothing an endpoint does not name, and only while the endpoint exists:
// the edge names the endpoint, never an address or a port, and whether the
// caller may use it is the edge's to decide.
func (a *Agent) openEndpoint(ctx context.Context, t edgedial.Target) (net.Conn, error) {
	if a.cfg.Drivers == nil {
		return nil, edgedial.Refuse(http.StatusServiceUnavailable, "this node runs no machines")
	}
	obj, err := a.api.Get(ctx, registry.PublicEndpoint, client.Key{Project: t.Project, Name: t.Name})
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, edgedial.Refuse(http.StatusNotFound, "public endpoint %s does not exist", t)
	}
	if err != nil {
		return nil, edgedial.Refuse(http.StatusServiceUnavailable, "reading public endpoint %s: %v", t, err)
	}
	ep := obj.(*networkv1alpha1.PublicEndpoint)
	if ep.GetMetadata().GetUid() != t.UID {
		return nil, edgedial.Refuse(http.StatusNotFound, "public endpoint %s with uid %s does not exist", t, t.UID)
	}
	if ep.GetMetadata().GetDeleteTime() != nil {
		return nil, edgedial.Refuse(http.StatusNotFound, "public endpoint %s is being deleted", t)
	}
	machine := ep.GetStatus().GetMachine()
	if machine == "" {
		return nil, edgedial.Refuse(http.StatusConflict, "public endpoint %s reaches no machine yet", t)
	}
	mobj, err := a.api.Get(ctx, registry.Machine, client.Key{Project: t.Project, Name: machine})
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, edgedial.Refuse(http.StatusConflict, "public endpoint %s's machine %s does not exist", t, machine)
	}
	if err != nil {
		return nil, edgedial.Refuse(http.StatusServiceUnavailable, "reading machine %s: %v", machine, err)
	}
	m := mobj.(*computev1alpha1.Machine)
	if node := m.GetSpec().GetNode(); node != a.cfg.Name {
		return nil, edgedial.Refuse(http.StatusConflict, "public endpoint %s's machine runs on node %q, not here", t, node)
	}
	uid := m.GetMetadata().GetUid()
	vm, err := a.vmOf(ctx, uid)
	if err != nil {
		return nil, edgedial.Refuse(http.StatusServiceUnavailable, "%v", err)
	}
	if vm == nil || vm.State != driver.VMRunning {
		return nil, edgedial.Refuse(http.StatusConflict, "public endpoint %s's machine is not running", t)
	}
	nic, err := a.eth0(ctx, uid)
	if err != nil {
		return nil, edgedial.Refuse(http.StatusConflict, "%s", errMessage(err))
	}
	addr := netip.AddrPortFrom(nic.Address.Addr(), uint16(ep.GetSpec().GetPort()))
	conn, err := a.cfg.Drivers.Network.Dial(ctx, nic.NetworkUID, addr)
	if err != nil {
		return nil, edgedial.Refuse(http.StatusBadGateway, "public endpoint %s's port %d: %v", t, addr.Port(), err)
	}
	return conn, nil
}

// errMessage is a Connect error's message without its code.
func errMessage(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	return err.Error()
}

// edgePortsOf are the ports, by machine name, that the live public
// endpoints of a project reach on their machines' first interfaces.
func (a *Agent) edgePortsOf(ctx context.Context, project string) (map[string][]uint16, error) {
	objs, _, err := a.api.ListAll(ctx, registry.PublicEndpoint, client.ListOptions{Project: project})
	if err != nil {
		return nil, err
	}
	out := map[string][]uint16{}
	for _, o := range objs {
		ep := o.(*networkv1alpha1.PublicEndpoint)
		m := ep.GetStatus().GetMachine()
		if m == "" || ep.GetMetadata().GetDeleteTime() != nil {
			continue
		}
		out[m] = append(out[m], uint16(ep.GetSpec().GetPort()))
	}
	return out, nil
}

// networksOfEndpoint maps a public endpoint to every network of its
// project: the machine it reaches, and the one it reached before, may be on
// any of them.
func (a *Agent) networksOfEndpoint(ctx context.Context, project string) []string {
	objs, _, err := a.api.ListAll(ctx, registry.Network, client.ListOptions{Project: project})
	if err != nil {
		a.cfg.Log.Error("listing networks failed", "project", project, "error", err)
		return nil
	}
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.(*networkv1alpha1.Network).GetMetadata().GetName())
	}
	return out
}
