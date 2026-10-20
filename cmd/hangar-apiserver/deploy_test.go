package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheServicesStepFindsMetricsListen: deploy/host's services step passes
// the apiserver --metrics-listen only when the apiserver's answer to -h
// names it (services_metrics_probe); this runs that shell code, with bash,
// against this very hangar-apiserver, built here.
func TestTheServicesStepFindsMetricsListen(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "hangar-apiserver"), ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	host, err := filepath.Abs("../../deploy/host")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", `set -euo pipefail
. "$1/lib.sh"
. "$1/steps/80-services.sh"
SERVICES_INSTALLED_BIN=$2
services_metrics_probe hangar-apiserver
services_metrics_flag hangar-apiserver "$SERVICES_APISERVER_METRICS"
`, "bash", host, dir)
	cmd.Env = append(os.Environ(), "HANGAR_RELEASE_DIR=")
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "has\n --metrics-listen 127.0.0.1:9101" {
		t.Errorf("the services step on this hangar-apiserver: %v, %q; want has, and the flag", err, out)
	}
}

// TestTheServicesStepPassesMonitoringURLs: deploy/host's services step
// passes the apiserver --prometheus-url and --alertmanager-url, at the
// addresses the monitoring step has them listen on, only when the
// apiserver's answer to -h names them (services_monitoring_probe): this
// very hangar-apiserver, built here, does; one from before them gets
// neither, and the step warns.
func TestTheServicesStepPassesMonitoringURLs(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	now, old := t.TempDir(), t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(now, "hangar-apiserver"), ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	// What a hangar-apiserver from before the flags answers -h with.
	if err := os.WriteFile(filepath.Join(old, "hangar-apiserver"), []byte("#!/bin/sh\necho 'Usage of hangar-apiserver:' >&2\necho '  -metrics-listen string' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	host, err := filepath.Abs("../../deploy/host")
	if err != nil {
		t.Fatal(err)
	}
	monitoring, err := os.ReadFile(filepath.Join(host, "steps/90-monitoring.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// The URLs are where the monitoring step has Prometheus and
	// Alertmanager listen.
	const prometheus, alertmanager = "127.0.0.1:9090", "127.0.0.1:9093"
	for _, l := range []string{"MON_PROM_LISTEN=" + prometheus, "MON_AM_LISTEN=" + alertmanager} {
		if !strings.Contains(string(monitoring), "\n"+l+"\n") {
			t.Errorf("90-monitoring.sh has no %s", l)
		}
	}
	for bin, want := range map[string]string{
		now: "[ --prometheus-url http://" + prometheus + " --alertmanager-url http://" + alertmanager + "]\n",
		old: "warning: the hangar-apiserver this run installs has no --prometheus-url: it runs without it, and the console's Monitoring page answers Unavailable\n[]\n",
	} {
		cmd := exec.Command("bash", "-c", `set -euo pipefail
. "$1/lib.sh"
. "$1/steps/80-services.sh"
SERVICES_INSTALLED_BIN=$2
services_monitoring_note
printf '[%s]\n' "$(services_monitoring_flags)"
`, "bash", host, bin)
		cmd.Env = append(os.Environ(), "HANGAR_RELEASE_DIR=")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.HasSuffix(string(out), want) {
			t.Errorf("the services step on %s: %v, %q; want it to end %q", bin, err, out, want)
		}
	}
}
