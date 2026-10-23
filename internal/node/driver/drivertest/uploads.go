package drivertest

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// filledImages holds the storage driver to FillImageBase: a base keyed by
// the image, filled once, read-only, cloned as a captured one is, and none
// left by a fill that fails. With WriteVolume and ReadVolume it also checks
// what the base holds.
func filledImages(t *testing.T, h Host) {
	ctx := t.Context()
	st := h.Storage
	key := driver.CustomImageBase("up1")
	const at = 1 << 20
	bad := errors.New("the converter failed")
	if _, err := st.FillImageBase(ctx, "up1", rootSize, func(string) error { return bad }); !errors.Is(err, bad) {
		t.Errorf("a fill that fails: %v, want its error", err)
	}
	if bases, err := st.ImageBases(ctx); err != nil || len(bases) != 0 {
		t.Errorf("image bases after a failed fill: %v %v", bases, err)
	}
	fills := 0
	base, err := st.FillImageBase(ctx, "up1", rootSize, func(dev string) error {
		fills++
		if h.WriteVolume != nil {
			h.WriteVolume(t, dev, at, []byte("uploaded"))
		}
		return nil
	})
	must(t, err)
	if base.Key != key || base.Size < rootSize || base.Used < 0 || base.Used > base.Size {
		t.Errorf("base %+v, want key %s and size %d", base, key, int64(rootSize))
	}
	if again, err := st.FillImageBase(ctx, "up1", rootSize, func(string) error { fills++; return nil }); err != nil || again != base || fills != 1 {
		t.Errorf("filling again: %+v %v, filled %d times; want the base made once", again, err, fills)
	}
	if bases, err := st.ImageBases(ctx); err != nil || !slices.Equal(bases, []string{key}) {
		t.Errorf("image bases %v %v", bases, err)
	}
	v, err := st.EnsureVolume(ctx, "from-upload", 2*rootSize, key)
	must(t, err)
	if v.Digest != key {
		t.Errorf("volume %+v, want it of %s", v, key)
	}
	if h.ReadVolume != nil {
		if got := string(h.ReadVolume(t, v.Path, at, len("uploaded"))); got != "uploaded" {
			t.Errorf("a volume of the filled base holds %q", got)
		}
	}
	must(t, st.DeleteVolume(ctx, "from-upload"))
	must(t, st.DeleteImageBase(ctx, key))
}

// converter holds the converter to its contract on a raw file: what it
// is, and the file written onto a base's device whole. With ReadVolume it
// checks what the base holds.
func converter(t *testing.T, h Host) {
	ctx := t.Context()
	data := bytes.Repeat([]byte("hangar uploads\n"), 1<<16)
	data = data[:len(data)/512*512]
	path := filepath.Join(t.TempDir(), "image.raw")
	must(t, os.WriteFile(path, data, 0o600))
	info, err := h.Converter.Inspect(ctx, "conv1", path)
	must(t, err)
	if info.Format != "raw" || info.VirtualSize != int64(len(data)) || info.BackingFile != "" || info.DataFile != "" {
		t.Errorf("info %+v, want raw of %d bytes naming no other file", info, len(data))
	}
	base, err := h.Storage.FillImageBase(ctx, "conv1", info.VirtualSize, func(dev string) error {
		return h.Converter.Convert(ctx, "conv1", path, info.Format, dev)
	})
	must(t, err)
	if h.ReadVolume != nil {
		v, err := h.Storage.EnsureVolume(ctx, "converted", base.Size, base.Key)
		must(t, err)
		if got := h.ReadVolume(t, v.Path, 0, len(data)); !bytes.Equal(got, data) {
			t.Error("the converted base does not hold the file")
		}
		must(t, h.Storage.DeleteVolume(ctx, "converted"))
	}
	must(t, h.Storage.DeleteImageBase(ctx, base.Key))
	if _, err := h.Converter.Inspect(ctx, "conv2", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("inspected a file that is not there")
	}
}
