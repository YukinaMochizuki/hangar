package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// share reads a share of the world's project.
func (w *world) share(name string) *storagev1alpha1.FileShare {
	w.t.Helper()
	fs, ok := get[*storagev1alpha1.FileShare](w, registry.FileShare, name)
	if !ok {
		w.t.Fatalf("no file share %s", name)
	}
	return fs
}

// waitSharePhase waits for a share to be in phase.
func (w *world) waitSharePhase(name string, phase storagev1alpha1.FileSharePhase) {
	w.t.Helper()
	testutil.Eventually(w.t, fmt.Sprintf("file share %s %v", name, phase), func() (bool, string) {
		fs := w.share(name)
		return fs.GetStatus().GetPhase() == phase, fs.GetStatus().String()
	})
}

// server is a share's server's VM as the host runs it.
func (w *world) server(name string) fake.VMView {
	w.t.Helper()
	uid := w.machine2("fs-" + name).GetMetadata().GetUid()
	vm, ok := w.host.Snapshot().VMs[uid]
	if !ok {
		w.t.Fatalf("the host runs no server of share %s", name)
	}
	return vm
}

// waitExport waits for a share's server to hold export, as told by its
// node.
func (w *world) waitExport(name string, want driver.ShareExport) {
	w.t.Helper()
	uid := w.machine2("fs-" + name).GetMetadata().GetUid()
	testutil.Eventually(w.t, "file share "+name+"'s server holding its export", func() (bool, string) {
		got, ok := w.host.Export(uid)
		return ok && got.Squash == want.Squash && got.AnonUID == want.AnonUID && got.AnonGID == want.AnonGID &&
			slices.Equal(got.Clients, want.Clients), fmt.Sprintf("%+v", got)
	})
}

// sharing is a container mounting share at /data.
func sharing(name, share string, readOnly bool) *computev1alpha1.Container {
	c := container(name, "nginx")
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{{Source: share, SourceKind: "FileShare", Target: "/data", ReadOnly: readOnly}}
	return c
}

func containerAddr(w *world, name string) netip.Addr {
	return netip.MustParseAddr(addrOf(w.machine(name)))
}

// nicOf is container name's first interface on the host.
func (w *world) nicOf(name string) driver.Interface {
	w.t.Helper()
	nics := w.host.Snapshot().Interfaces[w.machine(name).GetMetadata().GetUid()]
	if len(nics) == 0 {
		w.t.Fatalf("container %s has no interface on the host", name)
	}
	return nics[0]
}

// reaches says whether the fake host's firewall lets a guest of container
// name connect to addr, where something echoes.
func (w *world) reaches(name string, to netip.AddrPort) bool {
	w.t.Helper()
	return w.reachesFrom(w.nicOf(name), to)
}

func (w *world) reachesFrom(nic driver.Interface, to netip.AddrPort) bool {
	c, err := w.host.GuestConnect(nic, to)
	if err != nil {
		return false
	}
	defer func() { _ = c.Close() }()
	b := make([]byte, 2)
	_, werr := c.Write([]byte("hi"))
	_, rerr := io.ReadFull(c, b)
	return werr == nil && rerr == nil
}

// echoBack sends back what a connection sends, as a guest's server would.
func echoBack(c net.Conn) {
	defer func() { _ = c.Close() }()
	_, _ = io.Copy(c, c)
}

// TestAShareIsServed: a share's server runs on the share's volume, at its
// size, on interfaces that are a server's, and is told its export: the
// declaring machines' addresses on the network each mounts from, read-only
// as declared, and the share's squash. The share is Available, then InUse.
// Each container mounts it from the server's address. On the host, the
// server takes its clients alone, and a client reaches it whatever its
// groups let out; a machine declaring nothing does not. A client gone, or
// the squash changed, changes the export the server holds, never
// restarting it; a server booting again by its policy is told it again.
func TestAShareIsServed(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	anon := uint32(1000)
	if _, err := w.root.Apply(ctx, registry.FileShare, &storagev1alpha1.FileShare{
		Metadata: &metav1.ObjectMeta{Project: project, Name: "data"},
		Spec:     &storagev1alpha1.FileShareSpec{Size: "2Gi", Squash: storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_ROOT, AnonUid: &anon},
	}); err != nil {
		t.Fatal(err)
	}
	w.waitSharePhase("data", storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE)
	fs := w.share("data")
	srv := w.server("data")
	vol, ok := w.host.Snapshot().ShareVolumes[fs.GetMetadata().GetUid()]
	if !ok || vol.Size != 2<<30 || srv.Spec.FileShareServer == nil || srv.Spec.FileShareServer.Volume != vol {
		t.Fatalf("the server runs %+v, the host holds %+v %t, want the share's volume of 2Gi", srv.Spec.FileShareServer, vol, ok)
	}
	if srv.Spec.Root != (driver.Disk{}) || len(srv.Spec.Disks) > 0 || len(srv.Spec.Mounts) > 0 || len(srv.Spec.Interfaces) != 1 ||
		!srv.Spec.Interfaces[0].FileShareServer || srv.Spec.VCPUs != 1 || srv.Spec.MemoryMiB != 256 {
		t.Errorf("the server's spec is %+v", srv.Spec)
	}
	w.waitExport("data", driver.ShareExport{Squash: driver.SquashRoot, AnonUID: 1000, AnonGID: 65534})

	// Its clients, one locked by a group letting out only HTTPS to one
	// address.
	w.applyGroup("locked", &networkv1alpha1.SecurityGroupSpec{Network: "default",
		Egress: []*networkv1alpha1.EgressRule{{Protocol: networkv1alpha1.SecurityGroupProtocol_SECURITY_GROUP_PROTOCOL_TCP, Ports: "443",
			To: &networkv1alpha1.SecurityGroupPeer{Peer: &networkv1alpha1.SecurityGroupPeer_Cidr{Cidr: "203.0.113.1/32"}}}}})
	w.apply(sharing("web", "data", false))
	ro := sharing("reader", "data", true)
	ro.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: "default", SecurityGroups: []string{"locked"}}}
	w.apply(ro)
	w.apply(container("other", "nginx"))
	for _, c := range []string{"web", "reader", "other"} {
		w.waitReason(c, "Running")
	}
	w.waitSharePhase("data", storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_IN_USE)
	at := w.shareAddr("data", "default")
	web, reader, other := containerAddr(w, "web"), containerAddr(w, "reader"), containerAddr(w, "other")
	clients := []driver.ShareClient{{Address: web}, {Address: reader, ReadOnly: true}}
	slices.SortFunc(clients, func(a, b driver.ShareClient) int { return a.Address.Compare(b.Address) })
	w.waitExport("data", driver.ShareExport{Squash: driver.SquashRoot, AnonUID: 1000, AnonGID: 65534, Clients: clients})
	for name, ro := range map[string]bool{"web": false, "reader": true} {
		vm := w.host.Snapshot().VMs[w.machine(name).GetMetadata().GetUid()]
		if want := []driver.Mount{{Name: "data", Target: "/data", ReadOnly: ro, Share: at}}; !slices.Equal(vm.Spec.Mounts, want) {
			t.Errorf("%s mounts %+v, want %+v", name, vm.Spec.Mounts, want)
		}
	}

	// The firewall: the server takes its clients alone.
	network := w.networkUID("default")
	w.host.ServeGuest(network, netip.AddrPortFrom(at, driver.ShareServerPort), echoBack)
	w.host.ServeGuest(network, netip.AddrPortFrom(other, driver.ShareServerPort), echoBack)
	testutil.Eventually(t, "the locked client reaching its share", func() bool {
		return w.reaches("reader", netip.AddrPortFrom(at, driver.ShareServerPort))
	})
	if !w.reaches("web", netip.AddrPortFrom(at, driver.ShareServerPort)) {
		t.Error("a client does not reach its share")
	}
	if w.reaches("other", netip.AddrPortFrom(at, driver.ShareServerPort)) {
		t.Error("a container declaring no share reaches the server")
	}
	if w.reaches("reader", netip.AddrPortFrom(other, driver.ShareServerPort)) {
		t.Error("a locked client reaches another's 2049")
	}
	if dial(w, network, netip.AddrPortFrom(at, 22)) {
		t.Error("the platform's SSH reaches the server")
	}

	// A client gone, the squash changed: told, never restarted. Its tap,
	// were it left, would reach the server no more.
	booted := w.server("data")
	readerNIC := w.nicOf("reader")
	w.host.FailRemoval(errors.New("the tap is busy"))
	w.delete("reader")
	w.waitExport("data", driver.ShareExport{Squash: driver.SquashRoot, AnonUID: 1000, AnonGID: 65534, Clients: []driver.ShareClient{{Address: web}}})
	testutil.Eventually(t, "the client gone cut off", func() bool {
		return !w.reachesFrom(readerNIC, netip.AddrPortFrom(at, driver.ShareServerPort))
	})
	w.host.FailRemoval(nil)
	w.waitGone("reader")
	fs = w.share("data")
	fs.Spec.Squash = storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_ALL
	if _, err := w.root.Apply(ctx, registry.FileShare, fs); err != nil {
		t.Fatal(err)
	}
	now := driver.ShareExport{Squash: driver.SquashAll, AnonUID: 1000, AnonGID: 65534, Clients: []driver.ShareClient{{Address: web}}}
	w.waitExport("data", now)
	if after := w.server("data"); after.Boots != booted.Boots || !after.Started.Equal(booted.Started) {
		t.Errorf("the export changing restarted the server: %d boots from %v, were %d from %v", after.Boots, after.Started, booted.Boots, booted.Started)
	}

	// Booted again by its policy, it holds its boot disk's export, and is
	// told the share's again all the same: the agent knows what a boot holds
	// only by telling it.
	uid := w.machine2("fs-data").GetMetadata().GetUid()
	calls := w.host.ExportCalls(uid)
	if err := w.host.Exit(uid, 1); err != nil {
		t.Fatal(err)
	}
	w.waitExport("data", now)
	testutil.Eventually(t, "the server booting again told its export", func() bool { return w.host.ExportCalls(uid) > calls })
	if waiting := w.waitSettled(); len(waiting) > 0 {
		t.Errorf("still waiting on %v", waiting)
	}
}

// dial says whether the network's gateway reaches addr, as the platform's
// ways in do.
func dial(w *world, network string, addr netip.AddrPort) bool {
	c, err := w.host.Network().Dial(context.Background(), network, addr)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// TestARefusedExportIsReported: a server refusing its export is reported
// not ready, saying why, and its share Pending, until it takes it.
func TestARefusedExportIsReported(t *testing.T) {
	w := newWorld(t)
	w.applyShare("data", "1Gi")
	w.waitSharePhase("data", storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE)
	uid := w.machine2("fs-data").GetMetadata().GetUid()
	w.host.FailExport(uid, errors.New("invalid_argument: the export's client 0.0.0.0"))
	w.apply(sharing("web", "data", false))
	m := w.waitMachine("fs-data", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonExportFailed)
	if msg := conditions.Find(m.GetStatus().GetConditions(), computeapi.MachineReady).GetMessage(); !strings.Contains(msg, "0.0.0.0") {
		t.Errorf("Ready says %q", msg)
	}
	w.waitSharePhase("data", storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_PENDING)
	if got, _ := w.host.Export(uid); len(got.Clients) != 0 {
		t.Errorf("the server holds %+v, though it refused it", got)
	}
	w.host.FailExport(uid, nil)
	w.waitMachine("fs-data", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning)
	w.waitSharePhase("data", storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_IN_USE)
	w.waitExport("data", driver.ShareExport{Squash: driver.SquashNone, AnonUID: 65534, AnonGID: 65534,
		Clients: []driver.ShareClient{{Address: containerAddr(w, "web")}}})
}

// TestARestartedServerServesItsCurrentExport: a server booted again by its
// restart policy, its node unable to tell it anything, serves from the
// start the export as last computed: a client added since its VM started
// is in it, and a client removed while it ran, its guest answering
// nothing, is not.
func TestARestartedServerServesItsCurrentExport(t *testing.T) {
	w := newWorld(t)
	w.applyShare("data", "1Gi")
	w.waitSharePhase("data", storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE)
	uid := w.machine2("fs-data").GetMetadata().GetUid()
	w.apply(sharing("web", "data", false))
	w.apply(sharing("reader", "data", true))
	w.waitReason("web", "Running")
	w.waitReason("reader", "Running")
	web, reader := containerAddr(w, "web"), containerAddr(w, "reader")
	both := []driver.ShareClient{{Address: web}, {Address: reader, ReadOnly: true}}
	slices.SortFunc(both, func(a, b driver.ShareClient) int { return a.Address.Compare(b.Address) })
	w.waitExport("data", driver.ShareExport{Squash: driver.SquashNone, AnonUID: 65534, AnonGID: 65534, Clients: both})
	if waiting := w.waitSettled(); len(waiting) > 0 {
		t.Fatalf("still waiting on %v", waiting)
	}
	// restart has the server booted again by its policy while its guest
	// answers nothing, and says what it serves from the start.
	restart := func() driver.ShareExport {
		t.Helper()
		before := w.server("data")
		w.host.Silence(uid, true)
		if err := w.host.Exit(uid, 1); err != nil {
			t.Fatal(err)
		}
		if after := w.server("data"); after.Boots != before.Boots+1 {
			t.Fatalf("the server booted %d times, then %d", before.Boots, after.Boots)
		}
		got, _ := w.host.Export(uid)
		return got
	}

	// Its clients came after its VM started: the boot holds them.
	if got := restart(); !slices.Equal(got.Clients, both) {
		t.Errorf("booted again, the server serves %+v, not its clients %+v", got.Clients, both)
	}

	// A client removed while the server runs but answers nothing: its next
	// boot does not serve it, though the node has told the server nothing
	// since.
	calls := w.host.ExportCalls(uid)
	w.delete("reader")
	w.waitGone("reader")
	w.waitSettled()
	if got := restart(); !slices.Equal(got.Clients, []driver.ShareClient{{Address: web}}) {
		t.Errorf("booted again after a client was removed while it answered nothing, the server serves %+v", got.Clients)
	}
	if w.host.ExportCalls(uid) != calls {
		t.Error("a silent server was told its export: the boot proves nothing")
	}
	w.host.Silence(uid, false)
	w.waitExport("data", driver.ShareExport{Squash: driver.SquashNone, AnonUID: 65534, AnonGID: 65534, Clients: []driver.ShareClient{{Address: web}}})
	if waiting := w.waitSettled(); len(waiting) > 0 {
		t.Errorf("still waiting on %v", waiting)
	}
}

// TestABootDiskIsWrittenBeforeItsServerIsTold: a server's boot disk,
// written at its start, is written again only when its export changes, and
// then before the server is told; a settled server's, never.
func TestABootDiskIsWrittenBeforeItsServerIsTold(t *testing.T) {
	w := newWorld(t)
	w.applyShare("data", "1Gi")
	w.waitSharePhase("data", storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE)
	uid := w.machine2("fs-data").GetMetadata().GetUid()
	logIs := func(what string, want ...string) {
		t.Helper()
		if waiting := w.waitSettled(); len(waiting) > 0 {
			t.Fatalf("still waiting on %v", waiting)
		}
		if got := w.host.ExportLog(uid); !slices.Equal(got, want) {
			t.Errorf("%s: the server's export was asked %q, want %q", what, got, want)
		}
	}
	logIs("started", "tell")
	w.apply(sharing("web", "data", false))
	w.waitReason("web", "Running")
	w.waitExport("data", driver.ShareExport{Squash: driver.SquashNone, AnonUID: 65534, AnonGID: 65534,
		Clients: []driver.ShareClient{{Address: containerAddr(w, "web")}}})
	logIs("a client added", "tell", "boot", "tell")
	if err := w.host.Exit(uid, 1); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the server booting again told its export", func() bool { return len(w.host.ExportLog(uid)) == 4 })
	logIs("booted again by its policy", "tell", "boot", "tell", "tell")
}

// TestABootDiskRefusingItsExportIsReported: a server whose boot disk cannot
// be given its export serves it all the same, told, but is reported not
// ready, a restart losing it, and tried again until the disk takes it.
func TestABootDiskRefusingItsExportIsReported(t *testing.T) {
	w := newWorld(t)
	w.applyShare("data", "1Gi")
	w.waitSharePhase("data", storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE)
	uid := w.machine2("fs-data").GetMetadata().GetUid()
	w.host.FailBootExport(errors.New("no space left on device"))
	w.apply(sharing("web", "data", false))
	m := w.waitMachine("fs-data", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonExportFailed)
	if msg := conditions.Find(m.GetStatus().GetConditions(), computeapi.MachineReady).GetMessage(); !strings.Contains(msg, "boot disk") ||
		!strings.Contains(msg, "no space left on device") {
		t.Errorf("Ready says %q", msg)
	}
	clients := []driver.ShareClient{{Address: containerAddr(w, "web")}}
	w.waitExport("data", driver.ShareExport{Squash: driver.SquashNone, AnonUID: 65534, AnonGID: 65534, Clients: clients})
	// Tried again, with nothing else changing.
	w.reconciled("machines", controller.Key{Project: project, Name: "fs-data"}, 1)
	w.host.FailBootExport(nil)
	w.waitMachine("fs-data", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning)
	if got, _ := w.host.BootExport(uid); !slices.Equal(got.Clients, clients) {
		t.Errorf("the boot disk holds %+v, want %+v", got.Clients, clients)
	}
}

// TestGrowingAShareRestartsItsServer: a share grown grows its volume, and
// its server starts again on it, as a grown Disk's machine does.
func TestGrowingAShareRestartsItsServer(t *testing.T) {
	w := newWorld(t)
	w.applyShare("data", "1Gi")
	w.waitSharePhase("data", storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE)
	before := w.server("data")
	w.applyShare("data", "3Gi")
	uid := w.share("data").GetMetadata().GetUid()
	testutil.Eventually(t, "the server running on the grown volume", func() (bool, string) {
		s := w.host.Snapshot()
		vm := s.VMs[before.MachineUID]
		return s.ShareVolumes[uid].Size == 3<<30 && vm.Spec.FileShareServer != nil && vm.Spec.FileShareServer.Volume.Size == 3<<30 &&
			vm.State == driver.VMRunning, fmt.Sprintf("%+v %+v", s.ShareVolumes[uid], vm.Spec.FileShareServer)
	})
	if after := w.server("data"); after.Started.Equal(before.Started) {
		t.Error("the server runs on as it was started, on a volume grown under it")
	}
}

// TestAShareVolumeNoShareAccountsForIsSwept: a share's volume of no share
// bound to the node goes at the next sweep; a share's that is stays, its
// server running or not.
func TestAShareVolumeNoShareAccountsForIsSwept(t *testing.T) {
	w := newWorld(t)
	// The server never boots: nothing on the host holds the volume.
	w.host.FailBoot("", errors.New("no room for the VM"))
	w.applyShare("data", "1Gi")
	w.shareOnNode("data")
	uid := w.share("data").GetMetadata().GetUid()
	testutil.Eventually(t, "the share's volume made", func() bool {
		_, ok := w.host.Snapshot().ShareVolumes[uid]
		return ok
	})
	w.host.LeaveShareVolume("left-behind")
	w.sweeps(2)
	vols := w.host.Snapshot().ShareVolumes
	if _, ok := vols["left-behind"]; ok {
		t.Error("a volume no share accounts for survived the sweep")
	}
	if _, ok := vols[uid]; !ok {
		t.Error("the sweep took a bound share's volume")
	}
}

// addressless strips the server addresses from the file shares the agent
// reads while strip holds, as a share whose server has none yet reads.
type addressless struct {
	next  http.RoundTripper
	strip *atomic.Bool
}

func (a addressless) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/GetFileShare") {
		// Uncompressed, so it can be read and written again.
		req = req.Clone(req.Context())
		req.Header.Del("Accept-Encoding")
	}
	res, err := a.next.RoundTrip(req)
	if err != nil || !a.strip.Load() || !strings.HasSuffix(req.URL.Path, "/GetFileShare") || res.StatusCode != http.StatusOK {
		return res, err
	}
	b, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil {
		return nil, err
	}
	var msg storagev1alpha1.GetFileShareResponse
	if err := proto.Unmarshal(b, &msg); err != nil {
		return nil, err
	}
	if msg.GetFileShare().GetStatus() != nil {
		msg.FileShare.Status.Addresses = nil
	}
	if b, err = proto.Marshal(&msg); err != nil {
		return nil, err
	}
	res.Body, res.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
	res.Header.Del("Content-Length")
	return res, nil
}

// TestAContainerWaitsForItsSharesAddress: a container whose share's server
// has no address on its network yet, as its node reads the share, is
// refused and never booted; once it has one, it boots mounting it.
func TestAContainerWaitsForItsSharesAddress(t *testing.T) {
	w := newIdleWorld(t)
	var strip atomic.Bool
	strip.Store(true)
	w.wrap = func(next http.RoundTripper) http.RoundTripper { return addressless{next: next, strip: &strip} }
	w.startAgent()
	w.applyShare("data", "1Gi")
	w.waitSharePhase("data", storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE)
	w.apply(sharing("web", "data", false))
	refused := w.waitMachine("ct-web", computev1alpha1.MachinePhase_MACHINE_PHASE_CREATING, computeapi.ReasonBootFailed)
	if msg := conditions.Find(refused.GetStatus().GetConditions(), computeapi.MachineReady).GetMessage(); !strings.Contains(msg, "has no address on network \"default\" yet") {
		t.Errorf("Ready says %q", msg)
	}
	if _, ok := w.host.Snapshot().VMs[w.machine("web").GetMetadata().GetUid()]; ok {
		t.Error("the host runs a container whose share has no address")
	}
	strip.Store(false)
	w.waitReason("web", "Running")
	vm := w.host.Snapshot().VMs[w.machine("web").GetMetadata().GetUid()]
	if want := []driver.Mount{{Name: "data", Target: "/data", Share: w.shareAddr("data", "default")}}; vm.Boots != 1 || !slices.Equal(vm.Spec.Mounts, want) {
		t.Errorf("the container booted %d times mounting %+v, want once mounting %+v", vm.Boots, vm.Spec.Mounts, want)
	}
}

// TestAMachineNotItsSharesServerGetsNothing: a machine naming a share it
// is not the server of, placed on the share's node, is refused, and gets
// neither the share's volume nor a server's interface.
func TestAMachineNotItsSharesServerGetsNothing(t *testing.T) {
	w := newWorld(t)
	w.applyShare("data", "1Gi")
	w.waitSharePhase("data", storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE)
	w.applyMachine(&computev1alpha1.Machine{
		Metadata: &metav1.ObjectMeta{Project: project, Name: "impostor"},
		Spec: &computev1alpha1.MachineSpec{
			Node:          apiservertest.Node,
			RunState:      computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING,
			RestartPolicy: computev1alpha1.RestartPolicy_RESTART_POLICY_ALWAYS,
			Resources:     &computev1alpha1.Resources{Cpu: "1", Memory: "256Mi"},
			Boot: &computev1alpha1.MachineSpec_FileShareServer{FileShareServer: &computev1alpha1.FileShareServerBoot{
				FileShare: "data", Size: "1Gi",
			}},
			Interfaces: []*computev1alpha1.MachineInterface{{Name: "eth0", Network: "default"}},
		},
	})
	m := w.waitMachine("impostor", computev1alpha1.MachinePhase_MACHINE_PHASE_CREATING, computeapi.ReasonBootFailed)
	if msg := conditions.Find(m.GetStatus().GetConditions(), computeapi.MachineReady).GetMessage(); !strings.Contains(msg, "not file share data's server") {
		t.Errorf("Ready says %q", msg)
	}
	s := w.host.Snapshot()
	if _, ok := s.VMs[m.GetMetadata().GetUid()]; ok {
		t.Error("the host runs the impostor")
	}
	if nics := s.Interfaces[m.GetMetadata().GetUid()]; len(nics) > 0 {
		t.Errorf("the impostor has interfaces %+v", nics)
	}
}

// TestARunningContainerStopsWhenItsShareCannotBeMounted: a running
// container whose share loses its server's address on the network it
// mounts from, as its node reads the share, has its VM stopped and is
// refused: it never runs on without its share mounted.
func TestARunningContainerStopsWhenItsShareCannotBeMounted(t *testing.T) {
	w := newIdleWorld(t)
	var strip atomic.Bool
	w.wrap = func(next http.RoundTripper) http.RoundTripper { return addressless{next: next, strip: &strip} }
	w.startAgent()
	w.applyShare("data", "1Gi")
	w.waitSharePhase("data", storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE)
	w.apply(sharing("web", "data", false))
	w.waitReason("web", "Running")
	uid := w.machine("web").GetMetadata().GetUid()
	if _, ok := w.host.Snapshot().VMs[uid]; !ok {
		t.Fatal("the host runs no VM for the container")
	}

	strip.Store(true)
	// A change to the share has its node read it again.
	fs := w.share("data")
	fs.Spec.Squash = storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_ROOT
	if _, err := w.root.Apply(context.Background(), registry.FileShare, fs); err != nil {
		t.Fatal(err)
	}
	m := w.waitMachine("ct-web", computev1alpha1.MachinePhase_MACHINE_PHASE_CREATING, computeapi.ReasonBootFailed)
	if msg := conditions.Find(m.GetStatus().GetConditions(), computeapi.MachineReady).GetMessage(); !strings.Contains(msg, "has no address on network \"default\" yet") {
		t.Errorf("Ready says %q", msg)
	}
	testutil.Eventually(t, "the container's VM stopped", func() (bool, string) {
		vm, ok := w.host.Snapshot().VMs[uid]
		return !ok, fmt.Sprintf("%+v", vm.VM)
	})
}

// TestAContainerBootsOnlyOnceItsShareServesIt: a share and a container
// mounting it, made together, the share's server refusing every export:
// the container waits, saying so, and is never booted, nor failed though
// it never restarts; once the server takes an export holding it, it boots
// once.
func TestAContainerBootsOnlyOnceItsShareServesIt(t *testing.T) {
	w := newWorld(t)
	w.host.FailExport("", errors.New("the server is not ready"))
	w.applyShare("data", "1Gi")
	c := sharing("web", "data", false)
	c.Spec.RestartPolicy = computev1alpha1.RestartPolicy_RESTART_POLICY_NEVER
	w.apply(c)
	waiting := w.waitMachine("ct-web", computev1alpha1.MachinePhase_MACHINE_PHASE_CREATING, computeapi.ReasonFileShareNotReady)
	if msg := conditions.Find(waiting.GetStatus().GetConditions(), computeapi.MachineReady).GetMessage(); !strings.Contains(msg, "file share data") {
		t.Errorf("Ready says %q", msg)
	}
	// Its server runs, its export refused, all the while.
	w.waitMachine("fs-data", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonExportFailed)
	uid := waiting.GetMetadata().GetUid()
	if vm, ok := w.host.Snapshot().VMs[uid]; ok {
		t.Fatalf("the container booted before its share's server served it: %+v", vm.VM)
	}
	if p := w.machine("web").GetStatus().GetPhase(); p == computev1alpha1.MachinePhase_MACHINE_PHASE_FAILED {
		t.Error("the waiting container is failed")
	}

	w.host.FailExport("", nil)
	w.waitReason("web", "Running")
	srv := w.machine2("fs-data").GetMetadata().GetUid()
	if got, _ := w.host.Export(srv); !slices.ContainsFunc(got.Clients, func(c driver.ShareClient) bool { return c.Address == containerAddr(w, "web") }) {
		t.Errorf("the container runs, its share's server exporting %+v", got)
	}
	if vm := w.host.Snapshot().VMs[uid]; vm.Boots != 1 {
		t.Errorf("the container booted %d times", vm.Boots)
	}
}

// TestAContainerWaitsForItsServersCurrentExport: a container waits for its
// share's server to have taken, in its current boot, an export holding the
// container: not one without it, nor one told in an earlier boot, which
// the server lost booting again.
func TestAContainerWaitsForItsServersCurrentExport(t *testing.T) {
	w := newWorld(t)
	w.applyShare("data", "1Gi")
	w.waitSharePhase("data", storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE)
	srv := w.machine2("fs-data").GetMetadata().GetUid()

	// The server took an export without the container, and takes no more.
	w.host.FailExport(srv, errors.New("the server is not ready"))
	w.apply(sharing("web", "data", false))
	m := w.waitMachine("ct-web", computev1alpha1.MachinePhase_MACHINE_PHASE_CREATING, computeapi.ReasonFileShareNotReady)
	if msg := conditions.Find(m.GetStatus().GetConditions(), computeapi.MachineReady).GetMessage(); !strings.Contains(msg, "does not export it") {
		t.Errorf("Ready says %q", msg)
	}
	w.host.FailExport(srv, nil)
	w.waitReason("web", "Running")

	// A second container, its image held, is in an export the server then
	// loses, booting again with its refusing more.
	letGo := w.host.HoldUnpack("busybox")
	w.host.AddImage("busybox", "sha256:busybox-1", nginx)
	c := sharing("api", "data", false)
	c.Spec.Image = "busybox"
	w.apply(c)
	testutil.Eventually(t, "the server exporting to the second container", func() (bool, string) {
		got, _ := w.host.Export(srv)
		ok := false
		if m, found := get[*computev1alpha1.Machine](w, registry.Machine, "ct-api"); found && addrOf(m) != "" {
			at := netip.MustParseAddr(addrOf(m))
			ok = slices.ContainsFunc(got.Clients, func(c driver.ShareClient) bool { return c.Address == at })
		}
		return ok, fmt.Sprintf("%+v", got)
	})
	w.host.FailExport(srv, errors.New("the server is not ready"))
	if err := w.host.Exit(srv, 1); err != nil {
		t.Fatal(err)
	}
	w.waitMachine("fs-data", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonExportFailed)
	letGo()
	w.waitMachine("ct-api", computev1alpha1.MachinePhase_MACHINE_PHASE_CREATING, computeapi.ReasonFileShareNotReady)
	if _, ok := w.host.Snapshot().VMs[w.machine("api").GetMetadata().GetUid()]; ok {
		t.Error("the container booted on an export its server lost")
	}
	w.host.FailExport(srv, nil)
	w.waitReason("api", "Running")
}

// TestAContainerWaitsForItsSharesFirewall: a container whose share's
// server serves it, but whose network's firewall cannot be written (bound
// at one rule), so that neither its share's server lets it in nor it is let
// out to 2049, as the host reads the rules back, waits, never booted, nor
// failed though it never restarts; once the firewall is written, it boots
// once, reaching its share.
func TestAContainerWaitsForItsSharesFirewall(t *testing.T) {
	w := newWorld(t)
	w.host.LimitFirewall(1)
	w.applyShare("data", "1Gi")
	c := sharing("web", "data", false)
	c.Spec.RestartPolicy = computev1alpha1.RestartPolicy_RESTART_POLICY_NEVER
	w.apply(c)
	// Its share's server not serving yet comes first, under the same reason.
	waiting := w.waitMachineSaying("ct-web", computev1alpha1.MachinePhase_MACHINE_PHASE_CREATING, computeapi.ReasonFileShareNotReady, "firewall")
	network := w.networkUID("default")
	uid := waiting.GetMetadata().GetUid()
	at, web := w.shareAddr("data", "default"), containerAddr(w, "web")
	srv := w.machine2("fs-data").GetMetadata().GetUid()
	// Both ways in force is what the container waits for; the bound lets
	// one rule at most be.
	both := func() (bool, string) {
		rules, err := w.host.Network().ShareRules(context.Background(), network)
		if err != nil {
			t.Fatal(err)
		}
		return slices.Contains(rules[srv+"/eth0"].Clients, web) && slices.Contains(rules[uid+"/eth0"].Servers, at), fmt.Sprintf("%+v", rules)
	}
	if ok, rules := both(); ok {
		t.Errorf("the share rules in force are %s, past the bound", rules)
	}
	if vm, ok := w.host.Snapshot().VMs[uid]; ok {
		t.Fatalf("the container booted with its share's firewall closed: %+v", vm.VM)
	}

	w.host.LimitFirewall(0)
	w.waitReason("web", "Running")
	if ok, rules := both(); !ok {
		t.Errorf("the container runs with the share rules in force %s", rules)
	}
	if vm := w.host.Snapshot().VMs[uid]; vm.Boots != 1 {
		t.Errorf("the container booted %d times", vm.Boots)
	}
	w.host.ServeGuest(network, netip.AddrPortFrom(at, driver.ShareServerPort), echoBack)
	if !w.reaches("web", netip.AddrPortFrom(at, driver.ShareServerPort)) {
		t.Error("the container does not reach its share")
	}
}

// TestAChangedContainerWaitsOnItsOldVM: a running container whose spec
// changes while its share's server, booted again, takes no export, waits
// on the VM it has, which neither stops nor boots again; once the server
// serves it, it starts again on its new spec.
func TestAChangedContainerWaitsOnItsOldVM(t *testing.T) {
	w := newWorld(t)
	w.applyShare("data", "1Gi")
	w.apply(sharing("web", "data", false))
	w.waitReason("web", "Running")
	uid := w.machine("web").GetMetadata().GetUid()
	before := w.host.Snapshot().VMs[uid]

	srv := w.machine2("fs-data").GetMetadata().GetUid()
	w.host.FailExport("", errors.New("the server is not ready"))
	if err := w.host.Exit(srv, 1); err != nil {
		t.Fatal(err)
	}
	w.waitMachine("fs-data", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonExportFailed)
	c := sharing("web", "data", false)
	c.Spec.Env = []*computev1alpha1.EnvVar{{Name: "CHANGED", Value: "1"}}
	w.apply(c)
	w.waitMachine("ct-web", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonFileShareNotReady)
	if now := w.host.Snapshot().VMs[uid]; now.Boots != before.Boots || !now.Started.Equal(before.Started) || now.State != driver.VMRunning {
		t.Errorf("the changed container's VM: %d boots from %v, %v; was %d boots from %v", now.Boots, now.Started, now.State, before.Boots, before.Started)
	}

	w.host.FailExport("", nil)
	w.waitReason("web", "Running")
	if now := w.host.Snapshot().VMs[uid]; now.Started.Equal(before.Started) || !slices.Contains(now.Spec.Run.Env, "CHANGED=1") {
		t.Errorf("the container runs %v from %v, not started again on its new spec", now.Spec.Run.Env, now.Started)
	}
}
