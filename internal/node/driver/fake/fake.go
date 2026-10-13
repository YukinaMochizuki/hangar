// Package fake simulates a whole host behind the node's driver interfaces,
// for tests: a registry, storage, networking, VMs and their guests, sharing
// one state so they check each other as a host would (a VM needs its root
// disk, an interface needs its network). Tests inject faults and inspect
// what the host holds.
package fake

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

// Host is a simulated host. The zero value is not usable; call New.
type Host struct {
	mu sync.Mutex

	tags      map[string]string             // reference to digest
	configs   map[string]driver.ImageConfig // by digest; a digest stays pullable
	pullErr   map[string]error              // by reference
	unpackErr map[string]error              // by reference, or digest
	pulled    map[string]bool               // digests in the host's cache
	resolved  map[string]int                // resolutions, by reference

	bases     map[string]int64 // sizes, by digest
	baseSize  int64            // of every base made from now on
	building  map[string]*sync.Mutex
	roots     map[string]driver.Disk
	volumes   map[string]driver.Volume // by disk uid
	full      bool
	heldDisks map[string]int           // root disks in use, by machine uid
	heldVols  map[string]int           // volumes in use, by disk uid
	held      map[string]chan struct{} // unpacks waiting to be let go, by reference

	// vmBases are disk images' bases' sizes, by digest; diskImages what the
	// fake image servers serve, by URL, and fetches the fetches of each.
	vmBases    map[string]int64
	diskImages map[string]fakeDiskImage
	fetches    map[string]int
	// Unpacked counts how often each digest was unpacked into a base.
	unpacked map[string]int
	// unpacks counts the unpacks begun, by reference; unpacking, those
	// under way.
	unpacks         map[string]int
	unpacking, most int

	networks   map[string]driver.NetworkSpec
	interfaces map[string]map[string]driver.Interface // machine uid, then name
	// instances are the networks' namespaces', by network uid; sockets, every
	// socket handed out and maybe still open.
	instances     map[string]string
	instanceCount int
	sockets       []*socket
	// firewalls are the policies ensured, by network uid, then machine uid
	// and interface name.
	firewalls map[string]map[string]secgroup.Policy
	// firewallLimit, if not 0, bounds a firewall in place of
	// driver.MaxFirewallRules.
	firewallLimit int

	vms        map[string]*vm
	logs       map[string]*consoleLog // console logs kept, by machine uid
	logChanged chan struct{}          // closed when any log is written
	logReaders int
	stops      map[string]int               // guest Stops asked for, by machine uid
	bootErr    map[string]error             // by machine uid; "" for every machine
	silent     map[string]bool              // guests that never answer
	hung       map[string]bool              // guests that take a call and never return it
	stopDelay  map[string]*stopDelay        // by machine uid; "" for every machine
	stopCalls  map[string]int               // Hypervisor.Stop calls, failed ones too, by machine uid
	readyCalls map[string]int               // Guest.Ready calls, by machine uid
	execs      map[string]map[*process]bool // exec processes running, by machine uid
	listErr    map[string]error             // by listing: vms, interfaces, networks, roots, bases
	removalErr error                        // fails stopping VMs and removing disks and interfaces
	netErr     error                        // fails making and listing networks and interfaces
	cloneErr   error                        // fails making root disks
	capErr     error                        // fails reading the storage's capacity
	vmChanges  chan struct{}
	netChange  chan struct{}
}

type vm struct {
	driver.VM
	spec driver.VMSpec
	// Boots counts how often the VM started, restarts included.
	boots int
	// deadNICs says an interface was taken from under the running VM: the
	// tap it holds is gone, whatever is recreated by the same name.
	deadNICs bool
}

// New returns an empty host.
func New() *Host {
	return &Host{
		tags: map[string]string{}, configs: map[string]driver.ImageConfig{}, pullErr: map[string]error{}, pulled: map[string]bool{},
		unpackErr: map[string]error{}, resolved: map[string]int{}, hung: map[string]bool{},
		bases: map[string]int64{}, baseSize: DefaultBaseSize, building: map[string]*sync.Mutex{}, roots: map[string]driver.Disk{}, unpacked: map[string]int{}, unpacks: map[string]int{},
		held: map[string]chan struct{}{}, heldDisks: map[string]int{}, volumes: map[string]driver.Volume{}, heldVols: map[string]int{},
		vmBases: map[string]int64{}, diskImages: map[string]fakeDiskImage{}, fetches: map[string]int{},
		networks: map[string]driver.NetworkSpec{}, interfaces: map[string]map[string]driver.Interface{}, instances: map[string]string{},
		firewalls: map[string]map[string]secgroup.Policy{},
		vms:       map[string]*vm{}, logs: map[string]*consoleLog{}, logChanged: make(chan struct{}), stops: map[string]int{}, bootErr: map[string]error{}, silent: map[string]bool{}, listErr: map[string]error{},
		stopDelay: map[string]*stopDelay{}, stopCalls: map[string]int{}, readyCalls: map[string]int{},
		vmChanges: make(chan struct{}, 1), netChange: make(chan struct{}, 1),
	}
}

// Images, DiskImages, Storage, Network, Hypervisor and Guest are the host's
// drivers.
func (h *Host) Images() driver.Images         { return images{h} }
func (h *Host) DiskImages() driver.DiskImages { return diskImages{h} }
func (h *Host) Storage() driver.Storage       { return storage{h} }
func (h *Host) Network() driver.Network       { return network{h} }
func (h *Host) Hypervisor() driver.Hypervisor { return hypervisor{h} }
func (h *Host) Guest() driver.Guest           { return guest{h} }

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// --- what tests set up and inject ---

// AddImage publishes an image under ref. What ref named before stays
// pullable by its digest, as in a registry.
func (h *Host) AddImage(ref, digest string, config driver.ImageConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tags[ref] = digest
	h.configs[digest] = config
}

// HoldUnpack makes unpacking ref wait, as a large image does, until the
// returned function lets it go.
func (h *Host) HoldUnpack(ref string) (letGo func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan struct{})
	h.held[ref] = ch
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.held[ref] == ch {
			delete(h.held, ref)
			close(ch)
		}
	}
}

// FailUnpack makes unpacking ref, a reference or a digest, fail with err, as
// a layer that cannot be fetched, or an image at fault, does; nil clears
// it.
func (h *Host) FailUnpack(ref string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unpackErr[ref] = err
}

// HangGuest makes the machine's guest agent take a call and never return
// it, until the caller gives up.
func (h *Host) HangGuest(machineUID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hung[machineUID] = true
}

// stopDelay is how long stopping a running VM takes, unless let go first.
type stopDelay struct {
	d     time.Duration
	letGo chan struct{}
}

// DelayStop makes stopping the machine's VM while it runs take twice d; ""
// delays every machine's. The VM stays listed running for d, as a guest
// ignoring SIGTERM makes the real driver wait out the grace period, then is
// gone from the list for d more before Stop returns, as the real driver
// tidies up after its unit. A stop whose context ends first fails with it.
// letGo ends the delay of the stops waiting and of those to come.
func (h *Host) DelayStop(machineUID string, d time.Duration) (letGo func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sd := &stopDelay{d: d, letGo: make(chan struct{})}
	h.stopDelay[machineUID] = sd
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			close(sd.letGo)
			if h.stopDelay[machineUID] == sd {
				delete(h.stopDelay, machineUID)
			}
		})
	}
}

// Resolved returns how often ref was resolved.
func (h *Host) Resolved(ref string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.resolved[ref]
}

// FailPull makes resolving ref fail with err; nil clears it.
func (h *Host) FailPull(ref string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pullErr[ref] = err
}

// DefaultBaseSize is the size of a base unless SetBaseSize says otherwise.
const DefaultBaseSize = 64 << 20

// Capacity is the fake storage's size.
const Capacity = 1 << 40

// SetBaseSize sets the size of the bases made from now on, as a larger image
// makes a larger one.
func (h *Host) SetBaseSize(size int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.baseSize = size
}

// HoldDisk keeps a machine's root disk in use, as a running VM does, until
// the returned function lets it go.
func (h *Host) HoldDisk(machineUID string) (release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.heldDisks[machineUID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.heldDisks[machineUID]--
		})
	}
}

// HoldVolume keeps a Disk's volume in use, as a running VM does, until the
// returned function lets it go.
func (h *Host) HoldVolume(diskUID string) (release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.heldVols[diskUID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.heldVols[diskUID]--
		})
	}
}

// SetFull makes storage refuse every new disk with driver.ErrNoSpace.
func (h *Host) SetFull(full bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.full = full
}

// FailBoot makes starting the machine's VM fail with err; machineUID ""
// fails every machine, and nil clears it.
func (h *Host) FailBoot(machineUID string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bootErr[machineUID] = err
}

// Listings of what the host holds, for FailListing.
const (
	ListVMs        = "vms"
	ListInterfaces = "interfaces"
	ListNetworks   = "networks"
	ListRoots      = "roots"
	ListBases      = "bases"
	ListVolumes    = "volumes"
	ListImageBases = "imagebases"
	ListLogs       = "logs"
)

// FailListing makes the listings named, or every one if none is, fail with
// err; nil clears them.
func (h *Host) FailListing(err error, lists ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(lists) == 0 {
		lists = []string{ListVMs, ListInterfaces, ListNetworks, ListRoots, ListBases, ListVolumes, ListImageBases, ListLogs}
	}
	for _, l := range lists {
		h.listErr[l] = err
	}
}

// FailRemoval makes stopping VMs and removing root disks, volumes and
// interfaces fail with err; nil clears it.
func (h *Host) FailRemoval(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removalErr = err
}

// FailNetworking makes making and listing networks and interfaces fail with
// err, as a netlink error does; nil clears it.
func (h *Host) FailNetworking(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.netErr = err
}

// FailCapacity makes reading the storage's capacity fail with err; nil
// clears it.
func (h *Host) FailCapacity(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.capErr = err
}

// FailClone makes making root disks and volumes fail with err; nil clears
// it.
func (h *Host) FailClone(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cloneErr = err
}

// Silence keeps the machine's guest agent from answering, or lets it again.
func (h *Host) Silence(machineUID string, silent bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.silent[machineUID] = silent
}

// Exit ends the guest process of a running VM with code; the VM's restart
// policy decides whether it starts again.
func (h *Host) Exit(machineUID string, code int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exitLocked(machineUID, code)
}

func (h *Host) exitLocked(machineUID string, code int) error {
	v, ok := h.vms[machineUID]
	if !ok || v.State != driver.VMRunning {
		return fmt.Errorf("no running VM for machine %s", machineUID)
	}
	h.writeLog(machineUID, console.ExitRecord(console.ExitStatus{Code: int32(code)}, time.Now()))
	h.endExecsLocked(machineUID)
	switch {
	case v.spec.Restart == driver.RestartAlways,
		v.spec.Restart == driver.RestartOnFailure && code != 0:
		v.boots++
		v.Started = time.Now()
		h.boot(machineUID)
	default:
		v.State = driver.VMExited
		v.ExitCode = code
	}
	signal(h.vmChanges)
	return nil
}

// LoseNetwork removes a network and its interfaces behind the agent's back,
// as a networkd reconfiguration can, and says so on Changes.
func (h *Host) LoseNetwork(networkUID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removeNetwork(networkUID)
	signal(h.netChange)
}

// removeNetwork takes a network and every interface in it off the host,
// cutting off the VMs that held them. Callers hold h.mu.
func (h *Host) removeNetwork(networkUID string) {
	delete(h.networks, networkUID)
	delete(h.instances, networkUID)
	delete(h.firewalls, networkUID)
	for uid, ifs := range h.interfaces {
		for name, i := range ifs {
			if i.NetworkUID != networkUID {
				continue
			}
			delete(ifs, name)
			if v, ok := h.vms[uid]; ok && v.State == driver.VMRunning {
				v.deadNICs = true
			}
		}
		if len(ifs) == 0 {
			delete(h.interfaces, uid)
		}
	}
}

// LoseBase removes the base of digest behind the agent's back, as an
// administrator cleaning up by hand might.
func (h *Host) LoseBase(digest string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.bases, digest)
}

// Leave puts objects on the host that no machine accounts for, as a crash
// between steps leaves them.
func (h *Host) Leave(machineUID, networkUID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bases["sha256:orphan"] = h.baseSize
	h.roots[machineUID] = driver.Disk{MachineUID: machineUID, Digest: "sha256:orphan", Path: "/dev/fake/" + machineUID}
	h.networks[networkUID] = driver.NetworkSpec{UID: networkUID}
	h.newInstance(networkUID)
	h.interfaces[machineUID] = map[string]driver.Interface{"eth0": {MachineUID: machineUID, Name: "eth0", NetworkUID: networkUID}}
	h.vms[machineUID] = &vm{VM: driver.VM{MachineUID: machineUID, State: driver.VMRunning, Started: time.Now()}, boots: 1}
	h.logOf(machineUID)
}

// LeaveVolume puts a Disk's volume on the host, as a node that ran it
// leaves it.
func (h *Host) LeaveVolume(diskUID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.volumes[diskUID] = driver.Volume{DiskUID: diskUID, Path: "/dev/fake/disks/" + diskUID, Size: 1 << 30}
}

// LeaveLog puts a console log on the host, of a machine whose VM is gone.
func (h *Host) LeaveLog(machineUID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logOf(machineUID)
}

// --- what tests inspect ---

// HypervisorStops returns how often the machine's VM was asked to stop,
// failed calls included.
func (h *Host) HypervisorStops(machineUID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stopCalls[machineUID]
}

// GuestReadies returns how often the machine's guest agent was asked whether
// it answers, as each reconcile of a running machine does.
func (h *Host) GuestReadies(machineUID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.readyCalls[machineUID]
}

// GuestStops returns how often the machine's guest was asked to stop.
func (h *Host) GuestStops(machineUID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stops[machineUID]
}

// Snapshot is what the host holds.
type Snapshot struct {
	Bases      []string
	Roots      map[string]driver.Disk
	Volumes    map[string]driver.Volume
	ImageBases []string
	Networks   map[string]driver.NetworkSpec
	Interfaces map[string][]driver.Interface
	VMs        map[string]VMView
	// Logs are the machines whose console logs the host keeps.
	Logs []string
	// Listening are the sockets still open, such as "udp 10.0.0.1:53", by
	// network uid, including those of a namespace since made again.
	Listening map[string][]string
}

// VMView is a VM as a test sees it.
type VMView struct {
	driver.VM
	Spec  driver.VMSpec
	Boots int
	// DeadNICs says the running instance lost an interface from under it.
	DeadNICs bool
}

// Empty reports whether the host holds nothing at all.
func (s Snapshot) Empty() bool {
	return len(s.Bases) == 0 && len(s.Roots) == 0 && len(s.Volumes) == 0 && len(s.ImageBases) == 0 && len(s.Networks) == 0 && len(s.Interfaces) == 0 &&
		len(s.VMs) == 0 && len(s.Logs) == 0 && len(s.Listening) == 0
}

// Snapshot returns a copy of what the host holds.
func (h *Host) Snapshot() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := Snapshot{
		Bases: slices.Sorted(maps.Keys(h.bases)), Roots: maps.Clone(h.roots), Volumes: maps.Clone(h.volumes), ImageBases: slices.Sorted(maps.Keys(h.vmBases)), Networks: maps.Clone(h.networks),
		Interfaces: map[string][]driver.Interface{}, VMs: map[string]VMView{}, Logs: slices.Sorted(maps.Keys(h.logs)),
		Listening: h.listening(),
	}
	for uid, ifs := range h.interfaces {
		for _, name := range slices.Sorted(maps.Keys(ifs)) {
			s.Interfaces[uid] = append(s.Interfaces[uid], ifs[name])
		}
	}
	for uid, v := range h.vms {
		s.VMs[uid] = VMView{VM: v.VM, Spec: v.spec, Boots: v.boots, DeadNICs: v.deadNICs}
	}
	return s
}

// Cached reports whether the host's image cache holds digest.
func (h *Host) Cached(digest string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pulled[digest]
}

// Unpacks returns how often unpacking ref was begun, how many unpacks are
// under way, and the most ever under way at once.
func (h *Host) Unpacks(ref string) (begun, underWay, most int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.unpacks[ref], h.unpacking, h.most
}

// Unpacked returns how often the image with digest was unpacked.
func (h *Host) Unpacked(digest string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.unpacked[digest]
}

// --- Images ---

type images struct{ h *Host }

func (d images) Resolve(_ context.Context, ref string, _ driver.Credentials) (string, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	d.h.resolved[ref]++
	if err := d.h.pullErr[ref]; err != nil {
		return "", err
	}
	if _, digest, pinned := strings.Cut(ref, "@"); pinned {
		if _, ok := d.h.configs[digest]; ok {
			return digest, nil
		}
	}
	digest, ok := d.h.tags[ref]
	if !ok {
		return "", fmt.Errorf("%s: %w", ref, driver.ErrImageNotFound)
	}
	return digest, nil
}

// byDigest looks an image up in the host's cache, or else pulls it as a
// registry gives ref@digest.
func (d images) byDigest(ref, digest string) (driver.ImageConfig, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if err := d.h.pullErr[ref]; err != nil && !d.h.pulled[digest] {
		return driver.ImageConfig{}, err
	}
	config, ok := d.h.configs[digest]
	if !ok {
		return driver.ImageConfig{}, fmt.Errorf("%s@%s: %w", ref, digest, driver.ErrImageNotFound)
	}
	d.h.pulled[digest] = true
	return config, nil
}

func (d images) Config(_ context.Context, ref, digest string, _ driver.Credentials) (driver.ImageConfig, error) {
	return d.byDigest(ref, digest)
}

func (d images) Forget(_ context.Context, digest string) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	delete(d.h.pulled, digest)
	return nil
}

func (d images) Unpack(ctx context.Context, ref, digest, dir string, _ driver.Credentials) error {
	if _, err := d.byDigest(ref, digest); err != nil {
		return err
	}
	d.h.mu.Lock()
	held, failed := d.h.held[ref], cmp.Or(d.h.unpackErr[ref], d.h.unpackErr[digest])
	d.h.unpacks[ref]++
	d.h.unpacking++
	d.h.most = max(d.h.most, d.h.unpacking)
	d.h.mu.Unlock()
	defer func() {
		d.h.mu.Lock()
		d.h.unpacking--
		d.h.mu.Unlock()
	}()
	if failed != nil {
		return failed
	}
	if held != nil {
		select {
		case <-held:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return os.WriteFile(filepath.Join(dir, "image"), []byte(digest), 0o644)
}

// --- Storage ---

type storage struct{ h *Host }

func (d storage) EnsureBase(_ context.Context, digest string, size int64, fill func(dir string) error) error {
	d.h.mu.Lock()
	one := d.h.building[digest]
	if one == nil {
		one = &sync.Mutex{}
		d.h.building[digest] = one
	}
	d.h.mu.Unlock()
	one.Lock() // one maker per digest
	defer one.Unlock()

	d.h.mu.Lock()
	if _, ok := d.h.bases[digest]; ok {
		d.h.mu.Unlock()
		return nil
	}
	full := d.h.full || size > Capacity-d.h.usedLocked()
	d.h.mu.Unlock()
	if full {
		return driver.ErrNoSpace
	}
	dir, err := os.MkdirTemp("", "fake-base")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := fill(dir); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "image")); err != nil {
		return fmt.Errorf("the base for %s was left empty", digest)
	}
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	d.h.bases[digest] = d.h.baseSize
	d.h.unpacked[digest]++
	return nil
}

func (d storage) Bases(context.Context) ([]string, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if err := d.h.listErr[ListBases]; err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(d.h.bases)), nil
}

func (d storage) DeleteBase(_ context.Context, digest string) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	for _, r := range d.h.roots {
		if r.Digest == digest {
			return fmt.Errorf("base %s has root disks cloned from it", digest)
		}
	}
	delete(d.h.bases, digest)
	return nil
}

func (d storage) EnsureRoot(_ context.Context, machineUID, digest string, size int64) (driver.Disk, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if r, ok := d.h.roots[machineUID]; ok {
		if r.Digest != digest {
			return driver.Disk{}, fmt.Errorf("machine %s has a root disk of %s, not %s", machineUID, r.Digest, digest)
		}
		if r.Size < size {
			r.Size = size
			d.h.roots[machineUID] = r
		}
		return r, nil
	}
	baseSize, ok := d.h.bases[digest]
	if !ok {
		return driver.Disk{}, fmt.Errorf("no base for %s", digest)
	}
	if size < baseSize {
		return driver.Disk{}, fmt.Errorf("%w: %d bytes asked, the base of %s has %d", driver.ErrDiskTooSmall, size, digest, baseSize)
	}
	if d.h.cloneErr != nil {
		return driver.Disk{}, d.h.cloneErr
	}
	if d.h.full {
		return driver.Disk{}, driver.ErrNoSpace
	}
	r := driver.Disk{MachineUID: machineUID, Digest: digest, Path: "/dev/fake/" + machineUID, Size: size}
	d.h.roots[machineUID] = r
	return r, nil
}

func (d storage) Roots(context.Context) ([]driver.Disk, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if err := d.h.listErr[ListRoots]; err != nil {
		return nil, err
	}
	return slices.Collect(maps.Values(d.h.roots)), nil
}

func (d storage) DeleteRoot(_ context.Context, machineUID string) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.removalErr != nil {
		return d.h.removalErr
	}
	if v, ok := d.h.vms[machineUID]; ok && v.State == driver.VMRunning || d.h.heldDisks[machineUID] > 0 {
		return fmt.Errorf("the root disk of machine %s is in use", machineUID)
	}
	delete(d.h.roots, machineUID)
	return nil
}

func (d storage) EnsureImageBase(_ context.Context, digest string, size int64, fill func(dev string) error) error {
	d.h.mu.Lock()
	if _, ok := d.h.vmBases[digest]; ok {
		d.h.mu.Unlock()
		return nil
	}
	full := d.h.full || size > Capacity-d.h.usedLocked()
	d.h.mu.Unlock()
	if full {
		return driver.ErrNoSpace
	}
	if err := fill("/dev/fake/vmbases/" + digest); err != nil {
		return fmt.Errorf("filling the base of disk image %s: %w", digest, err)
	}
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	d.h.vmBases[digest] = size
	return nil
}

func (d storage) ImageBases(context.Context) ([]string, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if err := d.h.listErr[ListImageBases]; err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(d.h.vmBases)), nil
}

func (d storage) DeleteImageBase(_ context.Context, digest string) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	for _, v := range d.h.volumes {
		if v.Digest == digest {
			return fmt.Errorf("disk image %s has volumes cloned from it", digest)
		}
	}
	delete(d.h.vmBases, digest)
	return nil
}

func (d storage) EnsureVolume(_ context.Context, diskUID string, size int64, digest string) (driver.Volume, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if v, ok := d.h.volumes[diskUID]; ok {
		if v.Digest != digest {
			return driver.Volume{}, fmt.Errorf("disk %s has a volume of %q, not %q", diskUID, v.Digest, digest)
		}
		if v.Size < size {
			v.Size = size
			d.h.volumes[diskUID] = v
		}
		return v, nil
	}
	if d.h.cloneErr != nil {
		return driver.Volume{}, d.h.cloneErr
	}
	if d.h.full {
		return driver.Volume{}, driver.ErrNoSpace
	}
	v := driver.Volume{DiskUID: diskUID, Path: "/dev/fake/disks/" + diskUID, Size: max(size, 1), Digest: digest}
	if digest != "" {
		base, ok := d.h.vmBases[digest]
		if !ok {
			return driver.Volume{}, fmt.Errorf("no base for disk image %s", digest)
		}
		if size < base {
			return driver.Volume{}, fmt.Errorf("%w: %d bytes asked, disk image %s has %d", driver.ErrDiskTooSmall, size, digest, base)
		}
	}
	d.h.volumes[diskUID] = v
	return v, nil
}

func (d storage) Volumes(context.Context) ([]driver.Volume, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if err := d.h.listErr[ListVolumes]; err != nil {
		return nil, err
	}
	return slices.Collect(maps.Values(d.h.volumes)), nil
}

func (d storage) DeleteVolume(_ context.Context, diskUID string) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.removalErr != nil {
		return d.h.removalErr
	}
	if d.h.heldVols[diskUID] > 0 {
		return fmt.Errorf("the volume of disk %s is in use", diskUID)
	}
	for _, v := range d.h.vms {
		if v.State == driver.VMRunning && slices.ContainsFunc(v.spec.Disks, func(x driver.Volume) bool { return x.DiskUID == diskUID }) {
			return fmt.Errorf("the volume of disk %s is in use by machine %s", diskUID, v.MachineUID)
		}
	}
	delete(d.h.volumes, diskUID)
	return nil
}

func (d storage) Capacity(context.Context) (driver.Capacity, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.capErr != nil {
		return driver.Capacity{}, d.h.capErr
	}
	if d.h.full {
		return driver.Capacity{Total: Capacity}, nil
	}
	return driver.Capacity{Total: Capacity, Free: Capacity - d.h.usedLocked()}, nil
}

// usedLocked is what the bases take. Callers hold h.mu.
func (h *Host) usedLocked() int64 {
	used := int64(0)
	for _, size := range h.bases {
		used += size
	}
	for _, size := range h.vmBases {
		used += size
	}
	return used
}

// --- Disk images ---

type fakeDiskImage struct {
	digest string
	size   int64
	err    error
}

// AddDiskImage serves at url a disk image of digest holding a disk of size
// bytes.
func (h *Host) AddDiskImage(url, digest string, size int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.diskImages[url] = fakeDiskImage{digest: digest, size: size}
}

// FailDiskImage makes fetching url fail with err; nil clears it.
func (h *Host) FailDiskImage(url string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	i := h.diskImages[url]
	i.err = err
	h.diskImages[url] = i
}

// Fetches returns how often url was fetched, failed fetches included.
func (h *Host) Fetches(url string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fetches[url]
}

type diskImages struct{ h *Host }

func (d diskImages) Fetch(ctx context.Context, url, digest string) (driver.FetchedImage, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	d.h.fetches[url]++
	i, ok := d.h.diskImages[url]
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case i.err != nil:
		return nil, i.err
	case !ok || i.digest == "":
		return nil, fmt.Errorf("fetching %s: 404 Not Found", url)
	case i.digest != digest:
		return nil, fmt.Errorf("%s has %s, not %s: %w", url, i.digest, digest, driver.ErrDigestMismatch)
	}
	return fetchedImage{i.size}, nil
}

type fetchedImage struct{ size int64 }

func (f fetchedImage) Size() int64 { return f.size }

func (f fetchedImage) WriteTo(context.Context, string) error { return nil }

func (f fetchedImage) Close() error { return nil }

// --- Network ---

type network struct{ h *Host }

func (d network) EnsureNetwork(_ context.Context, n driver.NetworkSpec) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.netErr != nil {
		return d.h.netErr
	}
	if !n.CIDR.IsValid() || !n.Transit.IsValid() {
		return fmt.Errorf("network %s needs a range and a transit link", n.UID)
	}
	if _, ok := d.h.networks[n.UID]; !ok {
		d.h.newInstance(n.UID)
	}
	d.h.networks[n.UID] = n
	return nil
}

func (d network) Networks(context.Context) ([]driver.NetworkSpec, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if err := cmp.Or(d.h.listErr[ListNetworks], d.h.netErr); err != nil {
		return nil, err
	}
	return slices.Collect(maps.Values(d.h.networks)), nil
}

// DeleteNetwork does what a real driver does when asked to delete a network
// still in use: it deletes it, and its VMs lose their interfaces.
func (d network) DeleteNetwork(_ context.Context, networkUID string) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	d.h.removeNetwork(networkUID)
	return nil
}

func (d network) EnsureInterface(_ context.Context, i driver.Interface) (driver.Interface, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.netErr != nil {
		return driver.Interface{}, d.h.netErr
	}
	n, ok := d.h.networks[i.NetworkUID]
	if !ok {
		return driver.Interface{}, fmt.Errorf("no network %s for machine %s's %s", i.NetworkUID, i.MachineUID, i.Name)
	}
	if !n.CIDR.Contains(i.Address.Addr()) {
		return driver.Interface{}, fmt.Errorf("%s is outside network %s (%s)", i.Address, i.NetworkUID, n.CIDR)
	}
	sum := sha256.Sum256([]byte(i.MachineUID + "/" + i.Name))
	i.Device = "hm" + hex.EncodeToString(sum[:])[:13]
	if d.h.interfaces[i.MachineUID] == nil {
		d.h.interfaces[i.MachineUID] = map[string]driver.Interface{}
	}
	d.h.interfaces[i.MachineUID][i.Name] = i
	return i, nil
}

func (d network) Interfaces(context.Context) ([]driver.Interface, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if err := cmp.Or(d.h.listErr[ListInterfaces], d.h.netErr); err != nil {
		return nil, err
	}
	var all []driver.Interface
	for _, ifs := range d.h.interfaces {
		all = append(all, slices.Collect(maps.Values(ifs))...)
	}
	return all, nil
}

func (d network) DeleteInterfaces(_ context.Context, machineUID string) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.removalErr != nil {
		return d.h.removalErr
	}
	if v, ok := d.h.vms[machineUID]; ok && v.State == driver.VMRunning {
		return fmt.Errorf("the interfaces of machine %s are in use by its VM", machineUID)
	}
	delete(d.h.interfaces, machineUID)
	return nil
}

func (d network) Changes() <-chan struct{} { return d.h.netChange }

// --- Hypervisor ---

type hypervisor struct{ h *Host }

// errUnknownVM is what a guest of no running VM answers.
var errUnknownVM = errors.New("no running VM")

func (d hypervisor) Start(_ context.Context, spec driver.VMSpec) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	hash := spec.Hash()
	if v, ok := d.h.vms[spec.MachineUID]; ok {
		if v.SpecHash != hash {
			return fmt.Errorf("machine %s's VM runs another spec; stop it first", spec.MachineUID)
		}
		if v.State == driver.VMRunning {
			return nil
		}
	}
	if err := d.h.bootErr[spec.MachineUID]; err != nil {
		return err
	}
	if err := d.h.bootErr[""]; err != nil {
		return err
	}
	if r, ok := d.h.roots[spec.MachineUID]; !ok || r != spec.Root {
		return fmt.Errorf("machine %s has no root disk %s", spec.MachineUID, spec.Root.Path)
	}
	for _, v := range spec.Disks {
		if have, ok := d.h.volumes[v.DiskUID]; !ok || have != v {
			return fmt.Errorf("machine %s has no volume %s as specified", spec.MachineUID, v.Path)
		}
	}
	for _, i := range spec.Interfaces {
		if have, ok := d.h.interfaces[spec.MachineUID][i.Name]; !ok || have != i {
			return fmt.Errorf("machine %s has no interface %s as specified", spec.MachineUID, i.Name)
		}
	}
	boots := 1
	if v, ok := d.h.vms[spec.MachineUID]; ok {
		boots = v.boots + 1
	}
	d.h.vms[spec.MachineUID] = &vm{
		VM:    driver.VM{MachineUID: spec.MachineUID, SpecHash: hash, State: driver.VMRunning, Started: time.Now(), Run: spec.Run},
		spec:  spec,
		boots: boots,
	}
	d.h.boot(spec.MachineUID)
	return nil
}

// Stop asks a running VM's guest to stop its process first, as the real
// driver does, unless the guest does not answer, and takes as long as
// DelayStop says.
func (d hypervisor) Stop(ctx context.Context, machineUID string) error {
	d.h.mu.Lock()
	d.h.stopCalls[machineUID]++
	if err := d.h.removalErr; err != nil {
		d.h.mu.Unlock()
		return err
	}
	v, ok := d.h.vms[machineUID]
	running := ok && v.State == driver.VMRunning
	if running && !d.h.hung[machineUID] && !d.h.silent[machineUID] {
		d.h.stops[machineUID]++
	}
	delay := d.h.stopDelay[machineUID]
	if delay == nil {
		delay = d.h.stopDelay[""]
	}
	d.h.mu.Unlock()
	wait := func() error {
		if !running || delay == nil {
			return nil
		}
		t := time.NewTimer(delay.d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case <-delay.letGo:
		}
		return nil
	}
	if err := wait(); err != nil {
		return err
	}
	d.h.mu.Lock()
	delete(d.h.vms, machineUID)
	d.h.endExecsLocked(machineUID)
	d.h.mu.Unlock()
	return wait()
}

func (d hypervisor) VMs(context.Context) ([]driver.VM, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if err := d.h.listErr[ListVMs]; err != nil {
		return nil, err
	}
	var all []driver.VM
	for _, v := range d.h.vms {
		all = append(all, v.VM)
	}
	return all, nil
}

func (d hypervisor) Changes() <-chan struct{} { return d.h.vmChanges }

func (d hypervisor) Logs(context.Context) ([]string, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if err := d.h.listErr[ListLogs]; err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(d.h.logs)), nil
}

func (d hypervisor) DeleteLogs(_ context.Context, machineUID string) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.removalErr != nil {
		return d.h.removalErr
	}
	if _, ok := d.h.vms[machineUID]; ok {
		return fmt.Errorf("machine %s's VM is listed; stop it first", machineUID)
	}
	delete(d.h.logs, machineUID)
	return nil
}

// --- Guest ---

type guest struct{ h *Host }

func (d guest) Ready(ctx context.Context, machineUID string) error {
	d.h.mu.Lock()
	d.h.readyCalls[machineUID]++
	hung := d.h.hung[machineUID]
	d.h.mu.Unlock()
	if hung {
		<-ctx.Done()
		return ctx.Err()
	}
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	v, ok := d.h.vms[machineUID]
	if !ok || v.State != driver.VMRunning {
		return errUnknownVM
	}
	if d.h.silent[machineUID] {
		return fmt.Errorf("the guest agent of machine %s does not answer", machineUID)
	}
	return nil
}

// Stop ends the guest's process as SIGTERM does, at once: the VM exits with
// 143 and its restart policy decides what comes next.
func (d guest) Stop(ctx context.Context, machineUID string, _ time.Duration) error {
	d.h.mu.Lock()
	hung := d.h.hung[machineUID]
	d.h.stops[machineUID]++
	d.h.mu.Unlock()
	if hung {
		<-ctx.Done()
		return ctx.Err()
	}
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if v, ok := d.h.vms[machineUID]; !ok || v.State != driver.VMRunning {
		return errUnknownVM
	}
	if d.h.silent[machineUID] {
		return fmt.Errorf("the guest agent of machine %s does not answer", machineUID)
	}
	return d.h.exitLocked(machineUID, 143)
}
