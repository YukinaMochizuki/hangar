// Package metricstest holds a binary's metrics to the label rule of
// docs/decisions/monitoring.md, and reads them back for tests. A metric
// with no series yet is not gathered, so a test exercises every metric
// before it checks them.
package metricstest

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/YukinaMochizuki/hangar/internal/metrics"
)

// CheckLabels fails t for every label of every gathered metric whose name
// is not in metrics.Labels, and for every family in want that g has no
// series of.
func CheckLabels(t testing.TB, g prometheus.Gatherer, want ...string) {
	t.Helper()
	families := gather(t, g)
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
		bad := map[string]bool{}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if !slices.Contains(metrics.Labels, l.GetName()) {
					bad[l.GetName()] = true
				}
			}
		}
		for name := range bad {
			t.Errorf("metric %s has label %q, which is not in metrics.Labels", f.GetName(), name)
		}
	}
	for _, name := range want {
		if !seen[name] {
			t.Errorf("metric %s has no series, so its labels went unchecked", name)
		}
	}
}

// CheckAbsent fails t for every one of names that appears anywhere in what
// the metrics listener serves of g.
func CheckAbsent(t testing.TB, g prometheus.Gatherer, names ...string) {
	t.Helper()
	text := Scrape(t, g)
	for _, n := range names {
		if strings.Contains(text, n) {
			t.Errorf("the metrics carry %q:\n%s", n, matching(text, n))
		}
	}
}

// Scrape is what the metrics listener serves of g.
func Scrape(t testing.TB, g prometheus.Gatherer) string {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Handler(g).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, metrics.Path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", metrics.Path, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// Value is the value of the series of family name with exactly labels, and
// whether g has it. A counter's, a gauge's and an untyped metric's value is
// its own; a histogram's is its count of observations.
func Value(t testing.TB, g prometheus.Gatherer, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	for _, f := range gather(t, g) {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if !sameLabels(m.GetLabel(), labels) {
				continue
			}
			switch {
			case m.Counter != nil:
				return m.GetCounter().GetValue(), true
			case m.Gauge != nil:
				return m.GetGauge().GetValue(), true
			case m.Histogram != nil:
				return float64(m.GetHistogram().GetSampleCount()), true
			case m.Untyped != nil:
				return m.GetUntyped().GetValue(), true
			}
		}
	}
	return 0, false
}

func gather(t testing.TB, g prometheus.Gatherer) []*dto.MetricFamily {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	return families
}

func sameLabels(got []*dto.LabelPair, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, l := range got {
		if v, ok := want[l.GetName()]; !ok || v != l.GetValue() {
			return false
		}
	}
	return true
}

func matching(text, s string) string {
	var lines []string
	for l := range strings.SplitSeq(text, "\n") {
		if strings.Contains(l, s) {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}
