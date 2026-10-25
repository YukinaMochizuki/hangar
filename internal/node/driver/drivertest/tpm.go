package drivertest

import (
	"bytes"
	"slices"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// tpmKeys: a TPM key put is listed, and replaced whole by the next; removed,
// it is not, and removing it again is no error. A key that is not 32
// bytes, or a uid that cannot name a machine, is refused.
func tpmKeys(t *testing.T, h Host) {
	ctx := t.Context()
	hv := h.Hypervisor
	t.Cleanup(func() { _ = hv.RemoveTPMKey(ctx, "tk1") })
	one, two := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	must(t, hv.PutTPMKey(ctx, "tk1", one))
	must(t, hv.PutTPMKey(ctx, "tk1", two))
	if got, err := hv.TPMKeys(ctx); err != nil || !slices.Contains(got, "tk1") {
		t.Errorf("TPMKeys after a put: %q %v", got, err)
	}
	for _, bad := range []struct {
		what, uid string
		key       []byte
	}{
		{"a key of 31 bytes", "tk2", one[:31]},
		{"a key of 33 bytes", "tk2", append(slices.Clone(one), 3)},
		{"no key", "tk2", nil},
		{"an empty uid", "", one},
		{"a path", "../tk2", one},
		{"a slash", "a/b", one},
		{"a space", "a b", one},
		{"a dot first", ".tk2", one},
	} {
		if err := hv.PutTPMKey(ctx, bad.uid, bad.key); err == nil {
			t.Errorf("put %s for %q", bad.what, bad.uid)
			_ = hv.RemoveTPMKey(ctx, bad.uid)
		}
	}
	if got, err := hv.TPMKeys(ctx); err != nil || !slices.Equal(got, []string{"tk1"}) {
		t.Errorf("TPMKeys after the refusals: %q %v, want tk1 alone", got, err)
	}
	if err := hv.RemoveTPMKey(ctx, "../tk1"); err == nil {
		t.Error("removed the key of a uid that cannot name a machine")
	}
	must(t, hv.RemoveTPMKey(ctx, "tk1"))
	must(t, hv.RemoveTPMKey(ctx, "tk1"))
	if got, err := hv.TPMKeys(ctx); err != nil || slices.Contains(got, "tk1") {
		t.Errorf("TPMKeys after the removal: %q %v", got, err)
	}
}

// tpmState: a firmware VM with a TPM starts only with its key put; its TPM's
// state outlives its Stop, and the key's removal, and Logs lists the
// machine while the state is there, even with no log left; DeleteLogs
// removes the state, and Logs lists the machine no more.
func tpmState(t *testing.T, h Host) {
	ctx := t.Context()
	vol, err := h.Storage.EnsureVolume(ctx, "tpm1", rootSize, "")
	must(t, err)
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	nic, err := h.Network.EnsureInterface(ctx, eth0("tpmvm"))
	must(t, err)
	t.Cleanup(func() {
		_ = h.Hypervisor.Stop(ctx, "tpmvm")
		_ = h.Hypervisor.DeleteLogs(ctx, "tpmvm")
		_ = h.Hypervisor.RemoveTPMKey(ctx, "tpmvm")
		_ = h.Network.DeleteInterfaces(ctx, "tpmvm")
		_ = h.Storage.DeleteVolume(ctx, "tpm1")
	})
	spec := driver.VMSpec{
		MachineUID: "tpmvm", Machine: "test/tpmvm", VCPUs: 1, MemoryMiB: 256,
		Disks: []driver.Volume{vol}, Firmware: true, TPM: true, Interfaces: []driver.Interface{nic},
		Restart: driver.RestartOnFailure,
	}
	if err := h.Hypervisor.Start(ctx, spec); err == nil {
		t.Error("started a VM with a TPM and no key")
		must(t, h.Hypervisor.Stop(ctx, "tpmvm"))
	}
	must(t, h.Hypervisor.PutTPMKey(ctx, "tpmvm", bytes.Repeat([]byte{7}, 32)))
	must(t, h.Hypervisor.Start(ctx, spec))
	if vm, ok := vmOf(t, h, "tpmvm"); !ok || vm.State != driver.VMRunning {
		t.Fatalf("the VM with a TPM: %+v listed %t", vm, ok)
	}
	// Made as the VM starts, which a host may finish after Start returns.
	testutil.Eventually(t, "the running VM's TPM state", func() bool { return h.HasTPMState(t, "tpmvm") })
	must(t, h.Hypervisor.Stop(ctx, "tpmvm"))
	must(t, h.Hypervisor.RemoveTPMKey(ctx, "tpmvm"))
	if !h.HasTPMState(t, "tpmvm") {
		t.Fatal("the TPM's state went with the VM's stop")
	}
	logged := func() bool {
		logs, err := h.Hypervisor.Logs(ctx)
		must(t, err)
		return slices.Contains(logs, "tpmvm")
	}
	if !logged() {
		t.Error("Logs does not list a stopped VM with a TPM state")
	}
	h.DropLog(t, "tpmvm")
	if !logged() {
		t.Error("Logs does not list a machine whose TPM state is all that is left")
	}
	must(t, h.Hypervisor.DeleteLogs(ctx, "tpmvm"))
	if h.HasTPMState(t, "tpmvm") {
		t.Error("DeleteLogs left the TPM's state")
	}
	if logged() {
		t.Error("Logs lists a machine whose log and TPM state are gone")
	}
	must(t, h.Network.DeleteInterfaces(ctx, "tpmvm"))
	must(t, h.Storage.DeleteVolume(ctx, "tpm1"))
}
