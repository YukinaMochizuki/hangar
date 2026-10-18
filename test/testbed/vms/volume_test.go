//go:build testbed

package vms

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
)

// TestAGuestSeesItsVolumes: volumes attached to a container's machine are
// its next disks, vdc on, in order and at their sizes; what the guest
// writes to one is there for the next machine it is attached to, and the
// launcher refuses a volume whose link leads elsewhere.
func TestAGuestSeesItsVolumes(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	v1, err := h.storage.EnsureVolume(ctx, "tbdisk1", 1<<30, "")
	if err != nil {
		t.Fatal(err)
	}
	v2, err := h.storage.EnsureVolume(ctx, "tbdisk2", 2<<30, "")
	if err != nil {
		t.Fatal(err)
	}
	spec := h.machine(t, "withdisks", driver.RestartNever, guestScript)
	spec.Disks = []driver.Volume{v1, v2}
	startAndWait(t, h, spec)

	// Sizes in 512-byte sectors, as the guest's kernel counts them.
	out, code, err := execIn(ctx, "withdisks", "cat", "/sys/block/vdc/size", "/sys/block/vdd/size")
	if err != nil || code != 0 || strings.Join(strings.Fields(out), " ") != "2097152 4194304" {
		t.Errorf("the guest's vdc and vdd: %q, exit %d, %v", out, code, err)
	}
	if _, code, _ := execIn(ctx, "withdisks", "test", "-e", "/sys/block/vde"); code == 0 {
		t.Error("the guest has a disk more than it was given")
	}
	const marker = "written by withdisks"
	if out, code, err := execIn(ctx, "withdisks", "sh", "-c", "echo '"+marker+"' | dd of=/dev/vdc bs=512 conv=sync,fsync 2>&1"); err != nil || code != 0 {
		t.Fatalf("writing vdc: %q, exit %d, %v", out, code, err)
	}
	if err := h.storage.DeleteVolume(ctx, "tbdisk1"); err == nil {
		t.Error("deleted a volume a running VM has")
	}
	if err := h.hv.Stop(ctx, "withdisks"); err != nil {
		t.Fatal(err)
	}

	// The next machine, given the volume as its only one, reads it as vdc.
	next := h.machine(t, "nextmachine", driver.RestartNever, guestScript)
	next.Disks = []driver.Volume{v1}
	startAndWait(t, h, next)
	if out, code, err := execIn(ctx, "nextmachine", "head", "-c", "64", "/dev/vdc"); err != nil || code != 0 || !strings.HasPrefix(out, marker) {
		t.Errorf("the next machine's vdc: %q, exit %d, %v", out, code, err)
	}
	if err := h.hv.Stop(ctx, "nextmachine"); err != nil {
		t.Fatal(err)
	}

	// A volume's link leading to another zvol is refused, as a root
	// disk's is.
	link := v1.Path
	right, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := filepath.EvalSymlinks(v2.Path)
	if err != nil {
		t.Fatal(err)
	}
	relink := func(target string) {
		t.Helper()
		tmp := link + ".tb"
		_ = os.Remove(tmp)
		if err := os.Symlink(target, tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, link); err != nil {
			t.Fatal(err)
		}
	}
	// Put back if the test stops before it does; the volume deleted, its
	// link is gone with it.
	t.Cleanup(func() {
		if _, err := os.Lstat(link); err == nil {
			relink(right)
		}
	})
	relink(wrong)
	if err := h.hv.Start(ctx, next); err != nil {
		t.Fatal(err)
	}
	if code := waitExited(t, h, "nextmachine", time.Minute); code != chvm.ExitFailed {
		t.Errorf("a volume's link to another zvol: exited %d, want %d", code, chvm.ExitFailed)
	}
	if e := exitError(t, "nextmachine"); !strings.Contains(e, "leads to the zvol "+strings.TrimPrefix(v2.Path, "/dev/zvol/")) {
		t.Errorf("the launcher says %q", e)
	}
	if err := h.hv.Stop(ctx, "nextmachine"); err != nil {
		t.Fatal(err)
	}
	relink(right)

	for _, uid := range []string{"tbdisk1", "tbdisk2"} {
		if err := h.storage.DeleteVolume(ctx, uid); err != nil {
			t.Errorf("deleting %s once no VM has it: %v", uid, err)
		}
	}
}

// imageSeed is what the test image holds in /var/lib/app/seed.
const imageSeed = "from the image\n"

// TestAGuestMountsItsVolumes: hangar-init formats a blank volume and fills
// it from the image, owners and modes included, as Docker fills a new
// named volume; what the container writes is there for the next machine,
// which is not filled again; a read-only volume cannot be written, a
// subpath mounts its directory alone; and a volume holding a partition
// table fails the boot, left as it was. See
// docs/decisions/container-volumes.md.
func TestAGuestMountsItsVolumes(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	data, err := h.storage.EnsureVolume(ctx, "tbvol1", 1<<30, "")
	if err != nil {
		t.Fatal(err)
	}
	run := func(uid string, m driver.Mount) {
		t.Helper()
		spec := h.machine(t, uid, driver.RestartNever, guestScript)
		spec.Disks = []driver.Volume{data}
		m.Name = "data"
		spec.Mounts = []driver.Mount{m}
		startAndWait(t, h, spec)
	}
	in := func(uid string, args ...string) (string, int32) {
		t.Helper()
		out, code, err := execIn(ctx, uid, args...)
		if err != nil {
			t.Fatalf("%v in %s: %v", args, uid, err)
		}
		return out, code
	}

	run("firstvol", driver.Mount{Target: "/var/lib/app"})
	if out, _ := in("firstvol", "stat", "-c", "%u:%g %a", "/var/lib/app", "/var/lib/app/seed"); out != "70:70 700\n70:70 600\n" {
		t.Errorf("the new volume's root and seed: %q, want the image's owners and modes", out)
	}
	if out, _ := in("firstvol", "cat", "/var/lib/app/seed"); out != imageSeed {
		t.Errorf("seed: %q", out)
	}
	if out, _ := in("firstvol", "ls", "-A", "/var/lib/app"); out != "seed\n" {
		t.Errorf("the new volume holds %q, not the image's alone (a lost+found keeps initdb out)", out)
	}
	if out, _ := in("firstvol", "grep", " /var/lib/app ", "/proc/mounts"); !strings.HasPrefix(out, "/dev/vdc /var/lib/app ext4 rw") {
		t.Errorf("/proc/mounts: %q", out)
	}
	if out, code, err := execAs(ctx, "firstvol", "70:70", "sh", "-c", "echo by70 > /var/lib/app/by70"); err != nil || code != 0 {
		t.Errorf("the image's user writing its new volume: %q, exit %d, %v", out, code, err)
	}
	if out, code := in("firstvol", "sh", "-c", "echo kept > /var/lib/app/written && mkdir /var/lib/app/sub && echo s > /var/lib/app/sub/f && rm /var/lib/app/seed && sync"); code != 0 {
		t.Fatalf("writing the volume: %q, exit %d", out, code)
	}
	if !strings.Contains(initOf(t, "firstvol"), "made an ext4 filesystem on /dev/vdc") {
		t.Errorf("hangar-init did not say it formatted the volume: %q", initOf(t, "firstvol"))
	}
	if err := h.hv.Stop(ctx, "firstvol"); err != nil {
		t.Fatal(err)
	}

	// Grown, and mounted again where the image has its seed: the volume
	// fills its disk, and holds what the first machine left, its seed
	// deleted and not filled again.
	if data, err = h.storage.EnsureVolume(ctx, "tbvol1", 2<<30, ""); err != nil {
		t.Fatal(err)
	}
	run("againvol", driver.Mount{Target: "/var/lib/app"})
	if out, _ := in("againvol", "ls", "/var/lib/app"); out != "by70\nsub\nwritten\n" {
		t.Errorf("the volume mounted again holds %q, want what the first machine left, and no seed", out)
	}
	if out, _ := in("againvol", "stat", "-f", "-c", "%b %S", "/var/lib/app"); !grownTo(out, 1900<<20) {
		t.Errorf("the filesystem of the grown volume: %q blocks and block size", out)
	}
	if log := initOf(t, "againvol"); strings.Contains(log, "made an ext4") || strings.Contains(log, "filled volume") {
		t.Errorf("a volume in use was formatted or filled again: %q", log)
	}
	if err := h.hv.Stop(ctx, "againvol"); err != nil {
		t.Fatal(err)
	}

	// Read-only, it cannot be written.
	run("rovol", driver.Mount{Target: "/data", ReadOnly: true})
	if out, _ := in("rovol", "cat", "/data/written"); out != "kept\n" {
		t.Errorf("what the first machine wrote: %q", out)
	}
	if out, code := in("rovol", "touch", "/data/x"); code == 0 {
		t.Errorf("wrote a read-only volume: %q", out)
	}
	if err := h.hv.Stop(ctx, "rovol"); err != nil {
		t.Fatal(err)
	}

	// A subpath mounts that directory alone.
	run("subvol", driver.Mount{Target: "/srv", Subpath: "sub"})
	if out, _ := in("subvol", "ls", "/srv"); out != "f\n" {
		t.Errorf("/srv of subpath sub: %q", out)
	}
	if err := h.hv.Stop(ctx, "subvol"); err != nil {
		t.Fatal(err)
	}

	// A volume inside a read-only one gets its mountpoint there, and is
	// written all the same.
	var nest []driver.Volume
	for _, uid := range []string{"tbvol3", "tbvol4"} {
		v, err := h.storage.EnsureVolume(ctx, uid, 1<<30, "")
		if err != nil {
			t.Fatal(err)
		}
		nest = append(nest, v)
	}
	nested := h.machine(t, "nestvol", driver.RestartNever, guestScript)
	nested.Disks = nest
	nested.Mounts = []driver.Mount{{Disk: 1, Name: "inner", Target: "/nest/inner"}, {Disk: 0, Name: "outer", Target: "/nest", ReadOnly: true}}
	startAndWait(t, h, nested)
	if out, code := in("nestvol", "touch", "/nest/inner/x"); code != 0 {
		t.Errorf("writing the inner volume: %q, exit %d", out, code)
	}
	if out, code := in("nestvol", "touch", "/nest/y"); code == 0 {
		t.Errorf("wrote the read-only outer volume: %q", out)
	}
	if err := h.hv.Stop(ctx, "nestvol"); err != nil {
		t.Fatal(err)
	}

	// A partitioned volume is never formatted: the guest does not boot,
	// and the volume is as it was.
	parted, err := h.storage.EnsureVolume(ctx, "tbvol2", 1<<30, "")
	if err != nil {
		t.Fatal(err)
	}
	gpt := []byte("EFI PART")
	f, err := os.OpenFile(parted.Path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(gpt, 512); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	spec := h.machine(t, "partedvol", driver.RestartNever, guestScript)
	spec.Disks = []driver.Volume{parted}
	spec.Mounts = []driver.Mount{{Disk: 0, Name: "parted", Target: "/data"}}
	if err := h.hv.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if code := waitExited(t, h, "partedvol", time.Minute); code == 0 {
		t.Error("a guest with a partitioned volume booted")
	}
	if log := initOf(t, "partedvol"); !strings.Contains(log, "a partition table, neither ext4 nor zeros") {
		t.Errorf("hangar-init says %q", log)
	}
	head := make([]byte, 4096)
	if f, err := os.Open(parted.Path); err == nil {
		_, _ = f.ReadAt(head, 0)
		_ = f.Close()
	}
	if want := append(make([]byte, 512), gpt...); !bytes.Equal(head[:520], want) || !bytes.Equal(head[520:], make([]byte, 4096-520)) {
		t.Error("the partitioned volume was written to")
	}
	if err := h.hv.Stop(ctx, "partedvol"); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"tbvol1", "tbvol2", "tbvol3", "tbvol4"} {
		if err := h.storage.DeleteVolume(ctx, uid); err != nil {
			t.Errorf("deleting %s: %v", uid, err)
		}
	}
}

// grownTo reports whether stat -f's "blocks block-size" is at least bytes.
func grownTo(stat string, bytes int64) bool {
	var blocks, size int64
	if _, err := fmt.Sscan(stat, &blocks, &size); err != nil {
		return false
	}
	return blocks*size >= bytes
}

// initOf is what hangar-init said on the machine's console, over its runs.
func initOf(t *testing.T, uid string) string {
	t.Helper()
	var b strings.Builder
	for _, r := range consoleOf(t, uid) {
		if r.Stream == console.Init {
			b.Write(r.Payload)
		}
	}
	return b.String()
}
