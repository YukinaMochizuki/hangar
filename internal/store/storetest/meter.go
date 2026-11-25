package storetest

import (
	"cmp"
	"slices"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/store"
)

// MeterStore is a store.Meter that can be closed.
type MeterStore interface {
	store.Meter
	Close() error
}

// RunMeter runs the store.Meter contract. open must return a new, empty
// store; RunMeter closes it.
func RunMeter(t *testing.T, open func(t *testing.T) MeterStore) {
	tests := []struct {
		name string
		fn   func(t *testing.T, s store.Meter)
	}{
		{"MeterSumsMinutes", testMeterSumsMinutes},
		{"MeterMinuteReplaced", testMeterMinuteReplaced},
		{"MeterTenantsApart", testMeterTenantsApart},
		{"MeterSpanHalfOpen", testMeterSpanHalfOpen},
		{"MeterFoldKeepsSums", testMeterFoldKeepsSums},
		{"MeterFoldLeavesPartHours", testMeterFoldLeavesPartHours},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := open(t)
			t.Cleanup(func() { _ = s.Close() })
			tt.fn(t, s)
		})
	}
}

// meterStart is the first minute of an hour.
var meterStart = time.Unix(1_800_000_000, 0).Truncate(time.Hour)

func minuteAt(n int) time.Time { return meterStart.Add(time.Duration(n) * time.Minute) }

func reading(uid, item string, q int64) store.MeterReading {
	return store.MeterReading{UID: uid, Item: item, Quantity: q, Kind: "Container", Project: "web", Name: "n-" + uid}
}

func record(t *testing.T, s store.Meter, tenant string, minute time.Time, rs ...store.MeterReading) {
	t.Helper()
	if err := s.RecordMeter(t.Context(), tenant, minute, rs); err != nil {
		t.Fatal(err)
	}
}

// sums is what SumMeter answers, as uid/item to quantity.
func sums(t *testing.T, s store.Meter, tenant string, from, to time.Time) map[string]int64 {
	t.Helper()
	totals, err := s.SumMeter(t.Context(), tenant, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.IsSortedFunc(totals, func(a, b store.MeterTotal) int {
		return cmp.Or(cmp.Compare(a.UID, b.UID), cmp.Compare(a.Item, b.Item))
	}) {
		t.Errorf("totals not ordered by uid and item: %v", totals)
	}
	out := map[string]int64{}
	for _, m := range totals {
		if m.Kind != "Container" || m.Project != "web" || m.Name != "n-"+m.UID {
			t.Errorf("total %v does not say what its object was", m)
		}
		out[m.UID+"/"+m.Item] = m.Quantity
	}
	return out
}

func sameSums(t *testing.T, got, want map[string]int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("sums = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("sums = %v, want %v", got, want)
		}
	}
}

func testMeterSumsMinutes(t *testing.T, s store.Meter) {
	for i := range 3 {
		record(t, s, "acme", minuteAt(i), reading("a", "cpu", 2000), reading("a", "memory", 4096), reading("b", "disk", 10))
	}
	sameSums(t, sums(t, s, "acme", minuteAt(0), minuteAt(3)), map[string]int64{"a/cpu": 6000, "a/memory": 3 * 4096, "b/disk": 30})
}

func testMeterMinuteReplaced(t *testing.T, s store.Meter) {
	// A second reading of a minute is all the minute held: what the first
	// had and it lacks is gone, so a machine stopped in the minute's last
	// reading is not counted twice or kept.
	record(t, s, "acme", minuteAt(0), reading("a", "cpu", 2000), reading("b", "disk", 10))
	record(t, s, "acme", minuteAt(0).Add(30*time.Second), reading("b", "disk", 12))
	sameSums(t, sums(t, s, "acme", minuteAt(0), minuteAt(1)), map[string]int64{"b/disk": 12})
}

func testMeterTenantsApart(t *testing.T, s store.Meter) {
	record(t, s, "acme", minuteAt(0), reading("a", "cpu", 1000))
	record(t, s, "other", minuteAt(0), reading("x", "cpu", 3000))
	// Replacing one tenant's minute leaves another's.
	record(t, s, "acme", minuteAt(0))
	sameSums(t, sums(t, s, "acme", minuteAt(0), minuteAt(1)), map[string]int64{})
	sameSums(t, sums(t, s, "other", minuteAt(0), minuteAt(1)), map[string]int64{"x/cpu": 3000})
}

func testMeterSpanHalfOpen(t *testing.T, s store.Meter) {
	for i := range 5 {
		record(t, s, "acme", minuteAt(i), reading("a", "cpu", 1))
	}
	sameSums(t, sums(t, s, "acme", minuteAt(1), minuteAt(3)), map[string]int64{"a/cpu": 2})
	// Times within a minute count as its start.
	sameSums(t, sums(t, s, "acme", minuteAt(1).Add(59*time.Second), minuteAt(3).Add(59*time.Second)), map[string]int64{"a/cpu": 2})
}

func testMeterFoldKeepsSums(t *testing.T, s store.Meter) {
	for i := range 150 {
		record(t, s, "acme", minuteAt(i), reading("a", "cpu", 1000), reading("b", "disk", 2))
	}
	record(t, s, "other", minuteAt(0), reading("x", "cpu", 1))
	before := sums(t, s, "acme", minuteAt(0), minuteAt(150))
	if err := s.FoldMeter(t.Context(), "acme", minuteAt(0), minuteAt(120)); err != nil {
		t.Fatal(err)
	}
	sameSums(t, sums(t, s, "acme", minuteAt(0), minuteAt(150)), before)
	// Whole folded hours count where their first minute does.
	sameSums(t, sums(t, s, "acme", minuteAt(60), minuteAt(120)), map[string]int64{"a/cpu": 60_000, "b/disk": 120})
	// The minutes after the fold are still minutes.
	sameSums(t, sums(t, s, "acme", minuteAt(121), minuteAt(150)), map[string]int64{"a/cpu": 29_000, "b/disk": 58})
	// Another tenant's minutes are not folded.
	sameSums(t, sums(t, s, "other", minuteAt(0), minuteAt(1)), map[string]int64{"x/cpu": 1})
}

func testMeterFoldLeavesPartHours(t *testing.T, s store.Meter) {
	// Spans that end inside an hour, as periods cut by a change do, fold
	// only the hours wholly theirs, so each reads back exactly after.
	for i := range 180 {
		record(t, s, "acme", minuteAt(i), reading("a", "cpu", 1))
	}
	for _, span := range [][2]int{{0, 30}, {30, 150}, {150, 180}} {
		if err := s.FoldMeter(t.Context(), "acme", minuteAt(span[0]), minuteAt(span[1])); err != nil {
			t.Fatal(err)
		}
	}
	for _, span := range [][2]int{{0, 30}, {30, 150}, {150, 180}, {0, 180}} {
		sameSums(t, sums(t, s, "acme", minuteAt(span[0]), minuteAt(span[1])), map[string]int64{"a/cpu": int64(span[1] - span[0])})
	}
	// Folding again changes nothing.
	if err := s.FoldMeter(t.Context(), "acme", minuteAt(30), minuteAt(150)); err != nil {
		t.Fatal(err)
	}
	sameSums(t, sums(t, s, "acme", minuteAt(30), minuteAt(150)), map[string]int64{"a/cpu": 120})
}
