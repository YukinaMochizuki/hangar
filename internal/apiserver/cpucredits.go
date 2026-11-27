package apiserver

import (
	"context"
	"slices"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	billingv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/billing/v1alpha1"
	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
)

// canonicalBaseline is the cpu_baseline a machine of resources.cpu_baseline
// b carries: zero unless below 100, so a machine of all its vCPUs is
// written as one that names no baseline (docs/decisions/cpu-credits.md).
func canonicalBaseline(b int32) int32 {
	if b >= 100 {
		return 0
	}
	return b
}

// launchGrantsPerDay is how many of a tenant's burstable machines are
// given launch credits in any launchGrantWindow
// (docs/decisions/cpu-credits.md).
const launchGrantsPerDay = 10

// launchGrantWindow is how long a launch grant counts against its tenant.
const launchGrantWindow = 24 * time.Hour

// balanceKeep is how long a burstable machine's balance outlives the last
// time its node accounted it, as the node's creditKeep: past it, the
// machine starts afresh, and takes a launch decision again.
const balanceKeep = 7 * 24 * time.Hour

// decideLaunch sets want's cpu_launch: whether the node gives the machine
// launch credits as it starts a balance afresh. m, the machine as read
// (nil if there is none), keeps the decision it carries, but as it is
// asked to run with its balance past balanceKeep since a decision before
// it. A machine burstable now and undecided is granted them while the
// tenant of project has had fewer than launchGrantsPerDay grants in
// launchGrantWindow, recorded in the tenant's status before the machine is
// written, under owner, the uid of the object it runs for: a decision
// made again for the same owner, as after the machine's write failed, is
// the grant recorded, never another. Decisions are made one at a time, so
// two never take the last grant.
func (s *Server) decideLaunch(ctx context.Context, project, owner string, m *computev1alpha1.Machine, want *computev1alpha1.MachineSpec) error {
	now := s.clock.Now()
	burstable := want.GetResources().GetCpuBaseline() != 0
	if prev := m.GetSpec().GetCpuLaunch(); prev != nil && (!burstable || !balanceExpired(m, want, prev, now)) {
		want.CpuLaunch = proto.Clone(prev).(*computev1alpha1.CPULaunchDecision)
		return nil
	}
	if !burstable {
		return nil
	}
	s.launchGrants.Lock()
	defer s.launchGrants.Unlock()
	obj, err := s.get(ctx, registry.Project, resource.Key{Name: project})
	if err != nil || obj == nil {
		return err
	}
	tenant := obj.(*tenancyv1alpha1.Project).GetSpec().GetTenant()
	for attempt := 1; ; attempt++ {
		granted, err := s.grantLaunch(ctx, tenant, owner)
		// The tenant's status is the usage controller's to write too: a
		// write it got to first is read again, not left to a retry.
		if connect.CodeOf(err) == connect.CodeAborted && attempt < launchGrantAttempts {
			continue
		}
		if err != nil {
			return err
		}
		want.CpuLaunch = &computev1alpha1.CPULaunchDecision{Granted: granted, Decided: timestamppb.New(now)}
		return nil
	}
}

// balanceExpired says machine m, asked to run as want says, starts its
// balance afresh: its node last accounted it more than balanceKeep before
// now, and after its launch decision prev, so one decision serves one
// balance.
func balanceExpired(m *computev1alpha1.Machine, want *computev1alpha1.MachineSpec, prev *computev1alpha1.CPULaunchDecision, now time.Time) bool {
	asOf := m.GetStatus().GetCpuCredits().GetAsOf()
	return want.GetRunState() == computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING && asOf != nil &&
		now.Sub(asOf.AsTime()) > balanceKeep && asOf.AsTime().After(prev.GetDecided().AsTime())
}

// launchGrantAttempts bounds the reads of a tenant whose status changes
// under a launch grant's write.
const launchGrantAttempts = 5

// grantLaunch reads tenant's grants and says whether owner's machine is
// given launch credits, recording a new grant in the tenant's status,
// conditional on the tenant as read. s.launchGrants is held.
func (s *Server) grantLaunch(ctx context.Context, tenant, owner string) (bool, error) {
	obj, err := s.get(ctx, registry.Tenant, resource.Key{Name: tenant})
	if err != nil || obj == nil {
		return false, err
	}
	t := obj.(*tenancyv1alpha1.Tenant)
	now := s.clock.Now()
	var grants []*tenancyv1alpha1.CPULaunchGrant
	for _, g := range t.GetStatus().GetCpuLaunchGrants() {
		if now.Sub(g.GetTime().AsTime()) < launchGrantWindow {
			grants = append(grants, g)
		}
	}
	if slices.ContainsFunc(grants, func(g *tenancyv1alpha1.CPULaunchGrant) bool { return g.GetOwnerUid() == owner }) {
		return true, nil
	}
	if len(grants) >= launchGrantsPerDay {
		return false, nil
	}
	status := proto.Clone(t.GetStatus()).(*tenancyv1alpha1.TenantStatus)
	if status == nil {
		status = &tenancyv1alpha1.TenantStatus{}
	}
	status.CpuLaunchGrants = append(grants, &tenancyv1alpha1.CPULaunchGrant{OwnerUid: owner, Time: timestamppb.New(now)})
	rev := t.GetMetadata().GetResourceVersion()
	if _, err := s.backend(registry.Tenant).UpdateStatus(s.system(ctx), resource.Key{Name: tenant}, status, &rev); err != nil {
		return false, err
	}
	return true, nil
}

// checkBaseline holds a Container's or VirtualMachine's
// spec.resources.cpu_baseline to its rules: unset, 20, 40 or 100; and below
// 100 only for a tenant whose BillingAccount is PAY_AS_YOU_GO. It is about
// the tenant, so no caller is exempt. was is the stored object's, nil on
// create; a baseline below 100 that the write leaves as it was is not
// checked against the tenant again, so a manifest applied again passes
// after the tenant has left PAY_AS_YOU_GO.
func (s *Server) checkBaseline(ctx context.Context, project string, written int32, was *int32) error {
	switch written {
	case 0, 100:
		return nil
	case 20, 40:
	default:
		return invalid("spec.resources.cpu_baseline %d is not 20, 40 or 100", written)
	}
	if was != nil && canonicalBaseline(*was) == written {
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
	account, err := s.get(ctx, registry.BillingAccount, resource.Key{Name: tenant})
	if err != nil {
		return err
	}
	a, _ := account.(*billingv1alpha1.BillingAccount)
	if a.GetSpec().GetType() != billingv1alpha1.BillingType_BILLING_TYPE_PAY_AS_YOU_GO {
		return failedPrecondition("spec.resources.cpu_baseline %d makes a burstable machine, which only a tenant whose billing account is PAY_AS_YOU_GO may have; tenant %q's is not",
			written, tenant)
	}
	return nil
}
