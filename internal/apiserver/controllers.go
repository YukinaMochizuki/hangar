package apiserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

const (
	// NodeCleanupFinalizer keeps a deleted machine in place until its node
	// has freed what it held, which the node reports with Released.
	NodeCleanupFinalizer = "hangar.dev/node-cleanup"
	// MachineScheduled is the Machine condition the scheduler owns. The
	// scheduler also sets phase Pending on a machine no node has reported on;
	// every other part of a machine's status is its node's.
	MachineScheduled = "Scheduled"
	// MachineReleased is the Machine condition a node sets once a deleted
	// machine's VM and disks are gone from the host.
	MachineReleased = "Released"
	// ContainerReady is the Container condition saying its machine runs the
	// current spec on a node that is reporting.
	ContainerReady = "Ready"
	// DefaultNetwork is the network every container's machine joins.
	DefaultNetwork = "default"
)

// Sizes a container gets when its spec leaves them out.
const (
	defaultVCPUs     = 1
	defaultMemoryMiB = 512
)

func (s *Server) controllers() []*controller.Controller {
	return []*controller.Controller{
		{
			Name: "container", Store: s.store, Clock: s.clock, Log: s.log,
			Reconciler: controller.ReconcilerFunc(s.reconcileContainer),
			Watches: []controller.Watch{
				{Kind: registry.Container.Kind, Map: controller.Self},
				// A machine on a node changing may make room for a resize
				// another container is waiting on.
				{Kind: registry.Machine.Kind, Map: s.machineOwnerAndNeighbours},
				// A node going quiet changes what its containers report.
				{Kind: registry.Node.Kind, Map: s.containersOnNode},
			},
		},
		{
			Name: "scheduler", Store: s.store, Clock: s.clock, Log: s.log,
			Reconciler: controller.ReconcilerFunc(s.reconcileMachine),
			Watches: []controller.Watch{
				{Kind: registry.Machine.Kind, Map: s.machineAndWaiting},
				// Any node change may make room for a machine nobody could take.
				{Kind: registry.Node.Kind, Map: s.unscheduledMachines},
			},
		},
	}
}

// machineName is the machine a container runs as: prefixed so it cannot
// collide with machines made from other kinds, and shortened with a hash when
// the prefix would push it past a DNS label.
func machineName(container string) string {
	const prefix = "ct-"
	if len(prefix)+len(container) <= 63 {
		return prefix + container
	}
	sum := sha256.Sum256([]byte(container))
	return prefix + container[:63-len(prefix)-9] + "-" + hex.EncodeToString(sum[:])[:8]
}

func decodeMachine(o store.Object) (*computev1alpha1.Machine, bool) {
	var m computev1alpha1.Machine
	return &m, proto.Unmarshal(o.Data, &m) == nil
}

func containerOf(project string, m *computev1alpha1.Machine) []controller.Key {
	for _, ref := range m.GetMetadata().GetOwnerReferences() {
		if ref.GetController() && ref.GetKind() == registry.Container.Kind {
			return []controller.Key{{Project: project, Name: ref.GetName()}}
		}
	}
	return nil
}

// machineOwnerAndNeighbours maps a machine to its container and, when it sits
// on a node, to the containers of every machine on that node.
func (s *Server) machineOwnerAndNeighbours(ctx context.Context, o store.Object) []controller.Key {
	m, ok := decodeMachine(o)
	if !ok {
		return nil
	}
	keys := containerOf(o.Project, m)
	if node := m.GetSpec().GetNode(); node != "" {
		keys = append(keys, s.containersOnNode(ctx, store.Object{Key: store.Key{Name: node}})...)
	}
	return keys
}

// listMachines decodes every machine; a failure is logged and the next resync
// maps everything again.
func (s *Server) listMachines(ctx context.Context) []store.Object {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind})
	if err != nil {
		s.log.Error("listing machines failed", "error", err)
		return nil
	}
	return objs
}

// containersOnNode maps a node to the containers whose machines it runs.
func (s *Server) containersOnNode(ctx context.Context, n store.Object) []controller.Key {
	var keys []controller.Key
	for _, o := range s.listMachines(ctx) {
		if m, ok := decodeMachine(o); ok && m.GetSpec().GetNode() == n.Name {
			keys = append(keys, containerOf(o.Project, m)...)
		}
	}
	return keys
}

func (s *Server) unscheduledMachines(ctx context.Context, _ store.Object) []controller.Key {
	var keys []controller.Key
	for _, o := range s.listMachines(ctx) {
		if m, ok := decodeMachine(o); ok && m.GetSpec().GetNode() == "" {
			keys = append(keys, controller.Key{Project: o.Project, Name: o.Name})
		}
	}
	return keys
}

// machineAndWaiting maps a machine to itself and, when it sits on a node, to
// every machine waiting for room: it shrinking or going may make some.
func (s *Server) machineAndWaiting(ctx context.Context, o store.Object) []controller.Key {
	keys := controller.Self(ctx, o)
	if m, ok := decodeMachine(o); ok && m.GetSpec().GetNode() != "" {
		keys = append(keys, s.unscheduledMachines(ctx, o)...)
	}
	return keys
}

// get reads an object of kind r as the system; a missing object is nil.
func (s *Server) get(ctx context.Context, r registry.Resource, key resource.Key) (proto.Message, error) {
	b := s.backend(r)
	o, err := s.store.Get(ctx, b.storeKey(key))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return b.decode(o)
}

// blocked is a reconcile that cannot go on until someone changes something.
// It is reported on the object rather than retried.
type blocked struct{ reason, message string }

func (b *blocked) Error() string { return b.message }

// reconcileContainer keeps a container's machine in line with its spec, and
// reports the machine's state back on the container.
func (s *Server) reconcileContainer(ctx context.Context, k controller.Key) error {
	key := resource.Key{Project: k.Project, Name: k.Name}
	obj, err := s.get(ctx, registry.Container, key)
	if err != nil || obj == nil {
		return err
	}
	c := obj.(*computev1alpha1.Container)
	// Deleting cascades to the machine through its owner reference; a new
	// machine for a container on its way out would only be collected again.
	if c.GetMetadata().GetDeleteTime() != nil {
		return nil
	}
	mobj, err := s.get(ctx, registry.Machine, resource.Key{Project: k.Project, Name: machineName(k.Name)})
	if err != nil {
		return err
	}
	var m *computev1alpha1.Machine
	if mobj != nil {
		m = mobj.(*computev1alpha1.Machine)
	}
	m, err = s.ensureMachine(ctx, c, m)
	var stuck *blocked
	if errors.As(err, &stuck) {
		return s.reportContainer(ctx, c, m, nil, stuck)
	}
	if err != nil {
		return err
	}
	var node *computev1alpha1.Node
	if name := m.GetSpec().GetNode(); name != "" {
		nobj, err := s.get(ctx, registry.Node, resource.Key{Name: name})
		if err != nil {
			return err
		}
		if nobj != nil {
			node = nobj.(*computev1alpha1.Node)
		}
	}
	return s.reportContainer(ctx, c, m, node, nil)
}

// ensureMachine brings m, the container's machine as read (nil if there is
// none), in line with the container. Every write is conditional on m.
func (s *Server) ensureMachine(ctx context.Context, c *computev1alpha1.Container, m *computev1alpha1.Machine) (*computev1alpha1.Machine, error) {
	meta := c.GetMetadata()
	want := machineSpec(c.GetSpec())
	if m == nil {
		created := &computev1alpha1.Machine{
			Metadata: &metav1.ObjectMeta{
				Project: meta.GetProject(), Name: machineName(meta.GetName()),
				Finalizers: []string{NodeCleanupFinalizer},
				OwnerReferences: []*metav1.OwnerReference{{
					ApiVersion: registry.Container.APIVersion(), Kind: registry.Container.Kind,
					Name: meta.GetName(), Uid: meta.GetUid(), Controller: true,
				}},
			},
			Spec: want,
		}
		res, err := s.backend(registry.Machine).Apply(s.system(ctx), created)
		if err != nil {
			return nil, err
		}
		return res.Object.(*computev1alpha1.Machine), nil
	}
	if !ownedBy(m.GetMetadata(), meta.GetUid()) {
		return nil, &blocked{"MachineNameInUse", fmt.Sprintf(
			"machine %s belongs to another object, such as an earlier container of this name still being deleted", m.GetMetadata().GetName())}
	}
	if m.GetMetadata().GetDeleteTime() != nil {
		return m, nil
	}
	// The node is the scheduler's to set; everything else follows the
	// container.
	want.Node = m.GetSpec().GetNode()
	if proto.Equal(want, m.GetSpec()) {
		return m, nil
	}
	if node := want.GetNode(); node != "" && want.GetResources().GetMemoryMib() > m.GetSpec().GetResources().GetMemoryMib() {
		// The check and the write are one decision, as a placement is.
		s.memory.Lock()
		defer s.memory.Unlock()
		free, err := s.freeMemory(ctx, node, resource.Key{Project: m.GetMetadata().GetProject(), Name: m.GetMetadata().GetName()})
		if err != nil {
			return nil, err
		}
		if want.GetResources().GetMemoryMib() > free {
			return m, &blocked{"ResizeDoesNotFit", fmt.Sprintf(
				"node %s has %d MiB free for this machine, not %d; the change is not applied",
				node, free, want.GetResources().GetMemoryMib())}
		}
	}
	updated := proto.Clone(m).(*computev1alpha1.Machine)
	updated.Spec = want
	res, err := s.backend(registry.Machine).Apply(s.system(ctx), updated)
	if err != nil {
		return nil, err
	}
	return res.Object.(*computev1alpha1.Machine), nil
}

func ownedBy(meta *metav1.ObjectMeta, uid string) bool {
	return slices.ContainsFunc(meta.GetOwnerReferences(), func(r *metav1.OwnerReference) bool {
		return r.GetController() && r.GetUid() == uid
	})
}

// machineSpec is what a container's spec asks of its machine; node is left
// for the scheduler.
func machineSpec(cs *computev1alpha1.ContainerSpec) *computev1alpha1.MachineSpec {
	res := &computev1alpha1.Resources{
		Vcpus:     cs.GetResources().GetVcpus(),
		MemoryMib: cs.GetResources().GetMemoryMib(),
	}
	if res.Vcpus == 0 {
		res.Vcpus = defaultVCPUs
	}
	if res.MemoryMib == 0 {
		res.MemoryMib = defaultMemoryMiB
	}
	restart := cs.GetRestartPolicy()
	if restart == computev1alpha1.RestartPolicy_RESTART_POLICY_UNSPECIFIED {
		restart = computev1alpha1.RestartPolicy_RESTART_POLICY_ALWAYS
	}
	return &computev1alpha1.MachineSpec{
		Resources: res,
		RunState:  computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING,
		Boot: &computev1alpha1.MachineSpec_Container{Container: &computev1alpha1.ContainerBoot{
			Image: cs.GetImage(), Command: cs.GetCommand(), Args: cs.GetArgs(), Env: cs.GetEnv(),
		}},
		Interfaces:    []*computev1alpha1.MachineInterface{{Name: "eth0", Network: DefaultNetwork}},
		RestartPolicy: restart,
	}
}

// reportContainer writes the container's status, conditional on c as read:
// from a reconcile that is stuck, or else from its machine and that
// machine's node.
func (s *Server) reportContainer(ctx context.Context, c *computev1alpha1.Container, m *computev1alpha1.Machine, node *computev1alpha1.Node, stuck *blocked) error {
	gen := c.GetMetadata().GetGeneration()
	var ready *metav1.Condition
	if stuck != nil {
		ready = &metav1.Condition{Type: ContainerReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: stuck.reason, Message: stuck.message}
	} else {
		ready = machineReadiness(m, node)
	}
	ready.ObservedGeneration = gen
	status := &computev1alpha1.ContainerStatus{}
	if c.GetStatus() != nil {
		status = proto.Clone(c.GetStatus()).(*computev1alpha1.ContainerStatus)
	}
	status.ObservedGeneration = gen
	status.Conditions = conditions.Set(status.GetConditions(), ready, s.clock.Now())
	if proto.Equal(status, c.GetStatus()) {
		return nil
	}
	rv := c.GetMetadata().GetResourceVersion()
	key := resource.Key{Project: c.GetMetadata().GetProject(), Name: c.GetMetadata().GetName()}
	_, err := s.backend(registry.Container).UpdateStatus(s.system(ctx), key, status, &rv)
	return err
}

// machineReadiness says whether the machine runs its current spec on a node
// that is reporting; node is the machine's node, nil if it has none.
func machineReadiness(m *computev1alpha1.Machine, node *computev1alpha1.Node) *metav1.Condition {
	notReady := func(reason, msg string) *metav1.Condition {
		return &metav1.Condition{Type: ContainerReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reason, Message: msg}
	}
	st := m.GetStatus()
	if sched := conditions.Find(st.GetConditions(), MachineScheduled); sched.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE {
		return notReady("Unschedulable", sched.GetMessage())
	}
	if name := m.GetSpec().GetNode(); name != "" && !conditions.IsTrue(node.GetStatus().GetConditions(), NodeReady) {
		// What the node last said may no longer be true.
		return &metav1.Condition{
			Type: ContainerReady, Status: metav1.ConditionStatus_CONDITION_STATUS_UNKNOWN,
			Reason: "NodeNotReporting", Message: "node " + name + " is not reporting",
		}
	}
	if st.GetObservedGeneration() < m.GetMetadata().GetGeneration() {
		return notReady("Updating", "the node has not yet applied the latest change")
	}
	switch st.GetPhase() {
	case computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING:
		return &metav1.Condition{Type: ContainerReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Running"}
	case computev1alpha1.MachinePhase_MACHINE_PHASE_UNSPECIFIED:
		return notReady("Pending", "")
	}
	phase := strings.TrimPrefix(st.GetPhase().String(), "MACHINE_PHASE_")
	return notReady(strings.ToUpper(phase[:1])+strings.ToLower(phase[1:]), "")
}

// reconcileMachine places an unplaced machine on a node, and lets a deleted
// one go once its node has released it.
func (s *Server) reconcileMachine(ctx context.Context, k controller.Key) error {
	key := resource.Key{Project: k.Project, Name: k.Name}
	obj, err := s.get(ctx, registry.Machine, key)
	if err != nil || obj == nil {
		return err
	}
	return s.scheduleMachine(ctx, key, obj.(*computev1alpha1.Machine))
}

// scheduleMachine acts on m as read; every write is conditional on it.
func (s *Server) scheduleMachine(ctx context.Context, key resource.Key, m *computev1alpha1.Machine) error {
	if m.GetMetadata().GetDeleteTime() != nil {
		if m.GetSpec().GetNode() == "" || conditions.IsTrue(m.GetStatus().GetConditions(), MachineReleased) {
			return s.RemoveFinalizer(ctx, registry.Machine, key, NodeCleanupFinalizer)
		}
		return nil
	}
	if m.GetSpec().GetNode() != "" {
		return s.setScheduled(ctx, key, m, &metav1.Condition{
			Type: MachineScheduled, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Placed",
			Message: "running on node " + m.GetSpec().GetNode(),
		})
	}
	placed, node, why, err := s.place(ctx, m)
	if err != nil {
		return err
	}
	if placed == nil {
		return s.setScheduled(ctx, key, m, &metav1.Condition{
			Type: MachineScheduled, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "NoNodeAvailable", Message: why,
		})
	}
	return s.setScheduled(ctx, key, placed, &metav1.Condition{
		Type: MachineScheduled, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Placed",
		Message: "running on node " + node,
	})
}

// place picks a node for m and writes it, as one decision about the node's
// memory. Without a fit it returns no machine and says why.
func (s *Server) place(ctx context.Context, m *computev1alpha1.Machine) (*computev1alpha1.Machine, string, string, error) {
	s.memory.Lock()
	defer s.memory.Unlock()
	node, why, err := s.pickNode(ctx, m)
	if err != nil || node == "" {
		return nil, "", why, err
	}
	placed := proto.Clone(m).(*computev1alpha1.Machine)
	placed.Spec.Node = node
	res, err := s.backend(registry.Machine).Apply(s.system(ctx), placed)
	if err != nil {
		return nil, "", "", err
	}
	return res.Object.(*computev1alpha1.Machine), node, "", nil
}

// setScheduled records the scheduler's condition on m as read, leaving the
// rest of the status, which the node owns, as it is.
func (s *Server) setScheduled(ctx context.Context, key resource.Key, m *computev1alpha1.Machine, c *metav1.Condition) error {
	c.ObservedGeneration = m.GetMetadata().GetGeneration()
	old := conditions.Find(m.GetStatus().GetConditions(), MachineScheduled)
	if old != nil && old.GetStatus() == c.GetStatus() && old.GetReason() == c.GetReason() &&
		old.GetMessage() == c.GetMessage() && old.GetObservedGeneration() == c.GetObservedGeneration() {
		return nil
	}
	status := &computev1alpha1.MachineStatus{}
	if m.GetStatus() != nil {
		status = proto.Clone(m.GetStatus()).(*computev1alpha1.MachineStatus)
	}
	status.Conditions = conditions.Set(status.GetConditions(), c, s.clock.Now())
	if status.GetPhase() == computev1alpha1.MachinePhase_MACHINE_PHASE_UNSPECIFIED {
		status.Phase = computev1alpha1.MachinePhase_MACHINE_PHASE_PENDING
	}
	rv := m.GetMetadata().GetResourceVersion()
	_, err := s.backend(registry.Machine).UpdateStatus(s.system(ctx), key, status, &rv)
	if connect.CodeOf(err) == connect.CodeAborted {
		return fmt.Errorf("machine %s changed while recording %s: %w", key.Name, MachineScheduled, err)
	}
	return err
}

// nodeMemory returns every node and the memory machines hold on each. A
// machine still being deleted holds its memory until released.
func (s *Server) nodeMemory(ctx context.Context, except resource.Key) ([]*computev1alpha1.Node, map[string]int64, error) {
	nodeObjs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Node.Kind})
	if err != nil {
		return nil, nil, err
	}
	machineObjs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind})
	if err != nil {
		return nil, nil, err
	}
	used := map[string]int64{}
	for _, o := range machineObjs {
		if o.Project == except.Project && o.Name == except.Name {
			continue
		}
		m, ok := decodeMachine(o)
		if !ok {
			return nil, nil, fmt.Errorf("decode machine %s", o.Name)
		}
		if n := m.GetSpec().GetNode(); n != "" {
			used[n] += m.GetSpec().GetResources().GetMemoryMib()
		}
	}
	nodes := make([]*computev1alpha1.Node, 0, len(nodeObjs))
	for _, o := range nodeObjs {
		var n computev1alpha1.Node
		if err := proto.Unmarshal(o.Data, &n); err != nil {
			return nil, nil, err
		}
		nodes = append(nodes, &n)
	}
	return nodes, used, nil
}

func memoryOf(n *computev1alpha1.Node) int64 {
	if a := n.GetStatus().GetAllocatable().GetMemoryMib(); a != 0 {
		return a
	}
	return n.GetStatus().GetCapacity().GetMemoryMib()
}

// freeMemory is the memory a node has for one machine, not counting what
// that machine holds itself.
func (s *Server) freeMemory(ctx context.Context, node string, machine resource.Key) (int64, error) {
	nodes, used, err := s.nodeMemory(ctx, machine)
	if err != nil {
		return 0, err
	}
	for _, n := range nodes {
		if n.GetMetadata().GetName() == node {
			return memoryOf(n) - used[node], nil
		}
	}
	return 0, nil
}

// pickNode chooses the ready, schedulable node with the most free memory that
// fits the machine. Memory is the only resource that cannot be overcommitted;
// vCPUs are shared. With no fit, it says why.
func (s *Server) pickNode(ctx context.Context, m *computev1alpha1.Machine) (string, string, error) {
	nodes, used, err := s.nodeMemory(ctx, resource.Key{})
	if err != nil {
		return "", "", err
	}
	type candidate struct {
		name string
		free int64
	}
	var fits []candidate
	need := m.GetSpec().GetResources().GetMemoryMib()
	ready, schedulable := 0, 0
	for _, n := range nodes {
		if !conditions.IsTrue(n.GetStatus().GetConditions(), NodeReady) {
			continue
		}
		ready++
		if n.GetSpec().GetUnschedulable() {
			continue
		}
		schedulable++
		name := n.GetMetadata().GetName()
		if free := memoryOf(n) - used[name]; free >= need {
			fits = append(fits, candidate{name, free})
		}
	}
	if len(fits) == 0 {
		switch {
		case len(nodes) == 0:
			return "", "no nodes are registered", nil
		case ready == 0:
			return "", "no node is ready", nil
		case schedulable == 0:
			return "", "every ready node is cordoned", nil
		}
		return "", fmt.Sprintf("no ready node has %d MiB of memory free", need), nil
	}
	sort.Slice(fits, func(i, j int) bool {
		if fits[i].free != fits[j].free {
			return fits[i].free > fits[j].free
		}
		return fits[i].name < fits[j].name
	})
	return fits[0].name, "", nil
}
