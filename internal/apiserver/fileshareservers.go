package apiserver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// A FileShare's conditions. The apiserver writes FileShareAllocated and
// FileShareServerReady; the share's node writes every other type, such as
// FileShareReleased, and nothing else of its status (fileShareFromNode).
const (
	// FileShareAllocated says whether the share's node has room for its
	// size: False while it waits for its server to be placed, or for room to
	// grow.
	FileShareAllocated = "Allocated"
	// FileShareServerReady says whether the share's server runs its current
	// spec on a node that is reporting, which is the share being served.
	FileShareServerReady = "ServerReady"
	// FileShareReleased, described in computeapi, is what a deleted share's
	// NodeCleanupFinalizer waits for.
	FileShareReleased = computeapi.FileShareReleased
)

// The size of every share's server, the platform's (file-shares.md).
const (
	shareServerCPU    = 1
	shareServerMemory = 256 * quantity.MiB
)

// shareServerName is the machine that serves a share.
func shareServerName(share string) string { return prefixedName("fs-", share) }

// shareServerSize is the size of a share's volume its server is booted
// with: what the share has on its node, or, before it has a node, what it
// asks for.
func shareServerSize(fs *storagev1alpha1.FileShare) string {
	if have := fs.GetStatus().GetSize(); have != "" {
		if b, err := quantity.Bytes(have); err == nil {
			return quantity.FormatDisk(wholeGi(b) * quantity.GiB)
		}
	}
	b, _ := shareBytes(fs.GetSpec()) // admission checked it
	return quantity.FormatDisk(wholeGi(b) * quantity.GiB)
}

// shareServerSpec is the spec a share asks its server to have, given
// current, the server's spec as stored (nil before it has one); the node is
// left for the scheduler. Each network keeps the interface current gives
// it, and with it its address, which the share's clients on that network
// mount; a network added takes the lowest interface name free.
func shareServerSpec(fs *storagev1alpha1.FileShare, current *computev1alpha1.MachineSpec) *computev1alpha1.MachineSpec {
	networks := shareNetworks(fs.GetSpec())
	had := map[string]string{}
	taken := map[string]bool{}
	for _, nic := range current.GetInterfaces() {
		had[nic.GetNetwork()] = nic.GetName()
		if slices.Contains(networks, nic.GetNetwork()) {
			taken[nic.GetName()] = true
		}
	}
	nics := make([]*computev1alpha1.MachineInterface, 0, len(networks))
	next := 0
	for _, n := range networks {
		name, ok := had[n]
		if !ok {
			for taken[fmt.Sprintf("eth%d", next)] {
				next++
			}
			name = fmt.Sprintf("eth%d", next)
			taken[name] = true
		}
		nics = append(nics, &computev1alpha1.MachineInterface{Name: name, Network: n})
	}
	return &computev1alpha1.MachineSpec{
		Resources:     &computev1alpha1.Resources{Cpu: quantity.FormatCPU(shareServerCPU), Memory: quantity.FormatMemory(shareServerMemory)},
		RunState:      computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING,
		RestartPolicy: computev1alpha1.RestartPolicy_RESTART_POLICY_ALWAYS,
		Boot: &computev1alpha1.MachineSpec_FileShareServer{FileShareServer: &computev1alpha1.FileShareServerBoot{
			FileShare: fs.GetMetadata().GetName(), Size: shareServerSize(fs),
		}},
		Interfaces: nics,
	}
}

// shareFinalizers are what a share is created with (hooksFor). A share
// stored before it had them gets them before it gets a server.
var shareFinalizers = []string{dependentsFinalizer, NodeCleanupFinalizer}

// reconcileFileShare keeps a share's server in line with the share and
// reports the share's status from what is stored: its server, the machines
// declaring it, and its node's room. A deleted share is reported still, as
// its server goes, and let go once its node has removed its volume;
// dependentsFinalizer keeps it besides until its server is gone.
func (s *Server) reconcileFileShare(ctx context.Context, k controller.Key) error {
	key := resource.Key{Project: k.Project, Name: k.Name}
	obj, err := s.get(ctx, registry.FileShare, key)
	if err != nil || obj == nil {
		return err
	}
	fs := obj.(*storagev1alpha1.FileShare)
	if fs.GetMetadata().GetDeleteTime() != nil {
		return s.reportDeletedShare(ctx, key, fs)
	}
	if missing := slices.DeleteFunc(slices.Clone(shareFinalizers), func(f string) bool {
		return slices.Contains(fs.GetMetadata().GetFinalizers(), f)
	}); len(missing) > 0 {
		updated := proto.Clone(fs).(*storagev1alpha1.FileShare)
		updated.Metadata.Finalizers = append(updated.Metadata.Finalizers, missing...)
		_, err := s.backend(registry.FileShare).Apply(s.system(ctx), updated)
		return err
	}
	name := shareServerName(k.Name)
	mobj, err := s.get(ctx, registry.Machine, resource.Key{Project: k.Project, Name: name})
	if err != nil {
		return err
	}
	m, _ := mobj.(*computev1alpha1.Machine)
	m, err = s.ensureMachineOf(ctx, registry.FileShare, fs.GetMetadata(), name, shareServerSpec(fs, m.GetSpec()), m)
	var stuck *blocked
	var waiting error
	if errors.As(err, &stuck) {
		waiting = stillWaiting(err)
	} else if waiting, err = splitWaiting(err); err != nil {
		return err
	}
	node, err := s.nodeOf(ctx, m)
	if err != nil {
		return err
	}
	users, err := s.shareUsers(ctx, k.Project, k.Name)
	if err != nil {
		return err
	}
	return cmp.Or(s.reportFileShare(ctx, key, fs, m, node, users, stuck), waiting)
}

// reportDeletedShare reports a deleted share as its server goes, and lets
// it go once its node, if it has one, has released it.
func (s *Server) reportDeletedShare(ctx context.Context, key resource.Key, fs *storagev1alpha1.FileShare) error {
	mobj, err := s.get(ctx, registry.Machine, resource.Key{Project: key.Project, Name: shareServerName(key.Name)})
	if err != nil {
		return err
	}
	m, _ := mobj.(*computev1alpha1.Machine)
	if m != nil && !ownedBy(m.GetMetadata(), fs.GetMetadata().GetUid()) {
		m = nil
	}
	node, err := s.nodeOf(ctx, m)
	if err != nil {
		return err
	}
	users, err := s.shareUsers(ctx, key.Project, key.Name)
	if err != nil {
		return err
	}
	var gone *blocked
	if m == nil {
		gone = &blocked{"Deleted", "the share is deleted, and its server is gone"}
	}
	if err := s.reportFileShare(ctx, key, fs, m, node, users, gone); err != nil {
		return err
	}
	if fs.GetStatus().GetNode() == "" || conditions.IsTrue(fs.GetStatus().GetConditions(), FileShareReleased) {
		return s.RemoveFinalizer(ctx, registry.FileShare, key, NodeCleanupFinalizer)
	}
	return nil
}

// shareUsers are the machines, not being deleted, that declare a share, as
// their specs have it.
func (s *Server) shareUsers(ctx context.Context, project, share string) ([]*storagev1alpha1.FileShareUser, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	var users []*storagev1alpha1.FileShareUser
	for _, o := range objs {
		m, ok := decodeMachine(o)
		if !ok {
			return nil, fmt.Errorf("decode machine %s/%s", o.Project, o.Name)
		}
		f := declaredShare(m.GetSpec(), share)
		if f == nil || m.GetMetadata().GetDeleteTime() != nil {
			continue
		}
		u := &storagev1alpha1.FileShareUser{
			Kind: registry.Machine.Kind, Name: o.Name, Machine: o.Name, Network: f.GetNetwork(), ReadOnly: f.GetReadOnly(),
		}
		for _, ref := range m.GetMetadata().GetOwnerReferences() {
			if ref.GetController() {
				u.Kind, u.Name = ref.GetKind(), ref.GetName()
			}
		}
		for _, v := range m.GetSpec().GetContainer().GetVolumes() {
			if v.GetFileShare() == share {
				u.Path = v.GetTarget()
			}
		}
		users = append(users, u)
	}
	slices.SortFunc(users, func(a, b *storagev1alpha1.FileShareUser) int { return strings.Compare(a.GetMachine(), b.GetMachine()) })
	return users, nil
}

// shareAddresses are the addresses of m, a share's server, on each of its
// networks, in its interfaces' order; none if m is not the share's.
func shareAddresses(fs *storagev1alpha1.FileShare, m *computev1alpha1.Machine) []*storagev1alpha1.FileShareAddress {
	if m == nil || !ownedBy(m.GetMetadata(), fs.GetMetadata().GetUid()) {
		return nil
	}
	var out []*storagev1alpha1.FileShareAddress
	for _, nic := range m.GetSpec().GetInterfaces() {
		if p, err := netip.ParsePrefix(nic.GetIpv4()); err == nil {
			out = append(out, &storagev1alpha1.FileShareAddress{Network: nic.GetNetwork(), Address: p.Addr().String()})
		}
	}
	return out
}

// reportFileShare writes the apiserver's part of a share's status,
// conditional on fs as read, and nothing if nothing changed: its server m,
// on node, or why it has none as asked (stuck), and its users. A growth is
// granted under s.capacity, as one decision with the node's other
// reservations. Its node and the conditions its node writes stay as read.
func (s *Server) reportFileShare(ctx context.Context, key resource.Key, fs *storagev1alpha1.FileShare, m *computev1alpha1.Machine,
	node *computev1alpha1.Node, users []*storagev1alpha1.FileShareUser, stuck *blocked) error {
	gen := fs.GetMetadata().GetGeneration()
	status := &storagev1alpha1.FileShareStatus{}
	if fs.GetStatus() != nil {
		status = proto.Clone(fs.GetStatus()).(*storagev1alpha1.FileShareStatus)
	}
	status.ObservedGeneration = gen
	status.Users = users
	status.Addresses = shareAddresses(fs, m)
	allocated := &metav1.Condition{Type: FileShareAllocated, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Allocated"}
	if on := status.GetNode(); on == "" {
		allocated = &metav1.Condition{
			Type: FileShareAllocated, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "WaitingForServer",
			Message: "the share is made on the node its server is placed on",
		}
	} else {
		want, err := shareBytes(fs.GetSpec())
		if err != nil {
			return err
		}
		have, err := optionalBytes(status.GetSize())
		if err != nil {
			return fmt.Errorf("file share %s/%s status.size: %w", key.Project, key.Name, err)
		}
		if wholeGi(want) > wholeGi(have) {
			s.capacity.Lock()
			defer s.capacity.Unlock()
			err := s.growthFits(ctx, "the file share", on, have, want)
			var full *blocked
			switch {
			case errors.As(err, &full):
				allocated = &metav1.Condition{
					Type: FileShareAllocated, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: full.reason, Message: full.message,
				}
			case err != nil:
				return err
			default:
				status.Size = quantity.FormatDisk(want)
			}
		}
	}
	var ready *metav1.Condition
	switch {
	case stuck != nil:
		ready = &metav1.Condition{Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: stuck.reason, Message: stuck.message}
	case m == nil:
		ready = &metav1.Condition{Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "Pending", Message: "the share has no server yet"}
	default:
		ready = machineReadiness(m, node)
	}
	ready.Type = FileShareServerReady
	allocated.ObservedGeneration, ready.ObservedGeneration = gen, gen
	status.Conditions = conditions.Set(status.GetConditions(), allocated, s.clock.Now())
	status.Conditions = conditions.Set(status.GetConditions(), ready, s.clock.Now())
	switch {
	case ready.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE:
		status.Phase = storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_PENDING
	case len(users) > 0:
		status.Phase = storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_IN_USE
	default:
		status.Phase = storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE
	}
	if proto.Equal(status, fs.GetStatus()) {
		return nil
	}
	rv := fs.GetMetadata().GetResourceVersion()
	_, err := s.backend(registry.FileShare).UpdateStatus(s.system(ctx), key, status, &rv)
	return err
}

func shareCond(fs *storagev1alpha1.FileShare, typ string) *metav1.Condition {
	return conditions.Find(fs.GetStatus().GetConditions(), typ)
}

// fileShareFromNode is what a share's status becomes when its node writes
// reported over stored: the node's own conditions, and stored's everything
// else, which the apiserver derives from what is stored and never takes
// from a node.
func fileShareFromNode(stored, reported proto.Message) proto.Message {
	out := &storagev1alpha1.FileShareStatus{}
	if st, ok := stored.(*storagev1alpha1.FileShareStatus); ok && st != nil {
		out = proto.Clone(st).(*storagev1alpha1.FileShareStatus)
	}
	ours := func(c *metav1.Condition) bool {
		return c.GetType() == FileShareAllocated || c.GetType() == FileShareServerReady
	}
	conds := slices.DeleteFunc(out.GetConditions(), func(c *metav1.Condition) bool { return !ours(c) })
	for _, c := range reported.(*storagev1alpha1.FileShareStatus).GetConditions() {
		if !ours(c) {
			conds = append(conds, proto.Clone(c).(*metav1.Condition))
		}
	}
	out.Conditions = conds
	return out
}

// serverPlacement reads where m, a share's server, must go: the share's
// node once it has one, and before that the node most of the placed
// machines declaring it are on, so they can mount it; anywhere if none is
// placed. unbound is the share if it has no node yet, to be bound to the
// one m goes to. why, if not empty, says what keeps m from being placed.
func (s *Server) serverPlacement(ctx context.Context, m *computev1alpha1.Machine) (node string, unbound *storagev1alpha1.FileShare, why string, err error) {
	project := m.GetMetadata().GetProject()
	name := m.GetSpec().GetFileShareServer().GetFileShare()
	obj, err := s.get(ctx, registry.FileShare, resource.Key{Project: project, Name: name})
	if err != nil {
		return "", nil, "", err
	}
	fs, ok := obj.(*storagev1alpha1.FileShare)
	switch {
	case !ok:
		return "", nil, fmt.Sprintf("file share %s does not exist", name), nil
	case !ownedBy(m.GetMetadata(), fs.GetMetadata().GetUid()):
		return "", nil, fmt.Sprintf("the machine is not file share %s's server", name), nil
	case fs.GetMetadata().GetDeleteTime() != nil:
		return "", nil, fmt.Sprintf("file share %s is being deleted", name), nil
	case fs.GetStatus().GetNode() != "":
		return fs.GetStatus().GetNode(), nil, "", nil
	}
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind, Project: project})
	if err != nil {
		return "", nil, "", err
	}
	count := map[string]int{}
	for _, o := range objs {
		d, ok := decodeMachine(o)
		if !ok {
			return "", nil, "", fmt.Errorf("decode machine %s/%s", o.Project, o.Name)
		}
		if on := d.GetSpec().GetNode(); on != "" && d.GetMetadata().GetDeleteTime() == nil && declaresShare(d.GetSpec(), name) {
			count[on]++
		}
	}
	for n, c := range count {
		if node == "" || c > count[node] || c == count[node] && n < node {
			node = n
		}
	}
	return node, fs, "", nil
}

// bindShare binds a share to node at its spec.size, conditional on the share
// as read. The caller holds s.capacity, having found the room.
func (s *Server) bindShare(ctx context.Context, node string, fs *storagev1alpha1.FileShare) error {
	size, err := shareBytes(fs.GetSpec())
	if err != nil {
		return err
	}
	status := &storagev1alpha1.FileShareStatus{}
	if fs.GetStatus() != nil {
		status = proto.Clone(fs.GetStatus()).(*storagev1alpha1.FileShareStatus)
	}
	status.Node = node
	status.Size = quantity.FormatDisk(wholeGi(size) * quantity.GiB)
	rv := fs.GetMetadata().GetResourceVersion()
	key := resource.Key{Project: fs.GetMetadata().GetProject(), Name: fs.GetMetadata().GetName()}
	if _, err := s.backend(registry.FileShare).UpdateStatus(s.system(ctx), key, status, &rv); err != nil {
		return fmt.Errorf("binding file share %s to node %s: %w", key.Name, node, err)
	}
	return nil
}

// sharesOfMachine maps a machine to the shares whose status it changes: the
// one it serves, those it declares, and those whose users it is in, which
// it may no longer declare; to the shares of its project whose server was
// told a network is full, as it going frees an address; and, when it sits
// on a node, to every share waiting for room to grow, as it shrinking or
// going may make some.
func (s *Server) sharesOfMachine(ctx context.Context, o store.Object) []controller.Key {
	m, ok := decodeMachine(o)
	if !ok {
		return nil
	}
	keys := ownerOf(o.Project, m, registry.FileShare)
	for _, f := range m.GetSpec().GetFileShares() {
		keys = append(keys, controller.Key{Project: o.Project, Name: f.GetFileShare()})
	}
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.FileShare.Kind, Project: o.Project})
	if err != nil {
		s.log.Error("listing file shares failed", "error", err)
		return keys
	}
	for _, so := range objs {
		fs, ok := decodeFileShare(so)
		if !ok {
			continue
		}
		if shareCond(fs, FileShareServerReady).GetReason() == ReasonNoAddressLeft || slices.ContainsFunc(fs.GetStatus().GetUsers(), func(u *storagev1alpha1.FileShareUser) bool {
			return u.GetMachine() == o.Name
		}) {
			keys = append(keys, controller.Key{Project: so.Project, Name: so.Name})
		}
	}
	if m.GetSpec().GetNode() != "" {
		keys = append(keys, s.sharesWaitingForRoom(ctx, o)...)
	}
	return keys
}

// sharesWaitingForRoom maps to every share whose growth did not fit its
// node.
func (s *Server) sharesWaitingForRoom(ctx context.Context, _ store.Object) []controller.Key {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.FileShare.Kind})
	if err != nil {
		s.log.Error("listing file shares failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, o := range objs {
		fs, ok := decodeFileShare(o)
		if !ok {
			continue
		}
		if fs.GetStatus().GetNode() != "" && shareCond(fs, FileShareAllocated).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE {
			keys = append(keys, controller.Key{Project: o.Project, Name: o.Name})
		}
	}
	return keys
}

// shareAndWaiting maps a share to itself and, when it sits on a node, to
// every share waiting for room: it shrinking or going may make some.
func (s *Server) shareAndWaiting(ctx context.Context, o store.Object) []controller.Key {
	keys := controller.Self(ctx, o)
	if fs, ok := decodeFileShare(o); ok && fs.GetStatus().GetNode() != "" {
		keys = append(keys, s.sharesWaitingForRoom(ctx, o)...)
	}
	return keys
}

// sharesOnNode maps a node to the shares on it, whose servers it runs: a
// node going quiet, or making room, changes them.
func (s *Server) sharesOnNode(ctx context.Context, n store.Object) []controller.Key {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.FileShare.Kind})
	if err != nil {
		s.log.Error("listing file shares failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, o := range objs {
		if fs, ok := decodeFileShare(o); ok && fs.GetStatus().GetNode() == n.Name {
			keys = append(keys, controller.Key{Project: o.Project, Name: o.Name})
		}
	}
	return keys
}

// sharesInProject maps an object to every share in its project: a network
// appearing gives a server waiting for it its address.
func (s *Server) sharesInProject(ctx context.Context, o store.Object) []controller.Key {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.FileShare.Kind, Project: o.Project})
	if err != nil {
		s.log.Error("listing file shares failed", "error", err)
		return nil
	}
	return keysOf(objs)
}
