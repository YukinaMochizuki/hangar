package boot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// inRoot resolves a path as root's processes would, the kernel keeping
// every symlink and ".." inside root, magic links refused: a FileShare is
// written by other machines while this one boots, so what is checked must be
// the very file that is then mounted, held by its fd.
const inRoot = unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS

// openDirIn opens path under rootfd as an O_PATH directory, resolved in it.
func openDirIn(rootfd int, path string) (int, error) {
	return openat2(rootfd, path, &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: inRoot})
}

// openat2 is openat2(2), tried again when it fails with EAGAIN, as it does
// when a rename or mount anywhere races a ".." it resolves in a root.
func openat2(dirfd int, path string, how *unix.OpenHow) (int, error) {
	for range 64 {
		fd, err := unix.Openat2(dirfd, path, how)
		if !errors.Is(err, unix.EAGAIN) {
			return fd, err
		}
	}
	return -1, fmt.Errorf("%s: %w", path, unix.EAGAIN)
}

// makeDirIn opens path under rootfd, root's fd, as openDirIn does, making
// the directories missing on the way, each beside its parent's fd, and
// where a symlink leads that is not there yet, as Docker makes a volume's
// target. A directory refuse names, by where it lies in root, is never
// made.
func makeDirIn(root string, rootfd int, path string, refuse func(where string) bool) (int, error) {
	// Bounds the links followed, and the rounds a client of a share
	// changing it underneath can force.
	for round := 0; round < 64; round++ {
		fd, err := openDirIn(rootfd, path)
		if !errors.Is(err, unix.ENOENT) {
			return fd, err
		}
		if path, err = makeFirstMissing(root, rootfd, path, refuse); err != nil {
			return -1, err
		}
	}
	return -1, fmt.Errorf("%s: %w", path, unix.ELOOP)
}

// makeFirstMissing makes the first directory of path that is missing, and
// returns the path to try next: path itself, or, where the missing one is a
// symlink to nowhere yet, path through what it says.
func makeFirstMissing(root string, rootfd int, path string, refuse func(string) bool) (string, error) {
	var comps []string
	for _, c := range strings.Split(path, "/") {
		if c != "" && c != "." {
			comps = append(comps, c)
		}
	}
	for i := range comps {
		dir, prefix := "/"+strings.Join(comps[:i], "/"), "/"+strings.Join(comps[:i+1], "/")
		fd, err := openDirIn(rootfd, prefix)
		if err == nil {
			_ = unix.Close(fd)
			continue
		}
		if !errors.Is(err, unix.ENOENT) {
			return "", fmt.Errorf("%s: %w", prefix, err)
		}
		parent, err := openDirIn(rootfd, dir)
		if err != nil {
			return "", fmt.Errorf("%s: %w", dir, err)
		}
		defer func() { _ = unix.Close(parent) }()
		if under, err := whereIn(root, parent); err != nil {
			return "", err
		} else if p := filepath.Join(under, comps[i]); refuse(p) {
			return "", fmt.Errorf("%s leads to %s, which is not to be made", path, p)
		}
		err = unix.Mkdirat(parent, comps[i], 0o755)
		if !errors.Is(err, unix.EEXIST) {
			return path, err
		}
		buf := make([]byte, unix.PathMax)
		n, err := unix.Readlinkat(parent, comps[i], buf)
		if err != nil {
			return path, nil // made by another meanwhile
		}
		link, rest := string(buf[:n]), strings.Join(comps[i+1:], "/")
		if !strings.HasPrefix(link, "/") {
			link = dir + "/" + link
		}
		return link + "/" + rest, nil
	}
	return path, nil
}

// whereIn is where fd lies in root, the path its processes will see.
func whereIn(root string, fd int) (string, error) {
	p, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	switch {
	case root == "/":
		return p, nil
	case p == root:
		return "/", nil
	case strings.HasPrefix(p, root+"/"):
		return p[len(root):], nil
	}
	return "", fmt.Errorf("%s is outside %s", p, root)
}

// platformOwned reports whether t, a path in the image, is / or where the
// platform mounts its own filesystems.
func platformOwned(t string) bool {
	return t == "/" || slices.ContainsFunc(platformDirs, func(d string) bool { return t == d || strings.HasPrefix(t, d+"/") })
}
