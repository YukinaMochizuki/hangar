//go:build testbed

package vms

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/diskimgtest"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// serialOf is what the machine's serial console wrote, as its console log
// holds it.
func serialOf(t *testing.T, uid string) string {
	t.Helper()
	var b strings.Builder
	for _, r := range consoleOf(t, uid) {
		if r.Stream == console.Stdout {
			b.Write(r.Payload)
		}
	}
	return b.String()
}

// TestAnUbuntuVMBootsToALoginPrompt: an Ubuntu cloud image, made a volume
// by the disk image driver, boots through the firmware with its own kernel
// to a login prompt on its serial console, which its console log holds. At
// host shutdown, its unit stopped by systemd, its launcher presses its
// power button: it powers off within the shipped grace, its log says the
// platform stopped it, and the driver no longer lists it, for the agent to
// start it again, as a VirtualMachine's restart policy on-failure wants.
// It boots again to a login prompt, and the driver's stop powers it off
// the same way.
func TestAnUbuntuVMBootsToALoginPrompt(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	in := vmtest.Artifacts()
	ch, err := exec.LookPath("cloud-hypervisor")
	if err != nil {
		t.Fatal(err)
	}
	hv, err := chvm.New(ctx, chvm.Config{
		Dir: vmDir, CloudHypervisor: ch, Kernel: in.Kernel, Initramfs: in.Initramfs, Cmdline: in.Cmdline,
		Launcher: []string{in.Node, "vmm"}, StopGrace: testGrace,
		// The shipped firmware stop grace: Ubuntu must power off within it.
		Firmware: diskimgtest.Firmware(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hv.Close() })

	file := diskimgtest.Ubuntu(t)
	srv := diskimgtest.New(t)
	url, digest := srv.Serve(t, diskimgtest.UbuntuName, file)
	img, err := srv.Driver(t, filepath.Join(filepath.Dir(h.cache), "disk-images")).Fetch(ctx, url, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.storage.EnsureImageBase(ctx, digest, img.Size(), func(dev string) error { return img.WriteTo(ctx, dev) }); err != nil {
		t.Fatal(err)
	}
	_ = img.Close()
	root, err := h.storage.EnsureVolume(ctx, "ubunturoot", 10<<30, digest)
	if err != nil {
		t.Fatal(err)
	}

	// An interface on a network, as a VM has; no DHCP answers on it yet.
	spec := h.machine(t, "ubuntu", driver.RestartOnFailure, guestScript)
	spec.Root, spec.Run, spec.Firmware = driver.Disk{}, driver.RunSpec{}, true
	spec.Disks = []driver.Volume{root}
	spec.VCPUs, spec.MemoryMiB = 2, 1024
	start := time.Now()
	if err := hv.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hv.Stop(context.Background(), "ubuntu") })
	waitLogin(t, h, 1, start)
	serial := serialOf(t, "ubuntu")
	for _, line := range strings.Split(serial, "\n") {
		if strings.Contains(strings.ToLower(line), "nocloud") {
			t.Logf("cloud-init: %s", strings.TrimSpace(line))
		}
	}

	// The host shuts down: systemd stops the unit, and the launcher
	// presses the power button.
	took := interrupt(t, "ubuntu")
	poweredOff(t, "at host shutdown")
	t.Logf("powered off %v after systemd stopped its unit", took.Round(time.Second))
	if vm, ok := vmOf(t, h, "ubuntu"); ok {
		t.Errorf("after the shutdown the driver lists %+v: the agent would not start it again", vm)
	}

	// Back with the host, it boots again; the driver's stop powers it off.
	start = time.Now()
	if err := hv.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	waitLogin(t, h, 2, start)
	stopped := time.Now()
	if err := hv.Stop(ctx, "ubuntu"); err != nil {
		t.Fatal(err)
	}
	poweredOff(t, "stopped")
	t.Logf("powered off %v after its power button", time.Since(stopped).Round(time.Second))
	if err := hv.DeleteLogs(ctx, "ubuntu"); err != nil {
		t.Fatal(err)
	}
}

// waitLogin waits for the n-th login prompt on the ubuntu VM's serial
// console, for 15 minutes.
func waitLogin(t *testing.T, h *host, n int, start time.Time) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Minute)
	for strings.Count(serialOf(t, "ubuntu"), " login: ") < n {
		if vm, ok := vmOf(t, h, "ubuntu"); ok && vm.State != driver.VMRunning {
			t.Fatalf("the VM ended: %+v\n%s", vm, tail(serialOf(t, "ubuntu")))
		}
		if time.Now().After(deadline) {
			dump(t, "ubuntu")
			t.Fatalf("no login prompt %d in 15 minutes:\n%s", n, tail(serialOf(t, "ubuntu")))
		}
		time.Sleep(time.Second)
	}
	t.Logf("login prompt %d %v after the start; the serial console's end:\n%s", n, time.Since(start).Round(time.Second), tail(serialOf(t, "ubuntu")))
}

// poweredOff checks that the ubuntu VM's last boot ended with its guest
// powering off, its power button pressed by the platform.
func poweredOff(t *testing.T, when string) {
	t.Helper()
	recs := consoleOf(t, "ubuntu")
	st, err := console.ExitOf(recs[len(recs)-1])
	if err != nil || st.Code != 0 || st.Signal != driver.StoppedSignal {
		t.Errorf("%s, the boot ended with %+v %v, not a power-off the platform asked for; the serial console's end:\n%s",
			when, st, err, tail(serialOf(t, "ubuntu")))
	}
}

// tail is the last lines of s.
func tail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-40):], "\n")
}
