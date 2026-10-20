package node

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
)

// busyImages is the fake's Images, but a pull of busy is under way, which
// Cached says unless unlisted, as when the pull began since.
type busyImages struct {
	driver.Images
	busy     string
	unlisted bool
}

func (b busyImages) Cached(ctx context.Context) (driver.ImageCache, error) {
	c, err := b.Images.Cached(ctx)
	for i := range c.Images {
		c.Images[i].Busy = c.Images[i].Digest == b.busy && !b.unlisted
	}
	return c, err
}

func (b busyImages) Forget(ctx context.Context, digest string) error {
	if digest == b.busy {
		return fmt.Errorf("pulling %s: %w", digest, driver.ErrBusy)
	}
	return b.Images.Forget(ctx, digest)
}

// evictionAgent is an agent on host, with images as its Images driver,
// that keeps up to high bytes of images no machine uses, and images of
// each digest, made in that order, nothing using them.
func evictionAgent(t *testing.T, host *fake.Host, images driver.Images, high int64, digests ...string) *Agent {
	t.Helper()
	a, err := New(Config{Name: "n1", ImageCacheHigh: high, ImageCacheLow: high, Drivers: &Drivers{
		Images: images, DiskImages: host.DiskImages(), Storage: host.Storage(), Network: host.Network(),
		Hypervisor: host.Hypervisor(), Guest: host.Guest(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range digests {
		host.AddImage(d, d, driver.ImageConfig{Cmd: []string{"true"}, Size: 10 << 20})
		if err := host.Storage().EnsureBase(t.Context(), d, 0, func(dir string) error {
			return host.Images().Unpack(t.Context(), d, d, dir, driver.Anonymous)
		}); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// TestEvictionCountsThePullsOfNow: a digest a pull took up after the sweep
// began is not evicted.
func TestEvictionCountsThePullsOfNow(t *testing.T) {
	host := fake.New()
	a := evictionAgent(t, host, host.Images(), 1, "sha256:a")
	// As a pull resolving its reference meanwhile does.
	a.puller.jobs["a@"] = &pull{running: true, digest: "sha256:a"}
	if _, err := a.evictImages(t.Context(), map[string]bool{}, nil); err != nil {
		t.Fatal(err)
	}
	if !host.Cached("sha256:a") || len(host.Snapshot().Bases) != 1 {
		t.Errorf("an image a pull took up was evicted: cached %t, bases %v", host.Cached("sha256:a"), host.Snapshot().Bases)
	}
}

// TestANeedOfNoKnownRoomIsTheImageRooms: room asked for with no room said
// is made in the image room.
func TestANeedOfNoKnownRoomIsTheImageRooms(t *testing.T) {
	host := fake.New()
	host.SetImageCacheSize(100 << 20)
	a := evictionAgent(t, host, host.Images(), DefaultImageCacheHigh, "sha256:a")
	// 90 MiB free, the image's 10 to make.
	evicted, err := a.evictImages(t.Context(), map[string]bool{}, &driver.NoSpaceError{What: "something", Need: 95 << 20, Free: 90 << 20})
	if err != nil || !evicted || host.Cached("sha256:a") {
		t.Errorf("evicted %t (%v), cached %t; want the image evicted", evicted, err, host.Cached("sha256:a"))
	}
}

// TestARoomTooSmallEmptyEvictsNothing: room asked for that is known to be
// more than its room holds empty evicts nothing, though there would be as
// much to evict as was asked for.
func TestARoomTooSmallEmptyEvictsNothing(t *testing.T) {
	host := fake.New()
	a := evictionAgent(t, host, host.Images(), DefaultImageCacheHigh, "sha256:a", "sha256:b")
	// The quota cut below what the cache holds: 20 MiB of blobs in 15.
	host.SetImageCacheSize(15 << 20)
	evicted, err := a.evictImages(t.Context(), map[string]bool{}, &driver.NoSpaceError{What: "something", Need: 20 << 20, Least: 16 << 20})
	if err != nil || evicted || !host.Cached("sha256:a") || !host.Cached("sha256:b") {
		t.Errorf("evicted %t (%v), cached a %t, b %t; want nothing evicted", evicted, err, host.Cached("sha256:a"), host.Cached("sha256:b"))
	}
}

// TestAnUnpackEvictsWhatItAsksFor: room asked for is made by what the
// images evicted take, the least recently used first, until what was asked
// for is freed, whatever the storage says is free meanwhile.
func TestAnUnpackEvictsWhatItAsksFor(t *testing.T) {
	host := fake.New()
	a := evictionAgent(t, host, host.Images(), DefaultImageCacheHigh, "sha256:a", "sha256:b", "sha256:c")
	// 15 MiB asked for: two images of 10.
	evicted, err := a.evictImages(t.Context(), map[string]bool{}, &driver.NoSpaceError{What: "unpacking", Need: 15 << 20, Least: 1, Free: 0})
	if err != nil || !evicted || host.Cached("sha256:a") || host.Cached("sha256:b") || !host.Cached("sha256:c") {
		t.Errorf("evicted %t (%v), cached a %t, b %t, c %t; want a and b evicted", evicted, err, host.Cached("sha256:a"), host.Cached("sha256:b"), host.Cached("sha256:c"))
	}
}

// TestABaseThatWillNotGoKeepsItsImage: an image whose base cannot be
// deleted keeps its cache record and its finished pull, so it is still
// whole, and still found ready.
func TestABaseThatWillNotGoKeepsItsImage(t *testing.T) {
	host := fake.New()
	a := evictionAgent(t, host, host.Images(), 1, "sha256:a")
	a.puller.jobs["a@"] = &pull{ready: true, digest: "sha256:a"}
	host.FailDeleteBase(errors.New("dataset is busy"))
	if _, err := a.evictImages(t.Context(), map[string]bool{}, nil); err == nil {
		t.Error("a base that would not go was not reported")
	}
	if !host.Cached("sha256:a") || len(host.Snapshot().Bases) != 1 || a.puller.jobs["a@"] == nil {
		t.Errorf("cached %t, bases %v, job kept %t; want all kept", host.Cached("sha256:a"), host.Snapshot().Bases, a.puller.jobs["a@"] != nil)
	}
}

// TestAPullTakenUpSinceTheListingIsNotWaitedFor: an image a pull took up
// after the cache was listed loses its base, which the pull makes again,
// and keeps its cache record, with no error and no wait.
func TestAPullTakenUpSinceTheListingIsNotWaitedFor(t *testing.T) {
	host := fake.New()
	a := evictionAgent(t, host, busyImages{Images: host.Images(), busy: "sha256:a", unlisted: true}, 1, "sha256:a")
	if _, err := a.evictImages(t.Context(), map[string]bool{}, nil); err != nil {
		t.Errorf("an image a pull took up since: %v", err)
	}
	if !host.Cached("sha256:a") {
		t.Error("the cache record of an image being pulled went")
	}
}

// TestEvictionPassesByAnImageBeingPulled: an image the cache lists as being
// pulled is passed by, base and record, and the next least recently used
// goes in its place.
func TestEvictionPassesByAnImageBeingPulled(t *testing.T) {
	host := fake.New()
	a := evictionAgent(t, host, busyImages{Images: host.Images(), busy: "sha256:a"}, 1, "sha256:a", "sha256:b")
	if _, err := a.evictImages(t.Context(), map[string]bool{}, nil); err != nil {
		t.Errorf("passing by an image being pulled: %v", err)
	}
	if b := host.Snapshot().Bases; fmt.Sprint(b) != "[sha256:a]" || !host.Cached("sha256:a") || host.Cached("sha256:b") {
		t.Errorf("bases %v, cached a %t, b %t; want a kept, b evicted", b, host.Cached("sha256:a"), host.Cached("sha256:b"))
	}
}
