package main

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func files(t *testing.T, stamp time.Time, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range map[string]string{"hangar.exe": "MZ binary", "b": "second"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestPackIsReproducibleInTheOrderGiven(t *testing.T) {
	out := t.TempDir()
	a, b := filepath.Join(out, "a.zip"), filepath.Join(out, "b.zip")
	if err := pack(a, files(t, time.Unix(1, 0), 0o600), []string{"hangar.exe", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := pack(b, files(t, time.Unix(2_000_000_000, 0), 0o755), []string{"hangar.exe", "b"}); err != nil {
		t.Fatal(err)
	}
	ab, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ab, bb) {
		t.Fatal("the same files with other times and modes packed other bytes")
	}

	zr, err := zip.NewReader(bytes.NewReader(ab), int64(len(ab)))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ name, data string }{{"hangar.exe", "MZ binary"}, {"b", "second"}}
	if len(zr.File) != len(want) {
		t.Fatalf("%d entries, want %d", len(zr.File), len(want))
	}
	for i, f := range zr.File {
		if f.Name != want[i].name {
			t.Errorf("entry %d is %q, want %q", i, f.Name, want[i].name)
		}
		if !f.Modified.Equal(epoch) {
			t.Errorf("%s: time %v, want %v", f.Name, f.Modified, epoch)
		}
		if f.Mode() != 0o755 {
			t.Errorf("%s: mode %v, want 0755", f.Name, f.Mode())
		}
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want[i].data {
			t.Errorf("%s holds %q, want %q", f.Name, data, want[i].data)
		}
	}
}

func TestPackRefusesAnythingButAPlainFile(t *testing.T) {
	dir := files(t, time.Unix(1, 0), 0o644)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("b", filepath.Join(dir, "link")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	for _, name := range []string{"sub", "link", "sub/x", "../b", ""} {
		out := filepath.Join(t.TempDir(), "x.zip")
		if err := pack(out, dir, []string{"b", name}); err == nil {
			t.Errorf("packed %q", name)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Errorf("%q: a failed pack left %s", name, out)
		}
	}
}
