//go:build testbed

package disks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/zfsstore"
)

// What the tests make on the host, all under these, which a run sweeps
// first: datasets under testParent, files under scratchRoot.
const (
	pool        = "hangar"
	testParent  = pool + "/testbed-disks"
	scratchRoot = "/var/lib/hangar/images/testbed-disks"
)

var sweepOnce sync.Once

// prepare sweeps what an earlier run left, once per run, under the
// testbed's lock, which vmtest.Main holds.
func prepare(t *testing.T) {
	t.Helper()
	sweepOnce.Do(func() {
		if out, err := exec.Command("zpool", "list", "-H", "-o", "name", pool).CombinedOutput(); err != nil {
			t.Fatalf("no pool %s: run test/testbed/setup.sh first: %v: %s", pool, err, out)
		}
		if exec.Command("zfs", "list", "-H", testParent).Run() == nil {
			mustRun(t, "zfs", "destroy", "-r", testParent)
		}
		out, _ := tryRun("zfs", "list", "-H", "-o", "name", "-r", pool+"/images")
		for _, ds := range strings.Fields(out) {
			if strings.HasSuffix(ds, "-small") {
				mustRun(t, "zfs", "destroy", ds)
			}
		}
		if err := os.RemoveAll(scratchRoot); err != nil {
			t.Fatal(err)
		}
		mustRun(t, "zfs", "create", "-o", "mountpoint=none", testParent)
	})
}

func mustRun(t testing.TB, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

var hostCount atomic.Int32

// host is a dataset and directories of one test's own, for its drivers,
// with decoys the drivers must never list, change or destroy.
type host struct {
	dataset, cache, scratch string
	decoys                  map[string]string // dataset to its guid
}

// Digests the decoys claim, or sit at the base names of.
var (
	decoyDigest     = "sha256:" + strings.Repeat("de", 32)
	bareDecoyDigest = "sha256:" + strings.Repeat("be", 32)
)

func newHost(t *testing.T) *host {
	t.Helper()
	prepare(t)
	n := hostCount.Add(1)
	h := &host{
		dataset: fmt.Sprintf("%s/h%d", testParent, n),
		cache:   filepath.Join(scratchRoot, fmt.Sprint(n), "cache"),
		scratch: filepath.Join(scratchRoot, fmt.Sprint(n), "scratch"),
		decoys:  map[string]string{},
	}
	mustRun(t, "zfs", "create", "-o", "mountpoint=none", h.dataset)
	t.Cleanup(func() {
		h.checkDecoys(t)
		mustRun(t, "zfs", "destroy", "-r", h.dataset)
		_ = os.RemoveAll(filepath.Dir(h.cache))
	})
	// A first instance makes the driver's datasets, for the decoys to go
	// in them.
	h.storage(t)
	hexDecoy := strings.TrimPrefix(decoyDigest, "sha256:")
	for _, d := range []struct {
		name  string
		props []string
	}{
		// At a base's name, without the property that makes it one; the
		// first with a snapshot, which ZFS keeps from a plain destroy.
		{h.dataset + "/bases/" + hexDecoy, nil},
		{h.dataset + "/bases/" + strings.TrimPrefix(bareDecoyDigest, "sha256:"), nil},
		// A root disk's place, unclaimed, and claimed by another machine.
		{h.dataset + "/roots/decoy", nil},
		{h.dataset + "/roots/m-decoy", []string{"hangar:machine=someone-else", "hangar:digest=" + decoyDigest}},
		// Claiming to be a base and a root, outside the driver's datasets.
		{h.dataset + "/outside-" + hexDecoy, []string{"hangar:digest=" + decoyDigest}},
		{h.dataset + "/outside-root", []string{"hangar:machine=m1", "hangar:digest=" + decoyDigest}},
		// A volume's place, unclaimed, claimed by another disk, and a volume
		// outside the driver's datasets.
		{h.dataset + "/disks/decoy", nil},
		{h.dataset + "/disks/d-decoy", []string{"hangar:disk=someone-else"}},
		{h.dataset + "/outside-disk", []string{"hangar:disk=d1"}},
		// A disk image's base's place, unclaimed.
		{h.dataset + "/vmbases/" + hexDecoy, nil},
		// A custom image's base's place, unclaimed, and claimed by another
		// image.
		{h.dataset + "/vmbases/customimage-decoy", nil},
		{h.dataset + "/vmbases/customimage-other", []string{"hangar:customimage=someone-else"}},
	} {
		args := []string{"create", "-s", "-V", "8M", "-o", "volmode=dev"}
		for _, p := range d.props {
			args = append(args, "-o", p)
		}
		mustRun(t, "zfs", append(args, d.name)...)
		h.decoys[d.name] = guid(t, d.name)
	}
	snap := h.dataset + "/bases/" + hexDecoy + "@base"
	mustRun(t, "zfs", "snapshot", snap)
	h.decoys[snap] = guid(t, snap)
	// Custom images' snapshots: a base's, unclaimed and claimed by another
	// image, and one to capture a decoy disk from, which is not the
	// driver's.
	for _, s := range []struct{ name, prop string }{
		{h.dataset + "/vmbases/customimage-decoy@base", ""},
		{h.dataset + "/vmbases/customimage-other@base", "hangar:customimage=someone-else"},
		{h.dataset + "/disks/d-decoy@customimage-decoy", "hangar:customimage=decoy"},
	} {
		args := []string{"snapshot"}
		if s.prop != "" {
			args = append(args, "-o", s.prop)
		}
		mustRun(t, "zfs", append(args, s.name)...)
		h.decoys[s.name] = guid(t, s.name)
	}
	// A finished base in all but its name, which is another digest's.
	misnamed := h.dataset + "/bases/" + strings.Repeat("ad", 32)
	mustRun(t, "zfs", "create", "-s", "-V", "8M", "-o", "volmode=dev", "-o", "hangar:digest="+decoyDigest, misnamed)
	mustRun(t, "zfs", "snapshot", "-o", "hangar:digest="+decoyDigest, misnamed+"@base")
	h.decoys[misnamed] = guid(t, misnamed)
	h.decoys[misnamed+"@base"] = guid(t, misnamed+"@base")
	return h
}

func guid(t testing.TB, ds string) string {
	t.Helper()
	return strings.TrimSpace(mustRun(t, "zfs", "get", "-H", "-o", "value", "guid", ds))
}

// checkDecoys fails the test unless every decoy is still there, the same
// dataset it was made as.
func (h *host) checkDecoys(t *testing.T) {
	t.Helper()
	for ds, want := range h.decoys {
		out, err := exec.Command("zfs", "get", "-H", "-o", "value", "guid", ds).CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != want {
			t.Errorf("decoy %s: %v %s, want guid %s", ds, err, out, want)
		}
	}
}

func (h *host) images(t testing.TB) *registry.Images {
	t.Helper()
	d, err := registry.New(registry.Config{Cache: h.cache})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (h *host) storage(t testing.TB) *zfsstore.Store {
	t.Helper()
	s, err := zfsstore.New(context.Background(), zfsstore.Config{Dataset: h.dataset, Scratch: h.scratch})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// listing logs every dataset of the host with what the driver reads, for
// the record of a run.
func (h *host) listing(t testing.TB, when string) {
	t.Helper()
	out := mustRun(t, "zfs", "list", "-r", "-t", "all", "-o", "name,hangar:digest,hangar:machine,hangar:disk,volsize,used,origin", h.dataset)
	t.Logf("%s:\n%s", when, out)
}

// tryRun runs a command and returns its output and error.
func tryRun(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
