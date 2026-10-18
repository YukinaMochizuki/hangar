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

// The services step of deploy/host runs this command's check-placement on
// the host env file's placement; these tests run that shell code, with bash,
// against this very hangar-node, built here, and against stand-ins.

var built struct {
	once sync.Once
	path string
	err  error
	out  []byte
}

func TestMain(m *testing.M) {
	code := m.Run()
	if built.path != "" {
		_ = os.RemoveAll(filepath.Dir(built.path))
	}
	os.Exit(code)
}

// buildNode builds hangar-node into a directory that outlives the test that
// first asks for it, removed when the package's tests end.
func buildNode(t *testing.T) string {
	t.Helper()
	built.once.Do(func() {
		dir, err := os.MkdirTemp("", "hangar-node-deploy-test")
		if err != nil {
			built.err = err
			return
		}
		built.path = filepath.Join(dir, "hangar-node")
		cmd := exec.Command("go", "build", "-o", built.path, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		built.out, built.err = cmd.CombinedOutput()
	})
	if built.err != nil {
		t.Fatalf("go build: %v\n%s", built.err, built.out)
	}
	return built.path
}

// services runs script in bash with deploy/host's lib.sh and services step
// sourced, as bootstrap.sh does, and returns its stdout, its stderr and
// whether it succeeded.
func services(t *testing.T, env []string, script string) (stdout, stderr string, ok bool) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	host, err := filepath.Abs("../../deploy/host")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", `set -euo pipefail
. "$1/lib.sh"
. "$1/steps/80-services.sh"
`+script, "bash", host)
	cmd.Env = append(os.Environ(), env...)
	var o, e strings.Builder
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return o.String(), e.String(), err == nil
}

// stub writes an executable shell script named hangar-node in a directory
// of its own and returns its path.
func stub(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hangar-node")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestOldBinaryIsKnownByItsAnswer: deploy/host takes a hangar-node for one
// from before check-placement only when its answer to -h is such a usage;
// one that cannot run, or answers otherwise, is an error, never a skipped
// check.
func TestOldBinaryIsKnownByItsAnswer(t *testing.T) {
	notExecutable := filepath.Join(t.TempDir(), "hangar-node")
	if err := os.WriteFile(notExecutable, []byte("\x7fELF not run"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		what, bin string
		ok        bool
		says      string
	}{
		{"this hangar-node", buildNode(t), true, "has"},
		// The flag package's default usage, as a hangar-node from before
		// check-placement printed it.
		{"one from before check-placement", stub(t, `echo "Usage of $0:" >&2; echo "  -name string" >&2; exit 0`), true, "lacks"},
		{"one answering -h with exit 2", stub(t, `echo "Usage of $0:" >&2; exit 2`), true, "lacks"},
		{"a file that cannot be executed", notExecutable, false, "cannot run (exit 126)"},
		{"no file at all", filepath.Join(t.TempDir(), "hangar-node"), false, "cannot run (exit 127)"},
		{"one that fails", stub(t, `echo broken >&2; exit 1`), false, "cannot run (exit 1): broken"},
		{"one answering with something else", stub(t, `echo hello; exit 0`), false, "no hangar-node usage: hello"},
	} {
		out, _, ok := services(t, []string{"BIN=" + tt.bin}, `node_check_placement "$BIN"`)
		if ok != tt.ok || !strings.Contains(out, tt.says) {
			t.Errorf("%s: ok %t, said %q; want ok %t, saying %q", tt.what, ok, out, tt.ok, tt.says)
		}
	}
}

// release writes a release directory holding bin as hangar-node, mode
// 0644 as hangar-fetch-release leaves it, with its SHA256SUMS line.
func release(t *testing.T, bin string) string {
	t.Helper()
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hangar-node"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(hex.EncodeToString(sum[:])+"  hangar-node\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestAFetchedReleaseChecksThePlacement: with --release a release whose
// hangar-node is not executable, as a fetched one is, still has the
// placement checked by that hangar-node: a valid one passes with no warning,
// an invalid chassis is a problem, and a hangar-node that does not match
// SHA256SUMS is run not at all.
func TestAFetchedReleaseChecksThePlacement(t *testing.T) {
	dir := release(t, buildNode(t))
	check := `services_placement_problem`
	for _, tt := range []struct {
		what    string
		env     []string
		problem string
	}{
		{"a valid placement", []string{"HANGAR_REGION=region-1", "HANGAR_CHASSIS=generic-1u-2x-u2"}, ""},
		{"an invalid chassis", []string{"HANGAR_CHASSIS=Generic 1U"}, "the host env file's placement: --chassis:"},
	} {
		out, errOut, ok := services(t, append([]string{"HANGAR_RELEASE_DIR=" + dir}, tt.env...), check)
		if !ok {
			t.Fatalf("%s: the check failed: %s", tt.what, errOut)
		}
		if tt.problem == "" && out != "" || !strings.HasPrefix(out, tt.problem) {
			t.Errorf("%s: problem %q, want %q", tt.what, out, tt.problem)
		}
		if strings.Contains(errOut, "check-placement") {
			t.Errorf("%s: warned %q", tt.what, errOut)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "hangar-node")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("the check changed the release's file: %v %v", fi.Mode(), err)
	}

	tampered := release(t, buildNode(t))
	if err := os.WriteFile(filepath.Join(tampered, "hangar-node"), []byte("#!/bin/sh\ntouch \"$0.ran\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, _, ok := services(t, []string{"HANGAR_RELEASE_DIR=" + tampered, "HANGAR_CHASSIS=generic-1u-2x-u2"}, check)
	if !ok || !strings.Contains(out, "does not match") {
		t.Errorf("a hangar-node that does not match SHA256SUMS: ok %t, problem %q", ok, out)
	}
	if _, err := os.Stat(filepath.Join(tampered, "hangar-node.ran")); err == nil {
		t.Error("a hangar-node that does not match SHA256SUMS was run")
	}

	// A release whose hangar-node cannot run at all is a problem, not a
	// skipped check.
	notAProgram := filepath.Join(t.TempDir(), "hangar-node")
	if err := os.WriteFile(notAProgram, []byte("\x7fELF not a program"), 0o644); err != nil {
		t.Fatal(err)
	}
	broken := release(t, notAProgram)
	out, errOut, ok := services(t, []string{"HANGAR_RELEASE_DIR=" + broken, "HANGAR_CHASSIS=generic-1u-2x-u2"}, check)
	if !ok || !strings.Contains(out, "placement cannot be checked with "+broken+"/hangar-node") || strings.Contains(errOut, "has no check-placement") {
		t.Errorf("a hangar-node that cannot run: ok %t, problem %q, stderr %q", ok, out, errOut)
	}
}
