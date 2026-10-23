package zfsstore

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

const (
	captureVol  = "hangar/disks/d1"
	captureSnap = captureVol + "@customimage-img1"
	imageVol    = "hangar/vmbases/customimage-img1"
	imageSnap   = imageVol + "@base"
)

// captureHost is the fake with a pool of free bytes and disk d1's volume,
// of 10 GiB holding 3 GiB, 2 GiB of it written, snapshotted for the
// custom image img1.
func captureHost(t *testing.T, free int64) *fakeZFS {
	t.Helper()
	f := newFakeZFS(t)
	f.dataset("hangar", "used", "0", "available", itoa(free))
	f.dataset(captureVol, propDisk, "d1", "volsize", itoa(10*gib), "referenced", itoa(2*gib), "logicalreferenced", itoa(3*gib))
	if err := f.s.SnapshotVolume(t.Context(), "d1", "img1"); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakeZFS) exists(ds string) bool {
	_, err := os.Stat(f.path(ds))
	return err == nil
}

// left checks what a capture that failed leaves: no base, and the
// snapshot it is captured from, for the agent to decide on.
func (f *fakeZFS) left(t *testing.T) {
	t.Helper()
	if f.exists(imageVol) || f.exists(imageSnap) {
		t.Errorf("a failed capture left its base: volume %t, snapshot %t", f.exists(imageVol), f.exists(imageSnap))
	}
	if captures, err := f.s.Captures(t.Context()); err != nil || !slices.Equal(captures, []driver.Capture{{ImageUID: "img1", DiskUID: "d1"}}) {
		t.Errorf("captures %v %v; want the snapshot kept", captures, err)
	}
	if bases, err := f.s.ImageBases(t.Context()); err != nil || len(bases) != 0 {
		t.Errorf("image bases %v %v", bases, err)
	}
}

// TestACaptureIsSentReceivedAndFinished: the snapshot is sent whole into
// the base, received as its @base with the volume's property, set
// read-only, marked finished by the property set on @base itself, and the
// source's snapshot destroyed; a second call finds the base made.
func TestACaptureIsSentReceivedAndFinished(t *testing.T) {
	f := captureHost(t, 100*gib)
	base, err := f.s.CaptureImageBase(t.Context(), "img1")
	if err != nil {
		t.Fatal(err)
	}
	if want := (driver.ImageBase{Key: driver.CustomImageBase("img1"), Size: 10 * gib, Used: 3 * gib}); base != want {
		t.Errorf("base %+v, want %+v", base, want)
	}
	var steps []string
	for _, c := range f.calls("") {
		for _, p := range []string{"snapshot", "send", "receive", "set", "destroy"} {
			if strings.HasPrefix(c, p+" ") {
				steps = append(steps, c)
			}
		}
	}
	want := []string{
		"snapshot -o hangar:customimage=img1 " + captureSnap,
		"receive -o hangar:customimage=img1 -o refreservation=none " + imageSnap,
		"send " + captureSnap,
		"set readonly=on " + imageVol,
		"set hangar:customimage=img1 " + imageSnap,
		"destroy " + captureSnap,
	}
	// The receive starts first, but the two run at once.
	sorted := func(s []string) []string {
		s = slices.Clone(s)
		if len(s) > 2 {
			slices.Sort(s[1:3])
		}
		return s
	}
	if !slices.Equal(sorted(steps), sorted(want)) {
		t.Errorf("zfs ran\n%s\nwant\n%s", strings.Join(steps, "\n"), strings.Join(want, "\n"))
	}
	if got := f.prop(imageVol, "readonly"); got != "on" {
		t.Errorf("the base's readonly is %q", got)
	}
	if bases, err := f.s.ImageBases(t.Context()); err != nil || !slices.Equal(bases, []string{driver.CustomImageBase("img1")}) {
		t.Errorf("image bases %v %v", bases, err)
	}
	before := len(f.calls("receive"))
	if again, err := f.s.CaptureImageBase(t.Context(), "img1"); err != nil || again != base {
		t.Errorf("capturing again: %+v %v", again, err)
	}
	if n := len(f.calls("receive")); n != before {
		t.Errorf("a base made was received again")
	}
}

// TestABaseOnlyInheritingItsMarkIsUnfinished: a received @base inherits
// its volume's property; until it is set on @base itself the base is not
// listed, cloned from, or taken as made, and a capture makes it again.
func TestABaseOnlyInheritingItsMarkIsUnfinished(t *testing.T) {
	f := captureHost(t, 100*gib)
	f.dataset(imageVol, propCustomImage, "img1", "volsize", itoa(10*gib))
	f.dataset(imageSnap, "volsize", itoa(10*gib), "logicalreferenced", itoa(gib))
	if got, _, _ := props(t.Context(), imageSnap, propCustomImage); got[propCustomImage] != "img1" {
		t.Fatalf("the fake does not inherit: %v", got)
	}
	if bases, err := f.s.ImageBases(t.Context()); err != nil || len(bases) != 0 {
		t.Errorf("an unfinished base listed: %v %v", bases, err)
	}
	if _, err := f.s.EnsureVolume(t.Context(), "d2", 20*gib, driver.CustomImageBase("img1")); err == nil {
		t.Error("a volume cloned from an unfinished base")
	}
	base, err := f.s.CaptureImageBase(t.Context(), "img1")
	if err != nil {
		t.Fatal(err)
	}
	if base.Used != 3*gib {
		t.Errorf("base %+v: not the one captured again", base)
	}
	destroys := f.calls("destroy")
	if len(destroys) < 2 || destroys[0] != "destroy "+imageSnap || destroys[1] != "destroy "+imageVol {
		t.Errorf("destroyed %q; want the unfinished base, its snapshot first", destroys)
	}
	if got := f.prop(imageSnap, propCustomImage); got != "img1" {
		t.Errorf("the base's own mark is %q", got)
	}
}

// TestACaptureLeavesWhatIsNotItsOwn: a volume at the base's name claimed
// by another image, or by none, is not made over.
func TestACaptureLeavesWhatIsNotItsOwn(t *testing.T) {
	for name, props := range map[string][]string{
		"claimed by another": {propCustomImage, "other", "volsize", itoa(gib)},
		"claimed by none":    {"volsize", itoa(gib)},
	} {
		t.Run(name, func(t *testing.T) {
			f := captureHost(t, 100*gib)
			f.dataset(imageVol, props...)
			if _, err := f.s.CaptureImageBase(t.Context(), "img1"); err == nil {
				t.Error("captured over a volume not the driver's")
			}
			if !f.exists(imageVol) || len(f.calls("receive")) != 0 {
				t.Errorf("the volume there: %t; receives %q", f.exists(imageVol), f.calls("receive"))
			}
		})
	}
}

// TestACaptureNeedsRoom: a snapshot referencing more than the pool has
// free is refused before anything is sent.
func TestACaptureNeedsRoom(t *testing.T) {
	f := captureHost(t, gib)
	_, err := f.s.CaptureImageBase(t.Context(), "img1")
	var ns *driver.NoSpaceError
	if !errors.As(err, &ns) || ns.Need != 2*gib || ns.Free != gib {
		t.Errorf("capturing 2GiB into 1GiB free: %v, want a NoSpaceError", err)
	}
	if got := f.calls("send"); len(got) != 0 {
		t.Errorf("sent %q", got)
	}
	f.left(t)
}

// TestAFailedReceiveLeavesNoBase: a receive that fails once it has made
// the volume, or out of space, leaves no base; the latter is ErrNoSpace.
func TestAFailedReceiveLeavesNoBase(t *testing.T) {
	for _, tt := range []struct {
		switchName string
		noSpace    bool
	}{{"recv-late", false}, {"recv-nospace", true}} {
		t.Run(tt.switchName, func(t *testing.T) {
			f := captureHost(t, 100*gib)
			f.touch(tt.switchName)
			_, err := f.s.CaptureImageBase(t.Context(), "img1")
			if err == nil {
				t.Fatal("a failed receive captured")
			}
			if errors.Is(err, driver.ErrNoSpace) != tt.noSpace {
				t.Errorf("%v: ErrNoSpace %t, want %t", err, !tt.noSpace, tt.noSpace)
			}
			f.left(t)
		})
	}
}

// TestASendThatFailsIsTheCause: a send that fails makes the receive fail
// on the short stream; the error is the send's.
func TestASendThatFailsIsTheCause(t *testing.T) {
	f := captureHost(t, 100*gib)
	f.touch("send-fails")
	_, err := f.s.CaptureImageBase(t.Context(), "img1")
	if err == nil || !strings.Contains(err.Error(), "zfs send") || !strings.Contains(err.Error(), "Input/output error") ||
		strings.Contains(err.Error(), "zfs receive") {
		t.Errorf("a failing send: %v; want the send's error", err)
	}
	f.left(t)
}

// TestAReceiveThatFailsEndsTheSend: a receive that fails before reading
// ends a send that would write on: the capture returns at once with the
// receive's error, not at the context's deadline.
func TestAReceiveThatFailsEndsTheSend(t *testing.T) {
	f := captureHost(t, 100*gib)
	f.touch("send-forever")
	f.touch("recv-fails")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := f.s.CaptureImageBase(ctx, "img1")
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the capture returned after %v: the send outlived the receive", took)
	}
	if err == nil || !strings.Contains(err.Error(), "zfs receive") || !strings.Contains(err.Error(), "exists") {
		t.Errorf("%v; want the receive's error", err)
	}
	f.left(t)
}

// TestACaptureCutShortKeepsItsSnapshot: a capture whose context ends
// returns, destroys what it received, and keeps the snapshot, from which a
// later capture starts again.
func TestACaptureCutShortKeepsItsSnapshot(t *testing.T) {
	f := captureHost(t, 100*gib)
	f.touch("recv-hangs")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := f.s.CaptureImageBase(ctx, "img1")
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for len(f.calls("receive")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no receive began")
		}
		time.Sleep(10 * time.Millisecond) //nolint:forbidigo // a process outside the test, which says nothing else
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a capture cut short succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a capture cut short did not return")
	}
	f.left(t)
	if err := os.Remove(f.dir + "/recv-hangs"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CaptureImageBase(t.Context(), "img1"); err != nil {
		t.Errorf("capturing again from the kept snapshot: %v", err)
	}
}
