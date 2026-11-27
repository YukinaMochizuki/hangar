package apiserver

import (
	"context"
	"errors"
	"slices"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
)

// tenantCapabilities serves TenantCapabilityService.
type tenantCapabilities struct{ s *Server }

// CPUPoolUsage answers how often the tenant's CPU pool slowed its machines
// over the last hour, as the nodes its running pooled machines are on keep
// it, under the tenant's get (docs/decisions/cpu-pool.md).
func (h tenantCapabilities) CPUPoolUsage(ctx context.Context, req *connect.Request[tenancyv1alpha1.CPUPoolUsageRequest]) (*connect.Response[tenancyv1alpha1.CPUPoolUsageResponse], error) {
	s := h.s
	b := s.backend(registry.Tenant)
	p, err := b.begin(ctx)
	if err != nil {
		return nil, err
	}
	key := resource.Key{Name: req.Msg.GetTenant()}
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	if _, _, err := b.read(ctx, p, authz.Get, key); err != nil {
		return nil, err
	}
	res := &tenancyv1alpha1.CPUPoolUsageResponse{}
	pools, err := s.tenantPools(ctx, key.Name)
	if err != nil {
		return nil, apiError(err)
	}
	if pools.cpu == nil {
		return connect.NewResponse(res), nil
	}
	nodes, err := s.cpuPoolNodes(ctx, key.Name)
	if err != nil {
		return nil, apiError(err)
	}
	for _, node := range nodes {
		used, err := s.askCPUPool(ctx, p, key.Name, node)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The caller's calls are capped, which no other node lifts.
		var ce *connect.Error
		if errors.As(err, &ce) && ce.Code() == connect.CodeResourceExhausted {
			return nil, err
		}
		if err != nil {
			s.log.Debug("reading a CPU pool's usage from its node", "tenant", key.Name, "node", node, "error", err)
			res.NodesUnread++
			continue
		}
		if used.Slowed != nil && (res.Slowed == nil || used.GetSlowed() > res.GetSlowed()) {
			res.Slowed = used.Slowed
		}
		if used.MilliCpu != nil && (res.MilliCpu == nil || used.GetMilliCpu() > res.GetMilliCpu()) {
			res.MilliCpu = used.MilliCpu
		}
	}
	return connect.NewResponse(res), nil
}

// cpuPoolNodes names, sorted, the nodes on which tenant's CPU pool has a
// running machine.
func (s *Server) cpuPoolNodes(ctx context.Context, tenant string) ([]string, error) {
	projects, err := s.listWith(ctx, registry.Project, "", nil)
	if err != nil {
		return nil, err
	}
	var nodes []string
	for _, obj := range projects {
		project := obj.(*tenancyv1alpha1.Project)
		if project.GetSpec().GetTenant() != tenant {
			continue
		}
		machines, err := s.listWith(ctx, registry.Machine, project.GetMetadata().GetName(), nil)
		if err != nil {
			return nil, err
		}
		for _, obj := range machines {
			m := obj.(*computev1alpha1.Machine)
			if m.GetSpec().GetCpuPool().GetName() == tenant && m.GetSpec().GetNode() != "" &&
				m.GetStatus().GetPhase() == computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING {
				nodes = append(nodes, m.GetSpec().GetNode())
			}
		}
	}
	slices.Sort(nodes)
	return slices.Compact(nodes), nil
}

// askCPUPool asks node what it keeps of tenant's CPU pool, as p.
func (s *Server) askCPUPool(ctx context.Context, p auth.Principal, tenant, node string) (*nodev1.CPUPoolUsageResponse, error) {
	call, err := s.callNode(ctx, p, tenant, node, "tenant "+tenant+"'s CPU pool")
	if err != nil {
		return nil, err
	}
	defer call.release()
	used, err := call.nodes.CPUPoolUsage(ctx, connect.NewRequest(&nodev1.CPUPoolUsageRequest{Pool: tenant}))
	if err != nil {
		return nil, call.failed(err)
	}
	return used.Msg, nil
}
