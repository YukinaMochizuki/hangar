package apiserver

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	billingv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/billing/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/billing"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// billingResync is how often every account is reconciled, so a period is
// billed within a minute of its end.
const billingResync = time.Minute

// accrualEvery is how often an account's status says again what its open
// period has come to, so a quiet account is written every few minutes, not
// every minute.
const accrualEvery = 5 * time.Minute

func (s *Server) billingController() *controller.Controller {
	return &controller.Controller{
		Name: "billing", Source: controller.StoreSource{Store: s.store}, Clock: s.clock, Log: s.log,
		Reconciler: controller.ReconcilerFunc(s.reconcileBilling),
		Resync:     billingResync,
		Watches: []controller.Watch{
			{Kind: registry.BillingAccount.Kind, Map: controller.Self},
			// A tenant's Plan changing ends a FIXED account's period; its
			// account has its name.
			{Kind: registry.Tenant.Kind, Map: controller.Self, SkipResync: true},
			// A new price changes what the open period has come to.
			{Kind: registry.PriceList.Kind, Map: s.accountsOfPriceList, SkipResync: true},
			// A plan's price is a FIXED account's fee from its next period,
			// and from its first while that has not started.
			{Kind: registry.Plan.Kind, Map: s.accountsOnPlan, SkipResync: true},
		},
	}
}

func (s *Server) accountsOfPriceList(ctx context.Context, o store.Object) []controller.Key {
	accounts, err := s.listWith(ctx, registry.BillingAccount, "", nil)
	if err != nil {
		return nil
	}
	var out []controller.Key
	for _, obj := range accounts {
		a := obj.(*billingv1alpha1.BillingAccount)
		if a.GetSpec().GetPriceList() == o.Name || a.GetStatus().GetPeriodPriceList() == o.Name {
			out = append(out, controller.Key{Name: a.GetMetadata().GetName()})
		}
	}
	return out
}

// accountsOnPlan are the accounts of the tenants on the plan named.
func (s *Server) accountsOnPlan(ctx context.Context, o store.Object) []controller.Key {
	tenants, err := s.listWith(ctx, registry.Tenant, "", nil)
	if err != nil {
		return nil
	}
	var out []controller.Key
	for _, obj := range tenants {
		t := obj.(*tenancyv1alpha1.Tenant)
		if cmp.Or(t.GetSpec().GetPlan(), DefaultPlan) == o.Name {
			out = append(out, controller.Key{Name: t.GetMetadata().GetName()})
		}
	}
	return out
}

// priceListVersion is the resource version of the price list named, 0 for
// none.
func (s *Server) priceListVersion(ctx context.Context, name string) (int64, error) {
	if name == "" {
		return 0, nil
	}
	obj, err := s.get(ctx, registry.PriceList, resource.Key{Name: name})
	if err != nil || obj == nil {
		return 0, err
	}
	return object.Meta(obj).GetResourceVersion(), nil
}

// terms are what an account's period is billed by.
type terms struct {
	typ billingv1alpha1.BillingType
	// plan and fee are a FIXED account's Plan and the fee it had as the
	// period started.
	plan, fee string
	priceList string
	months    int
}

func statusTerms(st *billingv1alpha1.BillingAccountStatus) terms {
	return terms{typ: st.GetPeriodType(), plan: st.GetPeriodPlan(), fee: st.GetPeriodMonthlyFee(), priceList: st.GetPeriodPriceList(), months: int(st.GetPeriodMonths())}
}

func (t terms) into(st *billingv1alpha1.BillingAccountStatus) {
	st.PeriodType, st.PeriodPlan, st.PeriodMonthlyFee = t.typ, t.plan, t.fee
	st.PeriodPriceList, st.PeriodMonths = t.priceList, int32(t.months)
}

// cuts says whether going from have to want ends the open period at once:
// a change of type, or of a FIXED account's Plan. Anything else waits for
// the next period (docs/decisions/billing.md).
func cuts(have, want terms) bool {
	return have.typ != want.typ || (want.typ == billingv1alpha1.BillingType_BILLING_TYPE_FIXED && have.plan != want.plan)
}

// wantedTerms is what a's spec and, for a FIXED account, its tenant's Plan
// say now.
func (s *Server) wantedTerms(ctx context.Context, a *billingv1alpha1.BillingAccount) (terms, error) {
	spec := a.GetSpec()
	t := terms{typ: spec.GetType(), priceList: spec.GetPriceList(), months: int(max(spec.GetPeriodMonths(), 1))}
	if t.typ != billingv1alpha1.BillingType_BILLING_TYPE_FIXED {
		return t, nil
	}
	t.months = 1
	tenant, err := s.tenant(ctx, a.GetMetadata().GetName())
	if err != nil {
		return t, err
	}
	t.plan = cmp.Or(tenant.GetSpec().GetPlan(), DefaultPlan)
	obj, err := s.get(ctx, registry.Plan, resource.Key{Name: t.plan})
	if err != nil {
		return t, err
	}
	var price *tenancyv1alpha1.PlanPrice
	if obj != nil {
		price = obj.(*tenancyv1alpha1.Plan).GetSpec().GetPrice()
	}
	if price == nil {
		return t, fmt.Errorf("%w: plan %q of FIXED billing account %q has no price", controller.ErrWaiting, t.plan, a.GetMetadata().GetName())
	}
	t.fee = price.GetMonthlyFee()
	return t, nil
}

// reconcileBilling bills an account's ended periods, and its open one when
// its type or Plan changes or it goes, fixing each bound in its status
// before billing to it (docs/decisions/billing.md).
func (s *Server) reconcileBilling(ctx context.Context, k controller.Key) error {
	if s.cfg.Meter == nil {
		return nil
	}
	key := resource.Key{Name: k.Name}
	obj, err := s.get(ctx, registry.BillingAccount, key)
	if err != nil || obj == nil {
		return err
	}
	a := obj.(*billingv1alpha1.BillingAccount)
	spec := a.GetSpec()
	zone, err := billing.LoadZone(spec.GetTimeZone())
	if err != nil {
		return err
	}
	now := s.clock.Now().Truncate(time.Minute)
	limit := now
	deleting := a.GetMetadata().GetDeleteTime() != nil
	if deleting {
		// The meter stopped at the deletion; so does the bill, even if this
		// runs long after.
		if at := ceilMinute(a.GetMetadata().GetDeleteTime().AsTime()); at.Before(limit) {
			limit = at
		}
	}
	// wanted is what a period starting now would be billed by; a deleted
	// account's tenant may be gone, and it has no next period.
	wanted := func(have terms) (terms, error) {
		if deleting {
			return have, nil
		}
		return s.wantedTerms(ctx, a)
	}
	st := proto.Clone(a.GetStatus()).(*billingv1alpha1.BillingAccountStatus)
	if st == nil {
		st = &billingv1alpha1.BillingAccountStatus{}
	}
	st.ObservedGeneration = a.GetMetadata().GetGeneration()
	b := &billRun{s: s, account: a, zone: zone}
	if st.GetPeriodStartTime() == nil {
		// Until the first period starts, it is billed by the spec as it is,
		// deleted or not: nothing has set its terms yet.
		st.PeriodAnchorTime, st.PeriodOffsetMonths = spec.GetAnchorTime(), 0
		t, err := s.wantedTerms(ctx, a)
		if err != nil && deleting {
			// Its tenant or plan went first; nothing can be priced.
			s.log.Warn("billing account deleted before its terms were known; nothing billed", "account", k.Name, "error", err)
			return s.RemoveFinalizer(ctx, registry.BillingAccount, key, BillingFinalizer)
		}
		if err != nil {
			return err
		}
		t.into(st)
		anchor := st.GetPeriodAnchorTime().AsTime()
		if limit.Before(anchor) {
			if deleting {
				return s.RemoveFinalizer(ctx, registry.BillingAccount, key, BillingFinalizer)
			}
			return s.writeBillingStatus(ctx, a, st)
		}
		periods := billing.Periods{Anchor: anchor, Months: t.months, Zone: zone}
		st.PeriodStartTime, st.PeriodEndTime = timestamppb.New(anchor), timestamppb.New(periods.Start(1))
	}
	periodsOf := func() billing.Periods {
		return billing.Periods{Anchor: st.GetPeriodAnchorTime().AsTime(), Months: statusTerms(st).months, Offset: int(st.GetPeriodOffsetMonths()), Zone: zone}
	}
	// begin starts a period at from under t: cut from from, for a cut;
	// else the next of those before, or, of a new length, the first of its
	// own from there, on the same anchor so its day of the month stays.
	begin := func(t terms, from time.Time, recut bool) {
		switch {
		case recut:
			st.PeriodAnchorTime, st.PeriodOffsetMonths = timestamppb.New(from), 0
		case t.months != statusTerms(st).months:
			old := periodsOf()
			n, _, _, _ := old.At(from)
			st.PeriodOffsetMonths += int32(n * old.Months)
		}
		t.into(st)
		_, _, next, _ := periodsOf().At(from)
		st.PeriodStartTime, st.PeriodEndTime = timestamppb.New(from), timestamppb.New(next)
		st.Accrued, st.Metered, st.AccruedTime, st.UsageLines, st.PriceListVersion, st.Discount = "", "", nil, nil, 0, ""
	}
	// A cut recorded before is billed where it was recorded.
	if cut := st.GetPeriodCutTime(); cut != nil {
		at, start, end := cut.AsTime(), st.GetPeriodStartTime().AsTime(), st.GetPeriodEndTime().AsTime()
		if err := b.bill(ctx, statusTerms(st), start, at, end, billingv1alpha1.StatementEnd_STATEMENT_END_AGREEMENT_CHANGED); err != nil {
			return err
		}
		t, err := wanted(statusTerms(st))
		if err != nil {
			return err
		}
		st.PeriodCutTime = nil
		begin(t, at, true)
	}
	// Periods ended while nothing reconciled are each billed in turn; each
	// next one takes the agreement as it is then.
	for !st.GetPeriodEndTime().AsTime().After(limit) {
		start, end := st.GetPeriodStartTime().AsTime(), st.GetPeriodEndTime().AsTime()
		if err := b.bill(ctx, statusTerms(st), start, end, end, billingv1alpha1.StatementEnd_STATEMENT_END_PERIOD); err != nil {
			return err
		}
		t, err := wanted(statusTerms(st))
		if err != nil {
			return err
		}
		begin(t, end, false)
	}
	start, end := st.GetPeriodStartTime().AsTime(), st.GetPeriodEndTime().AsTime()
	if deleting {
		if limit.After(start) {
			if err := b.bill(ctx, statusTerms(st), start, limit, end, billingv1alpha1.StatementEnd_STATEMENT_END_ACCOUNT_DELETED); err != nil {
				return err
			}
		}
		return s.RemoveFinalizer(ctx, registry.BillingAccount, key, BillingFinalizer)
	}
	want, err := wanted(statusTerms(st))
	if err != nil {
		return err
	}
	if cuts(statusTerms(st), want) {
		if !limit.After(start) {
			// Nothing of the period has passed: it is simply the new one's.
			begin(want, start, true)
		} else {
			// The cut is recorded first, so a retry bills it at the same minute;
			// the write brings the next reconcile, which bills it.
			st.PeriodCutTime = timestamppb.New(limit)
			return s.writeBillingStatus(ctx, a, st)
		}
	}
	version, err := s.priceListVersion(ctx, statusTerms(st).priceList)
	if err != nil {
		return err
	}
	// A new price, or a spec changed, as by a promotion, rates it again.
	respec := a.GetStatus().GetObservedGeneration() != a.GetMetadata().GetGeneration()
	if st.GetAccruedTime() == nil || !now.Before(st.GetAccruedTime().AsTime().Add(accrualEvery)) || version != st.GetPriceListVersion() || respec {
		// The minute it is now is read already, and counts. A fixed fee is
		// the whole period's unless the period is cut.
		bill, err := b.rate(ctx, statusTerms(st), start, minTime(now.Add(time.Minute), end), end.Sub(start), end.Sub(start))
		if err != nil {
			return err
		}
		st.Accrued, st.Metered, st.AccruedTime, st.PriceListVersion = bill.total.String(), bill.metered.String(), timestamppb.New(now), version
		st.Discount = ""
		if bill.discount != 0 {
			st.Discount = billing.RoundToMinor(bill.discount, b.account.GetSpec().GetCurrency()).String()
		}
		st.UsageLines = nil
		for _, l := range bill.usage {
			st.UsageLines = append(st.UsageLines, l.Proto())
		}
	}
	return s.writeBillingStatus(ctx, a, st)
}

// writeBillingStatus writes st as a's status, conditional on a as read,
// unless it is a's already.
func (s *Server) writeBillingStatus(ctx context.Context, a *billingv1alpha1.BillingAccount, st *billingv1alpha1.BillingAccountStatus) error {
	if proto.Equal(st, a.GetStatus()) {
		return nil
	}
	rv := a.GetMetadata().GetResourceVersion()
	_, err := s.backend(registry.BillingAccount).UpdateStatus(s.system(ctx), resource.Key{Name: a.GetMetadata().GetName()}, st, &rv)
	return err
}

// billRun rates and bills the periods of one account.
type billRun struct {
	s       *Server
	account *billingv1alpha1.BillingAccount
	zone    *time.Location
}

// rated is a part of a period, rated.
type rated struct {
	lines []billing.Line
	total billing.Micros
	// usage is what the tenant held at the price list's prices, line by
	// line, and metered its total.
	usage   []billing.Line
	metered billing.Micros
	// discount is what the promotions take off, before total is rounded.
	discount billing.Micros
}

// rate is what the part of a period from start to end comes to under t:
// for a pay-as-you-go account, what the tenant held then at the price
// list's prices in each minute; for a fixed one, the fee, in the share
// passed is of whole. metered is what the tenant held then at the price
// list's prices, if t names one. The account's promotions take their
// part off, each on a line after the rest.
func (b *billRun) rate(ctx context.Context, t terms, start, end time.Time, passed, whole time.Duration) (rated, error) {
	currency := b.account.GetSpec().GetCurrency()
	var entries []billing.Entry
	if t.priceList != "" {
		obj, err := b.s.get(ctx, registry.PriceList, resource.Key{Name: t.priceList})
		if err != nil {
			return rated{}, err
		}
		if obj == nil {
			return rated{}, fmt.Errorf("%w: price list %q, which billing account %q is rated by, does not exist",
				controller.ErrWaiting, t.priceList, b.account.GetMetadata().GetName())
		}
		if entries, err = billing.ParseEntries(obj.(*billingv1alpha1.PriceList).GetSpec().GetEntries()); err != nil {
			return rated{}, err
		}
	}
	// usageOf is what the tenant held from from to to at the list's prices.
	usageOf := func(from, to time.Time) ([]billing.Line, error) {
		var lines []billing.Line
		if t.priceList == "" {
			return nil, nil
		}
		for _, span := range billing.Spans(entries, from, to) {
			held, err := b.s.meterHeld(ctx, b.account.GetMetadata().GetName(), span.From, span.To)
			if err != nil {
				return nil, err
			}
			lines = append(lines, billing.Rate(span, held)...)
		}
		return lines, nil
	}
	usage, err := usageOf(start, end)
	if err != nil {
		return rated{}, err
	}
	var out rated
	out.usage, out.metered = usage, billing.Total(usage, currency)
	// before is what the part of the period before until comes to.
	var before func(until time.Time) (billing.Micros, error)
	switch t.typ {
	case billingv1alpha1.BillingType_BILLING_TYPE_FIXED:
		fee, err := billing.ParseMoney(t.fee)
		if err != nil {
			return rated{}, err
		}
		out.lines = []billing.Line{billing.FeeLine(fee, passed, whole)}
		before = func(until time.Time) (billing.Micros, error) {
			return billing.FeeLine(fee, min(max(until.Sub(start), 0), passed), whole).Amount, nil
		}
	default:
		out.lines = usage
		before = func(until time.Time) (billing.Micros, error) {
			if !until.After(start) {
				return 0, nil
			}
			if !until.Before(end) {
				return sumOf(usage), nil
			}
			part, err := usageOf(start, until)
			return sumOf(part), err
		}
	}
	var promotions []billing.Promotion
	for _, p := range b.account.GetSpec().GetPromotions() {
		promotions = append(promotions, billing.Promotion{Label: p.GetLabel(), Percent: int64(p.GetPercent()), Until: p.GetUntilTime().AsTime()})
	}
	befores := map[time.Time]billing.Micros{}
	for _, p := range promotions {
		if _, ok := befores[p.Until]; !ok {
			if befores[p.Until], err = before(p.Until); err != nil {
				return rated{}, err
			}
		}
	}
	off := billing.PromotionLines(promotions, sumOf(out.lines), func(until time.Time) billing.Micros { return befores[until] })
	out.discount = -sumOf(off)
	out.lines = append(out.lines, off...)
	out.total = billing.Total(out.lines, currency)
	return out, nil
}

// sumOf is what lines come to, unrounded.
func sumOf(lines []billing.Line) billing.Micros {
	var sum billing.Micros
	for _, l := range lines {
		sum += l.Amount
	}
	return sum
}

// bill makes the Statement of a period from start to end, which would have
// ended at nominalEnd, once, then folds its whole hours. A name another
// account took already gets this account's uid after it.
func (b *billRun) bill(ctx context.Context, t terms, start, end, nominalEnd time.Time, why billingv1alpha1.StatementEnd) error {
	r, err := b.rate(ctx, t, start, end, end.Sub(start), nominalEnd.Sub(start))
	if err != nil {
		return err
	}
	tenant := b.account.GetMetadata().GetName()
	uid := b.account.GetMetadata().GetUid()
	spec := &billingv1alpha1.StatementSpec{
		Tenant: tenant, Type: t.typ, Currency: b.account.GetSpec().GetCurrency(),
		PeriodStartTime: timestamppb.New(start), PeriodEndTime: timestamppb.New(end), End: why,
		Total: r.total.String(), AccountUid: uid,
	}
	for _, l := range r.lines {
		spec.Lines = append(spec.Lines, l.Proto())
	}
	if t.typ == billingv1alpha1.BillingType_BILLING_TYPE_FIXED {
		spec.Plan = t.plan
		if t.priceList != "" {
			spec.Metered = r.metered.String()
		}
	}
	name := statementName(tenant, start, b.zone)
	suffix := uid[:min(len(uid), 6)]
	made := false
	// The same local minute starts two periods on the night clocks go back;
	// the second takes the minute it starts in UTC after its account's uid.
	for _, n := range []string{name, name + "-" + suffix, name + "-" + suffix + "-" + start.UTC().Format("0102t1504")} {
		if made, err = b.makeStatement(ctx, n, spec); err != nil || made {
			break
		}
	}
	if err != nil {
		return err
	}
	if !made {
		return fmt.Errorf("statement %s of billing account %s: its names are another account's", name, tenant)
	}
	return b.s.cfg.Meter.FoldMeter(ctx, tenant, start, end)
}

// makeStatement makes the Statement name with spec, and reports whether
// it is this period's: made now, or before by this account for the same
// start. Another account's, or another period's, is not.
func (b *billRun) makeStatement(ctx context.Context, name string, spec *billingv1alpha1.StatementSpec) (bool, error) {
	stmt := &billingv1alpha1.Statement{Metadata: &metav1.ObjectMeta{Name: name}, Spec: spec}
	_, err := b.s.backend(registry.Statement).Create(b.s.system(ctx), stmt)
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		return err == nil, err
	}
	obj, err := b.s.get(ctx, registry.Statement, resource.Key{Name: name})
	if err != nil || obj == nil {
		return false, err
	}
	had := obj.(*billingv1alpha1.Statement).GetSpec()
	return had.GetAccountUid() == spec.GetAccountUid() && had.GetPeriodStartTime().AsTime().Equal(spec.GetPeriodStartTime().AsTime()), nil
}

// statementName is the name of tenant's Statement of the period that starts
// at start, as "acme-202611010000", in the account's zone; a tenant's name
// is shortened so the name, with a uid after it, still fits.
func statementName(tenant string, start time.Time, zone *time.Location) string {
	const room = 63 - len("-200601021504") - len("-abcdef") - len("-0102t1504")
	if len(tenant) > room {
		tenant = strings.TrimRight(tenant[:room], "-")
	}
	return tenant + "-" + start.In(zone).Format("200601021504")
}

// ceilMinute is t rounded up to a whole minute.
func ceilMinute(t time.Time) time.Time {
	if m := t.Truncate(time.Minute); m.Before(t) {
		return m.Add(time.Minute)
	}
	return t
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
