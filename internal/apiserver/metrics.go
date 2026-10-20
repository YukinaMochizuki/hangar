package apiserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// A machine is in a crash loop when its boot_time changed more than
// crashLoopBoots times within crashLoopWindow. Its node restarts an exited
// container after a delay that grows to a minute (internal/node/driver/chvm),
// so one that keeps exiting boots about ten times in the window, while
// someone restarting or changing a machine by hand stays well under.
const (
	crashLoopBoots  = 5
	crashLoopWindow = 15 * time.Minute
)

// metricsTimeout bounds the store reads of one scrape.
const metricsTimeout = 5 * time.Second

// Collectors are the apiserver's metrics, for the registry its metrics
// listener serves (docs/decisions/monitoring.md). None is labelled with a
// tenant's, project's or machine's name: machines are counted, never named.
func (s *Server) Collectors() []prometheus.Collector {
	return []prometheus.Collector{s.rpc.requests, s.rpc.duration, stateCollector{s}}
}

// rpcMetrics counts the Connect calls every listener serves, by procedure
// and code. A procedure is one the server registered, so a caller cannot
// make up label values.
type rpcMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newRPCMetrics() *rpcMetrics {
	return &rpcMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_apiserver_rpc_requests_total",
			Help: "Connect calls served, unary and streaming, counted as they end, by procedure and Connect code.",
		}, []string{"procedure", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "hangar_apiserver_rpc_duration_seconds",
			Help:    "How long unary Connect calls took to serve, by procedure. Streams, such as watches, are left out.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"procedure"}),
	}
}

func codeOf(err error) string {
	if err == nil {
		return "ok"
	}
	return connect.CodeOf(err).String()
}

func (m *rpcMetrics) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		procedure := req.Spec().Procedure
		m.duration.WithLabelValues(procedure).Observe(time.Since(start).Seconds())
		m.requests.WithLabelValues(procedure, codeOf(err)).Inc()
		return resp, err
	}
}

func (m *rpcMetrics) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (m *rpcMetrics) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		err := next(ctx, conn)
		m.requests.WithLabelValues(conn.Spec().Procedure, codeOf(err)).Inc()
		return err
	}
}

var (
	nodeReadyDesc = prometheus.NewDesc("hangar_apiserver_node_ready",
		"1 for the status of each Node's Ready condition (True, False or Unknown), 0 for the others. Unknown when the node agent stopped reporting, or never did.",
		[]string{"node", "status"}, nil)
	machinesDesc = prometheus.NewDesc("hangar_apiserver_machines",
		"Machines by phase and by the reason of their Ready condition; Other for a reason the apiserver does not know, empty for none.",
		[]string{"phase", "reason"}, nil)
	crashLoopDesc = prometheus.NewDesc("hangar_apiserver_machines_crash_looping",
		fmt.Sprintf("Machines whose boot time changed more than %d times in the last %v, as far as this apiserver has seen since it started.", crashLoopBoots, crashLoopWindow),
		nil, nil)
)

// stateCollector reads Nodes and Machines from the store at each scrape.
type stateCollector struct{ s *Server }

func (c stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- nodeReadyDesc
	ch <- machinesDesc
	ch <- crashLoopDesc
}

func (c stateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), metricsTimeout)
	defer cancel()
	if err := c.collectNodes(ctx, ch); err != nil {
		ch <- prometheus.NewInvalidMetric(nodeReadyDesc, err)
	}
	if err := c.collectMachines(ctx, ch); err != nil {
		ch <- prometheus.NewInvalidMetric(machinesDesc, err)
	}
	ch <- prometheus.MustNewConstMetric(crashLoopDesc, prometheus.GaugeValue, float64(c.s.boots.looping()))
}

var conditionStatuses = []metav1.ConditionStatus{
	metav1.ConditionStatus_CONDITION_STATUS_TRUE,
	metav1.ConditionStatus_CONDITION_STATUS_FALSE,
	metav1.ConditionStatus_CONDITION_STATUS_UNKNOWN,
}

func (c stateCollector) collectNodes(ctx context.Context, ch chan<- prometheus.Metric) error {
	objs, _, err := c.s.store.List(ctx, store.ListOptions{Kind: registry.Node.Kind})
	if err != nil {
		return err
	}
	for _, o := range objs {
		var n computev1alpha1.Node
		if err := proto.Unmarshal(o.Data, &n); err != nil {
			return err
		}
		status := metav1.ConditionStatus_CONDITION_STATUS_UNKNOWN
		if r := conditions.Find(n.GetStatus().GetConditions(), computeapi.NodeReady); r != nil && r.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED {
			status = r.GetStatus()
		}
		for _, st := range conditionStatuses {
			v := 0.0
			if st == status {
				v = 1
			}
			ch <- prometheus.MustNewConstMetric(nodeReadyDesc, prometheus.GaugeValue, v, o.Name, enumWord(st.String(), "CONDITION_STATUS_"))
		}
	}
	return nil
}

func (c stateCollector) collectMachines(ctx context.Context, ch chan<- prometheus.Metric) error {
	objs, _, err := c.s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind})
	if err != nil {
		return err
	}
	type group struct{ phase, reason string }
	counts := map[group]int{}
	for _, o := range objs {
		var m computev1alpha1.Machine
		if err := proto.Unmarshal(o.Data, &m); err != nil {
			return err
		}
		g := group{phase: enumWord(m.GetStatus().GetPhase().String(), "MACHINE_PHASE_")}
		if r := conditions.Find(m.GetStatus().GetConditions(), computeapi.MachineReady); r != nil {
			g.reason = machineReason(r.GetReason())
		}
		counts[g]++
	}
	for g, n := range counts {
		ch <- prometheus.MustNewConstMetric(machinesDesc, prometheus.GaugeValue, float64(n), g.phase, g.reason)
	}
	return nil
}

// enumWord turns a protobuf enum's name, such as MACHINE_PHASE_RUNNING,
// into the word a label holds: Running.
func enumWord(name, prefix string) string {
	w := strings.ToLower(strings.TrimPrefix(name, prefix))
	if w == "" {
		return w
	}
	return strings.ToUpper(w[:1]) + w[1:]
}

// machineReasons are the reasons a node gives for MachineReady. Any other
// is counted as Other, so a label value never comes from anywhere else.
var machineReasons = map[string]bool{
	computeapi.ReasonRunning: true, computeapi.ReasonCreating: true, computeapi.ReasonPulling: true,
	computeapi.ReasonBooting: true, computeapi.ReasonStopped: true, computeapi.ReasonImagePullFailed: true,
	computeapi.ReasonImagePullUnauthorized: true, computeapi.ReasonDiskFailed: true, computeapi.ReasonDiskTooSmall: true,
	computeapi.ReasonNetworkFailed: true, computeapi.ReasonBootFailed: true, computeapi.ReasonExited: true,
	computeapi.ReasonPoweredOff: true, computeapi.ReasonNetworkNotReady: true, computeapi.ReasonDiskNotReady: true,
	computeapi.ReasonExportFailed: true, computeapi.ReasonFileShareNotReady: true,
}

func machineReason(r string) string {
	if machineReasons[r] {
		return r
	}
	return "Other"
}

// bootTracker remembers when each machine's boot_time changed, for the
// crash-loop gauge. It is held in memory: after a restart the apiserver
// counts again from what it sees.
type bootTracker struct {
	clock    clockwork.Clock
	mu       sync.Mutex
	machines map[controller.Key]*boots
}

type boots struct {
	uid  string
	last time.Time
	// changes are when the apiserver saw boot_time change, within the
	// window.
	changes []time.Time
}

func newBootTracker(clock clockwork.Clock) *bootTracker {
	return &bootTracker{clock: clock, machines: map[controller.Key]*boots{}}
}

// reconcileBoots notes a change of the machine's boot_time. It writes
// nothing.
func (s *Server) reconcileBoots(ctx context.Context, key controller.Key) error {
	o, err := s.store.Get(ctx, store.Key{Kind: registry.Machine.Kind, Project: key.Project, Name: key.Name})
	if errors.Is(err, store.ErrNotFound) {
		s.boots.forget(key)
		return nil
	}
	if err != nil {
		return err
	}
	var m computev1alpha1.Machine
	if err := proto.Unmarshal(o.Data, &m); err != nil {
		return err
	}
	var booted time.Time
	if bt := m.GetStatus().GetBootTime(); bt != nil {
		booted = bt.AsTime()
	}
	s.boots.observe(key, m.GetMetadata().GetUid(), booted)
	return nil
}

// observe records the machine's boot time. The first one seen is no change,
// nor is a machine with none, as one stopped may have.
func (b *bootTracker) observe(key controller.Key, uid string, booted time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := b.machines[key]
	if m == nil || m.uid != uid {
		m = &boots{uid: uid}
		b.machines[key] = m
	}
	if booted.IsZero() || booted.Equal(m.last) {
		return
	}
	now := b.clock.Now()
	if !m.last.IsZero() {
		m.changes = append(recent(m.changes, now), now)
	}
	m.last = booted
}

func (b *bootTracker) forget(key controller.Key) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.machines, key)
}

// looping counts the machines in a crash loop now.
func (b *bootTracker) looping() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	n := 0
	for _, m := range b.machines {
		m.changes = recent(m.changes, now)
		if len(m.changes) > crashLoopBoots {
			n++
		}
	}
	return n
}

// recent drops the changes that are out of the window at now.
func recent(changes []time.Time, now time.Time) []time.Time {
	i := 0
	for i < len(changes) && !changes[i].After(now.Add(-crashLoopWindow)) {
		i++
	}
	return changes[i:]
}
