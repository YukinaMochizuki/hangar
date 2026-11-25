package apiserver

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	billingv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/billing/v1alpha1"
	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/billing"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// billingEpoch is where the tests' clock starts: mid-month, so a month back
// or forward from it is the same day, whatever the real date.
var billingEpoch = time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)

// billedWith starts a server with its meter, node n1 with room and the
// clock at billingEpoch, and returns it with the system's, a platform
// root's and alice's contexts; alice owns tenant acme. controllers says
// whether its controllers run; without them a test reconciles by hand. The
// meter's own loop is left to the fake clock, so a test reads holdings with
// meterOnce when it means to.
func billedWith(t *testing.T, controllers bool) (s *Server, system, root, alice context.Context) {
	t.Helper()
	return billedAt(t, controllers, billingEpoch)
}

// billedAt is billedWith with the clock at epoch.
func billedAt(t *testing.T, controllers bool, epoch time.Time) (s *Server, system, root, alice context.Context) {
	t.Helper()
	s, system, alice = runningWith(t, func(c *Config) {
		c.Clock = clockwork.NewFakeClockAt(epoch)
		c.DisableControllers = !controllers
		c.Nodes = []string{"n1"}
		c.NodeMonitorInterval = 1000 * time.Hour
		c.Meter = c.Store.(store.Meter)
		c.MeterInterval = 1000 * time.Hour
	})
	if controllers {
		waitNode(t, s, "n1")
		nodeWithRoom(t, s, system, "n1", 64*1024)
	}
	return s, system, asRoot(t, s, system), alice
}

func billed(t *testing.T) (s *Server, system, root, alice context.Context) {
	t.Helper()
	return billedWith(t, true)
}

func fakeClock(s *Server) *clockwork.FakeClock { return s.clock.(*clockwork.FakeClock) }

func minuteNow(s *Server) time.Time { return s.clock.Now().Truncate(time.Minute) }

func priceList(name, currency string, entries ...*billingv1alpha1.PriceEntry) *billingv1alpha1.PriceList {
	return &billingv1alpha1.PriceList{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec:     &billingv1alpha1.PriceListSpec{Currency: currency, Entries: entries},
	}
}

func priceEntry(at time.Time, p *billingv1alpha1.Prices) *billingv1alpha1.PriceEntry {
	return &billingv1alpha1.PriceEntry{EffectiveTime: timestamppb.New(at), Prices: p}
}

func payg(tenant, list string, months int32, anchor time.Time) *billingv1alpha1.BillingAccount {
	return &billingv1alpha1.BillingAccount{
		Metadata: &metav1.ObjectMeta{Name: tenant},
		Spec: &billingv1alpha1.BillingAccountSpec{
			Type: billingv1alpha1.BillingType_BILLING_TYPE_PAY_AS_YOU_GO, Currency: "TWD",
			PriceList: list, PeriodMonths: months, AnchorTime: timestamppb.New(anchor),
		},
	}
}

func fixed(tenant string, anchor time.Time) *billingv1alpha1.BillingAccount {
	return &billingv1alpha1.BillingAccount{
		Metadata: &metav1.ObjectMeta{Name: tenant},
		Spec: &billingv1alpha1.BillingAccountSpec{
			Type: billingv1alpha1.BillingType_BILLING_TYPE_FIXED, Currency: "TWD", AnchorTime: timestamppb.New(anchor),
		},
	}
}

// pricedPlan is a plan of no limits with a monthly fee in TWD.
func pricedPlan(name, fee string) *tenancyv1alpha1.Plan {
	return &tenancyv1alpha1.Plan{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec:     &tenancyv1alpha1.PlanSpec{Limits: &tenancyv1alpha1.Limits{}, Price: &tenancyv1alpha1.PlanPrice{MonthlyFee: fee, Currency: "TWD"}},
	}
}

// onPlan moves tenant to plan, as root does, with an apply of no resource
// version: the usage controller writes the tenant's status meanwhile.
func onPlan(t *testing.T, s *Server, root context.Context, tenant, plan string) {
	t.Helper()
	mustApply(t, s, root, registry.Tenant, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: tenant}, Spec: &tenancyv1alpha1.TenantSpec{Plan: plan}})
}

// fixedOn opens a FIXED account for tenant on a plan of its name and fee.
func fixedOn(t *testing.T, s *Server, root context.Context, tenant, plan, fee string, anchor time.Time) {
	t.Helper()
	mustApply(t, s, root, registry.Plan, pricedPlan(plan, fee))
	onPlan(t, s, root, tenant, plan)
	mustApply(t, s, root, registry.BillingAccount, fixed(tenant, anchor))
}

func mustApply(t *testing.T, s *Server, ctx context.Context, r registry.Resource, obj proto.Message) {
	t.Helper()
	if _, err := s.backend(r).Apply(ctx, obj); err != nil {
		t.Fatalf("applying %s %s: %v", r.Kind, object.Meta(obj).GetName(), err)
	}
}

func accountOf(t *testing.T, s *Server, system context.Context, name string) *billingv1alpha1.BillingAccount {
	t.Helper()
	obj, err := s.get(system, registry.BillingAccount, resource.Key{Name: name})
	if err != nil || obj == nil {
		t.Fatalf("account %s: %v", name, err)
	}
	return obj.(*billingv1alpha1.BillingAccount)
}

func waitStatement(t *testing.T, s *Server, system context.Context, name string) *billingv1alpha1.Statement {
	t.Helper()
	return waitObject(t, s, system, registry.Statement, resource.Key{Name: name}, "statement "+name, func(*billingv1alpha1.Statement) bool { return true })
}

func waitAccount(t *testing.T, s *Server, system context.Context, what string, ok func(*billingv1alpha1.BillingAccount) bool) *billingv1alpha1.BillingAccount {
	t.Helper()
	return waitObject(t, s, system, registry.BillingAccount, resource.Key{Name: "acme"}, "account acme "+what, ok)
}

func statementsOf(t *testing.T, s *Server, system context.Context, tenant string) []*billingv1alpha1.Statement {
	t.Helper()
	objs, err := s.listWith(system, registry.Statement, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []*billingv1alpha1.Statement
	for _, o := range objs {
		if st := o.(*billingv1alpha1.Statement); st.GetSpec().GetTenant() == tenant {
			out = append(out, st)
		}
	}
	return out
}

func lineOf(st *billingv1alpha1.Statement, item string) *billingv1alpha1.StatementLine {
	for _, l := range st.GetSpec().GetLines() {
		if l.GetItem() == item {
			return l
		}
	}
	return nil
}

// runningContainer makes container name of 1 vCPU, 1Gi and 10Gi, and
// reports its machine running on n1.
func runningContainer(t *testing.T, s *Server, system, alice context.Context, name string) {
	t.Helper()
	c := sized(name, 1024)
	c.Spec.Resources.Disk = "10Gi"
	applyCtr(t, s, alice, c)
	m := waitMachine(t, s, system, "ct-"+name, "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration())
}

// reconcile runs the billing controller's reconcile of acme once, for a
// test without controllers.
func reconcile(t *testing.T, s *Server, system context.Context) {
	t.Helper()
	if err := s.reconcileBilling(system, controller.Key{Name: "acme"}); err != nil {
		t.Fatal(err)
	}
}

func TestBillingAccountAdmission(t *testing.T) {
	t.Parallel()
	s, _, root, alice := billed(t)
	now := minuteNow(s)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD", priceEntry(now, &billingv1alpha1.Prices{Cpu: "0.6"})))
	mustApply(t, s, root, registry.PriceList, priceList("usd", "USD"))
	mustApply(t, s, root, registry.Plan, pricedPlan("dollars", "10"))
	usd := pricedPlan("usd-plan", "10")
	usd.Spec.Price.Currency = "USD"
	mustApply(t, s, root, registry.Plan, usd)
	ok := payg("acme", "std", 3, now)
	for name, mutate := range map[string]func(a *billingv1alpha1.BillingAccount){
		"no type":                      func(a *billingv1alpha1.BillingAccount) { a.Spec.Type = 0 },
		"no price list":                func(a *billingv1alpha1.BillingAccount) { a.Spec.PriceList = "" },
		"a price list that is not":     func(a *billingv1alpha1.BillingAccount) { a.Spec.PriceList = "none" },
		"a price list in another unit": func(a *billingv1alpha1.BillingAccount) { a.Spec.PriceList = "usd" },
		"seven months":                 func(a *billingv1alpha1.BillingAccount) { a.Spec.PeriodMonths = 7 },
		"a bad currency":               func(a *billingv1alpha1.BillingAccount) { a.Spec.Currency = "twd" },
		"no anchor":                    func(a *billingv1alpha1.BillingAccount) { a.Spec.AnchorTime = nil },
		"an anchor off the minute":     func(a *billingv1alpha1.BillingAccount) { a.Spec.AnchorTime = timestamppb.New(now.Add(time.Second)) },
		"an unknown zone":              func(a *billingv1alpha1.BillingAccount) { a.Spec.TimeZone = "Not/AZone" },
		"no tenant of its name":        func(a *billingv1alpha1.BillingAccount) { a.Metadata.Name = "nobody" },
		// acme is on the default plan, which has no price.
		"a fixed one on a plan of no price": func(a *billingv1alpha1.BillingAccount) {
			a.Spec.Type, a.Spec.PriceList, a.Spec.PeriodMonths = billingv1alpha1.BillingType_BILLING_TYPE_FIXED, "", 0
		},
	} {
		a := proto.Clone(ok).(*billingv1alpha1.BillingAccount)
		mutate(a)
		if _, err := s.backend(registry.BillingAccount).Apply(root, a); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
	onPlan(t, s, root, "acme", "usd-plan")
	if _, err := s.backend(registry.BillingAccount).Apply(root, fixed("acme", now)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a fixed account on a plan in another currency: %v, want InvalidArgument", err)
	}
	onPlan(t, s, root, "acme", "dollars")
	two := fixed("acme", now)
	two.Spec.PeriodMonths = 2
	if _, err := s.backend(registry.BillingAccount).Apply(root, two); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a fixed account of two months: %v, want InvalidArgument", err)
	}
	if _, err := s.backend(registry.BillingAccount).Apply(root, ok); err != nil {
		t.Fatal(err)
	}
	// Its tenant's owner reads it, and cannot change it.
	if _, err := s.backend(registry.BillingAccount).Get(alice, resource.Key{Name: "acme"}); err != nil {
		t.Errorf("acme's owner reading its account: %v", err)
	}
	changed := proto.Clone(ok).(*billingv1alpha1.BillingAccount)
	changed.Spec.PeriodMonths = 1
	if _, err := s.backend(registry.BillingAccount).Apply(alice, changed); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("acme's owner changing its account: %v, want PermissionDenied", err)
	}
	if _, err := s.backend(registry.BillingAccount).Delete(alice, resource.Key{Name: "acme"}, nil); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("acme's owner deleting its account: %v, want PermissionDenied", err)
	}
	// Someone else's tenant's owner does not read it, nor make its own.
	addMember(t, s, "bob@example.com")
	bob := auth.WithPrincipal(alice, auth.Principal{Type: auth.User, ID: "bob@example.com"})
	if _, err := s.backend(registry.Tenant).Apply(bob, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "bobs"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.BillingAccount).Get(bob, resource.Key{Name: "acme"}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("another tenant's owner reading acme's account: %v, want PermissionDenied", err)
	}
	if list, err := s.backend(registry.BillingAccount).List(bob, resource.ListOptions{}); err != nil || len(list.Items) != 0 {
		t.Errorf("another tenant's owner lists %v, %v", list.Items, err)
	}
	if _, err := s.backend(registry.BillingAccount).Apply(bob, payg("bobs", "std", 1, now)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a tenant's owner opening its own account: %v, want PermissionDenied", err)
	}
	// Everyone reads the price lists, and only root writes them.
	if _, err := s.backend(registry.PriceList).Get(bob, resource.Key{Name: "std"}); err != nil {
		t.Errorf("reading a price list: %v", err)
	}
	if _, err := s.backend(registry.PriceList).Apply(alice, priceList("mine", "TWD")); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a tenant's owner writing a price list: %v", err)
	}
	// A price list an account names stays.
	if _, err := s.backend(registry.PriceList).Delete(root, resource.Key{Name: "std"}, nil); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("deleting a price list in use: %v, want FailedPrecondition", err)
	}
}

// TestWhoReadsBilling: a tenant's account and statements are read at the
// tenant alone, by its owners and viewers; not by its projects' people,
// whatever their role, nor by another tenant's. A Statement is written by
// the apiserver alone, root included.
func TestWhoReadsBilling(t *testing.T) {
	t.Parallel()
	s, system, root, _ := billed(t)
	now := minuteNow(s)
	fixedOn(t, s, root, "acme", "standard", "300", now.AddDate(0, -1, 0))
	name := statementName("acme", now.AddDate(0, -1, 0), time.UTC)
	waitStatement(t, s, system, name)
	people := map[string]struct {
		role  string
		scope *iamv1alpha1.BindingScope
		reads bool
	}{
		"viewer@example.com":   {"tenant.viewer", &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: "acme"}}, true},
		"powner@example.com":   {"project.owner", &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Project{Project: "web"}}, false},
		"pviewer@example.com":  {"project.viewer", &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Project{Project: "web"}}, false},
		"stranger@example.com": {"", nil, false},
	}
	for email, p := range people {
		addMember(t, s, email)
		if p.role != "" {
			mustApply(t, s, system, registry.RoleBinding, binding(usernameOf(email)+"-acme", email, p.role, p.scope))
		}
	}
	for email, p := range people {
		ctx := auth.WithPrincipal(system, auth.Principal{Type: auth.User, ID: email})
		for _, r := range []registry.Resource{registry.BillingAccount, registry.Statement} {
			key := resource.Key{Name: "acme"}
			if r.Kind == registry.Statement.Kind {
				key = resource.Key{Name: name}
			}
			_, err := s.backend(r).Get(ctx, key)
			if (err == nil) != p.reads {
				t.Errorf("%s reading %s: %v, want read %t", email, r.Kind, err, p.reads)
			}
			list, err := s.backend(r).List(ctx, resource.ListOptions{})
			if err != nil || (len(list.Items) > 0) != p.reads {
				t.Errorf("%s listing %s: %d, %v; want read %t", email, r.Plural, len(list.Items), err, p.reads)
			}
		}
	}
	st := waitStatement(t, s, system, name)
	st.Spec.Total = "0"
	if _, err := s.backend(registry.Statement).Apply(root, st); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("root rewriting a statement: %v, want PermissionDenied", err)
	}
	if _, err := s.backend(registry.Statement).Delete(root, resource.Key{Name: name}, nil); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("root deleting a statement: %v, want PermissionDenied", err)
	}
}

func TestAPriceInEffectNeverChanges(t *testing.T) {
	t.Parallel()
	s, _, root, _ := billed(t)
	now := minuteNow(s)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	b := s.backend(registry.PriceList)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD",
		priceEntry(past, &billingv1alpha1.Prices{Cpu: "0.6"}), priceEntry(future, &billingv1alpha1.Prices{Cpu: "0.7"})))
	for name, entries := range map[string][]*billingv1alpha1.PriceEntry{
		"a price in effect changed": {priceEntry(past, &billingv1alpha1.Prices{Cpu: "0.5"})},
		"a price in effect removed": {priceEntry(future, &billingv1alpha1.Prices{Cpu: "0.7"})},
		"a new price in the past": {priceEntry(past, &billingv1alpha1.Prices{Cpu: "0.6"}),
			priceEntry(now.Add(-time.Minute), &billingv1alpha1.Prices{Cpu: "0.1"})},
		// The minute it is is metered at the price it began with.
		"a new price this minute": {priceEntry(past, &billingv1alpha1.Prices{Cpu: "0.6"}),
			priceEntry(now, &billingv1alpha1.Prices{Cpu: "0.1"})},
	} {
		if _, err := b.Apply(root, priceList("std", "TWD", entries...)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
	// A price not yet in effect may change, and one may be added from the next minute.
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD",
		priceEntry(past, &billingv1alpha1.Prices{Cpu: "0.6"}), priceEntry(now.Add(time.Minute), &billingv1alpha1.Prices{Cpu: "0.65"}),
		priceEntry(future, &billingv1alpha1.Prices{Cpu: "0.8"})))
}

// TestAFixedTenantStaysOnAPricedPlan: while acme pays its plan's price, it
// is moved to no plan without one, and its plan keeps one in its currency.
func TestAFixedTenantStaysOnAPricedPlan(t *testing.T) {
	t.Parallel()
	s, _, root, _ := billed(t)
	fixedOn(t, s, root, "acme", "standard", "300", minuteNow(s))
	mustApply(t, s, root, registry.Plan, &tenancyv1alpha1.Plan{Metadata: &metav1.ObjectMeta{Name: "free"}, Spec: &tenancyv1alpha1.PlanSpec{Limits: &tenancyv1alpha1.Limits{}}})
	tn := &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "acme"}, Spec: &tenancyv1alpha1.TenantSpec{Plan: "free"}}
	if _, err := s.backend(registry.Tenant).Apply(root, tn); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("moving a fixed tenant to a plan of no price: %v, want InvalidArgument", err)
	}
	unpriced := pricedPlan("standard", "300")
	unpriced.Spec.Price = nil
	if _, err := s.backend(registry.Plan).Apply(root, unpriced); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("taking a fixed tenant's plan's price away: %v, want InvalidArgument", err)
	}
	usd := pricedPlan("standard", "10")
	usd.Spec.Price.Currency = "USD"
	if _, err := s.backend(registry.Plan).Apply(root, usd); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("changing a fixed tenant's plan's currency: %v, want InvalidArgument", err)
	}
	bad := pricedPlan("odd", "-1")
	if _, err := s.backend(registry.Plan).Apply(root, bad); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a negative fee: %v, want InvalidArgument", err)
	}
	// A new price is taken.
	mustApply(t, s, root, registry.Plan, pricedPlan("standard", "350"))
}

func TestTheMeterCountsWhatRunsAndWhatIsHeld(t *testing.T) {
	t.Parallel()
	s, system, root, alice := billed(t)
	now := minuteNow(s)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD"))
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 1, now))
	runningContainer(t, s, system, alice, "api")
	if err := s.meterOnce(system, now); err != nil {
		t.Fatal(err)
	}
	held, err := s.meterHeld(system, "acme", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if held["cpu"] != 1000 || held["memory"] != 1024 || held["disk"] != 10 {
		t.Errorf("a running container's minute = %v, want 1000 millicores, 1024 MiB and 10 GiB", held)
	}
	// A minute is read once: a later reading in it changes nothing.
	m := waitMachine(t, s, system, "ct-api", "read", func(*computev1alpha1.Machine) bool { return true })
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration())
	if err := s.meterOnce(system, now.Add(40*time.Second)); err != nil {
		t.Fatal(err)
	}
	if held, _ := s.meterHeld(system, "acme", now, now.Add(time.Minute)); held["cpu"] != 1000 {
		t.Errorf("a minute read twice = %v, want its first reading", held)
	}
	// Stopped, as its node reports it, it holds its disk alone.
	next := now.Add(time.Minute)
	if err := s.meterOnce(system, next); err != nil {
		t.Fatal(err)
	}
	held, err = s.meterHeld(system, "acme", next, next.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if held["cpu"] != 0 || held["memory"] != 0 || held["disk"] != 10 {
		t.Errorf("a stopped container's minute = %v, want 10 GiB alone", held)
	}
	later := next.Add(time.Minute)
	// A tenant with no account is not metered.
	if _, err := s.backend(registry.Tenant).Apply(system, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "other"}}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, s, system, registry.Project, &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: "lab"}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "other"}})
	other := sized("db", 1024)
	other.Metadata.Project = "lab"
	applyCtr(t, s, system, other)
	if err := s.meterOnce(system, later.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if held, _ := s.meterHeld(system, "other", now, later.Add(2*time.Minute)); len(held) != 0 {
		t.Errorf("a tenant with no account was metered: %v", held)
	}
}

// TestAPayAsYouGoPeriodIsBilled: a period of three months, metered for its
// last hour, whose price changes half way through that hour.
func TestAPayAsYouGoPeriodIsBilled(t *testing.T) {
	t.Parallel()
	s, system, root, alice := billed(t)
	clock := fakeClock(s)
	now := minuteNow(s)
	anchor := now.AddDate(0, -3, 0).Add(time.Hour)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD",
		priceEntry(anchor, &billingv1alpha1.Prices{Cpu: "0.6", Memory: "0.2", Disk: "0.005"}),
		priceEntry(now.Add(30*time.Minute), &billingv1alpha1.Prices{Cpu: "1.2", Memory: "0.2", Disk: "0.005"})))
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 3, anchor))
	runningContainer(t, s, system, alice, "api")
	for i := range 60 {
		if err := s.meterOnce(system, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(time.Hour)
	name := statementName("acme", anchor, time.UTC)
	st := waitStatement(t, s, system, name)
	// An hour of 1Gi and 10Gi: 0.2 + 0.05; and of 1 vCPU, half at 0.6 and
	// half at 1.2: 0.3 + 0.6.
	if st.GetSpec().GetTotal() != "1.15" || st.GetSpec().GetEnd() != billingv1alpha1.StatementEnd_STATEMENT_END_PERIOD {
		t.Errorf("statement %v", st.GetSpec())
	}
	var cpu []string
	for _, l := range st.GetSpec().GetLines() {
		if l.GetItem() == "cpu" {
			cpu = append(cpu, l.GetQuantity()+"@"+l.GetUnitPrice()+"="+l.GetAmount())
		}
	}
	if fmt.Sprint(cpu) != "[0.5@0.6=0.3 0.5@1.2=0.6]" {
		t.Errorf("cpu lines %v, want one at each price", cpu)
	}
	if !st.GetSpec().GetPeriodStartTime().AsTime().Equal(anchor) || !st.GetSpec().GetPeriodEndTime().AsTime().Equal(now.Add(time.Hour)) {
		t.Errorf("period %s to %s", st.GetSpec().GetPeriodStartTime().AsTime(), st.GetSpec().GetPeriodEndTime().AsTime())
	}
	a := waitAccount(t, s, system, "in its next period", func(a *billingv1alpha1.BillingAccount) bool {
		return a.GetStatus().GetPeriodStartTime().AsTime().Equal(now.Add(time.Hour))
	})
	if a.GetStatus().GetAccrued() != "0" || !a.GetStatus().GetPeriodEndTime().AsTime().Equal(anchor.AddDate(0, 6, 0)) {
		t.Errorf("the next period: %v", a.GetStatus())
	}
	// The billed minutes were folded, and still sum the same.
	held, err := s.meterHeld(system, "acme", anchor, now.Add(time.Hour))
	if err != nil || held["cpu"] != 60_000 {
		t.Errorf("held over the billed period = %v, %v", held, err)
	}
	// Folded, the hour is one row, counted whole where it starts: its second
	// half alone holds nothing.
	if held, _ := s.meterHeld(system, "acme", now.Add(30*time.Minute), now.Add(time.Hour)); held["cpu"] != 0 {
		t.Errorf("the second half of the billed hour holds %v: it was not folded", held)
	}
	// The tenant's owner reads the statement.
	if _, err := s.backend(registry.Statement).Get(alice, resource.Key{Name: name}); err != nil {
		t.Errorf("acme's owner reading its statement: %v", err)
	}
}

func TestPeriodsMissedAreEachBilled(t *testing.T) {
	t.Parallel()
	s, system, root, _ := billed(t)
	now := minuteNow(s)
	anchor := now.AddDate(0, -3, 0)
	fixedOn(t, s, root, "acme", "standard", "300", anchor)
	for n := range 3 {
		st := waitStatement(t, s, system, statementName("acme", anchor.AddDate(0, n, 0), time.UTC))
		if st.GetSpec().GetTotal() != "300" || lineOf(st, "monthly_fee").GetQuantity() != "1" || st.GetSpec().GetPlan() != "standard" {
			t.Errorf("period %d: %v", n, st.GetSpec())
		}
	}
	a := waitAccount(t, s, system, "in its fourth period", func(a *billingv1alpha1.BillingAccount) bool {
		return a.GetStatus().GetPeriodStartTime().AsTime().Equal(now)
	})
	if a.GetStatus().GetAccrued() != "300" {
		t.Errorf("a fixed period has come to %q, want its fee", a.GetStatus().GetAccrued())
	}
	if n := len(statementsOf(t, s, system, "acme")); n != 3 {
		t.Errorf("%d statements, want 3", n)
	}
}

func TestATypeChangeEndsThePeriod(t *testing.T) {
	t.Parallel()
	s, system, root, _ := billed(t)
	now := minuteNow(s)
	anchor := now.AddDate(0, 0, -10)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD"))
	fixedOn(t, s, root, "acme", "standard", "300", anchor)
	waitAccount(t, s, system, "in a period", func(a *billingv1alpha1.BillingAccount) bool { return a.GetStatus().GetPeriodStartTime() != nil })
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 3, anchor))
	st := waitStatement(t, s, system, statementName("acme", anchor, time.UTC))
	want := billing.FeeLine(300_000_000, now.Sub(anchor), anchor.AddDate(0, 1, 0).Sub(anchor))
	if st.GetSpec().GetEnd() != billingv1alpha1.StatementEnd_STATEMENT_END_AGREEMENT_CHANGED ||
		!st.GetSpec().GetPeriodEndTime().AsTime().Equal(now) || lineOf(st, "monthly_fee").GetAmount() != want.Amount.String() {
		t.Errorf("statement %v, want the fee's %s to now", st.GetSpec(), want.Amount)
	}
	a := waitAccount(t, s, system, "cut from the change", func(a *billingv1alpha1.BillingAccount) bool {
		return a.GetStatus().GetPeriodType() == billingv1alpha1.BillingType_BILLING_TYPE_PAY_AS_YOU_GO
	})
	if !a.GetStatus().GetPeriodAnchorTime().AsTime().Equal(now) || !a.GetStatus().GetPeriodEndTime().AsTime().Equal(now.AddDate(0, 3, 0)) ||
		a.GetStatus().GetPeriodCutTime() != nil {
		t.Errorf("status %v", a.GetStatus())
	}
}

// TestAPlanChangeEndsAFixedPeriod: a trial on a plan of no fee becomes a
// paying tenant at once, from the minute its plan changes.
func TestAPlanChangeEndsAFixedPeriod(t *testing.T) {
	t.Parallel()
	s, system, root, _ := billed(t)
	now := minuteNow(s)
	anchor := now.AddDate(0, 0, -10)
	fixedOn(t, s, root, "acme", "trial", "0", anchor)
	waitAccount(t, s, system, "in a period", func(a *billingv1alpha1.BillingAccount) bool { return a.GetStatus().GetPeriodStartTime() != nil })
	mustApply(t, s, root, registry.Plan, pricedPlan("standard", "300"))
	onPlan(t, s, root, "acme", "standard")
	st := waitStatement(t, s, system, statementName("acme", anchor, time.UTC))
	if st.GetSpec().GetTotal() != "0" || st.GetSpec().GetPlan() != "trial" || !st.GetSpec().GetPeriodEndTime().AsTime().Equal(now) ||
		st.GetSpec().GetEnd() != billingv1alpha1.StatementEnd_STATEMENT_END_AGREEMENT_CHANGED {
		t.Errorf("the trial's statement %v", st.GetSpec())
	}
	a := waitAccount(t, s, system, "on the new plan", func(a *billingv1alpha1.BillingAccount) bool {
		return a.GetStatus().GetPeriodPlan() == "standard"
	})
	if a.GetStatus().GetPeriodMonthlyFee() != "300" || !a.GetStatus().GetPeriodStartTime().AsTime().Equal(now) ||
		!a.GetStatus().GetPeriodEndTime().AsTime().Equal(now.AddDate(0, 1, 0)) {
		t.Errorf("status %v", a.GetStatus())
	}
}

// TestOtherChangesWaitForTheNextPeriod: a new price of the plan, a new price
// list and a new length change nothing of the period open.
func TestOtherChangesWaitForTheNextPeriod(t *testing.T) {
	t.Parallel()
	s, system, root, _ := billed(t)
	clock := fakeClock(s)
	now := minuteNow(s)
	anchor := now.AddDate(0, -1, 0).Add(time.Hour)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD"))
	mustApply(t, s, root, registry.PriceList, priceList("new", "TWD"))
	fixedOn(t, s, root, "acme", "standard", "300", anchor)
	waitAccount(t, s, system, "in a period", func(a *billingv1alpha1.BillingAccount) bool { return a.GetStatus().GetPeriodStartTime() != nil })
	mustApply(t, s, root, registry.Plan, pricedPlan("standard", "400"))
	waitSettled(t, s)
	if n := len(statementsOf(t, s, system, "acme")); n != 0 {
		t.Fatalf("a new price ended the period: %d statements", n)
	}
	clock.Advance(time.Hour)
	st := waitStatement(t, s, system, statementName("acme", anchor, time.UTC))
	if st.GetSpec().GetTotal() != "300" {
		t.Errorf("the period billed %s, want the fee it began with", st.GetSpec().GetTotal())
	}
	waitAccount(t, s, system, "at the new price", func(a *billingv1alpha1.BillingAccount) bool {
		return a.GetStatus().GetPeriodMonthlyFee() == "400"
	})
	// A pay-as-you-go account's price list and length wait too.
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 1, anchor))
	a := waitAccount(t, s, system, "pay as you go", func(a *billingv1alpha1.BillingAccount) bool {
		return a.GetStatus().GetPeriodType() == billingv1alpha1.BillingType_BILLING_TYPE_PAY_AS_YOU_GO
	})
	switched := a.GetStatus().GetPeriodStartTime().AsTime()
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "new", 3, anchor))
	waitSettled(t, s)
	a = accountOf(t, s, system, "acme")
	if a.GetStatus().GetPeriodPriceList() != "std" || a.GetStatus().GetPeriodMonths() != 1 || !a.GetStatus().GetPeriodStartTime().AsTime().Equal(switched) {
		t.Errorf("a new price list or length changed the period open: %v", a.GetStatus())
	}
}

// TestACutIsBilledWhereItWasSeen: a change of type seen at one minute is
// billed at that minute, however late the reconcile that bills it runs.
func TestACutIsBilledWhereItWasSeen(t *testing.T) {
	t.Parallel()
	// By hand: what is tested is a reconcile running late, after the clock
	// moved, which running controllers would not leave late.
	s, system, root, _ := billedWith(t, false)
	clock := fakeClock(s)
	now := minuteNow(s)
	anchor := now.AddDate(0, 0, -10)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD"))
	fixedOn(t, s, root, "acme", "standard", "300", anchor)
	reconcile(t, s, system)
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 1, anchor))
	reconcile(t, s, system)
	if cut := accountOf(t, s, system, "acme").GetStatus().GetPeriodCutTime(); cut == nil || !cut.AsTime().Equal(now) {
		t.Fatalf("cut %v, want recorded at %s", cut, now)
	}
	clock.Advance(10 * time.Minute)
	reconcile(t, s, system)
	reconcile(t, s, system)
	st := statementsOf(t, s, system, "acme")
	if len(st) != 1 || !st[0].GetSpec().GetPeriodEndTime().AsTime().Equal(now) {
		t.Fatalf("statements %v, want one ending at the cut", st)
	}
	a := accountOf(t, s, system, "acme")
	if !a.GetStatus().GetPeriodStartTime().AsTime().Equal(now) || a.GetStatus().GetPeriodCutTime() != nil {
		t.Errorf("the next period %v, want it from the cut", a.GetStatus())
	}
}

// TestAChangeBeforeTheAnchorIsTheFirstPeriods: an account changed before
// its first period starts starts it as changed, at its anchor.
func TestAChangeBeforeTheAnchorIsTheFirstPeriods(t *testing.T) {
	t.Parallel()
	s, system, root, _ := billed(t)
	clock := fakeClock(s)
	now := minuteNow(s)
	anchor := now.Add(time.Hour)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD"))
	fixedOn(t, s, root, "acme", "standard", "300", anchor)
	waitAccount(t, s, system, "fixed, before its anchor", func(a *billingv1alpha1.BillingAccount) bool {
		return a.GetStatus().GetPeriodType() == billingv1alpha1.BillingType_BILLING_TYPE_FIXED
	})
	// A new price before the anchor is the first period's, as the plan's
	// watch brings it, before any resync.
	mustApply(t, s, root, registry.Plan, pricedPlan("standard", "350"))
	waitAccount(t, s, system, "at the new price", func(a *billingv1alpha1.BillingAccount) bool {
		return a.GetStatus().GetPeriodMonthlyFee() == "350"
	})
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 1, anchor))
	waitAccount(t, s, system, "pay as you go before its anchor", func(a *billingv1alpha1.BillingAccount) bool {
		return a.GetStatus().GetPeriodType() == billingv1alpha1.BillingType_BILLING_TYPE_PAY_AS_YOU_GO
	})
	clock.Advance(2 * time.Hour)
	a := waitAccount(t, s, system, "in its first period", func(a *billingv1alpha1.BillingAccount) bool { return a.GetStatus().GetPeriodStartTime() != nil })
	waitSettled(t, s)
	if st := statementsOf(t, s, system, "acme"); len(st) != 0 {
		t.Errorf("statements %v, want none", st)
	}
	if a.GetStatus().GetPeriodType() != billingv1alpha1.BillingType_BILLING_TYPE_PAY_AS_YOU_GO || !a.GetStatus().GetPeriodStartTime().AsTime().Equal(anchor) {
		t.Errorf("status %v, want pay as you go from the anchor", a.GetStatus())
	}
}

// TestADeletedAccountBillsItsLastPeriod: to its deletion, however late that
// is billed; and a new account of the tenant starts no earlier.
func TestADeletedAccountBillsItsLastPeriod(t *testing.T) {
	t.Parallel()
	// By hand, as above: the deletion is billed by a reconcile running late.
	s, system, root, _ := billedWith(t, false)
	clock := fakeClock(s)
	now := minuteNow(s)
	anchor := now.AddDate(0, 0, -15)
	fixedOn(t, s, root, "acme", "standard", "300", anchor)
	reconcile(t, s, system)
	if _, err := s.backend(registry.BillingAccount).Delete(root, resource.Key{Name: "acme"}, nil); err != nil {
		t.Fatal(err)
	}
	clock.Advance(3 * time.Hour)
	reconcile(t, s, system)
	st := statementsOf(t, s, system, "acme")
	if len(st) != 1 || st[0].GetSpec().GetEnd() != billingv1alpha1.StatementEnd_STATEMENT_END_ACCOUNT_DELETED ||
		!st[0].GetSpec().GetPeriodEndTime().AsTime().Equal(now) {
		t.Fatalf("statements %v, want one to the deletion", st)
	}
	if obj, err := s.get(system, registry.BillingAccount, resource.Key{Name: "acme"}); err != nil || obj != nil {
		t.Fatalf("the account stayed: %v, %v", obj, err)
	}
	later := minuteNow(s)
	if _, err := s.backend(registry.BillingAccount).Apply(root, fixed("acme", anchor)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("an account over periods billed: %v, want InvalidArgument", err)
	}
	mustApply(t, s, root, registry.BillingAccount, fixed("acme", later))
}

// TestAStatementNameAnotherAccountTook: a period starting where another
// account of the tenant made a statement is billed under a name of its
// own, once.
func TestAStatementNameAnotherAccountTook(t *testing.T) {
	t.Parallel()
	s, system, root, _ := billed(t)
	now := minuteNow(s)
	anchor := now.AddDate(0, -1, 0)
	name := statementName("acme", anchor, time.UTC)
	mustApply(t, s, system, registry.Statement, &billingv1alpha1.Statement{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec: &billingv1alpha1.StatementSpec{Tenant: "acme", AccountUid: "someone-else",
			PeriodStartTime: timestamppb.New(anchor.Add(-time.Hour)), PeriodEndTime: timestamppb.New(anchor)},
	})
	fixedOn(t, s, root, "acme", "standard", "300", anchor)
	uid := accountOf(t, s, system, "acme").GetMetadata().GetUid()
	st := waitStatement(t, s, system, name+"-"+uid[:6])
	if st.GetSpec().GetTotal() != "300" {
		t.Errorf("the account's statement %v", st.GetSpec())
	}
	waitSettled(t, s)
	if n := len(statementsOf(t, s, system, "acme")); n != 2 {
		t.Errorf("%d statements, want the other's and one of its own", n)
	}
}

// TestASettledAccountStaysQuiet: between accruals, a reconcile writes
// nothing.
func TestASettledAccountStaysQuiet(t *testing.T) {
	t.Parallel()
	s, system, root, _ := billed(t)
	clock := fakeClock(s)
	fixedOn(t, s, root, "acme", "standard", "300", minuteNow(s).AddDate(0, 0, -1))
	waitAccount(t, s, system, "accrued", func(a *billingv1alpha1.BillingAccount) bool { return a.GetStatus().GetAccruedTime() != nil })
	waitSettled(t, s)
	rv := accountOf(t, s, system, "acme").GetMetadata().GetResourceVersion()
	clock.Advance(billingResync)
	waitSettled(t, s)
	if got := accountOf(t, s, system, "acme").GetMetadata().GetResourceVersion(); got != rv {
		t.Errorf("a resync within an accrual wrote the account: %d, was %d", got, rv)
	}
}

func TestOnlyRootRecordsAStatementPaid(t *testing.T) {
	t.Parallel()
	s, system, root, alice := billed(t)
	now := minuteNow(s)
	anchor := now.AddDate(0, -1, 0)
	fixedOn(t, s, root, "acme", "standard", "300", anchor)
	name := statementName("acme", anchor, time.UTC)
	waitStatement(t, s, system, name)
	pay := statementPayments{s}
	req := connect.NewRequest(&billingv1alpha1.MarkStatementPaidRequest{Name: name})
	if _, err := pay.MarkStatementPaid(alice, req); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("the tenant's owner recording it paid: %v, want PermissionDenied", err)
	}
	if _, err := s.backend(registry.Statement).UpdateStatus(root, resource.Key{Name: name}, &billingv1alpha1.StatementStatus{PaidTime: timestamppb.Now()}, nil); err == nil {
		t.Error("root wrote a statement's status directly")
	}
	res, err := pay.MarkStatementPaid(root, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetStatement().GetStatus().GetPaidTime() == nil || res.Msg.GetStatement().GetStatus().GetPaidRecordedBy() == "" {
		t.Errorf("status %v", res.Msg.GetStatement().GetStatus())
	}
	res, err = pay.MarkStatementPaid(root, connect.NewRequest(&billingv1alpha1.MarkStatementPaidRequest{Name: name, Unpaid: true}))
	if err != nil || res.Msg.GetStatement().GetStatus().GetPaidTime() != nil {
		t.Errorf("taking it back: %v, %v", res.Msg.GetStatement().GetStatus(), err)
	}
	if _, err := pay.MarkStatementPaid(root, connect.NewRequest(&billingv1alpha1.MarkStatementPaidRequest{Name: "none"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a statement that is not: %v", err)
	}
	// TestPermissions answers as the call does.
	for who, ctx := range map[string]context.Context{"root": root, "alice": alice} {
		p, _ := auth.FromContext(ctx)
		held := s.holds(p, "billing.statements.pay", testedScope{})
		if held != (who == "root") {
			t.Errorf("TestPermissions says %s holds billing.statements.pay: %t", who, held)
		}
	}
}

// TestAnAccountDeletedBeforeItsFirstReconcile: deleted before anything set
// its terms, it is billed by its spec to its deletion, and goes.
func TestAnAccountDeletedBeforeItsFirstReconcile(t *testing.T) {
	t.Parallel()
	s, system, root, _ := billedWith(t, false)
	anchor := minuteNow(s).AddDate(0, 0, -10)
	fixedOn(t, s, root, "acme", "standard", "300", anchor)
	if _, err := s.backend(registry.BillingAccount).Delete(root, resource.Key{Name: "acme"}, nil); err != nil {
		t.Fatal(err)
	}
	reconcile(t, s, system)
	st := statementsOf(t, s, system, "acme")
	if len(st) != 1 || st[0].GetSpec().GetEnd() != billingv1alpha1.StatementEnd_STATEMENT_END_ACCOUNT_DELETED ||
		st[0].GetSpec().GetType() != billingv1alpha1.BillingType_BILLING_TYPE_FIXED {
		t.Fatalf("statements %v, want one of the fixed fee to the deletion", st)
	}
	if obj, err := s.get(system, registry.BillingAccount, resource.Key{Name: "acme"}); err != nil || obj != nil {
		t.Errorf("the account stayed: %v, %v", obj, err)
	}
}

// TestPeriodsTheClocksRepeatAreEachBilled: on the night the clocks go back,
// two cuts an hour apart start at the same local minute; each is billed.
func TestPeriodsTheClocksRepeatAreEachBilled(t *testing.T) {
	t.Parallel()
	// 05:30Z is 01:30 EDT on November 1 2026, and 06:30Z is 01:30 EST.
	s, system, root, _ := billedAt(t, false, time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC))
	clock := fakeClock(s)
	anchor := time.Date(2026, 10, 15, 4, 0, 0, 0, time.UTC)
	for _, p := range []string{"a", "b", "c", "d"} {
		mustApply(t, s, root, registry.Plan, pricedPlan(p, "300"))
	}
	onPlan(t, s, root, "acme", "a")
	a := fixed("acme", anchor)
	a.Spec.TimeZone = "America/New_York"
	mustApply(t, s, root, registry.BillingAccount, a)
	reconcile(t, s, system)
	for _, step := range []struct {
		plan  string
		after time.Duration
	}{{"b", 0}, {"c", time.Hour}, {"d", 30 * time.Minute}} {
		clock.Advance(step.after)
		onPlan(t, s, root, "acme", step.plan)
		reconcile(t, s, system) // records the cut
		reconcile(t, s, system) // bills it
	}
	var billed time.Duration
	for _, st := range statementsOf(t, s, system, "acme") {
		billed += st.GetSpec().GetPeriodEndTime().AsTime().Sub(st.GetSpec().GetPeriodStartTime().AsTime())
	}
	if want := time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC).Sub(anchor); billed != want {
		t.Errorf("statements cover %s, want %s from the anchor to the last cut", billed, want)
	}
	if n := len(statementsOf(t, s, system, "acme")); n != 3 {
		t.Errorf("%d statements, want 3", n)
	}
}

// TestAFixedTenantDoesNotSeeItsListValue: what a fixed-fee tenant's holdings
// would cost is root's to read, not the tenant's.
func TestAFixedTenantDoesNotSeeItsListValue(t *testing.T) {
	t.Parallel()
	s, system, root, alice := billed(t)
	now := minuteNow(s)
	anchor := now.AddDate(0, -1, 0)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD", priceEntry(anchor, &billingv1alpha1.Prices{Disk: "1"})))
	mustApply(t, s, root, registry.Plan, pricedPlan("standard", "300"))
	onPlan(t, s, root, "acme", "standard")
	acct := fixed("acme", anchor)
	acct.Spec.PriceList = "std"
	mustApply(t, s, root, registry.BillingAccount, acct)
	name := statementName("acme", anchor, time.UTC)
	waitStatement(t, s, system, name)
	waitAccount(t, s, system, "valued", func(a *billingv1alpha1.BillingAccount) bool { return a.GetStatus().GetMetered() != "" })
	for who, ctx := range map[string]context.Context{"root": root, "alice": alice} {
		obj, err := s.backend(registry.BillingAccount).Get(ctx, resource.Key{Name: "acme"})
		if err != nil {
			t.Fatal(err)
		}
		st := obj.(*billingv1alpha1.BillingAccount).GetStatus()
		if seen := st.GetMetered() != "" || st.GetPeriodPriceList() != ""; seen != (who == "root") {
			t.Errorf("%s sees the account's list value: %t (%v)", who, seen, st)
		}
		obj, err = s.backend(registry.Statement).Get(ctx, resource.Key{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if seen := obj.(*billingv1alpha1.Statement).GetSpec().GetMetered() != ""; seen != (who == "root") {
			t.Errorf("%s sees the statement's list value: %t", who, seen)
		}
		list, err := s.backend(registry.BillingAccount).List(ctx, resource.ListOptions{})
		if err != nil || len(list.Items) != 1 || (list.Items[0].(*billingv1alpha1.BillingAccount).GetStatus().GetMetered() != "") != (who == "root") {
			t.Errorf("%s lists the account's list value: %v, %v", who, list.Items, err)
		}
	}
}

// TestANewLengthKeepsTheDay: periods of a new length start on the anchor's
// day, though a short month came between.
func TestANewLengthKeepsTheDay(t *testing.T) {
	t.Parallel()
	s, system, root, _ := billed(t)
	clock := fakeClock(s)
	anchor := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD"))
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 1, anchor))
	waitAccount(t, s, system, "to Nov 30", func(a *billingv1alpha1.BillingAccount) bool {
		return a.GetStatus().GetPeriodEndTime().AsTime().Equal(time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC))
	})
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 2, anchor))
	clock.Advance(16 * 24 * time.Hour)
	waitAccount(t, s, system, "from Nov 30 to Jan 31", func(a *billingv1alpha1.BillingAccount) bool {
		st := a.GetStatus()
		return st.GetPeriodStartTime().AsTime().Equal(time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)) &&
			st.GetPeriodEndTime().AsTime().Equal(time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC))
	})
}

// TestANewPriceRatesThePeriodAgain: a price list's change reaches what the
// open period has come to at once, as its watch brings it.
func TestANewPriceRatesThePeriodAgain(t *testing.T) {
	t.Parallel()
	s, system, root, _ := billed(t)
	now := minuteNow(s)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD", priceEntry(now.AddDate(0, -1, 0), &billingv1alpha1.Prices{Cpu: "1"})))
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 1, now.AddDate(0, 0, -1)))
	a := waitAccount(t, s, system, "accrued", func(a *billingv1alpha1.BillingAccount) bool { return a.GetStatus().GetAccruedTime() != nil })
	was := a.GetStatus().GetPriceListVersion()
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD", priceEntry(now.AddDate(0, -1, 0), &billingv1alpha1.Prices{Cpu: "1"}),
		priceEntry(now.Add(time.Hour), &billingv1alpha1.Prices{Cpu: "2"})))
	waitAccount(t, s, system, "rated at the new list", func(a *billingv1alpha1.BillingAccount) bool {
		return a.GetStatus().GetPriceListVersion() != was
	})
}

// TestTheMeterStopsAtTheDeletion: an account being deleted is not read.
func TestTheMeterStopsAtTheDeletion(t *testing.T) {
	t.Parallel()
	s, system, root, _ := billedWith(t, false)
	now := minuteNow(s)
	mustApply(t, s, root, registry.PriceList, priceList("std", "TWD"))
	mustApply(t, s, root, registry.BillingAccount, payg("acme", "std", 1, now.AddDate(0, 0, -1)))
	if _, err := s.backend(registry.BillingAccount).Delete(root, resource.Key{Name: "acme"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.meterOnce(system, now); err != nil {
		t.Fatal(err)
	}
	// The minute is still unread for acme.
	if !s.meterRead.first("acme", now) {
		t.Error("the meter read a tenant whose account is being deleted")
	}
}

// TestPlanAndAccountWritesDoNotCross: moving a tenant to a plan of no price
// and opening a FIXED account for it, at once, never both succeed.
func TestPlanAndAccountWritesDoNotCross(t *testing.T) {
	t.Parallel()
	s, _, root, _ := billedWith(t, false)
	now := minuteNow(s)
	mustApply(t, s, root, registry.Plan, pricedPlan("standard", "300"))
	mustApply(t, s, root, registry.Plan, &tenancyv1alpha1.Plan{Metadata: &metav1.ObjectMeta{Name: "free"}, Spec: &tenancyv1alpha1.PlanSpec{Limits: &tenancyv1alpha1.Limits{}}})
	for i := range 20 {
		name := fmt.Sprintf("race-%d", i)
		mustApply(t, s, root, registry.Tenant, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: name}, Spec: &tenancyv1alpha1.TenantSpec{Plan: "standard"}})
		errs := make(chan error, 2)
		start := make(chan struct{})
		go func() {
			<-start
			_, err := s.backend(registry.Tenant).Apply(root, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: name}, Spec: &tenancyv1alpha1.TenantSpec{Plan: "free"}})
			errs <- err
		}()
		go func() {
			<-start
			_, err := s.backend(registry.BillingAccount).Apply(root, fixed(name, now))
			errs <- err
		}()
		close(start)
		if e1, e2 := <-errs, <-errs; e1 == nil && e2 == nil {
			t.Fatalf("%s: a FIXED account opened as its tenant left for a plan of no price", name)
		}
	}
}

// TestBillingStartsOverAStoreWrittenOffline: an account written while no
// controller ran is billed for every period that ended, once they run.
func TestBillingStartsOverAStoreWrittenOffline(t *testing.T) {
	t.Parallel()
	db := filepath.Join(t.TempDir(), "hangar.db")
	configure := func(controllers bool) func(*Config) {
		return func(c *Config) {
			c.Clock = clockwork.NewFakeClockAt(billingEpoch)
			c.DisableControllers = !controllers
			c.Meter = c.Store.(store.Meter)
			c.MeterInterval = 1000 * time.Hour
		}
	}
	s, system, _, stop := stoppableAt(t, db, configure(false))
	root := asRoot(t, s, system)
	anchor := minuteNow(s).AddDate(0, -2, 0)
	fixedOn(t, s, root, "acme", "standard", "300", anchor)
	stop()
	s, ctx, _ := serve(t, db, configure(true))
	system = auth.WithPrincipal(ctx, auth.SystemPrincipal)
	for n := range 2 {
		waitStatement(t, s, system, statementName("acme", anchor.AddDate(0, n, 0), time.UTC))
	}
	waitSettled(t, s)
	if n := len(statementsOf(t, s, system, "acme")); n != 2 {
		t.Errorf("%d statements, want 2", n)
	}
}
