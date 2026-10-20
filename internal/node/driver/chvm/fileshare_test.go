package chvm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/guest/bootdisk"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// serverSpec is a file share's server's spec, of machine uid.
func serverSpec(uid string) driver.VMSpec {
	s := testSpec(uid, driver.RestartAlways)
	s.Root, s.Run, s.Nameservers, s.Search = driver.Disk{}, driver.RunSpec{}, nil, nil
	s.Interfaces = slices.Clone(s.Interfaces)
	s.Interfaces[0].FileShareServer = true
	s.FileShareServer = &driver.ShareServer{
		Volume: driver.ShareVolume{ShareUID: "s1", Path: "/dev/zvol/hangar/shares/s1", Size: 1 << 30},
		Export: driver.ShareExport{Squash: driver.SquashRoot, AnonUID: 1000, AnonGID: 1001, Clients: []driver.ShareClient{
			{Address: netip.MustParseAddr("10.0.0.5")}, {Address: netip.MustParseAddr("10.0.0.6"), ReadOnly: true},
		}},
	}
	return s
}

func readBootDisk(t *testing.T, spec driver.VMSpec) *guestv1.BootConfig {
	t.Helper()
	b, err := (&Hypervisor{}).bootDisk(spec)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := bootdisk.Read(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestAServersBootDisk: a share's server's boot disk makes it the share's
// server, with the export it starts with and no process, and names its
// machine; a container's names its machine too, and mounts a share by its
// server's address alone.
func TestAServersBootDisk(t *testing.T) {
	cfg := readBootDisk(t, serverSpec("m1"))
	want := &guestv1.FileShareExport{Squash: guestv1.FileShareSquash_FILE_SHARE_SQUASH_ROOT, AnonUid: 1000, AnonGid: 1001, Clients: []*guestv1.FileShareClient{
		{Address: "10.0.0.5"}, {Address: "10.0.0.6", ReadOnly: true},
	}}
	if got := cfg.GetFileShareServer().GetExport(); !proto.Equal(got, want) {
		t.Errorf("export %v, want %v", got, want)
	}
	if cfg.GetProcess() != nil || cfg.GetMachineUid() != "m1" || len(cfg.GetVolumes()) != 0 {
		t.Errorf("process %v, machine %q, volumes %v", cfg.GetProcess(), cfg.GetMachineUid(), cfg.GetVolumes())
	}
	for squash, want := range map[driver.Squash]guestv1.FileShareSquash{
		driver.SquashNone: guestv1.FileShareSquash_FILE_SHARE_SQUASH_NONE,
		driver.SquashRoot: guestv1.FileShareSquash_FILE_SHARE_SQUASH_ROOT,
		driver.SquashAll:  guestv1.FileShareSquash_FILE_SHARE_SQUASH_ALL,
	} {
		s := serverSpec("m1")
		s.FileShareServer.Export.Squash = squash
		if got := readBootDisk(t, s).GetFileShareServer().GetExport().GetSquash(); got != want {
			t.Errorf("squash %d is %v on the boot disk, want %v", squash, got, want)
		}
	}

	ctr := testSpec("m2", driver.RestartAlways)
	ctr.Mounts = []driver.Mount{{Name: "data", Target: "/data", ReadOnly: true, Subpath: "sub", Share: netip.MustParseAddr("10.0.0.9")}}
	cfg = readBootDisk(t, ctr)
	if cfg.GetMachineUid() != "m2" || cfg.GetFileShareServer() != nil || cfg.GetProcess() == nil {
		t.Errorf("a container's boot disk: machine %q, server %v, process %v", cfg.GetMachineUid(), cfg.GetFileShareServer(), cfg.GetProcess())
	}
	wantVol := &guestv1.Volume{Name: "data", Target: "/data", ReadOnly: true, Subpath: "sub", FileShare: &guestv1.FileShareMount{Address: "10.0.0.9"}}
	if v := cfg.GetVolumes(); len(v) != 1 || !proto.Equal(v[0], wantVol) {
		t.Errorf("volumes %v, want %v", v, wantVol)
	}
}

// TestAServerStarts: a share's server starts with a blank vda of its own,
// and may open its share's zvol and nothing of a root disk's; another
// export is the same spec.
func TestAServerStarts(t *testing.T) {
	s := newFakeSystemd()
	h := newDriver(t, shortDir(t), s, &fakeGuest{})
	spec := serverSpec("m1")
	if err := h.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(h.dir("m1"), blankFile))
	if err != nil || len(b) != blankSize || bytes.ContainsFunc(b, func(r rune) bool { return r != 0 }) {
		t.Errorf("the blank disk: %d bytes, not all zero, %v", len(b), err)
	}
	got := prop(s.props["hangar-vm-m1.service"], "DeviceAllow")
	want := []pathPair{{"/dev/kvm", "rw"}, {"/dev/net/tun", "rw"}, {"/dev/zvol/hangar/shares/s1", "rwm"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DeviceAllow=%v, want %v", got, want)
	}
	other := serverSpec("m1")
	other.FileShareServer.Export = driver.ShareExport{Squash: driver.SquashNone}
	if other.Hash() != spec.Hash() {
		t.Error("the export is part of the server's hash")
	}
	grown := serverSpec("m1")
	grown.FileShareServer.Volume.Size *= 2
	if grown.Hash() == spec.Hash() {
		t.Error("a grown share is the same spec")
	}
}

// TestSpecsNoServerCanRun: a share's server has no root disk, volume,
// mount or process, and a share volume with a path; it runs on servers'
// interfaces alone, and only it does.
func TestSpecsNoServerCanRun(t *testing.T) {
	h := newDriver(t, shortDir(t), newFakeSystemd(), &fakeGuest{})
	for name, change := range map[string]func(*driver.VMSpec){
		"with a root disk": func(s *driver.VMSpec) { s.Root = testSpec("m1", driver.RestartAlways).Root },
		"with a volume":    func(s *driver.VMSpec) { s.Disks = []driver.Volume{{DiskUID: "d1", Path: "/dev/zvol/hangar/disks/d1"}} },
		"with a mount": func(s *driver.VMSpec) {
			s.Mounts = []driver.Mount{{Name: "x", Target: "/x", Share: netip.MustParseAddr("10.0.0.9")}}
		},
		"with a process":       func(s *driver.VMSpec) { s.Run = driver.RunSpec{Command: []string{"sh"}} },
		"with no share's path": func(s *driver.VMSpec) { s.FileShareServer.Volume.Path = "" },
		"with no share":        func(s *driver.VMSpec) { s.FileShareServer.Volume.ShareUID = "" },
		"on another's tap":     func(s *driver.VMSpec) { s.Interfaces[0].FileShareServer = false },
		"firmware":             func(s *driver.VMSpec) { s.Firmware = true },
		"a container on its tap": func(s *driver.VMSpec) {
			*s = testSpec("m1", driver.RestartAlways)
			s.Interfaces = serverSpec("m1").Interfaces
		},
	} {
		spec := serverSpec("m1")
		srv := *spec.FileShareServer
		spec.FileShareServer = &srv
		change(&spec)
		if err := h.Start(t.Context(), spec); err == nil {
			t.Errorf("a server %s: started", name)
		}
	}
	if exists(h.dir("m1")) {
		t.Error("a refused spec left a directory")
	}
}

// TestTheLauncherAttachesAShare: a share's server's Cloud Hypervisor gets
// its blank disk, read-only, as vda, its boot disk as vdb and its share's
// zvol, opened as a volume's is, as vdc; no root disk is opened.
func TestTheLauncherAttachesAShare(t *testing.T) {
	l, dir := launcherDir(t, "m1", "exit:0")
	ln, err := readLaunch(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := serverSpec("m1")
	ln.Spec.Root, ln.Spec.Run, ln.Spec.FileShareServer = driver.Disk{}, driver.RunSpec{}, srv.FileShareServer
	ln.Hash = ln.Spec.Hash()
	b, err := json.Marshal(ln)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, specFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, blankFile), make([]byte, blankSize), 0o600); err != nil {
		t.Fatal(err)
	}
	var opened []string
	l.OpenDisk = func(path string) (*os.File, error) {
		opened = append(opened, path)
		return os.Create(filepath.Join(l.Dir, fmt.Sprintf("dev-%d", len(opened))))
	}
	if got := runFor(t, l, t.Context(), "m1"); got != 0 {
		t.Fatalf("exited %d", got)
	}
	if want := []string{srv.FileShareServer.Volume.Path}; !slices.Equal(opened, want) {
		t.Errorf("opened %q, want %q", opened, want)
	}
	var ch struct{ Args []string }
	b, _ = os.ReadFile(filepath.Join(dir, chDir, "args.json"))
	if err := json.Unmarshal(b, &ch); err != nil {
		t.Fatal(err)
	}
	i := slices.Index(ch.Args, "--disk")
	want := []string{
		"path=" + filepath.Join(dir, blankFile) + ",image_type=raw,readonly=on",
		"path=" + filepath.Join(dir, bootFile) + ",image_type=raw,readonly=on",
		"path=" + filepath.Join(dir, volumeNode(0)) + ",image_type=raw",
	}
	if i < 0 || len(ch.Args) < i+1+len(want) || !slices.Equal(ch.Args[i+1:i+1+len(want)], want) {
		t.Errorf("Cloud Hypervisor's arguments %q, want --disk %q", ch.Args, want)
	}
	if len(ch.Args) > i+1+len(want) && ch.Args[i+1+len(want)] != "--net" {
		t.Errorf("a disk past the share: %q", ch.Args[i+1+len(want)])
	}
}
