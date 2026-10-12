package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCollectSparesPinnedBlobs: a blob a pull or an unpack uses survives a
// collection even before any record holds it, and goes once let go.
func TestCollectSparesPinnedBlobs(t *testing.T) {
	d, err := New(Config{Cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	blob := "sha256:" + strings.Repeat("ab", 32)
	if err := os.WriteFile(d.blobPath(blob), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	release := d.pin([]string{blob})
	d.mu.Lock()
	err = d.collectLocked()
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.blobPath(blob)); err != nil {
		t.Errorf("a pinned blob was collected: %v", err)
	}
	release()
	release() // a second release changes nothing
	if n := d.pins[blob]; n != 0 {
		t.Errorf("pins left: %d", n)
	}
	d.mu.Lock()
	err = d.collectLocked()
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.blobPath(blob)); !os.IsNotExist(err) {
		t.Errorf("an unpinned, unrecorded blob survived: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(d.cfg.Cache, "blobs", "sha256")); len(entries) != 0 {
		t.Errorf("left: %v", entries)
	}
}

// TestACachedDigestInUseKeepsItsBlobs: a digest forgotten while it is being
// unpacked from the cache keeps its blobs until the unpack is done.
func TestACachedDigestInUseKeepsItsBlobs(t *testing.T) {
	d, err := New(Config{Cache: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("cd", 32)
	blob := "sha256:" + strings.Repeat("ef", 32)
	if err := os.WriteFile(d.blobPath(blob), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.writeRecord(strings.Repeat("cd", 32), &record{Layers: []layer{{Digest: blob}}}); err != nil {
		t.Fatal(err)
	}
	_, release, err := d.cached(strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Forget(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.blobPath(blob)); err != nil {
		t.Errorf("the blob of a digest in use was collected: %v", err)
	}
	// Let go, it goes, with no other Forget to collect it.
	release()
	if _, err := os.Stat(d.blobPath(blob)); !os.IsNotExist(err) {
		t.Errorf("the blob survived its digest: %v", err)
	}
	if n := d.digests.Len(); n != 0 {
		t.Errorf("%d digest locks kept", n)
	}
}
