//go:build testbed

package vms

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// upgradeDir holds the launcher TestAnUpgradeKeepsVMsRunning replaces.
const upgradeDir = "/var/tmp/hangar-testbed-vms-upgrade"

// childCH is the pid of the Cloud Hypervisor the machine's launcher runs,
// 0 if none, wherever its socket is.
func childCH(t *testing.T, uid string) int {
	t.Helper()
	out, _ := exec.Command("pgrep", "-P", strconv.Itoa(mainPID(t, uid)), "-x", "cloud-hyperviso").Output()
	pid, _ := strconv.Atoi(strings.TrimSpace(strings.Split(string(out), "\n")[0]))
	return pid
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestAnUpgradeKeepsVMsRunning: a VM a driver without VM users started, in
// a unit with no sandbox, runs on as root when hangar-node is upgraded,
// its launcher replaced as a release replaces it (a new file renamed over
// the old). The driver still reaches its guest. Restarting under its
// policy, it runs the new launcher, still as root: its unit says so. Once
// the driver starts it again, it runs as a VM user of its own, confined.
// HANGAR_TESTBED_OLD_NODE names the hangar-node it starts with, such as
// one built from a release without VM users; it is the one under test if
// unset.
func TestAnUpgradeKeepsVMsRunning(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	old := os.Getenv("HANGAR_TESTBED_OLD_NODE")
	if old == "" {
		old = vmtest.Artifacts().Node
	}
	if err := os.MkdirAll(upgradeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(upgradeDir) })
	launcher := filepath.Join(upgradeDir, "hangar-node")
	copyFile(t, old, launcher)

	spec := h.machine(t, "upg", driver.RestartAlways, guestScript)
	if err := h.hv.StartUnconfined(ctx, spec, []string{launcher, "vmm"}); err != nil {
		t.Fatal(err)
	}
	ready(t, "upg")
	waitReady(t, "upg", 1)
	if st := statusOf(t, childCH(t, "upg")); st["Uid"] != "0 0 0 0" {
		t.Fatalf("the VM a driver without VM users started runs as %q, not root", st["Uid"])
	}
	if err := h.hv.Guest().Ready(ctx, "upg"); err != nil {
		t.Errorf("the driver cannot reach the guest of a VM it did not start: %v", err)
	}

	copyFile(t, vmtest.Artifacts().Node, launcher+".new")
	if err := os.Rename(launcher+".new", launcher); err != nil {
		t.Fatal(err)
	}
	if err := h.hv.Guest().Ready(ctx, "upg"); err != nil {
		t.Errorf("the guest is out of reach once hangar-node is replaced: %v", err)
	}
	exit(t, "upg", 3)
	waitReady(t, "upg", 2)
	ready(t, "upg")
	if st := statusOf(t, childCH(t, "upg")); st["Uid"] != "0 0 0 0" {
		t.Errorf("restarted in its unit with no sandbox, the VM runs as %q, not root as the unit says", st["Uid"])
	}
	if err := h.hv.Guest().Ready(ctx, "upg"); err != nil {
		t.Errorf("the driver cannot reach the guest the new launcher runs: %v", err)
	}

	if err := h.hv.Stop(ctx, "upg"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdoutOf(t, "upg"), "got SIGTERM") {
		t.Error("the stop gave the container no SIGTERM")
	}
	startAndWait(t, h, spec)
	user := vmUserOf(t, "upg")
	st := statusOf(t, childCH(t, "upg"))
	if !between(user) || st["Uid"] != strings.TrimSpace(strings.Repeat(strconv.Itoa(user)+" ", 4)) || st["CapEff"] != "0000000000000000" {
		t.Errorf("started again, the VM runs as %q with CapEff %s, not as its VM user %d with none", st["Uid"], st["CapEff"], user)
	}
}
