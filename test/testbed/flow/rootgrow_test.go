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

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
)

// TestARootDiskGrowsByItself: a container whose root disk grows by itself,
// filled past its threshold, grows, through the node's usage poll, the
// owner's grant and the in-place path, in the same boot. Then, its
// project's disk limit reached, a root filled past its threshold again is
// refused its growth: it stays at its size, and filling it up ends in a
// clean ENOSPC, the filesystem writable once room is made, with no I/O
// error in the guest kernel's log.
func TestARootDiskGrowsByItself(t *testing.T) {
	c := startCluster(t)
	c.applyYAML(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: Container
metadata: {name: grower}
spec:
  image: ` + busyboxRef + `
  command: ["/bin/sh", "-c", ` + fmt.Sprintf("%q", idleScript) + `]
  resources: {cpu: 1, memory: 256Mi, disk: 1Gi, diskGrow: {max: 4Gi}}
`)
	c.waitReady("grower", 3*time.Minute)
	m := c.machine("grower")
	uid := m.GetMetadata().GetUid()
	launcher, ch := mainPID(t, uid), chPID(t, uid)
	bootID := strings.TrimSpace(mustExec(t, uid, "cat /proc/sys/kernel/random/boot_id"))
	bootTime := m.GetStatus().GetBootTime().AsTime()

	before := fillPast(t, uid, 85)
	var after int64
	eventually(t, 3*time.Minute, "grower's root grown by itself to 2Gi", func() (bool, string) {
		after = rootSizeKiB(t, uid)
		ct, _ := get(c, "container", "grower", &computev1alpha1.Container{})
		return roots(t)[uid].volsize == 2*gib && after > 2*gib/1024*9/10 && ct.GetStatus().GetDisk() == "2Gi",
			fmt.Sprintf("zvol %d bytes, the guest's root %d KiB, the container's disk %q, the machine asks %q",
				roots(t)[uid].volsize, after, ct.GetStatus().GetDisk(), c.machine("grower").GetStatus().GetDiskWanted())
	})
	if mainPID(t, uid) != launcher || chPID(t, uid) != ch {
		t.Errorf("grower's VM was started again to grow: launcher %d, Cloud Hypervisor %d; were %d, %d", mainPID(t, uid), chPID(t, uid), launcher, ch)
	}
	if got := strings.TrimSpace(mustExec(t, uid, "cat /proc/sys/kernel/random/boot_id")); got != bootID {
		t.Errorf("grower's guest booted again: boot id %s, was %s", got, bootID)
	}
	if bt := c.machine("grower").GetStatus().GetBootTime().AsTime(); !bt.Equal(bootTime) {
		t.Errorf("grower's boot time went from %v to %v", bootTime, bt)
	}
	t.Logf("grower's root, filled to 85%%, grew by itself from %d KiB to %d KiB in the same boot (%s)", before, after, bootID)

	// Its project held to the 2Gi it has: the next growth, to 4Gi, is
	// refused.
	project := m.GetMetadata().GetProject()
	p, ok := get(c, "project", project, &tenancyv1alpha1.Project{})
	if !ok {
		t.Fatalf("no project %s", project)
	}
	c.applyYAML(fmt.Sprintf(`apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Project
metadata: {name: %s}
spec: {tenant: %s, limits: {disk: 2Gi}}
`, project, p.GetSpec().GetTenant()))
	mustExec(t, uid, "rm -f /fill && sync")
	fillPast(t, uid, 85)
	eventually(t, 3*time.Minute, "grower's growth refused past its project's limit", func() (bool, string) {
		ct, _ := get(c, "container", "grower", &computev1alpha1.Container{})
		blocked := conditions.Find(ct.GetStatus().GetConditions(), "DiskGrowBlocked")
		return blocked.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE && blocked.GetReason() == "QuotaExceeded", fmt.Sprint(blocked)
	})
	if v := roots(t)[uid].volsize; v != 2*gib {
		t.Errorf("grower's zvol is %d bytes past its project's limit; want 2 GiB", v)
	}
	out := mustExec(t, uid, "dd if=/dev/zero of=/full bs=1M 2>&1; echo rc=$?")
	if !strings.Contains(out, "No space left on device") || strings.Contains(out, "I/O error") || strings.Contains(out, "rc=0") {
		t.Errorf("filling grower's root up: %q; want ENOSPC alone", out)
	}
	if out := mustExec(t, uid, "rm /full /fill && sync && echo kept > /after && cat /after"); strings.TrimSpace(out) != "kept" {
		t.Errorf("writing once room is made: %q", out)
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
	if v := roots(t)[uid].volsize; v != 2*gib || mainPID(t, uid) != launcher {
		t.Errorf("grower after ENOSPC: zvol %d bytes, launcher %d (was %d)", v, mainPID(t, uid), launcher)
	}
	t.Logf("grower, refused its growth past its project's limit, filled up to ENOSPC at %d KiB and wrote again: %q", rootSizeKiB(t, uid), out)
	c.cli("delete", "container", "grower")
}

// fillPast writes /fill in the guest until its root is full past percent,
// and returns the root's size in KiB.
func fillPast(t *testing.T, uid string, percent int64) int64 {
	t.Helper()
	fields := strings.Fields(mustExec(t, uid, "df -k / | tail -n 1"))
	if len(fields) < 3 {
		t.Fatalf("df: %q", fields)
	}
	size, err1 := strconv.ParseInt(fields[1], 10, 64)
	used, err2 := strconv.ParseInt(fields[2], 10, 64)
	if err1 != nil || err2 != nil {
		t.Fatalf("df: %q", fields)
	}
	mib := (size*percent/100-used)/1024 + 16
	mustExec(t, uid, fmt.Sprintf("dd if=/dev/zero of=/fill bs=1M count=%d 2>&1 && sync", mib))
	return size
}
