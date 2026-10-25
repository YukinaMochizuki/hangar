package e2e

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/secrets"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

func tpmVMOf(name string, tpm *computev1alpha1.VirtualMachineTPM, runState computev1alpha1.VirtualMachineRunState) *computev1alpha1.VirtualMachine {
	return &computev1alpha1.VirtualMachine{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec: &computev1alpha1.VirtualMachineSpec{
			RootDisk: &computev1alpha1.VirtualMachineRootDisk{Source: &computev1alpha1.VirtualMachineRootDisk_FromImage{
				FromImage: &computev1alpha1.VirtualMachineImageDisk{Image: &storagev1alpha1.ImageSource{Name: "noble"}, Size: "10Gi"},
			}},
			RunState: runState,
			Tpm:      tpm,
		},
	}
}

func (w *world) applyTPMVM(name string, runState computev1alpha1.VirtualMachineRunState) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.VirtualMachine, tpmVMOf(name, &computev1alpha1.VirtualMachineTPM{Enabled: true}, runState)); err != nil {
		w.t.Fatal(err)
	}
}

// TestAVirtualMachinesTPM: a VM made with a TPM starts on its host with the
// key the apiserver made for it, which the host keeps while the VM may run
// and which no read of the VM holds. Its TPM's state is the same across a
// stop and start, the key fetched again, and goes with the VM. The key not
// to be had, the VM waits, and starts once it is. A VM without a TPM, there
// all along, is never restarted for it.
func TestAVirtualMachinesTPM(t *testing.T) {
	t.Parallel()
	keys, err := secrets.NewKeyring(secrets.NewKEK("test"))
	if err != nil {
		t.Fatal(err)
	}
	w := newIdleWorldWith(t, func(c *apiserver.Config) { c.Secrets = keys })
	var unreachable atomic.Bool
	w.wrap = func(next http.RoundTripper) http.RoundTripper {
		return failing{next: next, procedure: "/ResolveVirtualTPMKey", on: &unreachable}
	}
	w.startAgent()
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	w.applyVM("plain", 0)
	w.waitVMReason("plain", "Running")
	plainUID := w.machine2(vmMachineOf("plain")).GetMetadata().GetUid()
	plainHash := w.host.Snapshot().VMs[plainUID].SpecHash

	// The key not to be had, the VM does not start, and waits on it.
	unreachable.Store(true)
	w.applyTPMVM("web", 0)
	testutil.Eventually(w.t, "the machine with a TPM placed", func() (bool, string) {
		m, ok := get[*computev1alpha1.Machine](w, registry.Machine, vmMachineOf("web"))
		return ok && m.GetSpec().GetNode() != "", fmt.Sprintf("%v", m.GetSpec())
	})
	m := w.machine2(vmMachineOf("web"))
	uid := m.GetMetadata().GetUid()
	if !m.GetSpec().GetFirmware().GetTpm() {
		t.Fatalf("the machine of a VM with a TPM has none: %v", m.GetSpec().GetFirmware())
	}
	w.reconciled("machines", controller.Key{Project: project, Name: vmMachineOf("web")}, 2)
	if vm, ok := w.host.Snapshot().VMs[uid]; ok {
		t.Fatalf("the VM started without its TPM key: %+v", vm)
	}
	if r := vmReadyOf(w, "web"); r.GetReason() != computeapi.ReasonBootFailed {
		t.Errorf("the VM waiting on its key reads %v", r)
	}
	unreachable.Store(false)
	w.waitVMReason("web", "Running")

	vm := w.host.Snapshot().VMs[uid]
	if !vm.Spec.TPM || !vm.Spec.Firmware {
		t.Errorf("the VM on the host: %+v", vm.Spec)
	}
	key, ok := w.host.TPMKey(uid)
	if !ok || len(key) != 32 {
		t.Fatalf("the host keeps a key of %d bytes (%t)", len(key), ok)
	}
	identity, ok := w.host.TPMIdentity(uid)
	if !ok {
		t.Fatal("the VM's TPM has no state")
	}
	// No read of the VM, as its owner or as its node, holds the key.
	for who, c := range map[string]*client.Client{"its owner": w.root, "its node": w.env.AsNode()} {
		got, err := c.Get(context.Background(), registry.VirtualMachine, client.Key{Project: project, Name: "web"})
		if err != nil {
			t.Fatal(err)
		}
		if got.(*computev1alpha1.VirtualMachine).GetSpec().GetTpm().GetSealedKey() != nil {
			t.Errorf("%s reads the VM's sealed key", who)
		}
	}

	// Stopped, its key goes from the host and its state stays; started, it
	// is the same TPM, with the key fetched again.
	w.applyTPMVM("web", computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED)
	w.waitVMReason("web", computeapi.ReasonStopped)
	if _, ok := w.host.TPMKey(uid); ok {
		t.Error("the host keeps a stopped VM's TPM key")
	}
	if got, ok := w.host.TPMIdentity(uid); !ok || got != identity {
		t.Errorf("a stopped VM's TPM state: %q %t, want %q", got, ok, identity)
	}
	w.applyTPMVM("web", 0)
	w.waitVMReason("web", "Running")
	if got, ok := w.host.TPMIdentity(uid); !ok || got != identity {
		t.Errorf("after a stop and start the TPM is %q (%t), not %q", got, ok, identity)
	}
	if again, ok := w.host.TPMKey(uid); !ok || string(again) != string(key) {
		t.Error("the VM started again with another key, or none")
	}

	// The VM without a TPM was never restarted, its spec's hash its own.
	if p := w.host.Snapshot().VMs[plainUID]; p.Boots != 1 || p.Spec.TPM || p.SpecHash != plainHash {
		t.Errorf("the VM without a TPM: %d boots, TPM %t, hash %s; want 1, none, %s", p.Boots, p.Spec.TPM, p.SpecHash, plainHash)
	}

	// The host reboots: the keys in /run go, and the agent fetches the key
	// again to start the VM; the TPM is the same.
	w.stopAgent()
	w.host.Reboot()
	w.startAgent()
	w.waitVMReason("web", "Running")
	if got, _ := w.host.TPMIdentity(uid); got != identity {
		t.Errorf("after a reboot the TPM is %q, not %q", got, identity)
	}

	// spec.tpm cannot change, nor be given a source.
	_, err = w.root.Apply(context.Background(), registry.VirtualMachine, tpmVMOf("web", &computev1alpha1.VirtualMachineTPM{}, 0))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("turning the VM's TPM off: %v", err)
	}
	_, err = w.root.Apply(context.Background(), registry.VirtualMachine, tpmVMOf("plain", &computev1alpha1.VirtualMachineTPM{Enabled: true}, 0))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("giving a VM a TPM: %v", err)
	}
	_, err = w.root.Apply(context.Background(), registry.VirtualMachine, tpmVMOf("sourced",
		&computev1alpha1.VirtualMachineTPM{Enabled: true, Source: &computev1alpha1.VirtualMachineTPMSource{}}, 0))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a TPM with a source: %v", err)
	}

	// Deleted, the VM takes its TPM's state and key with it.
	for _, name := range []string{"web", "plain"} {
		if _, err := w.root.Delete(context.Background(), registry.VirtualMachine, client.Key{Project: project, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	testutil.Eventually(w.t, "the TPM's state and key gone", func() (bool, string) {
		_, kept := w.host.TPMKey(uid)
		_, state := w.host.TPMIdentity(uid)
		return !kept && !state, fmt.Sprintf("key %t, state %t", kept, state)
	})
	w.deleteImage("noble")
	w.waitHostEmpty()
}

// TestATPMNeedsAKEK: an apiserver without a key-encryption key refuses a VM
// with a TPM, and says why; one without a TPM it takes.
func TestATPMNeedsAKEK(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	_, err := w.root.Apply(context.Background(), registry.VirtualMachine, tpmVMOf("web", &computev1alpha1.VirtualMachineTPM{Enabled: true}, 0))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a TPM without a KEK: %v, want refused", err)
	}
	w.applyVM("plain", 0)
	w.waitVMReason("plain", "Running")
}

// TestTheSweepRemovesAStrayTPMKey: a TPM key the host keeps for a machine
// that is not there, as one a deleted machine's agent left, goes at the
// next sweep.
func TestTheSweepRemovesAStrayTPMKey(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	if err := w.host.Hypervisor().PutTPMKey(context.Background(), "gone-machine", make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	w.sweeps(2)
	if _, ok := w.host.TPMKey("gone-machine"); ok {
		t.Error("the sweep left a TPM key no machine accounts for")
	}
}

// TestAStoppedVMsKeyLeftBehindGoes: a TPM key left on the host for a VM
// that is stopped, as by an agent killed between the VM's stop and the
// key's removal, goes once the agent is back: its machine is still there
// and wanted stopped, with no VM to stop.
func TestAStoppedVMsKeyLeftBehindGoes(t *testing.T) {
	t.Parallel()
	keys, err := secrets.NewKeyring(secrets.NewKEK("test"))
	if err != nil {
		t.Fatal(err)
	}
	w := newIdleWorldWith(t, func(c *apiserver.Config) { c.Secrets = keys })
	w.startAgent()
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	w.applyTPMVM("web", computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED)
	w.waitVMReason("web", computeapi.ReasonStopped)
	uid := w.machine2(vmMachineOf("web")).GetMetadata().GetUid()
	w.stopAgent()
	if _, ok := w.host.Snapshot().VMs[uid]; ok {
		t.Fatal("a VM wanted stopped is on the host")
	}
	if err := w.host.Hypervisor().PutTPMKey(context.Background(), uid, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	w.startAgent()
	testutil.Eventually(w.t, "the stopped VM's key gone", func() bool {
		_, ok := w.host.TPMKey(uid)
		return !ok
	})
}

// vmReadyOf is the VM's Ready condition.
func vmReadyOf(w *world, vm string) *metav1.Condition {
	v, _ := get[*computev1alpha1.VirtualMachine](w, registry.VirtualMachine, vm)
	for _, c := range v.GetStatus().GetConditions() {
		if c.GetType() == "Ready" {
			return c
		}
	}
	return nil
}
