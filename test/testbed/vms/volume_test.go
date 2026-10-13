//go:build testbed

package vms

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	v1, err := h.storage.EnsureVolume(ctx, "tbdisk1", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := h.storage.EnsureVolume(ctx, "tbdisk2", 2<<30)
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
