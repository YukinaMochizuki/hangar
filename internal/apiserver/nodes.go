package apiserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// NodeReady is the Node condition that says the agent is reporting. The agent
// sets it true with every report; the apiserver turns it Unknown when the
// reports stop.
const NodeReady = "Ready"

// heartbeats remembers, on the apiserver's own clock, when each node agent
// last wrote its Node's status. The agent's last_heartbeat_time is its own
// clock's opinion and is never compared with ours. It lives in memory: after
// a restart every node gets a full grace period from the moment the monitor
// starts.
type heartbeats struct {
	mu    sync.Mutex
	start time.Time
	heard map[string]time.Time
}

func (h *heartbeats) record(node string, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.heard == nil {
		h.heard = map[string]time.Time{}
	}
	h.heard[node] = at
}

// since returns when the grace period of a node began, and whether that was
// a report heard since the monitor started rather than the start itself.
func (h *heartbeats) since(node string) (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.heard[node]; ok && t.After(h.start) {
		return t, true
	}
	return h.start, false
}

// ensureNodes creates a Node for every configured agent that has none. An
// existing Node is left as it is, so a cordon survives restarts.
func (s *Server) ensureNodes(ctx context.Context) error {
	b := s.backend(registry.Node)
	for _, name := range s.cfg.Nodes {
		_, err := s.store.Get(ctx, b.storeKey(resource.Key{Name: name}))
		if err == nil {
			continue
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		_, err = b.Apply(s.system(ctx), &computev1alpha1.Node{
			Metadata: &metav1.ObjectMeta{Name: name},
			Spec:     &computev1alpha1.NodeSpec{},
		})
		if err != nil {
			return fmt.Errorf("registering node %s: %w", name, err)
		}
		s.log.Info("registered node", "node", name)
	}
	return nil
}

// runNodeMonitor marks nodes whose agent stopped reporting as Ready=Unknown,
// so nothing new is scheduled onto them.
func (s *Server) runNodeMonitor(ctx context.Context) error {
	s.heartbeats.mu.Lock()
	s.heartbeats.start = s.clock.Now()
	s.heartbeats.mu.Unlock()
	ticker := s.clock.NewTicker(s.cfg.NodeMonitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.Chan():
		}
		if err := s.checkNodes(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("checking node heartbeats failed", "error", err)
		}
	}
}

func (s *Server) checkNodes(ctx context.Context) error {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Node.Kind})
	if err != nil {
		return err
	}
	for _, o := range objs {
		if err := s.markIfSilent(ctx, o, s.clock.Now()); err != nil {
			return err
		}
	}
	return nil
}

// markIfSilent marks one node Ready=Unknown if its agent has been silent for
// longer than the grace period. The write is conditional on the node being
// as read, so a report that raced in is never overwritten with older status.
func (s *Server) markIfSilent(ctx context.Context, o store.Object, now time.Time) error {
	b := s.backend(registry.Node)
	obj, err := b.decode(o)
	if err != nil {
		return err
	}
	node := obj.(*computev1alpha1.Node)
	status := node.GetStatus()
	if conditions.Find(status.GetConditions(), NodeReady).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_UNKNOWN {
		return nil
	}
	since, heard := s.heartbeats.since(o.Name)
	if now.Sub(since) <= s.cfg.NodeGracePeriod {
		return nil
	}
	cond := &metav1.Condition{
		Type:               NodeReady,
		Status:             metav1.ConditionStatus_CONDITION_STATUS_UNKNOWN,
		Reason:             "NodeStatusUnknown",
		Message:            fmt.Sprintf("no report from the node agent since %s", since.UTC().Format(time.RFC3339)),
		ObservedGeneration: node.GetMetadata().GetGeneration(),
	}
	switch {
	case heard:
	case proto.Size(status) == 0:
		// Nothing the agent could have written is there.
		cond.Reason = "NodeNeverReported"
		cond.Message = "the node agent has never reported"
	default:
		cond.Message = fmt.Sprintf("no report from the node agent since the apiserver started at %s", since.UTC().Format(time.RFC3339))
	}
	updated := &computev1alpha1.NodeStatus{}
	if status != nil {
		updated = proto.Clone(status).(*computev1alpha1.NodeStatus)
	}
	updated.Conditions = conditions.Set(updated.GetConditions(), cond, now)
	rev := o.Revision
	_, err = b.UpdateStatus(s.system(ctx), resource.Key{Name: o.Name}, updated, &rev)
	// A report that raced in is newer news; the next check decides again.
	if connect.CodeOf(err) == connect.CodeAborted {
		return nil
	}
	if err != nil {
		return err
	}
	s.log.Warn("node stopped reporting", "node", o.Name, "reason", cond.GetReason())
	return nil
}
