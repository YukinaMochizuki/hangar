package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMonitoringURLsOnLoopbackOnly: a Prometheus or Alertmanager URL off
// loopback stops the apiserver, naming the flag.
func TestMonitoringURLsOnLoopbackOnly(t *testing.T) {
	for _, u := range []string{"http://0.0.0.0:9090", "http://localhost:9090", "http://192.0.2.1:9090", "http://127.0.0.1", "ftp://127.0.0.1:9090", "http://u:p@127.0.0.1:9090"} {
		for _, flag := range []string{"--prometheus-url", "--alertmanager-url"} {
			f := flags{db: filepath.Join(t.TempDir(), "hangar.db"), listen: freeAddr(t), authMode: "dev", maxOwnedTenants: 3}
			if flag == "--prometheus-url" {
				f.prometheusURL = u
			} else {
				f.alertmanagerURL = u
			}
			// An apiserver that took the URL serves until this ends.
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			err := run(ctx, f, slog.New(slog.DiscardHandler))
			cancel()
			if err == nil || !strings.Contains(err.Error(), flag) {
				t.Errorf("%s %s: %v", flag, u, err)
			}
		}
	}
}
