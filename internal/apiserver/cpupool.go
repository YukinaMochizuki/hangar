package apiserver

import (
	"context"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
)

// cpuPooled says whether the tenant's machines share its CPUs as a pool
// (docs/decisions/cpu-pool.md): its plan lets their vCPUs add up past its
// cpu limit, which it sets, and it does not pay as it goes.
func (t poolTerms) cpuPooled() bool {
	return t.limits.cpuLimitPercent() > 100 && t.limits.set[dimCPU] && !t.payg
}

// cpuPercent is how far the tenant's vCPUs may add up past a cpu limit:
// its plan's cpu_limit_percent while it has a CPU pool, 100 otherwise.
func (t poolTerms) cpuPercent() int64 {
	if !t.cpuPooled() {
		return 100
	}
	return t.limits.cpuLimitPercent()
}

// machinePools are the pools a tenant's machines carry, each nil when
// they share none.
type machinePools struct {
	memory *computev1alpha1.MemoryPool
	cpu    *computev1alpha1.CPUPool
}

// pools are the pools the machines of tenant, whose terms these are,
// carry.
func (t poolTerms) pools(tenant string) machinePools {
	var p machinePools
	if t.pooled() {
		p.memory = &computev1alpha1.MemoryPool{Name: tenant, Size: quantity.FormatMemory(uint64(t.limits.val[dimMemory]))}
	}
	if t.cpuPooled() {
		p.cpu = &computev1alpha1.CPUPool{Name: tenant, Cpu: formatAmount(dimCPU, t.limits.val[dimCPU])}
	}
	return p
}

// carries says whether spec carries these pools.
func (p machinePools) carries(spec *computev1alpha1.MachineSpec) bool {
	return proto.Equal(spec.GetMemoryPool(), p.memory) && proto.Equal(spec.GetCpuPool(), p.cpu)
}

// tenantPools are the pools tenant's machines carry, none for a tenant or
// a plan that does not exist.
func (s *Server) tenantPools(ctx context.Context, tenant string) (machinePools, error) {
	terms, ok, err := s.poolTermsOf(ctx, tenant)
	if err != nil || !ok {
		return machinePools{}, err
	}
	return terms.pools(tenant), nil
}

// poolsOf are the pools the machines of project carry, none in a project
// that does not exist.
func (s *Server) poolsOf(ctx context.Context, project string) (machinePools, error) {
	obj, err := s.get(ctx, registry.Project, resource.Key{Name: project})
	if err != nil || obj == nil {
		return machinePools{}, err
	}
	return s.tenantPools(ctx, obj.(*tenancyv1alpha1.Project).GetSpec().GetTenant())
}
