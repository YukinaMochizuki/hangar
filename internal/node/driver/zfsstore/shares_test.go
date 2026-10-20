package zfsstore

import (
	"slices"
	"strings"
	"testing"
)

// TestAShareVolume: a FileShare's volume is hangar/shares/<uid>, made
// sparse, as volmode=dev, with hangar:fileshare naming its share, at the
// size asked rounded up to a MiB; it is grown when asked bigger and never
// shrunk; and it is destroyed alone, never recursively.
func TestAShareVolume(t *testing.T) {
	f := newFakeZFS(t)
	ctx := t.Context()
	v, err := f.s.EnsureShareVolume(ctx, "s1", 2*gib-5)
	if err != nil {
		t.Fatal(err)
	}
	const ds = "hangar/shares/s1"
	if v.ShareUID != "s1" || v.Path != "/dev/zvol/"+ds || v.Size != 2*gib {
		t.Errorf("made %+v", v)
	}
	for prop, want := range map[string]string{"sparse": "yes", "volmode": "dev", propShare: "s1", "volsize": itoa(2 * gib)} {
		if got := f.prop(ds, prop); got != want {
			t.Errorf("%s is %q, want %q", prop, got, want)
		}
	}
	if v, err := f.s.EnsureShareVolume(ctx, "s1", 3*gib); err != nil || v.Size != 3*gib || f.prop(ds, "volsize") != itoa(3*gib) {
		t.Errorf("grown to %+v, volsize %s: %v", v, f.prop(ds, "volsize"), err)
	}
	if v, err := f.s.EnsureShareVolume(ctx, "s1", gib); err != nil || v.Size != 3*gib || f.prop(ds, "volsize") != itoa(3*gib) {
		t.Errorf("asked smaller: %+v, volsize %s: %v", v, f.prop(ds, "volsize"), err)
	}
	if got := f.calls("create"); len(got) != 1 {
		t.Errorf("made again: %q", got)
	}
	if err := f.s.DeleteShareVolume(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if got := f.calls("destroy"); !slices.Equal(got, []string{"destroy " + ds}) {
		t.Errorf("destroyed with %q", got)
	}
	if err := f.s.DeleteShareVolume(ctx, "s1"); err != nil {
		t.Errorf("deleting what is gone: %v", err)
	}
}

// TestAShareVolumeNotOursIsLeftAlone: a dataset where a share's volume
// would be that does not name that share, as another's or one made by
// hand, is neither taken, grown nor destroyed; a uid that cannot name a
// dataset is refused before zfs is asked anything.
func TestAShareVolumeNotOursIsLeftAlone(t *testing.T) {
	f := newFakeZFS(t)
	ctx := t.Context()
	f.dataset("hangar/shares/s1", "volsize", itoa(gib), propShare, "s2")
	f.dataset("hangar/shares/s3", "volsize", itoa(gib))
	for _, uid := range []string{"s1", "s3"} {
		if _, err := f.s.EnsureShareVolume(ctx, uid, 2*gib); err == nil || !strings.Contains(err.Error(), "left alone") {
			t.Errorf("ensuring %s: %v", uid, err)
		}
		if err := f.s.DeleteShareVolume(ctx, uid); err == nil {
			t.Errorf("deleted %s", uid)
		}
	}
	if got := f.calls("set"); len(got) != 0 {
		t.Errorf("changed: %q", got)
	}
	if got := f.calls("destroy"); len(got) != 0 {
		t.Errorf("destroyed: %q", got)
	}
	for _, uid := range []string{"", "../s1", "s1/x", "-r"} {
		if _, err := f.s.EnsureShareVolume(ctx, uid, gib); err == nil {
			t.Errorf("made a volume for %q", uid)
		}
		if err := f.s.DeleteShareVolume(ctx, uid); err == nil {
			t.Errorf("deleted a volume for %q", uid)
		}
	}
	if got := f.calls(""); len(got) != 4 {
		t.Errorf("asked zfs for the bad uids: %q", got)
	}
}
