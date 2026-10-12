//go:build testbed

package disks

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// busybox is small, so its base is far smaller than any root disk.
const (
	busyboxRef    = "docker.io/library/busybox:1.37"
	busyboxDigest = "sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
)

// TestTheGuestGrowsItsRoot: a guest booting from a root disk larger than
// its image's filesystem finds that filesystem grown to the whole disk, at
// once and again after the disk grows; an untouched disk is left as it is.
func TestTheGuestGrowsItsRoot(t *testing.T) {
	h := newHost(t)
	images, s := h.images(t), h.storage(t)
	ctx := context.Background()
	if err := s.EnsureBase(ctx, busyboxDigest, 0, func(dir string) error {
		return images.Unpack(ctx, busyboxRef, busyboxDigest, dir, anon)
	}); err != nil {
		t.Fatal(err)
	}
	base := h.dataset + "/bases/" + strings.TrimPrefix(busyboxDigest, "sha256:")
	baseSize, _ := strconv.ParseInt(strings.TrimSpace(mustRun(t, "zfs", "get", "-Hp", "-o", "value", "volsize", base+"@base")), 10, 64)
	t.Logf("the base is %d bytes", baseSize)

	for _, step := range []struct {
		size int64
		grew bool
	}{
		{2 << 30, true},
		{2 << 30, false}, // already the disk's size
		{5 << 30, true},
	} {
		root, err := s.EnsureRoot(ctx, "grow", busyboxDigest, step.size)
		if err != nil {
			t.Fatal(err)
		}
		v := vmtest.NewOn(t, &guestv1.BootConfig{Process: &guestv1.Process{
			Args: []string{"sh", "-c", `stat -f -c '%b %S' /; cat /sys/block/vda/size`},
		}}, root.Path)
		start := time.Now()
		v.Start()
		v.WaitPowerOff(2 * time.Minute)
		out, initLog := consoleOf(t, v)
		t.Logf("%d bytes asked, %d given; booted and off in %v; guest says %q; hangar-init says %q",
			step.size, root.Size, time.Since(start), out, initLog)
		var blocks, bsize, sectors int64
		if _, err := fmt.Sscan(out, &blocks, &bsize, &sectors); err != nil {
			t.Fatalf("the guest's output %q: %v", out, err)
		}
		if sectors*512 != root.Size {
			t.Errorf("the guest sees %d bytes of disk, %d were given", sectors*512, root.Size)
		}
		// statfs leaves out ext4's own metadata, a few percent.
		if fs := blocks * bsize; fs < root.Size*95/100 || fs > root.Size {
			t.Errorf("the guest's root filesystem is %d bytes on a disk of %d", fs, root.Size)
		}
		if grew := strings.Contains(initLog, "grew the root filesystem"); grew != step.grew {
			t.Errorf("grew %t, want %t: %q", grew, step.grew, initLog)
		}
		// udev never read the filesystem the guest left.
		mustRun(t, "udevadm", "settle")
		if props := mustRun(t, "udevadm", "info", "--query=property", "--name="+root.Path); strings.Contains(props, "ID_FS_") {
			t.Errorf("udev probed the root disk after its VM stopped:\n%s", props)
		}
		v.CheckCleanDisk(t)
		dump := mustRun(t, "dumpe2fs", "-h", root.Path)
		m := regexp.MustCompile(`(?m)^Block count:\s+(\d+)$`).FindStringSubmatch(dump)
		if m == nil || m[1] != fmt.Sprint(root.Size/4096) {
			t.Errorf("block count %v, want %d", m, root.Size/4096)
		}
	}
	// The base is never written: its filesystem is as mkfs left it.
	if out, err := exec.Command("e2fsck", "-fn", "/dev/zvol/"+base).CombinedOutput(); err != nil {
		t.Errorf("the base: %v: %s", err, out)
	}
	if err := s.DeleteRoot(ctx, "grow"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBase(ctx, busyboxDigest); err != nil {
		t.Fatal(err)
	}
}

// consoleOf returns what a powered-off guest's process wrote, and what
// hangar-init said.
func consoleOf(t *testing.T, v *vmtest.VM) (stdout, initLog string) {
	t.Helper()
	recs, err := v.Console()
	if err != nil {
		t.Fatal(err)
	}
	var out, init strings.Builder
	for _, r := range recs {
		switch r.Stream {
		case console.Stdout:
			out.Write(r.Payload)
		case console.Init:
			init.Write(r.Payload)
		}
	}
	return out.String(), init.String()
}
