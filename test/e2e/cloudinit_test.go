package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// askMetadata asks the metadata service of the network for a NoCloud
// document as the machine at addr would, and returns what it answers.
func (w *world) askMetadata(network, addr, doc string) (int, string) {
	w.t.Helper()
	r := httptest.NewRequest(http.MethodGet, "http://"+driver.MetadataAddr.String()+"/nocloud/"+doc, nil)
	r.RemoteAddr = addr + ":40000"
	rec := httptest.NewRecorder()
	w.metadata.Handler(w.networkUID(network)).ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

// served waits for the machine at addr to be served doc as want.
func (w *world) served(addr, doc, want string) {
	w.t.Helper()
	w.eventually(fmt.Sprintf("%s served %s %q", addr, doc, want), func() (bool, string) {
		code, body := w.askMetadata("default", addr, doc)
		return code == http.StatusOK && body == want, fmt.Sprintf("%d %q", code, body)
	})
}

func (w *world) applyVMWith(name, userData string, keys ...string) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.VirtualMachine, &computev1alpha1.VirtualMachine{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec: &computev1alpha1.VirtualMachineSpec{
			RootDisk: &computev1alpha1.VirtualMachineRootDisk{Source: &computev1alpha1.VirtualMachineRootDisk_FromImage{
				FromImage: &computev1alpha1.VirtualMachineImageDisk{Image: &storagev1alpha1.ImageSource{Name: "noble"}},
			}},
			UserData:          userData,
			SshAuthorizedKeys: keys,
		},
	}); err != nil {
		w.t.Fatal(err)
	}
}

// TestAVirtualMachinesCloudInit: the metadata service tells each VM's guest
// its VM's uid and name, its keys as vendor-data and its user-data as
// written, and each guest only its own; a container's answers are as they
// were. New user-data is served, and does not restart the VM.
func TestAVirtualMachinesCloudInit(t *testing.T) {
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ8eq2bLz6uO0j+5GcUpS0P6S1vUfQb5n6S2K9zUj1n3 alice@laptop"
	webData := "#cloud-config\nwrite_files:\n- path: /etc/web\n  content: web\n"
	w.applyVMWith("web", webData, key)
	w.applyVMWith("db", "#!/bin/sh\necho db > /etc/db\n")
	w.apply(container("api", "nginx"))
	web := w.waitVMReason("web", "Running")
	w.waitVMReason("db", "Running")
	w.waitReason("api", "Running")
	webAddr, dbAddr := addrOf(w.machine2(vmMachineOf("web"))), addrOf(w.machine2(vmMachineOf("db")))
	api := w.machine("api")

	w.served(webAddr, "meta-data", fmt.Sprintf("instance-id: %s\nlocal-hostname: web\n", web.GetMetadata().GetUid()))
	w.served(webAddr, "user-data", webData)
	w.served(webAddr, "vendor-data", "#cloud-config\nssh_authorized_keys:\n  - \""+key+"\"\n")
	// db is told its own, not web's.
	w.served(dbAddr, "user-data", "#!/bin/sh\necho db > /etc/db\n")
	w.served(dbAddr, "vendor-data", "")
	// A container is told what it was before VMs had cloud-init.
	w.served(addrOf(api), "meta-data", fmt.Sprintf("instance-id: %s\nlocal-hostname: api\n", api.GetMetadata().GetUid()))
	w.served(addrOf(api), "user-data", "")
	w.served(addrOf(api), "vendor-data", "")

	// New user-data is served at once, the VM running on.
	uid := w.machine2(vmMachineOf("web")).GetMetadata().GetUid()
	before := w.host.Snapshot().VMs[uid]
	w.applyVMWith("web", "#cloud-config\n{}\n", key)
	w.served(webAddr, "user-data", "#cloud-config\n{}\n")
	time.Sleep(200 * time.Millisecond)
	if now := w.host.Snapshot().VMs[uid]; now.Boots != before.Boots || !now.Started.Equal(before.Started) || now.State != driver.VMRunning {
		t.Errorf("new user-data restarted the VM: %+v, was %+v", now, before)
	}
	if lines := w.vmLogs("web"); slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, ":end:") }) {
		t.Errorf("the VM's log has a boot's end after new user-data: %q", lines)
	}

	for _, name := range []string{"web", "db"} {
		if _, err := w.root.Delete(context.Background(), registry.VirtualMachine, client.Key{Project: project, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	w.delete("api")
	w.deleteImage("noble")
	w.waitHostEmpty()
}
