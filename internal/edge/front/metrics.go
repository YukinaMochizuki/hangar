package front

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/YukinaMochizuki/hangar/internal/edge"
)

// The routes the front door's requests are counted by: the API, on its
// own site or on the console's; the console's files; the public endpoints.
// Never an endpoint's hostname (docs/decisions/monitoring.md).
const (
	routeControlPlane = "control-plane"
	routeConsole      = "console"
	routeEndpoint     = "endpoint"
)

// frontMetrics count the requests the front door takes over TLS for one of
// its sites.
type frontMetrics struct {
	requests       *prometheus.CounterVec
	duration       *prometheus.HistogramVec
	upstreamErrors *prometheus.CounterVec
}

func newFrontMetrics() *frontMetrics {
	m := &frontMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_edge_requests_total",
			Help: "Requests the front door answered over TLS for one of its sites, by route (control-plane, the API on either site; console, its files; endpoint) and HTTP status class.",
		}, []string{"route", "status_class"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "hangar_edge_request_duration_seconds",
			Help:    "Time from a request's arrival to its response's header, or to the switch of an upgrade, by route.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 100},
		}, []string{"route"}),
		upstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_edge_upstream_errors_total",
			Help: "Requests the front door answered itself, with 502 or 503, as what it passes them to did not answer: the apiserver, or an endpoint's machine.",
		}, []string{"route"}),
	}
	for _, r := range []string{routeControlPlane, routeConsole, routeEndpoint} {
		m.upstreamErrors.WithLabelValues(r)
	}
	return m
}

// Collectors are the front door's metrics, those of certs among them, for
// the registry the edge's metrics listener serves.
func (f *Front) Collectors(certs *Certs) []prometheus.Collector {
	return []prometheus.Collector{f.metrics.requests, f.metrics.duration, f.metrics.upstreamErrors, certCollector{f: f, certs: certs}}
}

// routeOf is the route r is counted by; false for none of the sites.
func (f *Front) routeOf(r *http.Request) (string, bool) {
	switch f.siteOf(r.Host) {
	case apiSite:
		return routeControlPlane, true
	case consoleSite:
		if isAPIPath(r.URL.Path) {
			return routeControlPlane, true
		}
		return routeConsole, true
	case endpointSite:
		return routeEndpoint, true
	}
	return "", false
}

// measure counts each request to one of the front door's sites, with what
// answered it and how soon, around next.
func (f *Front) measure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, ok := f.routeOf(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		rec := &recorder{ResponseWriter: w, start: time.Now()}
		r = r.WithContext(edge.WithUpstreamFailed(r.Context(), func() {
			f.metrics.upstreamErrors.WithLabelValues(route).Inc()
		}))
		next.ServeHTTP(rec, r)
		status, took := rec.result()
		f.metrics.requests.WithLabelValues(route, fmt.Sprintf("%dxx", status/100)).Inc()
		f.metrics.duration.WithLabelValues(route).Observe(took.Seconds())
	})
}

// recorder notes a response's status and when its header went. A request
// whose handler wrote nothing was answered 200, as net/http does.
type recorder struct {
	http.ResponseWriter
	start  time.Time
	status int
	took   time.Duration
}

func (r *recorder) note(status int) {
	if r.status == 0 {
		r.status, r.took = status, time.Since(r.start)
	}
}

func (r *recorder) WriteHeader(code int) {
	// An informational header but the switch of an upgrade comes before the
	// response's own.
	if code >= 200 || code == http.StatusSwitchingProtocols {
		r.note(code)
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	r.note(http.StatusOK)
	return r.ResponseWriter.Write(p)
}

// Hijack is how an upgrade's proxy takes the connection, writing the
// switch on it itself.
func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(r.ResponseWriter).Hijack()
	if err == nil {
		r.note(http.StatusSwitchingProtocols)
	}
	return c, rw, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) result() (int, time.Duration) {
	r.note(http.StatusOK)
	return r.status, r.took
}

// certLabels are the front door's certificates by the name they are
// counted by, never a hostname.
func (f *Front) certLabels() map[string]string {
	return map[string]string{
		"api":           "api." + f.cfg.CPDomain,
		"console":       "console." + f.cfg.CPDomain,
		"wildcard_edge": "*." + f.cfg.UserDomain,
	}
}

var certExpiryDesc = prometheus.NewDesc("hangar_edge_certificate_expiry_timestamp_seconds",
	"When the certificate the front door holds for each of its names expires, in Unix seconds; 0 while it holds none.",
	[]string{"cert"}, nil)

// certCollector reads the certificates' expiry at each scrape.
type certCollector struct {
	f     *Front
	certs *Certs
}

func (c certCollector) Describe(ch chan<- *prometheus.Desc) { ch <- certExpiryDesc }

func (c certCollector) Collect(ch chan<- prometheus.Metric) {
	for label, name := range c.f.certLabels() {
		v := 0.0
		if t, ok := c.certs.NotAfter(name); ok {
			v = float64(t.Unix())
		}
		ch <- prometheus.MustNewConstMetric(certExpiryDesc, prometheus.GaugeValue, v, label)
	}
}
