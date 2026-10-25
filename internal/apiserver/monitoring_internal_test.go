package apiserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"connectrpc.com/connect"

	monitoringv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/monitoring/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/auth"
)

// TestOverviewAsksItsQueriesOnly: the overview sends Prometheus each of its
// fixed queries, exactly as written, once, and nothing else.
func TestOverviewAsksItsQueriesOnly(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var asked []string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.URL.Path != "/api/v1/query" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		mu.Lock()
		asked = append(asked, r.Form.Get("query"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(prom.Close)
	s, _, _ := runningWith(t, func(c *Config) { c.Monitoring.PrometheusURL = prom.URL })
	local := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.Local, ID: "root@n1"})
	if _, err := (monitoring{s}).GetOverview(local, connect.NewRequest(&monitoringv1alpha1.GetOverviewRequest{})); err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(overviewQueries)
	slices.Sort(want)
	slices.Sort(asked)
	if !slices.Equal(asked, want) {
		t.Errorf("asked:\n%q\nwant:\n%q", asked, want)
	}
	if len(slices.Compact(slices.Clone(want))) != len(overviewQueries) {
		t.Errorf("overviewQueries names a query twice")
	}
}
