//go:build testbed

package disks

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/drivertest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry/registrytest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/zfsstore"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/diskimgtest"
)

var anon = driver.Credentials(driver.Anonymous)

// smallImage is an image of a few files, one setuid, different for each n.
func smallImage(t testing.TB, n int) v1.Image {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, h := range []tar.Header{
		{Typeflag: tar.TypeDir, Name: "bin/", Mode: 0o755},
		{Typeflag: tar.TypeReg, Name: "bin/su", Mode: 0o4755, Size: 2},
		{Typeflag: tar.TypeReg, Name: "etc/n", Mode: 0o644, Size: int64(len(fmt.Sprint(n)))},
		{Typeflag: tar.TypeSymlink, Name: "lib", Linkname: "usr/lib"},
	} {
		if err := w.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		switch h.Name {
		case "bin/su":
			_, _ = w.Write([]byte("su"))
		case "etc/n":
			_, _ = w.Write([]byte(fmt.Sprint(n)))
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(buf.Bytes())
	_ = zw.Close()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(gz.Bytes())), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{OS: "linux", Architecture: "amd64", Config: v1.Config{Cmd: []string{"true"}}})
	if err != nil {
		t.Fatal(err)
	}
	if img, err = mutate.AppendLayers(img, l); err != nil {
		t.Fatal(err)
	}
	return img
}

// TestConformance holds the real image and storage drivers to the drivers'
// contract on the host's pool, with decoys about.
func TestConformance(t *testing.T) {
	drivertest.Run(t, func(t *testing.T) drivertest.Host {
		h := newHost(t)
		r := registrytest.New(t)
		srv := diskimgtest.New(t)
		n := 0
		return drivertest.Host{
			Images:     h.images(t),
			DiskImages: srv.Driver(t, filepath.Join(filepath.Dir(h.cache), "disk-images")),
			Storage:    h.storage(t),
			PublishDiskImage: func(t *testing.T, name string, size int64) (string, string) {
				return srv.QCOW2(t, name, size, "disk image "+name)
			},
			Publish: func(t *testing.T, name string) (string, string) {
				n++
				return r.Push(t, name, smallImage(t, n))
			},
			Withdraw: func(_ *testing.T, ref string) {
				r.Withdraw(strings.SplitN(strings.TrimPrefix(ref, r.Host+"/"), ":", 2)[0])
			},
			Reopen: func(t *testing.T) (driver.Images, driver.Storage) { return h.images(t), h.storage(t) },
			Hold: func(t *testing.T, d driver.Disk) func() {
				// A VM holds its root disk open, as Cloud Hypervisor does.
				f, err := os.OpenFile(d.Path, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				return func() { _ = f.Close() }
			},
			HoldShareVolume: func(t *testing.T, v driver.ShareVolume) func() {
				f, err := os.OpenFile(v.Path, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				return func() { _ = f.Close() }
			},
			HoldVolume: func(t *testing.T, v driver.Volume) func() {
				f, err := os.OpenFile(v.Path, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				return func() { _ = f.Close() }
			},
		}
	})
}

// TestDecoysAreLeftAlone: what is not the driver's, even at its names, is
// never listed, made over, or destroyed.
func TestDecoysAreLeftAlone(t *testing.T) {
	h := newHost(t)
	s := h.storage(t)
	ctx := context.Background()
	h.listing(t, "with the decoys")
	if bases, err := s.Bases(ctx); err != nil || len(bases) != 0 {
		t.Errorf("bases %v %v", bases, err)
	}
	if roots, err := s.Roots(ctx); err != nil || len(roots) != 0 {
		t.Errorf("roots %v %v", roots, err)
	}
	if err := s.DeleteBase(ctx, decoyDigest); err == nil {
		t.Error("deleted a base the driver did not make")
	}
	for _, d := range []string{decoyDigest, bareDecoyDigest} {
		if err := s.EnsureBase(ctx, d, 0, func(string) error { return nil }); err == nil {
			t.Errorf("made a base of %s over one the driver did not make", d)
		}
		if err := s.DeleteBase(ctx, d); err == nil {
			t.Errorf("deleted a base of %s the driver did not make", d)
		}
	}
	for _, uid := range []string{"decoy", "m-decoy"} {
		if err := s.DeleteRoot(ctx, uid); err == nil {
			t.Errorf("deleted the root %s the driver did not make", uid)
		}
		if _, err := s.EnsureRoot(ctx, uid, decoyDigest, 1<<30); err == nil {
			t.Errorf("took %s as a root disk", uid)
		}
	}
	// A new instance clears unfinished bases of its own, not these.
	h.storage(t)
	for _, bad := range []string{"../x", "a/b", "", "-x", "x@snap"} {
		if err := s.DeleteRoot(ctx, bad); err == nil {
			t.Errorf("deleted the root of %q", bad)
		}
	}
	if err := s.DeleteBase(ctx, "sha256:../../x"); err == nil {
		t.Error("deleted the base of a digest that is not one")
	}
	if vols, err := s.Volumes(ctx); err != nil || len(vols) != 0 {
		t.Errorf("volumes %v %v", vols, err)
	}
	if bases, err := s.ImageBases(ctx); err != nil || len(bases) != 0 {
		t.Errorf("image bases %v %v", bases, err)
	}
	if err := s.EnsureImageBase(ctx, decoyDigest, 1<<20, func(string) error { return nil }); err == nil {
		t.Error("made an image base over one the driver did not make")
	}
	if err := s.DeleteImageBase(ctx, decoyDigest); err == nil {
		t.Error("deleted an image base the driver did not make")
	}
	for _, uid := range []string{"decoy", "d-decoy"} {
		if err := s.DeleteVolume(ctx, uid); err == nil {
			t.Errorf("deleted the volume %s the driver did not make", uid)
		}
		if _, err := s.EnsureVolume(ctx, uid, 1<<30, ""); err == nil {
			t.Errorf("took %s as a volume", uid)
		}
	}
	for _, bad := range []string{"../x", "a/b", "", "-x", "x@snap"} {
		if err := s.DeleteVolume(ctx, bad); err == nil {
			t.Errorf("deleted the volume of %q", bad)
		}
	}
	h.checkDecoys(t)
}

// TestCapacityIsTheDatasets: the storage's size and free space are its
// dataset's, as ZFS counts them.
func TestCapacityIsTheDatasets(t *testing.T) {
	h := newHost(t)
	c, err := h.storage(t).Capacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var used, avail int64
	if _, err := fmt.Sscan(mustRun(t, "zfs", "get", "-Hp", "-o", "value", "used,available", h.dataset), &used, &avail); err != nil {
		t.Fatal(err)
	}
	if c.Total != used+avail || c.Free != avail {
		t.Errorf("capacity %+v, ZFS says used %d available %d", c, used, avail)
	}
}

// TestAnUnfinishedBaseIsMadeAgain: a base whose making was cut short is
// cleared and made again, by the next attempt or the next instance.
func TestAnUnfinishedBaseIsMadeAgain(t *testing.T) {
	h := newHost(t)
	s := h.storage(t)
	ctx := context.Background()
	r := registrytest.New(t)
	ref, digest := r.Push(t, "app:1", smallImage(t, 1))
	images := h.images(t)
	// As a crash between making the volume and its snapshot leaves it.
	vol := h.dataset + "/bases/" + strings.TrimPrefix(digest, "sha256:")
	mustRun(t, "zfs", "create", "-s", "-V", "64M", "-o", "hangar:digest="+digest, vol)
	if bases, _ := s.Bases(ctx); len(bases) != 0 {
		t.Errorf("an unfinished base is listed: %v", bases)
	}
	h.storage(t) // a new instance clears it
	if out, err := tryRun("zfs", "list", vol); err == nil {
		t.Errorf("the unfinished base survived a new instance: %s", out)
	}
	mustRun(t, "zfs", "create", "-s", "-V", "64M", "-o", "hangar:digest="+digest, vol)
	if err := s.EnsureBase(ctx, digest, 0, func(dir string) error { return images.Unpack(ctx, ref, digest, dir, anon) }); err != nil {
		t.Fatal(err)
	}
	if bases, _ := s.Bases(ctx); len(bases) != 1 {
		t.Errorf("bases %v", bases)
	}
	// udev never read the filesystem mkfs wrote.
	mustRun(t, "udevadm", "settle")
	if props := mustRun(t, "udevadm", "info", "--query=property", "--name=/dev/zvol/"+vol); strings.Contains(props, "ID_FS_") {
		t.Errorf("udev probed the base:\n%s", props)
	}
	// A base is never written again; a root is a device only, and sparse.
	if got := strings.TrimSpace(mustRun(t, "zfs", "get", "-H", "-o", "value", "readonly,volmode", vol)); got != "on\ndev" {
		t.Errorf("the base's readonly, volmode: %q", got)
	}
	root, err := s.EnsureRoot(ctx, "m1", digest, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	rootVol := h.dataset + "/roots/m1"
	if got := strings.TrimSpace(mustRun(t, "zfs", "get", "-H", "-o", "value", "readonly,volmode,refreservation", rootVol)); got != "off\ndev\nnone" {
		t.Errorf("the root's readonly, volmode, refreservation: %q", got)
	}
	if err := s.DeleteRoot(ctx, root.MachineUID); err != nil {
		t.Fatal(err)
	}
	// A fill that fails leaves nothing behind.
	failed := errors.New("no")
	_, other := r.Push(t, "other:1", smallImage(t, 2))
	if err := s.EnsureBase(ctx, other, 0, func(string) error { return failed }); !errors.Is(err, failed) {
		t.Errorf("a failing fill: %v", err)
	}
	if out, err := tryRun("zfs", "list", h.dataset+"/bases/"+strings.TrimPrefix(other, "sha256:")); err == nil {
		t.Errorf("a failed fill left %s", out)
	}
	if entries, _ := os.ReadDir(h.scratch); len(entries) != 0 {
		t.Errorf("scratch left: %v", entries)
	}
	// A base mkfs cannot make leaves nothing behind either: here an xattr
	// larger than an ext4 block, which ZFS holds and ext4 does not.
	_, third := r.Push(t, "third:1", smallImage(t, 3))
	err = s.EnsureBase(ctx, third, 0, func(dir string) error {
		p := filepath.Join(dir, "f")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			return err
		}
		return unix.Setxattr(p, "user.big", bytes.Repeat([]byte("x"), 16<<10), 0)
	})
	if err == nil || !strings.Contains(err.Error(), "mkfs.ext4") {
		t.Errorf("a base mkfs cannot make: %v", err)
	}
	if out, err := tryRun("zfs", "list", h.dataset+"/bases/"+strings.TrimPrefix(third, "sha256:")); err == nil {
		t.Errorf("a failed mkfs left %s", out)
	}
	if err := s.DeleteBase(ctx, digest); err != nil {
		t.Fatal(err)
	}
}

// TestImagesThatDoNotFitAreNotUnpacked: an image whose layers alone take
// more than the room left where images are unpacked, here under a quota, is
// refused before anything is unpacked.
func TestImagesThatDoNotFitAreNotUnpacked(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	scratch := filepath.Join(filepath.Dir(h.scratch), "small")
	ds := "hangar/images/" + strings.ReplaceAll(strings.TrimPrefix(h.dataset, testParent+"/"), "/", "-") + "-small"
	mustRun(t, "zfs", "create", "-o", "quota=32M", "-o", "mountpoint="+scratch, ds)
	t.Cleanup(func() { _, _ = tryRun("zfs", "destroy", ds) })
	s, err := zfsstore.New(ctx, zfsstore.Config{Dataset: h.dataset, Scratch: scratch})
	if err != nil {
		t.Fatal(err)
	}
	r := registrytest.New(t)
	_, big := r.Push(t, "big:1", smallImage(t, 1))
	if err := s.EnsureBase(ctx, big, 64<<20, func(string) error {
		t.Error("an image larger than the quota was unpacked")
		return nil
	}); !errors.Is(err, driver.ErrNoSpace) {
		t.Errorf("64M of layers under a 32M quota: %v", err)
	}
	if err := s.EnsureBase(ctx, big, 1<<20, func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644)
	}); err != nil {
		t.Errorf("1M of layers under a 32M quota: %v", err)
	}
	if err := s.DeleteBase(ctx, big); err != nil {
		t.Fatal(err)
	}

	// Nor one that would not fit the pool, with room to unpack it.
	mustRun(t, "zfs", "set", "quota=256M", h.dataset)
	pooled := h.storage(t)
	if err := pooled.EnsureBase(ctx, big, 256<<20, func(string) error {
		t.Error("an image larger than the pool's room was unpacked")
		return nil
	}); !errors.Is(err, driver.ErrNoSpace) {
		t.Errorf("256M of layers in a pool of 256M: %v", err)
	}
}

// TestADestroyWaitsOutABriefOpen: a root disk something opens for a moment,
// as a probe of the device does, is deleted once it is let go.
func TestADestroyWaitsOutABriefOpen(t *testing.T) {
	h := newHost(t)
	images, s := h.images(t), h.storage(t)
	ctx := context.Background()
	r := registrytest.New(t)
	ref, digest := r.Push(t, "app:1", smallImage(t, 1))
	if err := s.EnsureBase(ctx, digest, 0, func(dir string) error { return images.Unpack(ctx, ref, digest, dir, anon) }); err != nil {
		t.Fatal(err)
	}
	root, err := s.EnsureRoot(ctx, "m1", digest, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := filepath.EvalSymlinks(root.Path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dev)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(500*time.Millisecond, func() { _ = f.Close() })
	start := time.Now()
	if err := s.DeleteRoot(ctx, "m1"); err != nil {
		t.Fatalf("deleting a root held open for 500ms: %v", err)
	}
	t.Logf("deleted after %v", time.Since(start))
	if err := s.DeleteBase(ctx, digest); err != nil {
		t.Fatal(err)
	}
}
