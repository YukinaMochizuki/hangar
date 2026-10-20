package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/metrics/metricstest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

func TestPullResults(t *testing.T) {
	live := context.Background()
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []struct {
		ctx  context.Context
		err  error
		want string
	}{
		{live, nil, pullResultOK},
		{live, pullFailed(errors.New("connection refused")), pullResultFailed},
		{live, pullFailed(fmt.Errorf("asking: %w", driver.ErrUnauthorized)), pullResultUnauthorized},
		{live, pullFailed(context.DeadlineExceeded), pullResultTimeout},
		{live, fmt.Errorf("unpacking: %w", driver.ErrImageTooLarge), pullResultInvalid},
		{live, blocked(computeapi.ReasonDiskFailed, driver.ErrNoSpace), pullResultDiskFailed},
		{stopped, pullFailed(context.Canceled), pullResultCanceled},
	} {
		if got := pullResult(c.ctx, c.err); got != c.want {
			t.Errorf("%v: %s, want %s", c.err, got, c.want)
		}
	}
}

func TestFailedHeartbeatsAreCounted(t *testing.T) {
	a, err := New(Config{
		Name: "n1", HTTP: client.UnixSocket(filepath.Join(t.TempDir(), "none.sock")), BaseURL: client.UnixSocketURL,
		HeartbeatInterval: time.Millisecond, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	reg := prometheus.NewRegistry()
	reg.MustRegister(a.Collectors()...)
	testutil.Eventually(t, "heartbeats to no apiserver counted as failed", func() (bool, string) {
		v, _ := metricstest.Value(t, reg, "hangar_node_heartbeat_failures_total", map[string]string{})
		return v >= 2, fmt.Sprint(v)
	})
}

func TestReconnectsCountTunnelsAfterTheFirst(t *testing.T) {
	m := newAgentMetrics()
	reg := prometheus.NewRegistry()
	reg.MustRegister(m.tunnelUp, m.tunnelReconnects)
	read := func() (up, reconnects float64) {
		up, _ = metricstest.Value(t, reg, "hangar_node_tunnel_connected", map[string]string{})
		reconnects, _ = metricstest.Value(t, reg, "hangar_node_tunnel_reconnects_total", map[string]string{})
		return up, reconnects
	}
	if up, n := read(); up != 0 || n != 0 {
		t.Errorf("before any tunnel: connected %v, reconnects %v", up, n)
	}
	m.tunnelOpen()
	if up, n := read(); up != 1 || n != 0 {
		t.Errorf("the first tunnel open: connected %v, reconnects %v", up, n)
	}
	m.tunnelClosed()
	if up, n := read(); up != 0 || n != 0 {
		t.Errorf("the first tunnel closed: connected %v, reconnects %v", up, n)
	}
	m.tunnelOpen()
	if up, n := read(); up != 1 || n != 1 {
		t.Errorf("the second tunnel open: connected %v, reconnects %v", up, n)
	}
}

func TestStatusWriteFailuresByCode(t *testing.T) {
	m := newAgentMetrics()
	reg := prometheus.NewRegistry()
	reg.MustRegister(m.statusFailures)
	if err := m.statusWritten(nil); err != nil {
		t.Fatal(err)
	}
	conflict := connect.NewError(connect.CodeAborted, errors.New("the object changed"))
	if err := m.statusWritten(conflict); !errors.Is(err, conflict) {
		t.Errorf("statusWritten returned %v, not the write's error", err)
	}
	_ = m.statusWritten(connect.NewError(connect.CodeUnavailable, errors.New("no apiserver")))
	_ = m.statusWritten(connect.NewError(connect.CodeUnavailable, errors.New("no apiserver")))
	metricstest.CheckLabels(t, reg, "hangar_node_status_report_failures_total")
	for code, want := range map[string]float64{"aborted": 1, "unavailable": 2, "ok": 0} {
		got, _ := metricstest.Value(t, reg, "hangar_node_status_report_failures_total", map[string]string{"code": code})
		if got != want {
			t.Errorf("failures with code %s: %v, want %v", code, got, want)
		}
	}
}
