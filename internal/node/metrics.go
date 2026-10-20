package node

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

// agentMetrics are the agent's metrics (docs/decisions/monitoring.md). None
// is labelled with a machine, a network or an image: they are the node's.
type agentMetrics struct {
	tunnelUp          prometheus.Gauge
	tunnelReconnects  prometheus.Counter
	heartbeatFailures prometheus.Counter
	statusFailures    *prometheus.CounterVec
	pulls             *prometheus.CounterVec
	imageCache        prometheus.Gauge
	// tunnelOpened says a tunnel was opened before, so the next is a
	// reconnect. Only the tunnel's one goroutine reads and writes it.
	tunnelOpened bool
}

// Results of preparing a container image, for pulls.
const (
	pullResultOK           = "ok"
	pullResultFailed       = "failed"
	pullResultUnauthorized = "unauthorized"
	pullResultTimeout      = "timeout"
	pullResultInvalid      = "invalid"
	pullResultDiskFailed   = "disk_failed"
	pullResultCanceled     = "canceled"
)

func newAgentMetrics() *agentMetrics {
	m := &agentMetrics{
		tunnelUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "hangar_node_tunnel_connected",
			Help: "1 while the tunnel to the apiserver, through which it calls the node, is open; 0 otherwise.",
		}),
		tunnelReconnects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "hangar_node_tunnel_reconnects_total",
			Help: "Tunnels to the apiserver opened again after the first.",
		}),
		heartbeatFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "hangar_node_heartbeat_failures_total",
			Help: "Reports of the Node, its heartbeat, that failed.",
		}),
		statusFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_node_status_report_failures_total",
			Help: "Writes of a machine's, a disk's or an image's status that failed, by Connect code; aborted is a write that lost a race and is tried again.",
		}, []string{"code"}),
		pulls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_node_image_pulls_total",
			Help: "Container images prepared, fetched and unpacked, by how it ended.",
		}, []string{"result"}),
		imageCache: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "hangar_node_image_cache_bytes",
			Help: "Bytes the container image cache's blobs take, as of the last sweep.",
		}),
	}
	for _, r := range []string{pullResultOK, pullResultFailed, pullResultUnauthorized, pullResultTimeout, pullResultInvalid, pullResultDiskFailed, pullResultCanceled} {
		m.pulls.WithLabelValues(r)
	}
	return m
}

// Collectors are the agent's metrics, for the registry its metrics
// listener serves.
func (a *Agent) Collectors() []prometheus.Collector {
	m := a.metrics
	return []prometheus.Collector{m.tunnelUp, m.tunnelReconnects, m.heartbeatFailures, m.statusFailures, m.pulls, m.imageCache}
}

func (m *agentMetrics) tunnelOpen() {
	if m.tunnelOpened {
		m.tunnelReconnects.Inc()
	}
	m.tunnelOpened = true
	m.tunnelUp.Set(1)
}

func (m *agentMetrics) tunnelClosed() { m.tunnelUp.Set(0) }

// statusWritten counts err, a status write's, if it failed.
func (m *agentMetrics) statusWritten(err error) error {
	if err != nil {
		m.statusFailures.WithLabelValues(connect.CodeOf(err).String()).Inc()
	}
	return err
}

// pulled counts how preparing an image ended, err being what the puller
// got and ctx what it ran under.
func (m *agentMetrics) pulled(ctx context.Context, err error) {
	m.pulls.WithLabelValues(pullResult(ctx, err)).Inc()
}

func pullResult(ctx context.Context, err error) string {
	var s *stuck
	switch {
	case err == nil:
		return pullResultOK
	case ctx.Err() != nil:
		return pullResultCanceled
	case isFault(err):
		return pullResultInvalid
	case timedOut(err):
		return pullResultTimeout
	case errors.As(err, &s) && s.reason == computeapi.ReasonImagePullUnauthorized:
		return pullResultUnauthorized
	case errors.As(err, &s) && s.reason == computeapi.ReasonDiskFailed:
		return pullResultDiskFailed
	}
	return pullResultFailed
}
