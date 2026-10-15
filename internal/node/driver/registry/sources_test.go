package registry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/ocilayer"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry/registrytest"
)

// The cache answers a digest only for the repositories it was fetched from
// in full (docs/decisions/secrets.md).

var userPass = driver.Credentials(func(context.Context, string) (driver.Login, error) {
	return driver.Login{Username: "user", Password: "pass"}, nil
})

// sourcesOf reads the repositories the cache's record of digest names.
func sourcesOf(t *testing.T, cache, digest string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cache, "images", strings.TrimPrefix(digest, "sha256:")+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Sources []string `json:"sources"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	return rec.Sources
}

func repoOf(t *testing.T, ref string) string {
	t.Helper()
	repo, err := driver.Repository(ref)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

// TestARecordWithoutSourcesHasNone: a digest a node cached before sources
// were recorded is fetched in full again for the first reference to it,
// which then becomes its source.
func TestARecordWithoutSourcesHasNone(t *testing.T) {
	r := registrytest.New(t)
	cache := t.TempDir()
	d := newImages(t, cache, ocilayer.Limits{})
	img := image(t, "amd64", []string{"true"}, gzLayer(t, "f", "old"))
	ref, digest := r.Push(t, "old:1", img)
	if _, err := d.Config(t.Context(), ref, digest, anon); err != nil {
		t.Fatal(err)
	}
	layers, _ := img.Layers()
	ld, _ := layers[0].Digest()
	record := filepath.Join(cache, "images", strings.TrimPrefix(digest, "sha256:")+".json")
	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec["sources"]; !ok {
		t.Fatalf("a new record has no sources: %s", b)
	}
	delete(rec, "sources")
	if b, err = json.Marshal(rec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record, b, 0o600); err != nil {
		t.Fatal(err)
	}

	// The agent restarts on the old cache.
	d = newImages(t, cache, ocilayer.Limits{})
	before := r.BlobGets(ld.String())
	if _, err := d.Config(t.Context(), ref, digest, anon); err != nil {
		t.Fatal(err)
	}
	if r.BlobGets(ld.String()) != before+1 {
		t.Errorf("a record without sources answered from the cache: %d blob fetches, want 1", r.BlobGets(ld.String())-before)
	}
	if got := sourcesOf(t, cache, digest); !slices.Equal(got, []string{repoOf(t, ref)}) {
		t.Errorf("sources %v after the fetch", got)
	}
	before = r.BlobGets(ld.String())
	if err := d.Unpack(t.Context(), ref, digest, t.TempDir(), anon); err != nil || r.BlobGets(ld.String()) != before {
		t.Errorf("its source again went to the registry: %v, %d fetches", err, r.BlobGets(ld.String())-before)
	}
}

// TestSourcesOutliveARestart: after the agent restarts, the cache still
// answers a digest's source without the registry, and still answers no
// other repository.
func TestSourcesOutliveARestart(t *testing.T) {
	r := registrytest.New(t)
	cache := t.TempDir()
	ref, digest := r.Push(t, "secret:1", image(t, "amd64", []string{"true"}, gzLayer(t, "f", "a's")))
	r.Private("secret")
	if _, err := newImages(t, cache, ocilayer.Limits{}).Config(t.Context(), ref, digest, userPass); err != nil {
		t.Fatal(err)
	}
	evil := r.Claim(t, "evil", digest)
	d := newImages(t, cache, ocilayer.Limits{})
	r.Withdraw("secret")
	if _, err := d.Config(t.Context(), ref, digest, anon); err != nil {
		t.Errorf("its source, with the registry down, after a restart: %v", err)
	}
	if _, err := d.Config(t.Context(), evil, digest, anon); err == nil {
		t.Error("a repository claiming the digest got it from the cache after a restart")
	}
	if got := sourcesOf(t, cache, digest); !slices.Equal(got, []string{repoOf(t, ref)}) {
		t.Errorf("sources %v", got)
	}
}

// TestABlobFetchedAgainMustMatch: a repository fetched from in full whose
// layer does not match its digest fails the pull, leaves the cached blob
// as it was, and does not become a source.
func TestABlobFetchedAgainMustMatch(t *testing.T) {
	r := registrytest.New(t)
	cache := t.TempDir()
	d := newImages(t, cache, ocilayer.Limits{})
	img := image(t, "amd64", []string{"true"}, gzLayer(t, "f", "content"))
	ref, digest := r.Push(t, "a:1", img)
	if _, err := d.Config(t.Context(), ref, digest, anon); err != nil {
		t.Fatal(err)
	}
	mirror := r.Mirror(t, "b:1", digest)
	layers, _ := img.Layers()
	ld, _ := layers[0].Digest()
	blob := filepath.Join(cache, "blobs", "sha256", ld.Hex)
	cached, err := os.ReadFile(blob)
	if err != nil {
		t.Fatal(err)
	}
	r.Tamper(ld.String(), []byte("not the layer"))
	// go-containerregistry's reader checks the digest as it reads, before
	// the driver's own check does.
	if _, err := d.Config(t.Context(), mirror, digest, anon); err == nil {
		t.Error("a mirror serving another layer was pulled")
	}
	if after, err := os.ReadFile(blob); err != nil || !bytes.Equal(after, cached) {
		t.Errorf("the cached blob changed: %v", err)
	}
	if got := sourcesOf(t, cache, digest); slices.Contains(got, repoOf(t, mirror)) {
		t.Errorf("the mirror became a source: %v", got)
	}
	if err := d.Unpack(t.Context(), ref, digest, t.TempDir(), anon); err != nil {
		t.Errorf("its first source after the mirror failed: %v", err)
	}
}
