package zfsstore

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// TestACloneRecordsTheUseOfItsBase: cloning a root disk from a base records
// on the base's snapshot when it was used, where Bases reads it from.
func TestACloneRecordsTheUseOfItsBase(t *testing.T) {
	f := newFakeZFS(t)
	snap := "hangar/bases/" + testHex + "@base"
	f.dataset(snap, propDigest, testDigest, "volsize", itoa(testBaseSize), propUsedAt, "2020-01-01T00:00:00Z")
	before := time.Now()
	if _, err := f.s.EnsureRoot(t.Context(), "m1", testDigest, testBaseSize); err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339Nano, f.prop(snap, propUsedAt))
	if err != nil || at.Before(before) {
		t.Errorf("cloned from at %v, the base says it was last used at %q (%v)", before, f.prop(snap, propUsedAt), err)
	}
}

// TestAnUnpackOutOfRoomSaysWhatItNeeds: a fill that runs out of room where
// it unpacks is a NoSpaceError there, known to need more than was free,
// and aiming at more.
func TestAnUnpackOutOfRoomSaysWhatItNeeds(t *testing.T) {
	f := newFakeZFS(t)
	f.s.cfg.Scratch = t.TempDir()
	f.dataset("hangar", "used", "0", "available", itoa(1<<40))
	err := f.s.EnsureBase(t.Context(), testDigest, 1<<20, func(dir string) error {
		return &os.PathError{Op: "write", Path: dir, Err: syscall.ENOSPC}
	})
	var full *driver.NoSpaceError
	if !errors.As(err, &full) || full.Room != driver.ImageRoom || full.AtLeast() != full.Free+1 || full.Need <= full.AtLeast() {
		t.Errorf("an unpack out of room: %v (%+v)", err, full)
	}
}

// TestDeletingARootRecordsTheUseOfItsBase: the base of a root disk deleted
// was in use until then, and says so.
func TestDeletingARootRecordsTheUseOfItsBase(t *testing.T) {
	f := newFakeZFS(t)
	snap := "hangar/bases/" + testHex + "@base"
	f.dataset(snap, propDigest, testDigest, "volsize", itoa(testBaseSize), propUsedAt, "2020-01-01T00:00:00Z")
	f.dataset("hangar/roots/m1", propMachine, "m1", propDigest, testDigest, "volsize", itoa(testBaseSize))
	before := time.Now()
	if err := f.s.DeleteRoot(t.Context(), "m1"); err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339Nano, f.prop(snap, propUsedAt))
	if err != nil || at.Before(before) {
		t.Errorf("a root deleted at %v, its base says it was last used at %q (%v)", before, f.prop(snap, propUsedAt), err)
	}
	if len(f.calls("destroy hangar/roots/m1")) != 1 {
		t.Errorf("destroys: %q", f.calls("destroy"))
	}
}
