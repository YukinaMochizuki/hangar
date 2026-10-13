// Package registry is the host's Images driver: it pulls OCI images from
// their registries into a cache on the host, keyed by digest, and unpacks
// them from there with package ocilayer. A cached digest never needs the
// registry again until it is forgotten. It knows nothing of where an
// unpacked image goes; see package zfsstore for that.
package registry

import (
	"bufio"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/klauspost/compress/zstd"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/keylock"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/ocilayer"
)

// DefaultLimits bound an image's unpacked size and entries. A layer is
// also bounded by them before it is fetched, compressed.
var DefaultLimits = ocilayer.Limits{MaxBytes: 32 << 30, MaxEntries: 4 << 20}

// Bounds on what a registry sends before it is trusted.
const (
	maxLayers     = 256
	maxConfigSize = 4 << 20
)

// platform is the only one the host runs.
var platform = v1.Platform{OS: "linux", Architecture: "amd64"}

// variants of the platform, from the one taken first.
var variants = []string{"", "v1", "v2", "v3", "v4"}

// Config configures the driver.
type Config struct {
	// Cache is the directory pulled images are kept in, root's alone; the
	// driver owns all of it.
	Cache string
	// Limits bound what an image may unpack to, and the work it takes. Zero
	// fields take DefaultLimits', and ocilayer's defaults after them.
	Limits ocilayer.Limits
	// Transport reaches the registries. Nil means http.DefaultTransport,
	// connecting within Timeouts.
	Transport http.RoundTripper
	// Timeouts bound how long a registry may make no progress. Zero fields
	// take DefaultTimeouts'.
	Timeouts Timeouts
	// open opens a cached blob; nil means os.Open. Tests make it fail.
	open func(name string) (io.ReadCloser, error)
}

// Images is the driver. It is safe for concurrent use.
type Images struct {
	cfg Config

	// mu guards the cache's records and pins, so a blob is never removed
	// while a pull or an unpack uses it, and never waits for one.
	mu sync.Mutex
	// pins counts the pulls and unpacks using each blob.
	pins map[string]int
	// spared says a collection left blobs because they were pinned; the
	// last release collects again.
	spared bool
	// digests holds a lock per digest, so a digest is pulled, or forgotten,
	// once at a time.
	digests keylock.Map
}

// New opens the cache, making it if need be, and clears what an
// interrupted pull left in it.
func New(cfg Config) (*Images, error) {
	cfg.Limits.MaxBytes = cmp.Or(cfg.Limits.MaxBytes, DefaultLimits.MaxBytes)
	cfg.Limits.MaxEntries = cmp.Or(cfg.Limits.MaxEntries, DefaultLimits.MaxEntries)
	// The others ocilayer defaults.
	cfg.Timeouts = cfg.Timeouts.orDefault()
	if cfg.Transport == nil {
		cfg.Transport = defaultTransport(cfg.Timeouts)
	}
	cfg.Transport = progress{cfg.Transport, cfg.Timeouts, ""}
	if cfg.open == nil {
		cfg.open = func(name string) (io.ReadCloser, error) { return os.Open(name) }
	}
	for _, d := range []string{"blobs/sha256", "images"} {
		if err := os.MkdirAll(filepath.Join(cfg.Cache, d), 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.RemoveAll(filepath.Join(cfg.Cache, "tmp")); err != nil {
		return nil, err
	}
	if err := os.Mkdir(filepath.Join(cfg.Cache, "tmp"), 0o700); err != nil {
		return nil, err
	}
	return &Images{cfg: cfg, pins: map[string]int{}}, nil
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// hexOf returns the hex of a sha256 digest, which names files in the cache,
// or fails for anything else.
func hexOf(digest string) (string, error) {
	if !digestRE.MatchString(digest) {
		return "", fmt.Errorf("%q is not a sha256 digest", digest)
	}
	return digest[len("sha256:"):], nil
}

// record is what the cache keeps of a pulled digest: the image of the host's
// platform it names, as the driver uses it.
type record struct {
	// Manifest is the digest of the image's manifest, the digest itself
	// unless it names an index.
	Manifest string             `json:"manifest"`
	Config   driver.ImageConfig `json:"config"`
	Layers   []layer            `json:"layers"`
}

type layer struct {
	Digest    string          `json:"digest"`
	MediaType types.MediaType `json:"mediaType"`
	Size      int64           `json:"size"`
}

func (r *record) blobs() []string {
	var out []string
	for _, l := range r.Layers {
		out = append(out, l.Digest)
	}
	return out
}

func (d *Images) recordPath(hex string) string {
	return filepath.Join(d.cfg.Cache, "images", hex+".json")
}

func (d *Images) blobPath(digest string) string {
	return filepath.Join(d.cfg.Cache, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
}

func (d *Images) options(ctx context.Context, creds driver.Credentials) []remote.Option {
	return []remote.Option{
		remote.WithContext(ctx),
		remote.WithTransport(d.cfg.Transport),
		remote.WithAuthFromKeychain(keychain{ctx, creds}),
		remote.WithUserAgent("hangar-node"),
	}
}

func (d *Images) Resolve(ctx context.Context, ref string, creds driver.Credentials) (string, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("%w: %w", driver.ErrImageNotFound, err)
	}
	desc, err := remote.Head(r, d.options(ctx, creds)...)
	if err != nil {
		var terr *transport.Error
		if errors.As(err, &terr) && terr.StatusCode == http.StatusMethodNotAllowed {
			// Not every registry answers HEAD.
			var got *remote.Descriptor
			if got, err = remote.Get(r, d.options(ctx, creds)...); err == nil {
				desc = &got.Descriptor
			}
		}
	}
	if err != nil {
		return "", classify(ref, err)
	}
	if dr, ok := r.(name.Digest); ok && dr.DigestStr() != desc.Digest.String() {
		return "", fmt.Errorf("%s: the registry answered for %s", ref, desc.Digest)
	}
	return desc.Digest.String(), nil
}

func (d *Images) Config(ctx context.Context, ref, digest string, creds driver.Credentials) (driver.ImageConfig, error) {
	rec, release, err := d.get(ctx, ref, digest, creds)
	if err != nil {
		return driver.ImageConfig{}, err
	}
	release()
	return rec.Config, nil
}

func (d *Images) Unpack(ctx context.Context, ref, digest, dir string, creds driver.Credentials) error {
	rec, release, err := d.get(ctx, ref, digest, creds)
	if err != nil {
		return err
	}
	defer release()
	a, err := ocilayer.Open(dir, d.cfg.Limits)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()
	for i, l := range rec.Layers {
		if err := d.applyLayer(ctx, a, l); err != nil {
			return fmt.Errorf("%s@%s, layer %d (%s): %w", ref, digest, i, l.Digest, err)
		}
	}
	return nil
}

func (d *Images) applyLayer(ctx context.Context, a *ocilayer.Applier, l layer) error {
	f, err := d.cfg.open(d.blobPath(l.Digest))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	blob := &blobReader{r: f}
	err = func() error {
		r, err := decompress(ctxReader{ctx, blob})
		if err != nil {
			return err
		}
		defer func() { _ = r.Close() }()
		if err := a.Apply(ctx, r); err != nil {
			return err
		}
		return ctx.Err()
	}()
	if blob.err != nil {
		// The cache is at fault, not the image, whatever the layer's
		// reader made of it: the blob is dropped, to be fetched again and
		// checked against its digest.
		d.dropBlob(l.Digest)
		return fmt.Errorf("reading the cached blob, dropped from the cache: %w", blob.err)
	}
	return err
}

// blobReader remembers the error reading a cached blob failed with.
type blobReader struct {
	r   io.Reader
	err error
}

func (b *blobReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		b.err = err
	}
	return n, err
}

// dropBlob removes a blob from the cache. The records holding it are taken
// as not cached until it is fetched again.
func (d *Images) dropBlob(digest string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = os.Remove(d.blobPath(digest))
}

// decompress reads a layer as its first bytes say it is compressed, as
// container runtimes do whatever its media type says.
func decompress(raw io.Reader) (io.ReadCloser, error) {
	r := bufio.NewReaderSize(raw, 1<<20)
	magic, err := r.Peek(4)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	switch {
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		return gzip.NewReader(r)
	case len(magic) == 4 && string(magic) == "\x28\xb5\x2f\xfd":
		z, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, err
		}
		return z.IOReadCloser(), nil
	}
	return io.NopCloser(r), nil
}

// ctxReader stops reading once its context is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// get returns the cached record of digest, pulling it first if need be,
// with its blobs pinned until release.
func (d *Images) get(ctx context.Context, ref, digest string, creds driver.Credentials) (*record, func(), error) {
	hex, err := hexOf(digest)
	if err != nil {
		return nil, nil, err
	}
	defer d.digests.Lock(hex)()
	if rec, release, err := d.cached(hex); err == nil {
		return rec, release, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	rec, err := d.pull(ctx, ref, digest, hex, creds)
	if err != nil {
		return nil, nil, err
	}
	return rec, d.pin(rec.blobs()), nil
}

// cached returns the record of hex with its blobs pinned, if the cache
// holds it.
func (d *Images) cached(hex string) (*record, func(), error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rec, err := d.readRecord(hex)
	if err != nil {
		return nil, nil, err
	}
	// A blob dropped from the cache is fetched again.
	for _, b := range rec.blobs() {
		if _, err := os.Stat(d.blobPath(b)); err != nil {
			return nil, nil, err
		}
	}
	return rec, d.pinLocked(rec.blobs()), nil
}

func (d *Images) readRecord(hex string) (*record, error) {
	b, err := os.ReadFile(d.recordPath(hex))
	if err != nil {
		return nil, err
	}
	var rec record
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("the cache's record of %s: %w", hex, err)
	}
	return &rec, nil
}

func (d *Images) pin(blobs []string) func() {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pinLocked(blobs)
}

func (d *Images) pinLocked(blobs []string) func() {
	for _, b := range blobs {
		d.pins[b]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			freed := false
			for _, b := range blobs {
				if d.pins[b]--; d.pins[b] <= 0 {
					delete(d.pins, b)
					freed = true
				}
			}
			if freed && d.spared {
				_ = d.collectLocked() // the next Forget tries again
			}
		})
	}
}

// pull fetches the image digest names for the host's platform into the
// cache and records it.
func (d *Images) pull(ctx context.Context, ref, digest, hex string, creds driver.Credentials) (*record, error) {
	base, err := name.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", driver.ErrImageNotFound, err)
	}
	pinned := base.Context().Digest(digest)
	opts := d.options(ctx, creds)
	desc, err := remote.Get(pinned, opts...)
	if err != nil {
		return nil, classify(pinned.String(), err)
	}
	var img v1.Image
	if desc.MediaType.IsIndex() {
		img, err = d.platformImage(desc)
	} else {
		img, err = desc.Image()
	}
	if err != nil {
		return nil, classify(pinned.String(), err)
	}
	m, err := img.Manifest()
	if err != nil {
		return nil, classify(pinned.String(), err)
	}
	mdigest, err := img.Digest()
	if err != nil {
		return nil, err
	}
	rec := &record{Manifest: mdigest.String()}
	if len(m.Layers) > maxLayers {
		return nil, fmt.Errorf("%w: %d layers", driver.ErrImageTooLarge, len(m.Layers))
	}
	var total int64
	for _, l := range m.Layers {
		if !l.MediaType.IsLayer() || !l.MediaType.IsDistributable() {
			return nil, fmt.Errorf("%s: layers of type %q are not pulled", pinned, l.MediaType)
		}
		if l.Size < 0 || l.Size > d.cfg.Limits.MaxBytes-total {
			return nil, fmt.Errorf("%w: its layers are more than %d bytes", driver.ErrImageTooLarge, d.cfg.Limits.MaxBytes)
		}
		total += l.Size
		if _, err := hexOf(l.Digest.String()); err != nil {
			return nil, fmt.Errorf("%s: layer %w", pinned, err)
		}
		rec.Layers = append(rec.Layers, layer{Digest: l.Digest.String(), MediaType: l.MediaType, Size: l.Size})
	}
	if m.Config.Size > maxConfigSize {
		return nil, fmt.Errorf("%w: a config of %d bytes", driver.ErrImageTooLarge, m.Config.Size)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, classify(pinned.String(), err)
	}
	if cf.OS != "" && (cf.OS != platform.OS || cf.Architecture != platform.Architecture) {
		return nil, fmt.Errorf("%s is %s/%s, not %s/%s", pinned, cf.OS, cf.Architecture, platform.OS, platform.Architecture)
	}
	rec.Config = driver.ImageConfig{
		Entrypoint: cf.Config.Entrypoint, Cmd: cf.Config.Cmd, Env: cf.Config.Env,
		WorkingDir: cf.Config.WorkingDir, User: cf.Config.User, Size: total,
	}

	release := d.pin(rec.blobs())
	defer release()
	for _, l := range rec.Layers {
		if err := d.fetchBlob(ctx, pinned.Context().Digest(l.Digest), l, opts); err != nil {
			return nil, err
		}
	}
	return rec, d.writeRecord(hex, rec)
}

// platformImage picks the host's platform from an index.
func (d *Images) platformImage(desc *remote.Descriptor) (v1.Image, error) {
	idx, err := desc.ImageIndex()
	if err != nil {
		return nil, err
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}
	// A variant names a newer CPU (amd64 v2 to v4) the host may lack: the
	// plainest one is taken.
	best, rank := v1.Hash{}, len(variants)
	for _, m := range im.Manifests {
		if !m.MediaType.IsImage() || m.Platform == nil || m.Platform.OS != platform.OS || m.Platform.Architecture != platform.Architecture {
			continue
		}
		if r := slices.Index(variants, m.Platform.Variant); r >= 0 && r < rank {
			best, rank = m.Digest, r
		}
	}
	if rank < len(variants) {
		return idx.Image(best)
	}
	return nil, fmt.Errorf("%w: no %s/%s image in the index %s", driver.ErrImageNotFound, platform.OS, platform.Architecture, desc.Digest)
}

// fetchBlob puts a layer in the cache, unless it is there, checked against
// its size and digest.
func (d *Images) fetchBlob(ctx context.Context, ref name.Digest, l layer, opts []remote.Option) error {
	final := d.blobPath(l.Digest)
	if _, err := os.Stat(final); err == nil {
		return nil
	}
	rl, err := remote.Layer(ref, opts...)
	if err != nil {
		return classify(ref.String(), err)
	}
	rc, err := rl.Compressed()
	if err != nil {
		return classify(ref.String(), err)
	}
	defer func() { _ = rc.Close() }()
	tmp, err := os.CreateTemp(filepath.Join(d.cfg.Cache, "tmp"), "blob-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	defer func() { _ = tmp.Close() }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(ctxReader{ctx, rc}, l.Size+1))
	if err != nil {
		return classify(ref.String(), err)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); n != l.Size || got != l.Digest {
		return fmt.Errorf("%s: the registry sent %d bytes of %s, not %d of %s", ref, n, got, l.Size, l.Digest)
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), final)
}

// writeRecord records a pulled digest, durably, once its blobs are in.
func (d *Images) writeRecord(hex string, rec *record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(d.cfg.Cache, "tmp"), "record-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return os.Rename(tmp.Name(), d.recordPath(hex))
}

func (d *Images) Forget(_ context.Context, digest string) error {
	hex, err := hexOf(digest)
	if err != nil {
		return err
	}
	defer d.digests.Lock(hex)()
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := os.Remove(d.recordPath(hex)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return d.collectLocked()
}

// collectLocked removes the blobs no record holds and nothing pins.
func (d *Images) collectLocked() error {
	used := map[string]bool{}
	records, err := os.ReadDir(filepath.Join(d.cfg.Cache, "images"))
	if err != nil {
		return err
	}
	for _, e := range records {
		hex, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		rec, err := d.readRecord(hex)
		if err != nil {
			return err // a record unread could hold any blob
		}
		for _, b := range rec.blobs() {
			used[b] = true
		}
	}
	blobs, err := os.ReadDir(filepath.Join(d.cfg.Cache, "blobs", "sha256"))
	if err != nil {
		return err
	}
	d.spared = false
	for _, e := range blobs {
		b := "sha256:" + e.Name()
		if !used[b] && d.pins[b] > 0 {
			d.spared = true
			continue
		}
		if !used[b] {
			if err := os.Remove(filepath.Join(d.cfg.Cache, "blobs", "sha256", e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// keychain answers go-containerregistry's lookups from driver.Credentials.
type keychain struct {
	ctx   context.Context
	creds driver.Credentials
}

func (k keychain) Resolve(r authn.Resource) (authn.Authenticator, error) {
	if k.creds == nil {
		return authn.Anonymous, nil
	}
	login, err := k.creds(k.ctx, r.RegistryStr())
	if err != nil {
		return nil, err
	}
	if login == (driver.Login{}) {
		return authn.Anonymous, nil
	}
	return authn.FromConfig(authn.AuthConfig{Username: login.Username, Password: login.Password, IdentityToken: login.IdentityToken}), nil
}

// classify wraps a registry's error in the driver error it is, if any.
func classify(what string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w", what, err)
	}
	var terr *transport.Error
	if errors.As(err, &terr) {
		for _, diag := range terr.Errors {
			switch diag.Code {
			case transport.UnauthorizedErrorCode, transport.DeniedErrorCode:
				return fmt.Errorf("%w: %s: %w", driver.ErrUnauthorized, what, err)
			case transport.ManifestUnknownErrorCode, transport.NameUnknownErrorCode, transport.BlobUnknownErrorCode:
				return fmt.Errorf("%w: %s: %w", driver.ErrImageNotFound, what, err)
			}
		}
		switch {
		case terr.StatusCode == http.StatusUnauthorized || terr.StatusCode == http.StatusForbidden:
			return fmt.Errorf("%w: %s: %w", driver.ErrUnauthorized, what, err)
		case terr.StatusCode == http.StatusNotFound:
			return fmt.Errorf("%w: %s: %w", driver.ErrImageNotFound, what, err)
		case terr.StatusCode == http.StatusTooManyRequests || terr.StatusCode >= 500:
			return fmt.Errorf("%w: %s: %w", driver.ErrRegistryUnreachable, what, err)
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	var nerr net.Error
	var operr *net.OpError
	if errors.As(err, &nerr) || errors.As(err, &operr) {
		return fmt.Errorf("%w: %s: %w", driver.ErrRegistryUnreachable, what, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}
