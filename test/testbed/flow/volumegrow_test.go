//go:build testbed

package flow

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
)

// TestADiskVolumeGrowsByItself: a container's Disk volume that grows by
// itself, filled past its threshold, grows, through the node's usage poll,
// the Disk's grant and the in-place path, in the same boot. Then, its
// project's disk limit reached, the volume filled past its threshold again
// is refused its growth: it stays at its size, and filling it up ends in a
// clean ENOSPC, the filesystem writable once room is made, with no I/O
// error in the guest kernel's log.
func TestADiskVolumeGrowsByItself(t *testing.T) {
	c := startCluster(t)
	c.applyYAML(`apiVersion: storage.hangarlab.cloud/v1alpha1
kind: Disk
metadata: {name: growdata}
spec: {size: 1Gi, grow: {max: 4Gi}}
`)
	c.applyYAML(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: Container
metadata: {name: voluser}
spec:
  image: ` + busyboxRef + `
  command: ["/bin/sh", "-c", ` + fmt.Sprintf("%q", idleScript) + `]
  resources: {cpu: 1, memory: 256Mi, disk: 1Gi}
  volumes:
  - {type: volume, source: growdata, target: /data}
`)
	c.waitReady("voluser", 3*time.Minute)
	m := c.machine("voluser")
	uid := m.GetMetadata().GetUid()
	d, _ := get(c, "disk", "growdata", &storagev1alpha1.Disk{})
	zvol := dataset + "/disks/" + d.GetMetadata().GetUid()
	launcher, ch := mainPID(t, uid), chPID(t, uid)
	bootID := strings.TrimSpace(mustExec(t, uid, "cat /proc/sys/kernel/random/boot_id"))
	bootTime := m.GetStatus().GetBootTime().AsTime()

	before := fillDir(t, uid, "/data", 85)
	var after int64
	eventually(t, 3*time.Minute, "growdata grown by itself to 2Gi", func() (bool, string) {
		after = dirSizeKiB(t, uid, "/data")
		d, _ := get(c, "disk", "growdata", &storagev1alpha1.Disk{})
		return volsize(t, zvol) == 2*gib && after > 2*gib/1024*9/10 && d.GetStatus().GetSize() == "2Gi",
			fmt.Sprintf("zvol %d bytes, the guest's /data %d KiB, the disk's size %q, the machine asks %v",
				volsize(t, zvol), after, d.GetStatus().GetSize(), c.machine("voluser").GetStatus().GetDisksWanted())
	})
	if mainPID(t, uid) != launcher || chPID(t, uid) != ch {
		t.Errorf("voluser's VM was started again to grow: launcher %d, Cloud Hypervisor %d; were %d, %d", mainPID(t, uid), chPID(t, uid), launcher, ch)
	}
	if got := strings.TrimSpace(mustExec(t, uid, "cat /proc/sys/kernel/random/boot_id")); got != bootID {
		t.Errorf("voluser's guest booted again: boot id %s, was %s", got, bootID)
	}
	if bt := c.machine("voluser").GetStatus().GetBootTime().AsTime(); !bt.Equal(bootTime) {
		t.Errorf("voluser's boot time went from %v to %v", bootTime, bt)
	}
	if g := conditions.Find(c.machine("voluser").GetStatus().GetConditions(), computeapi.VolumeGrowth); g != nil {
		t.Errorf("VolumeGrowth %v; want none, nothing failed", g)
	}
	t.Logf("voluser's /data, filled to 85%%, grew by itself from %d KiB to %d KiB in the same boot (%s)", before, after, bootID)

	// Its project held to the 3Gi it has, its 1Gi root and 2Gi volume: the
	// next growth, to 4Gi, is refused.
	project := m.GetMetadata().GetProject()
	p, ok := get(c, "project", project, &tenancyv1alpha1.Project{})
	if !ok {
		t.Fatalf("no project %s", project)
	}
	c.applyYAML(fmt.Sprintf(`apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Project
metadata: {name: %s}
spec: {tenant: %s, limits: {disk: 3Gi}}
`, project, p.GetSpec().GetTenant()))
	mustExec(t, uid, "rm -f /data/fill && sync")
	fillDir(t, uid, "/data", 85)
	eventually(t, 3*time.Minute, "growdata's growth refused past its project's limit", func() (bool, string) {
		d, _ := get(c, "disk", "growdata", &storagev1alpha1.Disk{})
		blocked := conditions.Find(d.GetStatus().GetConditions(), "DiskGrowBlocked")
		return blocked.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE && blocked.GetReason() == "QuotaExceeded", fmt.Sprint(blocked)
	})
	if v := volsize(t, zvol); v != 2*gib {
		t.Errorf("growdata's zvol is %d bytes past its project's limit; want 2 GiB", v)
	}
	out := mustExec(t, uid, "dd if=/dev/zero of=/data/full bs=1M 2>&1; echo rc=$?")
	if !strings.Contains(out, "No space left on device") || strings.Contains(out, "I/O error") || strings.Contains(out, "rc=0") {
		t.Errorf("filling growdata up: %q; want ENOSPC alone", out)
	}
	if got := mustExec(t, uid, "rm /data/full /data/fill && sync && echo kept > /data/after && cat /data/after"); strings.TrimSpace(got) != "kept" {
		t.Errorf("writing once room is made: %q", got)
	}
	serial, err := os.ReadFile(filepath.Join(vmDir, uid, chvm.SerialLog))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"I/O error", "EXT4-fs error", "Remounting filesystem read-only"} {
		if strings.Contains(string(serial), bad) {
			t.Errorf("the guest kernel's log says %q:\n%s", bad, tail(filepath.Join(vmDir, uid, chvm.SerialLog)))
		}
	}
	if v := volsize(t, zvol); v != 2*gib || mainPID(t, uid) != launcher {
		t.Errorf("voluser after ENOSPC: zvol %d bytes, launcher %d (was %d)", v, mainPID(t, uid), launcher)
	}
	t.Logf("growdata, refused its growth past its project's limit, filled up to ENOSPC at %d KiB and wrote again: %q", dirSizeKiB(t, uid, "/data"), out)
	c.cli("delete", "container", "voluser")
	if _, err := c.tryCLI(nil, "wait", "container", "voluser", "--for=delete", "--timeout=2m"); err != nil {
		t.Errorf("voluser never went: %v", err)
	}
	c.cli("delete", "disk", "growdata")
}

// volsize is the volsize of the zvol name, in bytes.
func volsize(t *testing.T, name string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(strings.TrimSpace(mustRun(t, "zfs", "get", "-H", "-p", "-o", "value", "volsize", name)), 10, 64)
	if err != nil {
		t.Fatalf("volsize of %s: %v", name, err)
	}
	return n
}

// dirSizeKiB is the size of the filesystem dir is on, in the guest, as df
// says.
func dirSizeKiB(t *testing.T, uid, dir string) int64 {
	t.Helper()
	fields := strings.Fields(mustExec(t, uid, "df -k "+dir+" | tail -n 1"))
	if len(fields) < 2 {
		t.Fatalf("df %s: %q", dir, fields)
	}
	n, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		t.Fatalf("df %s: %q: %v", dir, fields, err)
	}
	return n
}

// fillDir writes dir/fill in the guest until the filesystem dir is on is
// full past percent, and returns its size in KiB.
func fillDir(t *testing.T, uid, dir string, percent int64) int64 {
	t.Helper()
	fields := strings.Fields(mustExec(t, uid, "df -k "+dir+" | tail -n 1"))
	if len(fields) < 3 {
		t.Fatalf("df %s: %q", dir, fields)
	}
	size, err1 := strconv.ParseInt(fields[1], 10, 64)
	used, err2 := strconv.ParseInt(fields[2], 10, 64)
	if err1 != nil || err2 != nil {
		t.Fatalf("df %s: %q", dir, fields)
	}
	mib := (size*percent/100-used)/1024 + 16
	mustExec(t, uid, fmt.Sprintf("dd if=/dev/zero of=%s/fill bs=1M count=%d 2>&1 && sync", dir, mib))
	return size
}
