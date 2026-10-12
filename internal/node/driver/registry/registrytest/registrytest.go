// Package registrytest serves an OCI registry in a test, which tests can
// make fail, and checks an unpacked image against go-containerregistry's
// own flattening of it. It is for tests only.
package registrytest

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Registry is a registry served in the test.
type Registry struct {
	// Host is the registry's host:port, for references.
	Host string

	mu        sync.Mutex
	withdrawn map[string]bool
	private   map[string]bool
	slow      time.Duration
	tampered  map[string][]byte
	blobGets  map[string]int
	blocked   map[string]*block
	trickled  map[string]trickle
}

// trickle is how a repository's blobs are sent: so many bytes at once, then
// a byte every so often.
type trickle struct {
	after int
	every time.Duration
}

// block holds a repository's requests until it is released.
type block struct {
	arrived  chan struct{}
	once     sync.Once
	released chan struct{}
}

// New serves a registry until the test ends.
func New(t testing.TB) *Registry {
	t.Helper()
	r := &Registry{withdrawn: map[string]bool{}, private: map[string]bool{}, tampered: map[string][]byte{}, blobGets: map[string]int{}, blocked: map[string]*block{}, trickled: map[string]trickle{}}
	inner := ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		slow := r.slow
		repo := ""
		if parts := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/v2/"), "/", 2); len(parts) == 2 {
			repo = parts[0]
		}
		withdrawn, private := r.withdrawn[repo], r.private[repo]
		var tampered []byte
		if i := strings.Index(req.URL.Path, "/blobs/"); i >= 0 && req.Method == http.MethodGet {
			d := req.URL.Path[i+len("/blobs/"):]
			r.blobGets[d]++
			tampered = r.tampered[d]
		}
		b := r.blocked[repo]
		if tr, ok := r.trickled[repo]; ok && strings.Contains(req.URL.Path, "/blobs/") {
			w = &trickler{w: w, trickle: tr, done: req.Context().Done()}
		}
		r.mu.Unlock()
		if b != nil {
			b.once.Do(func() { close(b.arrived) })
			select {
			case <-b.released:
			case <-req.Context().Done():
				return
			}
		}
		if slow > 0 {
			select {
			case <-time.After(slow):
			case <-req.Context().Done():
				return
			}
		}
		if user, pass, ok := req.BasicAuth(); private && (!ok || user != "user" || pass != "pass") {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, `{"errors":[{"code":"UNAUTHORIZED","message":"log in"}]}`, http.StatusUnauthorized)
			return
		}
		switch {
		case withdrawn:
			http.Error(w, "down", http.StatusServiceUnavailable)
		case tampered != nil:
			_, _ = w.Write(tampered)
		default:
			inner.ServeHTTP(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	r.Host = strings.TrimPrefix(srv.URL, "http://")
	return r
}

// Withdraw makes the repository answer 503, as a registry that is down.
func (r *Registry) Withdraw(repo string) { r.set(func() { r.withdrawn[repo] = true }) }

// Private makes the repository ask for the login user:pass.
func (r *Registry) Private(repo string) { r.set(func() { r.private[repo] = true }) }

// Block holds every request for the repository until release is called;
// arrived is closed when the first comes in.
func (r *Registry) Block(repo string) (arrived <-chan struct{}, release func()) {
	b := &block{arrived: make(chan struct{}), released: make(chan struct{})}
	r.set(func() { r.blocked[repo] = b })
	var once sync.Once
	return b.arrived, func() {
		once.Do(func() {
			r.set(func() { delete(r.blocked, repo) })
			close(b.released)
		})
	}
}

// Trickle sends the bodies of the repository's blobs, after their headers
// and their first bytes, a byte every so often, as a registry stalling
// mid-body does.
func (r *Registry) Trickle(repo string, after int, every time.Duration) {
	r.set(func() { r.trickled[repo] = trickle{after, every} })
}

// trickler writes a byte at a time, after the first.
type trickler struct {
	w http.ResponseWriter
	trickle
	done <-chan struct{}
	sent int
}

func (t *trickler) Header() http.Header { return t.w.Header() }

func (t *trickler) WriteHeader(code int) {
	t.w.WriteHeader(code)
	t.w.(http.Flusher).Flush()
}

func (t *trickler) Write(p []byte) (int, error) {
	if fast := min(len(p), t.after-t.sent); fast > 0 {
		n, err := t.w.Write(p[:fast])
		t.sent += n
		if err != nil {
			return n, err
		}
		t.w.(http.Flusher).Flush()
		m, err := t.Write(p[fast:])
		return n + m, err
	}
	for i := range p {
		if _, err := t.w.Write(p[i : i+1]); err != nil {
			return i, err
		}
		t.w.(http.Flusher).Flush()
		select {
		case <-time.After(t.every):
		case <-t.done:
			return i + 1, errors.New("the client went away")
		}
	}
	return len(p), nil
}

// Slow delays every answer by d.
func (r *Registry) Slow(d time.Duration) { r.set(func() { r.slow = d }) }

// Tamper serves body for the blob of digest.
func (r *Registry) Tamper(digest string, body []byte) { r.set(func() { r.tampered[digest] = body }) }

// BlobGets returns how often the blob of digest was fetched.
func (r *Registry) BlobGets(digest string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.blobGets[digest]
}

func (r *Registry) set(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
}

// Push publishes img as repo:tag and returns its full reference and digest.
func (r *Registry) Push(t testing.TB, ref string, img v1.Image) (string, string) {
	t.Helper()
	full := r.Host + "/" + ref
	n, err := name.ParseReference(full)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(n, img); err != nil {
		t.Fatal(err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return full, d.String()
}

// entry is a file as both sides are compared by.
type entry struct {
	typ      byte
	mode     int64
	uid, gid int
	size     int64
	sum      string
	link     string
}

// MatchesExtract fails the test unless dir holds what mutate.Extract (crane
// export) flattens img to, but its device nodes and the symlinks it drops,
// and returns how many entries it compared. docs/runbook/testbed.md.
func MatchesExtract(t testing.TB, img v1.Image, dir string) int {
	t.Helper()
	rc := mutate.Extract(img)
	defer func() { _ = rc.Close() }()
	want := map[string]entry{}
	tr := tar.NewReader(rc)
	devices := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		p := path.Clean("/" + hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeChar, tar.TypeBlock:
			devices++
			continue
		case tar.TypeLink:
			// Compared as the file it links to, which comes first.
			target, ok := want[path.Clean("/"+hdr.Linkname)]
			if !ok {
				t.Errorf("%s: a hardlink to %s, which the reference lacks", p, hdr.Linkname)
				continue
			}
			want[p] = target
			continue
		}
		e := entry{typ: hdr.Typeflag, mode: hdr.Mode & 0o7777, uid: hdr.Uid, gid: hdr.Gid}
		switch hdr.Typeflag {
		case tar.TypeReg:
			e.typ = tar.TypeReg
			h := sha256.New()
			n, err := io.Copy(h, tr)
			if err != nil {
				t.Fatal(err)
			}
			e.size, e.sum = n, hex.EncodeToString(h.Sum(nil))
		case tar.TypeSymlink:
			e.mode, e.link = 0, hdr.Linkname
		}
		want[p] = e
	}
	if devices > 0 {
		t.Logf("%d device nodes in the image, not made on the host", devices)
	}

	got := map[string]entry{}
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		key := path.Clean("/" + rel)
		st := fi.Sys().(*syscall.Stat_t)
		e := entry{mode: int64(st.Mode & 0o7777), uid: int(st.Uid), gid: int(st.Gid)}
		switch {
		case fi.Mode().IsRegular():
			e.typ = tar.TypeReg
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			h := sha256.New()
			n, err := io.Copy(h, f)
			_ = f.Close()
			if err != nil {
				return err
			}
			e.size, e.sum = n, hex.EncodeToString(h.Sum(nil))
		case fi.IsDir():
			e.typ = tar.TypeDir
		case fi.Mode()&os.ModeSymlink != 0:
			e.typ, e.mode = tar.TypeSymlink, 0
			if e.link, err = os.Readlink(p); err != nil {
				return err
			}
		case fi.Mode()&os.ModeNamedPipe != 0:
			e.typ = tar.TypeFifo
		default:
			t.Errorf("%s: unexpected mode %v", rel, fi.Mode())
		}
		got[key] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, w := range want {
		g, ok := got[p]
		switch {
		case !ok:
			t.Errorf("%s: missing; the reference has %+v", p, w)
		case g != w:
			t.Errorf("%s: %+v, the reference has %+v", p, g, w)
		}
	}
	for p, g := range got {
		if _, ok := want[p]; ok || p == "/" {
			continue
		}
		if g.typ == tar.TypeSymlink && strings.HasPrefix(path.Clean(path.Join(path.Dir(p), g.link)), "/..") {
			continue // Extract drops what climbs out
		}
		if g.typ == tar.TypeDir && wantsUnder(want, p) {
			// A parent Extract leaves implicit, as tar does.
			continue
		}
		t.Errorf("%s: %+v, which the reference lacks", p, g)
	}
	return len(want)
}

// wantsUnder reports whether the reference has anything under dir.
func wantsUnder(want map[string]entry, dir string) bool {
	for p := range want {
		if strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}
