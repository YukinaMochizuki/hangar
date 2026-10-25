//go:build testbed

package vms

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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
// as an image without the platform's seed must.
func TestAVMsGuestReadsItsOwnSerialNumber(t *testing.T) {
	out, _ := bootWithSeed(t, "serialvm", `printf 'hangar-serial:[%s]\n' "$(cat /sys/class/dmi/id/product_serial)"`,
		func(_ *host, s *driver.VMSpec) { s.SystemSerialNumber = awkwardSerial })
	if m := regexp.MustCompile(`hangar-serial:\[(.*)\]\r?\n`).FindStringSubmatch(out); m == nil || m[1] != awkwardSerial {
		t.Errorf("the guest reads its serial number as %q, not %q", m, awkwardSerial)
	}
}

// TestAVMsGuestFindsItsDisksBySerial: each of a firmware VM's volumes with
// a serial is the guest's /dev/disk/by-id/virtio-<serial>, the volume the
// guest has at its place: one of 20 bytes, the most virtio takes, too.
func TestAVMsGuestFindsItsDisksBySerial(t *testing.T) {
	serials := []string{"disksroot", "disksseed", "782aad7135c14fc6c594"}
	script := `for s in ` + strings.Join(serials, " ") + `; do printf 'hangar-disk:%s=%s\n' "$s" "$(readlink -f /dev/disk/by-id/virtio-$s)"; done`
	out, _ := bootWithSeed(t, "disksvm", script, func(h *host, s *driver.VMSpec) {
		data, err := h.storage.EnsureVolume(context.Background(), "disksdata", 1<<30, "")
		if err != nil {
			t.Fatal(err)
		}
		s.Disks = append(s.Disks, data)
		s.SystemSerialNumber = "disksvm"
		for i := range s.Disks {
			s.Disks[i].Serial = serials[i]
		}
	})
	for i, s := range serials {
		want := fmt.Sprintf("hangar-disk:%s=/dev/vd%c", s, 'a'+i)
		if !strings.Contains(out, want+"\r\n") && !strings.Contains(out, want+"\n") {
			t.Errorf("the guest's serial console lacks %q:\n%s", want, tail(out))
		}
	}
}

// bootWithSeed boots an Ubuntu cloud image as the firmware VM uid, its root
// volume first and then a NoCloud seed volume, with spec changed by mutate
// on the host, and returns its serial console once the user-data script,
// whose output goes to it, has run, and the hypervisor running it. mutate
// must give the VM a serial number of its own: cloud-init prefers the one
// pointing it at the platform's metadata service, which this host has none
// of, to the seed volume.
func bootWithSeed(t *testing.T, uid, script string, mutate func(*host, *driver.VMSpec)) (string, *chvm.Hypervisor) {
	t.Helper()
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
	root, err := h.storage.EnsureVolume(ctx, uid+"root", 10<<30, digest)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := h.storage.EnsureVolume(ctx, uid+"seed", 64<<20, "")
	if err != nil {
		t.Fatal(err)
	}
	const done = "hangar-seed-done"
	writeSeed(t, seed.Path, map[string]string{
		"meta-data": "instance-id: " + uid + "\nlocal-hostname: " + uid + "\n",
		"user-data": "#!/bin/sh\n{\n" + script + "\necho " + done + "\n} > /dev/ttyS0 2>&1\n",
	})

	spec := h.machine(t, uid, driver.RestartOnFailure, guestScript)
	spec.Root, spec.Run, spec.Firmware = driver.Disk{}, driver.RunSpec{}, true
	spec.Disks = []driver.Volume{root, seed}
	spec.VCPUs, spec.MemoryMiB = 2, 1024
	mutate(h, &spec)
	start := time.Now()
	if err := hv.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = hv.Stop(context.Background(), uid)
		_ = hv.DeleteLogs(context.Background(), uid)
	})

	deadline := time.Now().Add(15 * time.Minute)
	for {
		out := serialOf(t, uid)
		if strings.Contains(out, done+"\r\n") || strings.Contains(out, done+"\n") {
			t.Logf("the user-data ran %v after the start", time.Since(start).Round(time.Second))
			return out, hv
		}
		if vm, ok := vmOf(t, h, uid); ok && vm.State != driver.VMRunning {
			t.Fatalf("the VM ended: %+v\n%s", vm, tail(out))
		}
		if time.Now().After(deadline) {
			dump(t, uid)
			t.Fatalf("the user-data did not run in 15 minutes:\n%s", tail(out))
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
