package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// TestEdgeServesMetrics: --metrics-listen serves /metrics and nothing else.
// The edge refuses to run as root, so this runs as a regular user, as CI
// does.
func TestEdgeServesMetrics(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the edge never runs as root")
	}
	free := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		return ln.Addr().String()
	}
	addr := free()
	dir := t.TempDir()
	f := flags{
		listen: free(), domain: "user.example.com", consoleURL: "https://console.example.com", metricsListen: addr,
		apiSocket: filepath.Join(dir, "edge.sock"), dialSocket: filepath.Join(dir, "dial.sock"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, f, slog.New(slog.DiscardHandler)) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	})
	get := func(path string) (int, string, error) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+path, nil)
		if err != nil {
			return 0, "", err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), err
	}
	testutil.Eventually(t, "/metrics served", func() (bool, string) {
		status, body, err := get("/metrics")
		return err == nil && status == http.StatusOK && strings.Contains(body, "go_goroutines"), fmt.Sprint(status, err)
	})
	if status, _, err := get("/healthz"); err != nil || status != http.StatusNotFound {
		t.Errorf("GET /healthz: %d %v", status, err)
	}
}
