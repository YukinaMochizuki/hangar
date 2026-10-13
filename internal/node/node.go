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
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/buildinfo"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
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
	// DNSUpstreams are the resolvers each network's resolver forwards names
	// outside its own zone to.
	DNSUpstreams []netip.AddrPort
	// MaxUnpackWork bounds the work unpacking one image takes, in
	// ocilayer's steps: the Images driver is built with it as
	// registry.Config.Limits.MaxWork. Zero means ocilayer.DefaultMaxWork.
	MaxUnpackWork int64
	Clock         clockwork.Clock
	Log           *slog.Logger
}

// DefaultMaxPulls is how many images a node prepares at once by default:
// pulls and unpacks share the host's disks and CPUs with running machines.
const DefaultMaxPulls = 2

// Agent is a running node agent.
type Agent struct {
	cfg      Config
	api      *client.Client
	nodes    computev1alpha1connect.NodeServiceClient
	machines computev1alpha1connect.MachineServiceClient
	networks networkv1alpha1connect.NetworkServiceClient
	// host serializes everything that changes the host: each reconcile, and
	// the sweep, which must not judge what a reconcile is building.
	host sync.Mutex
	// puller prepares images off the host lock, and stops stops VMs off it.
	puller *puller
	stops  *stops
	// trigger has every machine checked again.
	trigger chan struct{}
	// netEpoch counts the network driver's change signals, and netChecked
	// is the epoch each machine's interfaces were last found whole in.
	// Guarded by host, but netEpoch, which the signals bump.
	netEpoch   atomic.Int64
	netChecked map[string]int64
	// netsvc serves the networks on the host their services, resolver
	// among them, which netTrigger has checked again.
	netsvc     *netServices
	netTrigger chan struct{}
}

// New returns an agent for cfg.
func New(cfg Config) (*Agent, error) {
	if cfg.Name == "" {
		return nil, errors.New("the node agent needs the name of its Node")
	}
	if cfg.Host == nil {
		cfg.Host = hostResources
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
	if cfg.MaxPulls == 0 {
		cfg.MaxPulls = DefaultMaxPulls
	}
	if cfg.MaxPulls < 0 || cfg.MaxUnpackWork < 0 {
		return nil, fmt.Errorf("max pulls %d and max unpack work %d cannot be negative", cfg.MaxPulls, cfg.MaxUnpackWork)
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	cfg.Log = cfg.Log.With("node", cfg.Name)
	var puller *puller
	var netsvc *netServices
	if cfg.Drivers != nil {
		puller = newPuller(cfg.Drivers.Images, cfg.Drivers.Storage, cfg.Clock, cfg.PullTimeout, cfg.MaxPulls)
		netsvc = newNetServices(cfg.Drivers.Network, cfg.DNSUpstreams, cfg.Log)
	}
	return &Agent{
		cfg:        cfg,
		api:        client.New(cfg.HTTP, cfg.BaseURL),
		nodes:      computev1alpha1connect.NewNodeServiceClient(cfg.HTTP, cfg.BaseURL),
		machines:   computev1alpha1connect.NewMachineServiceClient(cfg.HTTP, cfg.BaseURL),
		networks:   networkv1alpha1connect.NewNetworkServiceClient(cfg.HTTP, cfg.BaseURL),
		netChecked: map[string]int64{},
		puller:     puller,
		stops:      newStops(),
		trigger:    make(chan struct{}, 1),
		netsvc:     netsvc,
		netTrigger: make(chan struct{}, 1),
	}, nil
}

// Run reports the node and, with drivers, runs its machines, until ctx is
// done. Failures are logged and retried, never fatal: the apiserver judges
// a node by its reports.
func (a *Agent) Run(ctx context.Context) error {
	// Stops end with ctx; the sweep and the reconciles begin them.
	defer a.stops.wg.Wait()
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if a.cfg.Tunnel != nil {
		wg.Go(func() { a.serveTunnel(ctx) })
	}
	if a.cfg.Drivers != nil {
		wg.Add(3)
		go func() {
			defer wg.Done()
			_ = a.machineController(ctx).Run(ctx)
			a.puller.wait() // its jobs end with ctx
		}()
		go func() {
			defer wg.Done()
			_ = a.networkController().Run(ctx)
			a.netsvc.services.CloseAll()
		}()
		go func() {
			defer wg.Done()
			a.sweepLoop(ctx)
		}()
	} else {
		a.cfg.Log.Warn("no host drivers: reporting the node, running no machines")
	}
	ticker := a.cfg.Clock.NewTicker(a.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		if err := a.report(ctx); err != nil && ctx.Err() == nil {
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
		select {
		case <-ctx.Done():
			return
		case <-ticker.Chan():
		}
	}
}

// machineController runs the machines placed on this node. Besides changes
// to machines and their networks, anything the drivers notice on the host
// checks every machine again.
func (a *Agent) machineController(ctx context.Context) *controller.Controller {
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
	})
	go forward(a.cfg.Drivers.Hypervisor.Changes(), func() {})
	// An image made ready is looked at as soon as it is.
	a.puller.start(ctx, a.recheck)
	return &controller.Controller{
		Name:       "machines",
		Source:     controller.APISource{Client: a.api},
		Reconciler: controller.ReconcilerFunc(a.reconcileMachine),
		Watches: []controller.Watch{
			{Kind: registry.Machine.Kind, Map: a.machineIfMine},
			// A network getting its transit link unblocks its machines. On a
			// resync every machine is queued already.
			{Kind: registry.Network.Kind, Map: a.machinesOnNetwork, SkipResync: true},
		},
		Trigger: a.trigger,
		Clock:   a.cfg.Clock,
		Log:     a.cfg.Log,
	}
}

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
// machines, and that the agent is alive.
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
