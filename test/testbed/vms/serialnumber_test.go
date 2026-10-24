//go:build testbed

package vms

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/diskimgtest"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// A serial number Cloud Hypervisor's option parser would split, unquote or
// trim, were it not quoted.
const awkwardSerial = ` ARX,"01"[2]=x `

// TestAVMsGuestReadsItsOwnSerialNumber: a firmware VM given a serial number
// of its own boots an Ubuntu cloud image whose guest reads exactly that as
// its SMBIOS system serial number. Its cloud-init, no longer pointed at the
// platform's metadata service, takes its seed from a CIDATA disk instead,
// as an image without the platform's seed must: the user-data there prints
// what the guest reads to its serial console.
func TestAVMsGuestReadsItsOwnSerialNumber(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	in := vmtest.Artifacts()
	ch, err := exec.LookPath("cloud-hypervisor")
	if err != nil {
		t.Fatal(err)
	}
	hv, err := chvm.New(ctx, chvm.Config{
		Dir: vmDir, CloudHypervisor: ch, Kernel: in.Kernel, Initramfs: in.Initramfs, Cmdline: in.Cmdline,
		Launcher: []string{in.Node, "vmm"}, StopGrace: testGrace, Firmware: diskimgtest.Firmware(t),
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
	root, err := h.storage.EnsureVolume(ctx, "serialroot", 10<<30, digest)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := h.storage.EnsureVolume(ctx, "serialseed", 64<<20, "")
	if err != nil {
		t.Fatal(err)
	}
	writeSeed(t, seed.Path, map[string]string{
		"meta-data": "instance-id: serialvm\nlocal-hostname: serialvm\n",
		"user-data": "#!/bin/sh\nprintf 'hangar-serial:[%s]\\n' \"$(cat /sys/class/dmi/id/product_serial)\" > /dev/ttyS0\n",
	})

	spec := h.machine(t, "serialvm", driver.RestartOnFailure, guestScript)
	spec.Root, spec.Run, spec.Firmware = driver.Disk{}, driver.RunSpec{}, true
	spec.Disks = []driver.Volume{root, seed}
	spec.VCPUs, spec.MemoryMiB = 2, 1024
	spec.SystemSerialNumber = awkwardSerial
	start := time.Now()
	if err := hv.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = hv.Stop(context.Background(), "serialvm")
		_ = hv.DeleteLogs(context.Background(), "serialvm")
	})

	printed := regexp.MustCompile(`hangar-serial:\[(.*)\]\r?\n`)
	deadline := time.Now().Add(15 * time.Minute)
	for {
		if m := printed.FindStringSubmatch(serialOf(t, "serialvm")); m != nil {
			if m[1] != awkwardSerial {
				t.Errorf("the guest reads its serial number as %q, not %q", m[1], awkwardSerial)
			}
			t.Logf("the guest read %q %v after the start", m[1], time.Since(start).Round(time.Second))
			break
		}
		if vm, ok := vmOf(t, h, "serialvm"); ok && vm.State != driver.VMRunning {
			t.Fatalf("the VM ended: %+v\n%s", vm, tail(serialOf(t, "serialvm")))
		}
		if time.Now().After(deadline) {
			dump(t, "serialvm")
			t.Fatalf("the guest printed no serial number in 15 minutes:\n%s", tail(serialOf(t, "serialvm")))
		}
		time.Sleep(time.Second)
	}
}

// writeSeed makes dev a NoCloud seed: a FAT file system labelled CIDATA
// holding files.
func writeSeed(t *testing.T, dev string, files map[string]string) {
	t.Helper()
	diskimgtest.Run(t, "mkfs.vfat", "-n", "CIDATA", dev)
	dir := t.TempDir()
	diskimgtest.Run(t, "mount", dev, dir)
	defer diskimgtest.Run(t, "umount", dir)
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
