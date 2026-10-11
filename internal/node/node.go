// Package node is the node agent, hangar-node: it reports its host to the
// apiserver and turns the machines placed on it into running microVMs. It
// keeps no state of its own; what the host runs is read back from the host.
// See docs/architecture.md.
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

// Config configures an Agent. Zero values take defaults.
type Config struct {
	// Name is the Node the agent reports as; the apiserver must accept it as
	// that node.
	Name string
	// HTTP and BaseURL reach the apiserver's node socket.
	HTTP    connect.HTTPClient
	BaseURL string
	// Host reports what the host has. Defaults to reading this host.
	Host func() (*computev1alpha1.NodeResources, error)
	// Reserved is kept back from machines for the host itself.
	Reserved *computev1alpha1.NodeResources
	// HeartbeatInterval is how often the agent reports. The default of ten
	// seconds fits the apiserver's NodeGracePeriod with three reports to
	// spare.
	HeartbeatInterval time.Duration
	Clock             clockwork.Clock
	Log               *slog.Logger
}

// Agent is a running node agent.
type Agent struct {
	cfg   Config
	nodes computev1alpha1connect.NodeServiceClient
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
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 10 * time.Second
	}
	if cfg.Clock == nil {
		cfg.Clock = clockwork.NewRealClock()
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Agent{
		cfg:   cfg,
		nodes: computev1alpha1connect.NewNodeServiceClient(cfg.HTTP, cfg.BaseURL),
	}, nil
}

// Run reports the node until ctx is done. A failed report is logged and
// tried again at the next beat: the apiserver judges the silence.
func (a *Agent) Run(ctx context.Context) error {
	log := a.cfg.Log.With("node", a.cfg.Name)
	ticker := a.cfg.Clock.NewTicker(a.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		if err := a.report(ctx); err != nil && ctx.Err() == nil {
			log.Error("reporting the node failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.Chan():
		}
	}
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
	status.Capacity = capacity
	status.Allocatable = allocatable(capacity, a.cfg.Reserved)
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

// allocatable is what the host has less what it keeps for itself, never
// below nothing.
func allocatable(capacity, reserved *computev1alpha1.NodeResources) *computev1alpha1.NodeResources {
	return &computev1alpha1.NodeResources{
		Vcpus:     max(capacity.GetVcpus()-reserved.GetVcpus(), 0),
		MemoryMib: max(capacity.GetMemoryMib()-reserved.GetMemoryMib(), 0),
	}
}

// Version is the agent's build: the VCS revision it was built from, marked
// when the tree had uncommitted changes.
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	return versionOf(info)
}

func versionOf(info *debug.BuildInfo) string {
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return info.Main.Version
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}
