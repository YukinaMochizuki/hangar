//go:build testbed

package vms

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// TestNothingIsLeft: once every VM is stopped and every log deleted, as the
// agent's sweep does once no machine is left, the host holds no unit, no
// directory and no Cloud Hypervisor of the driver's; the decoys, units and
// files at names close to its own, are as they were.
func TestNothingIsLeft(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	running := h.machine(t, "left-running", driver.RestartAlways, guestScript)
	startAndWait(t, h, running)
	exited := h.machine(t, "left-exited", driver.RestartNever, "echo bye")
	if err := h.hv.Start(ctx, exited); err != nil {
		t.Fatal(err)
	}
	waitExited(t, h, "left-exited", time.Minute)
	stopped := h.machine(t, "left-stopped", driver.RestartAlways, guestScript)
	startAndWait(t, h, stopped)
	if err := h.hv.Stop(ctx, "left-stopped"); err != nil {
		t.Fatal(err)
	}

	units, dirs, procs := listing(t, "before")
	for _, want := range []string{"hangar-vm-left-running.service"} {
		if !strings.Contains(units, want) {
			t.Errorf("no %s before", want)
		}
	}
	for _, want := range []string{"left-running", "left-exited", "left-stopped"} {
		if !strings.Contains(dirs, filepath.Join(vmDir, want)) {
			t.Errorf("no directory of %s before", want)
		}
	}
	if !strings.Contains(procs, "left-running") {
		t.Error("no Cloud Hypervisor before")
	}

	// A new instance, as after the agent restarts, does it.
	h.removeAll(t, ctx, newHypervisor(t, testGrace))

	units, _, procs = listing(t, "after")
	for _, line := range strings.Split(units, "\n") {
		if strings.HasPrefix(line, "hangar-vm-") {
			t.Errorf("a unit left: %s", line)
		}
	}
	if entries, err := os.ReadDir(vmDir); err != nil || len(entries) != 0 {
		t.Errorf("left in %s: %v %v", vmDir, entries, err)
	}
	if strings.TrimSpace(procs) != "" {
		t.Errorf("Cloud Hypervisor left:\n%s", procs)
	}
	for _, u := range decoyUnits {
		if out, _ := exec.Command("systemctl", "is-active", u).Output(); strings.TrimSpace(string(out)) != "active" {
			t.Errorf("the decoy %s is %s", u, out)
		}
	}
	if b, err := os.ReadFile(filepath.Join(decoyDir, chvm.ConsoleLog)); err != nil || string(b) != "decoy" {
		t.Errorf("the decoy file: %q %v", b, err)
	}
}

// syncBuffer is a buffer a process writes to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestTheNodeStartsWithHostDrivers: hangar-node --drivers=host builds every
// real driver on this host and starts; no apiserver answers it here.
func TestTheNodeStartsWithHostDrivers(t *testing.T) {
	prepare(t)
	in := vmtest.Artifacts()
	ds := testParent + "/node"
	mustRun(t, "zfs", "create", "-o", "mountpoint=none", ds)
	images := filepath.Join(imageRoot, "node")
	ch, err := exec.LookPath("cloud-hypervisor")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, in.Node, "--drivers=host", "--name=testbed", "--socket=/run/hangar-testbed-absent.sock",
		"--zfs-dataset="+ds, "--image-dir="+images, "--max-unpack-work=1000000", "--max-pulls=1",
		"--cloud-hypervisor="+ch, "--guest-kernel="+in.Kernel, "--guest-initramfs="+in.Initramfs,
		"--vm-dir="+vmDir, "--uplink="+uplink(t))
	var out syncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(out.String(), "node agent starting") && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Signal(os.Interrupt)
	_ = cmd.Wait()
	t.Logf("hangar-node said:\n%s", out.String())
	if !strings.Contains(out.String(), "node agent starting") || !strings.Contains(out.String(), "drivers=host") {
		t.Error("hangar-node did not start with the host drivers")
	}
	for _, made := range []string{ds + "/bases", ds + "/roots"} {
		if err := exec.Command("zfs", "list", made).Run(); err != nil {
			t.Errorf("the storage driver did not make %s", made)
		}
	}
	for _, made := range []string{filepath.Join(images, "cache"), vmDir} {
		if _, err := os.Stat(made); err != nil {
			t.Errorf("%s: %v", made, err)
		}
	}
}
