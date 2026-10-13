//go:build testbed

package vms

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/diskimgtest"
)

// A real cloud image, small: CirrOS, as its project publishes it, with the
// sha256 its SHA256SUMS gives.
const (
	cirrosName   = "cirros-0.6.3-x86_64-disk.img"
	cirrosURL    = "https://download.cirros-cloud.net/0.6.3/" + cirrosName
	cirrosSHA256 = "7d6355852aeb6dbcd191bcda7cd74f1536cfe5cbf8a10495a7283a8396e4b75b"
)

// TestAVolumeOfACloudImage: a distribution's qcow2, fetched and checked by
// the disk image driver, becomes a base holding what qemu-img makes of it
// raw, and a volume cloned from it, grown, which a guest sees as a disk
// with its partitions.
func TestAVolumeOfACloudImage(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	file := diskimgtest.Cached(t, cirrosName, cirrosURL, cirrosSHA256)
	srv := diskimgtest.New(t)
	url, digest := srv.Serve(t, cirrosName, file)
	images := srv.Driver(t, filepath.Join(filepath.Dir(h.cache), "disk-images"))

	img, err := images.Fetch(ctx, url, digest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = img.Close() }()
	raw := filepath.Join(t.TempDir(), "raw")
	diskimgtest.Run(t, "qemu-img", "convert", "-q", "-f", "qcow2", "-O", "raw", file, raw)
	want := diskimgtest.SumOf(t, raw, img.Size())
	if err := h.storage.EnsureImageBase(ctx, digest, img.Size(), func(dev string) error { return img.WriteTo(ctx, dev) }); err != nil {
		t.Fatal(err)
	}
	v, err := h.storage.EnsureVolume(ctx, "cirrosdisk", 1<<30, digest)
	if err != nil {
		t.Fatal(err)
	}
	if got := diskimgtest.SumOf(t, v.Path, img.Size()); got != want {
		t.Errorf("the volume starts with sha256 %s, the image converted raw is %s", got, want)
	}
	t.Logf("%s: %d bytes of disk, volume %s of %d bytes", cirrosName, img.Size(), v.Path, v.Size)

	spec := h.machine(t, "cirros", driver.RestartNever, guestScript)
	spec.Disks = []driver.Volume{v}
	startAndWait(t, h, spec)
	out, code, err := execIn(ctx, "cirros", "sh", "-c", "cat /sys/block/vdc/size; ls /sys/block/vdc")
	if err != nil || code != 0 || !strings.HasPrefix(out, "2097152\n") || !strings.Contains(out, "vdc1") {
		t.Errorf("the guest's vdc: %q, exit %d, %v", out, code, err)
	}
	if err := h.hv.Stop(ctx, "cirros"); err != nil {
		t.Fatal(err)
	}
	if err := h.storage.DeleteImageBase(ctx, digest); err == nil {
		t.Error("deleted the base of a volume")
	}
	if err := h.storage.DeleteVolume(ctx, "cirrosdisk"); err != nil {
		t.Fatal(err)
	}
	if err := h.storage.DeleteImageBase(ctx, digest); err != nil {
		t.Fatal(err)
	}
}
