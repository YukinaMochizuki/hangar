package apiserver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
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
	"github.com/YukinaMochizuki/hangar/internal/authz"
)

// poolKinds are the kinds whose machines a memory pool's usage names, to
// a caller who may get them.
var poolKinds = map[string]registry.Resource{
	registry.Container.Kind:      registry.Container,
	registry.VirtualMachine.Kind: registry.VirtualMachine,
}

// MemoryPoolUsage answers who holds the tenant's memory pool now, as the
// nodes its running machines are on last read it, to a caller who may get
// Containers or VirtualMachines in some project of the tenant; the
// machines it may not get are summed into other, unnamed
// (docs/decisions/memory-pool.md).
func (h tenantCapabilities) MemoryPoolUsage(ctx context.Context, req *connect.Request[tenancyv1alpha1.MemoryPoolUsageRequest]) (*connect.Response[tenancyv1alpha1.MemoryPoolUsageResponse], error) {
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
	res := &tenancyv1alpha1.MemoryPoolUsageResponse{}
	pools, err := s.tenantPools(ctx, key.Name)
	if err != nil {
		return nil, apiError(err)
	}
	if pools.memory == nil {
		return connect.NewResponse(res), nil
	}
	size, err := quantity.Bytes(pools.memory.GetSize())
	if err == nil && size > math.MaxInt64 {
		err = errors.New("past an int64")
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("tenant %s's memory pool's size %q: %w", key.Name, pools.memory.GetSize(), err))
	}
	res.SizeBytes = proto.Int64(int64(size))
	machines, err := s.memoryPoolMachines(ctx, key.Name)
	if err != nil {
		return nil, apiError(err)
	}
	read := map[string]*nodev1.MemoryPoolMember{}
	var oldest time.Time
	for _, node := range nodesOf(machines) {
		used, err := s.askMemoryPool(ctx, p, key.Name, node)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The caller's calls are capped, which no other node lifts.
		var ce *connect.Error
		if errors.As(err, &ce) && ce.Code() == connect.CodeResourceExhausted {
			return nil, err
		}
		if err != nil {
			s.log.Debug("reading a memory pool's usage from its node", "tenant", key.Name, "node", node, "error", err)
			res.NodesUnread++
			continue
		}
		if t := used.GetSampleTime(); t != nil && (oldest.IsZero() || t.AsTime().Before(oldest)) {
			oldest = t.AsTime()
		}
		for _, m := range used.GetMembers() {
			read[m.GetMachineUid()] = m
		}
	}
	if !oldest.IsZero() {
		res.SampleTime = timestamppb.New(oldest)
	}
	for _, m := range machines {
		pm := m.usage(read[m.uid])
		if !m.readable(s, p) {
			if res.Other == nil {
				res.Other = &tenancyv1alpha1.MemoryPoolTotal{}
			}
			res.Other.MemoryBytes += pm.GetMemoryBytes()
			res.Other.ReservedBytes += pm.GetReservedBytes()
			res.Other.CurrentBytes += currentOr(pm)
			res.Other.InUseBytes += inUseOr(pm)
			continue
		}
		res.Machines = append(res.Machines, pm)
	}
	slices.SortStableFunc(res.Machines, byPoolOrder)
	return connect.NewResponse(res), nil
}

// readsPoolMachines says whether p may get Containers or VirtualMachines
// in some project of tenant, or at the tenant, which covers them all.
func (s *Server) readsPoolMachines(p auth.Principal, tenant string) bool {
	for _, r := range poolKinds {
		if s.authz.AllowedInProjectsOf(p, authz.Permission(r, authz.Get), tenant) {
			return true
		}
	}
	return false
}

// poolMachine is a running machine in a memory pool, as its Machine says.
type poolMachine struct {
	uid, node, project, kind, name string
	phase                          computev1alpha1.MachinePhase
	memory, reserved               int64
}

// readable says whether p may get the object the machine runs; a machine
// run by no such object, or another kind, is never named.
func (m poolMachine) readable(s *Server, p auth.Principal) bool {
	r, ok := poolKinds[m.kind]
	return ok && s.backend(r).allowed(p, authz.Get, authz.Target{Project: m.project})
}

// usage is the machine as its node read it, nil for not read.
func (m poolMachine) usage(read *nodev1.MemoryPoolMember) *tenancyv1alpha1.MemoryPoolMachine {
	out := &tenancyv1alpha1.MemoryPoolMachine{
		Project: m.project, Kind: m.kind, Name: m.name, Phase: m.phase,
		MemoryBytes: m.memory, ReservedBytes: m.reserved,
		LastOomKillTime: read.GetLastOomKillTime(), BalloonUnresponsiveSince: read.GetBalloonUnresponsiveSince(),
	}
	if read != nil && read.SizeBytes != nil {
		out.CurrentBytes = proto.Int64(int64(min(read.GetSizeBytes(), uint64(m.memory))))
	}
	if read != nil && read.InUseBytes != nil {
		out.InUseBytes = proto.Int64(int64(min(read.GetInUseBytes(), uint64(m.memory))))
	}
	return out
}

// byPoolOrder orders a pool's machines as the console draws them:
// Dedicated first, then by what they use, largest first (inUseOr), then
// by project and name.
func byPoolOrder(a, b *tenancyv1alpha1.MemoryPoolMachine) int {
	dedicated := func(m *tenancyv1alpha1.MemoryPoolMachine) bool { return m.GetReservedBytes() >= m.GetMemoryBytes() }
	if da, db := dedicated(a), dedicated(b); da != db {
		if da {
			return -1
		}
		return 1
	}
	return cmp.Or(cmp.Compare(inUseOr(b), inUseOr(a)), cmp.Compare(a.GetProject(), b.GetProject()), cmp.Compare(a.GetName(), b.GetName()))
}

// currentOr is what the machine's guest has now, its reservation when its
// node could not tell.
func currentOr(m *tenancyv1alpha1.MemoryPoolMachine) int64 {
	if m.CurrentBytes != nil {
		return m.GetCurrentBytes()
	}
	return m.GetReservedBytes()
}

// inUseOr is what the machine's guest uses, what it has when the guest
// does not say, and its reservation when its node cannot tell either.
func inUseOr(m *tenancyv1alpha1.MemoryPoolMachine) int64 {
	if m.InUseBytes != nil {
		return m.GetInUseBytes()
	}
	return currentOr(m)
}

// memoryPoolMachines are the running machines in tenant's memory pool
// placed on a node, in project and name order.
func (s *Server) memoryPoolMachines(ctx context.Context, tenant string) ([]poolMachine, error) {
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
			if m.GetSpec().GetMemoryPool().GetName() != tenant || m.GetSpec().GetNode() == "" ||
				m.GetStatus().GetPhase() != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING {
				continue
			}
			memory, err := quantity.Bytes(m.GetSpec().GetResources().GetMemory())
			if err == nil && memory > math.MaxInt64 {
				err = errors.New("past an int64")
			}
			if err != nil {
				return nil, fmt.Errorf("machine %s/%s's memory %q: %w", m.GetMetadata().GetProject(), m.GetMetadata().GetName(), m.GetSpec().GetResources().GetMemory(), err)
			}
			reserved := memory
			if g := m.GetSpec().GetResources().GetMemoryReserved(); g != "" {
				r, err := quantity.Bytes(g)
				if err != nil {
					return nil, fmt.Errorf("machine %s/%s's memory_reserved %q: %w", m.GetMetadata().GetProject(), m.GetMetadata().GetName(), g, err)
				}
				reserved = min(r, memory)
			}
			owner := ownerOfMachine(m)
			out = append(out, poolMachine{
				uid: m.GetMetadata().GetUid(), node: m.GetSpec().GetNode(),
				project: owner.project, kind: owner.kind, name: owner.name,
				phase: m.GetStatus().GetPhase(), memory: int64(memory), reserved: int64(reserved),
			})
		}
	}
	return out, nil
}

// nodesOf names, sorted, the nodes the machines are on.
func nodesOf(machines []poolMachine) []string {
	var nodes []string
	for _, m := range machines {
		nodes = append(nodes, m.node)
	}
	slices.Sort(nodes)
	return slices.Compact(nodes)
}

// askMemoryPool asks node what it last read of tenant's memory pool, as p.
func (s *Server) askMemoryPool(ctx context.Context, p auth.Principal, tenant, node string) (*nodev1.MemoryPoolUsageResponse, error) {
	call, err := s.callNode(ctx, p, tenant, node, "tenant "+tenant+"'s memory pool")
	if err != nil {
		return nil, err
	}
	defer call.release()
	used, err := call.nodes.MemoryPoolUsage(ctx, connect.NewRequest(&nodev1.MemoryPoolUsageRequest{Pool: tenant}))
	if err != nil {
		return nil, call.failed(err)
	}
	return used.Msg, nil
}
