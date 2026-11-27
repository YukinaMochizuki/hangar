package apiserver

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
)

// tenantCapabilities serves TenantCapabilityService.
type tenantCapabilities struct{ s *Server }

// cpuPoolMinutes is how many minutes of the pool's slowed share are
// answered, as many as a node keeps: an hour.
const cpuPoolMinutes = 60

// CPUPoolUsage answers how often the tenant's CPU pool slowed its machines
// over the last hour, minute by minute, and what each of its running
// machines uses now, as the nodes they are on keep it, to a caller who may
// get Containers or VirtualMachines in some project of the tenant, as
// MemoryPoolUsage is; the machines it may not get are summed into other,
// unnamed, and what is said of nodes' samples is of those its named
// machines are on (docs/decisions/cpu-pool.md).
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
	if !s.readsPoolMachines(p, key.Name) {
		return nil, denied("%s may not get containers or virtual machines in any project of tenant %q", p, key.Name)
	}
	// Only a platform-wide caller gets this far for a tenant that does not
	// exist, and is told so.
	if _, err := s.store.Get(ctx, b.storeKey(key)); err != nil {
		return nil, apiError(err)
	}
	res := &tenancyv1alpha1.CPUPoolUsageResponse{}
	pools, err := s.tenantPools(ctx, key.Name)
	if err != nil {
		return nil, apiError(err)
	}
	if pools.cpu == nil {
		return connect.NewResponse(res), nil
	}
	res.Pooled = true
	machines, err := s.cpuPoolMachines(ctx, key.Name)
	if err != nil {
		return nil, apiError(err)
	}
	now := map[string]int64{}
	// sampled is when each node read last sampled the pool's machines; a
	// node not read, or with no sample lately, is not in it.
	sampled := map[string]time.Time{}
	minutes := map[time.Time]float64{}
	for _, node := range nodesOf(machines) {
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
			continue
		}
		// The pool is held on each node apart: its share and CPUs are the
		// largest of them, minute by minute too.
		if used.Slowed != nil && (res.Slowed == nil || used.GetSlowed() > res.GetSlowed()) {
			res.Slowed = used.Slowed
		}
		if used.MilliCpu != nil && (res.MilliCpu == nil || used.GetMilliCpu() > res.GetMilliCpu()) {
			res.MilliCpu = used.MilliCpu
		}
		for _, m := range used.GetMinutes() {
			at := m.GetTime().AsTime()
			if v, ok := minutes[at]; !ok || m.GetSlowed() > v {
				minutes[at] = m.GetSlowed()
			}
		}
		if t := used.GetSampleTime(); t != nil {
			sampled[node] = t.AsTime()
		}
		for _, m := range used.GetMembers() {
			if m.MilliCpu != nil {
				now[m.GetMachineUid()] = m.GetMilliCpu()
			}
		}
	}
	for _, at := range lastOf(slices.SortedFunc(maps.Keys(minutes), time.Time.Compare), cpuPoolMinutes) {
		res.Minutes = append(res.Minutes, &tenancyv1alpha1.CPUPoolMinute{Time: timestamppb.New(at), Slowed: minutes[at]})
	}
	var named []poolMachine
	for _, m := range machines {
		milli, known := now[m.uid]
		if !m.readable(s, p) {
			if res.Other == nil {
				res.Other = &tenancyv1alpha1.CPUPoolTotal{}
			}
			res.Other.CpuNowMilli += milli
			continue
		}
		named = append(named, m)
		pm := &tenancyv1alpha1.CPUPoolMachine{Project: m.project, Kind: m.kind, Name: m.name, Phase: m.phase, Vcpus: m.vcpus}
		if known {
			pm.CpuNowMilli = proto.Int64(milli)
		}
		res.Machines = append(res.Machines, pm)
	}
	// Of the nodes the named machines are on alone: the others' are no more
	// the caller's to know than their machines.
	var oldest time.Time
	for _, node := range nodesOf(named) {
		at, ok := sampled[node]
		switch {
		case !ok:
			res.NodesUnread++
		case oldest.IsZero() || at.Before(oldest):
			oldest = at
		}
	}
	if !oldest.IsZero() {
		res.SampleTime = timestamppb.New(oldest)
	}
	slices.SortStableFunc(res.Machines, byCPUNow)
	return connect.NewResponse(res), nil
}

// lastOf is the last n of xs, all of them when there are fewer.
func lastOf[T any](xs []T, n int) []T {
	return xs[max(len(xs)-n, 0):]
}

// byCPUNow orders a CPU pool's machines as the console draws them: by
// what they use now, largest first, those unknown last, then by project
// and name.
func byCPUNow(a, b *tenancyv1alpha1.CPUPoolMachine) int {
	nowOr := func(m *tenancyv1alpha1.CPUPoolMachine) int64 {
		if m.CpuNowMilli == nil {
			return -1
		}
		return m.GetCpuNowMilli()
	}
	return cmp.Or(cmp.Compare(nowOr(b), nowOr(a)), cmp.Compare(a.GetProject(), b.GetProject()), cmp.Compare(a.GetName(), b.GetName()))
}

// cpuPoolMachines are the running machines in tenant's CPU pool placed on
// a node, in project and name order.
func (s *Server) cpuPoolMachines(ctx context.Context, tenant string) ([]poolMachine, error) {
	projects, err := s.listWith(ctx, registry.Project, "", nil)
	if err != nil {
		return nil, err
	}
	var out []poolMachine
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
			if m.GetSpec().GetCpuPool().GetName() != tenant || m.GetSpec().GetNode() == "" ||
				m.GetStatus().GetPhase() != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING {
				continue
			}
			vcpus, err := quantity.CPU(m.GetSpec().GetResources().GetCpu())
			if err != nil {
				// One machine the caller may not even read fails nobody's
				// answer, as in memoryPoolMachines.
				s.log.Warn("leaving a machine out of its CPU pool's usage", "tenant", tenant, "project", m.GetMetadata().GetProject(), "machine", m.GetMetadata().GetName(), "error", err)
				continue
			}
			owner := ownerOfMachine(m)
			out = append(out, poolMachine{
				uid: m.GetMetadata().GetUid(), node: m.GetSpec().GetNode(),
				project: owner.project, kind: owner.kind, name: owner.name,
				phase: m.GetStatus().GetPhase(), vcpus: vcpus,
			})
		}
	}
	return out, nil
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
