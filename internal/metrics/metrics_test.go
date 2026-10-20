package metrics_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/YukinaMochizuki/hangar/internal/metrics"
	"github.com/YukinaMochizuki/hangar/internal/metrics/metricstest"
)

func TestMetricsListenOnLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:9101", "127.0.0.2:0", "[::1]:9101"} {
		if err := metrics.CheckAddr(addr); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:9101", ":9101", "[::]:9101", "localhost:9101", "192.0.2.1:9101", "[2001:db8::1]:9101", "127.0.0.1"} {
		if err := metrics.CheckAddr(addr); err == nil {
			t.Errorf("%s was taken", addr)
		}
		if ln, err := metrics.Listen(t.Context(), addr); err == nil {
			_ = ln.Close()
			t.Errorf("listening on %s was allowed", addr)
		}
	}
}

func TestTheListenerServesOnlyMetrics(t *testing.T) {
	reg := metrics.NewRegistry()
	ln, err := metrics.Listen(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- metrics.Serve(ctx, ln, reg, slog.New(slog.DiscardHandler)) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

	base := "http://" + ln.Addr().String()
	status, body := get(t, http.MethodGet, base+metrics.Path)
	if status != http.StatusOK || !strings.Contains(body, "go_goroutines") {
		t.Errorf("GET %s: %d\n%s", metrics.Path, status, body)
	}
	for _, path := range []string{"/", "/healthz", "/metrics/x", "/hangar.compute.v1alpha1.MachineService/ListMachines"} {
		if status, _ := get(t, http.MethodGet, base+path); status != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", path, status)
		}
	}
	if status, _ := get(t, http.MethodPost, base+metrics.Path); status != http.StatusMethodNotAllowed {
		t.Errorf("POST %s: %d, want 405", metrics.Path, status)
	}
}

func get(t *testing.T, method, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// recorder is a testing.TB that keeps what fails it, to test the helpers.
type recorder struct {
	testing.TB
	failures []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func TestCheckLabelsRefusesALabelOffTheList(t *testing.T) {
	reg := prometheus.NewRegistry()
	ok := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_ok_total", Help: "h"}, []string{"procedure", "code"})
	bad := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "test_bad", Help: "h"}, []string{"phase", "tenant"})
	unused := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "test_unused", Help: "h"}, []string{"phase"})
	reg.MustRegister(ok, bad, unused)
	ok.WithLabelValues("/p", "ok").Inc()
	bad.WithLabelValues("Running", "acme").Set(1)

	r := &recorder{TB: t}
	metricstest.CheckLabels(r, reg, "test_ok_total", "test_unused")
	want := []string{`metric test_bad has label "tenant"`, "metric test_unused has no series"}
	if len(r.failures) != len(want) {
		t.Fatalf("failures: %q, want %q", r.failures, want)
	}
	for i, w := range want {
		if !strings.HasPrefix(r.failures[i], w) {
			t.Errorf("failure %q, want %q", r.failures[i], w)
		}
	}
}

func TestCheckAbsentFindsANameAnywhere(t *testing.T) {
	reg := prometheus.NewRegistry()
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "test_machines", Help: "h"}, []string{"phase"})
	reg.MustRegister(g)
	g.WithLabelValues("acme-web-1").Set(1)

	r := &recorder{TB: t}
	metricstest.CheckAbsent(r, reg, "acme-web-1", "globex")
	if len(r.failures) != 1 || !strings.Contains(r.failures[0], `"acme-web-1"`) {
		t.Errorf("failures: %q, want one naming acme-web-1", r.failures)
	}
}
