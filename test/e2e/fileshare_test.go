package e2e

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1/storagev1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// placeShare puts a file share on the world's node, as placing its server
// will.
func (w *world) placeShare(name string) {
	w.t.Helper()
	c := storagev1alpha1connect.NewFileShareServiceClient(http.DefaultClient, w.env.SystemURL)
	if _, err := c.UpdateFileShareStatus(context.Background(), connect.NewRequest(&storagev1alpha1.UpdateFileShareStatusRequest{
		Project: project, Name: name, Status: &storagev1alpha1.FileShareStatus{Node: apiservertest.Node, Size: "1Gi"},
	})); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) applyVMSharing(name string, shares ...*computev1alpha1.VirtualMachineFileShare) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.VirtualMachine, &computev1alpha1.VirtualMachine{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec: &computev1alpha1.VirtualMachineSpec{
			RootDisk: &computev1alpha1.VirtualMachineRootDisk{Source: &computev1alpha1.VirtualMachineRootDisk_FromImage{
				FromImage: &computev1alpha1.VirtualMachineImageDisk{Image: &storagev1alpha1.ImageSource{Name: "noble"}},
			}},
			FileShares: shares,
		},
	}); err != nil {
		w.t.Fatal(err)
	}
}

// TestAVirtualMachinesSharesRestartNothing: a VM declaring file shares runs
// on their node, and changing what it declares, or how, changes its machine
// and leaves its VM running.
func TestAVirtualMachinesSharesRestartNothing(t *testing.T) {
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	for _, name := range []string{"data", "logs"} {
		if _, err := w.root.Apply(context.Background(), registry.FileShare, &storagev1alpha1.FileShare{
			Metadata: &metav1.ObjectMeta{Project: project, Name: name},
			Spec:     &storagev1alpha1.FileShareSpec{Size: "1Gi"},
		}); err != nil {
			t.Fatal(err)
		}
		w.placeShare(name)
	}
	w.applyVMSharing("web", &computev1alpha1.VirtualMachineFileShare{Name: "data"})
	w.waitVMReason("web", "Running")
	uid := w.machine2(vmMachineOf("web")).GetMetadata().GetUid()
	before := w.host.Snapshot().VMs[uid]

	w.applyVMSharing("web", &computev1alpha1.VirtualMachineFileShare{Name: "data", ReadOnly: true},
		&computev1alpha1.VirtualMachineFileShare{Name: "logs"})
	testutil.Eventually(t, "the machine to declare both shares", func() bool {
		return len(w.machine2(vmMachineOf("web")).GetSpec().GetFileShares()) == 2
	})
	w.waitVMReason("web", "Running")
	if waiting := w.waitSettled(); len(waiting) > 0 {
		t.Errorf("still waiting on %v", waiting)
	}
	if now := w.host.Snapshot().VMs[uid]; now.Boots != before.Boots || !now.Started.Equal(before.Started) || now.State != driver.VMRunning {
		t.Errorf("changing the VM's shares restarted it: %+v, was %+v", now, before)
	}
	if lines := w.vmLogs("web"); slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, ":end:") }) {
		t.Errorf("the VM's log has a boot's end after its shares changed: %q", lines)
	}

	if _, err := w.root.Delete(context.Background(), registry.VirtualMachine, client.Key{Project: project, Name: "web"}); err != nil {
		t.Fatal(err)
	}
	w.deleteImage("noble")
	w.waitHostEmpty()
}

// TestAContainerNeverRunsWithoutItsShare: a container declaring a share on
// no node is held back, and nothing boots on the host; once the share is on
// the node, which cannot mount shares yet, the node refuses to boot it,
// rather than let it write to its root disk at the share's target.
func TestAContainerNeverRunsWithoutItsShare(t *testing.T) {
	w := newWorld(t)
	if _, err := w.root.Apply(context.Background(), registry.FileShare, &storagev1alpha1.FileShare{
		Metadata: &metav1.ObjectMeta{Project: project, Name: "assets"},
		Spec:     &storagev1alpha1.FileShareSpec{Size: "1Gi"},
	}); err != nil {
		t.Fatal(err)
	}
	c := container("db", "nginx")
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{{Source: "assets", SourceKind: "FileShare", Target: "/srv"}}
	w.apply(c)
	held := w.waitReason("db", "MachineHeldBack")
	if msg := ready(held).GetMessage(); !strings.Contains(msg, `file share "assets" is on no node yet`) {
		t.Errorf("Ready says %q", msg)
	}
	if waiting := w.waitSettled(); len(waiting) != 1 {
		t.Errorf("waiting on %v, want the container alone", waiting)
	}
	if vms := w.host.Snapshot().VMs; len(vms) != 0 {
		t.Errorf("the host runs %v for a held back container", vms)
	}

	w.placeShare("assets")
	refused := w.waitReason("db", "BootFailed")
	if msg := ready(refused).GetMessage(); !strings.Contains(msg, "file share assets at /srv cannot be mounted") {
		t.Errorf("Ready says %q", msg)
	}
	if vms := w.host.Snapshot().VMs; len(vms) != 0 {
		t.Errorf("the host runs %v for a container whose share it cannot mount", vms)
	}
	w.delete("db")
	w.waitHostEmpty()
}
