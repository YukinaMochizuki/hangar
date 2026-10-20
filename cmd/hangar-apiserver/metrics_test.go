package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func getURL(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// TestMetricsHaveAListenerOfTheirOwn: /metrics is served on
// --metrics-listen alone, which serves nothing of the API.
func TestMetricsHaveAListenerOfTheirOwn(t *testing.T) {
	addr := freeAddr(t)
	l := start(t, 0, func(f *flags) { f.metricsListen = addr })
	m := "http://" + addr

	testutil.Eventually(t, "the node's Ready on /metrics", func() (bool, string) {
		status, body := getURL(t, m+"/metrics")
		return status == http.StatusOK && strings.Contains(body, `hangar_apiserver_node_ready{node="n1",status="Unknown"} 1`), body
	})
	for _, path := range []string{"/healthz", "/hangar.compute.v1alpha1.MachineService/ListMachines"} {
		if status, _ := getURL(t, m+path); status != http.StatusNotFound {
			t.Errorf("the metrics listener answered GET %s with %d", path, status)
		}
	}
	if status, _ := getURL(t, l.public+"/metrics"); status != http.StatusNotFound {
		t.Errorf("the public listener answered GET /metrics with %d", status)
	}
}

// TestMetricsListenOnLoopbackOnly: an address off loopback stops the
// apiserver.
func TestMetricsListenOnLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:9101", ":9101", "localhost:9101", "[::]:9101"} {
		f := flags{
			db: filepath.Join(t.TempDir(), "hangar.db"), listen: freeAddr(t), authMode: "dev", maxOwnedTenants: 3, metricsListen: addr,
		}
		// An apiserver that took the address serves until this ends.
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		err := run(ctx, f, slog.New(slog.DiscardHandler))
		cancel()
		if err == nil || !strings.Contains(err.Error(), "--metrics-listen") || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("--metrics-listen %s: %v", addr, err)
		}
	}
}
