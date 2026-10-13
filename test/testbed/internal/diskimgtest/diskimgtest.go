//go:build testbed

// Package diskimgtest serves disk images over HTTPS for the L4 tests of the
// disk image driver (internal/node/driver/diskimage): images qemu-img makes
// in the test, and real cloud images, downloaded once per testbed into a
// cache and checked against their pinned sha256, as is the firmware
// virtual machines boot through.
package diskimgtest

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver/diskimage"
)

// CacheDir keeps real cloud images between runs; deleting it only costs a
// download.
const CacheDir = "/var/cache/hangar/testbed/disk-images"

// Server serves files over HTTPS with a certificate of its own, which only
// the driver Driver makes trusts.
type Server struct {
	srv   *httptest.Server
	files map[string]string // path to file
}

// New starts a server, stopped as the test ends.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{files: map[string]string{}}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := s.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, f)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// Driver is the real disk image driver, downloading into dir, trusting the
// server.
func (s *Server) Driver(t *testing.T, dir string) *diskimage.Images {
	t.Helper()
	d, err := diskimage.New(diskimage.Config{Dir: dir, Transport: s.srv.Client().Transport})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Serve serves the file at path as name, and returns its URL and digest.
func (s *Server) Serve(t *testing.T, name, path string) (url, digest string) {
	t.Helper()
	s.files["/"+name] = path
	return s.srv.URL + "/" + name, "sha256:" + Sum(t, path)
}

// QCOW2 makes and serves, as name, a qcow2 holding a disk of size bytes
// that starts with marker, and returns its URL and digest.
func (s *Server) QCOW2(t *testing.T, name string, size int64, marker string) (url, digest string) {
	t.Helper()
	dir := t.TempDir()
	raw, img := filepath.Join(dir, "raw"), filepath.Join(dir, name)
	if err := os.WriteFile(raw, []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(raw, size); err != nil {
		t.Fatal(err)
	}
	Run(t, "qemu-img", "convert", "-q", "-f", "raw", "-O", "qcow2", raw, img)
	return s.Serve(t, name, img)
}

// Cached returns the file of url in CacheDir, downloading it if it is not
// there, and fails the test unless its sha256 is digest, in hex.
func Cached(t *testing.T, name, url, digest string) string {
	t.Helper()
	path := filepath.Join(CacheDir, name)
	if _, err := os.Stat(path); err != nil {
		if err := os.MkdirAll(CacheDir, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Logf("downloading %s into %s", url, CacheDir)
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %s", url, resp.Status)
		}
		f, err := os.CreateTemp(CacheDir, name+".part-")
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(f, resp.Body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		// Readable by all, as a VM's own user reads firmware.
		if err == nil {
			err = os.Chmod(f.Name(), 0o644)
		}
		if err == nil {
			err = os.Rename(f.Name(), path)
		}
		if err != nil {
			_ = os.Remove(f.Name())
			t.Fatal(err)
		}
	}
	if got := Sum(t, path); got != digest {
		_ = os.Remove(path)
		t.Fatalf("%s has sha256 %s, not %s; removed", path, got, digest)
	}
	return path
}

// Sum is the sha256 of the file at path, in hex.
func Sum(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SumOf is the sha256 of the first n bytes of the device or file at path,
// in hex.
func SumOf(t *testing.T, path string, n int64) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if got, err := io.CopyN(h, f, n); err != nil {
		t.Fatalf("%s: read %d of %d bytes: %v", path, got, n, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Run runs a command, failing the test if it fails, and returns its output.
func Run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
	return string(out)
}
