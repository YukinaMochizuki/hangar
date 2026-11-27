package apiserver

import (
	"context"
	"errors"
	"fmt"

	billingv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/billing/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// poolTerms are what decides whether a tenant's machines share its memory
// as a pool; see docs/decisions/memory-pool.md.
type poolTerms struct {
	plan   string
	limits limitSet
	// payg is whether the tenant's BillingAccount is PAY_AS_YOU_GO; a
	// tenant with none is not.
	payg bool
}

// pooled says whether the tenant's machines share its memory as a pool:
// its plan lets their memory add up past its memory limit, which it sets,
// and it does not pay as it goes.
func (t poolTerms) pooled() bool {
	return t.limits.memoryLimitPercent() > 100 && t.limits.set[dimMemory] && !t.payg
}

// poolTermsOf reads tenant's pool terms; ok is false for a tenant or a
// plan that does not exist, which has no pool.
func (s *Server) poolTermsOf(ctx context.Context, tenant string) (terms poolTerms, ok bool, err error) {
	obj, err := s.get(ctx, registry.Tenant, resource.Key{Name: tenant})
	if err != nil || obj == nil {
		return poolTerms{}, false, err
	}
	plan, limits, err := s.planOf(ctx, obj.(*tenancyv1alpha1.Tenant))
	if isNoPlan(err) {
		return poolTerms{}, false, nil
	}
	if err != nil {
		return poolTerms{}, false, err
	}
	account, err := s.get(ctx, registry.BillingAccount, resource.Key{Name: tenant})
	if err != nil {
		return poolTerms{}, false, err
	}
	a, _ := account.(*billingv1alpha1.BillingAccount)
	payg := a.GetSpec().GetType() == billingv1alpha1.BillingType_BILLING_TYPE_PAY_AS_YOU_GO
	return poolTerms{plan: plan, limits: limits, payg: payg}, true, nil
}

// reservation is a machine's reserved memory and its memory.
type reservation struct {
	reserved, memory uint64
}

// checkReserved holds a Container's or VirtualMachine's
// spec.resources.memory_reserved to its rules: written, a whole number of
// 256Mi from minimum, the kind's least memory, up to memory; and below
// memory only for a tenant whose machines share its memory as a pool
// (poolTerms.pooled). It is about the tenant, so no caller is exempt. was
// is the stored object's, nil on create; a reservation below memory that the
// write leaves as it was is not checked against the tenant again, so a
// manifest applied again passes after the tenant's plan has changed.
func (s *Server) checkReserved(ctx context.Context, project, written string, memory, minimum uint64, was *reservation) error {
	if written == "" {
		return nil
	}
	g, err := quantity.Bytes(written)
	if err != nil {
		return invalid("spec.resources.memory_reserved: %v", err)
	}
	switch {
	case g < shapeMemoryStep || g%shapeMemoryStep != 0:
		return invalid("spec.resources.memory_reserved %s is not a whole number of 256Mi steps, at least one", written)
	case g < minimum:
		return invalid("spec.resources.memory_reserved %s is below the minimum of %s", written, quantity.FormatMemory(minimum))
	case g > memory:
		return invalid("spec.resources.memory_reserved %s is above spec.resources.memory %s", written, quantity.FormatMemory(memory))
	case g == memory:
		return nil
	}
	if was != nil && *was == (reservation{g, memory}) {
		return nil
	}
	obj, err := s.get(ctx, registry.Project, resource.Key{Name: project})
	if err != nil {
		return err
	}
	if obj == nil {
		return failedPrecondition("project %q does not exist", project)
	}
	tenant := obj.(*tenancyv1alpha1.Project).GetSpec().GetTenant()
	terms, ok, err := s.poolTermsOf(ctx, tenant)
	if err != nil {
		return err
	}
	below := fmt.Sprintf("spec.resources.memory_reserved %s is below spec.resources.memory %s, which only a tenant whose machines share its memory as a pool may ask",
		written, quantity.FormatMemory(memory))
	switch {
	case !ok:
		return failedPrecondition("%s; tenant %q or its plan does not exist", below, tenant)
	case terms.payg:
		return failedPrecondition("%s; tenant %q's billing account is PAY_AS_YOU_GO, which has none", below, tenant)
	case !terms.pooled():
		return failedPrecondition("%s; tenant %q's plan %q has a memory_limit_percent of %d, and a pool needs one above 100. Ask a platform administrator",
			below, tenant, terms.plan, terms.limits.memoryLimitPercent())
	}
	return nil
}

// poolWatches wake the controller of kind r, which owns machines, for what
// decides its machines' memory and CPU pools: the tenant's Plan, its
// spec.plan and its BillingAccount.
func poolWatches(s *Server, r registry.Resource) []controller.Watch {
	mapper := s.ownersOfStalePools(r)
	return []controller.Watch{
		{Kind: registry.Plan.Kind, Map: mapper, SkipResync: true},
		{Kind: registry.Tenant.Kind, Map: mapper, SkipResync: true},
		{Kind: registry.BillingAccount.Kind, Map: mapper, SkipResync: true},
	}
}

// ownersOfStalePools maps a change of a Plan, a Tenant or a BillingAccount
// to the objects of kind r whose machine's memory_pool or cpu_pool it
// leaves out of date, so their controller brings them up to date. Only
// those are queued, so the tenant's status, written whenever its usage
// changes, wakes nothing.
func (s *Server) ownersOfStalePools(r registry.Resource) func(context.Context, store.Object) []controller.Key {
	return func(ctx context.Context, o store.Object) []controller.Key {
		tenants := []string{o.Name}
		if o.Kind == registry.Plan.Kind {
			tenants = nil
			for _, k := range s.tenantsOfPlan(ctx, o) {
				tenants = append(tenants, k.Name)
			}
		}
		var keys []controller.Key
		for _, tenant := range tenants {
			found, err := s.stalePoolOwners(ctx, r, tenant)
			if err != nil {
				s.log.Error("finding machines with a stale pool failed", "tenant", tenant, "error", err)
				continue
			}
			keys = append(keys, found...)
		}
		return keys
	}
}

// stalePoolOwners are tenant's objects of kind r whose machine does not
// carry the pools the tenant's machines carry now: none for a FileShare's
// server, which is in no pool (ensureMachineOf).
func (s *Server) stalePoolOwners(ctx context.Context, r registry.Resource, tenant string) ([]controller.Key, error) {
	var want machinePools
	if r.Kind != registry.FileShare.Kind {
		var err error
		if want, err = s.tenantPools(ctx, tenant); err != nil {
			return nil, err
		}
	}
	projects, err := s.listWith(ctx, registry.Project, "", nil)
	if err != nil {
		return nil, err
	}
	var keys []controller.Key
	for _, po := range projects {
		p := po.(*tenancyv1alpha1.Project)
		if p.GetSpec().GetTenant() != tenant {
			continue
		}
		objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind, Project: p.GetMetadata().GetName()})
		if err != nil {
			return nil, err
		}
		for _, mo := range objs {
			m, ok := decodeMachine(mo)
			if !ok {
				return nil, errors.New("decode machine " + mo.Name)
			}
			if m.GetMetadata().GetDeleteTime() != nil || want.carries(m.GetSpec()) {
				continue
			}
			for _, ref := range m.GetMetadata().GetOwnerReferences() {
				if ref.GetController() && ref.GetKind() == r.Kind {
					keys = append(keys, controller.Key{Project: mo.Project, Name: ref.GetName()})
				}
			}
		}
	}
	return keys, nil
}
