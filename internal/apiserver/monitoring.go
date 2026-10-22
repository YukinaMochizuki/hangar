package apiserver

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	monitoringv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/monitoring/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// MonitoringConfig is where MonitoringService asks: the host's Prometheus
// and Alertmanager, each on loopback (CheckMonitoringURL). An empty URL is
// none, and the calls that need it fail with Unavailable.
type MonitoringConfig struct {
	PrometheusURL   string
	AlertmanagerURL string
	// Timeout bounds each request to either; zero means
	// DefaultMonitoringTimeout.
	Timeout time.Duration
}

// monitoring serves MonitoringService: fixed queries, whose PromQL is in
// monitoringqueries.go, never a caller's (docs/decisions/monitoring.md).
type monitoring struct{ s *Server }

// newMonitoring makes the server's Prometheus and Alertmanager clients. A
// URL CheckMonitoringURL refuses is logged and left unset, so the calls
// answer Unavailable; hangar-apiserver checks its flags before.
func (s *Server) newMonitoring(cfg MonitoringConfig) {
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultMonitoringTimeout
	}
	var err error
	if s.prometheus, err = newMonitoringBackend("Prometheus", "--prometheus-url", cfg.PrometheusURL, cfg.Timeout); err != nil {
		s.log.Error("monitoring: Prometheus left unset", "error", err)
	}
	if s.alertmanager, err = newMonitoringBackend("Alertmanager", "--alertmanager-url", cfg.AlertmanagerURL, cfg.Timeout); err != nil {
		s.log.Error("monitoring: Alertmanager left unset", "error", err)
	}
}

// authorize lets in a platform member or service token holding
// platform.monitoring.read at the platform. It is checked before anything
// else, so a caller without it learns nothing, not even whether Prometheus
// is configured.
func (m monitoring) authorize(ctx context.Context) error {
	p, err := m.s.begin(ctx, false)
	if err != nil {
		return err
	}
	if !m.s.authz.Allowed(p, authz.MonitoringRead, authz.Target{}) {
		return denied("%s may not read the platform's monitoring: it takes %s at the platform", p, authz.MonitoringRead)
	}
	return nil
}

// maxConcurrentQueries caps the queries one overview has open at once.
const maxConcurrentQueries = 8

// GetOverview asks Prometheus every overview query at one instant, and
// builds the platform's layers from the answers, and each node's from those
// of its series.
func (m monitoring) GetOverview(ctx context.Context, _ *connect.Request[monitoringv1alpha1.GetOverviewRequest]) (*connect.Response[monitoringv1alpha1.GetOverviewResponse], error) {
	if err := m.authorize(ctx); err != nil {
		return nil, err
	}
	now := m.s.clock.Now()
	answers := make([][]sample, len(overviewQueries))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrentQueries)
	for i, q := range overviewQueries {
		g.Go(func() error {
			var err error
			answers[i], err = m.s.prometheus.query(gctx, q, now)
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	r := results{}
	for i, q := range overviewQueries {
		r[q] = answers[i]
	}
	res := &monitoringv1alpha1.GetOverviewResponse{
		ObserveTime: timestamppb.New(now),
		Layers:      []*monitoringv1alpha1.LayerState{reachabilityLayer(r), machinesLayer(r)},
	}
	for _, n := range r.nodes(m.s.cfg.Nodes) {
		rn := r.forNode(n)
		res.Nodes = append(res.Nodes, &monitoringv1alpha1.NodeOverview{
			Name:   n,
			Layers: []*monitoringv1alpha1.LayerState{hostLayer(rn), servicesLayer(n, rn)},
		})
	}
	return connect.NewResponse(res), nil
}

// GetSeries asks Prometheus one named series over the range, at the step
// the range has, ending at the last whole step: of the node named, for a
// node's series.
func (m monitoring) GetSeries(ctx context.Context, req *connect.Request[monitoringv1alpha1.GetSeriesRequest]) (*connect.Response[monitoringv1alpha1.GetSeriesResponse], error) {
	if err := m.authorize(ctx); err != nil {
		return nil, err
	}
	def, ok := seriesDefs[req.Msg.GetName()]
	if !ok {
		return nil, invalid("name: %s is no series", req.Msg.GetName())
	}
	rg, ok := seriesRanges[req.Msg.GetRange()]
	if !ok {
		return nil, invalid("range: %s is no range", req.Msg.GetRange())
	}
	node := req.Msg.GetNode()
	switch {
	case def.perNode && node == "":
		return nil, invalid("node: %s is a node's series; name the node", req.Msg.GetName())
	case !def.perNode && node != "":
		return nil, invalid("node: %s is the platform's series, of no node", req.Msg.GetName())
	case def.perNode:
		// A valid name is a DNS label, which goes into the query as it is.
		if err := ValidName(node); err != nil {
			return nil, invalid("node: %v", err)
		}
		known, err := m.s.exists(ctx, m.s.backend(registry.Node), node)
		if err != nil {
			return nil, err
		}
		if !known {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("node %q: no such Node", node))
		}
	}
	end := m.s.clock.Now().Truncate(rg.step)
	start := end.Add(-rg.span)
	q := def.promQL(rg, node)
	series, err := m.s.prometheus.queryRange(ctx, q, start, end, rg.step)
	if err != nil {
		return nil, err
	}
	if len(series) > 1 {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("the query %s answered %d series, not one", q, len(series)))
	}
	res := &monitoringv1alpha1.GetSeriesResponse{
		Name: req.Msg.GetName(), Node: node, Unit: def.unit, StepSeconds: int32(rg.step / time.Second),
	}
	for _, s := range series {
		for _, p := range s.points {
			if math.IsNaN(p.value) || math.IsInf(p.value, 0) {
				continue
			}
			res.Points = append(res.Points, &monitoringv1alpha1.Point{Time: timestamppb.New(p.time), Value: p.value})
		}
	}
	if def.capacity != "" {
		cap, err := m.s.prometheus.query(ctx, withNode(def.capacity, node), end)
		if err != nil {
			return nil, err
		}
		if v, ok := first(cap); ok {
			res.Capacity = proto.Float64(v)
			if def.alertAboveOf != nil {
				res.AlertAbove = proto.Float64(def.alertAboveOf(v))
			}
		}
	} else if def.alertAbove != 0 {
		res.AlertAbove = proto.Float64(def.alertAbove)
	}
	return connect.NewResponse(res), nil
}

// ListNodeUsage answers, for each Node, its machines counted and what they
// hold there, summed (nodeUsageSummary). It reads the store, so it answers
// without Prometheus.
func (m monitoring) ListNodeUsage(ctx context.Context, _ *connect.Request[monitoringv1alpha1.ListNodeUsageRequest]) (*connect.Response[monitoringv1alpha1.ListNodeUsageResponse], error) {
	if err := m.authorize(ctx); err != nil {
		return nil, err
	}
	nodes, err := m.s.nodeUsageSummary(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&monitoringv1alpha1.ListNodeUsageResponse{Nodes: nodes}), nil
}

// nodeUsageSummary is, for each Node by name, the machines placed on it
// counted and their vCPUs summed, with the memory and disk held there as
// the scheduler counts them (nodeUsage): the machines', and the disks' and
// file shares' placed there. No machine, tenant or project is named.
func (s *Server) nodeUsageSummary(ctx context.Context) ([]*monitoringv1alpha1.NodeUsage, error) {
	nodes, used, err := s.nodeUsage(ctx, resource.Key{})
	if err != nil {
		return nil, err
	}
	machineObjs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind})
	if err != nil {
		return nil, err
	}
	type placed struct {
		machines int32
		cpu      int64
	}
	per := map[string]placed{}
	for _, o := range machineObjs {
		mc, ok := decodeMachine(o)
		if !ok {
			return nil, fmt.Errorf("decode machine %s", o.Name)
		}
		n := mc.GetSpec().GetNode()
		if n == "" {
			continue
		}
		p := per[n]
		p.machines++
		// The controllers write a machine's cpu in its canonical form; one
		// that does not parse adds no vCPU, and the machine still counts.
		// The scheduler does not count vCPUs, which are shared.
		if c := mc.GetSpec().GetResources().GetCpu(); c != "" {
			if v, err := quantity.CPU(c); err == nil {
				p.cpu += int64(v)
			}
		}
		per[n] = p
	}
	var out []*monitoringv1alpha1.NodeUsage
	for _, n := range nodes {
		name := n.GetMetadata().GetName()
		p, h := per[name], used[name]
		out = append(out, &monitoringv1alpha1.NodeUsage{
			Node:     name,
			Machines: p.machines,
			Cpu:      strconv.FormatInt(p.cpu, 10),
			Memory:   formatAmount(dimMemory, clampInt64(h.memory)),
			Disk:     formatAmount(dimDisk, clampInt64(h.disk)),
		})
	}
	slices.SortFunc(out, func(a, b *monitoringv1alpha1.NodeUsage) int { return strings.Compare(a.GetNode(), b.GetNode()) })
	return out, nil
}

// clampInt64 is v, or the largest int64 when v is past it: nodeUsage holds
// its sums at the largest uint64 rather than wrapping.
func clampInt64(v uint64) int64 {
	return int64(min(v, math.MaxInt64))
}

// watchdog is the always-firing alert that proves the alerting path works,
// which ListAlerts leaves out.
const watchdog = "Watchdog"

// ListAlerts answers the alerts Alertmanager has firing, but Watchdog, with
// their name, severity, summary, start and node: never their other labels
// or annotations. A node's name is the platform's own (monitoring.md, "No
// tenant in a label").
func (m monitoring) ListAlerts(ctx context.Context, _ *connect.Request[monitoringv1alpha1.ListAlertsRequest]) (*connect.Response[monitoringv1alpha1.ListAlertsResponse], error) {
	if err := m.authorize(ctx); err != nil {
		return nil, err
	}
	alerts, err := m.s.alertmanager.alerts(ctx)
	if err != nil {
		return nil, err
	}
	res := &monitoringv1alpha1.ListAlertsResponse{}
	for _, a := range alerts {
		name := a.Labels["alertname"]
		if name == watchdog {
			continue
		}
		sev := monitoringv1alpha1.Severity_SEVERITY_CRITICAL
		if a.Labels["severity"] == "warning" {
			sev = monitoringv1alpha1.Severity_SEVERITY_WARNING
		}
		res.Alerts = append(res.Alerts, &monitoringv1alpha1.Alert{
			Name: name, Severity: sev, Summary: a.Annotations["summary"], StartTime: timestamppb.New(a.StartsAt), Node: a.Labels["node"],
		})
	}
	slices.SortStableFunc(res.Alerts, func(a, b *monitoringv1alpha1.Alert) int {
		return cmp.Or(
			cmp.Compare(a.GetSeverity(), b.GetSeverity()),
			a.GetStartTime().AsTime().Compare(b.GetStartTime().AsTime()),
			strings.Compare(a.GetName(), b.GetName()),
		)
	})
	return connect.NewResponse(res), nil
}
