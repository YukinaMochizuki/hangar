// Package metrics serves a Hangar binary's Prometheus metrics on a loopback
// listener of its own, never on an API's mux, and names the labels a metric
// may carry (docs/decisions/monitoring.md). It defines no binary's metrics;
// each binary registers its own.
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Path is the only path a metrics listener serves.
const Path = "/metrics"

// Labels are the label names a Hangar metric may carry. Each holds values
// the platform defines and bounds; none holds a tenant's, project's,
// machine's or endpoint's name, a uid, a hostname, an address or an email.
// metricstest.CheckLabels holds every registered metric to this list.
var Labels = []string{
	// An RPC's procedure, as the server registered it, and its Connect code.
	"procedure", "code",
	// A machine's phase and its Ready reason.
	"phase", "reason",
	// A Node's name, which the platform's operator gives it, and its Ready
	// condition's status.
	"node", "status",
	// Which of the edge's routes served a request (the control plane, the
	// console or the endpoints), and its HTTP status class, such as 2xx.
	"route", "status_class",
	// The name of one of the edge's certificate configurations, such as api.
	"cert",
	// How an operation of the node agent ended, such as an image pull.
	"result",
	// Histogram buckets and summary quantiles.
	"le", "quantile",
	// The Go version, on go_info.
	"version",
}

// NewRegistry returns a registry holding the Go runtime's and the process's
// metrics, for a binary to register its own in.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return reg
}

// CheckAddr refuses an address whose host is not a loopback IP: metrics are
// for the host's own Prometheus. Not localhost, which a hosts file can point
// anywhere.
func CheckAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("metrics address %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("metrics are served on a loopback IP only, such as 127.0.0.1:9101 or [::1]:9101, not %q", addr)
	}
	return nil
}

// Listen listens on addr, which must pass CheckAddr.
func Listen(ctx context.Context, addr string) (net.Listener, error) {
	if err := CheckAddr(addr); err != nil {
		return nil, err
	}
	return (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
}

// Handler serves g at Path and nothing else.
func Handler(g prometheus.Gatherer) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET "+Path, promhttp.HandlerFor(g, promhttp.HandlerOpts{}))
	return mux
}

// Serve serves Handler(g) on ln until ctx is done, then closes ln.
func Serve(ctx context.Context, ln net.Listener, g prometheus.Gatherer, log *slog.Logger) error {
	srv := &http.Server{Handler: Handler(g), ReadHeaderTimeout: 10 * time.Second}
	log.Info("serving metrics", "address", ln.Addr().String())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
	}
	return nil
}
