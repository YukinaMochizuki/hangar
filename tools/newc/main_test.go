package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

type entry struct {
	name             string
	mode, uid, gid   uint64
	mtime, size, ino uint64
	data             string
}

// parse reads a newc archive back, failing the test on any framing error.
func parse(t *testing.T, b []byte) []entry {
	t.Helper()
	var out []entry
	off := 0
	align := func() { off = (off + 3) &^ 3 }
	for {
		if len(b)-off < 110 || string(b[off:off+6]) != "070701" {
			t.Fatalf("bad header at offset %d", off)
		}
		field := func(i int) uint64 {
			v, err := strconv.ParseUint(string(b[off+6+8*i:off+14+8*i]), 16, 32)
			if err != nil {
				t.Fatalf("field %d at offset %d: %v", i, off, err)
			}
			return v
		}
		e := entry{ino: field(0), mode: field(1), uid: field(2), gid: field(3), mtime: field(5), size: field(6)}
		namesize := int(field(11))
		off += 110
		e.name = string(b[off : off+namesize-1])
		if b[off+namesize-1] != 0 {
			t.Fatalf("%s: name not NUL-terminated", e.name)
		}
		off += namesize
		align()
		if e.name == "TRAILER!!!" {
			if off != len(b) {
				t.Fatalf("%d bytes after the trailer", len(b)-off)
			}
			return out
		}
		e.data = string(b[off : off+int(e.size)])
		off += int(e.size)
		align()
		out = append(out, e)
	}
}

func tree(t *testing.T, stamp time.Time) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"sys", "dev", "newroot", "proc"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "init"), []byte("#!binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("init", filepath.Join(root, "linuxrc")); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"sys", "dev", "newroot", "proc", "init"} {
		if err := os.Chtimes(filepath.Join(root, n), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestPackIsReproducibleAndSorted(t *testing.T) {
	var a, b bytes.Buffer
	if err := pack(&a, tree(t, time.Unix(1, 0)), 1700000000); err != nil {
		t.Fatal(err)
	}
	if err := pack(&b, tree(t, time.Unix(2_000_000_000, 0)), 1700000000); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("two packs of equal trees differ")
	}

	got := parse(t, a.Bytes())
	want := []struct {
		name string
		mode uint64
		data string
	}{
		{"dev", 0o040755, ""},
		{"init", 0o100755, "#!binary"},
		{"linuxrc", 0o120777, "init"},
		{"newroot", 0o040755, ""},
		{"proc", 0o040755, ""},
		{"sys", 0o040755, ""},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.name != w.name || g.mode != w.mode || g.data != w.data {
			t.Errorf("entry %d = %q mode %o data %q, want %q mode %o data %q", i, g.name, g.mode, g.data, w.name, w.mode, w.data)
		}
		if g.uid != 0 || g.gid != 0 || g.mtime != 1700000000 || g.ino != uint64(i+1) {
			t.Errorf("%s: uid %d gid %d mtime %d ino %d", g.name, g.uid, g.gid, g.mtime, g.ino)
		}
	}
}

func TestPackRefusesSpecialFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("/dev/null", filepath.Join(root, "ok")); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skip("cannot create a fifo here:", err)
	}
	if err := pack(&bytes.Buffer{}, root, 0); err == nil {
		t.Fatal("packed a fifo")
	}
}
