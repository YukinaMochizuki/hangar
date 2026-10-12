package zfsstore

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestNames(t *testing.T) {
	s := &Store{cfg: Config{Dataset: "hangar"}}
	hex := strings.Repeat("ab", 32)
	if got, err := s.baseName("sha256:" + hex); err != nil || got != "hangar/bases/"+hex {
		t.Errorf("base name %q %v", got, err)
	}
	for _, bad := range []string{
		"", hex, "sha256:" + hex[:63], "sha256:" + hex + "0", "sha256:" + strings.ToUpper(hex),
		"sha512:" + hex, "sha256:../" + hex[3:], "sha256:" + hex + "\n", " sha256:" + hex,
	} {
		if got, err := s.baseName(bad); err == nil {
			t.Errorf("base name of %q: %q", bad, got)
		}
	}
	if got, err := s.rootName("0b6c3a1e-2f44-4b8e-9d3a-1f2e3d4c5b6a"); err != nil || got != "hangar/roots/0b6c3a1e-2f44-4b8e-9d3a-1f2e3d4c5b6a" {
		t.Errorf("root name %q %v", got, err)
	}
	for _, good := range []string{"m1", "M", "a_b-c", strings.Repeat("x", 128)} {
		if _, err := s.rootName(good); err != nil {
			t.Errorf("root name of %q: %v", good, err)
		}
	}
	for _, bad := range []string{
		"", "-m", "_m", "a/b", "../x", "a@b", "a b", "a.b", "a:b", "a\n", strings.Repeat("x", 129), "a%b",
	} {
		if got, err := s.rootName(bad); err == nil {
			t.Errorf("root name of %q: %q", bad, got)
		}
	}
	if device("hangar/roots/m1") != "/dev/zvol/hangar/roots/m1" {
		t.Error(device("hangar/roots/m1"))
	}
}

func TestBaseSize(t *testing.T) {
	write := func(t *testing.T, dir, name string, size int) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("empty", func(t *testing.T) {
		got, err := baseSize(t.TempDir())
		if err != nil || got != baseOverhead+align {
			// One dir of one block: 4096 + 819, then rounded up.
			t.Errorf("an empty image: %d %v, want %d", got, err, baseOverhead+align)
		}
	})
	t.Run("data", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "big", 100<<20)
		write(t, dir, "small", 1) // a whole block
		got, err := baseSize(dir)
		if err != nil {
			t.Fatal(err)
		}
		data := int64(100<<20) + 4096 + 4096 // and the root dir
		want := roundUp(data+data/5+baseOverhead, align)
		if got != want {
			t.Errorf("%d, want %d", got, want)
		}
		if got%align != 0 {
			t.Errorf("%d is not aligned", got)
		}
	})
	t.Run("hardlinks count once", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "a", 50<<20)
		one, _ := baseSize(dir)
		for i := range 4 {
			if err := os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "link"+string(rune('0'+i)))); err != nil {
				t.Fatal(err)
			}
		}
		if five, _ := baseSize(dir); five != one {
			t.Errorf("with four hardlinks %d, without %d", five, one)
		}
	})
	t.Run("many small files need inodes", func(t *testing.T) {
		dir := t.TempDir()
		for i := range 8000 {
			write(t, dir, filepath.Join("d", strings.Repeat("f", 1+i%50)+string(rune('a'+i%26))+"-"+strconv.Itoa(i)), 0)
		}
		got, err := baseSize(dir)
		if err != nil {
			t.Fatal(err)
		}
		// 8002 inodes at one per 16 KiB, with a fifth to spare.
		if min := int64(8002) * inodeRatio * 6 / 5; got < min {
			t.Errorf("%d bytes for 8002 inodes, want at least %d", got, min)
		}
	})
	t.Run("long symlinks take a block", func(t *testing.T) {
		dir := t.TempDir()
		short, _ := baseSize(dir)
		for i := range 1000 {
			if err := os.Symlink(strings.Repeat("t", 80), filepath.Join(dir, "l"+strconv.Itoa(i))); err != nil {
				t.Fatal(err)
			}
		}
		long, _ := baseSize(dir)
		if long-short < 1000*4096-align {
			t.Errorf("1000 long symlinks took %d bytes", long-short)
		}
	})
}

// TestCommandsSpeakEnglish: what zfs and mkfs say is read in English,
// whatever the host's locale.
func TestCommandsSpeakEnglish(t *testing.T) {
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LANG", "de_DE.UTF-8")
	out, err := run(t.Context(), "sh", "-c", `echo "$LC_ALL"`)
	if err != nil || strings.TrimSpace(string(out)) != "C" {
		t.Errorf("LC_ALL is %q %v, want C", out, err)
	}
}
