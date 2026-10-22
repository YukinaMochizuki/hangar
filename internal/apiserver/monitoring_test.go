package apiserver_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	mon "github.com/YukinaMochizuki/hangar/gen/go/hangar/monitoring/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
)

// promAsk is one request a fakeProm took.
type promAsk struct {
	path, query, time, start, end, step string
}

// fakeProm is a Prometheus that answers each instant query from vectors and
// each range query from matrices, by the query's text, with no series for
// any other, and records every request. With status set it answers every
// request with that status instead; with body set, with that body.
type fakeProm struct {
	*httptest.Server
	mu       sync.Mutex
	asked    []promAsk
	vectors  map[string][]promSeries
	matrices map[string][]promSeries
	status   int
	body     string
}

// promSeries is one series a fakeProm answers: its labels, and its values,
// one for an instant query.
type promSeries struct {
	labels map[string]string
	values []string
}

func newFakeProm(t *testing.T) *fakeProm {
	p := &fakeProm{vectors: map[string][]promSeries{}, matrices: map[string][]promSeries{}}
	p.Server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.Close)
	return p
}

func (p *fakeProm) serve(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a := promAsk{path: r.URL.Path, query: r.Form.Get("query"), time: r.Form.Get("time"),
		start: r.Form.Get("start"), end: r.Form.Get("end"), step: r.Form.Get("step")}
	p.mu.Lock()
	p.asked = append(p.asked, a)
	status, body := p.status, p.body
	vectors, matrices := p.vectors[a.query], p.matrices[a.query]
	p.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
		return
	}
	if body != "" {
		_, _ = w.Write([]byte(body))
		return
	}
	type result struct {
		Metric map[string]string `json:"metric"`
		Value  []any             `json:"value,omitempty"`
		Values [][]any           `json:"values,omitempty"`
	}
	results := []result{}
	resultType := "vector"
	switch a.path {
	case "/api/v1/query":
		for _, s := range vectors {
			results = append(results, result{Metric: s.labels, Value: []any{1700000000.0, s.values[0]}})
		}
	case "/api/v1/query_range":
		resultType = "matrix"
		start, _ := strconv.ParseFloat(a.start, 64)
		step, _ := strconv.ParseFloat(a.step, 64)
		for _, s := range matrices {
			res := result{Metric: s.labels}
			for i, v := range s.values {
				res.Values = append(res.Values, []any{start + float64(i)*step, v})
			}
			results = append(results, res)
		}
	default:
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": resultType, "result": results}})
}

func (p *fakeProm) requests() []promAsk {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]promAsk(nil), p.asked...)
}

func (p *fakeProm) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = nil
}

// vector adds to what an instant query answers: each series as labels and
// one value.
func (p *fakeProm) vector(q string, series ...promSeries) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.vectors[q] = append(p.vectors[q], series...)
}

func one(v string, labels ...string) promSeries {
	l := map[string]string{}
	for i := 0; i+1 < len(labels); i += 2 {
		l[labels[i]] = labels[i+1]
	}
	return promSeries{labels: l, values: []string{v}}
}

// fakeAlertmanager answers its alerts, and records each request's query.
type fakeAlertmanager struct {
	*httptest.Server
	mu      sync.Mutex
	queries []string
	alerts  []map[string]any
	status  int
}

func newFakeAlertmanager(t *testing.T) *fakeAlertmanager {
	am := &fakeAlertmanager{}
	am.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		am.mu.Lock()
		defer am.mu.Unlock()
		am.queries = append(am.queries, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		if am.status != 0 {
			w.WriteHeader(am.status)
			return
		}
		_ = json.NewEncoder(w).Encode(append([]map[string]any{}, am.alerts...))
	}))
	t.Cleanup(am.Close)
	return am
}

func (am *fakeAlertmanager) requests() []string {
	am.mu.Lock()
	defer am.mu.Unlock()
	return append([]string(nil), am.queries...)
}

func withMonitoring(prometheus, alertmanager string) envOption {
	return func(c *apiserver.Config) {
		c.Monitoring = apiserver.MonitoringConfig{PrometheusURL: prometheus, AlertmanagerURL: alertmanager}
	}
}

// callMonitoring makes each of MonitoringService's calls, and returns their
// errors by name.
func callMonitoring(t *testing.T, c clients) map[string]error {
	ctx := t.Context()
	_, overview := c.monitoring.GetOverview(ctx, connect.NewRequest(&mon.GetOverviewRequest{}))
	_, series := c.monitoring.GetSeries(ctx, connect.NewRequest(&mon.GetSeriesRequest{
		Name: mon.SeriesName_SERIES_NAME_HOST_CPU, Range: mon.SeriesRange_SERIES_RANGE_ONE_HOUR, Node: testNode,
	}))
	_, alerts := c.monitoring.ListAlerts(ctx, connect.NewRequest(&mon.ListAlertsRequest{}))
	return map[string]error{"GetOverview": overview, "GetSeries": series, "ListAlerts": alerts}
}

// TestMonitoringNeedsItsPermission: only platform.root and
// platform.sponsor, which hold platform.monitoring.read, may read the
// platform's monitoring; every other role, a service token without it, the
// node agent and someone never invited are refused before anything is
// asked of Prometheus, Alertmanager or the store.
func TestMonitoringNeedsItsPermission(t *testing.T) {
	prom, am := newFakeProm(t), newFakeAlertmanager(t)
	e := newPermEnv(t, withMonitoring(prom.URL, am.URL))
	refused := map[string]clients{
		"the node agent": e.asNode(),
		"a stranger":     e.as("stranger@example.com"),
	}
	for role, c := range e.callers {
		if role != "platform.root" && role != "platform.sponsor" {
			refused[role] = c
		}
	}
	for who, c := range refused {
		for call, err := range callMonitoring(t, c) {
			wantCode(t, err, connect.CodePermissionDenied, who+"'s "+call)
		}
		_, err := c.monitoring.ListNodeUsage(t.Context(), connect.NewRequest(&mon.ListNodeUsageRequest{}))
		wantCode(t, err, connect.CodePermissionDenied, who+"'s ListNodeUsage")
	}
	if got := prom.requests(); len(got) != 0 {
		t.Errorf("refused calls asked Prometheus %v", got)
	}
	if got := am.requests(); len(got) != 0 {
		t.Errorf("refused calls asked Alertmanager %v", got)
	}
	for _, role := range []string{"platform.root", "platform.sponsor"} {
		c := e.callers[role]
		for call, err := range callMonitoring(t, c) {
			if err != nil {
				t.Errorf("%s's %s: %v", role, call, err)
			}
		}
		if _, err := c.monitoring.ListNodeUsage(t.Context(), connect.NewRequest(&mon.ListNodeUsageRequest{})); err != nil {
			t.Errorf("%s's ListNodeUsage: %v", role, err)
		}
	}
}

// TestNodeUsageNeedsNoPrometheus: ListNodeUsage reads the store, so it
// answers every Node while no Prometheus is configured, and names nothing
// but the Node.
func TestNodeUsageNeedsNoPrometheus(t *testing.T) {
	e := newEnv(t)
	res, err := e.as(root).monitoring.ListNodeUsage(t.Context(), connect.NewRequest(&mon.ListNodeUsageRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	want := &mon.ListNodeUsageResponse{Nodes: []*mon.NodeUsage{{Node: testNode, Cpu: "0", Memory: "0", Disk: "0"}}}
	if !proto.Equal(res.Msg, want) {
		t.Errorf("got %v, want %v", res.Msg, want)
	}
}

// TestMonitoringUnavailable: with no Prometheus or Alertmanager configured,
// or one that does not answer, answers 5xx, or takes longer than the
// timeout, the calls fail with Unavailable; one answering more than the
// apiserver reads fails with Internal. A caller without the permission is
// refused whatever the configuration.
func TestMonitoringUnavailable(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	failing, failingAM := newFakeProm(t), newFakeAlertmanager(t)
	failing.status, failingAM.status = http.StatusServiceUnavailable, http.StatusInternalServerError
	// It reads the request first: the server notices the client going only
	// once the body is read.
	hanging := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		<-r.Context().Done()
	}))
	t.Cleanup(hanging.Close)
	for name, opt := range map[string]envOption{
		"unset":       func(*apiserver.Config) {},
		"unreachable": withMonitoring(gone.URL, gone.URL),
		"failing":     withMonitoring(failing.URL, failingAM.URL),
		"too slow": func(c *apiserver.Config) {
			c.Monitoring = apiserver.MonitoringConfig{PrometheusURL: hanging.URL, AlertmanagerURL: hanging.URL, Timeout: 50 * time.Millisecond}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, opt)
			for call, err := range callMonitoring(t, e.as(root)) {
				wantCode(t, err, connect.CodeUnavailable, "root's "+call)
			}
			for call, err := range callMonitoring(t, e.as(alice)) {
				wantCode(t, err, connect.CodePermissionDenied, "a member's "+call)
			}
		})
	}
	t.Run("answering too much", func(t *testing.T) {
		huge := newFakeProm(t)
		huge.body = `{"status":"success","data":{"resultType":"vector","result":[` + strings.Repeat(" ", 5<<20) + `]}}`
		e := newEnv(t, withMonitoring(huge.URL, ""))
		_, err := e.as(root).monitoring.GetOverview(t.Context(), connect.NewRequest(&mon.GetOverviewRequest{}))
		wantCode(t, err, connect.CodeInternal, "an answer of 5 MiB")
	})
}

const gib = 1 << 30

// tenantish are labels naming a tenant, a project and a machine, which every
// series a fakeProm answers carries and no answer may pass on.
var tenantish = []string{"tenant", "zebraco", "project", "zebrapj", "machine", "zebra-vm"}

func ts(v string, labels ...string) promSeries { return one(v, append(labels, tenantish...)...) }

// platformAnswers are what a fakeProm answers the overview's platform
// queries: a certificate 12 days from expiry, and two machines blocked by
// the platform.
func platformAnswers(p *fakeProm) {
	api, console := "https://api.hangarlab.cloud/healthz", "https://console.hangarlab.cloud/"
	p.vector(`min by (instance) (probe_success{job="blackbox"})`, ts("1", "instance", api), ts("1", "instance", console))
	p.vector(`max by (instance) (probe_http_status_code{job="blackbox"})`, ts("200", "instance", api), ts("200", "instance", console))
	p.vector(`max by (instance) (probe_duration_seconds{job="blackbox"})`, ts("0.041", "instance", api), ts("0.038", "instance", console))
	p.vector(`min by (instance) (probe_ssl_earliest_cert_expiry{job="blackbox"} - time()) / 86400`, ts("12.4", "instance", api), ts("61.2", "instance", console))
	p.vector(`sum by (phase) (hangar_apiserver_machines)`,
		ts("14", "phase", "Running"), ts("1", "phase", "Pending"), ts("6", "phase", "Stopped"), ts("3", "phase", "Failed"))
	p.vector(`sum by (reason) (hangar_apiserver_machines{reason=~"DiskFailed|NetworkFailed|ExportFailed|Exited|BootFailed"})`,
		ts("2", "reason", "DiskFailed"), ts("1", "reason", "Exited"))
	p.vector(`max(hangar_apiserver_machines_crash_looping)`, ts("1"))
}

// nodeAnswers add to what a fakeProm answers the overview's node queries
// one node's series: healthy, but for its md mirror when degraded, and for
// its Ready and its tunnel when down.
func nodeAnswers(p *fakeProm, node string, degraded, down bool) {
	s := func(v string, labels ...string) promSeries { return ts(v, append(labels, "node", node)...) }
	p.vector(`1 - avg by (node) (rate(node_cpu_seconds_total{mode="idle"}[5m]))`, s("0.23"))
	p.vector(`max by (node) (node_load1)`, s("2.1"))
	p.vector(`sum by (node) (node_memory_MemTotal_bytes)`, s(fmt.Sprint(128*gib)))
	p.vector(`sum by (node) (node_memory_MemAvailable_bytes) + (sum by (node) (clamp_min(node_zfs_arc_size - node_zfs_arc_c_min, 0)) or 0 * sum by (node) (node_memory_MemAvailable_bytes))`, s(fmt.Sprint(53.8*gib)))
	p.vector(`sum by (node) (node_memory_SwapTotal_bytes - node_memory_SwapFree_bytes)`, s("0"))
	p.vector(`node_zfs_zpool_state == 1`, s("1", "zpool", "hangar", "state", "online"))
	p.vector(`hangar_zpool_size_bytes`, s(fmt.Sprint(840*gib), "zpool", "hangar"))
	p.vector(`hangar_zpool_allocated_bytes`, s(fmt.Sprint(344.4*gib), "zpool", "hangar"))
	p.vector(`hangar_zpool_data_errors`, s("0", "zpool", "hangar"))
	p.vector(`sum by (node, zpool) (hangar_zpool_device_errors)`, s("0", "zpool", "hangar"))
	p.vector(`1 - node_filesystem_avail_bytes{mountpoint="/var/lib/hangar/images"} / node_filesystem_size_bytes{mountpoint="/var/lib/hangar/images"}`, s("0.62"))
	p.vector(`node_md_state == 1`, s("1", "device", "md127", "state", "active"))
	p.vector(`node_md_disks_required`, s("2", "device", "md127"))
	if degraded {
		p.vector(`node_md_disks{state="active"}`, s("1", "device", "md127", "state", "active"))
		p.vector(`max without (state) (node_md_disks{state="failed"} > 0 or node_md_disks_required - ignoring(state) node_md_disks{state="active"} > 0 or node_md_state{state="inactive"} == 1)`, s("1", "device", "md127"))
	} else {
		p.vector(`node_md_disks{state="active"}`, s("2", "device", "md127", "state", "active"))
	}
	p.vector(`smartctl_device_smart_status`, s("1", "device", "nvme0"), s("1", "device", "nvme1"))
	p.vector(`smartctl_device_critical_warning`, s("0", "device", "nvme0"), s("0", "device", "nvme1"))
	p.vector(`smartctl_device_media_errors`, s("0", "device", "nvme0"), s("0", "device", "nvme1"))
	p.vector(`smartctl_device_percentage_used`, s("12", "device", "nvme0"), s("14", "device", "nvme1"))
	p.vector(`smartctl_device_temperature{temperature_type="current"}`, s("41", "device", "nvme0"), s("43", "device", "nvme1"))
	p.vector(`max by (node) (node_timex_sync_status)`, s("1"))
	p.vector(`max by (node) (abs(node_timex_offset_seconds))`, s("0.0003"))
	p.vector(`node_systemd_unit_state{state="active",name=~"hangar-.+[.]service"}`,
		s("1", "name", "hangar-apiserver.service"), s("1", "name", "hangar-node.service"), s("1", "name", "hangar-edge.service"))
	p.vector(`clamp_min(node_systemd_service_restart_total{name=~"hangar-.+[.]service"} - node_systemd_service_restart_total{name=~"hangar-.+[.]service"} offset 1d, 0)`,
		s("0", "name", "hangar-apiserver.service"), s("0", "name", "hangar-node.service"), s("1", "name", "hangar-edge.service"))
	ready, tunnel := "True", "1"
	if down {
		ready, tunnel = "Unknown", "0"
	}
	p.vector(`hangar_apiserver_node_ready == 1`, s("1", "status", ready))
	p.vector(`max by (node) (hangar_node_tunnel_connected)`, s(tunnel))
}

// nodeOverview is what GetOverview answers of a node nodeAnswers gave.
func nodeOverview(node string, degraded, down bool) *mon.NodeOverview {
	host, md := hOK, c("md127 mirror", hOK, "active · 2 of 2 disks")
	if degraded {
		host, md = hCritical, c("md127 mirror", hCritical, "degraded · 1 of 2 disks")
	}
	services, ready, tunnel := hOK, c("Node "+node, hOK, "Ready"), c("Node tunnel", hOK, "connected")
	if down {
		services, ready, tunnel = hCritical, c("Node "+node, hCritical, "Ready Unknown"), c("Node tunnel", hCritical, "disconnected")
	}
	return &mon.NodeOverview{Name: node, Layers: []*mon.LayerState{
		{Layer: mon.Layer_LAYER_HOST, Health: host, Checks: []*mon.Check{
			c("CPU", hOK, "23% · load 2.1"),
			c("Memory", hOK, "74.2 of 128 GiB"),
			c("Swap", hOK, "0 B used"),
			c("Pool hangar", hOK, "ONLINE · 344 of 840 GiB · 41%"),
			c("hangar/images", hOK, "62% of quota"),
			md,
			c("nvme0", hOK, "PASSED · wear 12% · 41 °C"),
			c("nvme1", hOK, "PASSED · wear 14% · 43 °C"),
			c("Time sync", hOK, "synced · 0.3 ms"),
		}},
		{Layer: mon.Layer_LAYER_SERVICES, Health: services, Checks: []*mon.Check{
			c("hangar-apiserver", hOK, "active · 0 restarts 24 h"),
			c("hangar-edge", hOK, "active · 1 restart 24 h"),
			c("hangar-node", hOK, "active · 0 restarts 24 h"),
			ready,
			tunnel,
		}},
	}}
}

func c(name string, h mon.Health, value string) *mon.Check {
	return &mon.Check{Name: name, Health: h, Value: value}
}

const (
	hOK       = mon.Health_HEALTH_OK
	hWarning  = mon.Health_HEALTH_WARNING
	hCritical = mon.Health_HEALTH_CRITICAL
	hUnknown  = mon.Health_HEALTH_UNKNOWN
)

// noTenant fails the test if a response names the tenant, project or
// machine the store or the fakes hold.
func noTenant(t *testing.T, what string, m proto.Message) {
	t.Helper()
	if b := protojson.Format(m); strings.Contains(b, "zebra") {
		t.Errorf("%s names a tenant's: %s", what, b)
	}
}

// platformLayers are what GetOverview answers of what platformAnswers gave.
var platformLayers = []*mon.LayerState{
	{Layer: mon.Layer_LAYER_REACHABILITY, Health: hWarning, Checks: []*mon.Check{
		c("API /healthz", hOK, "200 · 41 ms"),
		c("Console", hOK, "200 · 38 ms"),
		c("Certificate api", hWarning, "12 days left"),
		c("Certificate console", hOK, "61 days left"),
	}},
	// MachinesFailedByThePlatform is critical.
	{Layer: mon.Layer_LAYER_MACHINES, Health: hCritical, Machines: &mon.MachineCounts{
		Phases: []*mon.PhaseCount{
			{Phase: "Running", Count: 14}, {Phase: "Pending", Count: 1}, {Phase: "Creating"},
			{Phase: "Stopped", Count: 6}, {Phase: "Suspended"}, {Phase: "Failed", Count: 3},
		},
		PlatformFailures: []*mon.ReasonCount{{Reason: "DiskFailed", Count: 2}},
		ProgramFailures:  []*mon.ReasonCount{{Reason: "Exited", Count: 1}},
		CrashLooping:     1,
	}},
}

// TestMonitoringOverview: the platform's layers and the node's are built
// from what Prometheus answers, at the thresholds of the alerting rules,
// and nothing of a tenant's is in them, though the store holds a tenant and
// every series Prometheus answers carries labels naming one.
func TestMonitoringOverview(t *testing.T) {
	prom := newFakeProm(t)
	platformAnswers(prom)
	nodeAnswers(prom, testNode, true, false)
	e := newEnv(t, withMonitoring(prom.URL, ""))
	e.setupTenant(alice, "zebraco", "zebrapj")
	got := must[mon.GetOverviewResponse](t)(e.as(root).monitoring.GetOverview(t.Context(), connect.NewRequest(&mon.GetOverviewRequest{})))
	want := &mon.GetOverviewResponse{
		ObserveTime: timestamppb.New(e.clock.Now()),
		Layers:      platformLayers,
		Nodes:       []*mon.NodeOverview{nodeOverview(testNode, true, false)},
	}
	if !proto.Equal(got, want) {
		t.Errorf("the overview:\n got %s\nwant %s", protojson.Format(got), protojson.Format(want))
	}
	noTenant(t, "the overview", got)
	for _, a := range prom.requests() {
		if a.time != fmt.Sprintf("%.3f", float64(e.clock.Now().UnixMilli())/1000) {
			t.Errorf("%s was asked at %s, not now", a.query, a.time)
		}
	}
}

// TestMonitoringOverviewOfTwoNodes: each node's layers come from its own
// series alone, the platform's once; a node Prometheus has series of is
// answered for though the apiserver does not accept it, and one it accepts
// with no series is answered as no data.
func TestMonitoringOverviewOfTwoNodes(t *testing.T) {
	prom := newFakeProm(t)
	platformAnswers(prom)
	nodeAnswers(prom, testNode, true, false)
	nodeAnswers(prom, "n2", false, true)
	e := newEnv(t, withMonitoring(prom.URL, ""), func(c *apiserver.Config) { c.Nodes = append(c.Nodes, "n3") })
	got := must[mon.GetOverviewResponse](t)(e.as(root).monitoring.GetOverview(t.Context(), connect.NewRequest(&mon.GetOverviewRequest{})))
	want := &mon.GetOverviewResponse{
		ObserveTime: timestamppb.New(e.clock.Now()),
		Layers:      platformLayers,
		Nodes: []*mon.NodeOverview{
			nodeOverview(testNode, true, false),
			nodeOverview("n2", false, true),
			{Name: "n3", Layers: noDataNode("n3")},
		},
	}
	if !proto.Equal(got, want) {
		t.Errorf("the overview:\n got %s\nwant %s", protojson.Format(got), protojson.Format(want))
	}
}

func noDataNode(node string) []*mon.LayerState {
	return []*mon.LayerState{
		{Layer: mon.Layer_LAYER_HOST, Health: hUnknown, Checks: []*mon.Check{
			c("CPU", hUnknown, "no data"), c("Memory", hUnknown, "no data"), c("Swap", hUnknown, "no data"),
			c("ZFS pools", hUnknown, "no data"), c("hangar/images", hUnknown, "no data"), c("md mirror", hUnknown, "no data"),
			c("NVMe disks", hUnknown, "no data"), c("Time sync", hUnknown, "no data"),
		}},
		{Layer: mon.Layer_LAYER_SERVICES, Health: hUnknown, Checks: []*mon.Check{
			c("hangar-* units", hUnknown, "no data"), c("Node "+node, hUnknown, "no data"), c("Node tunnel", hUnknown, "no data"),
		}},
	}
}

// TestMonitoringOverviewWithoutData: what Prometheus has nothing on is
// shown as such, never as healthy, for the platform and for each node the
// apiserver accepts.
func TestMonitoringOverviewWithoutData(t *testing.T) {
	prom := newFakeProm(t)
	e := newEnv(t, withMonitoring(prom.URL, ""))
	got := must[mon.GetOverviewResponse](t)(e.as(root).monitoring.GetOverview(t.Context(), connect.NewRequest(&mon.GetOverviewRequest{})))
	want := &mon.GetOverviewResponse{
		Layers: []*mon.LayerState{
			{Layer: mon.Layer_LAYER_REACHABILITY, Health: hUnknown, Checks: []*mon.Check{c("HTTPS probes", hUnknown, "no data")}},
			{Layer: mon.Layer_LAYER_MACHINES, Health: hUnknown, Checks: []*mon.Check{c("Machines", hUnknown, "no data")}},
		},
		Nodes: []*mon.NodeOverview{{Name: testNode, Layers: noDataNode(testNode)}},
	}
	got.ObserveTime = nil
	if !proto.Equal(got, want) {
		t.Errorf("the overview without data:\n got %s", protojson.Format(got))
	}
}

// TestMonitoringSeries: each series asks its own fixed PromQL over the
// range, at the step the range has, ending at the last whole step, a node's
// of that node alone; a step with no value has no point; capacity and the
// alert threshold come with the series that have them.
func TestMonitoringSeries(t *testing.T) {
	prom := newFakeProm(t)
	e := newEnv(t, withMonitoring(prom.URL, ""), func(c *apiserver.Config) { c.Nodes = append(c.Nodes, "n2") })
	for _, n := range []string{testNode, "n2"} {
		prom.vector(`sum(node_memory_MemTotal_bytes{node="`+n+`"})`, one(fmt.Sprint(128*gib)))
		prom.vector(`sum(hangar_zpool_size_bytes{node="`+n+`"})`, one(fmt.Sprint(840*gib)))
	}
	const (
		cpu     = `1 - avg(rate(node_cpu_seconds_total{mode="idle",node="$n"}[$w]))`
		memory  = `sum(node_memory_MemTotal_bytes{node="$n"}) - (sum(node_memory_MemAvailable_bytes{node="$n"}) + (sum(clamp_min(node_zfs_arc_size{node="$n"} - node_zfs_arc_c_min{node="$n"}, 0)) or vector(0)))`
		pool    = `sum(hangar_zpool_allocated_bytes{node="$n"})`
		rate    = `sum(rate(hangar_apiserver_rpc_requests_total[$w]))`
		errors  = `(sum(rate(hangar_apiserver_rpc_requests_total{code=~"unknown|internal|unavailable|data_loss"}[$w])) or vector(0)) / sum(rate(hangar_apiserver_rpc_requests_total[$w]))`
		latency = `histogram_quantile(0.99, sum by (le) (rate(hangar_apiserver_rpc_duration_seconds_bucket[$w])))`
	)
	series := []struct {
		name               mon.SeriesName
		nodes              []string
		query              string
		unit               mon.Unit
		capacity, alertsAt *float64
	}{
		{mon.SeriesName_SERIES_NAME_HOST_CPU, []string{testNode, "n2"}, cpu, mon.Unit_UNIT_RATIO, nil, nil},
		{mon.SeriesName_SERIES_NAME_HOST_MEMORY, []string{testNode, "n2"}, memory, mon.Unit_UNIT_BYTES, proto.Float64(128 * gib), proto.Float64(127 * gib)},
		{mon.SeriesName_SERIES_NAME_POOL_USED, []string{testNode, "n2"}, pool, mon.Unit_UNIT_BYTES, proto.Float64(840 * gib), proto.Float64(0.9 * 840 * gib)},
		{mon.SeriesName_SERIES_NAME_API_REQUEST_RATE, []string{""}, rate, mon.Unit_UNIT_PER_SECOND, nil, nil},
		{mon.SeriesName_SERIES_NAME_API_ERROR_RATE, []string{""}, errors, mon.Unit_UNIT_RATIO, nil, proto.Float64(0.05)},
		{mon.SeriesName_SERIES_NAME_API_LATENCY_P99, []string{""}, latency, mon.Unit_UNIT_SECONDS, nil, proto.Float64(1)},
	}
	ranges := []struct {
		rg         mon.SeriesRange
		span, step time.Duration
		window     string
	}{
		{mon.SeriesRange_SERIES_RANGE_ONE_HOUR, time.Hour, time.Minute, "5m"},
		{mon.SeriesRange_SERIES_RANGE_ONE_DAY, 24 * time.Hour, 15 * time.Minute, "15m"},
		{mon.SeriesRange_SERIES_RANGE_SEVEN_DAYS, 7 * 24 * time.Hour, time.Hour, "1h"},
	}
	promTime := func(t time.Time) string { return fmt.Sprintf("%.3f", float64(t.UnixMilli())/1000) }
	for _, s := range series {
		for _, node := range s.nodes {
			for _, r := range ranges {
				q := strings.NewReplacer("$w", r.window, "$n", node).Replace(s.query)
				prom.mu.Lock()
				prom.matrices[q] = []promSeries{{labels: map[string]string{"project": "zebrapj"}, values: []string{"0.1", "NaN", "0.3"}}}
				prom.mu.Unlock()
				prom.reset()
				got := must[mon.GetSeriesResponse](t)(e.as(root).monitoring.GetSeries(t.Context(), connect.NewRequest(&mon.GetSeriesRequest{Name: s.name, Range: r.rg, Node: node})))
				end := e.clock.Now().Truncate(r.step)
				start := end.Add(-r.span)
				want := &mon.GetSeriesResponse{
					Name: s.name, Node: node, Unit: s.unit, StepSeconds: int32(r.step / time.Second),
					Points: []*mon.Point{
						{Time: timestamppb.New(start), Value: 0.1},
						{Time: timestamppb.New(start.Add(2 * r.step)), Value: 0.3},
					},
					Capacity: s.capacity, AlertAbove: s.alertsAt,
				}
				if !proto.Equal(got, want) {
					t.Errorf("%s of %q over %s:\n got %s\nwant %s", s.name, node, r.rg, protojson.Format(got), protojson.Format(want))
				}
				noTenant(t, s.name.String(), got)
				asked := prom.requests()
				if len(asked) == 0 || asked[0] != (promAsk{path: "/api/v1/query_range", query: q, start: promTime(start), end: promTime(end), step: strconv.Itoa(int(r.step / time.Second))}) {
					t.Errorf("%s of %q over %s asked %+v, want the range query %s", s.name, node, r.rg, asked, q)
				}
				// A node's series never asks of another node.
				for _, a := range asked {
					for _, other := range []string{testNode, "n2"} {
						if other != node && strings.Contains(a.query, `node="`+other+`"`) {
							t.Errorf("%s of %q asked of %s: %s", s.name, node, other, a.query)
						}
					}
				}
			}
		}
	}
	hour := mon.SeriesRange_SERIES_RANGE_ONE_HOUR
	for _, tc := range []struct {
		req  *mon.GetSeriesRequest
		code connect.Code
	}{
		{&mon.GetSeriesRequest{Range: hour}, connect.CodeInvalidArgument},
		{&mon.GetSeriesRequest{Name: mon.SeriesName_SERIES_NAME_HOST_CPU, Node: testNode}, connect.CodeInvalidArgument},
		{&mon.GetSeriesRequest{Name: 99, Range: hour}, connect.CodeInvalidArgument},
		{&mon.GetSeriesRequest{Name: mon.SeriesName_SERIES_NAME_HOST_CPU, Range: 99, Node: testNode}, connect.CodeInvalidArgument},
		// A node's series names its node, a platform's none.
		{&mon.GetSeriesRequest{Name: mon.SeriesName_SERIES_NAME_HOST_CPU, Range: hour}, connect.CodeInvalidArgument},
		{&mon.GetSeriesRequest{Name: mon.SeriesName_SERIES_NAME_API_REQUEST_RATE, Range: hour, Node: testNode}, connect.CodeInvalidArgument},
		// The node is a valid name, never PromQL, and a Node there is.
		{&mon.GetSeriesRequest{Name: mon.SeriesName_SERIES_NAME_HOST_CPU, Range: hour, Node: `n1"} or vector(1) or {node="`}, connect.CodeInvalidArgument},
		{&mon.GetSeriesRequest{Name: mon.SeriesName_SERIES_NAME_POOL_USED, Range: hour, Node: "nosuch"}, connect.CodeNotFound},
	} {
		prom.reset()
		_, err := e.as(root).monitoring.GetSeries(t.Context(), connect.NewRequest(tc.req))
		wantCode(t, err, tc.code, fmt.Sprintf("series %v of %q over %v", tc.req.GetName(), tc.req.GetNode(), tc.req.GetRange()))
		if asked := prom.requests(); len(asked) != 0 {
			t.Errorf("a refused series asked %+v", asked)
		}
	}
}

// TestMonitoringAlerts: the alerts Alertmanager has firing, but those
// silenced or inhibited and Watchdog, most severe and then oldest first,
// with their name, severity, summary and start and nothing else of theirs.
func TestMonitoringAlerts(t *testing.T) {
	am := newFakeAlertmanager(t)
	e := newEnv(t, withMonitoring("", am.URL))
	now := e.clock.Now().UTC()
	alert := func(name, severity, summary string, ago time.Duration, node ...string) map[string]any {
		labels := map[string]string{"alertname": name, "severity": severity, "tenant": "zebraco"}
		if len(node) > 0 {
			labels["node"] = node[0]
		}
		return map[string]any{
			"labels":      labels,
			"annotations": map[string]string{"summary": summary, "description": "zebra-vm in zebrapj"},
			"startsAt":    now.Add(-ago).Format(time.RFC3339Nano),
			"status":      map[string]any{"state": "active"},
		}
	}
	am.alerts = []map[string]any{
		alert("Watchdog", "warning", "The alerting pipeline works", 48*time.Hour),
		alert("CertificateRenewalLate", "warning", "Certificate api expires in 12 days", 26*time.Hour),
		alert("MdArrayDegraded", "critical", "md array md127 is degraded", 47*time.Minute, "n1"),
		alert("Unlabelled", "", "A rule with no severity", time.Hour),
	}
	got := must[mon.ListAlertsResponse](t)(e.as(root).monitoring.ListAlerts(t.Context(), connect.NewRequest(&mon.ListAlertsRequest{})))
	want := &mon.ListAlertsResponse{Alerts: []*mon.Alert{
		{Name: "Unlabelled", Severity: mon.Severity_SEVERITY_CRITICAL, Summary: "A rule with no severity", StartTime: timestamppb.New(now.Add(-time.Hour))},
		// A host's alert names its node; the platform's name none.
		{Name: "MdArrayDegraded", Severity: mon.Severity_SEVERITY_CRITICAL, Summary: "md array md127 is degraded", StartTime: timestamppb.New(now.Add(-47 * time.Minute)), Node: "n1"},
		{Name: "CertificateRenewalLate", Severity: mon.Severity_SEVERITY_WARNING, Summary: "Certificate api expires in 12 days", StartTime: timestamppb.New(now.Add(-26 * time.Hour))},
	}}
	if !proto.Equal(got, want) {
		t.Errorf("the alerts:\n got %s\nwant %s", protojson.Format(got), protojson.Format(want))
	}
	noTenant(t, "the alerts", got)
	if asked := am.requests(); len(asked) != 1 || asked[0] != "GET /api/v2/alerts?active=true&inhibited=false&silenced=false&unprocessed=false" {
		t.Errorf("Alertmanager was asked %v", asked)
	}
}
