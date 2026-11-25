package apiserver

import (
	"cmp"
	"context"
	"fmt"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	billingv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/billing/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/billing"
)

// BillingFinalizer keeps a BillingAccount being deleted until the billing
// controller has billed the period it had open.
const BillingFinalizer = registry.Domain + "/billing"

func billingLock(s *Server) *sync.Mutex { return &s.billing }

// readsBillingOf reports whether p may read a billing object of tenant as
// one of the tenant's people, by the grants at rev: whoever holds the
// kind's read at the tenant itself, not at one of its projects.
func readsBillingOf(tenant func(proto.Message) string) func(*Server, auth.Principal, string, proto.Message, int64) (bool, bool) {
	return func(s *Server, p auth.Principal, permission string, obj proto.Message, rev int64) (bool, bool) {
		t := tenant(obj)
		if t == "" {
			return false, true
		}
		return s.authz.AllowedAt(p, permission, authz.Target{Tenant: t}, rev)
	}
}

// hideListValue clears what a FIXED account's holdings would cost at list
// prices from whoever reads it but at the platform: its tenant's people
// pay the fee, whatever they hold (docs/decisions/billing.md).
func hideListValue(s *Server, p auth.Principal, obj proto.Message) {
	switch o := obj.(type) {
	case *billingv1alpha1.BillingAccount:
		if o.GetStatus().GetPeriodType() != billingv1alpha1.BillingType_BILLING_TYPE_FIXED || s.readsAllBilling(p, registry.BillingAccount) {
			return
		}
		o.Status.Metered, o.Status.UsageLines, o.Status.PeriodPriceList = "", nil, ""
	case *billingv1alpha1.Statement:
		if o.GetSpec().GetType() != billingv1alpha1.BillingType_BILLING_TYPE_FIXED || s.readsAllBilling(p, registry.Statement) {
			return
		}
		o.Spec.Metered = ""
	}
}

// readsAllBilling reports whether p reads the kind at the platform.
func (s *Server) readsAllBilling(p auth.Principal, r registry.Resource) bool {
	return s.authz.Allowed(p, authz.Permission(r, authz.Get), authz.Target{})
}

func accountTenant(obj proto.Message) string { return object.Meta(obj).GetName() }

func statementTenant(obj proto.Message) string {
	return obj.(*billingv1alpha1.Statement).GetSpec().GetTenant()
}

func billingAccountOwner(_ context.Context, _ *Server, obj proto.Message) (registry.Resource, resource.Key, bool) {
	return registry.Tenant, resource.Key{Name: object.Meta(obj).GetName()}, true
}

// validateBillingAccount checks an account's agreement, its tenant's plan's
// price if FIXED, its price list, and that a new one starts after its
// tenant's last Statement; under planLock and billingLock.
func validateBillingAccount(ctx context.Context, s *Server, _ auth.Principal, old, obj proto.Message) error {
	a := obj.(*billingv1alpha1.BillingAccount)
	spec := a.GetSpec()
	name := a.GetMetadata().GetName()
	tobj, err := s.get(ctx, registry.Tenant, resource.Key{Name: name})
	if err != nil {
		return err
	}
	if tobj == nil {
		return invalid("metadata.name: a billing account is named after its tenant, and there is no tenant %q", name)
	}
	if !billing.ValidCurrency(spec.GetCurrency()) {
		return invalid("spec.currency: %q is not an ISO 4217 code such as TWD", spec.GetCurrency())
	}
	switch spec.GetType() {
	case billingv1alpha1.BillingType_BILLING_TYPE_FIXED:
		if spec.GetPeriodMonths() > 1 {
			return invalid("spec.period_months: a FIXED account is billed every month; leave it 0 or 1")
		}
		if err := s.checkPlanPrice(ctx, tobj.(*tenancyv1alpha1.Tenant).GetSpec().GetPlan(), spec.GetCurrency()); err != nil {
			return invalid("spec.type: a FIXED account pays its tenant's plan's price: %v", err)
		}
	case billingv1alpha1.BillingType_BILLING_TYPE_PAY_AS_YOU_GO:
		if spec.GetPriceList() == "" {
			return invalid("spec.price_list: a PAY_AS_YOU_GO account is rated by a price list")
		}
		if m := spec.GetPeriodMonths(); m < 0 || m > billing.MaxPeriodMonths {
			return invalid("spec.period_months: %d is not from 1 to %d", m, billing.MaxPeriodMonths)
		}
	default:
		return invalid("spec.type: FIXED or PAY_AS_YOU_GO is required")
	}
	if spec.GetPeriodMonths() < 0 {
		return invalid("spec.period_months: %d is below zero", spec.GetPeriodMonths())
	}
	if pl := spec.GetPriceList(); pl != "" {
		obj, err := s.get(ctx, registry.PriceList, resource.Key{Name: pl})
		if err != nil {
			return err
		}
		if obj == nil {
			return invalid("spec.price_list: there is no price list %q", pl)
		}
		if c := obj.(*billingv1alpha1.PriceList).GetSpec().GetCurrency(); c != spec.GetCurrency() {
			return invalid("spec.price_list: price list %q is in %s, the account in %s", pl, c, spec.GetCurrency())
		}
	}
	if spec.GetAnchorTime() == nil {
		return invalid("spec.anchor_time: when the first period starts is required")
	}
	anchor := spec.GetAnchorTime().AsTime()
	if !anchor.Equal(anchor.Truncate(time.Minute)) {
		return invalid("spec.anchor_time: %s is not on a whole minute", anchor.Format(time.RFC3339Nano))
	}
	if _, err := billing.LoadZone(spec.GetTimeZone()); err != nil {
		return invalid("spec.time_zone: %v", err)
	}
	for i, p := range spec.GetPromotions() {
		if n := len([]rune(p.GetLabel())); n < 1 || n > 40 {
			return invalid("spec.promotions[%d].label: 1 to 40 characters, not %d", i, n)
		}
		if p.GetPercent() < 1 || p.GetPercent() > 100 {
			return invalid("spec.promotions[%d].percent: %d is not from 1 to 100", i, p.GetPercent())
		}
		if p.GetUntilTime() == nil {
			return invalid("spec.promotions[%d].until_time is required", i)
		}
		if at := p.GetUntilTime().AsTime(); !at.Equal(at.Truncate(time.Minute)) {
			return invalid("spec.promotions[%d].until_time: %s is not on a whole minute", i, at.Format(time.RFC3339Nano))
		}
	}
	if old == nil {
		billed, err := s.billedUntil(ctx, name)
		if err != nil {
			return err
		}
		if anchor.Before(billed) {
			return invalid("spec.anchor_time: tenant %q is billed until %s already; a new account starts then or later", name, billed.Format(time.RFC3339))
		}
	}
	return nil
}

// checkPlanPrice reports why the Plan named plan, the default for "", has
// no price a FIXED account in currency could pay.
func (s *Server) checkPlanPrice(ctx context.Context, plan, currency string) error {
	plan = cmp.Or(plan, DefaultPlan)
	obj, err := s.get(ctx, registry.Plan, resource.Key{Name: plan})
	if err != nil {
		return err
	}
	if obj == nil {
		return fmt.Errorf("there is no plan %q", plan)
	}
	price := obj.(*tenancyv1alpha1.Plan).GetSpec().GetPrice()
	switch {
	case price == nil:
		return fmt.Errorf("plan %q has no price", plan)
	case price.GetCurrency() != currency:
		return fmt.Errorf("plan %q is priced in %s, the account in %s", plan, price.GetCurrency(), currency)
	}
	return nil
}

// billedUntil is when the last Statement of tenant ends; zero for none.
func (s *Server) billedUntil(ctx context.Context, tenant string) (time.Time, error) {
	objs, err := s.listWith(ctx, registry.Statement, "", nil)
	if err != nil {
		return time.Time{}, err
	}
	var until time.Time
	for _, o := range objs {
		st := o.(*billingv1alpha1.Statement)
		if end := st.GetSpec().GetPeriodEndTime().AsTime(); st.GetSpec().GetTenant() == tenant && end.After(until) {
			until = end
		}
	}
	return until, nil
}

// fixedAccountOf is tenant's billing account when it is FIXED, nil else.
func (s *Server) fixedAccountOf(ctx context.Context, tenant string) (*billingv1alpha1.BillingAccount, error) {
	obj, err := s.get(ctx, registry.BillingAccount, resource.Key{Name: tenant})
	if err != nil || obj == nil {
		return nil, err
	}
	a := obj.(*billingv1alpha1.BillingAccount)
	if a.GetSpec().GetType() != billingv1alpha1.BillingType_BILLING_TYPE_FIXED || a.GetMetadata().GetDeleteTime() != nil {
		return nil, nil
	}
	return a, nil
}

// checkTenantPlanPriced refuses moving a tenant with a FIXED account to a
// Plan that has no price in the account's currency.
func (s *Server) checkTenantPlanPriced(ctx context.Context, tenant, plan string) error {
	a, err := s.fixedAccountOf(ctx, tenant)
	if err != nil || a == nil {
		return err
	}
	if err := s.checkPlanPrice(ctx, plan, a.GetSpec().GetCurrency()); err != nil {
		return invalid("spec.plan: tenant %q pays its plan's price, a FIXED billing account: %v", tenant, err)
	}
	return nil
}

// checkPlanStillPriced refuses taking a Plan's price away, or changing its
// currency, while a tenant with a FIXED account is on it.
func (s *Server) checkPlanStillPriced(ctx context.Context, plan *tenancyv1alpha1.Plan) error {
	name := plan.GetMetadata().GetName()
	tenants, err := s.listWith(ctx, registry.Tenant, "", nil)
	if err != nil {
		return err
	}
	for _, o := range tenants {
		t := o.(*tenancyv1alpha1.Tenant)
		if cmp.Or(t.GetSpec().GetPlan(), DefaultPlan) != name {
			continue
		}
		a, err := s.fixedAccountOf(ctx, t.GetMetadata().GetName())
		if err != nil {
			return err
		}
		if a == nil {
			continue
		}
		if price := plan.GetSpec().GetPrice(); price == nil || price.GetCurrency() != a.GetSpec().GetCurrency() {
			return invalid("spec.price: tenant %q pays this plan's price in %s, a FIXED billing account", t.GetMetadata().GetName(), a.GetSpec().GetCurrency())
		}
	}
	return nil
}

// validatePriceList checks the prices, and that an apply keeps every entry
// whose time has come as it was: a price in effect, or that was, never
// changes, so what was used is never rated again at another.
func validatePriceList(_ context.Context, s *Server, _ auth.Principal, old, obj proto.Message) error {
	pl := obj.(*billingv1alpha1.PriceList)
	if !billing.ValidCurrency(pl.GetSpec().GetCurrency()) {
		return invalid("spec.currency: %q is not an ISO 4217 code such as TWD", pl.GetSpec().GetCurrency())
	}
	if _, err := billing.ParseEntries(pl.GetSpec().GetEntries()); err != nil {
		return invalid("spec.%v", err)
	}
	prev, _ := old.(*billingv1alpha1.PriceList)
	if prev == nil {
		return nil
	}
	now := s.clock.Now()
	entries := pl.GetSpec().GetEntries()
	kept := 0
	for i, e := range prev.GetSpec().GetEntries() {
		if e.GetEffectiveTime().AsTime().After(now) {
			break
		}
		if i >= len(entries) || !proto.Equal(e, entries[i]) {
			return invalid("spec.entries[%d]: the entry in effect from %s has taken effect, and cannot be changed or removed; add an entry instead",
				i, e.GetEffectiveTime().AsTime().Format(time.RFC3339))
		}
		kept = i + 1
	}
	for i := kept; i < len(entries); i++ {
		// The minute it is now is metered already, at the price it began with.
		if at := entries[i].GetEffectiveTime().AsTime(); !at.After(now.Truncate(time.Minute)) {
			return invalid("spec.entries[%d]: a new entry takes effect from the next minute on, not at %s", i, at.Format(time.RFC3339))
		}
	}
	return nil
}

// refuseDeletingPriceListInUse keeps a price list a billing account names.
func refuseDeletingPriceListInUse(ctx context.Context, s *Server, obj proto.Message) error {
	name := object.Meta(obj).GetName()
	accounts, err := s.listWith(ctx, registry.BillingAccount, "", nil)
	if err != nil {
		return err
	}
	for _, o := range accounts {
		a := o.(*billingv1alpha1.BillingAccount)
		if a.GetSpec().GetPriceList() == name || a.GetStatus().GetPeriodPriceList() == name {
			return failedPrecondition("price list %q rates billing account %q; give it another price list first", name, a.GetMetadata().GetName())
		}
	}
	return nil
}

// validateStatement checks what the billing controller writes.
func validateStatement(_ context.Context, _ *Server, _ auth.Principal, _, obj proto.Message) error {
	st := obj.(*billingv1alpha1.Statement)
	if st.GetSpec().GetTenant() == "" {
		return invalid("spec.tenant is required")
	}
	if st.GetSpec().GetPeriodStartTime() == nil || st.GetSpec().GetPeriodEndTime() == nil {
		return invalid("spec.period_start_time and spec.period_end_time are required")
	}
	return nil
}

// statementPayments serves StatementPaymentService.
type statementPayments struct{ s *Server }

func (h statementPayments) MarkStatementPaid(ctx context.Context, req *connect.Request[billingv1alpha1.MarkStatementPaidRequest]) (*connect.Response[billingv1alpha1.MarkStatementPaidResponse], error) {
	s := h.s
	p, err := s.begin(ctx, false)
	if err != nil {
		return nil, err
	}
	if !s.authz.Allowed(p, authz.StatementPay, authz.Target{}) {
		return nil, denied("%s may not record a statement paid: it takes %s at the platform", p, authz.StatementPay)
	}
	m := req.Msg
	if m.GetName() == "" {
		return nil, invalid("name is required")
	}
	key := resource.Key{Name: m.GetName()}
	for range maxRetries {
		obj, err := s.get(ctx, registry.Statement, key)
		if err != nil {
			return nil, apiError(err)
		}
		if obj == nil {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("statement %q not found", m.GetName()))
		}
		st := obj.(*billingv1alpha1.Statement)
		status := proto.Clone(st.GetStatus()).(*billingv1alpha1.StatementStatus)
		if status == nil {
			status = &billingv1alpha1.StatementStatus{}
		}
		switch {
		case m.GetUnpaid():
			status.PaidTime, status.PaidRecordedBy = nil, ""
		case status.GetPaidTime() != nil && m.GetPaidTime() == nil:
			// Already paid, and no other time asked for.
		default:
			at := m.GetPaidTime()
			if at == nil {
				at = timestamppb.New(s.clock.Now())
			}
			status.PaidTime, status.PaidRecordedBy = at, p.String()
		}
		if proto.Equal(status, st.GetStatus()) {
			return connect.NewResponse(&billingv1alpha1.MarkStatementPaidResponse{Statement: st}), nil
		}
		rv := st.GetMetadata().GetResourceVersion()
		out, err := s.backend(registry.Statement).UpdateStatus(s.system(ctx), key, status, &rv)
		if connect.CodeOf(err) == connect.CodeAborted {
			continue
		}
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&billingv1alpha1.MarkStatementPaidResponse{Statement: out.(*billingv1alpha1.Statement)}), nil
	}
	return nil, connect.NewError(connect.CodeAborted, fmt.Errorf("statement %q kept changing", m.GetName()))
}
