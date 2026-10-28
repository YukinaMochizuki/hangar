package chvm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
)

// TestResizeRootTellsTheDevicesSize: a running container's VM is told its
// root disk's size as the device the VM has says it, by the root disk's id,
// through its API socket; one started before it had a socket cannot be,
// and says so; a VM not running, or a firmware VM, is refused.
func TestResizeRootTellsTheDevicesSize(t *testing.T) {
	type call struct {
		socket, id string
		size       int64
	}
	var calls []call
	var sized []string
	h := newDriverWith(t, shortDir(t), newFakeSystemd(), &fakeGuest{}, func(c *Config) {
		c.resizeDisk = func(_ context.Context, socket, id string, size int64) error {
			calls = append(calls, call{socket, id, size})
			return nil
		}
		c.deviceSize = func(path string) (int64, error) {
			sized = append(sized, path)
			return 3 << 30, nil
		}
	})
	if _, err := h.ResizeRoot(t.Context(), "m1"); err == nil {
		t.Error("resized the root of a machine with no VM")
	}
	if err := h.Start(t.Context(), testSpec("m1", driver.RestartAlways)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ResizeRoot(t.Context(), "m1"); !errors.Is(err, driver.ErrNoLiveResize) {
		t.Errorf("a VM with no API socket: %v, want ErrNoLiveResize", err)
	}
	if len(calls) != 0 {
		t.Errorf("a VM with no API socket was asked %+v", calls)
	}
	dir := h.dir("m1")
	if err := os.MkdirAll(filepath.Join(dir, chDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, apiFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	size, err := h.ResizeRoot(t.Context(), "m1")
	if err != nil || size != 3<<30 {
		t.Fatalf("resized to %d, %v; want the device's 3 GiB", size, err)
	}
	want := call{filepath.Join(dir, apiFile), rootDiskID, 3 << 30}
	if len(calls) != 1 || calls[0] != want || len(sized) != 1 || sized[0] != filepath.Join(dir, diskNode) {
		t.Errorf("asked %+v of the sizes of %q; want %+v of %s", calls, sized, want, filepath.Join(dir, diskNode))
	}

	fw := firmwareDriver(t, newFakeSystemd(), func(context.Context, string) error { return nil })
	startFirmware(t, fw, "vm1")
	if _, err := fw.ResizeRoot(t.Context(), "vm1"); err == nil {
		t.Error("resized the root of a firmware VM, which has none")
	}
}

// TestResizeDisk: the size goes to Cloud Hypervisor as vm.resize-disk
// takes it; what it refuses is an error, with its words; and its socket is
// never reached through a link.
func TestResizeDisk(t *testing.T) {
	socket := filepath.Join(shortDir(t), "api")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	type asked struct {
		method, path string
		ID           string `json:"id"`
		DesiredSize  int64  `json:"desired_size"`
	}
	var got []asked
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := asked{method: r.Method, path: r.URL.Path}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &a)
		got = append(got, a)
		if a.DesiredSize != 2<<30 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`["Resize failed","Block device size 2147483648 does not match requested size"]`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	if err := resizeDisk(t.Context(), socket, "root", 2<<30); err != nil {
		t.Fatal(err)
	}
	if want := (asked{"PUT", "/api/v1/vm.resize-disk", "root", 2 << 30}); len(got) != 1 || got[0] != want {
		t.Errorf("asked %+v, want %+v", got, want)
	}
	if err := resizeDisk(t.Context(), socket, "root", 3<<30); err == nil {
		t.Error("a size Cloud Hypervisor refused is no error")
	}
	planted := filepath.Join(filepath.Dir(socket), "planted")
	if err := os.Symlink(socket, planted); err != nil {
		t.Fatal(err)
	}
	if err := resizeDisk(t.Context(), planted, "root", 2<<30); !errors.Is(err, chguest.ErrNotSocket) {
		t.Errorf("through a link: %v, want ErrNotSocket", err)
	}
	if len(got) != 2 {
		t.Errorf("the socket a link leads to was asked: %+v", got)
	}
}

// TestBlockSizeTakesOnlyABlockDevice: the size is read of a block device
// alone, not of a file or what a link leads to.
func TestBlockSizeTakesOnlyABlockDevice(t *testing.T) {
	file := filepath.Join(t.TempDir(), "disk")
	if err := os.WriteFile(file, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := blockSize(file); err == nil {
		t.Error("read the size of a regular file as a disk's")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink("/dev/null", link); err != nil {
		t.Fatal(err)
	}
	if _, err := blockSize(link); err == nil {
		t.Error("followed a link")
	}
}

// TestResizeVolumeTellsItsDevicesSize: a running container's volume is told
// the VM by its node's id, at the size its node's device has; a volume the
// launch gave no id, as a launch written before ids, cannot be, and says
// so; nor can a volume the VM has not.
func TestResizeVolumeTellsItsDevicesSize(t *testing.T) {
	type call struct {
		socket, id string
		size       int64
	}
	var calls []call
	var sized []string
	h := newDriverWith(t, shortDir(t), newFakeSystemd(), &fakeGuest{}, func(c *Config) {
		c.resizeDisk = func(_ context.Context, socket, id string, size int64) error {
			calls = append(calls, call{socket, id, size})
			return nil
		}
		c.deviceSize = func(path string) (int64, error) {
			sized = append(sized, path)
			return 5 << 30, nil
		}
	})
	spec := testSpec("m1", driver.RestartAlways)
	spec.Disks = []driver.Volume{{DiskUID: "d1", Path: "/dev/zvol/hangar/disks/d1", Size: 1 << 30}, {DiskUID: "d2", Path: "/dev/zvol/hangar/disks/d2", Size: 1 << 30}}
	if err := h.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	dir := h.dir("m1")
	if err := os.MkdirAll(filepath.Join(dir, chDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, apiFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if vm, ok := listed(t, h, "m1"); !ok || !slices.Equal(vm.DiskSizes, []int64{1 << 30, 1 << 30}) {
		t.Errorf("the VM lists its volumes as started at %v; want 1Gi each", vm.DiskSizes)
	}
	size, err := h.ResizeVolume(t.Context(), "m1", 1)
	if err != nil || size != 5<<30 {
		t.Fatalf("resized to %d, %v; want the device's 5 GiB", size, err)
	}
	want := call{filepath.Join(dir, apiFile), volumeNode(1), 5 << 30}
	if len(calls) != 1 || calls[0] != want || len(sized) != 1 || sized[0] != filepath.Join(dir, volumeNode(1)) {
		t.Errorf("asked %+v of the sizes of %q; want %+v of %s", calls, sized, want, filepath.Join(dir, volumeNode(1)))
	}
	if _, err := h.ResizeVolume(t.Context(), "m1", 2); err == nil || errors.Is(err, driver.ErrNoLiveResize) {
		t.Errorf("a volume the VM has not: %v, want an error", err)
	}

	ln, err := readLaunch(dir)
	if err != nil {
		t.Fatal(err)
	}
	ln.VolumeIDs = false
	if _, err := h.writeLaunch(dir, ln); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ResizeVolume(t.Context(), "m1", 0); !errors.Is(err, driver.ErrNoLiveResize) {
		t.Errorf("a volume of a launch with no ids: %v, want ErrNoLiveResize", err)
	}
	if len(calls) != 1 {
		t.Errorf("a volume of a launch with no ids was asked: %+v", calls)
	}
}

// TestAContainersVolumesHaveIDs: a launch that says so gives a container's
// volumes their node's name as their id in Cloud Hypervisor, and one that
// does not, none; a file share's server's share never has one.
func TestAContainersVolumesHaveIDs(t *testing.T) {
	spec := testSpec("m1", driver.RestartAlways)
	spec.Disks = []driver.Volume{{DiskUID: "d1"}, {DiskUID: "d2"}}
	with := strings.Join(chArgs(launch{Spec: spec, VolumeIDs: true}, "/d", 0), " ")
	for n := range spec.Disks {
		if want := "path=/d/" + volumeNode(n) + ",image_type=raw,id=" + volumeNode(n); !strings.Contains(with, want) {
			t.Errorf("Cloud Hypervisor's arguments %q lack %q", with, want)
		}
	}
	if without := strings.Join(chArgs(launch{Spec: spec}, "/d", 0), " "); strings.Contains(without, "id=disk-") {
		t.Errorf("a launch without ids gives its volumes some: %q", without)
	}
	srv := driver.VMSpec{MachineUID: "s1", FileShareServer: &driver.ShareServer{Volume: driver.ShareVolume{ShareUID: "f1", Path: "/dev/zvol/hangar/shares/f1"}}}
	if args := strings.Join(chArgs(launch{Spec: srv, VolumeIDs: true}, "/d", 0), " "); strings.Contains(args, "id=disk-") {
		t.Errorf("a file share's server's share has an id: %q", args)
	}
}
