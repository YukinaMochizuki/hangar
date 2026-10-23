// Package node is the node agent, hangar-node: it reports its host to the
// apiserver and turns the machines placed on it into running microVMs. It
// keeps no state of its own; what the host runs is read back from the host.
// See docs/architecture.md.
package node

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1/networkv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1/nodev1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1/storagev1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/buildinfo"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/metadata"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/tunnel"
)

// Config configures an Agent. Zero values take defaults.
type Config struct {
	// Name is the Node the agent reports as; the apiserver must accept it as
	// that node.
	Name string
	// HTTP and BaseURL reach the apiserver's node socket.
	HTTP    connect.HTTPClient
	BaseURL string
	// Tunnel, if set, connects to the apiserver's node socket for the
	// tunnel the agent keeps open, over which the apiserver calls it
	// (internal/tunnel); without it the apiserver cannot reach the node.
	Tunnel func(ctx context.Context) (net.Conn, error)
	// TunnelLiveness is the tunnel's; zero means tunnel.DefaultLiveness.
	TunnelLiveness tunnel.Liveness
	// LogPoll is how often a followed log's VM is checked to have ended.
	// Zero means a second.
	LogPoll time.Duration
	// Host reports what the host has. Defaults to reading this host.
	Host func() (*computev1alpha1.NodeResources, error)
	// Reserved is kept back from machines for the host itself, in
	// quantities.
	Reserved *computev1alpha1.NodeResources
	// Placement describes the host: the only source of its region, site
	// and chassis, and what it sets wins over what is read of the host.
	Placement *computev1alpha1.Placement
	// CPU reads the host's CPU model and generation, and Disks, Memory and
	// Network describe its disks, memory and ports, read once as Run
	// starts. They default to reading this host.
	CPU                    func() (*computev1alpha1.Placement, error)
	Disks, Memory, Network func() (string, error)
	// HeartbeatInterval is how often the agent reports. The default of ten
	// seconds fits the apiserver's NodeGracePeriod with three reports to
	// spare.
	HeartbeatInterval time.Duration
	// Drivers are the host. Without them the agent only reports the node.
	Drivers *Drivers
	// SweepInterval is how often the host is checked for what no machine
	// accounts for. Zero means a minute.
	SweepInterval time.Duration
	// PullTimeout bounds preparing one image, so a stalled pull fails and
	// is tried again. Zero means half an hour.
	PullTimeout time.Duration
	// MaxPulls bounds the images unpacked at once. Zero means
	// DefaultMaxPulls.
	MaxPulls int
	// CaptureTimeout bounds copying one CustomImage's snapshot into its
	// base, so a copy that stalls fails, for good, rather than leaving the
	// image capturing. Zero means DefaultCaptureTimeout.
	CaptureTimeout time.Duration
	// UploadDir is where files uploaded for CustomImages are staged until
	// converted, root's alone: a directory on the images dataset, whose
	// quota bounds them. Empty, the node takes no uploads.
	UploadDir string
	// UploadExpiry is how long after its image is made an upload may take
	// to be whole; zero means DefaultUploadExpiry.
	UploadExpiry time.Duration
	// MaxImageDiskSize bounds the disk an uploaded file holds; zero means
	// DefaultMaxImageDiskSize.
	MaxImageDiskSize int64
	// InspectTimeout bounds reading what one uploaded file is, and
	// ConvertTimeout converting it once read, so a converter that stalls
	// fails the image, for good; zero means DefaultInspectTimeout and
	// DefaultConvertTimeout.
	InspectTimeout time.Duration
	ConvertTimeout time.Duration
	// DNSUpstreams are the resolvers each network's resolver forwards names
	// outside its own zone to.
	DNSUpstreams []netip.AddrPort
	// Metadata is the metadata service the networks are served; nil makes
	// one. A test given one asks it as a guest would, from any address.
	Metadata *metadata.Server
	// IPv6Prefix is the host's public IPv6 /64, which the agent reports and
	// its machines' public addresses come from; invalid for none, and the
	// node's machines then have no IPv6.
	IPv6Prefix netip.Prefix
	// SharedIPv4 is the host's shared IPv4 address, which the agent reports
	// and its PortForwards open ports of; invalid for none, and the node
	// then forwards no port (docs/decisions/networking.md, "Port
	// forwards").
	SharedIPv4 netip.Addr
	// EdgeDial, if set, is the listener hangar-edge asks for connections to
	// public endpoints' ports on (internal/edgedial), from the peer uids
	// EdgeUIDs alone; without it no endpoint is reached on this node.
	EdgeDial net.Listener
	EdgeUIDs []uint32
	// MaxUnpackWork bounds the work unpacking one image takes, in
	// ocilayer's steps: the Images driver is built with it as
	// registry.Config.Limits.MaxWork. Zero means ocilayer.DefaultMaxWork.
	MaxUnpackWork int64
	// NATLog is the directory of the log of IPv4 connections out of the
	// host's networks (internal/node/natlog), if the network driver can
	// tell them; empty logs none.
	NATLog string
	// ImageCacheHigh and ImageCacheLow bound the images no machine uses
	// (docs/decisions/node-agent.md). Zero means DefaultImageCacheHigh,
	// and for ImageCacheLow four fifths of ImageCacheHigh.
	ImageCacheHigh, ImageCacheLow int64

	Clock clockwork.Clock
	Log   *slog.Logger
}

// DefaultMaxPulls is how many images a node prepares at once by default:
// pulls and unpacks share the host's disks and CPUs with running machines.
const DefaultMaxPulls = 2

// DefaultCaptureTimeout bounds copying a CustomImage's snapshot into its
// base: room for a full 2 TiB disk copied at 100 MB/s, as a pool of
// spinning disks does.
const DefaultCaptureTimeout = 6 * time.Hour

// DefaultImageCacheHigh is how much the images no machine uses may take by
// default (docs/decisions/node-agent.md).
const DefaultImageCacheHigh = 20 << 30

// Agent is a running node agent.
type Agent struct {
	cfg      Config
	api      *client.Client
	nodes    computev1alpha1connect.NodeServiceClient
	machines computev1alpha1connect.MachineServiceClient
	networks networkv1alpha1connect.NetworkServiceClient
	// portForwards reports on PortForwards.
	portForwards networkv1alpha1connect.PortForwardServiceClient
	disks        storagev1alpha1connect.DiskServiceClient
	shares       storagev1alpha1connect.FileShareServiceClient
	// imagesAPI reaches Images; images makes disk images' bases.
	imagesAPI storagev1alpha1connect.ImageServiceClient
	// customImages reaches CustomImages; captures makes their bases, and
	// has their controller look again through captureTrigger.
	customImages   storagev1alpha1connect.CustomImageServiceClient
	captures       *baseMaker
	captureTrigger chan struct{}
	// stager stages files uploaded for CustomImages, inspects reads what
	// they are, converts converts them, and reported has their controller
	// look again, at most every reportEvery, as chunks are counted.
	stager   *stager
	inspects *jobs[driver.ImageInfo]
	converts *baseMaker
	reported *throttle
	// wakes are when each upload expires, by image uid, which a timer
	// has the controller look at.
	wakeMu sync.Mutex
	wakes  map[string]time.Time
	// pullCreds gives the logins machines' images are pulled with.
	pullCreds nodev1connect.ImagePullCredentialsServiceClient
	images    *imageMaker
	// host serializes everything that changes the host: each reconcile, and
	// the sweep, which must not judge what a reconcile is building.
	host sync.Mutex
	// puller prepares images off the host lock, and stops stops VMs off it.
	puller *puller
	stops  *stops
	// guests asks off the host lock the guest agents that did not answer
	// in time.
	guests *guests
	// trigger has every machine checked again, and diskTrigger every disk.
	trigger, diskTrigger chan struct{}
	// netEpoch counts the network driver's change signals, and netChecked
	// is the epoch each machine's interfaces were last found whole in.
	// Guarded by host, but netEpoch, which the signals bump.
	netEpoch   atomic.Int64
	netChecked map[string]int64
	// told is, by machine uid, what each container's guest holds of its
	// public IPv6 in its current boot, and exported what each file share's
	// server holds of its export, and bootExported what its VM's boot disk
	// does, as far as the agent wrote it. Guarded by host.
	told         map[string]toldIPv6
	exported     map[string]toldExport
	bootExported map[string]driver.ShareExport
	// shareWaiters are the machines waiting for their shares to serve them
	// (sharesServe), which a server taking an export, or a network's
	// firewall written, queues again; requeue queues them, the machine
	// controller's Enqueue unless a test says otherwise.
	waitMu       sync.Mutex
	shareWaiters map[controller.Key]bool
	requeue      func(...controller.Key)
	// revoking holds a removed client's firewall cut until its server has
	// dropped it; requeueNetworks queues the networks to write again, the
	// network services controller's Enqueue unless a test says otherwise.
	revoking        *revocations
	requeueNetworks func(...controller.Key)
	// netsvc serves the networks on the host their services, resolver
	// among them, which netTrigger has checked again.
	netsvc     *netServices
	netTrigger chan struct{}
	// forwardTrigger has the host's port forwards ensured again.
	forwardTrigger chan struct{}
	// machinesCtl, networksCtl, disksCtl, sharesCtl, forwardsCtl and
	// customImagesCtl are the controllers Run runs with drivers.
	machinesCtl, networksCtl, disksCtl, sharesCtl, forwardsCtl, customImagesCtl *controller.Controller
	// sweeps counts the sweeps sweepLoop has finished.
	sweeps atomic.Int64
	// cpuRead is how the last read of the host's CPU went; only report
	// uses it.
	cpuRead readState
	// storage, memory and network are what Run read of the host as it
	// started.
	storage, memory, network string
	// metrics are what the agent counts for its metrics listener.
	metrics *agentMetrics
}

// New returns an agent for cfg.
func New(cfg Config) (*Agent, error) {
	if cfg.Name == "" {
		return nil, errors.New("the node agent needs the name of its Node")
	}
	if cfg.IPv6Prefix.IsValid() {
		if _, err := computeapi.ParseIPv6Prefix(cfg.IPv6Prefix.String()); err != nil {
			return nil, fmt.Errorf("the host's IPv6 prefix: %w", err)
		}
	}
	if cfg.SharedIPv4.IsValid() && !cfg.SharedIPv4.Is4() {
		return nil, fmt.Errorf("the host's shared address %s is not IPv4", cfg.SharedIPv4)
	}
	if cfg.Host == nil {
		cfg.Host = hostResources
	}
	if cfg.CPU == nil {
		cfg.CPU = hostCPU
	}
	if cfg.Disks == nil {
		cfg.Disks = hostDisks
	}
	if cfg.Memory == nil {
		cfg.Memory = hostMemory
	}
	if cfg.Network == nil {
		cfg.Network = hostNetwork
	}
	if cfg.Reserved == nil {
		cfg.Reserved = &computev1alpha1.NodeResources{}
	}
	if _, err := readAmounts(cfg.Reserved); err != nil {
		return nil, fmt.Errorf("reserved %w", err)
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 10 * time.Second
	}
	if cfg.Clock == nil {
		cfg.Clock = clockwork.NewRealClock()
	}
	if cfg.SweepInterval == 0 {
		cfg.SweepInterval = time.Minute
	}
	if cfg.PullTimeout == 0 {
		cfg.PullTimeout = 30 * time.Minute
	}
	if cfg.CaptureTimeout == 0 {
		cfg.CaptureTimeout = DefaultCaptureTimeout
	}
	if cfg.UploadExpiry == 0 {
		cfg.UploadExpiry = DefaultUploadExpiry
	}
	if cfg.MaxImageDiskSize == 0 {
		cfg.MaxImageDiskSize = DefaultMaxImageDiskSize
	}
	if cfg.InspectTimeout == 0 {
		cfg.InspectTimeout = DefaultInspectTimeout
	}
	if cfg.ConvertTimeout == 0 {
		cfg.ConvertTimeout = DefaultConvertTimeout
	}
	if cfg.MaxPulls == 0 {
		cfg.MaxPulls = DefaultMaxPulls
	}
	if cfg.MaxPulls < 0 || cfg.MaxUnpackWork < 0 {
		return nil, fmt.Errorf("max pulls %d and max unpack work %d cannot be negative", cfg.MaxPulls, cfg.MaxUnpackWork)
	}
	if cfg.ImageCacheHigh == 0 {
		cfg.ImageCacheHigh = DefaultImageCacheHigh
	}
	if cfg.ImageCacheLow == 0 {
		cfg.ImageCacheLow = cfg.ImageCacheHigh / 5 * 4
	}
	if cfg.ImageCacheLow < 0 || cfg.ImageCacheLow > cfg.ImageCacheHigh {
		return nil, fmt.Errorf("the image cache's low mark %d must be from 0 to its high mark %d", cfg.ImageCacheLow, cfg.ImageCacheHigh)
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	cfg.Log = cfg.Log.With("node", cfg.Name)
	cfg.Placement = configuredPlacement(cfg.Placement, cfg.Log)
	var puller *puller
	var netsvc *netServices
	var images *imageMaker
	var captures *baseMaker
	if cfg.Drivers != nil {
		puller = newPuller(cfg.Drivers.Images, cfg.Drivers.Storage, cfg.Clock, cfg.PullTimeout, cfg.MaxPulls)
		netsvc = newNetServices(cfg.Drivers.Network, cfg.DNSUpstreams, cfg.Metadata, cfg.Log)
		images = newImageMaker(cfg.Drivers.DiskImages, cfg.Drivers.Storage, cfg.Clock, cfg.PullTimeout, cfg.MaxPulls)
		captures = newCaptureMaker(cfg.Drivers.Storage, cfg.CaptureTimeout, cfg.MaxPulls)
	}
	a := &Agent{
		cfg:          cfg,
		api:          client.New(cfg.HTTP, cfg.BaseURL),
		nodes:        computev1alpha1connect.NewNodeServiceClient(cfg.HTTP, cfg.BaseURL),
		machines:     computev1alpha1connect.NewMachineServiceClient(cfg.HTTP, cfg.BaseURL),
		networks:     networkv1alpha1connect.NewNetworkServiceClient(cfg.HTTP, cfg.BaseURL),
		portForwards: networkv1alpha1connect.NewPortForwardServiceClient(cfg.HTTP, cfg.BaseURL),
		disks:        storagev1alpha1connect.NewDiskServiceClient(cfg.HTTP, cfg.BaseURL),
		shares:       storagev1alpha1connect.NewFileShareServiceClient(cfg.HTTP, cfg.BaseURL),
		imagesAPI:    storagev1alpha1connect.NewImageServiceClient(cfg.HTTP, cfg.BaseURL),
		customImages: storagev1alpha1connect.NewCustomImageServiceClient(cfg.HTTP, cfg.BaseURL),
		captures:     captures,
		pullCreds:    nodev1connect.NewImagePullCredentialsServiceClient(cfg.HTTP, cfg.BaseURL),
		images:       images,
		netChecked:   map[string]int64{},
		told:         map[string]toldIPv6{},
		exported:     map[string]toldExport{},
		bootExported: map[string]driver.ShareExport{},
		shareWaiters: map[controller.Key]bool{},
		revoking:     newRevocations(),
		puller:       puller,
		metrics:      newAgentMetrics(),
		stops:        newStops(),
		guests:       newGuests(),
		trigger:      make(chan struct{}, 1),
		netsvc:       netsvc,
		netTrigger:   make(chan struct{}, 1),
	}
	a.forwardTrigger = make(chan struct{}, 1)
	a.diskTrigger = make(chan struct{}, 1)
	a.captureTrigger = make(chan struct{}, 1)
	a.wakes = map[string]time.Time{}
	if cfg.Drivers != nil && cfg.UploadDir != "" && cfg.Drivers.Converter != nil {
		a.reported = newThrottle(cfg.Clock, reportEvery, func() { notify(a.captureTrigger) })
		st, err := newStager(cfg.UploadDir, func(_ string, whole bool) {
			if whole {
				notify(a.captureTrigger)
				return
			}
			a.reported.poke()
		})
		if err != nil {
			return nil, fmt.Errorf("the upload directory: %w", err)
		}
		a.stager = st
		// Inspections and conversions share their slots: each runs a
		// converter VM.
		slots := newFIFO(cfg.MaxPulls)
		a.inspects = newInspector(cfg.Drivers.Converter, st.whole, cfg.MaxImageDiskSize, cfg.InspectTimeout, slots)
		a.converts = newConvertMaker(cfg.Drivers.Converter, cfg.Drivers.Storage, st.whole, a.inspects.done, cfg.ConvertTimeout, slots)
	}
	if cfg.Drivers != nil {
		a.machinesCtl, a.networksCtl, a.disksCtl = a.machineController(), a.networkController(), a.diskController()
		a.sharesCtl, a.forwardsCtl = a.shareController(), a.forwardController()
		a.customImagesCtl = a.customImageController()
		a.requeue = a.machinesCtl.Enqueue
		a.requeueNetworks = a.networksCtl.Enqueue
		puller.makeRoom = a.makeRoom
		puller.pulled = a.metrics.pulled
	}
	return a, nil
}

// Activity counts what the agent has done outside its controllers.
type Activity struct {
	// Sweeps counts the sweeps of the host its loop has finished.
	Sweeps int64
	// NetworkChanges counts the network driver's change signals it has
	// taken in, each before the machines are checked again for it.
	NetworkChanges int64
}

// Activity reports what the agent has done outside its controllers.
func (a *Agent) Activity() Activity {
	return Activity{Sweeps: a.sweeps.Load(), NetworkChanges: a.netEpoch.Load()}
}

// ControllerStatus reports what each of the agent's controllers is doing;
// without drivers it runs none.
func (a *Agent) ControllerStatus() []controller.Status {
	var st []controller.Status
	for _, c := range []*controller.Controller{a.machinesCtl, a.networksCtl, a.disksCtl, a.sharesCtl, a.forwardsCtl, a.customImagesCtl} {
		if c != nil {
			st = append(st, c.Status())
		}
	}
	return st
}

// Run reports the node and, with drivers, runs its machines, until ctx is
// done. Failures are logged and retried, never fatal: the apiserver judges
// a node by its reports.
func (a *Agent) Run(ctx context.Context) error {
	// Before anything reports the node.
	a.storage = a.readOnce("disks", "storage", a.cfg.Disks)
	a.memory = a.readOnce("memory", "memory", a.cfg.Memory)
	a.network = a.readOnce("network", "network", a.cfg.Network)
	// Stops and asks of guests end with ctx; the sweep and the reconciles
	// begin them.
	defer a.stops.wg.Wait()
	defer a.guests.wg.Wait()
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if a.cfg.Tunnel != nil {
		wg.Go(func() { a.serveTunnel(ctx) })
	}
	if a.cfg.Drivers != nil {
		wg.Add(4)
		go func() {
			defer wg.Done()
			a.followHost(ctx)
			_ = a.machinesCtl.Run(ctx)
			a.puller.wait() // its jobs end with ctx
		}()
		go func() {
			defer wg.Done()
			_ = a.networksCtl.Run(ctx)
			a.netsvc.services.CloseAll()
		}()
		go func() {
			defer wg.Done()
			a.images.start(ctx, func() {
				select {
				case a.diskTrigger <- struct{}{}:
				default:
				}
			})
			_ = a.disksCtl.Run(ctx)
			a.images.wait() // its jobs end with ctx
		}()
		go func() {
			defer wg.Done()
			a.sweepLoop(ctx)
		}()
		wg.Go(func() { _ = a.sharesCtl.Run(ctx) })
		wg.Go(func() { _ = a.forwardsCtl.Run(ctx) })
		wg.Go(func() {
			a.captures.start(ctx, func() { notify(a.captureTrigger) })
			if a.converts != nil {
				a.inspects.start(ctx, func() { notify(a.captureTrigger) })
				a.converts.start(ctx, func() { notify(a.captureTrigger) })
			}
			_ = a.customImagesCtl.Run(ctx)
			a.captures.wait() // its jobs end with ctx
			if a.converts != nil {
				a.inspects.wait()
				a.converts.wait()
			}
		})
		if a.cfg.EdgeDial != nil {
			wg.Go(func() { a.serveEdgeDial(ctx) })
		}
		wg.Go(func() { a.followNAT(ctx) })
	} else {
		a.cfg.Log.Warn("no host drivers: reporting the node, running no machines")
	}
	ticker := a.cfg.Clock.NewTicker(a.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		if err := a.report(ctx); err != nil && ctx.Err() == nil {
			a.metrics.heartbeatFailures.Inc()
			a.cfg.Log.Error("reporting the node failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.Chan():
		}
	}
}

func (a *Agent) sweepLoop(ctx context.Context) {
	ticker := a.cfg.Clock.NewTicker(a.cfg.SweepInterval)
	defer ticker.Stop()
	for {
		if err := a.sweep(ctx); err != nil && ctx.Err() == nil {
			a.cfg.Log.Error("sweeping the host failed", "error", err)
		}
		a.sweeps.Add(1)
		select {
		case <-ctx.Done():
			return
		case <-ticker.Chan():
		}
	}
}

// followHost has anything the drivers notice on the host check every
// machine again, and an image made ready its machines.
func (a *Agent) followHost(ctx context.Context) {
	forward := func(from <-chan struct{}, noted func()) {
		for {
			select {
			case <-ctx.Done():
				return
			case <-from:
			}
			noted()
			a.recheck()
		}
	}
	go forward(a.cfg.Drivers.Network.Changes(), func() {
		a.netEpoch.Add(1)
		notify(a.netTrigger)
		notify(a.forwardTrigger)
	})
	go forward(a.cfg.Drivers.Hypervisor.Changes(), func() {})
	// An image made ready is looked at as soon as it is.
	a.puller.start(ctx, a.recheck)
}

// machineController runs the machines placed on this node, on changes to
// machines and their networks, disks and file shares, and on what
// followHost notices.
func (a *Agent) machineController() *controller.Controller {
	return &controller.Controller{
		Name:       "machines",
		Source:     controller.APISource{Client: a.api},
		Reconciler: controller.ReconcilerFunc(a.reconcileMachine),
		Watches: []controller.Watch{
			{Kind: registry.Machine.Kind, Map: a.machineIfMine},
			// A network getting its transit link unblocks its machines. On a
			// resync every machine is queued already.
			{Kind: registry.Network.Kind, Map: a.machinesOnNetwork, SkipResync: true},
			// So does a disk getting bound here, and a disk growing changes the
			// VMs that have it.
			{Kind: registry.Disk.Kind, Map: a.machinesWithDisk, SkipResync: true},
			// A share's addresses, users and squash change its server and the
			// containers mounting it.
			{Kind: registry.FileShare.Kind, Map: a.machinesOfShare, SkipResync: true},
		},
		Trigger: a.trigger,
		Clock:   a.cfg.Clock,
		Log:     a.cfg.Log,
	}
}

// machineIfMine maps a machine to itself, if it is on this node. A machine
// coming to declare a file share, or ceasing to, reaches the share's server
// through the share's users, which the apiserver rewrites (machinesOfShare).
func (a *Agent) machineIfMine(_ context.Context, o store.Object) []controller.Key {
	var m computev1alpha1.Machine
	if proto.Unmarshal(o.Data, &m) != nil || m.GetSpec().GetNode() != a.cfg.Name {
		return nil
	}
	return []controller.Key{{Project: o.Project, Name: o.Name}}
}

func (a *Agent) machinesOnNetwork(ctx context.Context, o store.Object) []controller.Key {
	objs, _, err := a.api.ListAll(ctx, registry.Machine, client.ListOptions{Project: o.Project})
	if err != nil {
		a.cfg.Log.Error("listing machines failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, obj := range objs {
		m := obj.(*computev1alpha1.Machine)
		if m.GetSpec().GetNode() != a.cfg.Name {
			continue
		}
		if slices.ContainsFunc(m.GetSpec().GetInterfaces(), func(i *computev1alpha1.MachineInterface) bool { return i.GetNetwork() == o.Name }) {
			keys = append(keys, controller.Key{Project: o.Project, Name: m.GetMetadata().GetName()})
		}
	}
	return keys
}

// report writes the node's status: what the host has, what is left for
// machines, what it is, and that the agent is alive.
func (a *Agent) report(ctx context.Context) error {
	capacity, err := a.cfg.Host()
	if err != nil {
		return fmt.Errorf("reading the host's resources: %w", err)
	}
	res, err := a.nodes.GetNode(ctx, connect.NewRequest(&computev1alpha1.GetNodeRequest{Name: a.cfg.Name}))
	if err != nil {
		return err
	}
	n := res.Msg.GetNode()
	status := &computev1alpha1.NodeStatus{}
	if n.GetStatus() != nil {
		status = proto.Clone(n.GetStatus()).(*computev1alpha1.NodeStatus)
	}
	now := a.cfg.Clock.Now()
	status.ObservedGeneration = n.GetMetadata().GetGeneration()
	capacity = proto.Clone(capacity).(*computev1alpha1.NodeResources)
	if d := a.cfg.Drivers; d != nil {
		if c, err := d.Storage.Capacity(ctx); err == nil {
			capacity.Disk = diskCapacity(c)
		} else {
			// A failed read keeps what was last reported rather than fail
			// the heartbeat, or none before: no disk, not an unchecked one.
			capacity.Disk = cmp.Or(n.GetStatus().GetCapacity().GetDisk(), quantity.FormatDisk(0))
			a.cfg.Log.Warn("reading the storage's capacity", "error", err)
		}
	}
	status.Capacity = capacity
	if status.Allocatable, err = allocatable(capacity, a.cfg.Reserved); err != nil {
		return fmt.Errorf("reading the host's resources: %w", err)
	}
	status.LastHeartbeatTime = timestamppb.New(now)
	status.AgentVersion = Version()
	status.Ipv6Prefix = ""
	if a.cfg.IPv6Prefix.IsValid() {
		status.Ipv6Prefix = a.cfg.IPv6Prefix.String()
	}
	status.SharedIpv4 = ""
	if a.cfg.SharedIPv4.IsValid() {
		status.SharedIpv4 = a.cfg.SharedIPv4.String()
	}
	status.Placement = placement(a.hardware(n.GetStatus().GetPlacement()), a.cfg.Placement)
	status.Conditions = conditions.Set(status.GetConditions(), &metav1.Condition{
		Type: computeapi.NodeReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "AgentReporting",
		ObservedGeneration: n.GetMetadata().GetGeneration(),
	}, now)
	_, err = a.nodes.UpdateNodeStatus(ctx, connect.NewRequest(&computev1alpha1.UpdateNodeStatusRequest{
		Name: a.cfg.Name, Status: status,
	}))
	return err
}

// Version is the agent's build.
func Version() string { return buildinfo.Version() }
