package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// inheritHelperEnv makes the test binary a child that runs inheritSockets on
// the descriptors and variables it was started with, as systemd starts the
// apiserver, and prints what it got.
const inheritHelperEnv = "HANGAR_TEST_INHERIT_SOCKETS"

func TestMain(m *testing.M) {
	if os.Getenv(inheritHelperEnv) == "1" {
		inheritHelper()
		return
	}
	os.Exit(m.Run())
}

func inheritHelper() {
	s, err := inheritSockets()
	for _, v := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		if val, ok := os.LookupEnv(v); ok {
			fmt.Printf("left %s=%s\n", v, val)
		}
	}
	if err != nil {
		fmt.Printf("error %s\n", err)
		os.Exit(0)
	}
	fmt.Printf("activated %t\n", s.activated)
	names := make([]string, 0, len(s.inherited))
	for name := range s.inherited {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Printf("socket %s %s\n", name, s.inherited[name].Addr())
	}
	os.Exit(0)
}

// runInherit starts the helper with one listening socket per path as
// descriptors 3, 4, ..., and LISTEN_PID set to the helper's own pid when
// pidSelf, else to its parent's. env is LISTEN_FDS and LISTEN_FDNAMES.
func runInherit(t *testing.T, paths []string, pidSelf bool, env ...string) []string {
	t.Helper()
	var files []*os.File
	for _, p := range paths {
		ln, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		f, err := ln.(*net.UnixListener).File()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		files = append(files, f)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The shell's $$ is the pid exec keeps, which is how systemd knows
	// the child's pid before it runs.
	pid := "$$"
	if !pidSelf {
		pid = "$PPID"
	}
	cmd := exec.Command("/bin/sh", "-c", `LISTEN_PID=`+pid+` exec "$0"`, exe)
	cmd.Env = append(os.Environ(), inheritHelperEnv+"=1")
	cmd.Env = append(cmd.Env, env...)
	cmd.ExtraFiles = files
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v: %s", err, out)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

func TestInheritSockets(t *testing.T) {
	dir := t.TempDir()
	node, admin := filepath.Join(dir, "node.sock"), filepath.Join(dir, "admin.sock")

	t.Run("by name, variables cleared", func(t *testing.T) {
		got := runInherit(t, []string{node, admin}, true, "LISTEN_FDS=2", "LISTEN_FDNAMES=node:admin")
		want := []string{"activated true", "socket admin " + admin, "socket node " + node}
		if !slices.Equal(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("fewer names than sockets", func(t *testing.T) {
		got := runInherit(t, []string{node + "2", admin + "2"}, true, "LISTEN_FDS=2", "LISTEN_FDNAMES=node")
		if len(got) != 1 || !strings.HasPrefix(got[0], "error systemd passed 2 sockets but named 1") {
			t.Errorf("got %q, want the count refused", got)
		}
	})
	t.Run("more names than sockets", func(t *testing.T) {
		got := runInherit(t, []string{node + "3"}, true, "LISTEN_FDS=1", "LISTEN_FDNAMES=node:admin")
		if len(got) != 1 || !strings.HasPrefix(got[0], "error systemd passed 1 sockets but named 2") {
			t.Errorf("got %q, want the count refused", got)
		}
	})
	t.Run("for another process", func(t *testing.T) {
		got := runInherit(t, []string{node + "4"}, false, "LISTEN_FDS=1", "LISTEN_FDNAMES=node")
		if want := []string{"activated false"}; !slices.Equal(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("not a count", func(t *testing.T) {
		got := runInherit(t, nil, true, "LISTEN_FDS=two", "LISTEN_FDNAMES=")
		if len(got) != 1 || !strings.HasPrefix(got[0], `error LISTEN_FDS="two"`) {
			t.Errorf("got %q, want the count refused", got)
		}
	})
	t.Run("two of one name", func(t *testing.T) {
		got := runInherit(t, []string{node + "5", admin + "5"}, true, "LISTEN_FDS=2", "LISTEN_FDNAMES=admin:admin")
		if len(got) != 1 || !strings.HasPrefix(got[0], `error systemd passed two sockets named "admin"`) {
			t.Errorf("got %q, want the duplicate refused", got)
		}
	})
}
