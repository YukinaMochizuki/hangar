package apiserver

import (
	"cmp"
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
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

const (
	// NodeCleanupFinalizer keeps a deleted machine in place until its node
	// has freed what it held, which the node reports with Released.
	NodeCleanupFinalizer = registry.Domain + "/node-cleanup"
	// MachineScheduled and MachineReleased are described in computeapi.
	MachineScheduled = computeapi.MachineScheduled
	MachineReleased  = computeapi.MachineReleased
	// ContainerReady is the Container condition saying its machine runs the
	// current spec on a node that is reporting.
	ContainerReady = "Ready"
	// ContainerStopped is the Container condition saying a stop asked for
	// with spec.run_state has taken effect: the machine's VM is down for
	// good, its disk kept. False while the container is asked to run.
	ContainerStopped = "Stopped"
	// DefaultNetwork is the network every container's machine joins.
	DefaultNetwork = "default"
)

func (s *Server) newControllers() []*controller.Controller {
	return []*controller.Controller{
		{
			Name: "container", Source: controller.StoreSource{Store: s.store}, Clock: s.clock, Log: s.log,
			Reconciler: controller.ReconcilerFunc(s.reconcileContainer),
			Watches: []controller.Watch{
				{Kind: registry.Container.Kind, Map: controller.Self},
				// A machine on a node changing may make room for a resize
				// another container is waiting on, and a machine going frees
				// its address.
				{Kind: registry.Machine.Kind, Map: s.machineOwnerAndNeighbours, SkipResync: true},
				// A node going quiet changes what its containers report.
				{Kind: registry.Node.Kind, Map: s.containersOnNode, SkipResync: true},
				// A network appearing unblocks containers waiting for an address.
				{Kind: registry.Network.Kind, Map: s.containersInProject, SkipResync: true},
				// A disk going may make room for a resize.
				{Kind: registry.Disk.Kind, Map: s.containersOnDiskNode, SkipResync: true},
				// An Address binding, moving or going changes the interfaces
				// of the containers it binds or leaves.
				{Kind: registry.Address.Kind, Map: s.targetsOfAddress(registry.Container), SkipResync: true},
				// A secret appearing or going is what a container waiting on
				// one reports.
				{Kind: registry.Secret.Kind, Map: s.containersInProject, SkipResync: true},
			},
		},
		{
			Name: "secret", Source: controller.StoreSource{Store: s.store}, Clock: s.clock, Log: s.log,
			Reconciler: controller.ReconcilerFunc(s.reconcileSecret),
			Watches:    []controller.Watch{{Kind: registry.Secret.Kind, Map: controller.Self}},
		},
		{
			Name: "virtualmachine", Source: controller.StoreSource{Store: s.store}, Clock: s.clock, Log: s.log,
			Reconciler: controller.ReconcilerFunc(s.reconcileVirtualMachine),
			Watches: []controller.Watch{
				{Kind: registry.VirtualMachine.Kind, Map: controller.Self},
				{Kind: registry.Machine.Kind, Map: s.vmOfMachineAndNeighbours, SkipResync: true},
				// A node going quiet changes what its VMs report.
				{Kind: registry.Node.Kind, Map: s.vmsOnNode, SkipResync: true},
				// A network appearing unblocks VMs waiting for an address, a
				// disk changing those that boot from it or keep it.
				{Kind: registry.Network.Kind, Map: s.vmsInProject, SkipResync: true},
				{Kind: registry.Disk.Kind, Map: s.vmsInProject, SkipResync: true},
				// An image fixed lets a root disk be made of it.
				{Kind: registry.Image.Kind, Map: s.vmsOfImage, SkipResync: true},
				{Kind: registry.Address.Kind, Map: s.targetsOfAddress(registry.VirtualMachine), SkipResync: true},
			},
		},
		{
			Name: "address", Source: controller.StoreSource{Store: s.store}, Clock: s.clock, Log: s.log,
			Reconciler: controller.ReconcilerFunc(s.reconcileAddress),
			Watches: []controller.Watch{
				{Kind: registry.Address.Kind, Map: controller.Self},
				{Kind: registry.Machine.Kind, Map: s.addressesOfMachine, SkipResync: true},
				// A node's prefix is what an address is reserved from.
				{Kind: registry.Node.Kind, Map: s.addressesOnNode, SkipResync: true},
			},
		},
		{
			Name: "publicendpoint", Source: controller.StoreSource{Store: s.store}, Clock: s.clock, Log: s.log,
			Reconciler: controller.ReconcilerFunc(s.reconcilePublicEndpoint),
			Watches: []controller.Watch{
				{Kind: registry.PublicEndpoint.Kind, Map: controller.Self},
				// Its target's machine coming, going, running or moving.
				{Kind: registry.Machine.Kind, Map: s.endpointsOfMachine, SkipResync: true},
				// A node going quiet changes whether its endpoints serve.
				{Kind: registry.Node.Kind, Map: s.endpointsOnNode, SkipResync: true},
			},
		},
		{
			Name: "container-endpoints", Source: controller.StoreSource{Store: s.store}, Clock: s.clock, Log: s.log,
			Reconciler: controller.ReconcilerFunc(s.reconcileContainerEndpoints),
			Watches: []controller.Watch{
				{Kind: registry.Container.Kind, Map: controller.Self},
				// An endpoint it owns changed or deleted, or one holding a name
				// it wants gone.
				{Kind: registry.PublicEndpoint.Kind, Map: s.containersOfEndpoint, SkipResync: true},
			},
		},
		{
			Name: "disk", Source: controller.StoreSource{Store: s.store}, Clock: s.clock, Log: s.log,
			Reconciler: controller.ReconcilerFunc(s.reconcileDisk),
			Watches: []controller.Watch{
				{Kind: registry.Disk.Kind, Map: controller.Self},
				{Kind: registry.Machine.Kind, Map: s.disksOfMachineAndWaiting},
				// Any node change may make room for a disk to grow.
				{Kind: registry.Node.Kind, Map: s.disksWaitingForRoom, SkipResync: true},
			},
		},
		{
			Name: "project-networks", Source: controller.StoreSource{Store: s.store}, Clock: s.clock, Log: s.log,
			Reconciler: controller.ReconcilerFunc(s.reconcileProject),
			Watches: []controller.Watch{
				{Kind: registry.Project.Kind, Map: controller.Self},
				{Kind: registry.Network.Kind, Map: projectOfNetwork},
			},
		},
		{
			Name: "network", Source: controller.StoreSource{Store: s.store}, Clock: s.clock, Log: s.log,
			Reconciler: controller.ReconcilerFunc(s.reconcileNetwork),
			Watches: []controller.Watch{
				{Kind: registry.Network.Kind, Map: controller.Self},
				{Kind: registry.Machine.Kind, Map: networksOfMachine},
			},
		},
		{
			Name: "scheduler", Source: controller.StoreSource{Store: s.store}, Clock: s.clock, Log: s.log,
			Reconciler: controller.ReconcilerFunc(s.reconcileMachine),
			Watches: []controller.Watch{
				{Kind: registry.Machine.Kind, Map: s.machineAndWaiting},
				// Any node change may make room for a machine nobody could take.
				{Kind: registry.Node.Kind, Map: s.unscheduledMachines},
				// So may a disk going or shrinking, and a disk is where some
				// machines must go.
				{Kind: registry.Disk.Kind, Map: s.unscheduledMachines, SkipResync: true},
			},
		},
		s.usageController(),
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
	keys = append(keys, s.containersWaitingForAddress(ctx, o.Project)...)
	// A machine letting go of an Address's address is what its target
	// waits on.
	return append(keys, s.addressTargets(ctx, o.Project, registry.Container)...)
}

// containersWaitingForAddress maps to the containers in a project told their
// network is full: a machine going frees an address without changing the
// network.
func (s *Server) containersWaitingForAddress(ctx context.Context, project string) []controller.Key {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Container.Kind, Project: project})
	if err != nil {
		s.log.Error("listing containers failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, o := range objs {
		var c computev1alpha1.Container
		if proto.Unmarshal(o.Data, &c) != nil {
			continue
		}
		if conditions.Find(c.GetStatus().GetConditions(), ContainerReady).GetReason() == ReasonNoAddressLeft {
			keys = append(keys, controller.Key{Project: o.Project, Name: o.Name})
		}
	}
	return keys
}

// containersInProject maps an object to every container in its project.
func (s *Server) containersInProject(ctx context.Context, o store.Object) []controller.Key {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Container.Kind, Project: o.Project})
	if err != nil {
		s.log.Error("listing containers failed", "error", err)
		return nil
	}
	keys := make([]controller.Key, 0, len(objs))
	for _, c := range objs {
		keys = append(keys, controller.Key{Project: c.Project, Name: c.Name})
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
		return s.reportContainer(ctx, c, m, nil, stuck, "")
	}
	// An Address moving to it is waited on, and the container reported
	// meanwhile.
	waiting, err := splitWaiting(err)
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
	missing, err := s.missingPullSecret(ctx, c)
	if err != nil {
		return err
	}
	return cmp.Or(s.reportContainer(ctx, c, m, node, nil, missing), waiting)
}

// splitWaiting splits err into what is waited on, wrapping
// controller.ErrWaiting, and any other error.
func splitWaiting(err error) (waiting, other error) {
	if errors.Is(err, controller.ErrWaiting) {
		return err, nil
	}
	return nil, err
}

// ensureMachine brings m, the container's machine as read (nil if there is
// none), in line with the container. Every write is conditional on m.
func (s *Server) ensureMachine(ctx context.Context, c *computev1alpha1.Container, m *computev1alpha1.Machine) (*computev1alpha1.Machine, error) {
	want, err := machineSpec(c.GetSpec())
	if err != nil {
		return m, &blocked{"InvalidResources", err.Error()}
	}
	return s.ensureMachineOf(ctx, registry.Container, c.GetMetadata(), machineName(c.GetMetadata().GetName()), want, m)
}

// ensureMachineOf brings m, the machine name as read (nil if there is
// none), in line with want, for its owner, an object of kind r. Every write
// is conditional on m. Admission refusing want is what the owner reports
// (MachineRefused), not retried: it refuses it again until the owner
// changes.
func (s *Server) ensureMachineOf(ctx context.Context, r registry.Resource, meta *metav1.ObjectMeta, name string, want *computev1alpha1.MachineSpec, m *computev1alpha1.Machine) (*computev1alpha1.Machine, error) {
	key := resource.Key{Project: meta.GetProject(), Name: name}
	owner := ipv6Owner{project: meta.GetProject(), kind: r.Kind, name: meta.GetName(), machine: name}
	if m == nil {
		// Addresses are chosen and written as one decision.
		s.ipam.Lock()
		defer s.ipam.Unlock()
		if err := s.assignAddresses(ctx, owner, nil, want); err != nil {
			return nil, err
		}
		created := &computev1alpha1.Machine{
			Metadata: &metav1.ObjectMeta{
				Project: meta.GetProject(), Name: name,
				Finalizers: []string{NodeCleanupFinalizer},
				OwnerReferences: []*metav1.OwnerReference{{
					ApiVersion: r.APIVersion(), Kind: r.Kind,
					Name: meta.GetName(), Uid: meta.GetUid(), Controller: true,
				}},
			},
			Spec: want,
		}
		res, err := s.backend(registry.Machine).Create(s.system(ctx), created)
		if err != nil {
			return nil, refusedMachine(err)
		}
		return res.Object.(*computev1alpha1.Machine), nil
	}
	if !ownedBy(m.GetMetadata(), meta.GetUid()) {
		return nil, &blocked{"MachineNameInUse", fmt.Sprintf(
			"machine %s belongs to another object, such as an earlier %s of this name still being deleted",
			m.GetMetadata().GetName(), strings.ToLower(r.Kind))}
	}
	if m.GetMetadata().GetDeleteTime() != nil {
		return m, nil
	}
	// The node is the scheduler's to set, and addresses the machine already
	// has stay; everything else follows the container.
	want.Node = m.GetSpec().GetNode()
	s.ipam.Lock()
	defer s.ipam.Unlock()
	// An Address still moving from another machine leaves the interface as
	// it is, and the rest is written all the same; the address follows on a
	// later reconcile.
	moving := s.assignAddresses(ctx, owner, m.GetSpec(), want)
	if moving != nil && !errors.Is(moving, errAddressMoving) {
		return m, moving
	}
	was, err := heldBy(m.GetSpec().GetResources())
	if err != nil {
		return nil, fmt.Errorf("machine %s: %w", key.Name, err)
	}
	now, err := heldBy(want.GetResources())
	if err != nil {
		return nil, err
	}
	// A placed machine's disk never shrinks. Admission lets a container
	// withdraw a growth its machine never got, so one that races the growth
	// landing can ask for less than the machine has; the machine keeps its
	// disk. An unplaced machine has no disk yet.
	if want.GetNode() != "" && now.disk < was.disk {
		want.Resources.Disk, now.disk = m.GetSpec().GetResources().GetDisk(), was.disk
	}
	if proto.Equal(want, m.GetSpec()) {
		return m, moving
	}
	if node := want.GetNode(); node != "" && (now.memory > was.memory || now.disk > was.disk) {
		// The check and the write are one decision, as a placement is.
		s.capacity.Lock()
		defer s.capacity.Unlock()
		if err := s.resizeFits(ctx, node, key, was, now); err != nil {
			return m, err
		}
	}
	updated := proto.Clone(m).(*computev1alpha1.Machine)
	updated.Spec = want
	res, err := s.backend(registry.Machine).Apply(s.system(ctx), updated)
	if err != nil {
		return m, refusedMachine(err)
	}
	return res.Object.(*computev1alpha1.Machine), moving
}

// refusedMachine makes admission refusing a machine as asked for what its
// owner reports; other errors, such as a disk still attached elsewhere,
// are retried.
func refusedMachine(err error) error {
	if connect.CodeOf(err) == connect.CodeInvalidArgument {
		return &blocked{"MachineRefused", err.Error()}
	}
	return err
}

func ownedBy(meta *metav1.ObjectMeta, uid string) bool {
	return slices.ContainsFunc(meta.GetOwnerReferences(), func(r *metav1.OwnerReference) bool {
		return r.GetController() && r.GetUid() == uid
	})
}

// machineSpec is what a container's spec asks of its machine, in canonical
// quantities; node is left for the scheduler.
func machineSpec(cs *computev1alpha1.ContainerSpec) (*computev1alpha1.MachineSpec, error) {
	sz, err := containerSizes(cs.GetResources())
	if err != nil {
		return nil, err
	}
	restart := cs.GetRestartPolicy()
	if restart == computev1alpha1.RestartPolicy_RESTART_POLICY_UNSPECIFIED {
		restart = computev1alpha1.RestartPolicy_RESTART_POLICY_ALWAYS
	}
	runState := computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING
	if cs.GetRunState() == computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED {
		runState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	}
	return &computev1alpha1.MachineSpec{
		Resources: sz.canonical(),
		RunState:  runState,
		Boot: &computev1alpha1.MachineSpec_Container{Container: &computev1alpha1.ContainerBoot{
			Image: cs.GetImage(), Command: cs.GetCommand(), Args: cs.GetArgs(), Env: cs.GetEnv(),
			ImagePullSecrets: slices.Clone(cs.GetImagePullSecrets()),
		}},
		Interfaces:    machineInterfaces(cs.GetInterfaces()),
		RestartPolicy: restart,
	}, nil
}

// machineInterfaces are the interfaces asked for, named by position, or one
// on the default network if none is, each in the groups it names or, naming
// none, in the network's implicit default group.
func machineInterfaces(nics []*computev1alpha1.NetworkInterface) []*computev1alpha1.MachineInterface {
	if len(nics) == 0 {
		nics = []*computev1alpha1.NetworkInterface{{Network: DefaultNetwork}}
	}
	out := make([]*computev1alpha1.MachineInterface, len(nics))
	for i, nic := range nics {
		groups := slices.Clone(nic.GetSecurityGroups())
		if len(groups) == 0 {
			groups = []string{secgroup.Default}
		}
		out[i] = &computev1alpha1.MachineInterface{Name: fmt.Sprintf("eth%d", i), Network: nic.GetNetwork(), SecurityGroups: groups}
	}
	return out
}

// reportContainer writes the container's status, conditional on c as read:
// from a reconcile that is stuck, or else from its machine and that
// machine's node. A container not ready that names an image pull secret
// that does not exist, missingSecret, says so instead: its image cannot be
// pulled with it.
func (s *Server) reportContainer(ctx context.Context, c *computev1alpha1.Container, m *computev1alpha1.Machine, node *computev1alpha1.Node, stuck *blocked, missingSecret string) error {
	gen := c.GetMetadata().GetGeneration()
	var ready, stopped *metav1.Condition
	if stuck != nil {
		ready = &metav1.Condition{Type: ContainerReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: stuck.reason, Message: stuck.message}
		stopped = &metav1.Condition{Type: ContainerStopped, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: stuck.reason, Message: stuck.message}
	} else {
		ready = machineReadiness(m, node)
		if missingSecret != "" && ready.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
			ready = &metav1.Condition{
				Type: ContainerReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: ReasonImagePullSecretNotFound,
				Message: fmt.Sprintf("image pull secret %q does not exist in project %q", missingSecret, c.GetMetadata().GetProject()),
			}
		}
		stopped = machineStopped(c.GetSpec().GetRunState() == computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED, "container", m, node)
	}
	ready.ObservedGeneration = gen
	stopped.ObservedGeneration = gen
	status := &computev1alpha1.ContainerStatus{}
	if c.GetStatus() != nil {
		status = proto.Clone(c.GetStatus()).(*computev1alpha1.ContainerStatus)
	}
	status.ObservedGeneration = gen
	status.Conditions = conditions.Set(status.GetConditions(), ready, s.clock.Now())
	status.Conditions = conditions.Set(status.GetConditions(), stopped, s.clock.Now())
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
	// What stands in the way, as the node says, for the current spec.
	nodeSays := conditions.Find(st.GetConditions(), computeapi.MachineReady)
	if nodeSays.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || nodeSays.GetObservedGeneration() != m.GetMetadata().GetGeneration() {
		nodeSays = nil
	}
	if nodeSays != nil {
		return notReady(nodeSays.GetReason(), nodeSays.GetMessage())
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

// machineStopped says whether an owner, what it is in words, asked to stop
// (stopAsked) has: its node has stopped the machine at the machine's
// current spec. An owner asked to run is never Stopped; Ready says whether
// it runs.
func machineStopped(stopAsked bool, what string, m *computev1alpha1.Machine, node *computev1alpha1.Node) *metav1.Condition {
	cond := func(status metav1.ConditionStatus, reason, msg string) *metav1.Condition {
		return &metav1.Condition{Type: ContainerStopped, Status: status, Reason: reason, Message: msg}
	}
	if !stopAsked {
		return cond(metav1.ConditionStatus_CONDITION_STATUS_FALSE, "RunStateRunning", "the "+what+" is asked to run")
	}
	if name := m.GetSpec().GetNode(); name != "" && !conditions.IsTrue(node.GetStatus().GetConditions(), NodeReady) {
		return cond(metav1.ConditionStatus_CONDITION_STATUS_UNKNOWN, "NodeNotReporting", "node "+name+" is not reporting")
	}
	// m is in line with its owner, so asked to stop too. The node
	// observes a machine's generation only once it has applied it, and
	// applying a stop is the VM being down.
	if m.GetStatus().GetObservedGeneration() == m.GetMetadata().GetGeneration() {
		return cond(metav1.ConditionStatus_CONDITION_STATUS_TRUE, computeapi.ReasonStopped, "")
	}
	return cond(metav1.ConditionStatus_CONDITION_STATUS_FALSE, "Stopping", "the node has not yet stopped the machine")
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
// memory and disk. The disks m lists that live on no node yet are bound to
// it first, and the IPv6 addresses of the node's prefix are given with it, so
// the machine starts with them. Without a fit it returns no machine and
// says why.
func (s *Server) place(ctx context.Context, m *computev1alpha1.Machine) (*computev1alpha1.Machine, string, string, error) {
	s.ipam.Lock()
	defer s.ipam.Unlock()
	s.capacity.Lock()
	defer s.capacity.Unlock()
	disksOn, unbound, why, err := s.machineDisks(ctx, m)
	if err != nil || why != "" {
		return nil, "", why, err
	}
	var extra uint64
	for _, d := range unbound {
		b, err := diskBytes(d.GetSpec())
		if err != nil {
			return nil, "", "", fmt.Errorf("disk %s: %w", d.GetMetadata().GetName(), err)
		}
		extra = add(extra, wholeGi(b)*quantity.GiB)
	}
	node, why, err := s.pickNode(ctx, m, disksOn, extra)
	if err != nil || node == "" {
		return nil, "", why, err
	}
	// A disk bound to a node the machine then fails to be written to only
	// sends the machine there next time.
	if err := s.bindDisks(ctx, node, unbound); err != nil {
		return nil, "", "", err
	}
	placed := proto.Clone(m).(*computev1alpha1.Machine)
	placed.Spec.Node = node
	// An Address still moving from another machine follows once placed.
	if err := s.assignIPv6(ctx, ownerOfMachine(m), m.GetSpec(), placed.GetSpec()); err != nil && !errors.Is(err, errAddressMoving) {
		return nil, "", "", err
	}
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

// nodeUsage returns every node and what machines and disks hold on each. A
// machine or disk still being deleted holds its share until released. Disks
// are sparse, so a machine holds the root disk size it declares, and a disk
// the size it has, not what was written to them.
func (s *Server) nodeUsage(ctx context.Context, except resource.Key) ([]*computev1alpha1.Node, map[string]held, error) {
	nodeObjs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Node.Kind})
	if err != nil {
		return nil, nil, err
	}
	machineObjs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind})
	if err != nil {
		return nil, nil, err
	}
	used := map[string]held{}
	for _, o := range machineObjs {
		if o.Project == except.Project && o.Name == except.Name {
			continue
		}
		m, ok := decodeMachine(o)
		if !ok {
			return nil, nil, fmt.Errorf("decode machine %s", o.Name)
		}
		n := m.GetSpec().GetNode()
		if n == "" {
			continue
		}
		h, err := heldBy(m.GetSpec().GetResources())
		if err != nil {
			return nil, nil, fmt.Errorf("machine %s/%s: %w", o.Project, o.Name, err)
		}
		u := used[n]
		u.memory = add(u.memory, h.memory)
		u.disk = add(u.disk, h.disk)
		used[n] = u
	}
	diskObjs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Disk.Kind})
	if err != nil {
		return nil, nil, err
	}
	for _, o := range diskObjs {
		d, ok := decodeDisk(o)
		if !ok {
			return nil, nil, fmt.Errorf("decode disk %s/%s", o.Project, o.Name)
		}
		n := d.GetStatus().GetNode()
		if n == "" {
			continue
		}
		size, err := diskHeld(d)
		if err != nil {
			return nil, nil, err
		}
		u := used[n]
		u.disk = add(u.disk, size)
		used[n] = u
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

// resizeFits says whether a machine placed on node can grow from was to now:
// each amount that grows must fit in what the node has left besides this
// machine. A node that reports no disk is not checked for disk.
func (s *Server) resizeFits(ctx context.Context, node string, machine resource.Key, was, now held) error {
	nodes, used, err := s.nodeUsage(ctx, machine)
	if err != nil {
		return err
	}
	var room nodeRoom
	for _, n := range nodes {
		if n.GetMetadata().GetName() == node {
			if room, err = roomOf(n); err != nil {
				// Retrying cannot help until the node reports again, which
				// brings the container back here.
				return &blocked{"NodeSizeUnreadable", err.Error() + "; the change is not applied"}
			}
		}
	}
	if mem := free(room.memory, used[node].memory); now.memory > was.memory && now.memory > mem {
		return &blocked{"ResizeDoesNotFit", fmt.Sprintf(
			"node %s has %s of memory free for this machine, not %s; the change is not applied",
			node, quantity.FormatMemory(mem), quantity.FormatMemory(now.memory))}
	}
	if disk := free(room.disk, used[node].disk); room.hasDisk && now.disk > was.disk && now.disk > disk {
		return &blocked{"ResizeDoesNotFit", fmt.Sprintf(
			"node %s has %s of disk free for this machine, not %s; the change is not applied",
			node, quantity.FormatDisk(disk), quantity.FormatDisk(now.disk))}
	}
	return nil
}

// pickNode chooses the ready, schedulable node with the most free memory that
// fits the machine's memory and disk, and extra bytes of disk besides. Memory
// and disk cannot be overcommitted; vCPUs are shared. If only is not empty,
// the node of that name is the only one considered. With no fit, it says why.
func (s *Server) pickNode(ctx context.Context, m *computev1alpha1.Machine, only string, extra uint64) (string, string, error) {
	nodes, used, err := s.nodeUsage(ctx, resource.Key{})
	if err != nil {
		return "", "", err
	}
	need, err := heldBy(m.GetSpec().GetResources())
	if err != nil {
		return "", "", fmt.Errorf("machine %s: %w", m.GetMetadata().GetName(), err)
	}
	need.disk = add(need.disk, extra)
	if only != "" {
		nodes = slices.DeleteFunc(nodes, func(n *computev1alpha1.Node) bool { return n.GetMetadata().GetName() != only })
		if len(nodes) == 0 {
			return "", fmt.Sprintf("its disks live on node %s, which is not registered", only), nil
		}
	}
	type candidate struct {
		name string
		free uint64
	}
	var fits []candidate
	ready, schedulable, memoryFits, diskFits := 0, 0, 0, 0
	var unreadable []string
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
		room, err := roomOf(n)
		if err != nil {
			unreadable = append(unreadable, err.Error())
			continue
		}
		mem := free(room.memory, used[name].memory)
		memoryFit := mem >= need.memory
		diskFit := !room.hasDisk || free(room.disk, used[name].disk) >= need.disk
		if memoryFit {
			memoryFits++
		}
		if diskFit {
			diskFits++
		}
		if memoryFit && diskFit {
			fits = append(fits, candidate{name, mem})
		}
	}
	if len(fits) == 0 {
		mem, disk := quantity.FormatMemory(need.memory), quantity.FormatDisk(need.disk)
		switch {
		case len(nodes) == 0:
			return "", "no nodes are registered", nil
		case ready == 0:
			return "", "no node is ready", nil
		case schedulable == 0:
			return "", "every ready node is cordoned", nil
		case len(unreadable) == schedulable:
			return "", "no ready node reports sizes that can be read: " + strings.Join(unreadable, "; "), nil
		}
		var why string
		switch {
		case memoryFits == 0 && diskFits == 0:
			why = fmt.Sprintf("no ready node has %s of memory or %s of disk free", mem, disk)
		case memoryFits == 0:
			why = fmt.Sprintf("no ready node has %s of memory free", mem)
		case diskFits == 0:
			why = fmt.Sprintf("no ready node has %s of disk free", disk)
		default:
			why = fmt.Sprintf("no ready node has both %s of memory and %s of disk free", mem, disk)
		}
		if len(unreadable) > 0 {
			why += "; not counted, as its sizes cannot be read: " + strings.Join(unreadable, "; ")
		}
		return "", why, nil
	}
	sort.Slice(fits, func(i, j int) bool {
		if fits[i].free != fits[j].free {
			return fits[i].free > fits[j].free
		}
		return fits[i].name < fits[j].name
	})
	return fits[0].name, "", nil
}
