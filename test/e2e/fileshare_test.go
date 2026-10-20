package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/miekg/dns"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1/storagev1alpha1connect"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

func (w *world) applyShare(name, size string) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.FileShare, &storagev1alpha1.FileShare{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec:     &storagev1alpha1.FileShareSpec{Size: size},
	}); err != nil {
		w.t.Fatal(err)
	}
}

// shareOnNode waits for a file share to be on the world's node, where its
// server is placed.
func (w *world) shareOnNode(name string) {
	w.t.Helper()
	c := storagev1alpha1connect.NewFileShareServiceClient(http.DefaultClient, w.env.SystemURL)
	testutil.Eventually(w.t, "file share "+name+" on the node", func() (bool, string) {
		res, err := c.GetFileShare(context.Background(), connect.NewRequest(&storagev1alpha1.GetFileShareRequest{Project: project, Name: name}))
		if err != nil {
			w.t.Fatal(err)
		}
		return res.Msg.GetFileShare().GetStatus().GetNode() == apiservertest.Node, res.Msg.GetFileShare().GetStatus().String()
	})
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
// on their node, is in their exports, and changing what it declares, or
// how, changes its machine and the exports, and leaves its VM running.
func TestAVirtualMachinesSharesRestartNothing(t *testing.T) {
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	for _, name := range []string{"data", "logs"} {
		w.applyShare(name, "1Gi")
		w.shareOnNode(name)
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
	at := netip.MustParsePrefix(w.machine2(vmMachineOf("web")).GetSpec().GetInterfaces()[0].GetIpv4()).Addr()
	w.waitExport("data", driver.ShareExport{Squash: driver.SquashNone, AnonUID: 65534, AnonGID: 65534,
		Clients: []driver.ShareClient{{Address: at, ReadOnly: true}}})
	w.waitExport("logs", driver.ShareExport{Squash: driver.SquashNone, AnonUID: 65534, AnonGID: 65534,
		Clients: []driver.ShareClient{{Address: at}}})

	if _, err := w.root.Delete(context.Background(), registry.VirtualMachine, client.Key{Project: project, Name: "web"}); err != nil {
		t.Fatal(err)
	}
	w.deleteImage("noble")
	for _, name := range []string{"data", "logs"} {
		testutil.Eventually(t, "file share "+name+" deleted, once no machine declares it", func() (bool, string) {
			_, err := w.root.Delete(context.Background(), registry.FileShare, client.Key{Project: project, Name: name})
			return err == nil, fmt.Sprint(err)
		})
		w.shareGone(project, name)
	}
	w.waitHostEmpty()
}

// TestAContainerNeverRunsWithoutItsShare: a container declaring a share on
// no node, whose server the node has no room for, is held back, and nothing
// boots on the host; once the share is on the node, it boots once, with
// the share mounted from its server's address, never without it.
func TestAContainerNeverRunsWithoutItsShare(t *testing.T) {
	w := newWorld(t)
	w.applyShare("assets", "64Ti")
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

	w.applyShare("assets", "1Gi")
	w.shareOnNode("assets")
	w.waitReason("db", "Running")
	vm := w.host.Snapshot().VMs[w.machine("db").GetMetadata().GetUid()]
	want := driver.Mount{Name: "assets", Target: "/srv", Share: w.shareAddr("assets", "default")}
	if vm.Boots != 1 || !slices.Equal(vm.Spec.Mounts, []driver.Mount{want}) {
		t.Errorf("the container booted %d times, mounting %+v, want once, mounting %+v", vm.Boots, vm.Spec.Mounts, want)
	}
	w.delete("db")
	w.waitGone("db")
	if _, err := w.root.Delete(context.Background(), registry.FileShare, client.Key{Project: project, Name: "assets"}); err != nil {
		t.Fatal(err)
	}
	w.shareGone(project, "assets")
	w.waitHostEmpty()
}

// shareAddr is the address of a share's server on network, as the share's
// status says.
func (w *world) shareAddr(name, network string) netip.Addr {
	w.t.Helper()
	var at netip.Addr
	testutil.Eventually(w.t, "file share "+name+"'s address on "+network, func() (bool, string) {
		fs, _ := get[*storagev1alpha1.FileShare](w, registry.FileShare, name)
		for _, a := range fs.GetStatus().GetAddresses() {
			if a.GetNetwork() == network {
				at = netip.MustParseAddr(a.GetAddress())
			}
		}
		return at.IsValid(), fmt.Sprint(fs.GetStatus().GetAddresses())
	})
	return at
}

// TestARunningContainerThatAddsAShareRestartsWithIt: a running container
// that comes to declare a share on its node is started again, mounting it.
func TestARunningContainerThatAddsAShareRestartsWithIt(t *testing.T) {
	w := newWorld(t)
	w.applyShare("assets", "1Gi")
	w.shareOnNode("assets")
	w.apply(container("db", "nginx"))
	w.waitReason("db", "Running")
	uid := w.machine("db").GetMetadata().GetUid()
	before := w.host.Snapshot().VMs[uid]
	c := container("db", "nginx")
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{{Source: "assets", SourceKind: "FileShare", Target: "/srv", ReadOnly: true}}
	w.apply(c)
	w.waitReason("db", "Running")
	now := w.host.Snapshot().VMs[uid]
	want := driver.Mount{Name: "assets", Target: "/srv", ReadOnly: true, Share: w.shareAddr("assets", "default")}
	if now.Started.Equal(before.Started) || !slices.Equal(now.Spec.Mounts, []driver.Mount{want}) {
		t.Errorf("the container runs %+v from %v, was started %v; want it started again mounting %+v", now.Spec.Mounts, now.Started, before.Started, want)
	}
	w.delete("db")
	w.waitGone("db")
	if _, err := w.root.Delete(context.Background(), registry.FileShare, client.Key{Project: project, Name: "assets"}); err != nil {
		t.Fatal(err)
	}
	w.shareGone(project, "assets")
	w.waitHostEmpty()
}

// shareGone waits for a share of project p to be gone, and its server.
func (w *world) shareGone(p, name string) {
	w.t.Helper()
	testutil.Eventually(w.t, "file share "+p+"/"+name+" and its server gone", func() (bool, string) {
		_, err := w.root.Get(context.Background(), registry.FileShare, client.Key{Project: p, Name: name})
		_, merr := w.root.Get(context.Background(), registry.Machine, client.Key{Project: p, Name: "fs-" + name})
		return connect.CodeOf(err) == connect.CodeNotFound && connect.CodeOf(merr) == connect.CodeNotFound, fmt.Sprint(err, merr)
	})
}

// TestADeletedShareGoes: a share placed on the node, whose server runs on
// its volume, goes once deleted: its server is released, its volume
// destroyed, and the node says it holds nothing of the share.
func TestADeletedShareGoes(t *testing.T) {
	w := newWorld(t)
	w.applyShare("assets", "1Gi")
	w.shareOnNode("assets")
	w.waitMachine("fs-assets", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning)
	fs, _ := get[*storagev1alpha1.FileShare](w, registry.FileShare, "assets")
	if _, ok := w.host.Snapshot().ShareVolumes[fs.GetMetadata().GetUid()]; !ok {
		t.Fatal("the share's server runs with no volume of the share on the host")
	}
	if _, err := w.root.Delete(context.Background(), registry.FileShare, client.Key{Project: project, Name: "assets"}); err != nil {
		t.Fatal(err)
	}
	w.shareGone(project, "assets")
	w.waitHostEmpty()
}

// TestDeletingAProjectTakesItsPlacedShares: a project's shares on the node
// go with it, and so does the project.
func TestDeletingAProjectTakesItsPlacedShares(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.root.Apply(ctx, registry.Project, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "files"}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: apiservertest.Tenant},
	}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "a share made in the new project", func() (bool, string) {
		_, err := w.root.Apply(ctx, registry.FileShare, &storagev1alpha1.FileShare{
			Metadata: &metav1.ObjectMeta{Project: "files", Name: "assets"}, Spec: &storagev1alpha1.FileShareSpec{Size: "1Gi"},
		})
		return err == nil, fmt.Sprint(err)
	})
	testutil.Eventually(t, "the share on the node", func() (bool, string) {
		obj, err := w.root.Get(ctx, registry.FileShare, client.Key{Project: "files", Name: "assets"})
		if err != nil {
			return false, err.Error()
		}
		return obj.(*storagev1alpha1.FileShare).GetStatus().GetNode() == apiservertest.Node, fmt.Sprint(obj)
	})
	if _, err := w.root.Delete(ctx, registry.Project, client.Key{Name: "files"}); err != nil {
		t.Fatal(err)
	}
	w.shareGone("files", "assets")
	testutil.Eventually(t, "the project gone", func() (bool, string) {
		_, err := w.root.Get(ctx, registry.Project, client.Key{Name: "files"})
		return connect.CodeOf(err) == connect.CodeNotFound, fmt.Sprint(err)
	})
	w.waitHostEmpty()
}

// TestAShareServerIsNoNeighbour: a share's server on a network takes no
// name there, even its share's, which a container may have too; it is in
// no security group, so the container's default group does not let it
// in; and it is served no metadata. (Its own policy, which lets nothing in
// or out, is internal/secgroup's, and none of its firewall's rules:
// TestAShareIsServed.)
func TestAShareServerIsNoNeighbour(t *testing.T) {
	w := newIdleWorld(t)
	w.dnsUpstreams = nil
	w.startAgent()
	w.apply(container("data", "nginx"))
	w.waitReason("data", "Running")
	ctr := addrOf(w.machine("data"))
	zone := project + ".hangar.internal"
	w.answers("default", "10.0.0.1", "data."+zone, dns.RcodeSuccess, ctr)

	w.applyShare("data", "1Gi")
	w.shareOnNode("data")
	server := w.machine2("fs-data")
	at := netip.MustParsePrefix(server.GetSpec().GetInterfaces()[0].GetIpv4()).Addr()
	// Everything the server's coming changes, the network's records among
	// it, is done.
	if waiting := w.waitSettled(); len(waiting) > 0 {
		t.Errorf("still waiting on %v", waiting)
	}
	w.answers("default", "10.0.0.1", "data."+zone, dns.RcodeSuccess, ctr)
	w.answers("default", "10.0.0.1", "fs-data."+zone, dns.RcodeNameError)
	w.policyOf("data", "its default group not holding the server", func(p secgroup.Policy) bool {
		return len(p.Ingress) == 1 && !slices.Contains(p.Ingress[0].Peers, netip.PrefixFrom(at, 32))
	})
	if code, body := w.askMetadata("default", at.String(), "meta-data"); code == http.StatusOK {
		t.Errorf("the server is served metadata: %q", body)
	}
}

// shareTaken waits until the agent's fileshares controller has taken in
// the share as it is now, and has nothing left to do.
func (w *world) shareTaken(name string) {
	w.t.Helper()
	obj, err := w.root.Get(context.Background(), registry.FileShare, client.Key{Project: project, Name: name})
	if err != nil {
		w.t.Fatal(err)
	}
	rv := obj.(*storagev1alpha1.FileShare).GetMetadata().GetResourceVersion()
	testutil.Eventually(w.t, "the node's fileshares controller caught up with "+name, func() (bool, string) {
		for _, s := range w.agent.ControllerStatus() {
			if s.Name == "fileshares" {
				return s.Followed >= rv && s.Queued == 0 && s.InFlight == 0, fmt.Sprintf("%+v", s)
			}
		}
		return false, "no fileshares controller"
	})
}

// TestAShareIsReleasedAfterItsServer: a deleted share's node does not say
// it holds nothing of the share while the share's server, which holds its
// volume, is not yet released.
func TestAShareIsReleasedAfterItsServer(t *testing.T) {
	w := newWorld(t)
	w.applyShare("assets", "1Gi")
	w.shareOnNode("assets")
	w.waitMachine("fs-assets", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning)
	w.host.FailRemoval(errors.New("device busy"))
	if _, err := w.root.Delete(context.Background(), registry.FileShare, client.Key{Project: project, Name: "assets"}); err != nil {
		t.Fatal(err)
	}
	w.reconciled("machines", controller.Key{Project: project, Name: "fs-assets"}, 2)
	w.shareTaken("assets")
	fs, ok := get[*storagev1alpha1.FileShare](w, registry.FileShare, "assets")
	if !ok {
		t.Fatal("the share went before its server")
	}
	if c := conditions.Find(fs.GetStatus().GetConditions(), computeapi.FileShareReleased); c != nil {
		t.Errorf("the share is %v while its server is held", c)
	}
	if _, ok := w.host.Snapshot().ShareVolumes[fs.GetMetadata().GetUid()]; !ok {
		t.Error("the share's volume went while its server is held")
	}
	w.host.FailRemoval(nil)
	w.shareGone(project, "assets")
	w.waitHostEmpty()
}
