//go:build testbed

package disks

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry/registrytest"
)

// TestAStaleLinkIsNotTaken: a root disk made again, as for a new image, is
// not handed out while its /dev/zvol link, udev held back, still leads to
// another zvol that took the old one's minor.
func TestAStaleLinkIsNotTaken(t *testing.T) {
	h := newHost(t)
	images, s := h.images(t), h.storage(t)
	ctx := context.Background()
	r := registrytest.New(t)
	ref, digest := r.Push(t, "app:1", smallImage(t, 1))
	if err := s.EnsureBase(ctx, digest, 0, func(dir string) error { return images.Unpack(ctx, ref, digest, dir, anon) }); err != nil {
		t.Fatal(err)
	}
	first, err := s.EnsureRoot(ctx, "x", digest, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	firstDev, _ := filepath.EvalSymlinks(first.Path)

	mustRun(t, "udevadm", "settle")
	mustRun(t, "udevadm", "control", "--stop-exec-queue")
	started := false
	defer func() {
		if !started {
			_ = exec.Command("udevadm", "control", "--start-exec-queue").Run()
		}
	}()
	if err := s.DeleteRoot(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	// Another zvol takes the freed minor; the old link still leads there.
	other := h.dataset + "/other"
	mustRun(t, "zfs", "create", "-s", "-V", "64M", other)
	h.decoys[other] = guid(t, other)
	stale, _ := filepath.EvalSymlinks(first.Path)
	name := strings.TrimSpace(mustRun(t, "/lib/udev/zvol_id", stale))
	t.Logf("the link of the deleted root leads to %s, which is %s (the root was %s)", stale, name, firstDev)
	if name != other {
		t.Logf("the minor went elsewhere this time; the link is only dangling")
	}

	type result struct {
		d   driver.Disk
		err error
	}
	done := make(chan result, 1)
	go func() {
		d, err := s.EnsureRoot(ctx, "x", digest, 1<<30)
		done <- result{d, err}
	}()
	select {
	case res := <-done:
		t.Fatalf("handed out %+v (%v) while its link led to %s", res.d, res.err, name)
	case <-time.After(2 * time.Second):
	}
	mustRun(t, "udevadm", "control", "--start-exec-queue")
	started = true
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	dev, err := filepath.EvalSymlinks(res.d.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(mustRun(t, "/lib/udev/zvol_id", dev)); got != h.dataset+"/roots/x" {
		t.Errorf("the root's link leads to %s, which is %s", dev, got)
	}
	mustRun(t, "udevadm", "settle")
	if err := s.DeleteRoot(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBase(ctx, digest); err != nil {
		t.Fatal(err)
	}
}
