package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// logBuffer is a log the agent writes while the test reads it.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// runNode starts the built hangar-node, with no drivers and no apiserver,
// with args, and returns it and what it logs.
func runNode(t *testing.T, args ...string) (*exec.Cmd, *logBuffer) {
	t.Helper()
	bin := buildNode(t)
	out := &logBuffer{}
	cmd := exec.Command(bin, append([]string{"--name=n1", "--drivers=none", "--socket=" + filepath.Join(t.TempDir(), "none.sock")}, args...)...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, out
}

// TestNodeMetricsOnLoopbackOnly: an address off loopback stops the agent.
func TestNodeMetricsOnLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "localhost:0"} {
		cmd, out := runNode(t, "--metrics-listen="+addr)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(out.String(), "loopback") {
				t.Errorf("--metrics-listen %s: %v\n%s", addr, err, out)
			}
		// A process that took the address runs until it is stopped.
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Errorf("--metrics-listen %s: the agent kept running\n%s", addr, out)
		}
	}
}

// TestNodeServesMetrics: --metrics-listen serves the agent's metrics.
func TestNodeServesMetrics(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cmd, out := runNode(t, "--metrics-listen="+addr)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	get := func(path string) (int, string, error) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+path, nil)
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
	// The agent has no apiserver: its heartbeats fail.
	testutil.Eventually(t, "a failed heartbeat on /metrics", func() (bool, string) {
		status, body, err := get("/metrics")
		if err != nil {
			return false, fmt.Sprint(err, "\n", out)
		}
		return status == http.StatusOK && strings.Contains(body, "hangar_node_heartbeat_failures_total 1"), body
	})
	if status, _, err := get("/healthz"); err != nil || status != http.StatusNotFound {
		t.Errorf("GET /healthz: %d %v", status, err)
	}
}
