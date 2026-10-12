package registry_test

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry/registrytest"
)

// stalls is what the tests below allow a registry: a body bringing less
// than 1 KiB in 300 ms, or an answer later than 300 ms, has stalled.
var stalls = registry.Timeouts{ResponseHeader: 300 * time.Millisecond, Window: 300 * time.Millisecond, MinBytes: 1 << 10}

// pullWithin pulls ref's config, and fails unless it ends within 5 s, long
// before the 20 s its context allows.
func pullWithin(t *testing.T, d *registry.Images, ref, digest string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, err := d.Config(ctx, ref, digest, anon)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("ended after %v", took)
	}
	return err
}

// TestAStalledBodyFails: a registry that answers and then trickles its body
// fails the pull at the first window it brings too little in, as a registry
// that cannot be reached.
func TestAStalledBodyFails(t *testing.T) {
	r := registrytest.New(t)
	ref, digest := r.Push(t, "trickle:1", image(t, "amd64", nil, gzLayer(t, "f", "x")))
	r.Trickle("trickle", 0, 50*time.Millisecond)
	d, err := registry.New(registry.Config{Cache: t.TempDir(), Timeouts: stalls})
	if err != nil {
		t.Fatal(err)
	}
	err = pullWithin(t, d, ref, digest)
	var stall *registry.StallError
	if !errors.Is(err, driver.ErrRegistryUnreachable) || !errors.As(err, &stall) {
		t.Errorf("%v, want the registry stalled", err)
	}
}

// TestABodyThatStallsLaterFails: a body that starts well and then stalls
// fails at the first window it brings too little in, however well the
// windows before went.
func TestABodyThatStallsLaterFails(t *testing.T) {
	r := registrytest.New(t)
	big := make([]byte, 256<<10)
	_, _ = rand.Read(big)
	ref, digest := r.Push(t, "later:1", image(t, "amd64", nil, gzLayer(t, "big", string(big))))
	r.Trickle("later", 128<<10, 50*time.Millisecond)
	d, err := registry.New(registry.Config{Cache: t.TempDir(), Timeouts: stalls})
	if err != nil {
		t.Fatal(err)
	}
	err = pullWithin(t, d, ref, digest)
	var stall *registry.StallError
	if !errors.Is(err, driver.ErrRegistryUnreachable) || !errors.As(err, &stall) {
		t.Errorf("%v, want the registry stalled", err)
	}
}

// TestAnUnansweredRequestFails: a registry that takes a request and never
// answers fails it at the header timeout.
func TestAnUnansweredRequestFails(t *testing.T) {
	r := registrytest.New(t)
	ref, digest := r.Push(t, "silent:1", image(t, "amd64", nil, gzLayer(t, "f", "x")))
	_, release := r.Block("silent")
	t.Cleanup(release)
	d, err := registry.New(registry.Config{Cache: t.TempDir(), Timeouts: stalls})
	if err != nil {
		t.Fatal(err)
	}
	err = pullWithin(t, d, ref, digest)
	var stall *registry.StallError
	if !errors.Is(err, driver.ErrRegistryUnreachable) || !errors.As(err, &stall) {
		t.Errorf("%v, want the registry stalled", err)
	}
}

// TestASlowSteadyBodyIsRead: a body slower than the window, but bringing
// enough each window, is read to its end.
func TestASlowSteadyBodyIsRead(t *testing.T) {
	r := registrytest.New(t)
	ref, digest := r.Push(t, "steady:1", image(t, "amd64", []string{"true"}, gzLayer(t, "f", "x")))
	// A config of hundreds of bytes, over several windows, each bringing
	// tens.
	r.Trickle("steady", 0, time.Millisecond)
	d, err := registry.New(registry.Config{Cache: t.TempDir(), Timeouts: registry.Timeouts{Window: 100 * time.Millisecond, MinBytes: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if err := pullWithin(t, d, ref, digest); err != nil {
		t.Error(err)
	}
}

// eio reads a file, then fails with EIO, as a disk with a checksum error
// does.
type eio struct {
	f    *os.File
	left int
}

func (e *eio) Read(p []byte) (int, error) {
	if e.left <= 0 {
		return 0, &os.PathError{Op: "read", Path: e.f.Name(), Err: syscall.EIO}
	}
	n, err := e.f.Read(p[:min(len(p), e.left)])
	e.left -= n
	return n, err
}

func (e *eio) Close() error { return e.f.Close() }

// TestAnUnreadableBlobIsFetchedAgain: a cached blob the host's disk fails to
// read is the cache's fault, not the image's: the unpack fails as one that
// may pass, and the blob is fetched again, and checked, for the next.
func TestAnUnreadableBlobIsFetchedAgain(t *testing.T) {
	// Incompressible, so the blob is larger than what is read of it.
	big := make([]byte, 256<<10)
	_, _ = rand.Read(big)
	for name, mt := range map[string]struct {
		compress func(*testing.T, []byte) []byte
		typ      types.MediaType
	}{
		"gzip": {gzipped, types.DockerLayer},
		"zstd": {zstded, types.OCILayerZStd},
	} {
		t.Run(name, func(t *testing.T) {
			r := registrytest.New(t)
			l := static.NewLayer(mt.compress(t, tarOf(t, "big", string(big))), mt.typ)
			ref, digest := r.Push(t, "app:1", image(t, "amd64", []string{"true"}, l))
			ld, err := l.Digest()
			if err != nil {
				t.Fatal(err)
			}
			var failing atomic.Bool
			d, err := registry.New(registry.WithOpen(registry.Config{Cache: t.TempDir()}, func(name string) (io.ReadCloser, error) {
				f, err := os.Open(name)
				if err != nil || !failing.Load() {
					return f, err
				}
				return &eio{f: f, left: 4 << 10}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if _, err := d.Config(ctx, ref, digest, anon); err != nil {
				t.Fatal(err)
			}
			failing.Store(true)
			err = d.Unpack(ctx, ref, digest, t.TempDir(), anon)
			if err == nil || errors.Is(err, driver.ErrInvalidImage) || !errors.Is(err, syscall.EIO) {
				t.Fatalf("an unreadable blob: %v, want EIO, not an invalid image", err)
			}
			failing.Store(false)
			if err := d.Unpack(ctx, ref, digest, t.TempDir(), anon); err != nil {
				t.Fatalf("after: %v", err)
			}
			if got := r.BlobGets(ld.String()); got != 2 {
				t.Errorf("the layer was fetched %d times, want again after it could not be read", got)
			}
		})
	}
}

// plainCancel is a transport that, as older ones did, says only that a
// request was cancelled, not why.
type plainCancel struct{}

func (plainCancel) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, context.Canceled
		}
		return nil, err
	}
	resp.Body = plainBody{resp.Body, req.Context()}
	return resp, nil
}

type plainBody struct {
	io.ReadCloser
	ctx context.Context
}

func (b plainBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && b.ctx.Err() != nil {
		err = context.Canceled
	}
	return n, err
}

// TestStallsAreToldWhateverTheTransportSays: a stall is told as one, not as
// a cancellation, through a transport that says only that it cancelled.
func TestStallsAreToldWhateverTheTransportSays(t *testing.T) {
	r := registrytest.New(t)
	ref1, digest1 := r.Push(t, "trickle:1", image(t, "amd64", nil, gzLayer(t, "f", "x")))
	r.Trickle("trickle", 0, 50*time.Millisecond)
	ref2, digest2 := r.Push(t, "silent:1", image(t, "amd64", nil, gzLayer(t, "f", "y")))
	_, release := r.Block("silent")
	t.Cleanup(release)
	d, err := registry.New(registry.Config{Cache: t.TempDir(), Transport: plainCancel{}, Timeouts: stalls})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range [][2]string{{ref1, digest1}, {ref2, digest2}} {
		err := pullWithin(t, d, p[0], p[1])
		var stall *registry.StallError
		if !errors.Is(err, driver.ErrRegistryUnreachable) || !errors.As(err, &stall) {
			t.Errorf("%s: %v, want the registry stalled", p[0], err)
		}
	}
}
