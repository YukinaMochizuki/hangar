package zfsstore

import (
	"strconv"
	"strings"
	"testing"
)

const (
	testDigest   = "sha256:6a81c37564db9b1ee84e141922625e1d7c5b389b99bb3c572e0243607d5bb4d2"
	testHex      = "6a81c37564db9b1ee84e141922625e1d7c5b389b99bb3c572e0243607d5bb4d2"
	testBaseSize = 3584 << 20
	gib          = 1 << 30
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestNewMakesItsDatasetsVolmodeDev: the driver's parent datasets carry
// volmode=dev, made with it or given it, so each zvol's device is made as
// dev and never made again when its own volmode is set; one that has it is
// left as it is.
func TestNewMakesItsDatasetsVolmodeDev(t *testing.T) {
	f := newFakeZFS(t)
	f.dataset("hangar", "volmode", "default")
	f.dataset("hangar/disks", "volmode", "default")
	f.dataset("hangar/roots", "volmode", "dev")
	if _, err := New(t.Context(), Config{Dataset: "hangar", Scratch: t.TempDir(), Log: f.s.cfg.Log}); err != nil {
		t.Fatal(err)
	}
	for _, ds := range []string{"hangar/bases", "hangar/roots", "hangar/disks", "hangar/vmbases"} {
		if got := f.prop(ds, "volmode"); got != "dev" {
			t.Errorf("%s has volmode %q", ds, got)
		}
	}
	if got := f.calls("create"); len(got) != 2 || !strings.Contains(got[0], "-o volmode=dev hangar/bases") ||
		!strings.Contains(got[1], "-o volmode=dev hangar/vmbases") {
		t.Errorf("made %q", got)
	}
	if got := f.calls("set"); len(got) != 1 || got[0] != "set volmode=dev hangar/disks" {
		t.Errorf("set %q", got)
	}
	if got := f.prop("hangar", "volmode"); got != "default" {
		t.Errorf("the parent dataset was changed: volmode %q", got)
	}
}

// TestACloneIsGrownAfter: a volume or root disk asked bigger than its base
// is cloned at the base's size, which no property set in the clone's ioctl
// changes, then grown.
func TestACloneIsGrownAfter(t *testing.T) {
	t.Run("volume", func(t *testing.T) {
		f := newFakeZFS(t)
		f.dataset("hangar/vmbases/"+testHex+"@base", propDigest, testDigest, "volsize", itoa(testBaseSize))
		v, err := f.s.EnsureVolume(t.Context(), "d1", 10*gib, testDigest)
		if err != nil {
			t.Fatal(err)
		}
		f.checkClone("hangar/disks/d1", v.Size, 10*gib, true)
		if got := f.prop("hangar/disks/d1", propDisk); got != "d1" {
			t.Errorf("%s is %q", propDisk, got)
		}
	})
	t.Run("root", func(t *testing.T) {
		f := newFakeZFS(t)
		f.dataset("hangar/bases/"+testHex+"@base", propDigest, testDigest, "volsize", itoa(testBaseSize))
		d, err := f.s.EnsureRoot(t.Context(), "m1", testDigest, 10*gib)
		if err != nil {
			t.Fatal(err)
		}
		f.checkClone("hangar/roots/m1", d.Size, 10*gib, true)
		if got := f.prop("hangar/roots/m1", propMachine); got != "m1" {
			t.Errorf("%s is %q", propMachine, got)
		}
	})
}

// TestACloneTheSizeOfItsBaseIsNotGrown: a clone asked at its base's size is
// not grown, and one asked smaller is refused before any clone.
func TestACloneTheSizeOfItsBaseIsNotGrown(t *testing.T) {
	f := newFakeZFS(t)
	f.dataset("hangar/vmbases/"+testHex+"@base", propDigest, testDigest, "volsize", itoa(testBaseSize))
	v, err := f.s.EnsureVolume(t.Context(), "d1", testBaseSize, testDigest)
	if err != nil {
		t.Fatal(err)
	}
	f.checkClone("hangar/disks/d1", v.Size, testBaseSize, false)
	if _, err := f.s.EnsureVolume(t.Context(), "d2", gib, testDigest); err == nil {
		t.Error("a volume smaller than its base was made")
	}
	if c := f.calls("clone"); len(c) != 1 {
		t.Errorf("clones: %q", c)
	}
}

// checkClone checks vol was made by one clone setting no volsize, grown
// after by one zfs set if grown, and is want bytes.
func (f *fakeZFS) checkClone(vol string, size, want int64, grown bool) {
	f.t.Helper()
	clones := f.calls("clone")
	if len(clones) != 1 || strings.Contains(clones[0], "volsize=") {
		f.t.Errorf("clones: %q", clones)
	}
	var sets []string
	for _, s := range f.calls("set") {
		if strings.HasSuffix(s, " "+vol) {
			sets = append(sets, s)
		}
	}
	switch {
	case grown && (len(sets) != 1 || sets[0] != "set volsize="+itoa(want)+" "+vol):
		f.t.Errorf("grown by %q", sets)
	case !grown && len(sets) != 0:
		f.t.Errorf("set %q", sets)
	}
	if size != want || f.prop(vol, "volsize") != itoa(want) {
		f.t.Errorf("%s is %d bytes, %s on the pool; want %d", vol, size, f.prop(vol, "volsize"), want)
	}
}

// TestAGrowKilledOnceItsChangeIsMadeIsDone: a zfs set volsize killed after
// its change is made, as the kernel's oops in zvol_os_update_volsize does,
// grows the volume all the same: for a clone just made, an existing volume
// and a root disk. One that fails before it changes anything fails.
func TestAGrowKilledOnceItsChangeIsMadeIsDone(t *testing.T) {
	t.Run("clone", func(t *testing.T) {
		f := newFakeZFS(t)
		f.dataset("hangar/vmbases/"+testHex+"@base", propDigest, testDigest, "volsize", itoa(testBaseSize))
		f.touch("set-killed")
		v, err := f.s.EnsureVolume(t.Context(), "d1", 10*gib, testDigest)
		if err != nil {
			t.Fatalf("a grow whose change was made failed: %v", err)
		}
		f.checkClone("hangar/disks/d1", v.Size, 10*gib, true)
	})
	t.Run("volume", func(t *testing.T) {
		f := newFakeZFS(t)
		f.dataset("hangar/disks/d1", propDisk, "d1", "volsize", itoa(gib))
		f.touch("set-killed")
		v, err := f.s.EnsureVolume(t.Context(), "d1", 2*gib, "")
		if err != nil || v.Size != 2*gib {
			t.Fatalf("a grow whose change was made: %d bytes, %v", v.Size, err)
		}
	})
	t.Run("root", func(t *testing.T) {
		f := newFakeZFS(t)
		f.dataset("hangar/roots/m1", propMachine, "m1", propDigest, testDigest, "volsize", itoa(4*gib))
		f.touch("set-killed")
		d, err := f.s.EnsureRoot(t.Context(), "m1", testDigest, 10*gib)
		if err != nil || d.Size != 10*gib {
			t.Fatalf("a grow whose change was made: %d bytes, %v", d.Size, err)
		}
	})
	t.Run("failed before its change", func(t *testing.T) {
		f := newFakeZFS(t)
		f.dataset("hangar/disks/d1", propDisk, "d1", "volsize", itoa(gib))
		f.touch("set-fails")
		if _, err := f.s.EnsureVolume(t.Context(), "d1", 2*gib, ""); err == nil || !strings.Contains(err.Error(), "out of luck") {
			t.Errorf("a grow that changed nothing: %v, want zfs's error", err)
		}
		if got := f.prop("hangar/disks/d1", "volsize"); got != itoa(gib) {
			t.Errorf("volsize %s", got)
		}
	})
}

// TestNeverShrinksNorTouchesAnothersVolume: a volume bigger than asked is
// left as it is, and one that is not the Disk's is never grown.
func TestNeverShrinksNorTouchesAnothersVolume(t *testing.T) {
	f := newFakeZFS(t)
	f.dataset("hangar/disks/d1", propDisk, "d1", "volsize", itoa(4*gib))
	f.dataset("hangar/disks/d2", propDisk, "other", "volsize", itoa(gib))
	v, err := f.s.EnsureVolume(t.Context(), "d1", gib, "")
	if err != nil || v.Size != 4*gib {
		t.Errorf("a bigger volume: %d bytes, %v", v.Size, err)
	}
	if _, err := f.s.EnsureVolume(t.Context(), "d2", 2*gib, ""); err == nil {
		t.Error("another's volume was taken")
	}
	if c := append(f.calls("set"), f.calls("clone")...); len(c) != 0 {
		t.Errorf("changed: %q", c)
	}
}
