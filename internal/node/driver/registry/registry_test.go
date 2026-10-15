package registry_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/klauspost/compress/zstd"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/drivertest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/ocilayer"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry/registrytest"
)

var anon = driver.Credentials(driver.Anonymous)

// tarOf is an uncompressed layer of files, name to contents.
func tarOf(t *testing.T, files ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for i := 0; i < len(files); i += 2 {
		if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: files[i], Mode: 0o644, Size: int64(len(files[i+1]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(files[i+1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zstded(t *testing.T, b []byte) []byte {
	t.Helper()
	w, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	return w.EncodeAll(b, nil)
}

// image is an image of the given layers, already compressed as their media
// types say.
func image(t *testing.T, arch string, cmd []string, layers ...v1.Layer) v1.Image {
	t.Helper()
	img, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{
		OS: "linux", Architecture: arch, Config: v1.Config{Cmd: cmd, Env: []string{"A=1"}, WorkingDir: "/w", User: "1:2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if img, err = mutate.AppendLayers(img, layers...); err != nil {
		t.Fatal(err)
	}
	return img
}

func gzLayer(t *testing.T, files ...string) v1.Layer {
	return static.NewLayer(gzipped(t, tarOf(t, files...)), types.DockerLayer)
}

func newImages(t *testing.T, cache string, lim ocilayer.Limits) *registry.Images {
	t.Helper()
	d, err := registry.New(registry.Config{Cache: cache, Limits: lim})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestConformance holds the driver to the images part of the drivers'
// contract.
func TestConformance(t *testing.T) {
	drivertest.Run(t, func(t *testing.T) drivertest.Host {
		r := registrytest.New(t)
		cache := t.TempDir()
		n := 0
		return drivertest.Host{
			Images: newImages(t, cache, ocilayer.Limits{}),
			Publish: func(t *testing.T, ref string) (string, string) {
				n++
				return r.Push(t, ref, image(t, "amd64", []string{"true"}, gzLayer(t, "f", fmt.Sprint(n))))
			},
			PublishPrivate: func(t *testing.T, ref string) (string, string, driver.Login) {
				n++
				repo := fmt.Sprintf("private%d", n)
				full, digest := r.Push(t, strings.Replace(ref, "private", repo, 1), image(t, "amd64", []string{"true"}, gzLayer(t, "f", fmt.Sprint(n))))
				r.Private(repo)
				return full, digest, driver.Login{Username: "user", Password: "pass"}
			},
			Claim: func(t *testing.T, name, digest string) string {
				n++
				return r.Claim(t, fmt.Sprintf("%s%d", name, n), digest)
			},
			Mirror: func(t *testing.T, name, digest string) string {
				n++
				return r.Mirror(t, fmt.Sprintf("%d%s", n, name), digest)
			},
			Withdraw: func(_ *testing.T, ref string) {
				repo := strings.SplitN(strings.TrimPrefix(ref, r.Host+"/"), ":", 2)[0]
				r.Withdraw(repo)
			},
		}
	})
}

// TestErrorClasses: a failed pull says why, as the agent tells them apart.
func TestErrorClasses(t *testing.T) {
	r := registrytest.New(t)
	d := newImages(t, t.TempDir(), ocilayer.Limits{})
	ref, digest := r.Push(t, "private:1", image(t, "amd64", nil, gzLayer(t, "f", "x")))
	r.Private("private")
	ctx := t.Context()

	if _, err := d.Resolve(ctx, r.Host+"/absent:1", anon); !errors.Is(err, driver.ErrImageNotFound) {
		t.Errorf("an absent repository: %v", err)
	}
	if _, err := d.Config(ctx, r.Host+"/absent:1", digest, anon); !errors.Is(err, driver.ErrImageNotFound) {
		t.Errorf("an absent digest: %v", err)
	}
	if _, err := d.Resolve(ctx, ref, anon); !errors.Is(err, driver.ErrUnauthorized) {
		t.Errorf("a private repository, anonymously: %v", err)
	}
	wrong := func(context.Context, string) (driver.Login, error) {
		return driver.Login{Username: "user", Password: "no"}, nil
	}
	if _, err := d.Config(ctx, ref, digest, wrong); !errors.Is(err, driver.ErrUnauthorized) {
		t.Errorf("a private repository, with a wrong password: %v", err)
	}
	var asked []string
	right := func(_ context.Context, registry string) (driver.Login, error) {
		asked = append(asked, registry)
		return driver.Login{Username: "user", Password: "pass"}, nil
	}
	if got, err := d.Resolve(ctx, ref, right); err != nil || got != digest {
		t.Errorf("a private repository, logged in: %s %v", got, err)
	}
	if len(asked) == 0 || asked[0] != r.Host {
		t.Errorf("credentials asked for %q, want the registry's host %q", asked, r.Host)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	_ = ln.Close()
	if _, err := d.Resolve(ctx, dead+"/app:1", anon); !errors.Is(err, driver.ErrRegistryUnreachable) {
		t.Errorf("a dead registry: %v", err)
	}
	r.Withdraw("down")
	if _, err := d.Resolve(ctx, r.Host+"/down:1", anon); !errors.Is(err, driver.ErrRegistryUnreachable) {
		t.Errorf("a registry answering 503: %v", err)
	}

	r.Slow(5 * time.Second)
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := d.Resolve(short, r.Host+"/absent:1", anon); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a slow registry: %v", err)
	}
}

// TestThePlatformIsPickedFromAnIndex: of a multi-platform image, the
// host's platform is pulled, and the digest stays the index's.
func TestThePlatformIsPickedFromAnIndex(t *testing.T) {
	r := registrytest.New(t)
	d := newImages(t, t.TempDir(), ocilayer.Limits{})
	arm := image(t, "arm64", []string{"arm"}, gzLayer(t, "arch", "arm64"))
	amd := image(t, "amd64", []string{"amd"}, gzLayer(t, "arch", "amd64"))
	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: arm, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
		mutate.IndexAddendum{Add: amd, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
	)
	n, _ := name.ParseReference(r.Host + "/multi:1")
	if err := remote.WriteIndex(n, idx); err != nil {
		t.Fatal(err)
	}
	want, _ := idx.Digest()
	ctx := t.Context()
	digest, err := d.Resolve(ctx, n.String(), anon)
	if err != nil || digest != want.String() {
		t.Fatalf("resolved %s %v, want the index's %s", digest, err, want)
	}
	cfg, err := d.Config(ctx, n.String(), digest, anon)
	if err != nil || len(cfg.Cmd) != 1 || cfg.Cmd[0] != "amd" || cfg.WorkingDir != "/w" || cfg.User != "1:2" || cfg.Env[0] != "A=1" {
		t.Errorf("config %+v %v", cfg, err)
	}
	amdLayers, _ := amd.Layers()
	size, _ := amdLayers[0].Size()
	if cfg.Size != size {
		t.Errorf("size %d, want the amd64 layer's %d", cfg.Size, size)
	}
	dir := t.TempDir()
	if err := d.Unpack(ctx, n.String(), digest, dir, anon); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "arch")); string(b) != "amd64" {
		t.Errorf("unpacked %q", b)
	}

	// An index without the host's platform has no image for it.
	only := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: arm, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}})
	n2, _ := name.ParseReference(r.Host + "/armonly:1")
	if err := remote.WriteIndex(n2, only); err != nil {
		t.Fatal(err)
	}
	od, _ := only.Digest()
	if _, err := d.Config(ctx, n2.String(), od.String(), anon); !errors.Is(err, driver.ErrImageNotFound) {
		t.Errorf("an index without amd64: %v", err)
	}
	// Nor does an image of another platform.
	ref, armDigest := r.Push(t, "arm:1", arm)
	if _, err := d.Config(ctx, ref, armDigest, anon); err == nil {
		t.Error("pulled an arm64 image")
	}
}

// TestCompressions: layers are unpacked whatever they are compressed with.
func TestCompressions(t *testing.T) {
	r := registrytest.New(t)
	d := newImages(t, t.TempDir(), ocilayer.Limits{})
	ref, digest := r.Push(t, "mixed:1", image(t, "amd64", nil,
		static.NewLayer(gzipped(t, tarOf(t, "gz", "1")), types.OCILayer),
		static.NewLayer(zstded(t, tarOf(t, "zst", "2")), types.OCILayerZStd),
		static.NewLayer(tarOf(t, "raw", "3"), types.OCIUncompressedLayer),
	))
	dir := t.TempDir()
	if err := d.Unpack(t.Context(), ref, digest, dir, anon); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]string{"gz": "1", "zst": "2", "raw": "3"} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err != nil || string(b) != want {
			t.Errorf("%s: %q %v", f, b, err)
		}
	}
}

// TestBombsAreBounded: an image unpacking to more than the bound fails as
// too large, and one declaring more is not even fetched.
func TestBombsAreBounded(t *testing.T) {
	r := registrytest.New(t)
	lim := ocilayer.Limits{MaxBytes: 8 << 20, MaxEntries: 1000}
	d := newImages(t, t.TempDir(), lim)
	// 64 MiB of zeros, a few hundred KiB compressed.
	bomb := gzLayer(t, "zeros", string(make([]byte, 64<<20)))
	ref, digest := r.Push(t, "bomb:1", image(t, "amd64", nil, bomb))
	if err := d.Unpack(t.Context(), ref, digest, t.TempDir(), anon); !errors.Is(err, driver.ErrImageTooLarge) {
		t.Errorf("a decompression bomb: %v", err)
	}

	big := static.NewLayer(make([]byte, 9<<20), types.OCIUncompressedLayer)
	ref, digest = r.Push(t, "big:1", image(t, "amd64", nil, big))
	if _, err := d.Config(t.Context(), ref, digest, anon); !errors.Is(err, driver.ErrImageTooLarge) {
		t.Errorf("a layer larger than the bound: %v", err)
	}
	bd, _ := big.Digest()
	if n := r.BlobGets(bd.String()); n != 0 {
		t.Errorf("the oversized layer was fetched %d times", n)
	}

	// A layer of a few hundred bytes nesting dirs 2000 deep is refused at
	// once, within its deadline.
	deep := strings.Repeat("a/", 2000)
	var files []string
	for i := range 20 {
		files = append(files, fmt.Sprintf("%sf%d", deep, i), "")
	}
	ref, digest = r.Push(t, "deep:1", image(t, "amd64", nil, gzLayer(t, files...)))
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if _, err := d.Config(t.Context(), ref, digest, anon); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := d.Unpack(ctx, ref, digest, t.TempDir(), anon)
	// Bounded to catch a hang, not to time it.
	if err == nil || time.Since(start) > 10*time.Second {
		t.Errorf("a layer 2000 deep: %v after %v", err, time.Since(start))
	}

	// Nor is an image of more layers than any real one has.
	var many []v1.Layer
	for i := range 257 {
		many = append(many, static.NewLayer(tarOf(t, fmt.Sprint(i), "x"), types.OCIUncompressedLayer))
	}
	ref, digest = r.Push(t, "many:1", image(t, "amd64", nil, many...))
	if _, err := d.Config(t.Context(), ref, digest, anon); !errors.Is(err, driver.ErrImageTooLarge) {
		t.Errorf("257 layers: %v", err)
	}
	first, _ := many[0].Digest()
	if n := r.BlobGets(first.String()); n != 0 {
		t.Errorf("a layer of the 257 was fetched %d times", n)
	}

	// Nor a config larger than any real one.
	huge, err := mutate.Config(image(t, "amd64", nil, gzLayer(t, "f", "x")), v1.Config{Env: []string{"X=" + strings.Repeat("x", 5<<20)}})
	if err != nil {
		t.Fatal(err)
	}
	ref, digest = r.Push(t, "hugeconfig:1", huge)
	if _, err := d.Config(t.Context(), ref, digest, anon); !errors.Is(err, driver.ErrImageTooLarge) {
		t.Errorf("a 5 MiB config: %v", err)
	}
}

// TestTamperedBlobsAreRefused: a layer that is not what its digest says
// never reaches the cache.
func TestTamperedBlobsAreRefused(t *testing.T) {
	r := registrytest.New(t)
	cache := t.TempDir()
	d := newImages(t, cache, ocilayer.Limits{})
	l := gzLayer(t, "f", "good")
	ref, digest := r.Push(t, "app:1", image(t, "amd64", nil, l))
	ld, _ := l.Digest()
	size, _ := l.Size()
	for name, body := range map[string][]byte{
		"other bytes": bytes.Repeat([]byte{'x'}, int(size)),
		"longer":      bytes.Repeat([]byte{'x'}, int(size)+10),
		"shorter":     []byte("x"),
	} {
		t.Run(name, func(t *testing.T) {
			r.Tamper(ld.String(), body)
			if err := d.Unpack(t.Context(), ref, digest, t.TempDir(), anon); err == nil {
				t.Error("unpacked a tampered layer")
			}
			if blobs, _ := os.ReadDir(filepath.Join(cache, "blobs", "sha256")); len(blobs) != 0 {
				t.Errorf("cached %v", blobs)
			}
		})
	}
}

// TestForgetKeepsWhatOthersUse: forgetting a digest drops the blobs only it
// uses.
func TestForgetKeepsWhatOthersUse(t *testing.T) {
	r := registrytest.New(t)
	cache := t.TempDir()
	d := newImages(t, cache, ocilayer.Limits{})
	shared := gzLayer(t, "shared", "s")
	refA, a := r.Push(t, "a:1", image(t, "amd64", nil, shared, gzLayer(t, "a", "a")))
	refB, b := r.Push(t, "b:1", image(t, "amd64", nil, shared, gzLayer(t, "b", "b")))
	ctx := t.Context()
	for ref, digest := range map[string]string{refA: a, refB: b} {
		if _, err := d.Config(ctx, ref, digest, anon); err != nil {
			t.Fatal(err)
		}
	}
	blobs := func() int {
		entries, err := os.ReadDir(filepath.Join(cache, "blobs", "sha256"))
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	if n := blobs(); n != 3 {
		t.Errorf("%d blobs cached, want 3", n)
	}
	if err := d.Forget(ctx, a); err != nil {
		t.Fatal(err)
	}
	if n := blobs(); n != 2 {
		t.Errorf("%d blobs after forgetting a, want b's 2", n)
	}
	r.Withdraw("b")
	dir := t.TempDir()
	if err := d.Unpack(ctx, refB, b, dir, anon); err != nil {
		t.Fatalf("b after forgetting a: %v", err)
	}
	if err := d.Forget(ctx, b); err != nil {
		t.Fatal(err)
	}
	if n := blobs(); n != 0 {
		t.Errorf("%d blobs after forgetting both", n)
	}
	if err := d.Forget(ctx, b); err != nil {
		t.Errorf("forgetting again: %v", err)
	}
}

// TestForgetWaitsForNothing: forgetting a digest while another is being
// unpacked does not wait for it, and leaves it its blobs.
func TestForgetWaitsForNothing(t *testing.T) {
	r := registrytest.New(t)
	cache := t.TempDir()
	d := newImages(t, cache, ocilayer.Limits{})
	refA, a := r.Push(t, "a:1", image(t, "amd64", nil, gzLayer(t, "a", "a")))
	refB, b := r.Push(t, "b:1", image(t, "amd64", nil, gzLayer(t, "b", "b")))
	ctx := t.Context()
	if _, err := d.Config(ctx, refA, a, anon); err != nil {
		t.Fatal(err)
	}
	// b's pull is held open until a is forgotten.
	arrived, release := r.Block("b")
	defer release()
	done := make(chan error, 1)
	go func() { done <- d.Unpack(ctx, refB, b, t.TempDir(), anon) }()
	<-arrived
	forgot := make(chan error, 1)
	go func() { forgot <- d.Forget(ctx, a) }()
	select {
	case err := <-forgot:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Forget waits for another digest's pull")
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("b's unpack: %v", err)
	}
}

// TestDigestsAreChecked: a digest that is not one names no file in the
// cache.
func TestDigestsAreChecked(t *testing.T) {
	r := registrytest.New(t)
	cache := t.TempDir()
	d := newImages(t, cache, ocilayer.Limits{})
	ref, _ := r.Push(t, "app:1", image(t, "amd64", nil, gzLayer(t, "f", "x")))
	hex := strings.Repeat("a", 64)
	for _, bad := range []string{
		"", "sha256:../../../etc/passwd", "sha256:" + strings.Repeat("A", 64), "sha512:" + strings.Repeat("a", 128), "sha256:" + hex[:63],
		"sha256:" + hex + "/../../x", "../sha256:" + hex, "sha256:" + hex + "\n",
	} {
		if _, err := d.Config(t.Context(), ref, bad, anon); err == nil {
			t.Errorf("config of %q", bad)
		}
		if err := d.Forget(t.Context(), bad); err == nil {
			t.Errorf("forgetting %q", bad)
		}
	}
	// A reference by digest answered for another digest is refused.
	_, other := r.Push(t, "other:1", image(t, "amd64", nil, gzLayer(t, "f", "y")))
	if _, err := d.Config(t.Context(), ref, other, anon); !errors.Is(err, driver.ErrImageNotFound) {
		t.Errorf("app at other's digest: %v", err)
	}
}

// TestUnpackIsConfined: a hostile layer pulled from a registry writes
// nothing outside the directory.
func TestUnpackIsConfined(t *testing.T) {
	r := registrytest.New(t)
	d := newImages(t, t.TempDir(), ocilayer.Limits{})
	base := t.TempDir()
	dir := filepath.Join(base, "root")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ref, digest := r.Push(t, "evil:1", image(t, "amd64", nil, gzLayer(t, "../escaped", "pwned")))
	if err := d.Unpack(t.Context(), ref, digest, dir, anon); err == nil {
		t.Error("unpacked a layer climbing out")
	}
	if _, err := os.Stat(filepath.Join(base, "escaped")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("escaped: %v", err)
	}
}

// TestADigestIsPulledOnce: callers wanting one digest at once fetch each
// layer once.
func TestADigestIsPulledOnce(t *testing.T) {
	r := registrytest.New(t)
	d := newImages(t, t.TempDir(), ocilayer.Limits{})
	l := gzLayer(t, "f", "x")
	ref, digest := r.Push(t, "app:1", image(t, "amd64", nil, l))
	var wg sync.WaitGroup
	var failed atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.Unpack(t.Context(), ref, digest, t.TempDir(), anon); err != nil {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	ld, _ := l.Digest()
	if n := r.BlobGets(ld.String()); n != 1 || failed.Load() != 0 {
		t.Errorf("the layer was fetched %d times, %d unpacks failed", n, failed.Load())
	}
}

// TestAnInterruptedPullIsCleared: what a pull left half-written when the
// agent stopped is cleared when the driver starts.
func TestAnInterruptedPullIsCleared(t *testing.T) {
	cache := t.TempDir()
	newImages(t, cache, ocilayer.Limits{})
	if err := os.WriteFile(filepath.Join(cache, "tmp", "blob-1"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	newImages(t, cache, ocilayer.Limits{})
	if entries, _ := os.ReadDir(filepath.Join(cache, "tmp")); len(entries) != 0 {
		t.Errorf("left: %v", entries)
	}
}

// TestTheWorkBoundIsSetAlone: a bound on work alone, as the node's
// --max-unpack-work sets it, bounds the work and leaves the other bounds
// their defaults.
func TestTheWorkBoundIsSetAlone(t *testing.T) {
	r := registrytest.New(t)
	var files []string
	for i := range 20 {
		files = append(files, fmt.Sprintf("d/f%d", i), "x")
	}
	ref, digest := r.Push(t, "app:1", image(t, "amd64", []string{"true"}, gzLayer(t, files...)))
	for work, ok := range map[int64]bool{100: false, 1_000_000: true} {
		d := newImages(t, t.TempDir(), ocilayer.Limits{MaxWork: work})
		err := d.Unpack(t.Context(), ref, digest, t.TempDir(), anon)
		if ok != (err == nil) || !ok && !errors.Is(err, driver.ErrImageTooLarge) {
			t.Errorf("20 files with a bound of %d steps: %v", work, err)
		}
	}
}
