package boot

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestOnlyZerosAreBlank: a volume is formatted only when both its ends are
// zeros; an ext4 superblock is mounted, and anything else, a partition
// table first among them, is neither.
func TestOnlyZerosAreBlank(t *testing.T) {
	const size = 8 << 20
	disk := func(at int64, b []byte) *bytes.Reader {
		d := make([]byte, size)
		copy(d[at:], b)
		return bytes.NewReader(d)
	}
	ext4 := make([]byte, 2)
	binary.LittleEndian.PutUint16(ext4, ext4Magic)
	for name, tt := range map[string]struct {
		r    *bytes.Reader
		size int64
		want content
	}{
		"zeros":                           {disk(0, nil), size, contentBlank},
		"a small disk of zeros":           {bytes.NewReader(make([]byte, 4096)), 4096, contentBlank},
		"an ext4 superblock":              {disk(superblockAt+0x38, ext4), size, contentExt4},
		"a GPT":                           {disk(512, []byte("EFI PART")), size, contentPartitionTable},
		"an MBR":                          {disk(510, []byte{0x55, 0xaa}), size, contentPartitionTable},
		"xfs":                             {disk(0, []byte("XFSB")), size, contentUnknown},
		"btrfs, at 64 KiB":                {disk(64<<10+0x40, []byte("_BHRfS_M")), size, contentUnknown},
		"an md 1.0 superblock at the end": {disk(size-8<<10, []byte{0xfc, 0x4e, 0x2b, 0xa9}), size, contentUnknown},
		"a byte in the last MiB":          {disk(size-1, []byte{1}), size, contentUnknown},
	} {
		got, err := probe(tt.r, tt.size)
		if err != nil || got != tt.want {
			t.Errorf("%s: %q (%v), want %q", name, got, err, tt.want)
		}
	}
}

// TestOnlyAnEmptyLostAndFoundIsNew: a volume is new when it holds nothing,
// or an empty lost+found directory alone, which goes; a file of that name,
// or a lost+found holding what e2fsck found, is the tenant's, and stays.
func TestOnlyAnEmptyLostAndFoundIsNew(t *testing.T) {
	for name, tt := range map[string]struct {
		make func(dir string) error
		new  bool
		kept bool // whether lost+found is there after
	}{
		"nothing":             {func(string) error { return nil }, true, false},
		"an empty lost+found": {func(d string) error { return os.Mkdir(filepath.Join(d, "lost+found"), 0o700) }, true, false},
		"a lost+found holding a file": {func(d string) error {
			if err := os.Mkdir(filepath.Join(d, "lost+found"), 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(d, "lost+found", "#12"), []byte("found"), 0o600)
		}, false, true},
		"a file named lost+found": {func(d string) error {
			return os.WriteFile(filepath.Join(d, "lost+found"), []byte("tenant data"), 0o600)
		}, false, true},
		"a symlink named lost+found": {func(d string) error { return os.Symlink("/etc", filepath.Join(d, "lost+found")) }, false, true},
		"an empty lost+found and a file": {func(d string) error {
			if err := os.Mkdir(filepath.Join(d, "lost+found"), 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(d, ".keep"), nil, 0o600)
		}, false, true},
	} {
		dir := t.TempDir()
		if err := tt.make(dir); err != nil {
			t.Fatal(err)
		}
		got, err := newVolume(dir)
		_, statErr := os.Lstat(filepath.Join(dir, "lost+found"))
		if err != nil || got != tt.new || (statErr == nil) != tt.kept {
			t.Errorf("%s: new %t (%v), lost+found kept %t; want new %t, kept %t", name, got, err, statErr == nil, tt.new, tt.kept)
		}
	}
}

// TestTargetsResolveInTheImage: a target is where the image's symlinks lead
// it, so nesting and the platform's directories are judged there.
func TestTargetsResolveInTheImage(t *testing.T) {
	root := t.TempDir()
	for _, l := range [][2]string{{"/srv/a/b/data", "data"}, {"/run", "var/run"}, {"/", "everything"}} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(l[1])), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(l[0], filepath.Join(root, l[1])); err != nil {
			t.Fatal(err)
		}
	}
	for target, want := range map[string]string{"/data": "/srv/a/b/data", "/srv/a": "/srv/a", "/data/x": "/srv/a/b/data/x"} {
		if got, err := imageTarget(root, target); err != nil || got != want {
			t.Errorf("%s leads to %q (%v), want %q", target, got, err, want)
		}
	}
	// Volumes at /data and /srv/a: /data leads into /srv/a, so it goes on
	// after it, or /srv/a would hide it.
	var in []string
	for _, target := range []string{"/data", "/srv/a"} {
		p, err := imageTarget(root, target)
		if err != nil {
			t.Fatal(err)
		}
		in = append(in, p)
	}
	if got := bindOrder(in); got[0] != 1 || got[1] != 0 {
		t.Errorf("volumes at /data and /srv/a are bound in the order %v; /srv/a must go first", got)
	}
	for _, target := range []string{"/var/run/x", "/everything", "/everything/proc"} {
		if got, err := imageTarget(root, target); err == nil {
			t.Errorf("%s accepted, leading to %s", target, got)
		}
	}
}

// TestCopiedFilesStaySparse: a file with holes, such as lastlog, takes no
// more room in a new volume than in the image, and reads the same.
func TestCopiedFilesStaySparse(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	f, err := os.Create(filepath.Join(src, "lastlog"))
	if err != nil {
		t.Fatal(err)
	}
	const size = 64 << 20
	for _, at := range []int64{0, 32 << 20, size - 5} {
		if _, err := f.WriteAt([]byte("data!"), at); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	var a, b syscall.Stat_t
	if err := syscall.Stat(filepath.Join(src, "lastlog"), &a); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Stat(filepath.Join(dst, "lastlog"), &b); err != nil {
		t.Fatal(err)
	}
	if b.Size != size || b.Blocks > a.Blocks+16 {
		t.Errorf("copy: %d bytes in %d blocks; the image's: %d in %d", b.Size, b.Blocks, a.Size, a.Blocks)
	}
	want, _ := os.ReadFile(filepath.Join(src, "lastlog"))
	if got, err := os.ReadFile(filepath.Join(dst, "lastlog")); err != nil || !bytes.Equal(got, want) {
		t.Errorf("the copy reads otherwise (%v)", err)
	}
}

func TestOuterVolumesAreBoundFirst(t *testing.T) {
	for p, want := range map[string]int{"/data": 1, "/data/b": 2, "/a/b/c": 3, "/data/": 1} {
		if got := depth(p); got != want {
			t.Errorf("depth(%q) = %d, want %d", p, got, want)
		}
	}
}

// TestCopyTreeKeepsWhatTheImageHas: a new volume gets the image's tree as
// it is, its root's mode and times included, with nothing followed out of
// it.
func TestCopyTreeKeepsWhatTheImageHas(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "secret")
	must(os.WriteFile(outside, []byte("not the image's"), 0o600))
	must(os.MkdirAll(filepath.Join(src, "conf.d", "deep"), 0o750))
	must(os.WriteFile(filepath.Join(src, "conf.d", "deep", "a.conf"), []byte("a"), 0o640))
	must(os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\n"), 0o4755))
	// unix.Chmod takes the bits as they are; os.Chmod wants os.Mode* flags.
	must(unix.Chmod(filepath.Join(src, "run.sh"), 0o4755))
	must(os.Link(filepath.Join(src, "run.sh"), filepath.Join(src, "again.sh")))
	must(os.Symlink(outside, filepath.Join(src, "out")))
	must(os.Symlink("conf.d", filepath.Join(src, "conf")))
	must(unix.Mkfifo(filepath.Join(src, "pipe"), 0o600))
	must(unix.Chmod(filepath.Join(src, "pipe"), 0o620))
	must(os.Mkdir(filepath.Join(dst, "lost+found"), 0o700))
	must(unix.Chmod(src, 0o2770))
	// As root, as hangar-init runs, owners are copied too: the volume's
	// root is the image directory's owner's, as postgres's data must be.
	uid, gid, linkUID := os.Getuid(), os.Getgid(), os.Getuid()
	if uid == 0 {
		uid, gid, linkUID = 70, 70, 1000
		must(os.Lchown(src, uid, gid))
		must(os.Lchown(filepath.Join(src, "run.sh"), uid, gid))
		must(os.Lchown(filepath.Join(src, "out"), 1000, 1000))
		must(unix.Chmod(filepath.Join(src, "run.sh"), 0o4755)) // chown cleared it
	}
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	must(os.Chtimes(filepath.Join(src, "conf.d"), old, old))
	must(os.Chtimes(src, old, old))
	_ = unix.Lsetxattr(filepath.Join(src, "run.sh"), "user.hangar", []byte("kept"), 0) // where tmpfs takes user xattrs

	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "conf.d", "deep", "a.conf")); err != nil || string(b) != "a" {
		t.Errorf("a.conf: %q, %v", b, err)
	}
	for path, mode := range map[string]os.FileMode{
		".": os.ModeDir | os.ModeSetgid | 0o770, "conf.d": os.ModeDir | 0o750, "conf.d/deep/a.conf": 0o640,
		"run.sh": os.ModeSetuid | 0o755, "pipe": os.ModeNamedPipe | 0o620,
	} {
		if fi, err := os.Lstat(filepath.Join(dst, path)); err != nil || fi.Mode() != mode {
			t.Errorf("%s: mode %v (%v), want %v", path, fi.Mode(), err, mode)
		}
	}
	for path, want := range map[string]string{"out": outside, "conf": "conf.d"} {
		if got, err := os.Readlink(filepath.Join(dst, path)); err != nil || got != want {
			t.Errorf("symlink %s: %q, %v; want %q", path, got, err, want)
		}
	}
	a, err1 := os.Stat(filepath.Join(dst, "run.sh"))
	b, err2 := os.Stat(filepath.Join(dst, "again.sh"))
	if err1 != nil || err2 != nil || a.Sys().(*syscall.Stat_t).Ino != b.Sys().(*syscall.Stat_t).Ino {
		t.Errorf("hardlinks copied apart: %v %v", err1, err2)
	}
	for _, path := range []string{".", "conf.d"} {
		if fi, err := os.Stat(filepath.Join(dst, path)); err != nil || !fi.ModTime().Equal(old) {
			t.Errorf("%s: modified %v (%v), want %v", path, fi.ModTime(), err, old)
		}
	}
	if fi, err := os.Stat(filepath.Join(dst, "lost+found")); err != nil || !fi.IsDir() {
		t.Errorf("lost+found: %v", err)
	}
	if _, err := unix.Lgetxattr(filepath.Join(src, "run.sh"), "user.hangar", nil); err == nil {
		v := make([]byte, 16)
		if n, err := unix.Lgetxattr(filepath.Join(dst, "run.sh"), "user.hangar", v); err != nil || string(v[:n]) != "kept" {
			t.Errorf("xattr user.hangar: %q, %v", v[:max(n, 0)], err)
		}
	}
	for path, want := range map[string][2]int{".": {uid, gid}, "run.sh": {uid, gid}, "out": {linkUID, -1}} {
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(dst, path), &st); err != nil || int(st.Uid) != want[0] || want[1] >= 0 && int(st.Gid) != want[1] {
			t.Errorf("%s owned by %d:%d (%v), want %v", path, st.Uid, st.Gid, err, want)
		}
	}
}
