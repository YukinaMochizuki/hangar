package node

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1/storagev1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
)

func testShare(name, uid string, networks ...string) *storagev1alpha1.FileShare {
	return &storagev1alpha1.FileShare{
		Metadata: &metav1.ObjectMeta{Project: "p", Name: name, Uid: uid},
		Spec:     &storagev1alpha1.FileShareSpec{Size: "1Gi", Networks: networks},
	}
}

// testMachine is a machine with an interface on each network, at the
// address given after it, declaring shares.
func testMachine(name string, nics map[string]string, shares ...*computev1alpha1.MachineFileShare) *computev1alpha1.Machine {
	m := &computev1alpha1.Machine{
		Metadata: &metav1.ObjectMeta{Project: "p", Name: name, Uid: "uid-" + name},
		Spec:     &computev1alpha1.MachineSpec{FileShares: shares},
	}
	for _, n := range slices.Sorted(func(yield func(string) bool) {
		for n := range nics {
			if !yield(n) {
				return
			}
		}
	}) {
		m.Spec.Interfaces = append(m.Spec.Interfaces, &computev1alpha1.MachineInterface{Name: "eth" + n, Network: n, Ipv4: nics[n] + "/24"})
	}
	return m
}

func testServer(fs *storagev1alpha1.FileShare, nics map[string]string) *computev1alpha1.Machine {
	m := testMachine("fs-"+fs.GetMetadata().GetName(), nics)
	m.Spec.Boot = &computev1alpha1.MachineSpec_FileShareServer{FileShareServer: &computev1alpha1.FileShareServerBoot{
		FileShare: fs.GetMetadata().GetName(), Size: "1Gi",
	}}
	m.Metadata.OwnerReferences = []*metav1.OwnerReference{{Kind: "FileShare", Name: fs.GetMetadata().GetName(), Uid: fs.GetMetadata().GetUid(), Controller: true}}
	return m
}

func declares(share, network string, readOnly bool) *computev1alpha1.MachineFileShare {
	return &computev1alpha1.MachineFileShare{FileShare: share, Network: network, ReadOnly: readOnly}
}

func addrs(ss ...string) []netip.Addr {
	var out []netip.Addr
	for _, s := range ss {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

// TestAShareIsServedByItsOwnServerAlone: a machine serves a share only if
// the share controls it, by uid: one naming the share with no owner, or
// owned by another of the name, serves nothing, gets none of its clients,
// and is no server whose address a client is given.
func TestAShareIsServedByItsOwnServerAlone(t *testing.T) {
	fs := testShare("data", "share-1", "default")
	own := testServer(fs, map[string]string{"default": "10.0.0.9"})
	impostor := testServer(fs, map[string]string{"default": "10.0.0.8"})
	impostor.Metadata.Name, impostor.Metadata.OwnerReferences = "impostor", nil
	stale := testServer(fs, map[string]string{"default": "10.0.0.7"})
	stale.Metadata.Name, stale.Metadata.OwnerReferences[0].Uid = "stale", "share-0"
	owner := testServer(fs, map[string]string{"default": "10.0.0.6"})
	owner.Metadata.Name, owner.Metadata.OwnerReferences[0].Controller = "owner", false
	client := testMachine("web", map[string]string{"default": "10.0.0.5"}, declares("data", "default", false))
	p := newProjectShares([]proto.Message{fs}, []proto.Message{impostor, stale, owner, own, client})
	if got := p.served(own); got != fs {
		t.Errorf("the share's own server serves %v", got)
	}
	for _, m := range []*computev1alpha1.Machine{impostor, stale, owner} {
		if got := p.served(m); got != nil {
			t.Errorf("%s serves %s", m.GetMetadata().GetName(), got.GetMetadata().GetName())
		}
		if clients, servers := p.sharePolicy(m, "default"); len(clients) > 0 || len(servers) > 0 {
			t.Errorf("%s takes clients %v", m.GetMetadata().GetName(), clients)
		}
	}
	if _, servers := p.sharePolicy(client, "default"); !slices.Equal(servers, addrs("10.0.0.9")) {
		t.Errorf("the client reaches %v, want the share's own server", servers)
	}
}

// TestAShareExport: the export is the declaring machines' addresses on the
// network each mounts from, the share's alone, once each, sorted,
// read-only as declared, the least of two at one address; with its squash
// and anonymous ids, 65534 when unset. A machine being deleted, declaring
// none, or mounting from a network the share is not on, or it has no
// address on, is no client.
func TestAShareExport(t *testing.T) {
	fs := testShare("data", "share-1", "default", "back")
	anon := uint32(1000)
	fs.Spec.Squash, fs.Spec.AnonUid = storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_ALL, &anon
	deleted := testMachine("gone", map[string]string{"default": "10.0.0.20"}, declares("data", "default", false))
	deleted.Metadata.DeleteTime = timestamppb.Now()
	p := newProjectShares([]proto.Message{fs}, []proto.Message{
		testMachine("web", map[string]string{"default": "10.0.0.5", "back": "10.1.0.5"}, declares("data", "back", false)),
		testMachine("ro", map[string]string{"default": "10.0.0.4"}, declares("data", "default", true)),
		testMachine("twin", map[string]string{"elsewhere": "10.0.0.4"}, declares("data", "default", false)),
		testMachine("other", map[string]string{"default": "10.0.0.6"}, declares("logs", "default", false)),
		testMachine("off", map[string]string{"third": "10.2.0.6"}, declares("data", "third", false)),
		testMachine("unset", map[string]string{"default": "10.0.0.7"}, declares("data", "", false)),
		testMachine("none", map[string]string{"default": "10.0.0.8"}),
		deleted,
	})
	got := shareExport(fs, p.clients(fs))
	want := driver.ShareExport{Squash: driver.SquashAll, AnonUID: 1000, AnonGID: 65534, Clients: []driver.ShareClient{
		{Address: netip.MustParseAddr("10.0.0.4"), ReadOnly: true}, {Address: netip.MustParseAddr("10.1.0.5")},
	}}
	if !sameExport(got, want) {
		t.Errorf("export %+v, want %+v", got, want)
	}
	// twin, at ro's address on a network the share is not on, is no
	// client; were two at one address, the least of what they are counts.
	twin := shareExport(fs, []shareClient{{addr: netip.MustParseAddr("10.0.0.4")}, {addr: netip.MustParseAddr("10.0.0.4"), readOnly: true}})
	if !slices.Equal(twin.Clients, []driver.ShareClient{{Address: netip.MustParseAddr("10.0.0.4"), ReadOnly: true}}) {
		t.Errorf("two clients at one address: %+v", twin.Clients)
	}
	for squash, want := range map[storagev1alpha1.FileShareSquash]driver.Squash{
		storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_UNSPECIFIED: driver.SquashNone,
		storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_NONE:        driver.SquashNone,
		storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_ROOT:        driver.SquashRoot,
		storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_ALL:         driver.SquashAll,
	} {
		s := testShare("s", "u")
		s.Spec.Squash = squash
		if got := shareExport(s, nil); got.Squash != want || got.AnonUID != 65534 || got.AnonGID != 65534 {
			t.Errorf("squash %v: %+v", squash, got)
		}
	}
	// On each network, the server takes the clients mounting from it.
	srv := testServer(fs, map[string]string{"default": "10.0.0.9", "back": "10.1.0.9"})
	p.servers["data"] = srv
	p.machines = append(p.machines, srv)
	for network, want := range map[string][]netip.Addr{"default": addrs("10.0.0.4"), "back": addrs("10.1.0.5")} {
		if clients, _ := p.sharePolicy(srv, network); !slices.Equal(clients, want) {
			t.Errorf("on %s the server takes %v, want %v", network, clients, want)
		}
	}
	web := p.machines[0]
	for network, want := range map[string][]netip.Addr{"default": nil, "back": addrs("10.1.0.9")} {
		if _, servers := p.sharePolicy(web, network); !slices.Equal(servers, want) {
			t.Errorf("on %s web reaches %v, want %v", network, servers, want)
		}
	}
}

// shares is a FileShare service answering Get from a map, or failing.
type shares struct {
	storagev1alpha1connect.FileShareServiceClient
	byName map[string]*storagev1alpha1.FileShare
	err    error
}

func (s shares) GetFileShare(_ context.Context, req *connect.Request[storagev1alpha1.GetFileShareRequest]) (*connect.Response[storagev1alpha1.GetFileShareResponse], error) {
	if s.err != nil {
		return nil, s.err
	}
	fs, ok := s.byName[req.Msg.GetName()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("not found"))
	}
	return connect.NewResponse(&storagev1alpha1.GetFileShareResponse{FileShare: fs}), nil
}

// TestAContainersShareMount: a container mounts a share from its server's
// address on the network it mounts from, read-only if its volume or its
// declaration is; a share with no address there yet, on another node, gone
// or not declared holds it back, unbootable; the API not read does not.
func TestAContainersShareMount(t *testing.T) {
	a := &Agent{cfg: Config{Name: "n1"}}
	fs := testShare("data", "share-1", "default", "back")
	fs.Status = &storagev1alpha1.FileShareStatus{Node: "n1", Addresses: []*storagev1alpha1.FileShareAddress{
		{Network: "default", Address: "10.0.0.9"}, {Network: "back", Address: "10.1.0.9"},
	}}
	elsewhere := testShare("far", "share-2", "default")
	elsewhere.Status = &storagev1alpha1.FileShareStatus{Node: "n2", Addresses: fs.Status.Addresses}
	early := testShare("early", "share-3", "default")
	early.Status = &storagev1alpha1.FileShareStatus{Node: "n1", Addresses: []*storagev1alpha1.FileShareAddress{{Network: "back", Address: "10.1.0.9"}}}
	a.shares = shares{byName: map[string]*storagev1alpha1.FileShare{"data": fs, "far": elsewhere, "early": early}}
	m := testMachine("web", map[string]string{"default": "10.0.0.5", "back": "10.1.0.5"},
		declares("data", "back", true), declares("far", "default", false), declares("early", "default", false), declares("gone", "default", false))
	vol := func(share string, readOnly bool) *computev1alpha1.ContainerBootVolume {
		return &computev1alpha1.ContainerBootVolume{FileShare: share, Target: "/data", Subpath: "sub", ReadOnly: readOnly}
	}
	got, err := a.shareMount(t.Context(), m, vol("data", false))
	want := driver.Mount{Name: "data", Target: "/data", Subpath: "sub", ReadOnly: true, Share: netip.MustParseAddr("10.1.0.9")}
	if err != nil || got != want {
		t.Errorf("mount %+v %v, want %+v", got, err, want)
	}
	for _, share := range []string{"far", "early", "gone", "undeclared"} {
		var no *unbootable
		if _, err := a.shareMount(t.Context(), m, vol(share, false)); !errors.As(err, &no) {
			t.Errorf("share %s: %v, want it unbootable", share, err)
		}
	}
	a.shares = shares{err: connect.NewError(connect.CodeUnavailable, errors.New("down"))}
	var no *unbootable
	if _, err := a.shareMount(t.Context(), m, vol("data", false)); err == nil || errors.As(err, &no) {
		t.Errorf("the API down: %v, want an error that is not unbootable", err)
	}
}

// releasing is a FileShare service answering Get with one share, and
// taking any status write.
type releasing struct {
	storagev1alpha1connect.FileShareServiceClient
	fs     *storagev1alpha1.FileShare
	writes int
}

func (r *releasing) GetFileShare(context.Context, *connect.Request[storagev1alpha1.GetFileShareRequest]) (*connect.Response[storagev1alpha1.GetFileShareResponse], error) {
	return connect.NewResponse(&storagev1alpha1.GetFileShareResponse{FileShare: r.fs}), nil
}

func (r *releasing) UpdateFileShareStatus(context.Context, *connect.Request[storagev1alpha1.UpdateFileShareStatusRequest]) (*connect.Response[storagev1alpha1.UpdateFileShareStatusResponse], error) {
	r.writes++
	return connect.NewResponse(&storagev1alpha1.UpdateFileShareStatusResponse{}), nil
}

// TestAShareVolumeGoesOnlyOnceItsServerIs: a deleted share's volume is
// destroyed, and the share Released, only once its server is gone (its
// dependents finalizer gone), never while it is not; nor is a live
// share's, or another node's.
func TestAShareVolumeGoesOnlyOnceItsServerIs(t *testing.T) {
	host := fake.New()
	a := &Agent{cfg: Config{Name: "n1", Clock: clockwork.NewRealClock(), Drivers: &Drivers{Storage: host.Storage()}}}
	fs := testShare("data", "share-1")
	fs.Status = &storagev1alpha1.FileShareStatus{Node: "n1"}
	fs.Metadata.Finalizers = []string{computeapi.DependentsFinalizer, registry.Domain + "/node-cleanup"}
	r := &releasing{fs: fs}
	a.shares = r
	host.LeaveShareVolume("share-1")
	k := controller.Key{Project: "p", Name: "data"}
	kept := func(what string) {
		t.Helper()
		if err := a.reconcileShare(t.Context(), k); err != nil {
			t.Fatal(err)
		}
		if _, ok := host.Snapshot().ShareVolumes["share-1"]; !ok || r.writes != 0 {
			t.Errorf("%s: the volume destroyed (%t), %d status writes", what, !ok, r.writes)
		}
	}
	kept("a live share")
	fs.Metadata.DeleteTime = timestamppb.Now()
	kept("a deleted share whose server is not gone")
	fs.Metadata.Finalizers = fs.Metadata.Finalizers[1:]
	fs.Status.Node = "n2"
	kept("another node's share")
	fs.Status.Node = "n1"
	if err := a.reconcileShare(t.Context(), k); err != nil {
		t.Fatal(err)
	}
	if _, ok := host.Snapshot().ShareVolumes["share-1"]; ok || r.writes != 1 {
		t.Errorf("a deleted share whose server is gone: volume left %t, %d status writes", ok, r.writes)
	}
}

// TestAnExportTakenRequeuesWaitingMachines: a server taking a new export
// has the machines waiting for their shares queued again, so a container
// waiting for it boots; one told nothing new, or refusing, does not.
func TestAnExportTakenRequeuesWaitingMachines(t *testing.T) {
	host := fake.New()
	var queued []controller.Key
	a := &Agent{cfg: Config{Name: "n1", Drivers: &Drivers{Guest: host.Guest()}}, exported: map[string]toldExport{}, revoking: newRevocations(), shareWaiters: map[controller.Key]bool{},
		requeue: func(keys ...controller.Key) { queued = append(queued, keys...) }}
	a.cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	a.cfg.Clock = clockwork.NewRealClock()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(host.Network().EnsureNetwork(ctx, driver.NetworkSpec{UID: "n1", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.0/31")}))
	nic, err := host.Network().EnsureInterface(ctx, driver.Interface{MachineUID: "srv", Name: "eth0", NetworkUID: "n1", MAC: "02:00:00:00:00:09",
		Address: netip.MustParsePrefix("10.0.0.9/24"), Gateway: netip.MustParseAddr("10.0.0.1"), FileShareServer: true})
	must(err)
	vol, err := host.Storage().EnsureShareVolume(ctx, "s1", 1<<30)
	must(err)
	export := driver.ShareExport{Squash: driver.SquashNone, AnonUID: 65534, AnonGID: 65534, Clients: []driver.ShareClient{{Address: netip.MustParseAddr("10.0.0.5")}}}
	spec := driver.VMSpec{MachineUID: "srv", Machine: "p/fs-s1", VCPUs: 1, MemoryMiB: 256, Interfaces: []driver.Interface{nic},
		FileShareServer: &driver.ShareServer{Volume: vol, Export: export}}
	must(host.Hypervisor().Start(ctx, spec))
	vms, err := host.Hypervisor().VMs(ctx)
	must(err)
	vm := &vms[0]
	waiting := controller.Key{Project: "p", Name: "web"}
	rechecked := func() bool {
		defer func() { queued = nil; a.waitForShares(waiting) }()
		return slices.Equal(queued, []controller.Key{waiting})
	}
	a.waitForShares(waiting)
	if _, err := a.ensureExport(ctx, spec, vm, &observation{}); err != nil || !rechecked() {
		t.Errorf("an export taken: %v, or no machine checked again", err)
	}
	if _, err := a.ensureExport(ctx, spec, vm, &observation{}); err != nil || rechecked() {
		t.Errorf("nothing new: %v, machines checked again", err)
	}
	host.FailExport("srv", errors.New("refused"))
	spec.FileShareServer = &driver.ShareServer{Volume: vol, Export: driver.ShareExport{Squash: driver.SquashAll, AnonUID: 1, AnonGID: 1}}
	if _, err := a.ensureExport(ctx, spec, vm, &observation{}); err == nil || rechecked() {
		t.Errorf("an export refused: %v, machines checked again", err)
	}
}

// TestAServerServesOnlyRunningInTheBootItTookItsExport: a share's server
// serves a container only while it runs, and only the export it took in
// its current boot: not with no VM, nor exited, nor told nothing, nor
// told in an earlier boot.
func TestAServerServesOnlyRunningInTheBootItTookItsExport(t *testing.T) {
	boot := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	told := toldExport{boot: boot}
	running := &driver.VM{State: driver.VMRunning, Started: boot}
	exited := &driver.VM{State: driver.VMExited, Started: boot}
	again := &driver.VM{State: driver.VMRunning, Started: boot.Add(time.Minute)}
	for what, c := range map[string]struct {
		vm   *driver.VM
		ok   bool
		want bool
	}{
		"running, told in this boot": {running, true, true},
		"no VM":                      {nil, true, false},
		"exited":                     {exited, true, false},
		"told nothing":               {running, false, false},
		"told in an earlier boot":    {again, true, false},
	} {
		if got := serving(c.vm, told, c.ok); got != c.want {
			t.Errorf("%s: serving %t, want %t", what, got, c.want)
		}
	}
}

// TestTheFirewallLetsAClientBothWays: a client waits for the share rules in
// force to let it in to its server and out to it, both: neither, one, or
// the other's of another machine or address are not enough.
func TestTheFirewallLetsAClientBothWays(t *testing.T) {
	at, srv := netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("10.0.0.9")
	mnt := driver.Mount{Name: "data", Share: srv}
	in := driver.ShareRules{Clients: []netip.Addr{at}}
	out := driver.ShareRules{Servers: []netip.Addr{srv}}
	for what, c := range map[string]struct {
		rules map[string]driver.ShareRules
		want  bool
	}{
		"both":                      {map[string]driver.ShareRules{"s/eth0": in, "c/eth0": out}, true},
		"neither":                   {nil, false},
		"in alone":                  {map[string]driver.ShareRules{"s/eth0": in}, false},
		"out alone":                 {map[string]driver.ShareRules{"c/eth0": out}, false},
		"in for another address":    {map[string]driver.ShareRules{"s/eth0": {Clients: []netip.Addr{srv}}, "c/eth0": out}, false},
		"out to another server":     {map[string]driver.ShareRules{"s/eth0": in, "c/eth0": {Servers: []netip.Addr{at}}}, false},
		"another machine's opening": {map[string]driver.ShareRules{"s/eth0": in, "x/eth0": out}, false},
	} {
		if got := firewallLets(c.rules, "s/eth0", "c/eth0", at, mnt) == ""; got != c.want {
			t.Errorf("%s: let through %t, want %t", what, got, c.want)
		}
	}
}
