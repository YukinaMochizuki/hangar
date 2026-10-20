package registry_test

import (
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/ocilayer"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry/registrytest"
)

// cachedOf returns what d's cache lists of digest.
func cachedOf(t *testing.T, d driver.Images, digest string) (driver.CachedImage, bool) {
	t.Helper()
	c, err := d.Cached(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range c.Images {
		if i.Digest == digest {
			return i, true
		}
	}
	return driver.CachedImage{}, false
}

// TestTheCacheKeepsTheLastUse: a digest read from the cache is last used
// then, and a new instance on the same cache reads the same; its blobs are
// listed with their sizes, a blob two digests share under each.
func TestTheCacheKeepsTheLastUse(t *testing.T) {
	r := registrytest.New(t)
	cache := t.TempDir()
	d := newImages(t, cache, ocilayer.Limits{})
	shared := gzLayer(t, "shared", "x")
	ref1, digest1 := r.Push(t, "app:1", image(t, "amd64", nil, shared, gzLayer(t, "f", "1")))
	ref2, digest2 := r.Push(t, "app:2", image(t, "amd64", nil, shared, gzLayer(t, "f", "2")))
	for _, p := range [][2]string{{ref1, digest1}, {ref2, digest2}} {
		if _, err := d.Config(t.Context(), p[0], p[1], anon); err != nil {
			t.Fatal(err)
		}
	}
	sharedDigest, _ := shared.Digest()
	sharedSize, _ := shared.Size()
	for _, digest := range []string{digest1, digest2} {
		c, ok := cachedOf(t, d, digest)
		if !ok || len(c.Blobs) != 2 || c.Blobs[sharedDigest.String()] != sharedSize {
			t.Errorf("%s cached as %+v (listed: %t), want two blobs, %s of %d bytes", digest, c, ok, sharedDigest, sharedSize)
		}
	}

	pulled, _ := cachedOf(t, d, digest1)
	before := time.Now()
	r.Withdraw("app")
	if _, err := d.Config(t.Context(), ref1, digest1, anon); err != nil {
		t.Fatal(err)
	}
	used, _ := cachedOf(t, d, digest1)
	if used.UsedAt.Before(before) || !used.UsedAt.After(pulled.UsedAt) {
		t.Errorf("read at %v, pulled at %v: last used at %v", before, pulled.UsedAt, used.UsedAt)
	}
	if again, _ := cachedOf(t, newImages(t, cache, ocilayer.Limits{}), digest1); !again.UsedAt.Equal(used.UsedAt) {
		t.Errorf("last used at %v, by a new instance at %v", used.UsedAt, again.UsedAt)
	}
}
