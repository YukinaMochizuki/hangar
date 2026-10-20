package front_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/YukinaMochizuki/hangar/internal/metrics/metricstest"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

func doorRegistry(d *door) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(d.front.Collectors(d.certs)...)
	return reg
}

// waitValue waits until the series of name with labels is want: a request
// is counted once its handler returns, which may be after its client has
// the answer.
func waitValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string, want float64) {
	t.Helper()
	testutil.Eventually(t, fmt.Sprintf("%s %v at %v", name, labels, want), func() (bool, string) {
		got, ok := metricstest.Value(t, reg, name, labels)
		return ok && got == want, fmt.Sprint(got, ok)
	})
}

// TestFrontMetricsNameNoHost: the front door counts its requests by route
// and status class, and its certificates by their names, and names no
// hostname nor address.
func TestFrontMetricsNameNoHost(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	o := defaultOptions()
	o.api = fakeAPI(release)
	o.consoleDir = consoleFiles(t)
	o.endpoints = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			http.Error(w, "the endpoint failed", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("endpoint"))
	})
	d := startDoor(t, newCA(t), o)
	reg := doorRegistry(d)

	whoAmI := "/hangar.iam.v1alpha1.AccountService/WhoAmI"
	for _, r := range []struct {
		method, host, path string
		code               int
	}{
		{http.MethodPost, apiHost, whoAmI, http.StatusOK},
		// The console's API paths are the control plane's.
		{http.MethodPost, consoleHost, whoAmI, http.StatusOK},
		{http.MethodGet, consoleHost, "/", http.StatusOK},
		{http.MethodGet, webHost, "/", http.StatusOK},
		{http.MethodGet, webHost, "/fail", http.StatusInternalServerError},
	} {
		if code, body, _ := do(t, d.client, r.method, "https://"+r.host+r.path, strings.NewReader("{}"), nil); code != r.code {
			t.Fatalf("%s %s%s: %d %s", r.method, r.host, r.path, code, body)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "wss://"+apiHost+"/v1/ws", &websocket.DialOptions{HTTPClient: d.client1})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Write(ctx, websocket.MessageBinary, []byte("ping"))
	_, _, _ = c.Read(ctx)
	_ = c.CloseNow()

	requests := "hangar_edge_requests_total"
	waitValue(t, reg, requests, map[string]string{"route": "control-plane", "status_class": "2xx"}, 2)
	waitValue(t, reg, requests, map[string]string{"route": "control-plane", "status_class": "1xx"}, 1)
	waitValue(t, reg, requests, map[string]string{"route": "console", "status_class": "2xx"}, 1)
	waitValue(t, reg, requests, map[string]string{"route": "endpoint", "status_class": "2xx"}, 1)
	waitValue(t, reg, requests, map[string]string{"route": "endpoint", "status_class": "5xx"}, 1)
	waitValue(t, reg, "hangar_edge_request_duration_seconds", map[string]string{"route": "control-plane"}, 3)
	waitValue(t, reg, "hangar_edge_upstream_errors_total", map[string]string{"route": "endpoint"}, 0)
	for _, cert := range []string{"api", "console", "wildcard_edge"} {
		v, ok := metricstest.Value(t, reg, "hangar_edge_certificate_expiry_timestamp_seconds", map[string]string{"cert": cert})
		if !ok || time.Unix(int64(v), 0).Before(time.Now()) {
			t.Errorf("certificate %s expires at %v (present %t), not in the future", cert, time.Unix(int64(v), 0), ok)
		}
	}

	metricstest.CheckLabels(t, reg, requests, "hangar_edge_request_duration_seconds", "hangar_edge_upstream_errors_total",
		"hangar_edge_certificate_expiry_timestamp_seconds")
	metricstest.CheckAbsent(t, reg, "web-demo", cpDomain, userDomain, "127.0.0.1")
}

// TestAnUnansweredAPIIsAnUpstreamError: the front door counts what it
// answers itself when the apiserver does not.
func TestAnUnansweredAPIIsAnUpstreamError(t *testing.T) {
	o := defaultOptions()
	o.api = nil
	d := startDoor(t, newCA(t), o)
	reg := doorRegistry(d)
	if code, body, _ := do(t, d.client, http.MethodGet, "https://"+apiHost+"/healthz", nil, nil); code != http.StatusBadGateway {
		t.Fatalf("GET with no apiserver: %d %s", code, body)
	}
	waitValue(t, reg, "hangar_edge_upstream_errors_total", map[string]string{"route": "control-plane"}, 1)
	waitValue(t, reg, "hangar_edge_requests_total", map[string]string{"route": "control-plane", "status_class": "5xx"}, 1)
}
