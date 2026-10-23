package chvm

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	sd "github.com/coreos/go-systemd/v22/dbus"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// converterSpec is a converter VM's spec, of the file and target.
func converterSpec(file, target string) driver.VMSpec {
	return driver.VMSpec{
		MachineUID: converterPrefix + "img1", Machine: "converter/img1", VCPUs: converterVCPUs, MemoryMiB: converterMemoryMiB,
		Restart: driver.RestartNever, Converter: &driver.ConverterSpec{File: file, Target: target},
	}
}

// converterDriver is a driver with a converter initramfs of its own, and
// what it gave the files it was given, by path.
func converterDriver(t *testing.T, s *fakeSystemd) (*Hypervisor, string, map[string]int) {
	t.Helper()
	initramfs := filepath.Join(t.TempDir(), "converter.cpio")
	if err := os.WriteFile(initramfs, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	given := map[string]int{}
	h := newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) {
		c.ConverterInitramfs = initramfs
		c.giveFile = func(path string, gid int) error {
			given[path] = gid
			return nil
		}
	})
	return h, initramfs, given
}

func propOf(props []sd.Property, name string) (any, bool) {
	for _, p := range props {
		if p.Name == name {
			return p.Value.Value(), true
		}
	}
	return nil, false
}

// TestAConverterVMStarts: a converter VM boots the converter initramfs
// with the file bound read-only into its unit and given to its group, no
// interface, the target its only device beside KVM's, a blank vda, a boot
// disk making it a converter, and its run bounded; it is no machine's, so
// VMs and Logs leave it out.
func TestAConverterVMStarts(t *testing.T) {
	s := newFakeSystemd()
	h, initramfs, given := converterDriver(t, s)
	file := filepath.Join(t.TempDir(), "img1")
	if err := os.WriteFile(file, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := converterSpec(file, "/dev/zvol/hangar/vmbases/customimage-img1")
	if err := h.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	uid := spec.MachineUID
	ln, err := readLaunch(h.dir(uid))
	if err != nil {
		t.Fatal(err)
	}
	if ln.Initramfs != initramfs {
		t.Errorf("the converter boots %q, want the converter initramfs %q", ln.Initramfs, initramfs)
	}
	if gid, ok := given[file]; !ok || gid != int(ln.VMUser) || gid == 0 {
		t.Errorf("the file was given to %d (%t), want the VM's group %d", gid, ok, ln.VMUser)
	}
	props := s.props[unitName(uid)]
	binds, _ := propOf(props, "BindReadOnlyPaths")
	var bound []string
	for _, b := range binds.([]bindPath) {
		bound = append(bound, b.Source)
	}
	if !slices.Contains(bound, file) || !slices.Contains(bound, initramfs) {
		t.Errorf("bound read-only %v; want the file and the converter initramfs", bound)
	}
	got, _ := propOf(props, "DeviceAllow")
	want := []pathPair{{"/dev/kvm", "rw"}, {"/dev/net/tun", "rw"}, {"/dev/zvol/hangar/vmbases/customimage-img1", "rwm"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DeviceAllow=%v, want %v", got, want)
	}
	if v, ok := propOf(props, "RuntimeMaxUSec"); !ok || v != usec(DefaultConverterRuntime) {
		t.Errorf("RuntimeMaxUSec=%v (%t), want %d", v, ok, usec(DefaultConverterRuntime))
	}
	if v, _ := propOf(props, "PrivateNetwork"); v != true {
		t.Errorf("PrivateNetwork=%v", v)
	}
	if v, _ := propOf(props, "Restart"); v != "no" {
		t.Errorf("Restart=%v, want no", v)
	}
	if _, err := os.Stat(filepath.Join(h.dir(uid), blankFile)); err != nil {
		t.Errorf("no blank vda: %v", err)
	}
	cfg := readBootDisk(t, spec)
	if cfg.GetImageConverter() == nil || cfg.GetProcess() != nil || len(cfg.GetInterfaces()) != 0 || cfg.GetFileShareServer() != nil {
		t.Errorf("boot disk %v; want a converter, no process and no interface", cfg)
	}
	args := strings.Join(chArgs(ln, h.dir(uid), 0), " ")
	disks := "--disk path=" + filepath.Join(h.dir(uid), blankFile) + ",image_type=raw,readonly=on path=" + filepath.Join(h.dir(uid), bootFile) +
		",image_type=raw,readonly=on path=" + file + ",image_type=raw,readonly=on path=" + filepath.Join(h.dir(uid), volumeNode(0)) + ",image_type=raw"
	if !strings.Contains(args, disks) || strings.Contains(args, "--net") {
		t.Errorf("Cloud Hypervisor runs with %q; want the disks %q and no network", args, disks)
	}
	if _, ok := listed(t, h, uid); ok {
		t.Error("a converter VM listed among the VMs")
	}
	if logs, err := h.Logs(t.Context()); err != nil || len(logs) != 0 {
		t.Errorf("logs %v %v; want a converter's left out", logs, err)
	}
}

// TestAConverterSpecIsRefused: a converter VM has only its file and its
// target, of the driver's own uid; a host with no converter initramfs
// starts none.
func TestAConverterSpecIsRefused(t *testing.T) {
	h, _, _ := converterDriver(t, newFakeSystemd())
	for name, change := range map[string]func(*driver.VMSpec){
		"an interface":         func(s *driver.VMSpec) { s.Interfaces = testSpec(s.MachineUID, driver.RestartNever).Interfaces },
		"a process":            func(s *driver.VMSpec) { s.Run.Command = []string{"sh"} },
		"a machine's uid":      func(s *driver.VMSpec) { s.MachineUID = "m1" },
		"a relative file":      func(s *driver.VMSpec) { s.Converter.File = "img1" },
		"a file with a comma":  func(s *driver.VMSpec) { s.Converter.File = "/tmp/a,readonly=off" },
		"a target not a zvol":  func(s *driver.VMSpec) { s.Converter.Target = "/dev/sda" },
		"a file share's share": func(s *driver.VMSpec) { s.FileShareServer = serverSpec("m1").FileShareServer },
	} {
		spec := converterSpec("/var/lib/hangar/images/uploads/img1", "")
		change(&spec)
		if err := h.Start(t.Context(), spec); err == nil {
			t.Errorf("a converter with %s started", name)
		}
	}
	plain := newDriver(t, shortDir(t), newFakeSystemd(), &fakeGuest{})
	if err := plain.Start(t.Context(), converterSpec("/var/lib/hangar/images/uploads/img1", "")); err == nil {
		t.Error("a host with no converter initramfs started a converter")
	}
}

// TestTheLauncherTakesOnlyAConvertersOwnFile: the file Cloud Hypervisor
// opens by its path is a regular file of root's, the VM's group's to read
// and no one else's.
func TestTheLauncherTakesOnlyAConvertersOwnFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "img1")
	if err := os.WriteFile(file, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	gid := os.Getegid()
	if err := checkConverterFile(file, gid); err != nil {
		t.Errorf("the file as the driver leaves it: %v", err)
	}
	for mode, why := range map[os.FileMode]string{0o644: "everyone's to read", 0o660: "the group's to write"} {
		if err := os.Chmod(file, mode); err != nil {
			t.Fatal(err)
		}
		if err := checkConverterFile(file, gid); err == nil {
			t.Errorf("a file %s taken", why)
		}
	}
	if err := os.Chmod(file, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := checkConverterFile(file, gid+1); err == nil {
		t.Error("a file of another group taken")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if err := checkConverterFile(link, gid); err == nil {
		t.Error("a link taken")
	}
}

// TestAConvertersFileIsWholeSectors: a file not of whole sectors is padded
// with zeros to them, as a disk holds; one that is stays as it is.
func TestAConvertersFileIsWholeSectors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "img1")
	for _, tt := range []struct{ size, want int64 }{{1000, 1024}, {1024, 1024}, {1, 512}} {
		if err := os.WriteFile(file, make([]byte, tt.size), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := padToSector(file); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(file); err != nil || fi.Size() != tt.want {
			t.Errorf("a file of %d bytes padded to %d, want %d", tt.size, fi.Size(), tt.want)
		}
	}
}
