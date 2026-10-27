//go:build testbed

package flow

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
)

// growTrimScript ticks /data/tick every second, for as long as the
// container runs.
const growTrimScript = `trap 'echo got SIGTERM; exit 0' TERM
echo ready
i=0
while :; do i=$((i+1)); echo $i > /data/tick; sleep 1 & wait $!; done`

// TestAVolumeGrowsAndIsTrimmedWhileItsContainerRuns: a container's 2Gi
// volume grown to 5Gi grows in the running container, its process running
// on and df saying about 5G; then 1G of random data written to it and
// deleted goes back to the pool at the guest's next trim, the zvol's used
// dropping, with the agent's trim interval made short.
func TestAVolumeGrowsAndIsTrimmedWhileItsContainerRuns(t *testing.T) {
	if installed {
		t.Skip("the installed agent trims hourly, as the host has it")
	}
	c := startClusterWith(t, func(c *cluster) { c.nodeArgs = []string{"--guest-trim-interval", "20s"} })
	disk := func(size string) string {
		return fmt.Sprintf("apiVersion: storage.hangarlab.cloud/v1alpha1\nkind: Disk\nmetadata: {name: data}\nspec: {size: %s}\n", size)
	}
	c.applyYAML(disk("2Gi"))
	c.applyYAML(fmt.Sprintf(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: Container
metadata: {name: grower}
spec:
  image: %s
  command: ["/bin/sh", "-c", %q]
  resources: {cpu: 1, memory: 256Mi, disk: 1Gi}
  volumes:
  - {source: data, target: /data}
`, busyboxRef, growTrimScript))
	c.waitReady("grower", 2*time.Minute)
	uid := c.machine("grower").GetMetadata().GetUid()
	d, ok := get(c, "disk", "data", &storagev1alpha1.Disk{})
	if !ok {
		t.Fatal("no disk data")
	}
	zvol := dataset + "/disks/" + d.GetMetadata().GetUid()
	dataKiB := func() int64 {
		f := strings.Fields(mustExec(t, uid, "df -k /data | tail -n 1"))
		n, _ := strconv.ParseInt(f[1], 10, 64)
		return n
	}
	if n := dataKiB(); n > 2*gib/1024 {
		t.Fatalf("/data is %d KiB before growing", n)
	}
	launcher := mainPID(t, uid)
	booted := c.machine("grower").GetStatus().GetBootTime().AsTime()

	// 1. Grown, while the process runs.
	c.applyYAML(disk("5Gi"))
	eventually(t, 2*time.Minute, "/data grown to about 5G in the running guest", func() (bool, string) {
		n := dataKiB()
		return n >= 5*gib/1024*9/10, fmt.Sprintf("%d KiB", n)
	})
	if mainPID(t, uid) != launcher || !c.machine("grower").GetStatus().GetBootTime().AsTime().Equal(booted) {
		t.Error("the container was started again to grow its volume")
	}
	tick := mustExec(t, uid, "cat /data/tick")
	eventually(t, 10*time.Second, "the process ticking on", func() (bool, string) {
		now := mustExec(t, uid, "cat /data/tick")
		return now != tick, now
	})
	if strings.Contains(consoleText(t, uid), "got SIGTERM") {
		t.Error("the container's process was sent SIGTERM to grow its volume")
	}

	// 2. 1G written and deleted goes back to the pool at a trim. busybox's
	// dd, not head, which takes no size suffix.
	used := func() int64 {
		n, _ := strconv.ParseInt(strings.TrimSpace(mustRun(t, "zfs", "get", "-Hp", "-o", "value", "used", zvol)), 10, 64)
		return n
	}
	before := used()
	// Longer than mustExec waits: a 1 vCPU guest writes 1G in tens of seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	out, code, err := execIn(ctx, uid, "/bin/sh", "-c", "dd if=/dev/urandom of=/data/big bs=1048576 count=1024 2>/dev/null && sync")
	cancel()
	if err != nil || code != 0 {
		t.Fatalf("writing 1G: exit %d, %v: %s", code, err, out)
	}
	full := used()
	if full < before+900<<20 {
		t.Fatalf("the zvol uses %d bytes after 1G was written, %d before", full, before)
	}
	mustExec(t, uid, "rm /data/big && sync")
	eventually(t, 2*time.Minute, "the zvol's used dropping back at a trim", func() (bool, string) {
		now := used()
		return now < full-800<<20, fmt.Sprintf("used %d, %d before writing, %d once written", now, before, full)
	})
	t.Logf("the zvol used %d bytes, %d with 1G written, %d once it was deleted and trimmed", before, full, used())
}
