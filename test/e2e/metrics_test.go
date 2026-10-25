package e2e

import (
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/metrics/metricstest"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// TestNodeMetricsNameNoMachine runs the node agent's metrics over machines
// whose names and images carry a marker, and finds the marker nowhere in
// what its metrics listener serves.
func TestNodeMetricsNameNoMachine(t *testing.T) {
	t.Parallel()
	const marker = "qz7"
	w := newWorld(t)
	img := nginx
	img.Size = 3 * mib
	w.host.AddImage("app-"+marker, "sha256:app-"+marker, img)
	w.apply(container("web-"+marker, "app-"+marker))
	w.waitReason("web-"+marker, computeapi.ReasonRunning)
	w.apply(container("bad-"+marker, "missing-"+marker))
	w.waitReason("bad-"+marker, computeapi.ReasonImagePullFailed)
	uid := w.machine("web-" + marker).GetMetadata().GetUid()
	w.sweeps(1)

	reg := prometheus.NewRegistry()
	reg.MustRegister(w.agent.Collectors()...)
	value := func(name string, labels map[string]string) float64 {
		t.Helper()
		v, ok := metricstest.Value(t, reg, name, labels)
		if !ok {
			t.Errorf("no %s %v", name, labels)
		}
		return v
	}
	none := map[string]string{}
	testutil.Eventually(t, "the tunnel open", func() (bool, string) {
		v := value("hangar_node_tunnel_connected", none)
		return v == 1, fmt.Sprint(v)
	})

	metricstest.CheckLabels(t, reg, "hangar_node_tunnel_connected", "hangar_node_tunnel_reconnects_total",
		"hangar_node_heartbeat_failures_total", "hangar_node_image_pulls_total", "hangar_node_image_cache_bytes")
	metricstest.CheckAbsent(t, reg, marker, uid, apiservertest.Tenant)

	if v := value("hangar_node_image_pulls_total", map[string]string{"result": "ok"}); v < 1 {
		t.Errorf("%v images pulled, want at least the running container's", v)
	}
	if v := value("hangar_node_image_pulls_total", map[string]string{"result": "failed"}); v < 1 {
		t.Errorf("%v pulls failed, want at least the missing image's", v)
	}
	if v := value("hangar_node_image_cache_bytes", none); v != float64(img.Size) {
		t.Errorf("the image cache takes %v bytes, want the one image's %d", v, img.Size)
	}
	if v := value("hangar_node_heartbeat_failures_total", none); v != 0 {
		t.Errorf("%v heartbeats failed", v)
	}
	if v := value("hangar_node_tunnel_reconnects_total", none); v != 0 {
		t.Errorf("%v reconnects of a tunnel that never dropped", v)
	}
}
