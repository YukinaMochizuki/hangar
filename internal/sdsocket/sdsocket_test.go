package sdsocket

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// inheritHelperEnv makes the test binary a child that runs Inherit on
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
	s, err := Inherit()
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

func TestSockets(t *testing.T) {
	dir := t.TempDir()
	inherit := func(t *testing.T, name, path string) *Sockets {
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		return &Sockets{inherited: map[string]net.Listener{name: ln}, activated: true}
	}
	t.Run("the one systemd passed", func(t *testing.T) {
		p := filepath.Join(dir, "a.sock")
		s := inherit(t, "admin", p)
		ln, err := s.Unix("admin", p, 0o600)
		if err != nil || ln.Addr().String() != p {
			t.Fatalf("got %v, %v", ln, err)
		}
		if err := s.Unused(); err != nil {
			t.Error(err)
		}
	})
	t.Run("one at another path", func(t *testing.T) {
		s := inherit(t, "admin", filepath.Join(dir, "b.sock"))
		if _, err := s.Unix("admin", filepath.Join(dir, "other.sock"), 0o600); err == nil {
			t.Fatal("took a socket at another path")
		}
	})
	t.Run("none of that name under systemd", func(t *testing.T) {
		s := inherit(t, "node", filepath.Join(dir, "c.sock"))
		p := filepath.Join(dir, "d.sock")
		if _, err := s.Unix("admin", p, 0o600); err == nil {
			t.Fatal("made its own socket under systemd")
		}
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("left a socket at %s: %v", p, err)
		}
		if err := s.Unused(); err == nil || !strings.Contains(err.Error(), "node") {
			t.Errorf("unused: %v", err)
		}
	})
	t.Run("TCP, the one systemd passed", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		s := &Sockets{inherited: map[string]net.Listener{"https": ln}, activated: true}
		got, err := s.TCP(t.Context(), "https", "127.0.0.1:1", nil)
		if err != nil || got != ln {
			t.Fatalf("got %v, %v; want the passed socket, whatever the address", got, err)
		}
		if _, err := s.TCP(t.Context(), "http", "127.0.0.1:0", nil); err == nil {
			t.Error("opened its own TCP socket under systemd")
		}
	})
	t.Run("TCP, a unix socket under its name", func(t *testing.T) {
		s := inherit(t, "https", filepath.Join(dir, "f.sock"))
		if _, err := s.TCP(t.Context(), "https", "", nil); err == nil {
			t.Fatal("took a unix socket for TCP")
		}
	})
	t.Run("TCP, its own without systemd", func(t *testing.T) {
		s := &Sockets{inherited: map[string]net.Listener{}}
		ln, err := s.TCP(t.Context(), "https", "127.0.0.1:0", nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = ln.Close()
		if _, err := s.TCP(t.Context(), "https", "", nil); err == nil {
			t.Error("no address and no socket, and no error")
		}
	})
	t.Run("its own without systemd", func(t *testing.T) {
		s := &Sockets{inherited: map[string]net.Listener{}}
		p := filepath.Join(dir, "e.sock")
		ln, err := s.Unix("admin", p, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("made %v, %v", info.Mode(), err)
		}
	})
}
