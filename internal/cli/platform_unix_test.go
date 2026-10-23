//go:build !windows

package cli_test

import (
	"os"
	"syscall"
	"testing"
)

// What private means differs by platform (private_unix.go and
// private_windows.go); these helpers let one test check it on either.

// configHome is the variable whose directory holds the CLI's own config
// directory.
const configHome = "XDG_CONFIG_HOME"

// fixHint is in the error that refuses a file others may read.
const fixHint = "chmod 600"

// isPrivate says whether only its owner may at the file or directory at
// path: mode 0600, or 0700 for a directory.
func isPrivate(t *testing.T, path string) bool {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.IsDir() {
		return fi.Mode().Perm() == 0o700
	}
	return fi.Mode().Perm() == 0o600
}

// openUp lets others read the file or directory at path.
func openUp(t *testing.T, path string) {
	t.Helper()
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		mode = 0o755
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// closeUp makes the file at path private again.
func closeUp(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

// accessOf is who may at path, to tell whether it changed.
func accessOf(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().String()
}

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }
