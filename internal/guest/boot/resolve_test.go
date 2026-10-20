package boot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func openRoot(t *testing.T, root string) int {
	t.Helper()
	fd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	mustDo(t, err)
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

func refuseNothing(string) bool { return false }

// TestATargetStaysInTheRoot: a target or subpath is resolved by the kernel
// inside the root, whatever symlinks a share's other clients plant, and the
// directory made and opened is where it says.
func TestATargetStaysInTheRoot(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	mustDo(t, os.MkdirAll(filepath.Join(root, "srv"), 0o755))
	mustDo(t, os.Symlink(outside, filepath.Join(root, "abs")))                        // absolute, out of the root
	mustDo(t, os.Symlink("../../../../../../"+outside, filepath.Join(root, "climb"))) // relative, climbing out
	mustDo(t, os.Symlink("/srv", filepath.Join(root, "data")))
	rootfd := openRoot(t, root)

	for target, want := range map[string]string{
		"/data/x":       "/srv/x",
		"/abs/x":        outside + "/x",
		"/climb/y":      outside + "/y",
		"/new/a/b":      "/new/a/b",
		"/srv/../srv/z": "/srv/z",
	} {
		fd, err := makeDirIn(root, rootfd, target, refuseNothing)
		if err != nil {
			t.Errorf("%s: %v", target, err)
			continue
		}
		got, err := whereIn(root, fd)
		_ = unix.Close(fd)
		if err != nil || got != want {
			t.Errorf("%s leads to %q (%v), want %q in the root", target, got, err, want)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("a directory was made outside the root: %v", entries)
	}
	if fd, err := openDirIn(rootfd, "/missing"); err == nil {
		_ = unix.Close(fd)
		t.Error("a missing subpath was opened")
	}
	for p, owned := range map[string]bool{"/": true, "/proc": true, "/run/x": true, "/data": false, "/procs": false} {
		if platformOwned(p) != owned {
			t.Errorf("platformOwned(%q) = %t", p, !owned)
		}
	}
}

// TestMagicLinksAreRefused: a link of procfs's, such as /proc/self/cwd,
// which a link planted in a share could lead to once /proc is mounted, is
// never followed.
func TestMagicLinksAreRefused(t *testing.T) {
	rootfd := openRoot(t, "/")
	if fd, err := openDirIn(rootfd, "/proc/self/cwd"); !errors.Is(err, unix.ELOOP) {
		if err == nil {
			_ = unix.Close(fd)
		}
		t.Errorf("/proc/self/cwd: %v, want ELOOP", err)
	}
}

// TestNoDirectoryIsMadeForAPlatformTarget: a target leading where the
// platform mounts its own is refused before any directory is made for it.
func TestNoDirectoryIsMadeForAPlatformTarget(t *testing.T) {
	root := t.TempDir()
	mustDo(t, os.Symlink("/run", filepath.Join(root, "torun")))
	rootfd := openRoot(t, root)
	for _, target := range []string{"/torun/x/y", "/proc/x", "/run/a"} {
		if fd, err := makeDirIn(root, rootfd, target, platformOwned); err == nil {
			_ = unix.Close(fd)
			t.Errorf("%s: made", target)
		}
	}
	for _, p := range []string{"run", "proc"} {
		if _, err := os.Lstat(filepath.Join(root, p)); err == nil {
			t.Errorf("/%s was made", p)
		}
	}
}

// TestBindRefusesAPlatformTarget: a target an outer share's symlink leads to
// /, or to an existing directory under /run, is refused at bind, though it
// needs no directory made.
func TestBindRefusesAPlatformTarget(t *testing.T) {
	root, staged := t.TempDir(), t.TempDir()
	outer := filepath.Join(root, "outer") // stands in for a share
	mustDo(t, os.MkdirAll(outer, 0o755))
	mustDo(t, os.MkdirAll(filepath.Join(root, "run/x"), 0o755))
	mustDo(t, os.Symlink("/", filepath.Join(outer, "toroot")))
	mustDo(t, os.Symlink("/run/x", filepath.Join(outer, "torun")))
	rootfd := openRoot(t, root)
	for _, target := range []string{"/outer/toroot", "/outer/torun"} {
		fd, err := bind(root, rootfd, &guestv1.Volume{Name: "v", Target: target}, staged)
		if err == nil {
			_ = unix.Close(fd)
		}
		if err == nil || !strings.Contains(err.Error(), "where the platform mounts its own") {
			t.Errorf("%s: %v, want it refused as the platform's", target, err)
		}
	}
}
