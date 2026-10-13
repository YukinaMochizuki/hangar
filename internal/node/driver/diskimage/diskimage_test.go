package diskimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry"
)

// stub is a qemu-img that answers info with the JSON in the file info next
// to it, or fails as the file fail says, and logs its arguments.
const stub = `#!/bin/sh
dir=$(dirname "$0")
echo "$@" >> "$dir/args"
[ -e "$dir/fail" ] && { cat "$dir/fail" >&2; exit 1; }
[ "$1" = info ] && cat "$dir/info"
exit 0
`

type env struct {
	t      *testing.T
	d      *Images
	srv    *httptest.Server
	tools  string
	served map[string][]byte
}

func newEnv(t *testing.T, cfg Config) *env {
	t.Helper()
	e := &env{t: t, tools: t.TempDir(), served: map[string][]byte{}}
	if err := os.WriteFile(filepath.Join(e.tools, "qemu-img"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	e.info(`{"format": "qcow2", "virtual-size": 1073741824}`)
	e.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/stall":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		case "/plain":
			http.Redirect(w, r, "http://example.invalid/x", http.StatusFound)
			return
		}
		b, ok := e.served[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Has("unsized") {
			w.(http.Flusher).Flush() // no Content-Length
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(e.srv.Close)
	cfg.Dir = filepath.Join(t.TempDir(), "images")
	cfg.Transport = e.srv.Client().Transport
	cfg.QemuImg = filepath.Join(e.tools, "qemu-img")
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.d = d
	return e
}

func (e *env) info(json string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.tools, "info"), []byte(json), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// serve serves content at path and returns its URL and digest.
func (e *env) serve(path string, content []byte) (string, string) {
	e.served[path] = content
	sum := sha256.Sum256(content)
	return e.srv.URL + path, "sha256:" + hex.EncodeToString(sum[:])
}

func (e *env) left() []os.DirEntry {
	e.t.Helper()
	entries, err := os.ReadDir(e.d.cfg.Dir)
	if err != nil {
		e.t.Fatal(err)
	}
	return entries
}

func TestFetchChecksAndWrites(t *testing.T) {
	e := newEnv(t, Config{})
	url, digest := e.serve("/cloud.img", []byte("a qcow2, as far as the stub says"))
	img, err := e.d.Fetch(t.Context(), url, digest)
	if err != nil {
		t.Fatal(err)
	}
	if img.Size() != 1<<30 {
		t.Errorf("size %d", img.Size())
	}
	if err := img.WriteTo(t.Context(), "/dev/zd0"); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(e.tools, "args"))
	// The format inspected is the one written from, never probed again,
	// onto a device of zeros that qemu-img neither makes nor writes zeros to.
	if lines := strings.Split(strings.TrimSpace(string(args)), "\n"); len(lines) != 2 ||
		!strings.HasPrefix(lines[0], "info --output=json "+e.d.cfg.Dir+"/") ||
		!strings.HasPrefix(lines[1], "convert -q -n --target-is-zero -f qcow2 -O raw "+e.d.cfg.Dir+"/") || !strings.HasSuffix(lines[1], " /dev/zd0") {
		t.Errorf("qemu-img ran with %q", lines)
	}
	if len(e.left()) != 1 {
		t.Errorf("the fetched file is not kept until closed: %v", e.left())
	}
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	if left := e.left(); len(left) != 0 {
		t.Errorf("left after closing: %v", left)
	}
}

func TestFetchRefuses(t *testing.T) {
	big := []byte(strings.Repeat("x", 2048))
	for _, tt := range []struct {
		name string
		// path served, and with what; info is qemu-img's answer, fail its
		// failure.
		path, info, fail string
		digest           func(string) string
		want             error
		// fault says the image is at fault, for good.
		fault bool
	}{
		{name: "another digest", path: "/a.img", digest: func(string) string { return "sha256:" + strings.Repeat("0", 64) }, want: driver.ErrDigestMismatch, fault: true},
		{name: "too large, said", path: "/big.img", want: driver.ErrImageTooLarge, fault: true},
		{name: "too large, unsaid", path: "/big.img?unsized", want: driver.ErrImageTooLarge, fault: true},
		{name: "another format", path: "/a.img", info: `{"format": "vmdk", "virtual-size": 1024}`, want: driver.ErrInvalidImage, fault: true},
		{name: "a backing file", path: "/a.img", info: `{"format": "qcow2", "virtual-size": 1024, "backing-filename": "/etc/shadow"}`, want: driver.ErrInvalidImage, fault: true},
		{name: "a data file", path: "/a.img", info: `{"format": "qcow2", "virtual-size": 1024, "format-specific": {"data": {"data-file": "/etc/shadow"}}}`, want: driver.ErrInvalidImage, fault: true},
		{name: "a disk too large", path: "/a.img", info: `{"format": "raw", "virtual-size": 1099511627776}`, want: driver.ErrImageTooLarge, fault: true},
		{name: "no disk", path: "/a.img", info: `{"format": "raw", "virtual-size": 0}`, want: driver.ErrInvalidImage, fault: true},
		{name: "qemu-img refusing it", path: "/a.img", fail: "qemu-img: Could not open", want: driver.ErrInvalidImage, fault: true},
		{name: "not there", path: "/missing.img"},
		{name: "a redirect to http", path: "/plain"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, Config{MaxFileSize: 1024, MaxDiskSize: 1 << 30})
			e.served["/a.img"] = []byte("small")
			e.served["/big.img"] = big
			if tt.info != "" {
				e.info(tt.info)
			}
			if tt.fail != "" {
				if err := os.WriteFile(filepath.Join(e.tools, "fail"), []byte(tt.fail), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			path, _, _ := strings.Cut(tt.path, "?")
			sum := sha256.Sum256(e.served[path])
			digest := "sha256:" + hex.EncodeToString(sum[:])
			if tt.digest != nil {
				digest = tt.digest(digest)
			}
			_, err := e.d.Fetch(t.Context(), e.srv.URL+tt.path, digest)
			if err == nil || tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("%v, want %v", err, tt.want)
			}
			if fault := errors.Is(err, driver.ErrDigestMismatch) || errors.Is(err, driver.ErrImageTooLarge) || errors.Is(err, driver.ErrInvalidImage); fault != tt.fault {
				t.Errorf("%v: at fault %t, want %t", err, fault, tt.fault)
			}
			if left := e.left(); len(left) != 0 {
				t.Errorf("a refused fetch left %v", left)
			}
		})
	}
}

func TestFetchNeedsHTTPS(t *testing.T) {
	e := newEnv(t, Config{})
	if _, err := e.d.Fetch(t.Context(), "http://example.com/a.img", "sha256:"+strings.Repeat("0", 64)); !errors.Is(err, driver.ErrInvalidImage) {
		t.Errorf("an http URL: %v", err)
	}
	if _, err := e.d.Fetch(t.Context(), "https://example.com/a.img", "md5:0"); err == nil {
		t.Error("a digest that is not sha256 was taken")
	}
}

// TestAQemuImgThatCannotRun says nothing of the image: the fetch fails as
// one that may pass.
func TestAQemuImgThatCannotRun(t *testing.T) {
	e := newEnv(t, Config{})
	e.d.cfg.QemuImg = filepath.Join(e.tools, "absent")
	url, digest := e.serve("/a.img", []byte("x"))
	_, err := e.d.Fetch(t.Context(), url, digest)
	if err == nil || errors.Is(err, driver.ErrInvalidImage) {
		t.Errorf("%v, want a failure not of the image's", err)
	}
}

func TestAStallingServerIsGivenUp(t *testing.T) {
	e := newEnv(t, Config{Timeouts: registry.Timeouts{Window: 200 * time.Millisecond, MinBytes: 1}})
	start := time.Now()
	_, err := e.d.Fetch(t.Context(), e.srv.URL+"/stall", "sha256:"+strings.Repeat("0", 64))
	var stall *registry.StallError
	if !errors.As(err, &stall) || !strings.HasPrefix(stall.Error(), "the image server ") {
		t.Errorf("%v, want the image server stalled", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("given up after %v", took)
	}
	if errors.Is(err, driver.ErrImageTooLarge) || errors.Is(err, driver.ErrInvalidImage) || errors.Is(err, driver.ErrDigestMismatch) {
		t.Errorf("a stall is not the image's fault: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.d.Fetch(ctx, e.srv.URL+"/stall", "sha256:"+strings.Repeat("0", 64)); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled fetch: %v", err)
	}
}

func TestNewClearsWhatAFetchLeft(t *testing.T) {
	e := newEnv(t, Config{})
	if err := os.WriteFile(filepath.Join(e.d.cfg.Dir, "image-123"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(e.d.cfg); err != nil {
		t.Fatal(err)
	}
	if left := e.left(); len(left) != 0 {
		t.Errorf("left: %v", left)
	}
	if fi, err := os.Stat(e.d.cfg.Dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the directory: %v %v", fi.Mode(), err)
	}
}
