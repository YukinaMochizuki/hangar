// Package drivertest is the conformance suite of the node's host drivers:
// the contract of package driver as tests. It runs against the fake in CI
// and against the real drivers on the testbed, so the fake the hermetic
// tests rely on behaves as the host does (docs/decisions/testing-strategy.md).
package drivertest

import (
	"errors"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// Host is one host's drivers, with what the suite needs to drive them. A
// part of the suite whose drivers a Host lacks is skipped, so a real driver
// runs it before the others exist.
type Host struct {
	Images     driver.Images
	Storage    driver.Storage
	Network    driver.Network
	Hypervisor driver.Hypervisor
	Guest      driver.Guest
	// Publish makes an image pullable under a reference made of name, and
	// returns that reference and the image's digest.
	Publish func(t *testing.T, name string) (ref, digest string)
	// Withdraw makes the registry stop serving ref, as when it is down.
	Withdraw func(t *testing.T, ref string)
	// Reopen returns new instances of the image and storage drivers on the
	// same host, as the agent has after it restarts.
	Reopen func(t *testing.T) (driver.Images, driver.Storage)
	// Hold keeps a root disk in use, as a running VM does, until release.
	Hold func(t *testing.T, d driver.Disk) (release func())
	// Exit ends the guest process of a running VM with code, as the
	// container's process exiting does.
	Exit func(t *testing.T, machineUID string, code int)
	// Run is what the guests run, which Exit knows how to end; zero runs
	// sleep infinity.
	Run driver.RunSpec
	// ReopenHypervisor, if set, returns a new instance of the hypervisor
	// driver on the same host, as the agent has after it restarts.
	ReopenHypervisor func(t *testing.T) driver.Hypervisor
	// AskedToStop reports whether the guest of the machine's VM, stopped,
	// was asked to stop its process before the VM went.
	AskedToStop func(t *testing.T, machineUID string) bool
}

// Run runs the suite, each test on a host from fresh.
func Run(t *testing.T, fresh func(t *testing.T) Host) {
	part := func(name string, test func(*testing.T, Host), needs func(Host) bool) {
		t.Run(name, func(t *testing.T) {
			h := fresh(t)
			if !needs(h) {
				t.Skip("the host lacks the drivers this part needs")
			}
			test(t, h)
		})
	}
	disks := func(h Host) bool { return h.Images != nil && h.Storage != nil }
	vms := func(h Host) bool {
		return disks(h) && h.Network != nil && h.Hypervisor != nil && h.Guest != nil && h.AskedToStop != nil
	}
	part("images", images, func(h Host) bool { return h.Images != nil })
	part("storage", storage, disks)
	part("root sizes", rootSizes, disks)
	part("deleting disks", deletingDisks, disks)
	part("reopening", reopening, disks)
	nets := func(h Host) bool { return h.Network != nil }
	part("network", network, nets)
	part("network changes", networkChanges, nets)
	part("hypervisor", hypervisor, vms)
	part("restart policies", restarts, vms)
	part("logs", logs, vms)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

var anon = driver.Credentials(driver.Anonymous)

func images(t *testing.T, h Host) {
	ctx := t.Context()
	ref, digest := h.Publish(t, "app:1")
	got, err := h.Images.Resolve(ctx, ref, anon)
	must(t, err)
	if got != digest {
		t.Errorf("resolved %s, published %s", got, digest)
	}
	// A reference by digest resolves to that digest; ref ends in its tag.
	pinned := ref[:strings.LastIndex(ref, ":")] + "@" + digest
	if got, err := h.Images.Resolve(ctx, pinned, anon); err != nil || got != digest {
		t.Errorf("resolving %s: %s %v", pinned, got, err)
	}
	if _, err := h.Images.Config(ctx, ref, digest, anon); err != nil {
		t.Errorf("config: %v", err)
	}
	dir := t.TempDir()
	must(t, h.Images.Unpack(ctx, ref, digest, dir, anon))
	if entries, err := os.ReadDir(dir); err != nil || len(entries) == 0 {
		t.Errorf("unpacked nothing: %v %v", entries, err)
	}
	absent := strings.Replace(ref, "app:1", "absent:1", 1)
	if _, err := h.Images.Resolve(ctx, absent, anon); !errors.Is(err, driver.ErrImageNotFound) {
		t.Errorf("resolving an image never published: %v, want not found", err)
	}

	// A pulled digest is the host's until forgotten, registry or not.
	h.Withdraw(t, ref)
	if _, err := h.Images.Config(ctx, ref, digest, anon); err != nil {
		t.Errorf("config of a pulled digest with the registry down: %v", err)
	}
	if err := h.Images.Unpack(ctx, ref, digest, t.TempDir(), anon); err != nil {
		t.Errorf("unpacking a pulled digest with the registry down: %v", err)
	}
	must(t, h.Images.Forget(ctx, digest))
	must(t, h.Images.Forget(ctx, digest))
	if _, err := h.Images.Config(ctx, ref, digest, anon); err == nil {
		t.Error("a forgotten digest answered with the registry down")
	}
}

// base makes the base of a published image.
func base(t *testing.T, h Host, name string) string {
	t.Helper()
	ref, digest := h.Publish(t, name)
	must(t, h.Storage.EnsureBase(t.Context(), digest, 0, func(dir string) error {
		return h.Images.Unpack(t.Context(), ref, digest, dir, anon)
	}))
	return digest
}

// Sizes of root disks the suite asks for, above any base it makes.
const (
	gib       = int64(1) << 30
	rootSize  = 1 * gib
	grownSize = 2 * gib
)

func storage(t *testing.T, h Host) {
	ctx := t.Context()
	// Made at once from two places, a base is made once.
	ref, digest := h.Publish(t, "app:1")
	var fills atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.Storage.EnsureBase(ctx, digest, 0, func(dir string) error {
				fills.Add(1)
				time.Sleep(50 * time.Millisecond) // both callers are in by now
				return h.Images.Unpack(ctx, ref, digest, dir, anon)
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := fills.Load(); n != 1 {
		t.Errorf("a base made at once from two places was filled %d times", n)
	}
	filled := false
	must(t, h.Storage.EnsureBase(ctx, digest, 0, func(string) error { filled = true; return nil }))
	if filled {
		t.Error("an existing base was filled again")
	}
	if bases, err := h.Storage.Bases(ctx); err != nil || len(bases) != 1 || bases[0] != digest {
		t.Errorf("bases %v %v", bases, err)
	}
	// A fill that fails leaves no base.
	_, failing := h.Publish(t, "failing:1")
	failed := errors.New("the fill failed")
	if err := h.Storage.EnsureBase(ctx, failing, 0, func(string) error { return failed }); !errors.Is(err, failed) {
		t.Errorf("a failing fill: %v, want its error", err)
	}
	if bases, _ := h.Storage.Bases(ctx); slices.Contains(bases, failing) {
		t.Error("a failed fill left a base")
	}
	// An image larger than the room left is refused before it is unpacked.
	c, err := h.Storage.Capacity(ctx)
	must(t, err)
	if err := h.Storage.EnsureBase(ctx, failing, c.Free+1, func(string) error {
		t.Error("an image larger than the room left was unpacked")
		return nil
	}); !errors.Is(err, driver.ErrNoSpace) {
		t.Errorf("an image larger than the room left: %v, want no space", err)
	}

	root, err := h.Storage.EnsureRoot(ctx, "m1", digest, rootSize)
	must(t, err)
	if root.MachineUID != "m1" || root.Digest != digest || root.Path == "" || root.Size < rootSize {
		t.Errorf("root %+v", root)
	}
	again, err := h.Storage.EnsureRoot(ctx, "m1", digest, rootSize)
	if err != nil || again != root {
		t.Errorf("ensuring the root again: %+v %v", again, err)
	}
	other := base(t, h, "app:2")
	if _, err := h.Storage.EnsureRoot(ctx, "m1", other, rootSize); err == nil {
		t.Error("a root disk of one image was taken for another")
	}
	if roots, err := h.Storage.Roots(ctx); err != nil || len(roots) != 1 || roots[0] != root {
		t.Errorf("roots %v %v", roots, err)
	}
	if err := h.Storage.DeleteBase(ctx, digest); err == nil {
		t.Error("deleted a base a root disk was cloned from")
	}

	c, err = h.Storage.Capacity(ctx)
	if err != nil || c.Total <= 0 || c.Free < 0 || c.Free > c.Total {
		t.Errorf("capacity %+v %v", c, err)
	}

	must(t, h.Storage.DeleteRoot(ctx, "m1"))
	must(t, h.Storage.DeleteRoot(ctx, "m1"))
	must(t, h.Storage.DeleteBase(ctx, digest))
	must(t, h.Storage.DeleteBase(ctx, digest))
	must(t, h.Storage.DeleteBase(ctx, other))
	if roots, _ := h.Storage.Roots(ctx); len(roots) != 0 {
		t.Errorf("roots left: %v", roots)
	}
	if bases, _ := h.Storage.Bases(ctx); len(bases) != 0 {
		t.Errorf("bases left: %v", bases)
	}
}

// rootOf returns the machine's root disk as listed.
func rootOf(t *testing.T, s driver.Storage, machine string) (driver.Disk, bool) {
	t.Helper()
	roots, err := s.Roots(t.Context())
	must(t, err)
	for _, r := range roots {
		if r.MachineUID == machine {
			return r, true
		}
	}
	return driver.Disk{}, false
}

func rootSizes(t *testing.T, h Host) {
	ctx := t.Context()
	digest := base(t, h, "app:1")

	// Below the base's size is too small, and makes nothing.
	if _, err := h.Storage.EnsureRoot(ctx, "m1", digest, 1); !errors.Is(err, driver.ErrDiskTooSmall) {
		t.Errorf("a root of one byte: %v, want too small", err)
	}
	if _, ok := rootOf(t, h.Storage, "m1"); ok {
		t.Error("a root too small was made")
	}

	root, err := h.Storage.EnsureRoot(ctx, "m1", digest, rootSize)
	must(t, err)
	// Asked smaller, it is not shrunk.
	smaller, err := h.Storage.EnsureRoot(ctx, "m1", digest, rootSize/2)
	if err != nil || smaller != root {
		t.Errorf("asked smaller: %+v %v, want %+v", smaller, err, root)
	}
	// Asked larger, it grows, and is listed so.
	grown, err := h.Storage.EnsureRoot(ctx, "m1", digest, grownSize)
	must(t, err)
	if grown.Size < grownSize || grown.Path != root.Path {
		t.Errorf("grown %+v from %+v", grown, root)
	}
	if listed, _ := rootOf(t, h.Storage, "m1"); listed != grown {
		t.Errorf("listed %+v, grown %+v", listed, grown)
	}
	// Even too small for the base, an existing root is only kept.
	if kept, err := h.Storage.EnsureRoot(ctx, "m1", digest, 1); err != nil || kept != grown {
		t.Errorf("asked one byte of a grown root: %+v %v", kept, err)
	}
	must(t, h.Storage.DeleteRoot(ctx, "m1"))
	must(t, h.Storage.DeleteBase(ctx, digest))
}

func deletingDisks(t *testing.T, h Host) {
	ctx := t.Context()
	a := base(t, h, "app:1")
	b := base(t, h, "app:2")
	// A base no root was cloned from goes at once.
	must(t, h.Storage.DeleteBase(ctx, b))

	// A base goes only once its last root has.
	var roots []driver.Disk
	for _, m := range []string{"m1", "m2", "m3"} {
		r, err := h.Storage.EnsureRoot(ctx, m, a, rootSize)
		must(t, err)
		roots = append(roots, r)
	}
	if paths := map[string]bool{roots[0].Path: true, roots[1].Path: true, roots[2].Path: true}; len(paths) != 3 {
		t.Errorf("roots share devices: %+v", roots)
	}
	must(t, h.Storage.DeleteRoot(ctx, "m2"))
	if err := h.Storage.DeleteBase(ctx, a); err == nil {
		t.Error("deleted a base two roots were cloned from")
	}

	// A root in use stays until it is let go.
	if h.Hold != nil {
		release := h.Hold(t, roots[0])
		if err := h.Storage.DeleteRoot(ctx, "m1"); err == nil {
			t.Error("deleted a root in use")
		}
		if _, ok := rootOf(t, h.Storage, "m1"); !ok {
			t.Error("a root in use is gone after a failed delete")
		}
		release()
	}
	must(t, h.Storage.DeleteRoot(ctx, "m1"))
	if err := h.Storage.DeleteBase(ctx, a); err == nil {
		t.Error("deleted a base a root was cloned from")
	}
	must(t, h.Storage.DeleteRoot(ctx, "m3"))
	must(t, h.Storage.DeleteBase(ctx, a))
	if bases, _ := h.Storage.Bases(ctx); len(bases) != 0 {
		t.Errorf("bases left: %v", bases)
	}
	// Deleting what was never made is not an error.
	must(t, h.Storage.DeleteRoot(ctx, "never"))
	_, never := h.Publish(t, "never:1")
	must(t, h.Storage.DeleteBase(ctx, never))
}

func reopening(t *testing.T, h Host) {
	if h.Reopen == nil {
		t.Skip("the host cannot reopen its drivers")
	}
	ctx := t.Context()
	ref, digest := h.Publish(t, "app:1")
	must(t, h.Storage.EnsureBase(ctx, digest, 0, func(dir string) error {
		return h.Images.Unpack(ctx, ref, digest, dir, anon)
	}))
	r1, err := h.Storage.EnsureRoot(ctx, "m1", digest, rootSize)
	must(t, err)
	r2, err := h.Storage.EnsureRoot(ctx, "m2", digest, grownSize)
	must(t, err)

	// What one instance made, another finds, the registry gone.
	h.Withdraw(t, ref)
	images, storage := h.Reopen(t)
	if bases, err := storage.Bases(ctx); err != nil || !slices.Equal(bases, []string{digest}) {
		t.Errorf("bases after reopening: %v %v", bases, err)
	}
	roots, err := storage.Roots(ctx)
	must(t, err)
	slices.SortFunc(roots, func(a, b driver.Disk) int { return strings.Compare(a.MachineUID, b.MachineUID) })
	if !slices.Equal(roots, []driver.Disk{r1, r2}) {
		t.Errorf("roots after reopening: %+v, want %+v %+v", roots, r1, r2)
	}
	if _, err := images.Config(ctx, ref, digest, anon); err != nil {
		t.Errorf("config of a pulled digest after reopening: %v", err)
	}
	must(t, storage.EnsureBase(ctx, digest, 0, func(string) error {
		t.Error("a base made before reopening was filled again")
		return nil
	}))
	if again, err := storage.EnsureRoot(ctx, "m1", digest, rootSize); err != nil || again != r1 {
		t.Errorf("ensuring a root after reopening: %+v %v", again, err)
	}
	must(t, storage.DeleteRoot(ctx, "m1"))
	must(t, storage.DeleteRoot(ctx, "m2"))
	must(t, storage.DeleteBase(ctx, digest))
	must(t, images.Forget(ctx, digest))
}

var testNet = driver.NetworkSpec{
	UID: "n1", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.0/31"),
}

func eth0(machine string) driver.Interface {
	return driver.Interface{
		MachineUID: machine, Name: "eth0", NetworkUID: testNet.UID, MAC: "02:00:00:00:00:01",
		Address: netip.MustParsePrefix("10.0.0.2/24"), Gateway: netip.MustParseAddr("10.0.0.1"),
	}
}

func network(t *testing.T, h Host) {
	ctx := t.Context()
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	if nets, err := h.Network.Networks(ctx); err != nil || len(nets) != 1 || nets[0] != testNet {
		t.Errorf("networks %v %v", nets, err)
	}
	made, err := h.Network.EnsureInterface(ctx, eth0("m1"))
	must(t, err)
	if made.Device == "" {
		t.Error("an interface without a device")
	}
	if again, err := h.Network.EnsureInterface(ctx, eth0("m1")); err != nil || again != made {
		t.Errorf("ensuring the interface again: %+v %v", again, err)
	}
	if ifs, err := h.Network.Interfaces(ctx); err != nil || len(ifs) != 1 || ifs[0] != made {
		t.Errorf("interfaces %+v %v, want them as ensured", ifs, err)
	}
	// Machines made in the same instant still get taps of their own, with
	// names Linux takes.
	other := eth0("m1-and-a-longer-uid-sharing-its-prefix")
	other.Address = netip.MustParsePrefix("10.0.0.3/24")
	second, err := h.Network.EnsureInterface(ctx, other)
	must(t, err)
	for _, d := range []string{made.Device, second.Device} {
		if len(d) > 15 {
			t.Errorf("device %q is longer than Linux allows", d)
		}
	}
	if second.Device == made.Device {
		t.Errorf("two machines share the device %q", made.Device)
	}
	must(t, h.Network.DeleteInterfaces(ctx, other.MachineUID))
	must(t, h.Network.DeleteInterfaces(ctx, "m1"))
	must(t, h.Network.DeleteInterfaces(ctx, "m1"))
	must(t, h.Network.DeleteNetwork(ctx, testNet.UID))
	must(t, h.Network.DeleteNetwork(ctx, testNet.UID))
	if ifs, _ := h.Network.Interfaces(ctx); len(ifs) != 0 {
		t.Errorf("interfaces left: %v", ifs)
	}
	if nets, _ := h.Network.Networks(ctx); len(nets) != 0 {
		t.Errorf("networks left: %v", nets)
	}
}

// networkChanges holds the host to the spec it is ensured to as the spec
// changes, and refuses interfaces no network on it can take.
func networkChanges(t *testing.T, h Host) {
	ctx := t.Context()
	other := driver.NetworkSpec{
		UID: "n2", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.2/31"),
	}
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	must(t, h.Network.EnsureNetwork(ctx, other))
	networks := func() map[string]driver.NetworkSpec {
		t.Helper()
		nets, err := h.Network.Networks(ctx)
		must(t, err)
		m := map[string]driver.NetworkSpec{}
		for _, n := range nets {
			m[n.UID] = n
		}
		return m
	}
	if nets := networks(); len(nets) != 2 || nets[testNet.UID] != testNet || nets[other.UID] != other {
		t.Errorf("two networks with the same range listed as %v", nets)
	}
	other.CIDR = netip.MustParsePrefix("10.1.0.0/16")
	other.Transit = netip.MustParsePrefix("100.64.0.4/31")
	must(t, h.Network.EnsureNetwork(ctx, other))
	if nets := networks(); nets[other.UID] != other {
		t.Errorf("a network ensured to another range and transit is listed as %+v", nets[other.UID])
	}

	nowhere := eth0("m1")
	nowhere.NetworkUID = "absent"
	if _, err := h.Network.EnsureInterface(ctx, nowhere); err == nil {
		t.Error("made an interface on a network the host does not have")
	}
	outside := eth0("m1")
	outside.Address = netip.MustParsePrefix("10.9.0.2/24")
	if _, err := h.Network.EnsureInterface(ctx, outside); err == nil {
		t.Error("made an interface with an address outside its network")
	}
	if ifs, _ := h.Network.Interfaces(ctx); len(ifs) != 0 {
		t.Errorf("refused interfaces are listed: %+v", ifs)
	}

	// An interface ensured again with another spec is listed once, as
	// ensured last, wherever that puts it.
	nic := eth0("m1")
	_, err := h.Network.EnsureInterface(ctx, nic)
	must(t, err)
	nic.MAC = "02:00:00:00:00:02"
	nic.NetworkUID = other.UID
	nic.Address = netip.MustParsePrefix("10.1.0.2/16")
	nic.Gateway = netip.MustParseAddr("10.1.0.1")
	moved, err := h.Network.EnsureInterface(ctx, nic)
	must(t, err)
	if ifs, err := h.Network.Interfaces(ctx); err != nil || len(ifs) != 1 || ifs[0] != moved {
		t.Errorf("an interface moved to another network is listed as %+v %v, want %+v", ifs, err, moved)
	}
	second := eth0("m1")
	second.Name = "eth1"
	second.MAC = "02:00:00:00:00:03"
	second, err = h.Network.EnsureInterface(ctx, second)
	must(t, err)
	if second.Device == moved.Device {
		t.Errorf("two interfaces of a machine share the device %q", moved.Device)
	}

	// A network deleted with interfaces in it takes them with it.
	must(t, h.Network.DeleteNetwork(ctx, other.UID))
	if ifs, err := h.Network.Interfaces(ctx); err != nil || len(ifs) != 1 || ifs[0] != second {
		t.Errorf("after deleting the network of one, interfaces %+v %v, want only %+v", ifs, err, second)
	}
	if nets := networks(); len(nets) != 1 || nets[testNet.UID] != testNet {
		t.Errorf("after deleting one network, %v", nets)
	}
	must(t, h.Network.DeleteInterfaces(ctx, "m1"))
	must(t, h.Network.DeleteNetwork(ctx, testNet.UID))
	if nets := networks(); len(nets) != 0 {
		t.Errorf("networks left: %v", nets)
	}
}

// vmSpec makes what a VM of machine needs and returns its spec.
func vmSpec(t *testing.T, h Host, machine string, restart driver.RestartPolicy) driver.VMSpec {
	t.Helper()
	ctx := t.Context()
	digest := base(t, h, "app:1")
	root, err := h.Storage.EnsureRoot(ctx, machine, digest, rootSize)
	must(t, err)
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	nic, err := h.Network.EnsureInterface(ctx, eth0(machine))
	must(t, err)
	run := h.Run
	if len(run.Command) == 0 {
		run = driver.RunSpec{Command: []string{"sleep", "infinity"}}
	}
	return driver.VMSpec{
		MachineUID: machine, Machine: "test/" + machine, VCPUs: 1, MemoryMiB: 256,
		Root: root, Interfaces: []driver.Interface{nic},
		Run:     run,
		Restart: restart,
	}
}

func vmOf(t *testing.T, h Host, machine string) (driver.VM, bool) {
	t.Helper()
	return vmIn(t, h.Hypervisor, machine)
}

func vmIn(t *testing.T, hv driver.Hypervisor, machine string) (driver.VM, bool) {
	t.Helper()
	vms, err := hv.VMs(t.Context())
	must(t, err)
	for _, v := range vms {
		if v.MachineUID == machine {
			return v, true
		}
	}
	return driver.VM{}, false
}

// eventually polls cond for as long as a VM may take to change state.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func hypervisor(t *testing.T, h Host) {
	ctx := t.Context()
	spec := vmSpec(t, h, "m1", driver.RestartAlways)
	must(t, h.Hypervisor.Start(ctx, spec))
	vm, ok := vmOf(t, h, "m1")
	if !ok || vm.State != driver.VMRunning || vm.SpecHash != spec.Hash() {
		t.Fatalf("VM %+v listed %t", vm, ok)
	}
	eventually(t, "the guest agent answering", func() bool { return h.Guest.Ready(ctx, "m1") == nil })

	must(t, h.Hypervisor.Start(ctx, spec))
	if again, _ := vmOf(t, h, "m1"); !again.Started.Equal(vm.Started) {
		t.Error("starting the same spec again restarted the VM")
	}
	other := spec
	other.MemoryMiB = 512
	if err := h.Hypervisor.Start(ctx, other); err == nil {
		t.Error("started another spec over a running VM")
	}
	if err := h.Storage.DeleteRoot(ctx, "m1"); err == nil {
		t.Error("deleted the root disk of a running VM")
	}
	if err := h.Network.DeleteInterfaces(ctx, "m1"); err == nil {
		t.Error("deleted the interfaces of a running VM")
	}
	if err := h.Hypervisor.DeleteLogs(ctx, "m1"); err == nil {
		t.Error("deleted the console log of a running VM")
	}

	// What the host runs is read back from it, not remembered.
	if h.ReopenHypervisor != nil {
		fresh := h.ReopenHypervisor(t)
		got, ok := vmIn(t, fresh, "m1")
		if !ok || got.SpecHash != vm.SpecHash || got.State != driver.VMRunning || !got.Started.Equal(vm.Started) {
			t.Errorf("a new instance lists %+v %t, the first listed %+v", got, ok, vm)
		}
		must(t, fresh.Start(ctx, spec))
		if err := fresh.Start(ctx, other); err == nil {
			t.Error("a new instance started another spec over a running VM")
		}
		if again, _ := vmOf(t, h, "m1"); !again.Started.Equal(vm.Started) {
			t.Error("a new instance starting the same spec restarted the VM")
		}
	}

	must(t, h.Hypervisor.Stop(ctx, "m1"))
	must(t, h.Hypervisor.Stop(ctx, "m1"))
	if _, ok := vmOf(t, h, "m1"); ok {
		t.Error("a stopped VM is still listed")
	}
	if err := h.Guest.Ready(ctx, "m1"); err == nil {
		t.Error("the guest of a stopped VM answers")
	}
	if !h.AskedToStop(t, "m1") {
		t.Error("the VM was stopped without its guest being asked to stop its process")
	}
	// Stopped, it stays down, though its policy is always.
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if vm, ok := vmOf(t, h, "m1"); ok {
			t.Fatalf("a stopped VM came back: %+v", vm)
		}
	}
	// The log outlives the VM until the machine is gone.
	if logs, err := h.Hypervisor.Logs(ctx); err != nil || !slices.Contains(logs, "m1") {
		t.Errorf("logs after the VM stopped: %v %v", logs, err)
	}
	must(t, h.Hypervisor.DeleteLogs(ctx, "m1"))
	must(t, h.Hypervisor.DeleteLogs(ctx, "m1"))
	if logs, err := h.Hypervisor.Logs(ctx); err != nil || slices.Contains(logs, "m1") {
		t.Errorf("logs after deleting them: %v %v", logs, err)
	}
	must(t, h.Network.DeleteInterfaces(ctx, "m1"))
	must(t, h.Storage.DeleteRoot(ctx, "m1"))
}

func restarts(t *testing.T, h Host) {
	ctx := t.Context()
	for _, tt := range []struct {
		name    string
		policy  driver.RestartPolicy
		code    int
		restart bool
	}{
		{"always after success", driver.RestartAlways, 0, true},
		{"always after failure", driver.RestartAlways, 3, true},
		{"on failure after success", driver.RestartOnFailure, 0, false},
		{"on failure after failure", driver.RestartOnFailure, 3, true},
		{"never after success", driver.RestartNever, 0, false},
		{"never after failure", driver.RestartNever, 3, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := vmSpec(t, h, "m1", tt.policy)
			// A failed case leaves nothing for the next.
			t.Cleanup(func() {
				_ = h.Hypervisor.Stop(ctx, "m1")
				_ = h.Hypervisor.DeleteLogs(ctx, "m1")
				_ = h.Network.DeleteInterfaces(ctx, "m1")
				_ = h.Storage.DeleteRoot(ctx, "m1")
			})
			must(t, h.Hypervisor.Start(ctx, spec))
			first, _ := vmOf(t, h, "m1")
			eventually(t, "the guest agent answering", func() bool { return h.Guest.Ready(ctx, "m1") == nil })
			for len(h.Hypervisor.Changes()) > 0 { // what starting it signalled
				<-h.Hypervisor.Changes()
			}
			h.Exit(t, "m1", tt.code)
			select {
			case <-h.Hypervisor.Changes():
			case <-time.After(30 * time.Second):
				t.Fatal("the VM's exit was never signalled")
			}
			if tt.restart {
				eventually(t, "the VM started again", func() bool {
					vm, ok := vmOf(t, h, "m1")
					return ok && vm.State == driver.VMRunning && vm.Started.After(first.Started)
				})
			} else {
				eventually(t, "the VM listed as exited", func() bool {
					vm, ok := vmOf(t, h, "m1")
					return ok && vm.State == driver.VMExited && vm.ExitCode == tt.code
				})
				if h.ReopenHypervisor != nil {
					if vm, ok := vmIn(t, h.ReopenHypervisor(t), "m1"); !ok || vm.State != driver.VMExited || vm.ExitCode != tt.code {
						t.Errorf("a new instance lists the exited VM as %+v %t", vm, ok)
					}
				}
				// Starting the same spec again runs it again.
				must(t, h.Hypervisor.Start(ctx, spec))
				if vm, ok := vmOf(t, h, "m1"); !ok || vm.State != driver.VMRunning {
					t.Errorf("an exited VM started again is listed as %+v %t", vm, ok)
				}
			}
			must(t, h.Hypervisor.Stop(ctx, "m1"))
			must(t, h.Hypervisor.DeleteLogs(ctx, "m1"))
			must(t, h.Network.DeleteInterfaces(ctx, "m1"))
			must(t, h.Storage.DeleteRoot(ctx, "m1"))
		})
	}
}
