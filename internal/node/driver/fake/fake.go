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
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// Host is a simulated host. The zero value is not usable; call New.
type Host struct {
	mu sync.Mutex

	tags      map[string]string             // reference to digest
	configs   map[string]driver.ImageConfig // by digest; a digest stays pullable
	pullErr   map[string]error              // by reference
	private   map[string]driver.Login       // by repository, the login it asks for
	unpackErr map[string]error              // by reference, or digest
	pulled    map[string]time.Time          // digests in the host's cache, when last used
	pulls     map[string]int                // fetches in full from a registry, by digest
	// cacheSize is the bytes the image room holds: the layers, as
	// ImageConfig.Size counts them, of the digests in the cache, and an
	// unpack under way.
	cacheSize int64
	// unpackedSize is what each digest unpacks to, by digest; its
	// ImageConfig.Size if none.
	unpackedSize map[string]int64
	// imageTaken is what else takes the image room, as downloads and
	// unpacks under way do.
	imageTaken int64
	// deferFrees says the room an unpack out of room took shows free only
	// once the next base is begun, as ZFS frees a destroyed file's blocks
	// after the call; freeing is that room, meanwhile.
	deferFrees bool
	freeing    int64
	// storageSize is the bytes the storage holds.
	storageSize int64
	// sources are, by digest in the cache, the repositories it was fetched
	// from; holds and claims, by repository, the digests a registry serves,
	// or only says it has.
	sources, holds, claims map[string]map[string]bool
	resolved               map[string]int // resolutions, by reference

	bases     map[string]int64 // sizes, by digest
	baseUsed  map[string]time.Time
	baseSize  int64 // of every base made from now on
	building  map[string]*sync.Mutex
	roots     map[string]driver.Disk
	volumes   map[string]driver.Volume // by disk uid
	full      bool
	heldDisks map[string]int // root disks in use, by machine uid
	heldVols  map[string]int // volumes in use, by disk uid
	// shareVols are FileShares' volumes, by share uid; exports what each
	// share's server holds, by machine uid, which exportErr refuses, by
	// machine uid or "" for every one; exportCalls counts SetExport.
	shareVols   map[string]driver.ShareVolume
	heldShares  map[string]int // share volumes in use, by share uid
	exports     map[string]driver.ShareExport
	exportErr   map[string]error
	exportCalls map[string]int
	// exportLog is, by machine uid, each SetBootExport ("boot") and
	// SetExport ("tell") asked, in order, failed ones included.
	exportLog map[string][]string
	// bootExportErr refuses SetBootExport.
	bootExportErr error
	// exportHolds keep SetExport, by machine uid, from answering until
	// closed or its context ends.
	exportHolds map[string]chan struct{}
	held        map[string]chan struct{} // unpacks waiting to be let go, by reference

	// vmBases are disk images' bases' sizes, by digest; diskImages what the
	// fake image servers serve, by URL, and fetches the fetches of each.
	vmBases    map[string]int64
	diskImages map[string]fakeDiskImage
	fetches    map[string]int
	// captures are the snapshots taken for CustomImages, by image uid;
	// imageUsed what each CustomImage's base holds, by key, which
	// usedPerMille of its size sets; captureErr fails captures, and
	// captureHold keeps them from ending until it is closed; capturing
	// counts the captures under way.
	captures     map[string]capture
	imageUsed    map[string]int64
	usedPerMille int64
	captureErr   error
	captureHold  chan struct{}
	capturing    int
	// inspects and converts count the converter's calls, by image uid;
	// imageInfo, if set, is what Inspect says of every file, and inspectErr
	// and convertErr fail each call; inspectHold and convertHold keep
	// inspections and conversions from ending until each is closed, and
	// converting counts the conversions under way.
	inspects, converts map[string]int
	imageInfo          *driver.ImageInfo
	inspectErr         error
	convertErr         error
	inspectHold        chan struct{}
	convertHold        chan struct{}
	converting         int
	// filling is the bytes of the custom images' bases being filled.
	filling int64
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
	// guests answer Dial; dials are what it was asked to reach.
	guests map[guestKey]func(net.Conn)
	dials  []string
	// firewalls are the policies ensured, by network uid, then machine uid
	// and interface name.
	firewalls map[string]map[string]driver.InterfacePolicy
	// firewallLimit, if not 0, bounds a firewall in place of
	// driver.MaxFirewallRules.
	firewallLimit int
	// ipv6Prefix is the host's, which interfaces' IPv6 addresses are of;
	// invalid for none. packetConns are every packet socket handed out.
	ipv6Prefix  netip.Prefix
	packetConns []*packetConn
	// sharedIPv4 is the host's shared address, invalid for none; forwards
	// are those in force, by protocol and port, which forwardWrites counts
	// the changes of.
	sharedIPv4    netip.Addr
	forwards      []driver.Forward
	forwardWrites int
	// natFollowers are the FollowNATs under way; natPort the last public
	// port handed out.
	natFollowers map[*natFollower]bool
	natPort      uint16

	vms        map[string]*vm
	logs       map[string]*consoleLog // console logs kept, by machine uid
	logChanged chan struct{}          // closed when any log is written
	logReaders int
	stops      map[string]int         // guest Stops asked for, by machine uid
	bootErr    map[string]error       // by machine uid; "" for every machine
	silent     map[string]bool        // guests that never answer
	hung       map[string]bool        // guests that take a call and never return it
	stopDelay  map[string]*stopDelay  // by machine uid; "" for every machine
	stopCalls  map[string][]time.Time // when Hypervisor.Stop was called, failed calls too, by machine uid
	readyCalls map[string]int         // Guest.Ready calls, by machine uid
	// guestIPv6 is what each container's guest holds of its public IPv6,
	// by machine uid; oldGuests cannot be told.
	guestIPv6 map[string][]driver.GuestIPv6
	oldGuests map[string]bool
	// ipv6Refused counts interfaces refused for an IPv6 address another
	// holds.
	ipv6Refused int
	execs       map[string]map[*process]bool // exec processes running, by machine uid
	consoles    map[string]*fakeConsole      // running firmware VMs' serial consoles, by machine uid
	listErr     map[string]error             // by listing: vms, interfaces, networks, roots, bases
	removalErr  error                        // fails stopping VMs and removing disks and interfaces
	netErr      error                        // fails making and listing networks and interfaces
	cloneErr    error                        // fails making root disks
	baseDelErr  error                        // fails deleting bases
	capErr      error                        // fails reading the storage's capacity
	vmChanges   chan struct{}
	netChange   chan struct{}
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
		tags: map[string]string{}, configs: map[string]driver.ImageConfig{}, pullErr: map[string]error{}, private: map[string]driver.Login{}, pulled: map[string]time.Time{},
		pulls: map[string]int{}, cacheSize: DefaultImageCacheSize, baseUsed: map[string]time.Time{},
		unpackedSize: map[string]int64{}, storageSize: Capacity,
		sources: map[string]map[string]bool{}, holds: map[string]map[string]bool{}, claims: map[string]map[string]bool{},
		unpackErr: map[string]error{}, resolved: map[string]int{}, hung: map[string]bool{},
		bases: map[string]int64{}, baseSize: DefaultBaseSize, building: map[string]*sync.Mutex{}, roots: map[string]driver.Disk{}, unpacked: map[string]int{}, unpacks: map[string]int{},
		held: map[string]chan struct{}{}, heldDisks: map[string]int{}, volumes: map[string]driver.Volume{}, heldVols: map[string]int{},
		vmBases: map[string]int64{}, diskImages: map[string]fakeDiskImage{}, fetches: map[string]int{},
		captures: map[string]capture{}, imageUsed: map[string]int64{}, usedPerMille: 500,
		inspects: map[string]int{}, converts: map[string]int{},
		networks: map[string]driver.NetworkSpec{}, interfaces: map[string]map[string]driver.Interface{}, instances: map[string]string{},
		firewalls: map[string]map[string]driver.InterfacePolicy{},
		shareVols: map[string]driver.ShareVolume{}, heldShares: map[string]int{}, exports: map[string]driver.ShareExport{}, exportErr: map[string]error{}, exportCalls: map[string]int{}, exportLog: map[string][]string{}, exportHolds: map[string]chan struct{}{},
		vms: map[string]*vm{}, consoles: map[string]*fakeConsole{}, logs: map[string]*consoleLog{}, logChanged: make(chan struct{}), stops: map[string]int{}, bootErr: map[string]error{}, silent: map[string]bool{}, listErr: map[string]error{},
		stopDelay: map[string]*stopDelay{}, stopCalls: map[string][]time.Time{}, readyCalls: map[string]int{},
		guestIPv6: map[string][]driver.GuestIPv6{}, oldGuests: map[string]bool{},
		vmChanges: make(chan struct{}, 1), netChange: make(chan struct{}, 1),
	}
}

// Images, DiskImages, Storage, Network, Hypervisor and Guest are the host's
// drivers.
func (h *Host) Images() driver.Images         { return images{h} }
func (h *Host) DiskImages() driver.DiskImages { return diskImages{h} }
func (h *Host) Storage() driver.Storage       { return storage{h} }
func (h *Host) Converter() driver.Converter   { return converter{h} }
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
	add(h.holds, repositoryOf(ref), digest)
}

// ClaimDigest has ref's repository answer a resolve of digest, as a
// registry an attacker runs can, though it cannot serve the image's
// content; a tag ref names it there too. It returns ref by digest.
func (h *Host) ClaimDigest(ref, digest string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	repo, _, _ := strings.Cut(ref, "@")
	h.tags[ref] = digest
	add(h.claims, repositoryOf(ref), digest)
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	return repo + "@" + digest
}

func add(m map[string]map[string]bool, k, v string) {
	if m[k] == nil {
		m[k] = map[string]bool{}
	}
	m[k][v] = true
}

// MakePrivate has the registry ask for login to anything of ref's
// repository, by tag or digest, as a private repository does: anything else
// is driver.ErrUnauthorized. What the host has pulled stays its own.
func (h *Host) MakePrivate(ref string, login driver.Login) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.private[repositoryOf(ref)] = login
}

// repositoryOf is a reference's repository, as driver.Repository gives it,
// or the reference itself for one it cannot read.
func repositoryOf(ref string) string {
	if repo, err := driver.Repository(ref); err == nil {
		return repo
	}
	return ref
}

// registryOf is the host a reference's registry is asked for logins by.
func registryOf(ref string) string {
	repo, _, _ := strings.Cut(ref, "@")
	if r, err := name.ParseReference(repo); err == nil {
		return r.Context().RegistryStr()
	}
	return ""
}

// authorize checks the login creds give for ref's registry against what a
// private repository asks for, as a registry does before it answers.
func (h *Host) authorize(ctx context.Context, ref string, creds driver.Credentials) error {
	h.mu.Lock()
	want, private := h.private[repositoryOf(ref)]
	h.mu.Unlock()
	if !private {
		return nil
	}
	got, err := creds(ctx, registryOf(ref))
	if err != nil {
		return fmt.Errorf("%s: %w", ref, err)
	}
	if got != want {
		return fmt.Errorf("%s: %w", ref, driver.ErrUnauthorized)
	}
	return nil
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
// it, until the caller gives up; the returned function has it answer the
// calls made after.
func (h *Host) HangGuest(machineUID string) (letGo func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hung[machineUID] = true
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.hung, machineUID)
	}
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

// ForgetSources drops what the host's cache knows of the repositories its
// content came from, keeping the content, as a cache written before sources
// were recorded is: each digest must be fetched in full again before the
// cache answers it.
func (h *Host) ForgetSources() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sources = map[string]map[string]bool{}
}

// DefaultBaseSize is the size of a base unless SetBaseSize says otherwise.
const DefaultBaseSize = 64 << 20

// DefaultImageCacheSize is the image cache's room unless SetImageCacheSize
// says otherwise.
const DefaultImageCacheSize = 100 << 30

// SetImageCacheSize sets the bytes the image cache may hold, as the
// quota of its dataset does.
func (h *Host) SetImageCacheSize(size int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cacheSize = size
}

// SetUnpackedSize sets what the image of digest unpacks to, in the image
// room beside the cache.
func (h *Host) SetUnpackedSize(digest string, size int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unpackedSize[digest] = size
}

// TakeImageRoom has n bytes of the image room taken by what is not the
// cache's, as a disk image downloaded or an unpack under way takes it.
func (h *Host) TakeImageRoom(n int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.imageTaken = n
}

// DeferFrees makes the room an unpack out of room took show free, in
// what Images.Cached says, only once the next base is begun, as ZFS shows
// a destroyed file's blocks free some seconds after.
func (h *Host) DeferFrees(on bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.deferFrees = on
}

// FailDeleteBase makes deleting bases fail with err; nil clears it.
func (h *Host) FailDeleteBase(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.baseDelErr = err
}

// SetStorageSize sets the bytes the storage holds, as the pool's size
// does.
func (h *Host) SetStorageSize(size int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.storageSize = size
}

// Pulls returns how often digest was fetched in full from a registry.
func (h *Host) Pulls(digest string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pulls[digest]
}

// Capacity is the fake storage's size unless SetStorageSize says
// otherwise.
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
	ListShares     = "shares"
	ListImageBases = "imagebases"
	ListLogs       = "logs"
	ListImageCache = "imagecache"
)

// FailListing makes the listings named, or every one if none is, fail with
// err; nil clears them.
func (h *Host) FailListing(err error, lists ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(lists) == 0 {
		lists = []string{ListVMs, ListInterfaces, ListNetworks, ListRoots, ListBases, ListVolumes, ListShares, ListImageBases, ListLogs, ListImageCache}
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
		h.bootGuest(v)
	default:
		v.State = driver.VMExited
		v.ExitCode = code
	}
	signal(h.vmChanges)
	return nil
}

// endFirmwareBootLocked ends the boot of a running firmware VM that the
// platform stopped, pressing its power button, as chvm's launcher records
// it: powered off, or killed if its guest does not answer.
func (h *Host) endFirmwareBootLocked(machineUID string) {
	code := int32(0)
	if h.hung[machineUID] || h.silent[machineUID] {
		code = 253
	}
	h.writeLog(machineUID, console.ExitRecord(console.ExitStatus{Code: code, Signal: driver.StoppedSignal}, time.Now()))
}

// Reboot shuts the host down and boots it again, the agent's to start
// what it wants running: as at a shutdown, each running VM is stopped
// without a stop asked of the driver, a firmware VM's by its power button
// and a container's by its guest's stop. As chvm lists them after, a VM
// whose restart policy is never has exited, with 253 (chvm.ExitNoRecord),
// and any other is gone, to be started again. Execs end.
func (h *Host) Reboot() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for uid, v := range h.vms {
		if v.State != driver.VMRunning {
			continue // its exit is recorded, and stays
		}
		if v.spec.Firmware {
			h.endFirmwareBootLocked(uid)
		} else {
			h.writeLog(uid, console.ExitRecord(console.ExitStatus{Code: 143}, time.Now()))
		}
		h.endExecsLocked(uid)
		if v.spec.Restart == driver.RestartNever {
			v.State, v.ExitCode = driver.VMExited, 253
		} else {
			delete(h.vms, uid)
		}
	}
	signal(h.vmChanges)
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
	delete(h.baseUsed, digest)
}

// Leave puts objects on the host that no machine accounts for, as a crash
// between steps leaves them.
func (h *Host) Leave(machineUID, networkUID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bases["sha256:orphan"] = h.baseSize
	h.baseUsed["sha256:orphan"] = time.Now()
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
	return len(h.stopCalls[machineUID])
}

// HypervisorStopTimes returns when the machine's VM was asked to stop,
// failed calls included.
func (h *Host) HypervisorStopTimes(machineUID string) []time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.stopCalls[machineUID])
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
	Bases   []string
	Roots   map[string]driver.Disk
	Volumes map[string]driver.Volume
	// ShareVolumes are FileShares' volumes, by share uid.
	ShareVolumes map[string]driver.ShareVolume
	ImageBases   []string
	// Captures are the snapshots taken for CustomImages, by image uid.
	Captures   []string
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
	return len(s.Bases) == 0 && len(s.Roots) == 0 && len(s.Volumes) == 0 && len(s.ShareVolumes) == 0 && len(s.ImageBases) == 0 && len(s.Captures) == 0 &&
		len(s.Networks) == 0 && len(s.Interfaces) == 0 &&
		len(s.VMs) == 0 && len(s.Logs) == 0 && len(s.Listening) == 0
}

// EmptyButImages reports whether the host holds nothing but the container
// images' bases it keeps for their next use.
func (s Snapshot) EmptyButImages() bool {
	s.Bases = nil
	return s.Empty()
}

// Snapshot returns a copy of what the host holds.
func (h *Host) Snapshot() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := Snapshot{
		Bases: slices.Sorted(maps.Keys(h.bases)), Roots: maps.Clone(h.roots), Volumes: maps.Clone(h.volumes), ShareVolumes: maps.Clone(h.shareVols),
		ImageBases: slices.Sorted(maps.Keys(h.vmBases)), Captures: slices.Sorted(maps.Keys(h.captures)), Networks: maps.Clone(h.networks),
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
	_, ok := h.pulled[digest]
	return ok
}

// CacheUsedAt returns when the image cache last used digest; zero for one
// it does not hold.
func (h *Host) CacheUsedAt(digest string) time.Time {
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

func (d images) Resolve(ctx context.Context, ref string, creds driver.Credentials) (string, error) {
	if err := d.h.authorize(ctx, ref, creds); err != nil {
		return "", err
	}
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	d.h.resolved[ref]++
	if err := d.h.pullErr[ref]; err != nil {
		return "", err
	}
	if _, digest, pinned := strings.Cut(ref, "@"); pinned {
		repo := repositoryOf(ref)
		if d.h.holds[repo][digest] || d.h.claims[repo][digest] {
			return digest, nil
		}
		return "", fmt.Errorf("%s: %w", ref, driver.ErrImageNotFound)
	}
	digest, ok := d.h.tags[ref]
	if !ok {
		return "", fmt.Errorf("%s: %w", ref, driver.ErrImageNotFound)
	}
	return digest, nil
}

// byDigest looks an image up in the host's cache, whatever creds give, if
// it was fetched from ref's repository; or else pulls it in full as that
// repository gives ref@digest, with creds, which a repository that only
// claims the digest cannot.
func (d images) byDigest(ctx context.Context, ref, digest string, creds driver.Credentials) (driver.ImageConfig, error) {
	repo, err := driver.Repository(ref)
	if err != nil {
		return driver.ImageConfig{}, err
	}
	d.h.mu.Lock()
	cached := d.h.sources[digest][repo]
	d.h.mu.Unlock()
	if !cached {
		if err := d.h.authorize(ctx, ref, creds); err != nil {
			return driver.ImageConfig{}, err
		}
	}
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	config, known := d.h.configs[digest]
	if cached = d.h.sources[digest][repo]; !cached {
		if err := d.h.pullErr[ref]; err != nil {
			return driver.ImageConfig{}, err
		}
		if !d.h.holds[repo][digest] {
			return driver.ImageConfig{}, fmt.Errorf("%s@%s: the registry serves none of its blobs: %w", ref, digest, driver.ErrImageNotFound)
		}
		if _, in := d.h.pulled[digest]; known && !in {
			if free := d.h.imageFreeLocked(); config.Size > free {
				return driver.ImageConfig{}, &driver.NoSpaceError{What: "the blobs of " + ref + "@" + digest, Room: driver.ImageRoom, Need: config.Size, Free: free}
			}
		}
	}
	if !known {
		return driver.ImageConfig{}, fmt.Errorf("%s@%s: %w", ref, digest, driver.ErrImageNotFound)
	}
	if !cached {
		d.h.pulls[digest]++
	}
	d.h.pulled[digest] = time.Now()
	add(d.h.sources, digest, repo)
	return config, nil
}

// imageFreeLocked is what is free in the image room. Callers hold h.mu.
func (h *Host) imageFreeLocked() int64 { return h.cacheSize - h.cachedLocked() - h.imageTaken }

// cachedLocked is what the image cache holds. Callers hold h.mu.
func (h *Host) cachedLocked() int64 {
	var n int64
	for digest := range h.pulled {
		n += h.configs[digest].Size
	}
	return n
}

// blob names the one blob the fake keeps of a digest.
func blob(digest string) string { return "blob-of-" + digest }

func (d images) Cached(context.Context) (driver.ImageCache, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if err := d.h.listErr[ListImageCache]; err != nil {
		return driver.ImageCache{}, err
	}
	out := driver.ImageCache{Total: d.h.cacheSize, Free: d.h.imageFreeLocked() - d.h.freeing}
	for _, digest := range slices.Sorted(maps.Keys(d.h.pulled)) {
		out.Images = append(out.Images, driver.CachedImage{
			Digest: digest, UsedAt: d.h.pulled[digest], Blobs: map[string]int64{blob(digest): d.h.configs[digest].Size},
		})
	}
	return out, nil
}

func (d images) Config(ctx context.Context, ref, digest string, creds driver.Credentials) (driver.ImageConfig, error) {
	return d.byDigest(ctx, ref, digest, creds)
}

func (d images) Forget(_ context.Context, digest string) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	delete(d.h.pulled, digest)
	delete(d.h.sources, digest)
	return nil
}

func (d images) Unpack(ctx context.Context, ref, digest, dir string, creds driver.Credentials) error {
	if _, err := d.byDigest(ctx, ref, digest, creds); err != nil {
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
	d.h.mu.Lock()
	size := d.h.configs[digest].Size
	unpacked, ok := d.h.unpackedSize[digest]
	if !ok {
		unpacked = size
	}
	free := d.h.imageFreeLocked()
	d.h.mu.Unlock()
	if unpacked > free {
		return &os.PathError{Op: "write", Path: filepath.Join(dir, "image"), Err: syscall.ENOSPC}
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
	full := d.h.full
	d.h.freeing = 0
	storageFree, imageFree := d.h.storageSize-d.h.usedLocked(), d.h.imageFreeLocked()
	d.h.mu.Unlock()
	switch {
	case full:
		return driver.ErrNoSpace
	case size > imageFree:
		return &driver.NoSpaceError{What: "unpacking " + digest, Room: driver.ImageRoom, Need: size, Free: imageFree}
	case size > storageFree:
		return &driver.NoSpaceError{What: "the base of " + digest, Room: driver.StorageRoom, Need: size, Free: storageFree}
	}
	dir, err := os.MkdirTemp("", "fake-base")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := fill(dir); err != nil {
		if driver.OutOfRoom(err) {
			d.h.mu.Lock()
			if d.h.deferFrees {
				d.h.freeing = imageFree
			}
			d.h.mu.Unlock()
			return driver.UnpackShortfall("unpacking "+digest, size, imageFree, err)
		}
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "image")); err != nil {
		return fmt.Errorf("the base for %s was left empty", digest)
	}
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if free := d.h.storageSize - d.h.usedLocked(); d.h.baseSize > free {
		return &driver.NoSpaceError{What: "the base of " + digest, Room: driver.StorageRoom, Need: d.h.baseSize, Free: free}
	}
	d.h.bases[digest] = d.h.baseSize
	d.h.baseUsed[digest] = time.Now()
	d.h.unpacked[digest]++
	return nil
}

func (d storage) Bases(context.Context) ([]driver.Base, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if err := d.h.listErr[ListBases]; err != nil {
		return nil, err
	}
	var out []driver.Base
	for _, digest := range slices.Sorted(maps.Keys(d.h.bases)) {
		out = append(out, driver.Base{Digest: digest, Size: d.h.bases[digest], UsedAt: d.h.baseUsed[digest]})
	}
	return out, nil
}

func (d storage) DeleteBase(_ context.Context, digest string) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.baseDelErr != nil {
		return d.h.baseDelErr
	}
	for _, r := range d.h.roots {
		if r.Digest == digest {
			return fmt.Errorf("base %s has root disks cloned from it", digest)
		}
	}
	delete(d.h.bases, digest)
	delete(d.h.baseUsed, digest)
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
	d.h.baseUsed[digest] = time.Now()
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
	if r, ok := d.h.roots[machineUID]; ok {
		if _, ok := d.h.bases[r.Digest]; ok {
			d.h.baseUsed[r.Digest] = time.Now()
		}
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
	full := d.h.full || size > d.h.storageSize-d.h.usedLocked()
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
	delete(d.h.imageUsed, digest)
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
	for image, c := range d.h.captures {
		if c.diskUID == diskUID {
			return fmt.Errorf("the volume of disk %s has a snapshot for custom image %s", diskUID, image)
		}
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
		return driver.Capacity{Total: d.h.storageSize}, nil
	}
	return driver.Capacity{Total: d.h.storageSize, Free: d.h.storageSize - d.h.usedLocked()}, nil
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
	if i.IPv6.IsValid() && !d.h.ipv6Prefix.Contains(i.IPv6) {
		return driver.Interface{}, fmt.Errorf("machine %s's %s: %s is not of the host's IPv6 prefix %s", i.MachineUID, i.Name, i.IPv6, d.h.ipv6Prefix)
	}
	if i.IPv6.IsValid() {
		for uid, ifs := range d.h.interfaces {
			for name, o := range ifs {
				if o.IPv6 == i.IPv6 && (uid != i.MachineUID || name != i.Name) {
					d.h.ipv6Refused++
					return driver.Interface{}, fmt.Errorf("machine %s's %s: %s is machine %s's %s's: %w",
						i.MachineUID, i.Name, i.IPv6, uid, name, driver.ErrIPv6InUse)
				}
			}
		}
	}
	for uid, ifs := range d.h.interfaces {
		for name, o := range ifs {
			if o.NetworkUID == i.NetworkUID && strings.EqualFold(o.MAC, i.MAC) && (uid != i.MachineUID || name != i.Name) {
				return driver.Interface{}, fmt.Errorf("machine %s's %s: %s is machine %s's %s's: %w",
					i.MachineUID, i.Name, i.MAC, uid, name, driver.ErrMACInUse)
			}
		}
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
	switch srv := spec.FileShareServer; {
	case srv != nil:
		if spec.Firmware || spec.Root != (driver.Disk{}) || len(spec.Disks) > 0 || len(spec.Mounts) > 0 || len(spec.Run.Command) > 0 {
			return fmt.Errorf("machine %s, a file share's server, has a root disk, volumes, mounts or a process", spec.MachineUID)
		}
		if have, ok := d.h.shareVols[srv.Volume.ShareUID]; !ok || have != srv.Volume {
			return fmt.Errorf("machine %s has no share volume %s as specified", spec.MachineUID, srv.Volume.Path)
		}
	case spec.Firmware && (spec.Root != driver.Disk{} || len(spec.Disks) == 0):
		return fmt.Errorf("machine %s boots firmware from a volume, not root disk %+v", spec.MachineUID, spec.Root)
	case !spec.Firmware:
		if r, ok := d.h.roots[spec.MachineUID]; !ok || r != spec.Root {
			return fmt.Errorf("machine %s has no root disk %s", spec.MachineUID, spec.Root.Path)
		}
	}
	if spec.SystemSerialNumber != "" && !spec.Firmware {
		return fmt.Errorf("machine %s is given a system serial number, and boots no firmware", spec.MachineUID)
	}
	if err := computeapi.CheckSystemSerialNumber(spec.SystemSerialNumber); err != nil {
		return fmt.Errorf("machine %s: system serial number: %w", spec.MachineUID, err)
	}
	if spec.TPM && !spec.Firmware {
		return fmt.Errorf("machine %s is given a TPM, and boots no firmware", spec.MachineUID)
	}
	for _, v := range spec.Disks {
		if v.Serial != "" && !spec.Firmware {
			return fmt.Errorf("machine %s: only a firmware VM's volumes have serials", spec.MachineUID)
		}
		if err := computeapi.CheckDiskSerial(v.Serial); err != nil {
			return fmt.Errorf("machine %s: volume %s's serial: %w", spec.MachineUID, v.Path, err)
		}
		bare := v
		bare.Serial = ""
		if have, ok := d.h.volumes[v.DiskUID]; !ok || have != bare {
			return fmt.Errorf("machine %s has no volume %s as specified", spec.MachineUID, v.Path)
		}
	}
	for _, m := range spec.Mounts {
		if spec.Firmware || (!m.Share.IsValid() && (m.Disk < 0 || m.Disk >= len(spec.Disks))) || m.Target == "" {
			return fmt.Errorf("machine %s: mount %+v is of no volume of its, or has no target", spec.MachineUID, m)
		}
	}
	for _, i := range spec.Interfaces {
		if have, ok := d.h.interfaces[spec.MachineUID][i.Name]; !ok || have != i {
			return fmt.Errorf("machine %s has no interface %s as specified", spec.MachineUID, i.Name)
		}
		if i.FileShareServer != (spec.FileShareServer != nil) {
			return fmt.Errorf("machine %s's interface %s is a file share server's only if the machine is", spec.MachineUID, i.Name)
		}
	}
	boots := 1
	if v, ok := d.h.vms[spec.MachineUID]; ok {
		boots = v.boots + 1
	}
	d.h.vms[spec.MachineUID] = &vm{
		VM:    driver.VM{MachineUID: spec.MachineUID, SpecHash: hash, State: driver.VMRunning, Started: time.Now(), Run: spec.Run, Interfaces: spec.Interfaces},
		spec:  spec,
		boots: boots,
	}
	d.h.boot(spec.MachineUID)
	d.h.bootGuest(d.h.vms[spec.MachineUID])
	return nil
}

// bootGuest has a guest, booting, hold what its boot disk gives it, written
// when the VM started: as hangar-init does, whatever it was told in an
// earlier boot. A container's is its public IPv6, a share's server's its
// export.
func (h *Host) bootGuest(v *vm) {
	switch {
	case v.spec.FileShareServer != nil:
		h.exports[v.MachineUID] = cloneExport(v.spec.FileShareServer.Export)
	case v.spec.Firmware:
		h.bootConsoleLocked(v.MachineUID)
	default:
		h.guestIPv6[v.MachineUID] = driver.GuestIPv6Of(v.spec.Interfaces)
	}
}

// Stop asks a running VM's guest to stop its process first, as the real
// driver does, unless the guest does not answer, and takes as long as
// DelayStop says.
func (d hypervisor) Stop(ctx context.Context, machineUID string) error {
	d.h.mu.Lock()
	d.h.stopCalls[machineUID] = append(d.h.stopCalls[machineUID], time.Now())
	if err := d.h.removalErr; err != nil {
		d.h.mu.Unlock()
		return err
	}
	v, ok := d.h.vms[machineUID]
	running := ok && v.State == driver.VMRunning
	// A firmware VM has no guest agent to ask: its power button is pressed.
	if running && !v.spec.Firmware && !d.h.hung[machineUID] && !d.h.silent[machineUID] {
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
	if v, ok := d.h.vms[machineUID]; ok && v.State == driver.VMRunning && v.spec.Firmware {
		d.h.endFirmwareBootLocked(machineUID)
	}
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

func (d guest) SetIPv6(_ context.Context, machineUID string, ifaces []driver.GuestIPv6) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if v, ok := d.h.vms[machineUID]; !ok || v.State != driver.VMRunning {
		return errUnknownVM
	}
	if d.h.silent[machineUID] {
		return fmt.Errorf("the guest agent of machine %s does not answer", machineUID)
	}
	if d.h.oldGuests[machineUID] {
		return fmt.Errorf("%w: setting IPv6", driver.ErrGuestCannot)
	}
	v := d.h.vms[machineUID]
	gateways := 0
	for _, i := range ifaces {
		if !slices.ContainsFunc(v.spec.Interfaces, func(n driver.Interface) bool { return n.MAC == i.MAC }) {
			return fmt.Errorf("machine %s's guest has no interface %s", machineUID, i.MAC)
		}
		if (i.Address.IsValid() && (!i.Address.Is6() || i.Address.Is4In6())) || (i.Gateway.IsValid() && !i.Gateway.Is6()) {
			return fmt.Errorf("machine %s's guest's interface %s: %s via %s is no IPv6", machineUID, i.MAC, i.Address, i.Gateway)
		}
		if i.Gateway.IsValid() {
			gateways++
		}
	}
	if gateways > 1 {
		return fmt.Errorf("machine %s's guest: %d IPv6 default routes", machineUID, gateways)
	}
	held := driver.GuestIPv6Of(v.spec.Interfaces)
	for n := range held {
		if i := slices.IndexFunc(ifaces, func(g driver.GuestIPv6) bool { return g.MAC == held[n].MAC }); i >= 0 {
			held[n] = ifaces[i]
		} else if was := d.h.guestIPv6[machineUID]; n < len(was) {
			held[n] = was[n]
		}
	}
	d.h.guestIPv6[machineUID] = held
	return nil
}

// OldGuest makes the machine's guest one that cannot be told its IPv6, as
// one started before it could; false makes it able again.
func (h *Host) OldGuest(machineUID string, old bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.oldGuests[machineUID] = old
}

// GuestIPv6 is what the machine's guest holds of its public IPv6: what its
// boot disk gave it at its last boot, or what it was told since; nil for a
// guest that never booted.
func (h *Host) GuestIPv6(machineUID string) []driver.GuestIPv6 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.guestIPv6[machineUID])
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

// StartedByOldNode makes the machine's VM one an older node started, whose
// hash counted its interfaces' IPv6 addresses (driver.VMSpec.LegacyHash).
func (h *Host) StartedByOldNode(machineUID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v, ok := h.vms[machineUID]; ok {
		v.SpecHash = v.spec.LegacyHash(v.Interfaces)
	}
}

// StartedWithoutDiskSerials makes the machine's VM one a node started
// before volumes had serials: its volumes have none, and its hash counts
// none (driver.VMSpec.Runs).
func (h *Host) StartedWithoutDiskSerials(machineUID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v, ok := h.vms[machineUID]; ok {
		v.spec.Disks = slices.Clone(v.spec.Disks)
		for i := range v.spec.Disks {
			v.spec.Disks[i].Serial = ""
		}
		v.SpecHash = v.spec.Hash()
	}
}

// IPv6Refusals counts the interfaces refused for an IPv6 address another
// interface held (driver.ErrIPv6InUse): the agent waits for one to let go,
// and never asks.
func (h *Host) IPv6Refusals() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ipv6Refused
}
