package e2e

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

const mib = 1 << 20

// runOnce runs a container of image until it runs, then deletes it.
func (w *world) runOnce(name, image string) {
	w.t.Helper()
	w.apply(container(name, image))
	w.waitReason(name, "Running")
	w.delete(name)
	w.waitGone(name)
}

// kept checks that the host keeps exactly the images of digests, base
// and cache record.
func (w *world) kept(what string, digests ...string) {
	w.t.Helper()
	bases := w.host.Snapshot().Bases
	var cached []string
	for _, d := range bases {
		if w.host.Cached(d) {
			cached = append(cached, d)
		}
	}
	slices.Sort(digests)
	if !slices.Equal(bases, digests) || !slices.Equal(cached, digests) {
		w.t.Errorf("%s: bases %v, of them cached %v; want %v", what, bases, cached, digests)
	}
}

// TestARecreatedContainerPullsNothing: a container deleted and made again
// finds its image kept, and pulls and unpacks nothing again.
func TestARecreatedContainerPullsNothing(t *testing.T) {
	w := newWorld(t)
	w.runOnce("api", "nginx")
	w.waitHostEmpty()
	w.sweeps(1)
	w.kept("after the container went", "sha256:nginx-1")

	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	if n := w.host.Pulls("sha256:nginx-1"); n != 1 {
		t.Errorf("the image was pulled %d times", n)
	}
	if n := w.host.Unpacked("sha256:nginx-1"); n != 1 {
		t.Errorf("the image was unpacked %d times", n)
	}
}

// TestTheLeastRecentlyUsedImageGoes: past the bound, the image used
// longest ago goes, though another was pulled before it.
func TestTheLeastRecentlyUsedImageGoes(t *testing.T) {
	w := newIdleWorld(t)
	// Three bases of 64 MiB; two fit.
	w.imageCacheHigh, w.imageCacheLow = 150*mib, 150*mib
	for _, name := range []string{"a", "b", "c"} {
		w.host.AddImage(name, "sha256:"+name, nginx)
	}
	w.startAgent()
	w.runOnce("first", "a")
	w.runOnce("second", "b")
	w.runOnce("again", "a")
	w.sweeps(1)
	w.kept("two images, within the bound", "sha256:a", "sha256:b")

	w.runOnce("third", "c")
	testutil.Eventually(t, "an image gone", func() (bool, string) {
		b := w.host.Snapshot().Bases
		return len(b) == 2, fmt.Sprint(b)
	})
	w.kept("past the bound", "sha256:a", "sha256:c")
}

// TestAnImageUsedLongerGoesLater: an image whose container ran until
// later outlasts one pulled after it but let go before it: its use lasted
// until its root disk went.
func TestAnImageUsedLongerGoesLater(t *testing.T) {
	w := newIdleWorld(t)
	// Three bases of 64 MiB; two fit.
	w.imageCacheHigh, w.imageCacheLow = 150*mib, 150*mib
	for _, name := range []string{"long", "short", "c"} {
		w.host.AddImage(name, "sha256:"+name, nginx)
	}
	w.startAgent()
	w.apply(container("long", "long"))
	w.waitReason("long", "Running")
	w.runOnce("brief", "short")
	w.delete("long")
	w.waitGone("long")
	w.sweeps(1)
	w.kept("two images, within the bound", "sha256:long", "sha256:short")

	w.runOnce("third", "c")
	testutil.Eventually(t, "an image gone", func() (bool, string) {
		b := w.host.Snapshot().Bases
		return len(b) == 2, fmt.Sprint(b)
	})
	w.kept("past the bound", "sha256:long", "sha256:c")
}

// TestAnImageInUseIsNeitherEvictedNorCounted: the image a running container
// uses stays, and only the images no machine uses count against the bound.
func TestAnImageInUseIsNeitherEvictedNorCounted(t *testing.T) {
	w := newIdleWorld(t)
	w.imageCacheHigh, w.imageCacheLow = 100*mib, 100*mib
	big := nginx
	big.Size = 1 << 30
	w.host.AddImage("big", "sha256:big", big)
	w.host.AddImage("small", "sha256:small", nginx)
	w.startAgent()
	w.apply(container("keep", "big"))
	w.waitReason("keep", "Running")
	w.runOnce("gone", "small")
	w.sweeps(2)
	w.kept("one image in use, past the bound alone, and one not", "sha256:big", "sha256:small")
}

// TestAnImageBeingPulledIsKept: the image a pull is still making a base of
// stays in the cache, however far past the bound.
func TestAnImageBeingPulledIsKept(t *testing.T) {
	w := newIdleWorld(t)
	w.imageCacheHigh, w.imageCacheLow = 1, 1
	big := nginx
	big.Size = 1 << 30
	w.host.AddImage("big", "sha256:big", big)
	letGo := w.host.HoldUnpack("big")
	defer letGo()
	w.startAgent()
	w.apply(container("api", "big"))
	w.waitReason("api", computeapi.ReasonPulling)
	testutil.Eventually(t, "the image fetched", func() bool { return w.host.Cached("sha256:big") })
	w.sweeps(2)
	if !w.host.Cached("sha256:big") {
		t.Error("an image being pulled was evicted")
	}
	letGo()
	w.waitReason("api", "Running")
	if n := w.host.Pulls("sha256:big"); n != 1 {
		t.Errorf("the image was pulled %d times", n)
	}
}

// TestTheLastUseOutlivesTheAgent: an agent started again evicts by the
// last uses the host recorded, not by name.
func TestTheLastUseOutlivesTheAgent(t *testing.T) {
	w := newWorld(t)
	// By name, sha256:a-new would go first.
	w.host.AddImage("old", "sha256:z-old", nginx)
	w.host.AddImage("new", "sha256:a-new", nginx)
	w.runOnce("first", "old")
	w.runOnce("second", "new")
	w.waitHostEmpty()
	w.stopAgent()

	w.imageCacheHigh, w.imageCacheLow = 100*mib, 100*mib
	w.startAgent()
	testutil.Eventually(t, "an image gone", func() (bool, string) {
		b := w.host.Snapshot().Bases
		return len(b) == 1, fmt.Sprint(b)
	})
	w.kept("after the restart", "sha256:a-new")
}

// TestAFullCacheIsCleared: an image cache past 85% of its room is cleared
// of images no machine uses, however far from the bound they are.
func TestAFullCacheIsCleared(t *testing.T) {
	w := newIdleWorld(t)
	large := nginx
	large.Size = 90 * mib
	w.host.AddImage("large", "sha256:large", large)
	w.startAgent()
	w.runOnce("api", "large")
	// The quota cut: 90 MiB of blobs in 100.
	w.host.SetImageCacheSize(100 * mib)
	testutil.Eventually(t, "the image evicted", func() bool { return !w.host.Cached("sha256:large") })
	w.kept("a full cache cleared")
}

// TestAPullMakesRoomInTheCache: a pull the cache has no room for evicts the
// images no machine uses until it fits, rather than fail.
func TestAPullMakesRoomInTheCache(t *testing.T) {
	w := newIdleWorld(t)
	one, two := nginx, nginx
	one.Size, two.Size = 60*mib, 45*mib
	w.host.AddImage("one", "sha256:one", one)
	w.host.AddImage("two", "sha256:two", two)
	w.startAgent()
	w.runOnce("first", "one")
	// The quota cut: 40 MiB free, two's blobs 45, and as much to unpack.
	w.host.SetImageCacheSize(100 * mib)
	w.sweeps(1)
	w.kept("an image within the cache's room", "sha256:one")

	w.apply(container("second", "two"))
	w.waitReason("second", "Running")
	w.kept("room made", "sha256:two")
	if n := w.host.Pulls("sha256:two"); n != 1 {
		t.Errorf("the image was pulled %d times", n)
	}
}

// TestAnUnpackMakesRoom: an image whose blobs fit the cache but which has
// no room to unpack beside them evicts the images no machine uses, and is
// unpacked.
func TestAnUnpackMakesRoom(t *testing.T) {
	w := newIdleWorld(t)
	w.host.SetImageCacheSize(150 * mib)
	old, small := nginx, nginx
	old.Size, small.Size = 60*mib, 30*mib
	w.host.AddImage("old", "sha256:old", old)
	w.host.AddImage("new", "sha256:new", small)
	// 60 MiB of old's blobs and 30 of its own leave 60 to unpack 80 in.
	w.host.SetUnpackedSize("sha256:new", 80*mib)
	w.startAgent()
	w.runOnce("first", "old")
	w.sweeps(1)
	w.kept("an image within the cache's room", "sha256:old")

	w.apply(container("second", "new"))
	w.waitReason("second", "Running")
	w.kept("room made", "sha256:new")
}

// TestAnUnpackShortOfLittleMakesRoom: an unpack whose shortfall one image
// no machine uses covers evicts it and is unpacked, though it asks for
// more than could be made.
func TestAnUnpackShortOfLittleMakesRoom(t *testing.T) {
	w := newIdleWorld(t)
	w.host.SetImageCacheSize(100 * mib)
	u, n := nginx, nginx
	u.Size, n.Size = 10*mib, 20*mib
	w.host.AddImage("u", "sha256:u", u)
	w.host.AddImage("new", "sha256:new", n)
	w.host.SetUnpackedSize("sha256:new", 55*mib)
	w.startAgent()
	w.runOnce("first", "u")
	w.sweeps(1)
	// A download takes 20 MiB: with 30 of blobs, 50 are free to unpack 55
	// in, asking for 20 more; evicting u makes 10.
	w.host.TakeImageRoom(20 * mib)
	w.apply(container("second", "new"))
	w.waitReason("second", "Running")
	w.kept("room made", "sha256:new")
}

// unusedKept returns which of the images named name and a number below n
// the cache keeps.
func (w *world) unusedKept(name string, n int) []string {
	var kept []string
	for i := range n {
		if d := fmt.Sprintf("sha256:%s%d", name, i); w.host.Cached(d) {
			kept = append(kept, d)
		}
	}
	return kept
}

// TestAnUnpackEvictsItsSizeATry: an unpack 3 MiB short evicts the least
// recently used images that free its compressed size, and no more, by what
// they take, though what the storage says is free lags their going.
func TestAnUnpackEvictsItsSizeATry(t *testing.T) {
	for name, deferred := range map[string]bool{"frees at once": false, "frees deferred": true} {
		t.Run(name, func(t *testing.T) {
			w := newIdleWorld(t)
			w.host.DeferFrees(deferred)
			n := nginx
			n.Size = 20 * mib
			for i := range 8 {
				u := nginx
				u.Size = 10 * mib
				w.host.AddImage(fmt.Sprint("u", i), fmt.Sprint("sha256:u", i), u)
			}
			w.host.AddImage("new", "sha256:new", n)
			w.host.SetUnpackedSize("sha256:new", 25*mib)
			w.startAgent()
			for i := range 8 {
				w.runOnce(fmt.Sprint("c", i), fmt.Sprint("u", i))
			}
			w.host.SetImageCacheSize(200 * mib)
			w.sweeps(1)
			// 80 MiB of blobs, new's 20, and a download's 78: 22 free to
			// unpack 25, asking for 20 more: two images of 10.
			w.host.TakeImageRoom(78 * mib)
			w.apply(container("second", "new"))
			w.waitReason("second", "Running")
			if kept := w.unusedKept("u", 8); len(kept) != 6 || slices.Contains(kept, "sha256:u0") || slices.Contains(kept, "sha256:u1") {
				t.Errorf("kept %v of 8, want all but the two least recently used", kept)
			}
		})
	}
}

// TestAJustDeletedContainersImageMakesRoom: the image of a container just
// deleted makes room for a pull at once, no sweep having forgotten the
// pull it was made by.
func TestAJustDeletedContainersImageMakesRoom(t *testing.T) {
	w := newIdleWorld(t)
	w.sweepInterval = time.Hour
	old, n := nginx, nginx
	old.Size, n.Size = 60*mib, 45*mib
	w.host.AddImage("old", "sha256:old", old)
	w.host.AddImage("new", "sha256:new", n)
	w.startAgent()
	w.runOnce("first", "old")
	testutil.Eventually(t, "the root disk gone", func() bool { return len(w.host.Snapshot().Roots) == 0 })
	// The quota cut: 40 MiB free, new's blobs 45, and as much to unpack.
	w.host.SetImageCacheSize(100 * mib)
	sweeps := w.agent.Activity().Sweeps
	w.apply(container("second", "new"))
	w.waitReason("second", "Running")
	w.kept("room made", "sha256:new")
	if n := w.agent.Activity().Sweeps; n != sweeps {
		t.Errorf("%d sweeps meanwhile; the test shows nothing", n-sweeps)
	}
}

// TestABaseMakesRoomInTheStorage: a base the storage has no room for evicts
// the least recently used images no machine uses, and is made.
func TestABaseMakesRoomInTheStorage(t *testing.T) {
	w := newIdleWorld(t)
	// Bases of 4 GiB; two fit, and room for a root disk of 10 GiB, as the
	// fake's root disks take none.
	w.host.SetBaseSize(4 << 30)
	w.host.SetStorageSize(10<<30 + 100*mib)
	for _, name := range []string{"a", "b", "c"} {
		w.host.AddImage(name, "sha256:"+name, nginx)
	}
	w.startAgent()
	w.runOnce("first", "a")
	w.runOnce("second", "b")
	w.sweeps(1)
	w.kept("two images, within the storage", "sha256:a", "sha256:b")

	w.apply(container("third", "c"))
	w.waitReason("third", "Running")
	w.kept("room made", "sha256:b", "sha256:c")
}

// TestAHopelessPullEvictsNothing: a pull that would not fit with every
// image no machine uses gone evicts none of them.
func TestAHopelessPullEvictsNothing(t *testing.T) {
	w := newIdleWorld(t)
	w.host.SetImageCacheSize(100 * mib)
	small, huge := nginx, nginx
	small.Size, huge.Size = 10*mib, 200*mib
	w.host.AddImage("small", "sha256:small", small)
	w.host.AddImage("huge", "sha256:huge", huge)
	w.startAgent()
	w.runOnce("first", "small")
	w.sweeps(1)
	w.apply(container("api", "huge"))
	w.waitReason("api", computeapi.ReasonDiskFailed)
	w.sweeps(1)
	w.kept("after a pull that could never fit", "sha256:small")
}

// TestAnEvictedImageIsPulledAgain: an image evicted and asked for again is
// pulled again, and runs.
func TestAnEvictedImageIsPulledAgain(t *testing.T) {
	w := newIdleWorld(t)
	// Bases of 64 MiB; one fits.
	w.imageCacheHigh, w.imageCacheLow = 100*mib, 100*mib
	w.host.AddImage("a", "sha256:a", nginx)
	w.host.AddImage("b", "sha256:b", nginx)
	w.startAgent()
	w.runOnce("first", "a")
	w.runOnce("second", "b")
	testutil.Eventually(t, "a evicted", func() bool { return !w.host.Cached("sha256:a") })
	w.kept("past the bound", "sha256:b")

	w.apply(container("again", "a"))
	w.waitReason("again", "Running")
	if n := w.host.Pulls("sha256:a"); n != 2 {
		t.Errorf("an evicted image was pulled %d times, want twice", n)
	}
}

// TestRootDisksKeepTheirImagesAfterARestart: a stopped container's root disk
// keeps its image, though the agent, started again, has no pull of it to
// count: only the root disk says it is in use.
func TestRootDisksKeepTheirImagesAfterARestart(t *testing.T) {
	w := newIdleWorld(t)
	w.imageCacheHigh, w.imageCacheLow = 100*mib, 100*mib
	big := nginx
	big.Size = 1 << 30
	w.host.AddImage("big", "sha256:big", big)
	w.host.AddImage("small", "sha256:small", nginx)
	w.startAgent()
	c := container("keep", "big")
	w.apply(c)
	w.waitReason("keep", "Running")
	c.Spec.RunState = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED
	w.apply(c)
	w.waitStopped("keep", metav1.ConditionStatus_CONDITION_STATUS_TRUE)
	w.runOnce("gone", "small")
	w.stopAgent()

	w.startAgent()
	w.waitSettled()
	w.sweeps(2)
	w.kept("a stopped container's image, and one past the bound only if it counted", "sha256:big", "sha256:small")
}

// TestTheCacheJudgesItsOwnFullness: what else takes the image room, as
// downloads and unpacks under way do, does not make the cache full.
func TestTheCacheJudgesItsOwnFullness(t *testing.T) {
	w := newIdleWorld(t)
	w.host.SetImageCacheSize(100 * mib)
	half := nginx
	half.Size = 50 * mib
	w.host.AddImage("half", "sha256:half", half)
	w.startAgent()
	w.runOnce("api", "half")
	// 50 MiB of blobs and 40 of a download: 90% taken, 50% the cache's.
	w.host.TakeImageRoom(40 * mib)
	w.sweeps(2)
	w.kept("a cache half full", "sha256:half")
}

// TestNoRoomInTheCacheIsTheDisks: a pull the cache has no room for, even
// with every image no machine uses gone, is the disk's failure.
func TestNoRoomInTheCacheIsTheDisks(t *testing.T) {
	w := newIdleWorld(t)
	w.host.SetImageCacheSize(100 * mib)
	huge := nginx
	huge.Size = 200 * mib
	w.host.AddImage("huge", "sha256:huge", huge)
	w.startAgent()
	w.apply(container("api", "huge"))
	c := w.waitReason("api", computeapi.ReasonDiskFailed)
	if msg := ready(c).GetMessage(); !strings.Contains(msg, driver.ErrNoSpace.Error()) {
		t.Errorf("message %q, want it to say %q", msg, driver.ErrNoSpace)
	}
}
