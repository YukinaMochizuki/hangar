package drivertest

import (
	"errors"
	"slices"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// customImages holds the storage driver to the CustomImage part of its
// contract: a volume's snapshot copied whole into a base keyed by the
// image, which outlives the volume, and volumes cloned from that base.
// With WriteVolume and ReadVolume it also checks what the base holds.
func customImages(t *testing.T, h Host) {
	ctx := t.Context()
	st := h.Storage
	key := driver.CustomImageBase("img1")
	const at = 1 << 20
	write := func(v driver.Volume, data string) {
		if h.WriteVolume != nil {
			h.WriteVolume(t, v.Path, at, []byte(data))
		}
	}
	holds := func(v driver.Volume, want string) {
		t.Helper()
		if h.ReadVolume == nil {
			return
		}
		if got := string(h.ReadVolume(t, v.Path, at, len(want))); got != want {
			t.Errorf("volume of disk %s holds %q, want %q", v.DiskUID, got, want)
		}
	}

	if err := st.SnapshotVolume(ctx, "nope", "img0"); err == nil {
		t.Error("snapshotted a volume that does not exist")
	}
	src, err := st.EnsureVolume(ctx, "src", rootSize, "")
	must(t, err)
	write(src, "golden")
	must(t, st.SnapshotVolume(ctx, "src", "img1"))
	must(t, st.SnapshotVolume(ctx, "src", "img1"))
	if captures, err := st.Captures(ctx); err != nil || !slices.Equal(captures, []driver.Capture{{ImageUID: "img1", DiskUID: "src"}}) {
		t.Errorf("captures %v %v", captures, err)
	}
	if err := st.DeleteVolume(ctx, "src"); err == nil {
		t.Error("deleted a volume with a snapshot for a custom image")
	}
	// What the volume holds after the snapshot is not the image's.
	write(src, "afters")

	base, err := st.CaptureImageBase(ctx, "img1")
	must(t, err)
	if base.Key != key || base.Size != src.Size || base.Used <= 0 || base.Used > base.Size {
		t.Errorf("base %+v, want key %s and size %d, holding some of it", base, key, src.Size)
	}
	if again, err := st.CaptureImageBase(ctx, "img1"); err != nil || again != base {
		t.Errorf("capturing again: %+v %v, want %+v", again, err, base)
	}
	if captures, err := st.Captures(ctx); err != nil || len(captures) != 0 {
		t.Errorf("captures once the base is made: %v %v", captures, err)
	}
	if _, err := st.CaptureImageBase(ctx, "img0"); !errors.Is(err, driver.ErrNoCapture) {
		t.Errorf("capturing an image with no snapshot: %v, want ErrNoCapture", err)
	}
	if bases, err := st.ImageBases(ctx); err != nil || !slices.Equal(bases, []string{key}) {
		t.Errorf("image bases %v %v", bases, err)
	}
	if bases, err := st.Bases(ctx); err != nil || len(bases) != 0 {
		t.Errorf("bases %v %v", bases, err)
	}

	// The base is a copy: its source goes, and it stays.
	must(t, st.DeleteVolume(ctx, "src"))
	if _, err := st.EnsureVolume(ctx, "copy", src.Size/2, key); !errors.Is(err, driver.ErrDiskTooSmall) {
		t.Errorf("a volume smaller than its custom image: %v, want too small", err)
	}
	cp, err := st.EnsureVolume(ctx, "copy", 2*rootSize, key)
	must(t, err)
	if cp.Digest != key || cp.Size < 2*rootSize {
		t.Errorf("volume %+v", cp)
	}
	holds(cp, "golden")
	if listed, ok := volumeOf(t, st, "copy"); !ok || listed != cp {
		t.Errorf("listed %+v %t, made %+v", listed, ok, cp)
	}
	if _, err := st.EnsureVolume(ctx, "copy", 2*rootSize, ""); err == nil {
		t.Error("a volume of a custom image was taken for a blank one")
	}
	if err := st.DeleteImageBase(ctx, key); err == nil {
		t.Error("deleted a custom image's base a volume was cloned from")
	}
	if h.Reopen != nil {
		_, reopened := h.Reopen(t)
		if bases, err := reopened.ImageBases(ctx); err != nil || !slices.Equal(bases, []string{key}) {
			t.Errorf("image bases after reopening: %v %v", bases, err)
		}
		if listed, ok := volumeOf(t, reopened, "copy"); !ok || listed != cp {
			t.Errorf("after reopening, listed %+v %t, made %+v", listed, ok, cp)
		}
	}
	must(t, st.DeleteVolume(ctx, "copy"))
	must(t, st.DeleteImageBase(ctx, key))
	must(t, st.DeleteImageBase(ctx, key))

	// A snapshot not wanted any more goes, and nothing is captured from it.
	other, err := st.EnsureVolume(ctx, "other", rootSize, "")
	must(t, err)
	must(t, st.SnapshotVolume(ctx, other.DiskUID, "img2"))
	must(t, st.DeleteCapture(ctx, "img2"))
	must(t, st.DeleteCapture(ctx, "img2"))
	if _, err := st.CaptureImageBase(ctx, "img2"); !errors.Is(err, driver.ErrNoCapture) {
		t.Errorf("capturing an image whose snapshot is gone: %v, want ErrNoCapture", err)
	}
	must(t, st.DeleteVolume(ctx, "other"))
	if bases, err := st.ImageBases(ctx); err != nil || len(bases) != 0 {
		t.Errorf("image bases left: %v %v", bases, err)
	}
	if captures, err := st.Captures(ctx); err != nil || len(captures) != 0 {
		t.Errorf("captures left: %v %v", captures, err)
	}
}
