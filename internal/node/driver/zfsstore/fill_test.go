package zfsstore

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// fillHost is the fake with a pool of free bytes.
func fillHost(t *testing.T, free int64) *fakeZFS {
	t.Helper()
	f := newFakeZFS(t)
	f.dataset("hangar", "used", "0", "available", itoa(free))
	return f
}

// noFilledBase checks a fill that failed left no base of img1.
func (f *fakeZFS) noFilledBase(t *testing.T) {
	t.Helper()
	if f.exists(imageVol) || f.exists(imageSnap) {
		t.Errorf("a failed fill left its base: volume %t, snapshot %t", f.exists(imageVol), f.exists(imageSnap))
	}
	if bases, err := f.s.ImageBases(t.Context()); err != nil || len(bases) != 0 {
		t.Errorf("image bases %v %v", bases, err)
	}
}

// TestAFilledBaseIsMarkedAsACapturedOne: an uploaded image's base is a
// sparse volume of the size asked, rounded up to the alignment, filled,
// snapshotted, set read-only and marked finished on @base itself, as a
// captured one; a second call finds it made and fills nothing.
func TestAFilledBaseIsMarkedAsACapturedOne(t *testing.T) {
	f := fillHost(t, 100*gib)
	var filled []string
	fill := func(dev string) error {
		filled = append(filled, dev)
		return nil
	}
	if _, err := f.s.FillImageBase(t.Context(), "img1", 3*gib+1, fill); err != nil {
		t.Fatal(err)
	}
	if len(filled) != 1 || filled[0] == "" {
		t.Errorf("filled %q, want the volume's device once", filled)
	}
	var steps []string
	for _, c := range f.calls("") {
		for _, p := range []string{"create", "snapshot", "set", "destroy"} {
			if strings.HasPrefix(c, p+" ") {
				steps = append(steps, c)
			}
		}
	}
	want := []string{
		"create -s -V " + itoa(3*gib+align) + " -o volmode=dev -o hangar:customimage=img1 " + imageVol,
		"snapshot " + imageSnap,
		"set readonly=on " + imageVol,
		"set hangar:customimage=img1 " + imageSnap,
	}
	if !slices.Equal(steps, want) {
		t.Errorf("zfs ran\n%s\nwant\n%s", strings.Join(steps, "\n"), strings.Join(want, "\n"))
	}
	if bases, err := f.s.ImageBases(t.Context()); err != nil || !slices.Equal(bases, []string{driver.CustomImageBase("img1")}) {
		t.Errorf("image bases %v %v", bases, err)
	}
	if _, err := f.s.FillImageBase(t.Context(), "img1", 3*gib, fill); err != nil || len(filled) != 1 {
		t.Errorf("filling again: %v, filled %d times", err, len(filled))
	}
}

// TestAFailedFillLeavesNoBase: a fill that fails, or a pool without room
// for the whole volume, leaves no base; an unfinished one is made again,
// and a volume not the driver's is left alone.
func TestAFailedFillLeavesNoBase(t *testing.T) {
	ok := func(string) error { return nil }
	t.Run("fill fails", func(t *testing.T) {
		f := fillHost(t, 100*gib)
		bad := errors.New("qemu-img convert: exit status 1")
		if _, err := f.s.FillImageBase(t.Context(), "img1", gib, func(string) error { return bad }); !errors.Is(err, bad) {
			t.Errorf("a failed fill: %v, want its error", err)
		}
		f.noFilledBase(t)
	})
	t.Run("no room", func(t *testing.T) {
		f := fillHost(t, gib)
		_, err := f.s.FillImageBase(t.Context(), "img1", 2*gib, ok)
		var ns *driver.NoSpaceError
		if !errors.As(err, &ns) || ns.Need != 2*gib {
			t.Errorf("filling 2GiB into 1GiB free: %v, want a NoSpaceError", err)
		}
		if got := f.calls("create"); len(got) != 0 {
			t.Errorf("created %q", got)
		}
	})
	t.Run("unfinished", func(t *testing.T) {
		f := fillHost(t, 100*gib)
		f.dataset(imageVol, propCustomImage, "img1", "volsize", itoa(gib))
		if _, err := f.s.FillImageBase(t.Context(), "img1", gib, ok); err != nil {
			t.Fatal(err)
		}
		if d := f.calls("destroy"); len(d) == 0 || d[len(d)-1] != "destroy "+imageVol {
			t.Errorf("destroyed %q; want the unfinished volume", d)
		}
	})
	t.Run("not its own", func(t *testing.T) {
		f := fillHost(t, 100*gib)
		f.dataset(imageVol, propCustomImage, "other", "volsize", itoa(gib))
		if _, err := f.s.FillImageBase(t.Context(), "img1", gib, ok); err == nil {
			t.Error("filled over a volume not the image's")
		}
		if !f.exists(imageVol) || len(f.calls("create")) != 0 {
			t.Errorf("the volume there: %t; creates %q", f.exists(imageVol), f.calls("create"))
		}
	})
}
