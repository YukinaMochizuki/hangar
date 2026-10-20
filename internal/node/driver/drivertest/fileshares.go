package drivertest

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

func shareVolumeOf(t *testing.T, s driver.Storage, share string) (driver.ShareVolume, bool) {
	t.Helper()
	vols, err := s.ShareVolumes(t.Context())
	must(t, err)
	i := slices.IndexFunc(vols, func(v driver.ShareVolume) bool { return v.ShareUID == share })
	if i < 0 {
		return driver.ShareVolume{}, false
	}
	return vols[i], true
}

// shareVolumes: a FileShare's volume is made at least the size asked, kept
// asked smaller and grown asked larger, and listed so; it is no Disk's
// volume and has a device of its own; it stays while in use; another
// instance finds it; and it goes when deleted, deleting it again or one
// never made being no error.
func shareVolumes(t *testing.T, h Host) {
	ctx := t.Context()
	v1, err := h.Storage.EnsureShareVolume(ctx, "s1", rootSize)
	must(t, err)
	if v1.ShareUID != "s1" || v1.Path == "" || v1.Size < rootSize {
		t.Errorf("share volume %+v", v1)
	}
	if again, err := h.Storage.EnsureShareVolume(ctx, "s1", rootSize); err != nil || again != v1 {
		t.Errorf("ensuring the share volume again: %+v %v", again, err)
	}
	if kept, err := h.Storage.EnsureShareVolume(ctx, "s1", 1); err != nil || kept != v1 {
		t.Errorf("asked smaller: %+v %v, want %+v", kept, err, v1)
	}
	grown, err := h.Storage.EnsureShareVolume(ctx, "s1", grownSize)
	must(t, err)
	if grown.Size < grownSize || grown.Path != v1.Path {
		t.Errorf("grown %+v from %+v", grown, v1)
	}
	if listed, ok := shareVolumeOf(t, h.Storage, "s1"); !ok || listed != grown {
		t.Errorf("listed %+v %t, grown %+v", listed, ok, grown)
	}
	// A Disk of the same uid has a volume of its own.
	disk, err := h.Storage.EnsureVolume(ctx, "s1", rootSize, "")
	must(t, err)
	if disk.Path == grown.Path {
		t.Errorf("a disk's volume and a share's share a device: %+v %+v", disk, grown)
	}
	if vols, err := h.Storage.Volumes(ctx); err != nil || len(vols) != 1 {
		t.Errorf("the Disks' volumes are %+v %v, want the disk's alone", vols, err)
	}
	must(t, h.Storage.DeleteVolume(ctx, "s1"))
	if _, ok := shareVolumeOf(t, h.Storage, "s1"); !ok {
		t.Error("deleting a disk's volume took the share's of the same uid")
	}
	v2, err := h.Storage.EnsureShareVolume(ctx, "s2", rootSize)
	must(t, err)
	if h.HoldShareVolume != nil {
		release := h.HoldShareVolume(t, v2)
		if err := h.Storage.DeleteShareVolume(ctx, "s2"); err == nil {
			t.Error("deleted a share volume in use")
		}
		if _, ok := shareVolumeOf(t, h.Storage, "s2"); !ok {
			t.Error("a share volume in use is gone after a failed delete")
		}
		release()
	}
	if h.Reopen != nil {
		_, storage := h.Reopen(t)
		vols, err := storage.ShareVolumes(ctx)
		must(t, err)
		slices.SortFunc(vols, func(a, b driver.ShareVolume) int { return strings.Compare(a.ShareUID, b.ShareUID) })
		if !slices.Equal(vols, []driver.ShareVolume{grown, v2}) {
			t.Errorf("share volumes after reopening: %+v, want %+v %+v", vols, grown, v2)
		}
	}
	must(t, h.Storage.DeleteShareVolume(ctx, "s1"))
	must(t, h.Storage.DeleteShareVolume(ctx, "s1"))
	must(t, h.Storage.DeleteShareVolume(ctx, "s2"))
	must(t, h.Storage.DeleteShareVolume(ctx, "never"))
	if vols, err := h.Storage.ShareVolumes(ctx); err != nil || len(vols) != 0 {
		t.Errorf("share volumes left: %v %v", vols, err)
	}
}

// nicAt is machine's eth0 on testNet at 10.0.0.<host>, a file share's
// server's if server.
func nicAt(machine string, host byte, server bool) driver.Interface {
	sum := sha256.Sum256([]byte(machine))
	return driver.Interface{
		MachineUID: machine, Name: "eth0", NetworkUID: testNet.UID, MAC: fmt.Sprintf("02:02:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3]),
		Address: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 0, 0, host}), 24), Gateway: netip.MustParseAddr("10.0.0.1"), FileShareServer: server,
	}
}

// shareServerFirewall: a file share's server's interface takes only TCP to
// 2049 from the clients its policy names, whatever else its policy or
// edge ports say, and opens nothing itself; given no policy, it takes
// nothing. A client reaches the server's 2049 whatever its own groups let
// out, and nothing else by it. Neither the platform's SSH nor an edge
// port reaches the server from the gateway. A client taken out reaches it
// no more.
func shareServerFirewall(t *testing.T, h Host) {
	ctx := t.Context()
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	var srv, locked, open, other driver.Interface
	for _, p := range []struct {
		nic *driver.Interface
		at  driver.Interface
	}{
		{&srv, nicAt("srv", 9, true)}, {&locked, nicAt("locked", 5, false)}, {&open, nicAt("open", 6, false)}, {&other, nicAt("other", 7, false)},
	} {
		nic, err := h.Network.EnsureInterface(ctx, p.at)
		must(t, err)
		if nic.FileShareServer != p.at.FileShareServer {
			t.Errorf("%s made as %+v", p.at.MachineUID, nic)
		}
		*p.nic = nic
	}
	t.Cleanup(func() {
		for _, m := range []string{"srv", "locked", "open", "other"} {
			_ = h.Network.DeleteInterfaces(ctx, m)
		}
		_ = h.Network.DeleteNetwork(ctx, testNet.UID)
	})
	if listed, ok := interfaceOf(t, h.Network, "srv"); !ok || !listed.FileShareServer {
		t.Errorf("the server's interface is listed as %+v %t", listed, ok)
	}
	h.ServeGuest(t, srv, driver.ShareServerPort, 22, 80)
	h.ServeGuest(t, other, driver.ShareServerPort, 22)
	h.ServeGuest(t, open, 22)
	at := func(i driver.Interface, port uint16) netip.AddrPort {
		return netip.AddrPortFrom(i.Address.Addr(), port)
	}
	anything := secgroup.Policy{Ingress: []secgroup.Rule{{Protocol: secgroup.All, Peers: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}}, EgressAll: true}
	share := []netip.Addr{srv.Address.Addr()}
	ensure := func(srvPolicy *driver.InterfacePolicy) {
		t.Helper()
		ps := []driver.InterfacePolicy{
			{MachineUID: "locked", Name: "eth0", Shares: share},
			{MachineUID: "open", Name: "eth0", Policy: anything, Shares: share},
			{MachineUID: "other", Name: "eth0", Policy: anything},
		}
		if srvPolicy != nil {
			ps = append(ps, *srvPolicy)
		}
		must(t, h.Network.EnsureFirewall(ctx, testNet.UID, ps))
	}
	// The rules in force, as the host reads them back.
	inForce := func(what string, want map[string]driver.ShareRules) {
		t.Helper()
		got, err := h.Network.ShareRules(ctx, testNet.UID)
		must(t, err)
		for k, r := range got {
			slices.SortFunc(r.Clients, netip.Addr.Compare)
			slices.SortFunc(r.Servers, netip.Addr.Compare)
			got[k] = r
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the share rules in force are %+v, want %+v", what, got, want)
		}
	}
	check := func(what string, from driver.Interface, to netip.AddrPort, want bool) {
		t.Helper()
		if got := h.Reaches(t, from, to); got != want {
			t.Errorf("%s: reached %t, want %t", what, got, want)
		}
	}
	dials := func(what string, to netip.AddrPort, want bool) {
		t.Helper()
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		c, err := h.Network.Dial(dctx, testNet.UID, to)
		got := false
		if err == nil {
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			b := make([]byte, 2)
			_, werr := c.Write([]byte("hi"))
			_, rerr := io.ReadFull(c, b)
			got = werr == nil && rerr == nil && string(b) == "hi"
			_ = c.Close()
		}
		if got != want {
			t.Errorf("%s: reached %t, want %t (%v)", what, got, want, err)
		}
	}

	// Given no policy, the server takes nothing.
	ensure(nil)
	clientsOf := map[string]driver.ShareRules{"locked/eth0": {Servers: share}, "open/eth0": {Servers: share}}
	inForce("a server given no policy", clientsOf)
	testutil.Eventually(t, "the control: a guest open to all reached", func() bool { return h.Reaches(t, open, at(other, 22)) })
	check("a client to a server given no policy", open, at(srv, driver.ShareServerPort), false)

	// Groups and edge ports given to a server are none of its rules.
	clients := []netip.Addr{locked.Address.Addr(), open.Address.Addr()}
	ensure(&driver.InterfacePolicy{MachineUID: "srv", Name: "eth0", Policy: anything, EdgePorts: []uint16{80}, ShareClients: clients})
	inForce("a server and its clients", map[string]driver.ShareRules{
		"srv/eth0": {Clients: clients}, "locked/eth0": {Servers: share}, "open/eth0": {Servers: share},
	})
	testutil.Eventually(t, "a locked client reaching its share", func() bool { return h.Reaches(t, locked, at(srv, driver.ShareServerPort)) })
	check("an open client to its share", open, at(srv, driver.ShareServerPort), true)
	check("a machine not declaring the share", other, at(srv, driver.ShareServerPort), false)
	check("a client to another port of the server", open, at(srv, 22), false)
	check("a locked client to another's 2049", locked, at(other, driver.ShareServerPort), false)
	check("the server to a guest open to all", srv, at(other, 22), false)
	check("the server to a client", srv, at(open, 22), false)
	check("the control: a guest to a guest open to all", other, at(open, 22), true)
	dials("the platform's SSH to the server", at(srv, 22), false)
	dials("an edge port given to the server", at(srv, 80), false)
	dials("the control: the platform's SSH to a guest", at(other, 22), true)

	// A client taken out reaches it no more; the other still does.
	ensure(&driver.InterfacePolicy{MachineUID: "srv", Name: "eth0", ShareClients: clients[:1]})
	inForce("a client taken out", map[string]driver.ShareRules{
		"srv/eth0": {Clients: clients[:1]}, "locked/eth0": {Servers: share}, "open/eth0": {Servers: share},
	})
	check("a client taken out", open, at(srv, driver.ShareServerPort), false)
	check("the client left", locked, at(srv, driver.ShareServerPort), true)
}

func interfaceOf(t *testing.T, n driver.Network, machine string) (driver.Interface, bool) {
	t.Helper()
	all, err := n.Interfaces(t.Context())
	must(t, err)
	i := slices.IndexFunc(all, func(i driver.Interface) bool { return i.MachineUID == machine })
	if i < 0 {
		return driver.Interface{}, false
	}
	return all[i], true
}

// shareServers: a file share's server boots on its volume, on a server's
// interface alone; it takes an export, and refuses one it cannot serve,
// keeping the one it had; its volume stays while it runs; and it is
// stopped as any guest is, asked first. A server's spec on another's
// interface, or another's on a server's, is never started.
func shareServers(t *testing.T, h Host) {
	ctx := t.Context()
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	vol, err := h.Storage.EnsureShareVolume(ctx, "s1", rootSize)
	must(t, err)
	plain, err := h.Network.EnsureInterface(ctx, nicAt("plain", 8, false))
	must(t, err)
	nic, err := h.Network.EnsureInterface(ctx, nicAt("srv", 9, true))
	must(t, err)
	export := driver.ShareExport{Squash: driver.SquashNone, AnonUID: 65534, AnonGID: 65534,
		Clients: []driver.ShareClient{{Address: netip.MustParseAddr("10.0.0.5")}}}
	spec := driver.VMSpec{
		MachineUID: "srv", Machine: "test/fs-s1", VCPUs: 1, MemoryMiB: 256, Interfaces: []driver.Interface{nic},
		Restart: driver.RestartAlways, FileShareServer: &driver.ShareServer{Volume: vol, Export: export},
	}
	wrong := spec
	wrong.MachineUID, wrong.Interfaces = "plain", []driver.Interface{plain}
	if err := h.Hypervisor.Start(ctx, wrong); err == nil {
		t.Error("started a file share's server on another's interface")
		must(t, h.Hypervisor.Stop(ctx, "plain"))
	}
	container := vmSpec(t, h, "ctr", driver.RestartAlways)
	srvNIC := nicAt("ctr", 10, true)
	if nic, err := h.Network.EnsureInterface(ctx, srvNIC); err == nil {
		container.Interfaces = []driver.Interface{nic}
		if err := h.Hypervisor.Start(ctx, container); err == nil {
			t.Error("started a container on a file share's server's interface")
			must(t, h.Hypervisor.Stop(ctx, "ctr"))
		}
	}
	must(t, h.Network.DeleteInterfaces(ctx, "ctr"))
	must(t, h.Storage.DeleteRoot(ctx, "ctr"))

	must(t, h.Hypervisor.Start(ctx, spec))
	vm, ok := vmOf(t, h, "srv")
	if !ok || vm.State != driver.VMRunning || vm.SpecHash != spec.Hash() {
		t.Fatalf("VM %+v listed %t", vm, ok)
	}
	testutil.EventuallyWithin(t, vmWait, "the server's agent answering", func() bool { return h.Guest.Ready(ctx, "srv") == nil })
	// Another export is the same spec: the server is told it, not
	// started again.
	told := spec
	told.FileShareServer = &driver.ShareServer{Volume: vol, Export: driver.ShareExport{Squash: driver.SquashAll, AnonUID: 1000, AnonGID: 1000}}
	if told.Hash() != spec.Hash() {
		t.Error("the export is part of the spec's hash")
	}
	must(t, h.Hypervisor.Start(ctx, told))
	if again, _ := vmOf(t, h, "srv"); !again.Started.Equal(vm.Started) {
		t.Error("starting the spec with another export restarted the server")
	}
	must(t, h.Guest.SetExport(ctx, "srv", told.FileShareServer.Export))
	for _, client := range []netip.Addr{netip.IPv4Unspecified(), netip.MustParseAddr("255.255.255.255")} {
		bad := export
		bad.Clients = append(slices.Clone(bad.Clients), driver.ShareClient{Address: client})
		if err := h.Guest.SetExport(ctx, "srv", bad); err == nil {
			t.Errorf("the server took an export with a client %s", client)
		}
	}
	if err := h.Guest.SetExport(ctx, "srv", driver.ShareExport{AnonUID: 65534, AnonGID: 65534}); err == nil {
		t.Error("the server took an export with no squash")
	}
	if err := h.Storage.DeleteShareVolume(ctx, "s1"); err == nil {
		t.Error("deleted the volume of a running server")
	}
	must(t, h.Hypervisor.Stop(ctx, "srv"))
	if !h.AskedToStop(t, "srv") {
		t.Error("the server was stopped without being asked first")
	}
	must(t, h.Hypervisor.DeleteLogs(ctx, "srv"))
	must(t, h.Network.DeleteInterfaces(ctx, "srv"))
	must(t, h.Network.DeleteInterfaces(ctx, "plain"))
	must(t, h.Storage.DeleteShareVolume(ctx, "s1"))
}
