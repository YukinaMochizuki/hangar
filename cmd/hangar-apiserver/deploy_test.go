package main

import (
	"os"
	"os/exec"
	"path/filepath"
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
