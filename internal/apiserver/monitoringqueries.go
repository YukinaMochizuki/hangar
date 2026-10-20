package apiserver

import (
	"cmp"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	monitoringv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/monitoring/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

// The PromQL MonitoringService asks, each fixed here and never a caller's
// (docs/decisions/monitoring.md). The metric names are those
// deploy/host/steps/90-monitoring.sh has Prometheus scrape (node_exporter,
// smartctl_exporter, blackbox_exporter, hangar-zpool-metrics) and Hangar's
// own (internal/apiserver/metrics.go, the node agent's and the edge's). The
// thresholds a check turns WARNING or CRITICAL at are those of the alerting
// rules, deploy/host/monitoring/rules/host.yml, so the overview and the
// alerts agree.
const (
	// The HTTPS probes of the API's /healthz and the console, by URL.
	qProbeSuccess  = `probe_success{job="blackbox"}`
	qProbeStatus   = `probe_http_status_code{job="blackbox"}`
	qProbeDuration = `probe_duration_seconds{job="blackbox"}`
	// Days left on the certificate each probed URL serves.
	qCertDaysLeft = `(probe_ssl_earliest_cert_expiry{job="blackbox"} - time()) / 86400`

	qCPUBusy = `1 - avg(rate(node_cpu_seconds_total{mode="idle"}[5m]))`
	qLoad1   = `max(node_load1)`
	// What the kernel can still hand out, as HostMemoryLow counts it:
	// MemAvailable, and the ARC it would give back down to its minimum.
	qMemTotal     = `sum(node_memory_MemTotal_bytes)`
	qMemAvailable = `sum(node_memory_MemAvailable_bytes) + (sum(clamp_min(node_zfs_arc_size - node_zfs_arc_c_min, 0)) or vector(0))`
	qSwapUsed     = `sum(node_memory_SwapTotal_bytes - node_memory_SwapFree_bytes)`

	// Each pool's state, by the state that holds.
	qPoolState        = `node_zfs_zpool_state == 1`
	qPoolSize         = `hangar_zpool_size_bytes`
	qPoolAllocated    = `hangar_zpool_allocated_bytes`
	qPoolDataErrors   = `hangar_zpool_data_errors`
	qPoolDeviceErrors = `sum by (zpool) (hangar_zpool_device_errors)`
	qImagesUsed       = `1 - node_filesystem_avail_bytes{mountpoint="/var/lib/hangar/images"} / node_filesystem_size_bytes{mountpoint="/var/lib/hangar/images"}`

	// Each md array's state, its active and its required disks, and, as
	// MdArrayDegraded has it, those degraded.
	qMdState    = `node_md_state == 1`
	qMdActive   = `node_md_disks{state="active"}`
	qMdRequired = `node_md_disks_required`
	qMdDegraded = `max without (state) (node_md_disks{state="failed"} > 0 or node_md_disks_required - ignoring(state) node_md_disks{state="active"} > 0 or node_md_state{state="inactive"} == 1)`

	// Each NVMe disk's SMART, by device.
	qSmartStatus          = `smartctl_device_smart_status`
	qSmartCriticalWarning = `smartctl_device_critical_warning`
	qSmartMediaErrors     = `smartctl_device_media_errors`
	qSmartWear            = `smartctl_device_percentage_used`
	qSmartTemperature     = `smartctl_device_temperature{temperature_type="current"}`

	qTimeSynced = `max(node_timex_sync_status)`
	qTimeOffset = `max(abs(node_timex_offset_seconds))`

	// Each hangar-* service node_exporter reports: whether it is active,
	// and its automatic restarts in a day and, as UnitRestartingOften
	// counts them, in an hour. A difference, since increase() extrapolates.
	qUnitActive     = `node_systemd_unit_state{state="active",name=~"hangar-.+[.]service"}`
	qUnitRestarts1d = `clamp_min(node_systemd_service_restart_total{name=~"hangar-.+[.]service"} - node_systemd_service_restart_total{name=~"hangar-.+[.]service"} offset 1d, 0)`
	qUnitRestarts1h = `clamp_min(node_systemd_service_restart_total{name=~"hangar-.+[.]service"} - node_systemd_service_restart_total{name=~"hangar-.+[.]service"} offset 1h, 0)`

	// Each Node's Ready, by the status that holds; the node agent's tunnel.
	qNodeReady = `hangar_apiserver_node_ready == 1`
	qTunnel    = `max(hangar_node_tunnel_connected)`

	// Machines, counted: by phase, by the reasons the overview tells apart,
	// and in a crash loop. The apiserver reports no machines series while
	// there are none, so the crash-loop gauge, always reported, says
	// whether Prometheus has the apiserver's metrics at all.
	qMachinePhases  = `sum by (phase) (hangar_apiserver_machines)`
	qMachineReasons = `sum by (reason) (hangar_apiserver_machines{reason=~"DiskFailed|NetworkFailed|Exited|BootFailed"})`
	qCrashLooping   = `max(hangar_apiserver_machines_crash_looping)`
)

// overviewQueries are every query GetOverview asks, each once.
var overviewQueries = []string{
	qProbeSuccess, qProbeStatus, qProbeDuration, qCertDaysLeft,
	qCPUBusy, qLoad1, qMemTotal, qMemAvailable, qSwapUsed,
	qPoolState, qPoolSize, qPoolAllocated, qPoolDataErrors, qPoolDeviceErrors, qImagesUsed,
	qMdState, qMdActive, qMdRequired, qMdDegraded,
	qSmartStatus, qSmartCriticalWarning, qSmartMediaErrors, qSmartWear, qSmartTemperature,
	qTimeSynced, qTimeOffset,
	qUnitActive, qUnitRestarts1d, qUnitRestarts1h,
	qNodeReady, qTunnel,
	qMachinePhases, qMachineReasons, qCrashLooping,
}

// Machine reasons the overview counts apart: those only the platform causes,
// which alert, and those of a tenant's own program, which alert no one. They
// match qMachineReasons.
var (
	platformReasons = []string{computeapi.ReasonDiskFailed, computeapi.ReasonNetworkFailed}
	programReasons  = []string{computeapi.ReasonExited, computeapi.ReasonBootFailed}
)

// machinePhases are the phases the overview counts, in its order, as the
// apiserver's metrics name them.
var machinePhases = []string{"Running", "Pending", "Creating", "Stopped", "Suspended", "Failed"}

// seriesRange is a SeriesRange's span and the step the server picks for it.
type seriesRange struct {
	span, step time.Duration
}

var seriesRanges = map[monitoringv1alpha1.SeriesRange]seriesRange{
	monitoringv1alpha1.SeriesRange_SERIES_RANGE_ONE_HOUR:   {time.Hour, time.Minute},
	monitoringv1alpha1.SeriesRange_SERIES_RANGE_ONE_DAY:    {24 * time.Hour, 15 * time.Minute},
	monitoringv1alpha1.SeriesRange_SERIES_RANGE_SEVEN_DAYS: {7 * 24 * time.Hour, time.Hour},
}

// minRateWindow is the shortest window a series' rate is taken over: four
// of Prometheus's 30 s scrapes and more. A longer step takes the step, so
// no sample falls between two points.
const minRateWindow = 5 * time.Minute

// seriesDef is one SeriesName's query.
type seriesDef struct {
	// query is the PromQL; $window stands for the rate window.
	query string
	unit  monitoringv1alpha1.Unit
	// capacity, if set, is an instant query of what the value is out of,
	// asked at the range's end; alertAboveOf turns it into where an alert
	// fires.
	capacity     string
	alertAboveOf func(capacity float64) float64
	// alertAbove is where an alert fires on a value with no capacity; zero
	// is none.
	alertAbove float64
}

// promQL is the query for a range, its rate window filled in.
func (d seriesDef) promQL(r seriesRange) string {
	w := max(r.step, minRateWindow)
	return strings.ReplaceAll(d.query, "$window", strconv.FormatInt(int64(w/time.Second), 10)+"s")
}

const gib = 1 << 30

// apiServerErrorCodes are the Connect codes that are the server's failing,
// as the rpc metrics label them; the others are the caller's.
const apiServerErrorCodes = `internal|unknown|unavailable|data_loss|deadline_exceeded`

var seriesDefs = map[monitoringv1alpha1.SeriesName]seriesDef{
	monitoringv1alpha1.SeriesName_SERIES_NAME_HOST_CPU: {
		query: `1 - avg(rate(node_cpu_seconds_total{mode="idle"}[$window]))`,
		unit:  monitoringv1alpha1.Unit_UNIT_RATIO,
	},
	monitoringv1alpha1.SeriesName_SERIES_NAME_HOST_MEMORY: {
		query:    qMemTotal + ` - (` + qMemAvailable + `)`,
		unit:     monitoringv1alpha1.Unit_UNIT_BYTES,
		capacity: qMemTotal,
		// HostMemoryLow: less than 1 GiB left.
		alertAboveOf: func(total float64) float64 { return total - gib },
	},
	monitoringv1alpha1.SeriesName_SERIES_NAME_POOL_USED: {
		query:    `sum(hangar_zpool_allocated_bytes)`,
		unit:     monitoringv1alpha1.Unit_UNIT_BYTES,
		capacity: `sum(hangar_zpool_size_bytes)`,
		// ZpoolAlmostFull: over 90%.
		alertAboveOf: func(size float64) float64 { return 0.9 * size },
	},
	monitoringv1alpha1.SeriesName_SERIES_NAME_API_REQUEST_RATE: {
		query: `sum(rate(hangar_apiserver_rpc_requests_total[$window]))`,
		unit:  monitoringv1alpha1.Unit_UNIT_PER_SECOND,
	},
	monitoringv1alpha1.SeriesName_SERIES_NAME_API_ERROR_RATE: {
		query: `(sum(rate(hangar_apiserver_rpc_requests_total{code=~"` + apiServerErrorCodes + `"}[$window])) or vector(0)) / sum(rate(hangar_apiserver_rpc_requests_total[$window]))`,
		unit:  monitoringv1alpha1.Unit_UNIT_RATIO,
	},
	monitoringv1alpha1.SeriesName_SERIES_NAME_API_LATENCY_P99: {
		query: `histogram_quantile(0.99, sum by (le) (rate(hangar_apiserver_rpc_duration_seconds_bucket[$window])))`,
		unit:  monitoringv1alpha1.Unit_UNIT_SECONDS,
	},
}

// results are the overview's answers, by query.
type results map[string][]sample

// first is the value of the first series, if there is one.
func first(s []sample) (float64, bool) {
	if len(s) == 0 {
		return 0, false
	}
	return s[0].value, true
}

// by maps each series' value of the query by one label.
func (r results) by(q, label string) map[string]float64 {
	out := map[string]float64{}
	for _, s := range r[q] {
		out[s.labels[label]] = s.value
	}
	return out
}

// keys are a map's keys, sorted.
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

var healthRank = map[monitoringv1alpha1.Health]int{
	monitoringv1alpha1.Health_HEALTH_OK:       0,
	monitoringv1alpha1.Health_HEALTH_INFO:     1,
	monitoringv1alpha1.Health_HEALTH_UNKNOWN:  2,
	monitoringv1alpha1.Health_HEALTH_WARNING:  3,
	monitoringv1alpha1.Health_HEALTH_CRITICAL: 4,
}

func worse(a, b monitoringv1alpha1.Health) monitoringv1alpha1.Health {
	if healthRank[b] > healthRank[a] {
		return b
	}
	return a
}

func layer(l monitoringv1alpha1.Layer, checks []*monitoringv1alpha1.Check) *monitoringv1alpha1.LayerState {
	h := monitoringv1alpha1.Health_HEALTH_OK
	for _, c := range checks {
		h = worse(h, c.GetHealth())
	}
	return &monitoringv1alpha1.LayerState{Layer: l, Health: h, Checks: checks}
}

func check(name string, h monitoringv1alpha1.Health, format string, args ...any) *monitoringv1alpha1.Check {
	return &monitoringv1alpha1.Check{Name: name, Health: h, Value: fmt.Sprintf(format, args...)}
}

// noData is the check of something Prometheus has nothing on.
func noData(name string) *monitoringv1alpha1.Check {
	return check(name, monitoringv1alpha1.Health_HEALTH_UNKNOWN, "no data")
}

const (
	healthOK       = monitoringv1alpha1.Health_HEALTH_OK
	healthWarning  = monitoringv1alpha1.Health_HEALTH_WARNING
	healthCritical = monitoringv1alpha1.Health_HEALTH_CRITICAL
)

func reachabilityLayer(r results) *monitoringv1alpha1.LayerState {
	var checks []*monitoringv1alpha1.Check
	success := r.by(qProbeSuccess, "instance")
	status := r.by(qProbeStatus, "instance")
	duration := r.by(qProbeDuration, "instance")
	for _, u := range keys(success) {
		if success[u] == 1 {
			checks = append(checks, check(probeName(u), healthOK, "%.0f · %s", status[u], millis(duration[u])))
		} else {
			checks = append(checks, check(probeName(u), healthCritical, "failing · %.0f", status[u]))
		}
	}
	days := r.by(qCertDaysLeft, "instance")
	for _, u := range keys(days) {
		d := days[u]
		// CertificateExpiresSoon and CertificateRenewalLate.
		h := healthOK
		switch {
		case d < 7:
			h = healthCritical
		case d < 14:
			h = healthWarning
		}
		checks = append(checks, check("Certificate "+hostLabel(u), h, "%s left", plural(int(math.Floor(d)), "day")))
	}
	if len(checks) == 0 {
		checks = append(checks, noData("HTTPS probes"))
	}
	return layer(monitoringv1alpha1.Layer_LAYER_REACHABILITY, checks)
}

func hostLayer(r results) *monitoringv1alpha1.LayerState {
	var checks []*monitoringv1alpha1.Check
	if cpu, has := first(r[qCPUBusy]); has {
		v := fmt.Sprintf("%.0f%%", cpu*100)
		if load, has := first(r[qLoad1]); has {
			v += fmt.Sprintf(" · load %.1f", load)
		}
		checks = append(checks, check("CPU", healthOK, "%s", v))
	} else {
		checks = append(checks, noData("CPU"))
	}
	total, hasTotal := first(r[qMemTotal])
	avail, hasAvail := first(r[qMemAvailable])
	if hasTotal && hasAvail {
		// HostMemoryLow: less than 1 GiB left.
		h := healthOK
		if avail < gib {
			h = healthCritical
		}
		checks = append(checks, check("Memory", h, "%s", bytesOf(total-avail, total)))
	} else {
		checks = append(checks, noData("Memory"))
	}
	if swap, has := first(r[qSwapUsed]); has {
		checks = append(checks, check("Swap", healthOK, "%s used", bytesText(swap)))
	} else {
		checks = append(checks, noData("Swap"))
	}
	checks = append(checks, poolChecks(r)...)
	if used, has := first(r[qImagesUsed]); has {
		// ImagesQuotaAlmostFull: over 90%.
		h := healthOK
		if used > 0.9 {
			h = healthCritical
		}
		checks = append(checks, check("hangar/images", h, "%.0f%% of quota", used*100))
	} else {
		checks = append(checks, noData("hangar/images"))
	}
	checks = append(checks, mdChecks(r)...)
	checks = append(checks, diskChecks(r)...)
	synced, hasSynced := first(r[qTimeSynced])
	offset, _ := first(r[qTimeOffset])
	switch {
	case !hasSynced:
		checks = append(checks, noData("Time sync"))
	case synced == 1 && offset <= 0.5:
		checks = append(checks, check("Time sync", healthOK, "synced · %s", millis(offset)))
	default:
		// ClockNotSynchronised.
		checks = append(checks, check("Time sync", healthWarning, "not synced · %s", millis(offset)))
	}
	return layer(monitoringv1alpha1.Layer_LAYER_HOST, checks)
}

func poolChecks(r results) []*monitoringv1alpha1.Check {
	states := map[string]string{}
	for _, s := range r[qPoolState] {
		states[s.labels["zpool"]] = s.labels["state"]
	}
	size := r.by(qPoolSize, "zpool")
	alloc := r.by(qPoolAllocated, "zpool")
	dataErrors := r.by(qPoolDataErrors, "zpool")
	deviceErrors := r.by(qPoolDeviceErrors, "zpool")
	pools := map[string]bool{}
	for _, m := range []map[string]float64{size, alloc} {
		for p := range m {
			pools[p] = true
		}
	}
	for p := range states {
		pools[p] = true
	}
	if len(pools) == 0 {
		return []*monitoringv1alpha1.Check{noData("ZFS pools")}
	}
	var out []*monitoringv1alpha1.Check
	for _, p := range keys(pools) {
		h := healthOK
		state := strings.ToUpper(cmp.Or(states[p], "unknown"))
		parts := []string{state}
		// ZpoolNotOnline.
		if states[p] != "online" {
			h = healthCritical
		}
		if size[p] > 0 {
			used := alloc[p] / size[p]
			// ZpoolAlmostFull and ZpoolFilling.
			switch {
			case used > 0.9:
				h = worse(h, healthCritical)
			case used > 0.8:
				h = worse(h, healthWarning)
			}
			parts = append(parts, bytesOf(alloc[p], size[p]), fmt.Sprintf("%.0f%%", used*100))
		}
		// ZpoolDataErrors and ZpoolDeviceErrors.
		if n := dataErrors[p]; n > 0 {
			h = worse(h, healthCritical)
			parts = append(parts, plural(int(n), "data error"))
		}
		if n := deviceErrors[p]; n > 0 {
			h = worse(h, healthWarning)
			parts = append(parts, plural(int(n), "device error"))
		}
		out = append(out, check("Pool "+p, h, "%s", strings.Join(parts, " · ")))
	}
	return out
}

func mdChecks(r results) []*monitoringv1alpha1.Check {
	states := map[string]string{}
	for _, s := range r[qMdState] {
		states[s.labels["device"]] = s.labels["state"]
	}
	active := r.by(qMdActive, "device")
	required := r.by(qMdRequired, "device")
	degraded := r.by(qMdDegraded, "device")
	if len(required) == 0 {
		return []*monitoringv1alpha1.Check{noData("md mirror")}
	}
	var out []*monitoringv1alpha1.Check
	for _, d := range keys(required) {
		disks := fmt.Sprintf("%.0f of %.0f disks", active[d], required[d])
		// MdArrayDegraded.
		if _, bad := degraded[d]; bad {
			out = append(out, check(d+" mirror", healthCritical, "degraded · %s", disks))
		} else {
			out = append(out, check(d+" mirror", healthOK, "%s · %s", cmp.Or(states[d], "unknown"), disks))
		}
	}
	return out
}

func diskChecks(r results) []*monitoringv1alpha1.Check {
	status := r.by(qSmartStatus, "device")
	if len(status) == 0 {
		return []*monitoringv1alpha1.Check{noData("NVMe disks")}
	}
	criticalWarning := r.by(qSmartCriticalWarning, "device")
	media := r.by(qSmartMediaErrors, "device")
	wear := r.by(qSmartWear, "device")
	temp := r.by(qSmartTemperature, "device")
	var out []*monitoringv1alpha1.Check
	for _, d := range keys(status) {
		h := healthOK
		parts := []string{"PASSED"}
		// SmartFailed.
		if status[d] == 0 || criticalWarning[d] > 0 {
			h, parts[0] = healthCritical, "FAILED"
		}
		if w, has := wear[d]; has {
			// SmartWearHigh.
			if w > 80 {
				h = worse(h, healthWarning)
			}
			parts = append(parts, fmt.Sprintf("wear %.0f%%", w))
		}
		if t, has := temp[d]; has {
			// DiskTemperatureHigh.
			if t > 70 {
				h = worse(h, healthWarning)
			}
			parts = append(parts, fmt.Sprintf("%.0f °C", t))
		}
		// SmartMediaErrors.
		if n := media[d]; n > 0 {
			h = worse(h, healthCritical)
			parts = append(parts, plural(int(n), "media error"))
		}
		out = append(out, check(d, h, "%s", strings.Join(parts, " · ")))
	}
	return out
}

func servicesLayer(r results) *monitoringv1alpha1.LayerState {
	var checks []*monitoringv1alpha1.Check
	active := r.by(qUnitActive, "name")
	day := r.by(qUnitRestarts1d, "name")
	hour := r.by(qUnitRestarts1h, "name")
	if len(active) == 0 {
		checks = append(checks, noData("hangar-* units"))
	}
	for _, u := range keys(active) {
		restarts := plural(int(day[u]), "restart") + " 24 h"
		name := strings.TrimSuffix(u, ".service")
		switch {
		case active[u] != 1:
			// UnitNotActive.
			checks = append(checks, check(name, healthCritical, "not active · %s", restarts))
		case hour[u] > 3:
			// UnitRestartingOften.
			checks = append(checks, check(name, healthWarning, "active · %s", restarts))
		default:
			checks = append(checks, check(name, healthOK, "active · %s", restarts))
		}
	}
	ready := map[string]string{}
	for _, s := range r[qNodeReady] {
		ready[s.labels["node"]] = s.labels["status"]
	}
	if len(ready) == 0 {
		checks = append(checks, noData("Nodes"))
	}
	for _, n := range keys(ready) {
		if ready[n] == "True" {
			checks = append(checks, check("Node "+n, healthOK, "Ready"))
		} else {
			checks = append(checks, check("Node "+n, healthCritical, "Ready %s", ready[n]))
		}
	}
	switch v, has := first(r[qTunnel]); {
	case !has:
		checks = append(checks, noData("Node tunnel"))
	case v == 1:
		checks = append(checks, check("Node tunnel", healthOK, "connected"))
	default:
		checks = append(checks, check("Node tunnel", healthCritical, "disconnected"))
	}
	return layer(monitoringv1alpha1.Layer_LAYER_SERVICES, checks)
}

func machinesLayer(r results) *monitoringv1alpha1.LayerState {
	crash, has := first(r[qCrashLooping])
	if !has {
		return layer(monitoringv1alpha1.Layer_LAYER_MACHINES, []*monitoringv1alpha1.Check{noData("Machines")})
	}
	phases := r.by(qMachinePhases, "phase")
	reasons := r.by(qMachineReasons, "reason")
	m := &monitoringv1alpha1.MachineCounts{CrashLooping: int32(crash)}
	for _, p := range machinePhases {
		m.Phases = append(m.Phases, &monitoringv1alpha1.PhaseCount{Phase: p, Count: int32(phases[p])})
	}
	count := func(rs []string) []*monitoringv1alpha1.ReasonCount {
		var out []*monitoringv1alpha1.ReasonCount
		for _, reason := range rs {
			if n := int32(reasons[reason]); n > 0 {
				out = append(out, &monitoringv1alpha1.ReasonCount{Reason: reason, Count: n})
			}
		}
		return out
	}
	m.PlatformFailures = count(platformReasons)
	m.ProgramFailures = count(programReasons)
	l := layer(monitoringv1alpha1.Layer_LAYER_MACHINES, nil)
	l.Machines = m
	if len(m.PlatformFailures) > 0 {
		l.Health = healthWarning
	}
	return l
}

// probeName names a probed URL as the console does: "API /healthz" for
// https://api.<domain>/healthz, "Console" for https://console.<domain>/.
func probeName(instance string) string {
	label := hostLabel(instance)
	name := label
	switch label {
	case "api":
		name = "API"
	case "console":
		name = "Console"
	}
	if u, err := url.Parse(instance); err == nil && u.Path != "" && u.Path != "/" {
		name += " " + u.Path
	}
	return name
}

// hostLabel is the first label of a probed URL's host, such as api.
func hostLabel(instance string) string {
	u, err := url.Parse(instance)
	if err != nil || u.Hostname() == "" {
		return instance
	}
	h, _, _ := strings.Cut(u.Hostname(), ".")
	return h
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// millis is a duration in seconds as milliseconds, to a tenth below 10.
func millis(seconds float64) string {
	ms := seconds * 1000
	if ms < 10 {
		return strconv.FormatFloat(ms, 'f', 1, 64) + " ms"
	}
	return strconv.FormatFloat(ms, 'f', 0, 64) + " ms"
}

var byteUnits = []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}

// byteUnit is the largest unit v holds one of, and its size.
func byteUnit(v float64) (string, float64) {
	i, size := 0, 1.0
	for i < len(byteUnits)-1 && v >= size*1024 {
		i, size = i+1, size*1024
	}
	return byteUnits[i], size
}

// scaled is v in units of size: to a tenth below 100, whole above.
func scaled(v, size float64) string {
	x := v / size
	if x < 100 && size > 1 {
		return strconv.FormatFloat(x, 'f', 1, 64)
	}
	return strconv.FormatFloat(x, 'f', 0, 64)
}

// bytesText is a byte count, such as "0 B" or "1.5 GiB".
func bytesText(v float64) string {
	unit, size := byteUnit(v)
	return scaled(v, size) + " " + unit
}

// bytesOf is a part of a whole in the whole's unit, such as "74.2 of 128 GiB".
func bytesOf(part, whole float64) string {
	unit, size := byteUnit(whole)
	return scaled(part, size) + " of " + scaled(whole, size) + " " + unit
}
