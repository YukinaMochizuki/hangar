package node

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// defaultAnonID is the user and group a share squashes to when its spec
// names none: nobody.
const defaultAnonID = 65534

// projectShares is what a project's machines make of its FileShares: each
// share's server, and the machines that declare it.
type projectShares struct {
	shares   map[string]*storagev1alpha1.FileShare
	servers  map[string]*computev1alpha1.Machine
	machines []*computev1alpha1.Machine
}

// readProjectShares reads a project's shares and machines from the API.
func (a *Agent) readProjectShares(ctx context.Context, project string) (*projectShares, error) {
	shares, _, err := a.api.ListAll(ctx, registry.FileShare, client.ListOptions{Project: project})
	if err != nil {
		return nil, fmt.Errorf("listing file shares: %w", err)
	}
	machines, _, err := a.api.ListAll(ctx, registry.Machine, client.ListOptions{Project: project})
	if err != nil {
		return nil, fmt.Errorf("listing machines: %w", err)
	}
	return newProjectShares(shares, machines), nil
}

// newProjectShares is what machines make of shares, both a project's as
// the API lists them.
func newProjectShares(shares, machines []proto.Message) *projectShares {
	p := &projectShares{shares: map[string]*storagev1alpha1.FileShare{}, servers: map[string]*computev1alpha1.Machine{}}
	for _, o := range shares {
		fs := o.(*storagev1alpha1.FileShare)
		p.shares[fs.GetMetadata().GetName()] = fs
	}
	for _, o := range machines {
		m := o.(*computev1alpha1.Machine)
		p.machines = append(p.machines, m)
		if fs := p.served(m); fs != nil {
			p.servers[fs.GetMetadata().GetName()] = m
		}
	}
	return p
}

// sharePolicy is what of the shares the firewall holds for m's interface
// on network: if m is a share's server, the addresses that may connect to
// it there; otherwise those of the servers of the shares m mounts from
// there, which it reaches whatever its groups say.
func (p *projectShares) sharePolicy(m *computev1alpha1.Machine, network string) (clients, servers []netip.Addr) {
	if fs := p.served(m); fs != nil {
		for _, c := range p.clients(fs) {
			if c.network == network {
				clients = append(clients, c.addr)
			}
		}
		return clients, nil
	}
	for _, f := range m.GetSpec().GetFileShares() {
		if f.GetNetwork() != network {
			continue
		}
		if a := p.serverAddr(f.GetFileShare(), network); a.IsValid() {
			servers = append(servers, a)
		}
	}
	return nil, servers
}

// networksOfShare maps a share to the networks it is on, whose firewalls
// hold who reaches its server.
func networksOfShare(_ context.Context, o store.Object) []controller.Key {
	var fs storagev1alpha1.FileShare
	if proto.Unmarshal(o.Data, &fs) != nil {
		return nil
	}
	var keys []controller.Key
	for _, n := range computeapi.ShareNetworks(fs.GetSpec()) {
		keys = append(keys, controller.Key{Project: o.Project, Name: n})
	}
	return keys
}

// served is the share m serves, if m is the server that share owns; nil
// otherwise. A machine naming a share that does not own it serves nothing.
func (p *projectShares) served(m *computev1alpha1.Machine) *storagev1alpha1.FileShare {
	srv := m.GetSpec().GetFileShareServer()
	if srv == nil {
		return nil
	}
	fs := p.shares[srv.GetFileShare()]
	if fs == nil || !controlledBy(m.GetMetadata(), fs.GetMetadata().GetUid()) {
		return nil
	}
	return fs
}

// controlledBy says whether the object's controlling owner has uid.
func controlledBy(meta *metav1.ObjectMeta, uid string) bool {
	return uid != "" && slices.ContainsFunc(meta.GetOwnerReferences(), func(r *metav1.OwnerReference) bool {
		return r.GetController() && r.GetUid() == uid
	})
}

// shareClient is a machine that may mount a share: its address on the
// network it mounts the share from.
type shareClient struct {
	machine  string
	network  string
	addr     netip.Addr
	readOnly bool
}

// clients are the machines declaring share fs, not being deleted, each on
// the network it mounts from, as the apiserver stored it, if the share is
// on that network and the machine has an address there.
func (p *projectShares) clients(fs *storagev1alpha1.FileShare) []shareClient {
	nets := computeapi.ShareNetworks(fs.GetSpec())
	var out []shareClient
	for _, m := range p.machines {
		if m.GetMetadata().GetDeleteTime() != nil {
			continue
		}
		f := declared(m.GetSpec(), fs.GetMetadata().GetName())
		if f == nil || !slices.Contains(nets, f.GetNetwork()) {
			continue
		}
		for _, nic := range m.GetSpec().GetInterfaces() {
			if a, err := netip.ParsePrefix(nic.GetIpv4()); err == nil && nic.GetNetwork() == f.GetNetwork() {
				out = append(out, shareClient{machine: m.GetMetadata().GetName(), network: f.GetNetwork(), addr: a.Addr(), readOnly: f.GetReadOnly()})
			}
		}
	}
	return out
}

// serverAddr is share's server's address on network; invalid if it has
// none.
func (p *projectShares) serverAddr(share, network string) netip.Addr {
	for _, nic := range p.servers[share].GetSpec().GetInterfaces() {
		if a, err := netip.ParsePrefix(nic.GetIpv4()); err == nil && nic.GetNetwork() == network {
			return a.Addr()
		}
	}
	return netip.Addr{}
}

// declared is the entry of spec.file_shares naming share; nil if none
// does.
func declared(spec *computev1alpha1.MachineSpec, share string) *computev1alpha1.MachineFileShare {
	for _, f := range spec.GetFileShares() {
		if f.GetFileShare() == share {
			return f
		}
	}
	return nil
}

// shareExport is who may mount fs, and how: its clients, each address once,
// read-only if any machine at it is, and its squash.
func shareExport(fs *storagev1alpha1.FileShare, clients []shareClient) driver.ShareExport {
	e := driver.ShareExport{Squash: driver.SquashNone, AnonUID: defaultAnonID, AnonGID: defaultAnonID}
	switch fs.GetSpec().GetSquash() {
	case storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_ROOT:
		e.Squash = driver.SquashRoot
	case storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_ALL:
		e.Squash = driver.SquashAll
	}
	if fs.GetSpec().AnonUid != nil {
		e.AnonUID = fs.GetSpec().GetAnonUid()
	}
	if fs.GetSpec().AnonGid != nil {
		e.AnonGID = fs.GetSpec().GetAnonGid()
	}
	for _, c := range clients {
		// Networks never overlap a share's, so one address is one machine;
		// were it not, the server could not tell them apart, and each is
		// given the least of what they are.
		if i := slices.IndexFunc(e.Clients, func(x driver.ShareClient) bool { return x.Address == c.addr }); i >= 0 {
			e.Clients[i].ReadOnly = e.Clients[i].ReadOnly || c.readOnly
			continue
		}
		e.Clients = append(e.Clients, driver.ShareClient{Address: c.addr, ReadOnly: c.readOnly})
	}
	slices.SortFunc(e.Clients, func(a, b driver.ShareClient) int { return a.Address.Compare(b.Address) })
	return e
}

// convergeShareServer makes the host run m, a FileShare's server, as
// specified, and says what it then runs: the share's volume at the size m
// names, its interfaces closed to all but the share's clients, and the
// share's export, which the server is told at each boot and whenever it
// changes. A machine that is not the server its share owns, or whose share
// is not this node's, gets nothing of it.
func (a *Agent) convergeShareServer(ctx context.Context, m *computev1alpha1.Machine, vm *driver.VM, epoch int64, vcpus int32, memoryMiB int64) (*observation, error) {
	uid := m.GetMetadata().GetUid()
	p, err := a.readProjectShares(ctx, m.GetMetadata().GetProject())
	if err != nil {
		return nil, err
	}
	name := m.GetSpec().GetFileShareServer().GetFileShare()
	fs := p.served(m)
	switch {
	case fs == nil:
		return nil, a.refuse(ctx, vm, fmt.Errorf("the machine is not file share %s's server", name))
	case fs.GetStatus().GetNode() != a.cfg.Name:
		return nil, a.refuse(ctx, vm, fmt.Errorf("file share %s is bound to node %q, not this one", name, fs.GetStatus().GetNode()))
	}
	size, err := quantity.Bytes(m.GetSpec().GetFileShareServer().GetSize())
	if err != nil || size > 1<<62 {
		return nil, a.refuse(ctx, vm, fmt.Errorf("file share %s's size %q: %w", name, m.GetSpec().GetFileShareServer().GetSize(), err))
	}
	vol, err := a.cfg.Drivers.Storage.EnsureShareVolume(ctx, fs.GetMetadata().GetUid(), int64(size))
	if err != nil {
		return nil, blocked(computeapi.ReasonDiskFailed, err)
	}
	want, err := a.interfaces(ctx, m)
	if err != nil {
		return nil, err
	}
	for i := range want {
		want[i].FileShareServer, want[i].IPv6 = true, netip.Addr{}
	}
	ifaces, vm, pending, err := a.ensureInterfaces(ctx, uid, want, vm, false)
	if err != nil {
		return nil, err
	}
	a.netChecked[uid] = epoch
	// No resolver, nor any other of its networks' services, answers it.
	spec := driver.VMSpec{
		MachineUID:      uid,
		Machine:         m.GetMetadata().GetProject() + "/" + m.GetMetadata().GetName(),
		VCPUs:           vcpus,
		MemoryMiB:       memoryMiB,
		Interfaces:      ifaces,
		Restart:         restartPolicy(m.GetSpec().GetRestartPolicy()),
		FileShareServer: &driver.ShareServer{Volume: vol, Export: shareExport(fs, p.clients(fs))},
	}
	obs, err := a.ensureVM(ctx, spec, vm)
	return obs, errors.Join(err, pending)
}

// toldExport is what a share's server was told of its export in one boot.
type toldExport struct {
	boot   time.Time
	export driver.ShareExport
}

// ensureExport tells a running share's server its export, running says
// how it runs if it takes it: on the agent's first sight of each boot, as
// what the server holds then is the agent's to know only by telling it,
// and whenever the export changes. A server that refuses it keeps the
// export it had, and is reported not ready, saying why.
func (a *Agent) ensureExport(ctx context.Context, spec driver.VMSpec, vm *driver.VM, running *observation) (*observation, error) {
	uid := spec.MachineUID
	want := spec.FileShareServer.Export
	if told, ok := a.exported[uid]; ok && told.boot.Equal(vm.Started) && sameExport(told.export, want) {
		return running, nil
	}
	gctx, cancel := context.WithTimeout(ctx, guestTimeout)
	err := a.cfg.Drivers.Guest.SetExport(gctx, uid, want)
	cancel()
	if err != nil {
		// The server holds what it took last, which is what exported says:
		// the export differing, it is told again.
		a.cfg.Log.Warn("a file share's server did not take its export", "machine", spec.Machine, "machine_uid", uid, "error", err)
		obs := notReady(computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonExportFailed,
			"the server did not take its export, and serves the one it had: "+err.Error())
		obs.started = running.started
		return obs, fmt.Errorf("machine %s's export: %w", spec.Machine, err)
	}
	a.exported[uid] = toldExport{boot: vm.Started, export: want}
	a.requeueShareWaiters()
	return running, nil
}

// ensureBootExport writes a listed server's export to its boot disk, on a
// change, before the server is told; a failed write is reported, and the
// server told all the same (docs/decisions/file-shares.md).
func (a *Agent) ensureBootExport(ctx context.Context, spec driver.VMSpec) error {
	uid := spec.MachineUID
	want := spec.FileShareServer.Export
	if have, ok := a.bootExported[uid]; ok && sameExport(have, want) {
		return nil
	}
	if err := a.cfg.Drivers.Hypervisor.SetBootExport(ctx, uid, want); err != nil {
		// The disk holds what it did, bootExported says: written whole or
		// not at all.
		a.cfg.Log.Warn("a file share's server's boot disk did not take its export", "machine", spec.Machine, "machine_uid", uid, "error", err)
		return fmt.Errorf("machine %s's boot disk: %w", spec.Machine, err)
	}
	a.bootExported[uid] = want
	return nil
}

func sameExport(a, b driver.ShareExport) bool {
	return a.Squash == b.Squash && a.AnonUID == b.AnonUID && a.AnonGID == b.AnonGID && slices.Equal(a.Clients, b.Clients)
}

// shareMount is the mount of a container's FileShare volume v: the server's
// address on the network the machine mounts the share from, as the
// apiserver stored both. A share with no server address there yet, or on
// another node, holds the container back, unbootable: it never boots
// without it.
func (a *Agent) shareMount(ctx context.Context, m *computev1alpha1.Machine, v *computev1alpha1.ContainerBootVolume) (driver.Mount, error) {
	name := v.GetFileShare()
	f := declared(m.GetSpec(), name)
	if f == nil {
		return driver.Mount{}, &unbootable{fmt.Errorf("file share %s at %s is not one the machine declares", name, v.GetTarget())}
	}
	fs, err := a.getShare(ctx, m.GetMetadata().GetProject(), name)
	if err != nil {
		return driver.Mount{}, err
	}
	if fs == nil {
		return driver.Mount{}, &unbootable{fmt.Errorf("file share %s at %s does not exist", name, v.GetTarget())}
	}
	if on := fs.GetStatus().GetNode(); on != a.cfg.Name {
		return driver.Mount{}, &unbootable{fmt.Errorf("file share %s at %s is on node %q, not this one", name, v.GetTarget(), on)}
	}
	var addr netip.Addr
	for _, sa := range fs.GetStatus().GetAddresses() {
		if a, err := netip.ParseAddr(sa.GetAddress()); err == nil && sa.GetNetwork() == f.GetNetwork() && f.GetNetwork() != "" {
			addr = a
		}
	}
	if !addr.Is4() {
		return driver.Mount{}, &unbootable{fmt.Errorf("file share %s at %s has no address on network %q yet", name, v.GetTarget(), f.GetNetwork())}
	}
	return driver.Mount{
		Name: name, Target: v.GetTarget(), ReadOnly: v.GetReadOnly() || f.GetReadOnly(), Subpath: v.GetSubpath(), Share: addr,
	}, nil
}

// sharesServe says why machine m, a container mounting mnts, cannot boot
// yet, if it cannot: a share it mounts has no server running on this node,
// or one that has not taken an export holding m's address on the network
// m mounts from, since its boot (exported); or the firewall there, as the
// host reads it back, does not yet let m in to the server's port, or m out
// to it. Empty means each share serves it now. A container waiting for
// one is queued again as soon as a server takes an export, or a network's
// firewall is written (requeueShareWaiters).
func (a *Agent) sharesServe(ctx context.Context, m *computev1alpha1.Machine, mnts []driver.Mount) (why string, err error) {
	if !slices.ContainsFunc(mnts, func(mnt driver.Mount) bool { return mnt.Share.IsValid() }) {
		return "", nil
	}
	p, err := a.readProjectShares(ctx, m.GetMetadata().GetProject())
	if err != nil {
		return "", err
	}
	for _, mnt := range mnts {
		if !mnt.Share.IsValid() {
			continue
		}
		srv := p.servers[mnt.Name]
		if srv == nil || srv.GetSpec().GetNode() != a.cfg.Name {
			return fmt.Sprintf("file share %s has no server on this node yet", mnt.Name), nil
		}
		vm, err := a.vmOf(ctx, srv.GetMetadata().GetUid())
		if err != nil {
			return "", err
		}
		told, ok := a.exported[srv.GetMetadata().GetUid()]
		if !serving(vm, told, ok) {
			return fmt.Sprintf("file share %s's server is not serving yet", mnt.Name), nil
		}
		var at netip.Addr
		if f := declared(m.GetSpec(), mnt.Name); f != nil {
			for _, nic := range m.GetSpec().GetInterfaces() {
				if a, err := netip.ParsePrefix(nic.GetIpv4()); err == nil && nic.GetNetwork() == f.GetNetwork() {
					at = a.Addr()
				}
			}
		}
		if !slices.ContainsFunc(told.export.Clients, func(c driver.ShareClient) bool { return c.Address == at }) {
			return fmt.Sprintf("file share %s's server does not export it to %s yet", mnt.Name, at), nil
		}
		if why, err := a.firewallServes(ctx, m, srv, mnt, at); why != "" || err != nil {
			return why, err
		}
	}
	return "", nil
}

// firewallServes says why the firewall of the network m mounts share mnt
// from, as the host reads it back, does not let m, at its address there,
// reach the share's server srv on its port, if it does not.
func (a *Agent) firewallServes(ctx context.Context, m, srv *computev1alpha1.Machine, mnt driver.Mount, at netip.Addr) (why string, err error) {
	network := declared(m.GetSpec(), mnt.Name).GetNetwork()
	nicOn := func(m *computev1alpha1.Machine) string {
		for _, nic := range m.GetSpec().GetInterfaces() {
			if nic.GetNetwork() == network {
				return m.GetMetadata().GetUid() + "/" + nic.GetName()
			}
		}
		return ""
	}
	n, err := a.getNetwork(ctx, m.GetMetadata().GetProject(), network)
	if err != nil {
		return "", err
	}
	rules, err := a.cfg.Drivers.Network.ShareRules(ctx, n.GetMetadata().GetUid())
	if err != nil {
		return "", err
	}
	return firewallLets(rules, nicOn(srv), nicOn(m), at, mnt), nil
}

// serving says whether a share's server, its VM vm (nil for none), serves
// the export told, if ok: it runs, and took the export in this boot; one
// it took before booting again it does not hold.
func serving(vm *driver.VM, told toldExport, ok bool) bool {
	return vm != nil && vm.State == driver.VMRunning && ok && told.boot.Equal(vm.Started)
}

// firewallLets says why rules, the share rules in force on a network, do
// not let the client at, its interface cli, in to the server's interface
// srv, and out to the share mnt's server, if they do not.
func firewallLets(rules map[string]driver.ShareRules, srv, cli string, at netip.Addr, mnt driver.Mount) string {
	if !slices.Contains(rules[srv].Clients, at) {
		return fmt.Sprintf("file share %s's server's firewall does not let %s in yet", mnt.Name, at)
	}
	if !slices.Contains(rules[cli].Servers, mnt.Share) {
		return fmt.Sprintf("the firewall does not let the machine out to file share %s yet", mnt.Name)
	}
	return ""
}

func (a *Agent) getShare(ctx context.Context, project, name string) (*storagev1alpha1.FileShare, error) {
	res, err := a.shares.GetFileShare(ctx, connect.NewRequest(&storagev1alpha1.GetFileShareRequest{Project: project, Name: name}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return res.Msg.GetFileShare(), nil
}

// machinesOfShare maps a share to the machines on this node it changes:
// its server, whose export and volume it is, and those declaring it, which
// mount it at its server's addresses.
func (a *Agent) machinesOfShare(ctx context.Context, o store.Object) []controller.Key {
	objs, _, err := a.api.ListAll(ctx, registry.Machine, client.ListOptions{Project: o.Project})
	if err != nil {
		a.cfg.Log.Error("listing machines failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, obj := range objs {
		m := obj.(*computev1alpha1.Machine)
		if m.GetSpec().GetNode() == a.cfg.Name &&
			(m.GetSpec().GetFileShareServer().GetFileShare() == o.Name || declared(m.GetSpec(), o.Name) != nil) {
			keys = append(keys, controller.Key{Project: o.Project, Name: m.GetMetadata().GetName()})
		}
	}
	return keys
}

// waitForShares records that machine k waits for its shares to serve it.
func (a *Agent) waitForShares(k controller.Key) {
	a.waitMu.Lock()
	defer a.waitMu.Unlock()
	a.shareWaiters[k] = true
}

// requeueShareWaiters queues again the machines waiting for their shares,
// as a server took an export or a network's firewall was written: each
// still waiting records itself again.
func (a *Agent) requeueShareWaiters() {
	a.waitMu.Lock()
	keys := make([]controller.Key, 0, len(a.shareWaiters))
	for k := range a.shareWaiters {
		keys = append(keys, k)
	}
	clear(a.shareWaiters)
	a.waitMu.Unlock()
	if len(keys) > 0 && a.requeue != nil {
		a.requeue(keys...)
	}
}
