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
	"sync"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// resized records what GrowDisks asked Cloud Hypervisor, and fails as err
// says.
type resized struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (r *resized) resize(_ context.Context, socket, id string, size int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, filepath.Base(filepath.Dir(filepath.Dir(socket)))+" "+id+" "+itoa(size))
	return r.err
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// TestGrowDisksGrowsThemInTheRunningVM: GrowDisks asks Cloud Hypervisor,
// on the VM's API socket, for each disk that grew by its id, leaves the
// boot and the hash as they were, and lists the VM at its new sizes,
// grown; a disk that did not grow, or shrank, is not asked for, and a
// second call asks nothing.
func TestGrowDisksGrowsThemInTheRunningVM(t *testing.T) {
	r := &resized{}
	h := newDriverWith(t, shortDir(t), newFakeSystemd(), &fakeGuest{}, func(c *Config) { c.resizeDisk = r.resize })
	ctx := t.Context()
	spec := testSpec("m1", driver.RestartAlways)
	spec.Disks = []driver.Volume{
		{DiskUID: "d1", Path: "/dev/zvol/hangar/disks/d1", Size: 1 << 30},
		{DiskUID: "d2", Path: "/dev/zvol/hangar/disks/d2", Size: 1 << 30},
	}
	if err := h.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	before, _ := listed(t, h, "m1")
	grown := spec
	grown.Root.Size = 3 << 30
	grown.Disks = slices.Clone(spec.Disks)
	grown.Disks[1].Size = 2 << 30
	if ok, err := h.GrowDisks(ctx, grown); !ok || err != nil {
		t.Fatalf("GrowDisks: %v, %v", ok, err)
	}
	want := []string{"m1 disk " + itoa(3<<30), "m1 disk-2 " + itoa(2<<30)}
	if !slices.Equal(r.calls, want) {
		t.Errorf("Cloud Hypervisor was asked %q, want %q", r.calls, want)
	}
	vm, ok := listed(t, h, "m1")
	switch {
	case !ok || vm.State != driver.VMRunning:
		t.Fatalf("the VM after growing: %+v", vm)
	case vm.SpecHash != before.SpecHash || !vm.Started.Equal(before.Started):
		t.Error("growing changed the VM's hash or its boot")
	case !slices.Equal(vm.DiskSizes, grown.DiskSizes()) || !vm.Grown:
		t.Errorf("the VM lists its disks at %v, grown %v; want %v, grown", vm.DiskSizes, vm.Grown, grown.DiskSizes())
	case !grown.Runs(vm):
		t.Error("the VM does not run the grown spec")
	}
	r.calls = nil
	shrunk := grown
	shrunk.Root.Size = 1 << 30
	for _, s := range []driver.VMSpec{grown, shrunk} {
		if ok, err := h.GrowDisks(ctx, s); ok || err != nil {
			t.Errorf("GrowDisks of nothing bigger: %v, %v", ok, err)
		}
	}
	if len(r.calls) != 0 {
		t.Errorf("Cloud Hypervisor was asked %q for nothing bigger", r.calls)
	}
}

// TestGrowDisksSaysWhatFailed: a disk Cloud Hypervisor refuses keeps its
// size in the spec file, to be asked for again; a VM started before its
// disks could grow, or one not running, fails with ErrGrowsAtStart, and
// asks Cloud Hypervisor nothing.
func TestGrowDisksSaysWhatFailed(t *testing.T) {
	r := &resized{err: errors.New("block device size 1 does not match requested size 2")}
	dir := shortDir(t)
	h := newDriverWith(t, dir, newFakeSystemd(), &fakeGuest{}, func(c *Config) { c.resizeDisk = r.resize })
	ctx := t.Context()
	spec := testSpec("m1", driver.RestartAlways)
	if err := h.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	grown := spec
	grown.Root.Size = 2 << 30
	if ok, err := h.GrowDisks(ctx, grown); ok || err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("GrowDisks refused by Cloud Hypervisor: %v, %v", ok, err)
	}
	if vm, _ := listed(t, h, "m1"); vm.Grown || vm.DiskSizes[0] != 1<<30 {
		t.Errorf("a disk Cloud Hypervisor refused is listed at %v, grown %v", vm.DiskSizes, vm.Grown)
	}
	r.err = nil
	if ok, err := h.GrowDisks(ctx, grown); !ok || err != nil {
		t.Errorf("GrowDisks asked again: %v, %v", ok, err)
	}

	// As a node before Growable wrote it.
	if err := h.Start(ctx, testSpec("m2", driver.RestartAlways)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "m2", specFile)
	ln, err := readLaunch(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	ln.Growable = false
	b, _ := json.Marshal(ln)
	if strings.Contains(string(b), "Growable") {
		t.Errorf("a launch that cannot grow encodes as %s, unlike one written before it could", b)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	r.calls = nil
	old := testSpec("m2", driver.RestartAlways)
	old.Root.Size = 2 << 30
	if _, err := h.GrowDisks(ctx, old); !errors.Is(err, driver.ErrGrowsAtStart) {
		t.Errorf("GrowDisks of a VM started before it could: %v", err)
	}
	if ok, err := h.GrowDisks(ctx, testSpec("m2", driver.RestartAlways)); ok || err != nil {
		t.Errorf("GrowDisks of nothing bigger in a VM started before it could: %v, %v", ok, err)
	}
	if _, err := h.GrowDisks(ctx, testSpec("m3", driver.RestartAlways)); !errors.Is(err, driver.ErrGrowsAtStart) {
		t.Errorf("GrowDisks of a machine with no VM: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("Cloud Hypervisor was asked %q of VMs that cannot grow", r.calls)
	}
}

// TestGrowableVMsServeTheirAPIAndNameTheirDisks: a container's and a
// share's server's Cloud Hypervisor serve their API, and every disk that
// grows has the id GrowDisks names it by.
func TestGrowableVMsServeTheirAPIAndNameTheirDisks(t *testing.T) {
	ct := testSpec("m1", driver.RestartAlways)
	ct.Disks = []driver.Volume{{DiskUID: "d1", Path: "/dev/zvol/hangar/disks/d1", Size: 1 << 30}}
	fw := driver.VMSpec{MachineUID: "m3", VCPUs: 1, MemoryMiB: 512, Firmware: true, Disks: ct.Disks}
	for name, tt := range map[string]struct {
		spec driver.VMSpec
		args []string
	}{
		"container": {ct, chArgs(launch{Spec: ct}, "/d", 0)},
		"server":    {serverSpec("m2"), chArgs(launch{Spec: serverSpec("m2")}, "/d", 0)},
		"firmware":  {fw, firmwareArgs(launch{Spec: fw}, "/d", 0)},
	} {
		joined := strings.Join(tt.args, " ")
		if !strings.Contains(joined, "--api-socket path=/d/"+apiFile) {
			t.Errorf("%s: no API socket in %s", name, joined)
		}
		ids := map[string]bool{}
		for _, arg := range tt.args {
			for _, opt := range strings.Split(arg, ",") {
				if id, ok := strings.CutPrefix(opt, "id="); ok {
					ids[id] = true
				}
			}
		}
		for i := range tt.spec.DiskSizes() {
			if id, ok := diskID(tt.spec, i); ok && !ids[id] {
				t.Errorf("%s: no disk with id %s in %s", name, id, joined)
			}
		}
	}
	if _, ok := diskID(fw, 0); ok {
		t.Error("a firmware VM has a root disk to grow")
	}
}

// TestResizeDisk: Cloud Hypervisor is asked vm.resize-disk with the disk's
// id and its size in bytes, as v53's VmResizeDisk has them, and its refusal
// comes back with what it said.
func TestResizeDisk(t *testing.T) {
	socket := filepath.Join(shortDir(t), "api")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Content-Type")+" "+string(body))
		if strings.Contains(string(body), `"disk-2"`) {
			http.Error(w, "Block device size 1 does not match requested size 2", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	if err := resizeDisk(t.Context(), socket, "disk", 3<<30); err != nil {
		t.Fatal(err)
	}
	if err := resizeDisk(t.Context(), socket, "disk-2", 2); err == nil || !strings.Contains(err.Error(), "does not match requested size") {
		t.Errorf("a refused resize: %v", err)
	}
	want := []string{
		`PUT /api/v1/vm.resize-disk application/json {"id":"disk","desired_size":3221225472}`,
		`PUT /api/v1/vm.resize-disk application/json {"id":"disk-2","desired_size":2}`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("asked %q, want %q", got, want)
	}
}
