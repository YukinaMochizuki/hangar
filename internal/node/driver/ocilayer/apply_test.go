package ocilayer_test

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// tree lists what is under root, as relative paths, directories with a
// trailing slash.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			rel += "/"
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func wantTree(t *testing.T, root string, want ...string) {
	t.Helper()
	if got := tree(t, root); !slices.Equal(got, want) {
		t.Errorf("tree\n got %q\nwant %q", got, want)
	}
}

// TestWhiteouts: a whiteout removes what the layers below made at its name,
// a whole tree for a dir, and is never made itself.
func TestWhiteouts(t *testing.T) {
	s := newSandbox(t)
	err := apply(t, s.root, unlimited,
		layer(t, dir("a", 0o755), file("a/x", "x", 0o644), file("a/y", "y", 0o644),
			dir("d", 0o755), dir("d/e", 0o755), file("d/e/f", "f", 0o644), symlink("l", "a/x"), file("keep", "k", 0o644)),
		layer(t, file("a/.wh.x", "", 0o644), file(".wh.d", "", 0o644), file(".wh.l", "", 0o644), file(".wh.absent", "", 0o644),
			file("missing/.wh.x", "", 0o644)),
	)
	if err != nil {
		t.Fatal(err)
	}
	wantTree(t, s.root, "a/", "a/y", "keep")
}

// TestWhiteoutsHideOnlyLowerLayers: a whiteout leaves what its own layer
// made at its name.
func TestWhiteoutsHideOnlyLowerLayers(t *testing.T) {
	s := newSandbox(t)
	err := apply(t, s.root, unlimited,
		layer(t, file("f", "lower", 0o644)),
		layer(t, file("f", "upper", 0o644), file(".wh.f", "", 0o644)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.read(t, "f"); got != "upper" {
		t.Errorf("f: %q", got)
	}
}

// TestOpaqueDirs: an opaque marker empties its dir of what the layers below
// made, keeping what its own layer puts there, before or after it.
func TestOpaqueDirs(t *testing.T) {
	s := newSandbox(t)
	err := apply(t, s.root, unlimited,
		layer(t, dir("a", 0o755), file("a/x", "x", 0o644), dir("a/sub", 0o755), file("a/sub/y", "y", 0o644),
			dir("b", 0o755), file("b/z", "z", 0o644)),
		layer(t, file("a/before", "1", 0o644), dir("a/sub", 0o700), file("a/.wh..wh..opq", "", 0o644), file("a/after", "2", 0o644),
			file("new/.wh..wh..opq", "", 0o644), file("new/n", "n", 0o644)),
	)
	if err != nil {
		t.Fatal(err)
	}
	// a/sub was named in the upper layer, so it stays, but as made there:
	// its contents from below go with the rest.
	wantTree(t, s.root, "a/", "a/after", "a/before", "a/sub/", "b/", "b/z", "new/", "new/n")
	if fi, err := os.Stat(filepath.Join(s.root, "a/sub")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("a/sub: %v %v", fi, err)
	}
}

// TestOpaqueDirsKeepImplicitParents: dirs a layer made only to hold its
// entries are that layer's, which its opaque marker keeps.
func TestOpaqueDirsKeepImplicitParents(t *testing.T) {
	s := newSandbox(t)
	err := apply(t, s.root, unlimited,
		layer(t, file("b/old", "old", 0o644)),
		layer(t, file("b/c/f", "f", 0o644), file("b/.wh..wh..opq", "", 0o644)),
	)
	if err != nil {
		t.Fatal(err)
	}
	wantTree(t, s.root, "b/", "b/c/", "b/c/f")
}

// TestWhiteoutsUnderAFile: a whiteout under what is not a dir has nothing
// below to hide.
func TestWhiteoutsUnderAFile(t *testing.T) {
	s := newSandbox(t)
	err := apply(t, s.root, unlimited,
		layer(t, file("f", "f", 0o644)),
		layer(t, file("f/.wh.x", "", 0o644), file("f/.wh..wh..opq", "", 0o644)),
	)
	if err != nil {
		t.Fatal(err)
	}
	wantTree(t, s.root, "f")
}

// TestOpaqueDirsAtTheRoot: an opaque marker at the root empties the root.
func TestOpaqueDirsAtTheRoot(t *testing.T) {
	s := newSandbox(t)
	err := apply(t, s.root, unlimited,
		layer(t, file("x", "x", 0o644), dir("d", 0o755)),
		layer(t, file(".wh..wh..opq", "", 0o644), file("y", "y", 0o644)),
	)
	if err != nil {
		t.Fatal(err)
	}
	wantTree(t, s.root, "y")
}

// TestReplacing: an entry replaces what is at its name, but a dir over a dir
// keeps what the dir holds.
func TestReplacing(t *testing.T) {
	s := newSandbox(t)
	err := apply(t, s.root, unlimited,
		layer(t, dir("dir-to-file", 0o755), file("dir-to-file/x", "x", 0o644),
			file("file-to-dir", "x", 0o644), dir("merged", 0o755), file("merged/x", "x", 0o644),
			file("file", "old", 0o644)),
		layer(t, file("dir-to-file", "now a file", 0o644), dir("file-to-dir", 0o755),
			dir("merged", 0o711), file("merged/y", "y", 0o644), file("file", "new", 0o600)),
	)
	if err != nil {
		t.Fatal(err)
	}
	wantTree(t, s.root, "dir-to-file", "file", "file-to-dir/", "merged/", "merged/x", "merged/y")
	if got := s.read(t, "file"); got != "new" {
		t.Errorf("file: %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(s.root, "merged")); fi.Mode().Perm() != 0o711 {
		t.Errorf("merged: %v", fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Join(s.root, "file")); fi.Mode().Perm() != 0o600 {
		t.Errorf("file: %v", fi.Mode())
	}
}

// TestMetadata: modes with their special bits, owners, times, symlink
// targets, hardlinks and the xattrs an image may carry are kept.
func TestMetadata(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("owners can be given only as root")
	}
	s := newSandbox(t)
	suid := file("bin/su", "elf", 0o4755)
	suid.hdr.Uid, suid.hdr.Gid = 0, 0
	owned := file("home/u/f", "u", 0o640)
	owned.hdr.Uid, owned.hdr.Gid = 1000, 1001
	sticky := dir("tmp", 0o1777)
	sgid := dir("shared", 0o2775)
	sgid.hdr.Gid = 50
	ping := file("bin/ping", "elf", 0o755)
	// cap_net_raw+ep, as setcap writes it.
	ping.hdr.PAXRecords = map[string]string{
		"SCHILY.xattr.security.capability": "\x01\x00\x00\x02\x00\x20\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00",
		"SCHILY.xattr.user.note":           "kept",
		"SCHILY.xattr.trusted.overlay.x":   "dropped",
	}
	link := symlink("bin/link", "su")
	link.hdr.Uid, link.hdr.Gid = 7, 8
	err := apply(t, s.root, unlimited, layer(t,
		dir("./", 0o750), dir("bin", 0o755), suid, owned, sticky, sgid, ping, link, hardlink("bin/su2", "bin/su"),
	))
	if err != nil {
		t.Fatal(err)
	}
	stat := func(name string) syscall.Stat_t {
		var st syscall.Stat_t
		if err := syscall.Lstat(filepath.Join(s.root, name), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	for name, want := range map[string]struct {
		mode     uint32
		uid, gid uint32
	}{
		".":        {syscall.S_IFDIR | 0o750, 0, 0},
		"bin/su":   {syscall.S_IFREG | 0o4755, 0, 0},
		"home/u/f": {syscall.S_IFREG | 0o640, 1000, 1001},
		"home":     {syscall.S_IFDIR | 0o755, 0, 0}, // made for what it holds
		"tmp":      {syscall.S_IFDIR | 0o1777, 0, 0},
		"shared":   {syscall.S_IFDIR | 0o2775, 0, 50},
		"bin/link": {syscall.S_IFLNK | 0o777, 7, 8},
	} {
		st := stat(name)
		if st.Mode != want.mode || st.Uid != want.uid || st.Gid != want.gid {
			t.Errorf("%s: mode %o uid %d gid %d, want %o %d %d", name, st.Mode, st.Uid, st.Gid, want.mode, want.uid, want.gid)
		}
	}
	if st := stat("bin/su"); st.Mtim.Sec != epoch.Unix() {
		t.Errorf("bin/su's mtime %d", st.Mtim.Sec)
	}
	if st := stat("bin"); st.Mtim.Sec != epoch.Unix() {
		t.Errorf("bin's mtime %d, after what was made in it", st.Mtim.Sec)
	}
	if su, su2 := stat("bin/su"), stat("bin/su2"); su.Ino != su2.Ino || su.Nlink != 2 {
		t.Errorf("bin/su2 is not a hardlink of bin/su")
	}
	if l, err := os.Readlink(filepath.Join(s.root, "bin/link")); err != nil || l != "su" {
		t.Errorf("bin/link -> %q %v", l, err)
	}
	pingPath := filepath.Join(s.root, "bin/ping")
	buf := make([]byte, 64)
	if n, err := unix.Lgetxattr(pingPath, "security.capability", buf); err != nil || n != 20 {
		t.Errorf("security.capability: %d %v", n, err)
	}
	if n, err := unix.Lgetxattr(pingPath, "user.note", buf); err != nil && !errors.Is(err, unix.ENOTSUP) || err == nil && string(buf[:n]) != "kept" {
		t.Errorf("user.note: %q %v", buf[:max(n, 0)], err)
	}
	if _, err := unix.Lgetxattr(pingPath, "trusted.overlay.x", buf); err == nil {
		t.Error("trusted.overlay.x was set")
	}
}

// TestRootDefaults: a root no entry describes is left as a root dir is,
// 0755, whatever it was made with.
func TestRootDefaults(t *testing.T) {
	s := newSandbox(t)
	if err := os.Chmod(s.root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := apply(t, s.root, unlimited, layer(t, file("f", "x", 0o644))); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(s.root); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("root: %v %v", fi, err)
	}
}

// TestFifo: a fifo is made with its mode, and never opened.
func TestFifo(t *testing.T) {
	s := newSandbox(t)
	if err := apply(t, s.root, unlimited, layer(t, entry{hdr: tar.Header{Typeflag: tar.TypeFifo, Name: "p", Mode: 0o600}})); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(s.root, "p")); err != nil || fi.Mode() != os.ModeNamedPipe|0o600 {
		t.Errorf("p: %v %v", fi, err)
	}
}
