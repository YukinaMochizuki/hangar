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
		t.Errorf("base %+v, want key %s and size %d", base, key, rootSize)
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

// stage writes data as the node stages an upload (driver.UploadOffset,
// driver.UploadedLength), and returns the file.
func stage(t *testing.T, data []byte) driver.UploadedFile {
	t.Helper()
	path := filepath.Join(t.TempDir(), "upload")
	staged := make([]byte, driver.UploadedLength(int64(len(data))))
	copy(staged[driver.UploadOffset:], data)
	must(t, os.WriteFile(path, staged, 0o600))
	return driver.UploadedFile{Path: path, Offset: driver.UploadOffset, Size: int64(len(data))}
}

// converter holds the converter to its contract on raw files: what they
// are, and a file written onto a base's device whole, the bytes staged
// with it never in it. A raw file whose last sector is a VHD footer, or
// whose first bytes are VHDX's signature, is taken as any other: the
// hypervisor, which reads a disk's ends for a format's header, sees only
// the staging's zeros there; what qemu-img in the converter makes of it is
// its own. With ReadVolume it checks what the base holds.
func converter(t *testing.T, h Host) {
	ctx := t.Context()
	convert := func(uid string, data []byte) {
		t.Helper()
		file := stage(t, data)
		info, err := h.Converter.Inspect(ctx, uid, file)
		must(t, err)
		want := (int64(len(data)) + 511) &^ 511
		if info.Format != "raw" || info.VirtualSize != want || info.BackingFile != "" || info.DataFile != "" {
			t.Errorf("%s: info %+v, want raw of %d bytes naming no other file", uid, info, want)
		}
		base, err := h.Storage.FillImageBase(ctx, uid, info.VirtualSize, func(dev string) error {
			return h.Converter.Convert(ctx, uid, file, info.Format, dev)
		})
		must(t, err)
		if h.ReadVolume != nil {
			v, err := h.Storage.EnsureVolume(ctx, "converted-"+uid, base.Size, base.Key)
			must(t, err)
			got := h.ReadVolume(t, v.Path, 0, len(data)+512)
			if !bytes.Equal(got[:len(data)], data) || !bytes.Equal(got[len(data):], make([]byte, 512)) {
				t.Errorf("%s: the converted base does not hold the file alone", uid)
			}
			must(t, h.Storage.DeleteVolume(ctx, "converted-"+uid))
		}
		must(t, h.Storage.DeleteImageBase(ctx, base.Key))
	}
	data := bytes.Repeat([]byte("hangar uploads\n"), 1<<16)
	convert("conv1", data[:len(data)/512*512])
	convert("conv2", data[:1000])
	footer := append(bytes.Repeat([]byte{7}, 4096), []byte("conectix")...)
	footer = append(footer, make([]byte, 504)...)
	convert("vhdfooter", footer)

	// Whatever the converter makes of VHDX's signature, it is no failure
	// to boot or attach the file.
	vhdx := append([]byte("vhdxfile"), bytes.Repeat([]byte{9}, 64<<10)...)
	if info, err := h.Converter.Inspect(ctx, "vhdx", stage(t, vhdx)); err != nil && !errors.Is(err, driver.ErrInvalidImage) {
		t.Errorf("a raw file starting with VHDX's signature: %+v %v, want it inspected", info, err)
	}

	short := stage(t, data[:4096])
	short.Size = 1 << 20
	if _, err := h.Converter.Inspect(ctx, "conv3", short); err == nil {
		t.Error("inspected a file shorter than it says")
	}
	if _, err := h.Converter.Inspect(ctx, "conv4", driver.UploadedFile{Path: filepath.Join(t.TempDir(), "missing"), Offset: driver.UploadOffset, Size: 1}); err == nil {
		t.Error("inspected a file that is not there")
	}
}
