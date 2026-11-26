package node

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/squeeze"
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
	squeezes          *prometheus.CounterVec
	// balloonUnresponsive counts the balloons found not following what
	// they are asked.
	balloonUnresponsive prometheus.Counter
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
		squeezes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_node_memory_pool_squeezes_total",
			Help: "Polls of a memory pool that changed its machines' balloons, by result: safe and reservation inflated them, to what their guests use and a margin or to their reservations; deflate gave memory back.",
		}, []string{"result"}),
		balloonUnresponsive: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "hangar_node_memory_pool_balloons_unresponsive_total",
			Help: "Balloons of memory pools' machines found not following what they are asked, as a guest with no balloon driver: asked nothing more until the VM starts again.",
		}),
	}
	for _, r := range []string{pullResultOK, pullResultFailed, pullResultUnauthorized, pullResultTimeout, pullResultInvalid, pullResultDiskFailed, pullResultCanceled} {
		m.pulls.WithLabelValues(r)
	}
	for _, s := range []squeeze.Stage{squeeze.Safe, squeeze.Reserve, squeeze.Deflate} {
		m.squeezes.WithLabelValues(s.String())
	}
	return m
}

// squeezed counts a poll of a memory pool that changed its balloons.
func (m *agentMetrics) squeezed(s squeeze.Stage) { m.squeezes.WithLabelValues(s.String()).Inc() }

// oomKills reads the VMs the host's OOM killer killed whole, from the
// hypervisor driver as it is scraped: the host counts them, so a restart
// of the agent loses none. A scrape it fails gives no sample.
type oomKills struct {
	desc *prometheus.Desc
	read func(context.Context) (uint64, error)
}

func newOOMKills(read func(context.Context) (uint64, error)) oomKills {
	return oomKills{
		desc: prometheus.NewDesc("hangar_node_vm_oom_group_kills_total",
			"VMs the host's OOM killer killed whole since the host booted, as when a memory pool ran out.", nil, nil),
		read: read,
	}
}

func (o oomKills) Describe(ch chan<- *prometheus.Desc) { ch <- o.desc }

func (o oomKills) Collect(ch chan<- prometheus.Metric) {
	n, err := o.read(context.Background())
	if err != nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(o.desc, prometheus.CounterValue, float64(n))
}

// Collectors are the agent's metrics, for the registry its metrics
// listener serves.
func (a *Agent) Collectors() []prometheus.Collector {
	m := a.metrics
	cs := []prometheus.Collector{m.tunnelUp, m.tunnelReconnects, m.heartbeatFailures, m.statusFailures, m.pulls, m.imageCache, m.squeezes, m.balloonUnresponsive}
	if a.cfg.Drivers != nil {
		cs = append(cs, newOOMKills(a.cfg.Drivers.Hypervisor.OOMGroupKills))
	}
	return cs
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
