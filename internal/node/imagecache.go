package node

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// The image cache's fullness, as kubelet's image garbage collector judges
// a disk, counting its blobs alone of what shares its room.
const (
	cacheHighPercent = 85
	cacheLowPercent  = 80
)

// evictImages removes the container images no root disk, pull or digest in
// used names, least recently used first, past the image cache's bounds and
// until need, if any, would fit (docs/decisions/node-agent.md). The caller
// holds the host lock.
func (a *Agent) evictImages(ctx context.Context, used map[string]bool, need *driver.NoSpaceError) (bool, error) {
	d := a.cfg.Drivers
	used = union(used, a.puller.digests())
	bases, err := d.Storage.Bases(ctx)
	if err != nil {
		return false, err
	}
	cache, err := d.Images.Cached(ctx)
	if err != nil {
		return false, err
	}
	type image struct {
		hasBase bool
		base    int64
		blobs   map[string]int64
		usedAt  time.Time
	}
	images := map[string]*image{}
	of := func(digest string, usedAt time.Time) *image {
		i := images[digest]
		if i == nil {
			i = &image{}
			images[digest] = i
		}
		if usedAt.After(i.usedAt) {
			i.usedAt = usedAt
		}
		return i
	}
	for _, b := range bases {
		i := of(b.Digest, b.UsedAt)
		i.hasBase, i.base = true, b.Size
	}
	// A digest being pulled is in use: Forget would fail on it, and its
	// base is about to be wanted.
	for _, c := range cache.Images {
		used[c.Digest] = used[c.Digest] || c.Busy
	}
	// holders counts the digests holding each blob, used or not.
	holders, inUse, sizes := map[string]int{}, map[string]bool{}, map[string]int64{}
	for _, c := range cache.Images {
		of(c.Digest, c.UsedAt).blobs = c.Blobs
		for b, n := range c.Blobs {
			holders[b]++
			sizes[b] = n
			inUse[b] = inUse[b] || used[c.Digest]
		}
	}
	// unusedBlobs is what evicting every candidate frees in the image room,
	// unusedBases in the storage, and held what the cache's blobs take.
	var unusedBlobs, unusedBases, held int64
	var candidates []string
	for digest, i := range images {
		if !used[digest] {
			candidates = append(candidates, digest)
			unusedBases += i.base
		}
	}
	for b, n := range sizes {
		held += n
		if !inUse[b] {
			unusedBlobs += n
		}
	}
	defer func() { a.metrics.imageCache.Set(float64(held)) }()
	slices.SortFunc(candidates, func(x, y string) int {
		return cmp.Or(images[x].usedAt.Compare(images[y].usedAt), cmp.Compare(x, y))
	})

	// Room is made by what the images evicted take as the drivers count
	// them, never by what is free read again meanwhile: ZFS frees in its
	// own time, and in bytes that compression makes fewer. A need of no
	// known room is the image room's.
	room := driver.ImageRoom
	// toFree is what need asks to be freed, and freed what has been.
	var toFree, freed int64
	if need != nil {
		evictable, total := unusedBlobs, cache.Total
		if need.Room == driver.StorageRoom {
			room = driver.StorageRoom
			c, err := d.Storage.Capacity(ctx)
			if err != nil {
				return false, err
			}
			evictable, total = unusedBases, c.Total
		}
		// Judged by what it is known to need alone: nothing goes for what
		// its room could not hold empty, nor what evicting all could not.
		if need.AtLeast() > total || evictable < need.AtLeast()-need.Free {
			a.cfg.Log.Info("no room to make for an image", "what", need.What, "need", need.AtLeast(),
				"free", need.Free, "evictable", evictable, "room", total)
			return false, nil
		}
		toFree = need.Need - need.Free
	}
	unused := unusedBlobs + unusedBases
	shrink := unused > a.cfg.ImageCacheHigh
	full := cache.Total > 0 && held*100 > cache.Total*cacheHighPercent
	heldTo := cache.Total * cacheLowPercent / 100
	evicted := false
	var errs []error
	for _, digest := range candidates {
		shrinking := shrink && unused > a.cfg.ImageCacheLow
		freeingCache := full && held > heldTo || need != nil && room == driver.ImageRoom && freed < toFree
		freeingStorage := need != nil && room == driver.StorageRoom && freed < toFree
		if !shrinking && !freeingCache && !freeingStorage {
			break
		}
		i := images[digest]
		var blobs int64
		for b, n := range i.blobs {
			if holders[b] == 1 {
				blobs += n
			}
		}
		helps := shrinking && i.base+blobs > 0 || freeingCache && blobs > 0 || freeingStorage && i.base > 0
		if !helps {
			continue
		}
		// The base goes first: one that will not leaves the image whole.
		if i.hasBase {
			if err := d.Storage.DeleteBase(ctx, digest); err != nil {
				errs = append(errs, err)
				continue
			}
			evicted = true
			unused -= i.base
			if room == driver.StorageRoom {
				freed += i.base
			}
		}
		a.puller.forget(digest)
		// Not waited for: a pull that took it up since it was listed holds it
		// for as long as it takes, and remakes the base.
		if err := d.Images.Forget(ctx, digest); errors.Is(err, driver.ErrBusy) {
			a.cfg.Log.Info("passing by an image being pulled", "digest", digest)
			continue
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		a.cfg.Log.Info("removed an image no machine has used lately", "digest", digest, "used_at", i.usedAt)
		evicted = true
		for b, n := range i.blobs {
			if holders[b]--; holders[b] == 0 {
				unused -= n
				held -= n
				if room == driver.ImageRoom {
					freed += n
				}
			}
		}
	}
	return evicted, errors.Join(errs...)
}

// union returns the union of sets.
func union(sets ...map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, s := range sets {
		for k := range s {
			out[k] = true
		}
	}
	return out
}

// makeRoom removes images nothing uses, least recently used first, until
// what full asked for would fit, and says whether it removed any. Nothing
// goes if not enough can.
func (a *Agent) makeRoom(ctx context.Context, full *driver.NoSpaceError) bool {
	a.host.Lock()
	defer a.host.Unlock()
	roots, err := a.cfg.Drivers.Storage.Roots(ctx)
	if err != nil {
		a.cfg.Log.Warn("making room for an image", "error", err)
		return false
	}
	used := map[string]bool{}
	for _, r := range roots {
		used[r.Digest] = true
	}
	evicted, err := a.evictImages(ctx, used, full)
	if err != nil {
		a.cfg.Log.Warn("making room for an image", "error", err)
	}
	return evicted
}
