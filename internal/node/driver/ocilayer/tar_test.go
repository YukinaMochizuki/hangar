package ocilayer_test

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver/ocilayer"
)

// An entry is one tar header with its body, for building layers.
type entry struct {
	hdr  tar.Header
	body []byte
}

var epoch = time.Unix(1700000000, 0)

func file(name, body string, mode int64) entry {
	return entry{tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(body)), ModTime: epoch}, []byte(body)}
}

func dir(name string, mode int64) entry {
	return entry{hdr: tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: mode, ModTime: epoch}}
}

func symlink(name, target string) entry {
	return entry{hdr: tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target, Mode: 0o777, ModTime: epoch}}
}

func hardlink(name, target string) entry {
	return entry{hdr: tar.Header{Typeflag: tar.TypeLink, Name: name, Linkname: target, ModTime: epoch}}
}

func device(name string, kind byte) entry {
	return entry{hdr: tar.Header{Typeflag: kind, Name: name, Mode: 0o666, Devmajor: 8, Devminor: 0, ModTime: epoch}}
}

// layer encodes entries as a tar stream.
func layer(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, e := range entries {
		h := e.hdr
		h.Format = tar.FormatPAX
		if err := w.WriteHeader(&h); err != nil {
			t.Fatalf("writing %s: %v", h.Name, err)
		}
		if _, err := w.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// unlimited is far more than any test layer holds.
var unlimited = ocilayer.Limits{MaxBytes: 1 << 30, MaxEntries: 1 << 20}

// apply applies the layers to dir in order and returns the first error.
func apply(t *testing.T, dir string, lim ocilayer.Limits, layers ...[]byte) error {
	t.Helper()
	a, err := ocilayer.Open(dir, lim)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	for _, l := range layers {
		if err := a.Apply(t.Context(), bytes.NewReader(l)); err != nil {
			return err
		}
	}
	return nil
}

// sandbox is a directory holding the root to unpack into and a secret file
// beside it that nothing may reach, whose path is returned too.
type sandbox struct {
	base, root, secret string
}

func newSandbox(t *testing.T) sandbox {
	t.Helper()
	base := t.TempDir()
	s := sandbox{base: base, root: filepath.Join(base, "root"), secret: filepath.Join(base, "secret")}
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

// untouched fails the test unless the sandbox outside the root holds only
// what it was made with, the secret unchanged and unlinked elsewhere.
func (s sandbox) untouched(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(s.base)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 || names[0] != "root" || names[1] != "secret" {
		t.Errorf("beside the root: %v, want only root and secret", names)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(s.secret, &st); err != nil {
		t.Fatalf("the secret is gone: %v", err)
	}
	if st.Nlink != 1 || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Mode&0o7777 != 0o600 {
		t.Errorf("the secret was linked or changed: nlink %d mode %o", st.Nlink, st.Mode)
	}
	if b, err := os.ReadFile(s.secret); err != nil || string(b) != "secret" {
		t.Errorf("the secret reads %q %v", b, err)
	}
	st = syscall.Stat_t{}
	if err := syscall.Lstat(s.base, &st); err != nil || st.Mode&0o7777 != 0o700 {
		t.Errorf("the sandbox's mode changed: %o %v", st.Mode, err)
	}
}

// read returns the contents of a file under the root, or fails the test.
func (s sandbox) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// countingReader counts what was read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
