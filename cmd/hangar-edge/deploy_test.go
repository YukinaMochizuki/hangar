package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The edge step of deploy/host has hangar-edge check-flags check the flags
// the unit would get before it changes the unit; these tests run that shell
// code, with bash, against this very hangar-edge, built here, and against a
// stand-in for one from before check-flags.

var built struct {
	once sync.Once
	dir  string
	err  error
	out  []byte
}

func TestMain(m *testing.M) {
	code := m.Run()
	if built.dir != "" {
		_ = os.RemoveAll(built.dir)
	}
	os.Exit(code)
}

// buildEdge builds hangar-edge into a directory that outlives the test that
// first asks for it, removed when the package's tests end.
func buildEdge(t *testing.T) string {
	t.Helper()
	built.once.Do(func() {
		built.dir, built.err = os.MkdirTemp("", "hangar-edge-deploy-test")
		if built.err != nil {
			return
		}
		cmd := exec.Command("go", "build", "-o", filepath.Join(built.dir, "hangar-edge"), ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		built.out, built.err = cmd.CombinedOutput()
	})
	if built.err != nil {
		t.Fatalf("go build: %v\n%s", built.err, built.out)
	}
	return filepath.Join(built.dir, "hangar-edge")
}

// releaseOf writes a release directory holding bin as hangar-edge, mode
// 0644 as hangar-fetch-release leaves it, with its SHA256SUMS line.
func releaseOf(t *testing.T, bin []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hangar-edge"), bin, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bin)
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(hex.EncodeToString(sum[:])+"  hangar-edge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// edgeStep runs script in bash with deploy/host's lib.sh and edge step
// sourced, as bootstrap.sh does, its local env file holding localEnv and
// the release in dir, and returns its stdout and stderr.
func edgeStep(t *testing.T, dir, localEnv, script string) (stdout, stderr string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	host, err := filepath.Abs("../../deploy/host")
	if err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(t.TempDir(), "local.env")
	if err := os.WriteFile(env, []byte(localEnv), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	cmd := exec.Command("bash", "-c", `set -euo pipefail
. "$1/lib.sh"
. "$1/steps/85-edge.sh"
LOCAL_ENV=$2 SERVICES_CHECK_PARENT=$3
`+script, "bash", host, env, parent)
	cmd.Env = append(os.Environ(), "HANGAR_RELEASE_DIR="+dir, "HANGAR_IPV6_ADDR=2001:db8:0:1::1/64")
	var o, e strings.Builder
	cmd.Stdout, cmd.Stderr = &o, &e
	if err := cmd.Run(); err != nil {
		t.Fatalf("the edge step's script failed: %v\n%s", err, e.String())
	}
	if left, _ := os.ReadDir(parent); len(left) > 0 {
		t.Errorf("the check left %v in %s", left, parent)
	}
	return o.String(), e.String()
}

// frontDoorEnv is a local env file with the front door on, whole but for
// the two domains.
func frontDoorEnv(cp, user string) string {
	return "HANGAR_AUTH_MODE=oidc\nHANGAR_PUBLIC_URL=https://console." + cp + "\n" +
		"HANGAR_CP_DOMAIN=" + cp + "\nHANGAR_EDGE_DOMAIN=" + user + "\n" +
		"HANGAR_ACME_EMAIL=ops@example.com\nHANGAR_ACME_DNS=testbed:127.0.0.1:8053\n"
}

// TestTheEdgeStepHasTheEdgeCheckItsFlags: edge_problem refuses what the
// release's hangar-edge refuses to start with, by check-flags on the very
// words the unit's ExecStart gets, siblings under one registrable domain
// included; it lets through what that hangar-edge would start with.
func TestTheEdgeStepHasTheEdgeCheckItsFlags(t *testing.T) {
	b, err := os.ReadFile(buildEdge(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := releaseOf(t, b)
	for _, tt := range []struct {
		cp, user, problem string
	}{
		{"hangarlab.cloud", "hangarlab.dev", ""},
		{"cp.test", "user.test", ""},
		{"cp.example.com", "user.example.com", "the edge unit's command line: --cp-domain cp.example.com and --domain user.example.com must not share a registrable domain"},
		{"example.com", "user.example.com", "the edge unit's command line: --cp-domain example.com and --domain user.example.com must not share a registrable domain"},
	} {
		out, errOut := edgeStep(t, dir, frontDoorEnv(tt.cp, tt.user), "edge_problem")
		if tt.problem == "" && out != "" || !strings.Contains(out, tt.problem) {
			t.Errorf("%s and %s: problem %q, want %q", tt.cp, tt.user, out, tt.problem)
		}
		if strings.Contains(errOut, "check-flags") {
			t.Errorf("%s and %s: warned %q", tt.cp, tt.user, errOut)
		}
	}

	// What is checked is what the unit runs.
	out, _ := edgeStep(t, dir, frontDoorEnv("hangarlab.cloud", "hangarlab.dev"),
		`[ "$(edge_unit | sed -n 's|^ExecStart=/usr/local/bin/hangar-edge ||p')" = "$(edge_flags)" ] && echo same; edge_flags`)
	if !strings.HasPrefix(out, "same\n") || !strings.Contains(out, " --cp-domain hangarlab.cloud ") || !strings.Contains(out, " --acme-email ops@example.com ") {
		t.Errorf("the checked flags are not the unit's: %q", out)
	}
}

// TestAnEdgeFromBeforeCheckFlagsIsWarnedOf: a release whose hangar-edge
// answers -h with a usage that does not name check-flags is not given the
// word, which it would take for its own arguments and start an edge; the
// step warns that the flags are not checked. One that cannot run is a
// problem, not a skipped check.
func TestAnEdgeFromBeforeCheckFlagsIsWarnedOf(t *testing.T) {
	old := releaseOf(t, []byte("#!/bin/sh\necho \"Usage of $0:\" >&2\nexit 2\n"))
	out, errOut := edgeStep(t, old, frontDoorEnv("cp.example.com", "user.example.com"), "edge_problem")
	if out != "" || !strings.Contains(errOut, "has no check-flags; the edge unit's command line is not checked this run") {
		t.Errorf("an edge from before check-flags: problem %q, stderr %q", out, errOut)
	}
	broken := releaseOf(t, []byte("\x7fELF not a program"))
	out, _ = edgeStep(t, broken, frontDoorEnv("hangarlab.cloud", "hangarlab.dev"), "edge_problem")
	if !strings.Contains(out, "the edge unit's command line cannot be checked with "+broken+"/hangar-edge") {
		t.Errorf("an edge that cannot run: problem %q", out)
	}
}
