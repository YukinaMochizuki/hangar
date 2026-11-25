package billing_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	billingv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/billing/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/billing"
)

func TestParseMoney(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want billing.Micros
		ok   bool
	}{
		{"0", 0, true},
		{"300", 300_000_000, true},
		{"0.6", 600_000, true},
		{"0.000001", 1, true},
		{"12.345678", 12_345_678, true},
		{"0.0000001", 0, false},
		{"-1", 0, false},
		{"1.", 0, false},
		{".5", 0, false},
		{"1e3", 0, false},
		{"", 0, false},
		{"99999999999999", 0, false},
	} {
		got, err := billing.ParseMoney(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("ParseMoney(%q) = %d, %v; want %d, ok %v", tt.in, got, err, tt.want, tt.ok)
		}
	}
}

func TestMicrosString(t *testing.T) {
	for m, want := range map[billing.Micros]string{0: "0", 300_000_000: "300", 600_000: "0.6", 1: "0.000001", 12_500_000: "12.5", -2_500_000: "-2.5"} {
		if got := m.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int64(m), got, want)
		}
	}
}

func TestRoundToMinor(t *testing.T) {
	for _, tt := range []struct {
		m        billing.Micros
		currency string
		want     billing.Micros
	}{
		{1_234_567, "TWD", 1_230_000},
		{1_235_000, "TWD", 1_240_000},
		{1_234_999, "USD", 1_230_000},
		{1_500_000, "JPY", 2_000_000},
		{1_499_999, "JPY", 1_000_000},
		{1_234_500, "KWD", 1_235_000},
	} {
		if got := billing.RoundToMinor(tt.m, tt.currency); got != tt.want {
			t.Errorf("RoundToMinor(%d, %s) = %d, want %d", tt.m, tt.currency, got, tt.want)
		}
	}
}

func date(y int, m time.Month, d, h, min int, loc *time.Location) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, loc)
}

func TestPeriodsClampToMonthEnd(t *testing.T) {
	p := billing.Periods{Anchor: date(2026, 1, 31, 0, 0, time.UTC), Months: 1, Zone: time.UTC}
	want := []time.Time{
		date(2026, 1, 31, 0, 0, time.UTC), date(2026, 2, 28, 0, 0, time.UTC), date(2026, 3, 31, 0, 0, time.UTC),
		date(2026, 4, 30, 0, 0, time.UTC), date(2027, 1, 31, 0, 0, time.UTC),
	}
	for i, n := range []int{0, 1, 2, 3, 12} {
		if got := p.Start(n); !got.Equal(want[i]) {
			t.Errorf("Start(%d) = %s, want %s", n, got, want[i])
		}
	}
	// A leap year's February has a 29th.
	if got := p.Start(25); !got.Equal(date(2028, 2, 29, 0, 0, time.UTC)) {
		t.Errorf("Start(25) = %s, want 2028-02-29", got)
	}
}

func TestPeriodsManyMonths(t *testing.T) {
	p := billing.Periods{Anchor: date(2026, 11, 15, 9, 30, time.UTC), Months: 6, Zone: time.UTC}
	if got := p.Start(1); !got.Equal(date(2027, 5, 15, 9, 30, time.UTC)) {
		t.Errorf("Start(1) = %s", got)
	}
	if got := p.Start(2); !got.Equal(date(2027, 11, 15, 9, 30, time.UTC)) {
		t.Errorf("Start(2) = %s", got)
	}
}

// TestPeriodsKeepTheirDayAcrossALengthChange: periods cut from an offset of
// the same anchor keep its day, though a short month came between.
func TestPeriodsKeepTheirDayAcrossALengthChange(t *testing.T) {
	anchor := date(2027, 1, 31, 0, 0, time.UTC)
	monthly := billing.Periods{Anchor: anchor, Months: 1, Zone: time.UTC}
	if got := monthly.Start(1); !got.Equal(date(2027, 2, 28, 0, 0, time.UTC)) {
		t.Fatalf("Start(1) = %s", got)
	}
	// From the second period on, two months each.
	two := billing.Periods{Anchor: anchor, Months: 2, Offset: 1, Zone: time.UTC}
	for n, want := range []time.Time{date(2027, 2, 28, 0, 0, time.UTC), date(2027, 4, 30, 0, 0, time.UTC), date(2027, 6, 30, 0, 0, time.UTC)} {
		if got := two.Start(n); !got.Equal(want) {
			t.Errorf("Start(%d) = %s, want %s", n, got, want)
		}
	}
	if n, start, end, ok := two.At(date(2027, 3, 31, 0, 0, time.UTC)); !ok || n != 0 || !start.Equal(date(2027, 2, 28, 0, 0, time.UTC)) || !end.Equal(date(2027, 4, 30, 0, 0, time.UTC)) {
		t.Errorf("At(Mar 31) = %d %s %s %v", n, start, end, ok)
	}
	if _, _, _, ok := two.At(date(2027, 2, 27, 0, 0, time.UTC)); ok {
		t.Error("a time before the first period is in one")
	}
	// No length is one month, not a period of no time.
	none := billing.Periods{Anchor: anchor, Zone: time.UTC}
	if _, _, end, ok := none.At(anchor); !ok || !end.Equal(date(2027, 2, 28, 0, 0, time.UTC)) {
		t.Errorf("a period of no length ends %s, %v", end, ok)
	}
}

func TestPeriodsAt(t *testing.T) {
	p := billing.Periods{Anchor: date(2026, 1, 31, 12, 0, time.UTC), Months: 1, Zone: time.UTC}
	for _, tt := range []struct {
		at         time.Time
		n          int
		start, end time.Time
	}{
		{date(2026, 1, 31, 12, 0, time.UTC), 0, date(2026, 1, 31, 12, 0, time.UTC), date(2026, 2, 28, 12, 0, time.UTC)},
		// Before the time of day on the period's day is still the period before.
		{date(2026, 2, 28, 11, 59, time.UTC), 0, date(2026, 1, 31, 12, 0, time.UTC), date(2026, 2, 28, 12, 0, time.UTC)},
		{date(2026, 2, 28, 12, 0, time.UTC), 1, date(2026, 2, 28, 12, 0, time.UTC), date(2026, 3, 31, 12, 0, time.UTC)},
		// March 1st to 30th is in the period from February 28.
		{date(2026, 3, 15, 0, 0, time.UTC), 1, date(2026, 2, 28, 12, 0, time.UTC), date(2026, 3, 31, 12, 0, time.UTC)},
		{date(2027, 1, 1, 0, 0, time.UTC), 11, date(2026, 12, 31, 12, 0, time.UTC), date(2027, 1, 31, 12, 0, time.UTC)},
	} {
		n, start, end, ok := p.At(tt.at)
		if !ok || n != tt.n || !start.Equal(tt.start) || !end.Equal(tt.end) {
			t.Errorf("At(%s) = %d, %s, %s, %v; want %d, %s, %s", tt.at, n, start, end, ok, tt.n, tt.start, tt.end)
		}
	}
	if _, _, _, ok := p.At(date(2026, 1, 31, 11, 59, time.UTC)); ok {
		t.Error("a time before the anchor is in a period")
	}
}

func TestPeriodsCutInTheirZone(t *testing.T) {
	taipei, err := billing.LoadZone("Asia/Taipei")
	if err != nil {
		t.Fatal(err)
	}
	// Midnight of the 1st in Taipei is the 30th or 31st in UTC; the periods
	// still start on the 1st there, never drifting to February's 28th.
	p := billing.Periods{Anchor: date(2026, 12, 1, 0, 0, taipei), Months: 1, Zone: taipei}
	for n, want := range []time.Time{date(2026, 12, 1, 0, 0, taipei), date(2027, 1, 1, 0, 0, taipei), date(2027, 2, 1, 0, 0, taipei), date(2027, 3, 1, 0, 0, taipei)} {
		if got := p.Start(n); !got.Equal(want) {
			t.Errorf("Start(%d) = %s, want %s", n, got, want)
		}
	}
	if _, err := billing.LoadZone("Not/AZone"); err == nil {
		t.Error("an unknown zone loaded")
	}
}

func entries(t *testing.T, es ...*billingv1alpha1.PriceEntry) []billing.Entry {
	t.Helper()
	out, err := billing.ParseEntries(es)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func entry(at time.Time, p *billingv1alpha1.Prices) *billingv1alpha1.PriceEntry {
	return &billingv1alpha1.PriceEntry{EffectiveTime: timestamppb.New(at), Prices: p}
}

func TestParseEntriesRefuses(t *testing.T) {
	at := date(2026, 11, 1, 0, 0, time.UTC)
	for name, es := range map[string][]*billingv1alpha1.PriceEntry{
		"no time":          {{Prices: &billingv1alpha1.Prices{}}},
		"not on a minute":  {entry(at.Add(time.Second), nil)},
		"out of order":     {entry(at, nil), entry(at.Add(-time.Minute), nil)},
		"the same time":    {entry(at, nil), entry(at, nil)},
		"a bad price":      {entry(at, &billingv1alpha1.Prices{Cpu: "-1"})},
		"too fine a price": {entry(at, &billingv1alpha1.Prices{Disk: "0.0000001"})},
	} {
		if _, err := billing.ParseEntries(es); err == nil {
			t.Errorf("%s: entries accepted", name)
		}
	}
}

func TestSpans(t *testing.T) {
	t0 := date(2026, 11, 1, 0, 0, time.UTC)
	es := entries(t, entry(t0.Add(time.Hour), &billingv1alpha1.Prices{Cpu: "1"}), entry(t0.Add(3*time.Hour), &billingv1alpha1.Prices{Cpu: "2"}))
	spans := billing.Spans(es, t0, t0.Add(4*time.Hour))
	if len(spans) != 3 {
		t.Fatalf("spans = %+v", spans)
	}
	if spans[0].Entry != nil || !spans[0].To.Equal(t0.Add(time.Hour)) {
		t.Errorf("before the first entry: %+v", spans[0])
	}
	if spans[1].Entry != &es[0] || !spans[1].From.Equal(t0.Add(time.Hour)) || !spans[1].To.Equal(t0.Add(3*time.Hour)) {
		t.Errorf("first entry: %+v", spans[1])
	}
	if spans[2].Entry != &es[1] || !spans[2].To.Equal(t0.Add(4*time.Hour)) {
		t.Errorf("second entry: %+v", spans[2])
	}
	// A period inside one entry is one span of it.
	if got := billing.Spans(es, t0.Add(90*time.Minute), t0.Add(2*time.Hour)); len(got) != 1 || got[0].Entry != &es[0] {
		t.Errorf("spans inside an entry = %+v", got)
	}
}

func TestRate(t *testing.T) {
	t0 := date(2026, 11, 1, 0, 0, time.UTC)
	es := entries(t, entry(t0, &billingv1alpha1.Prices{Cpu: "0.6", Memory: "0.2", Disk: "0.005"}))
	span := billing.Spans(es, t0, t0.Add(24*time.Hour))[0]
	// 2 vCPUs, 4Gi and 20Gi for a day, as the meter sums them: millicore-,
	// MiB- and GiB-minutes. Addresses are held but free.
	day := int64(24 * 60)
	lines := billing.Rate(span, map[string]int64{"cpu": 2000 * day, "memory": 4096 * day, "disk": 20 * day, "addresses": day})
	want := map[string]struct{ quantity, amount string }{
		"cpu":    {"48", "28.8"},
		"memory": {"96", "19.2"},
		"disk":   {"480", "2.4"},
	}
	if len(lines) != len(want) {
		t.Fatalf("lines = %+v", lines)
	}
	for _, l := range lines {
		w := want[l.Item]
		if l.Quantity.String() != w.quantity || l.Amount.String() != w.amount || !l.Effective.Equal(t0) {
			t.Errorf("line %s = %s for %s, want %s for %s", l.Item, l.Quantity, l.Amount, w.quantity, w.amount)
		}
	}
	if got := billing.Total(lines, "TWD"); got.String() != "50.4" {
		t.Errorf("total = %s, want 50.4", got)
	}
	// A minute is a sixtieth of an hour's price, rounded to a micro-unit
	// per line, and the total to the currency's minor unit.
	one := billing.Rate(span, map[string]int64{"cpu": 1000})
	if len(one) != 1 || one[0].Amount != 10_000 {
		t.Errorf("one vCPU-minute = %+v, want 0.01", one)
	}
	if got := billing.Rate(billing.Span{From: t0, To: t0.Add(time.Hour)}, map[string]int64{"cpu": 1000}); got != nil {
		t.Errorf("a span before any price = %+v", got)
	}
}

func TestFeeLine(t *testing.T) {
	fee, _ := billing.ParseMoney("300")
	month := 30 * 24 * time.Hour
	if l := billing.FeeLine(fee, month, month); l.Amount != fee || l.Quantity.String() != "1" {
		t.Errorf("a whole period = %+v", l)
	}
	if l := billing.FeeLine(fee, 10*24*time.Hour, month); l.Amount.String() != "100" || l.Quantity.String() != "0.333333" {
		t.Errorf("a third = %s for %s", l.Quantity, l.Amount)
	}
}

func TestPromotionLines(t *testing.T) {
	until := date(2027, 1, 1, 0, 0, time.UTC)
	half := func(time.Time) billing.Micros { return 100_000_000 }
	// 100% of the part before its end: 100 of 300 off.
	lines := billing.PromotionLines([]billing.Promotion{{Label: "Early access", Percent: 100, Until: until}}, 300_000_000, half)
	if len(lines) != 1 || lines[0].Amount != -100_000_000 || lines[0].Item != "promotion" || lines[0].Description != "Early access" {
		t.Fatalf("lines %+v", lines)
	}
	// Together they take no more than what is billed.
	all := func(time.Time) billing.Micros { return 300_000_000 }
	lines = billing.PromotionLines([]billing.Promotion{{Label: "a", Percent: 80, Until: until}, {Label: "b", Percent: 50, Until: until}}, 300_000_000, all)
	var off billing.Micros
	for _, l := range lines {
		off += l.Amount
	}
	if off != -300_000_000 || len(lines) != 2 {
		t.Errorf("two promotions took %s, want all 300 and no more: %+v", off, lines)
	}
	// One that takes nothing has no line.
	if lines := billing.PromotionLines([]billing.Promotion{{Label: "a", Percent: 50, Until: until}}, 300_000_000, func(time.Time) billing.Micros { return 0 }); len(lines) != 0 {
		t.Errorf("a promotion over nothing: %+v", lines)
	}
}
