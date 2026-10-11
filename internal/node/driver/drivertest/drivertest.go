// Package drivertest is the conformance suite of the node's host drivers:
// the contract of package driver as tests. It runs against the fake in CI
// and against the real drivers on the testbed, so the fake the hermetic
// tests rely on behaves as the host does (docs/decisions/testing-strategy.md).
package drivertest

import (
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// Host is one host's drivers, with what the suite needs to drive them.
type Host struct {
	Images     driver.Images
	Storage    driver.Storage
	Network    driver.Network
	Hypervisor driver.Hypervisor
	Guest      driver.Guest
	// Publish makes an image pullable under ref and returns its digest.
	Publish func(t *testing.T, ref string) string
	// Withdraw makes the registry stop serving ref, as when it is down.
	Withdraw func(t *testing.T, ref string)
	// Exit ends the guest process of a running VM with code, as the
	// container's process exiting does.
	Exit func(t *testing.T, machineUID string, code int)
}

// Run runs the suite, each test on a host from fresh.
func Run(t *testing.T, fresh func(t *testing.T) Host) {
	t.Run("images", func(t *testing.T) { images(t, fresh(t)) })
	t.Run("storage", func(t *testing.T) { storage(t, fresh(t)) })
	t.Run("network", func(t *testing.T) { network(t, fresh(t)) })
	t.Run("hypervisor", func(t *testing.T) { hypervisor(t, fresh(t)) })
	t.Run("restart policies", func(t *testing.T) { restarts(t, fresh(t)) })
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func images(t *testing.T, h Host) {
	ctx := t.Context()
	digest := h.Publish(t, "app:1")
	got, err := h.Images.Resolve(ctx, "app:1")
	must(t, err)
	if got != digest {
		t.Errorf("resolved %s, published %s", got, digest)
	}
	if _, err := h.Images.Config(ctx, "app:1", digest); err != nil {
		t.Errorf("config: %v", err)
	}
	dir := t.TempDir()
	must(t, h.Images.Unpack(ctx, "app:1", digest, dir))
	if entries, err := os.ReadDir(dir); err != nil || len(entries) == 0 {
		t.Errorf("unpacked nothing: %v %v", entries, err)
	}
	if _, err := h.Images.Resolve(ctx, "absent:1"); err == nil {
		t.Error("resolved an image never published")
	}

	// A pulled digest is the host's until forgotten, registry or not.
	h.Withdraw(t, "app:1")
	if _, err := h.Images.Config(ctx, "app:1", digest); err != nil {
		t.Errorf("config of a pulled digest with the registry down: %v", err)
	}
	if err := h.Images.Unpack(ctx, "app:1", digest, t.TempDir()); err != nil {
		t.Errorf("unpacking a pulled digest with the registry down: %v", err)
	}
	must(t, h.Images.Forget(ctx, digest))
	must(t, h.Images.Forget(ctx, digest))
	if _, err := h.Images.Config(ctx, "app:1", digest); err == nil {
		t.Error("a forgotten digest answered with the registry down")
	}
}

// base makes the base of a published image.
func base(t *testing.T, h Host, ref string) string {
	t.Helper()
	digest := h.Publish(t, ref)
	must(t, h.Storage.EnsureBase(t.Context(), digest, func(dir string) error {
		return h.Images.Unpack(t.Context(), ref, digest, dir)
	}))
	return digest
}

func storage(t *testing.T, h Host) {
	ctx := t.Context()
	// Made at once from two places, a base is made once.
	digest := h.Publish(t, "app:1")
	var fills atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.Storage.EnsureBase(ctx, digest, func(dir string) error {
				fills.Add(1)
				time.Sleep(50 * time.Millisecond) // both callers are in by now
				return h.Images.Unpack(ctx, "app:1", digest, dir)
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
	must(t, h.Storage.EnsureBase(ctx, digest, func(string) error { filled = true; return nil }))
	if filled {
		t.Error("an existing base was filled again")
	}
	if bases, err := h.Storage.Bases(ctx); err != nil || len(bases) != 1 || bases[0] != digest {
		t.Errorf("bases %v %v", bases, err)
	}

	root, err := h.Storage.EnsureRoot(ctx, "m1", digest)
	must(t, err)
	if root.MachineUID != "m1" || root.Digest != digest || root.Path == "" {
		t.Errorf("root %+v", root)
	}
	again, err := h.Storage.EnsureRoot(ctx, "m1", digest)
	if err != nil || again != root {
		t.Errorf("ensuring the root again: %+v %v", again, err)
	}
	other := base(t, h, "app:2")
	if _, err := h.Storage.EnsureRoot(ctx, "m1", other); err == nil {
		t.Error("a root disk of one image was taken for another")
	}
	if roots, err := h.Storage.Roots(ctx); err != nil || len(roots) != 1 || roots[0] != root {
		t.Errorf("roots %v %v", roots, err)
	}
	if err := h.Storage.DeleteBase(ctx, digest); err == nil {
		t.Error("deleted a base a root disk was cloned from")
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

// vmSpec makes what a VM of machine needs and returns its spec.
func vmSpec(t *testing.T, h Host, machine string, restart driver.RestartPolicy) driver.VMSpec {
	t.Helper()
	ctx := t.Context()
	digest := base(t, h, "app:1")
	root, err := h.Storage.EnsureRoot(ctx, machine, digest)
	must(t, err)
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	nic, err := h.Network.EnsureInterface(ctx, eth0(machine))
	must(t, err)
	return driver.VMSpec{
		MachineUID: machine, Machine: "test/" + machine, VCPUs: 1, MemoryMiB: 256,
		Root: root, Interfaces: []driver.Interface{nic},
		Run:     driver.RunSpec{Command: []string{"sleep", "infinity"}},
		Restart: restart,
	}
}

func vmOf(t *testing.T, h Host, machine string) (driver.VM, bool) {
	t.Helper()
	vms, err := h.Hypervisor.VMs(t.Context())
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

	must(t, h.Hypervisor.Stop(ctx, "m1"))
	must(t, h.Hypervisor.Stop(ctx, "m1"))
	if _, ok := vmOf(t, h, "m1"); ok {
		t.Error("a stopped VM is still listed")
	}
	if err := h.Guest.Ready(ctx, "m1"); err == nil {
		t.Error("the guest of a stopped VM answers")
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
		{"never after failure", driver.RestartNever, 3, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := vmSpec(t, h, "m1", tt.policy)
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
			}
			must(t, h.Hypervisor.Stop(ctx, "m1"))
			must(t, h.Network.DeleteInterfaces(ctx, "m1"))
			must(t, h.Storage.DeleteRoot(ctx, "m1"))
		})
	}
}
