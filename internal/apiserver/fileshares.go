package apiserver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// MaxShareNetworks bounds the networks a file share is on.
const MaxShareNetworks = 8

// MaxFileShares bounds the file shares a machine declares.
const MaxFileShares = 8

// sourceKindFileShare is a container volume's source_kind for a FileShare.
const sourceKindFileShare = "FileShare"

func decodeFileShare(o store.Object) (*storagev1alpha1.FileShare, bool) {
	var fs storagev1alpha1.FileShare
	return &fs, proto.Unmarshal(o.Data, &fs) == nil
}

// shareNetworks are the networks a share is on: those its spec names, or
// the default network when it names none.
func shareNetworks(spec *storagev1alpha1.FileShareSpec) []string {
	if len(spec.GetNetworks()) == 0 {
		return []string{DefaultNetwork}
	}
	return spec.GetNetworks()
}

// shareBytes reads a share's spec.size.
func shareBytes(spec *storagev1alpha1.FileShareSpec) (uint64, error) {
	if spec.GetSize() == "" {
		return 0, errors.New("spec.size is required, such as 10Gi")
	}
	b, err := quantity.Bytes(spec.GetSize())
	if err != nil {
		return 0, fmt.Errorf("spec.size: %w", err)
	}
	return b, nil
}

// shareGi is what a share holds, in whole Gi: the larger of the size it asks
// for and the size it has been made with, as a disk's (diskGi).
func shareGi(fs *storagev1alpha1.FileShare) uint64 {
	var gi uint64
	if b, err := shareBytes(fs.GetSpec()); err == nil {
		gi = wholeGi(b)
	}
	if st := fs.GetStatus().GetSize(); st != "" {
		if b, err := quantity.Bytes(st); err == nil {
			gi = max(gi, wholeGi(b))
		}
	}
	return gi
}

// validateFileShare checks a share's networks, size and squashing. A size
// may not go below what the share has on its node, as a disk's
// (validateDisk). A network may not be added while it is being deleted,
// nor removed while what declares the share mounts from it, and what
// declares it must reach it on one of its networks: one declaring it
// before it is made, or before its networks change, included. The caller
// holds s.attach, which every write of networks and of what declares a
// share holds too.
func validateFileShare(ctx context.Context, s *Server, _ auth.Principal, old, obj proto.Message) error {
	fs := obj.(*storagev1alpha1.FileShare)
	spec := fs.GetSpec()
	if n := len(spec.GetNetworks()); n > MaxShareNetworks {
		return invalid("spec.networks: %d networks, at most %d", n, MaxShareNetworks)
	}
	seen := map[string]bool{}
	for i, n := range spec.GetNetworks() {
		if err := ValidName(n); err != nil {
			return invalid("spec.networks[%d]: %v", i, err)
		}
		if seen[n] {
			return invalid("spec.networks: network %q twice", n)
		}
		seen[n] = true
	}
	size, err := shareBytes(spec)
	if err != nil {
		return invalid("%v", err)
	}
	if size < minDisk {
		return invalid("spec.size %s is below the minimum of 1Gi", spec.GetSize())
	}
	if size > maxDisk {
		return invalid("spec.size %s is above the maximum of 64Ti", spec.GetSize())
	}
	if err := validateSquash(spec); err != nil {
		return err
	}
	var was *storagev1alpha1.FileShare
	if old != nil {
		was = old.(*storagev1alpha1.FileShare)
	}
	// Only a change of size is checked against what the share has.
	if stored, err := shareBytes(was.GetSpec()); was != nil && (err != nil || wholeGi(stored) != wholeGi(size)) {
		if have := was.GetStatus().GetSize(); have != "" {
			if b, err := quantity.Bytes(have); err == nil && wholeGi(size) < wholeGi(b) {
				return invalid("spec.size %s is smaller than the current %s; a file share can grow but never shrink", spec.GetSize(), have)
			}
		}
	}
	project := fs.GetMetadata().GetProject()
	now := shareNetworks(spec)
	var before []string
	if was != nil {
		before = shareNetworks(was.GetSpec())
	}
	for _, n := range now {
		if slices.Contains(before, n) {
			continue
		}
		obj, err := s.get(ctx, registry.Network, resource.Key{Project: project, Name: n})
		if err != nil {
			return err
		}
		if net, ok := obj.(*networkv1alpha1.Network); ok && net.GetMetadata().GetDeleteTime() != nil {
			return failedPrecondition("network %q is being deleted", n)
		}
	}
	name := fs.GetMetadata().GetName()
	var added []string
	for _, n := range now {
		if !slices.Contains(before, n) {
			added = append(added, n)
		}
	}
	if err := s.checkShareRanges(ctx, project, name, now, added, nil); err != nil {
		return invalid("%v", err)
	}
	declarers, err := s.declarersOf(ctx, project, name)
	if err != nil {
		return err
	}
	for _, n := range before {
		if slices.Contains(now, n) {
			continue
		}
		var from []string
		for _, d := range declarers {
			if !d.viaMachine && d.mountsFrom(before) == n {
				from = append(from, d.String())
			}
		}
		if len(from) > 0 {
			return failedPrecondition("file share %q cannot leave network %q: it is mounted from there by %s",
				name, n, strings.Join(from, ", "))
		}
	}
	var cut []string
	for _, d := range declarers {
		if !d.deleting && !d.viaMachine && d.mountsFrom(now) == "" {
			cut = append(cut, fmt.Sprintf("%s (on network %s)", d, strings.Join(d.networks, ", ")))
		}
	}
	if len(cut) > 0 {
		return failedPrecondition("file share %q on network %s could not be reached by %s, which declare it; put it on one of their networks",
			name, strings.Join(now, ", "), strings.Join(cut, ", "))
	}
	return nil
}

// networkRange is the IPv4 range of a project's network, ok false for one
// not made yet; default's is always DefaultNetworkCIDR, made or not.
func (s *Server) networkRange(ctx context.Context, project, name string) (netip.Prefix, bool, error) {
	if name == DefaultNetwork {
		return netip.MustParsePrefix(DefaultNetworkCIDR), true, nil
	}
	obj, err := s.get(ctx, registry.Network, resource.Key{Project: project, Name: name})
	if err != nil || obj == nil {
		return netip.Prefix{}, false, err
	}
	p, err := netip.ParsePrefix(obj.(*networkv1alpha1.Network).GetSpec().GetIpv4Cidr())
	return p, err == nil, nil
}

// checkShareRanges refuses share on networks two of which, one of them
// among added, have overlapping ranges: its server's export knows a client
// by its address alone, so it could not tell apart two machines of one
// address on the two networks, nor take one's access away alone
// (docs/decisions/file-shares.md). known gives ranges not stored yet, as a
// network being made has. A network not made yet is checked when it is.
func (s *Server) checkShareRanges(ctx context.Context, project, share string, networks, added []string, known map[string]netip.Prefix) error {
	if len(added) == 0 {
		return nil
	}
	ranges := map[string]netip.Prefix{}
	for _, n := range networks {
		if p, ok := known[n]; ok {
			ranges[n] = p
			continue
		}
		p, ok, err := s.networkRange(ctx, project, n)
		if err != nil {
			return err
		}
		if ok {
			ranges[n] = p
		}
	}
	for _, a := range added {
		pa, ok := ranges[a]
		if !ok {
			continue
		}
		for _, b := range networks {
			if pb, ok := ranges[b]; ok && b != a && pa.Overlaps(pb) {
				return fmt.Errorf("file share %q cannot be on networks %s (%s) and %s (%s), whose ranges overlap: "+
					"its server knows a client by its address alone, and could not tell their machines apart", share, b, pb, a, pa)
			}
		}
	}
	return nil
}

// checkRangeOfShares refuses a network being made, of range p, that a share
// naming it is on with another network whose range overlaps p
// (checkShareRanges). The caller holds s.attach, which every share write
// holds too.
func (s *Server) checkRangeOfShares(ctx context.Context, project, network string, p netip.Prefix) error {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.FileShare.Kind, Project: project})
	if err != nil {
		return err
	}
	for _, o := range objs {
		fs, ok := decodeFileShare(o)
		if !ok {
			return fmt.Errorf("decode file share %s/%s", o.Project, o.Name)
		}
		nets := shareNetworks(fs.GetSpec())
		if fs.GetMetadata().GetDeleteTime() != nil || !slices.Contains(nets, network) {
			continue
		}
		if err := s.checkShareRanges(ctx, project, o.Name, nets, []string{network}, map[string]netip.Prefix{network: p}); err != nil {
			return failedPrecondition("network %q: %v", network, err)
		}
	}
	return nil
}

// noUID is (uid_t)-1, which no user is.
const noUID = math.MaxUint32

// validateSquash checks a share's squashing: a known mode, and the
// anonymous user and group only where a mode squashes.
func validateSquash(spec *storagev1alpha1.FileShareSpec) error {
	switch spec.GetSquash() {
	case storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_UNSPECIFIED, storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_NONE:
		if spec.AnonUid != nil || spec.AnonGid != nil {
			return invalid("spec.anon_uid and spec.anon_gid are set only with spec.squash ROOT or ALL")
		}
	case storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_ROOT, storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_ALL:
	default:
		return invalid("spec.squash %v is not NONE, ROOT or ALL", spec.GetSquash())
	}
	if spec.AnonUid != nil && spec.GetAnonUid() == noUID {
		return invalid("spec.anon_uid %d is no user", spec.GetAnonUid())
	}
	if spec.AnonGid != nil && spec.GetAnonGid() == noUID {
		return invalid("spec.anon_gid %d is no group", spec.GetAnonGid())
	}
	return nil
}

// declarer is an object that declares a share: a Container, a
// VirtualMachine, or a Machine, which may exist before, or without, the
// machine it is made into.
type declarer struct {
	// kind is in words, such as "virtual machine".
	kind, name string
	// networks are its interfaces' networks, in order, as its machine has
	// them.
	networks []string
	// network is the one a machine mounts the share from, as it stores it.
	network  string
	deleting bool
	// viaMachine says a container or VM has a machine declaring the share,
	// whose stored network is the one it mounts from.
	viaMachine bool
}

func (d declarer) String() string {
	return d.kind + " " + d.name
}

// mountsFrom is the network d mounts the share from while the share is on
// shareNets: a machine's stored one, if it is among them, or else the first
// of d's networks the share is on; empty if none is.
func (d declarer) mountsFrom(shareNets []string) string {
	if d.network != "" {
		if slices.Contains(shareNets, d.network) {
			return d.network
		}
		return ""
	}
	return mountNetwork(d.networks, shareNets)
}

// declarersOf returns the containers, virtual machines and machines, live
// or being deleted, that declare the share.
func (s *Server) declarersOf(ctx context.Context, project, share string) ([]declarer, error) {
	var out []declarer
	// The uids of the objects whose machines declare the share.
	viaMachine := map[string]bool{}
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		m, ok := decodeMachine(o)
		if !ok {
			return nil, fmt.Errorf("decode machine %s/%s", o.Project, o.Name)
		}
		f := declaredShare(m.GetSpec(), share)
		if f == nil {
			continue
		}
		out = append(out, declarer{kind: "machine", name: o.Name, networks: machineNetworks(m.GetSpec()), network: f.GetNetwork(),
			deleting: m.GetMetadata().GetDeleteTime() != nil})
		for _, r := range m.GetMetadata().GetOwnerReferences() {
			if r.GetController() {
				viaMachine[r.GetUid()] = true
			}
		}
	}
	objs, _, err = s.store.List(ctx, store.ListOptions{Kind: registry.Container.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		obj, err := s.backend(registry.Container).decode(o)
		if err != nil {
			return nil, err
		}
		c := obj.(*computev1alpha1.Container)
		if slices.Contains(containerShares(c.GetSpec().GetVolumes()), share) {
			out = append(out, declarer{kind: "container", name: o.Name, networks: interfaceNetworks(c.GetSpec().GetInterfaces()),
				deleting: c.GetMetadata().GetDeleteTime() != nil, viaMachine: viaMachine[c.GetMetadata().GetUid()]})
		}
	}
	objs, _, err = s.store.List(ctx, store.ListOptions{Kind: registry.VirtualMachine.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		vm, ok := decodeVM(o)
		if !ok {
			return nil, fmt.Errorf("decode virtual machine %s/%s", o.Project, o.Name)
		}
		if slices.Contains(vmShares(vm.GetSpec()), share) {
			out = append(out, declarer{kind: "virtual machine", name: o.Name, networks: interfaceNetworks(vm.GetSpec().GetInterfaces()),
				deleting: vm.GetMetadata().GetDeleteTime() != nil, viaMachine: viaMachine[vm.GetMetadata().GetUid()]})
		}
	}
	return out, nil
}

// machineNetworks are the networks of a machine's interfaces, in order.
func machineNetworks(spec *computev1alpha1.MachineSpec) []string {
	var out []string
	for _, nic := range spec.GetInterfaces() {
		out = append(out, nic.GetNetwork())
	}
	return out
}

func declaresShare(spec *computev1alpha1.MachineSpec, share string) bool {
	return declaredShare(spec, share) != nil
}

// declaredShare is the entry of spec.file_shares naming share; nil if none
// does.
func declaredShare(spec *computev1alpha1.MachineSpec, share string) *computev1alpha1.MachineFileShare {
	for _, f := range spec.GetFileShares() {
		if f.GetFileShare() == share {
			return f
		}
	}
	return nil
}

// mountNetwork is the network what is on networks, in order, mounts a
// share on shareNets from: the first of them the share is on; empty if none
// is.
func mountNetwork(networks, shareNets []string) string {
	for _, n := range networks {
		if slices.Contains(shareNets, n) {
			return n
		}
	}
	return ""
}

// refuseDeletingDeclaredShare keeps users from deleting a share that a live
// container, virtual machine or machine declares. A project being deleted
// takes its shares with it.
func refuseDeletingDeclaredShare(ctx context.Context, s *Server, obj proto.Message) error {
	fs := obj.(*storagev1alpha1.FileShare)
	project, err := s.get(ctx, registry.Project, resource.Key{Name: fs.GetMetadata().GetProject()})
	if err != nil {
		return err
	}
	if project == nil || project.(*tenancyv1alpha1.Project).GetMetadata().GetDeleteTime() != nil {
		return nil
	}
	declarers, err := s.declarersOf(ctx, fs.GetMetadata().GetProject(), fs.GetMetadata().GetName())
	if err != nil {
		return err
	}
	var live []string
	for _, d := range declarers {
		if !d.deleting {
			live = append(live, d.String())
		}
	}
	if len(live) == 0 {
		return nil
	}
	return failedPrecondition("file share %q is declared by %s; stop declaring it first",
		fs.GetMetadata().GetName(), strings.Join(live, ", "))
}

// sharesOn returns the shares, not being deleted, on the network.
func (s *Server) sharesOn(ctx context.Context, project, network string) ([]string, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.FileShare.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, o := range objs {
		fs, ok := decodeFileShare(o)
		if !ok {
			return nil, fmt.Errorf("decode file share %s/%s", o.Project, o.Name)
		}
		if fs.GetMetadata().GetDeleteTime() == nil && slices.Contains(shareNetworks(fs.GetSpec()), network) {
			names = append(names, o.Name)
		}
	}
	return names, nil
}

// checkShareNames checks the names of the shares a machine, or what it is
// made from, declares: at most MaxFileShares, each valid and once. at is the
// field, such as spec.file_shares.
func checkShareNames(at string, names []string) error {
	if len(names) > MaxFileShares {
		return invalid("%s: %d file shares, at most %d", at, len(names), MaxFileShares)
	}
	seen := map[string]bool{}
	for i, name := range names {
		if err := ValidName(name); err != nil {
			return invalid("%s[%d]: %v", at, i, err)
		}
		if seen[name] {
			return invalid("%s: file share %q twice", at, name)
		}
		seen[name] = true
	}
	return nil
}

// checkShareReach refuses a share, among names, that exists and is on none
// of networks, the networks of the machine declaring it: it could not reach
// the share. A share that does not exist yet holds the machine back
// instead (checkDeclaring).
func (s *Server) checkShareReach(ctx context.Context, project, machine string, networks, names []string) error {
	for _, name := range names {
		obj, err := s.get(ctx, registry.FileShare, resource.Key{Project: project, Name: name})
		if err != nil {
			return err
		}
		fs, ok := obj.(*storagev1alpha1.FileShare)
		if !ok || fs.GetMetadata().GetDeleteTime() != nil {
			continue
		}
		on := shareNetworks(fs.GetSpec())
		if !slices.ContainsFunc(networks, func(n string) bool { return slices.Contains(on, n) }) {
			return invalid("file share %q is on network %s, and %s is on network %s; add one of its networks to the share, or give it an interface on one of the share's",
				name, strings.Join(on, ", "), machine, strings.Join(networks, ", "))
		}
	}
	return nil
}

// checkDeclaring checks the shares a machine declares: each once, and on
// one of its networks, from which it mounts it (settleNetwork); each one it
// did not declare before (old is nil on create) a live share, and, for a
// placed container's machine, one on its node. A share that does not exist
// yet holds the machine back, and so does one on no node yet for a
// container's, which would otherwise run writing to its root disk at the
// share's target, and one of another node is refused it. A VM's takes one
// on no node or another node, and reports it unmounted (fileSharesOnNode).
// The caller holds s.attach until the machine is written.
func (s *Server) checkDeclaring(ctx context.Context, meta *metav1.ObjectMeta, old, spec *computev1alpha1.MachineSpec) error {
	var names []string
	for _, f := range spec.GetFileShares() {
		names = append(names, f.GetFileShare())
	}
	if err := checkShareNames("spec.file_shares", names); err != nil {
		return err
	}
	if err := s.checkShareReach(ctx, meta.GetProject(), "machine "+meta.GetName(), machineNetworks(spec), names); err != nil {
		return err
	}
	for i, f := range spec.GetFileShares() {
		name := f.GetFileShare()
		obj, err := s.get(ctx, registry.FileShare, resource.Key{Project: meta.GetProject(), Name: name})
		if err != nil {
			return err
		}
		fs, ok := obj.(*storagev1alpha1.FileShare)
		if ok && fs.GetMetadata().GetDeleteTime() != nil {
			ok = false
		}
		was := declaredShare(old, name)
		if err := settleNetwork(i, meta.GetName(), spec, f, was, fs, ok); err != nil {
			return err
		}
		if was != nil {
			continue
		}
		on := fs.GetStatus().GetNode()
		switch {
		case obj == nil:
			return failedPrecondition("file share %q does not exist", name)
		case !ok:
			return failedPrecondition("file share %q is being deleted", name)
		case on == "" && spec.GetContainer() != nil:
			return failedPrecondition("file share %q is on no node yet; a container runs only with its file shares mounted", name)
		case spec.GetNode() == "", on == "", on == spec.GetNode():
		case spec.GetContainer() != nil:
			return invalid("file share %q lives on node %s, and machine %s runs on node %s; a machine never moves: "+
				"it is made again to take a share of another node", name, on, meta.GetName(), spec.GetNode())
		default:
			// A VM takes it, and mounts it once it is on the machine's
			// node (fileSharesOnNode).
		}
	}
	return nil
}

// settleNetwork gives f, the i-th share spec declares, the network the
// machine mounts it from: the one it had in was, the share as the machine
// declared it before, while the machine is still on it; or else one the
// writer chose, which must be the machine's and the share's; or else the
// first of the machine's networks the share, fs, is on, when live.
func settleNetwork(i int, machine string, spec *computev1alpha1.MachineSpec, f, was *computev1alpha1.MachineFileShare, fs *storagev1alpha1.FileShare, live bool) error {
	networks := machineNetworks(spec)
	if kept := was.GetNetwork(); kept != "" && slices.Contains(networks, kept) {
		if f.GetNetwork() != "" && f.GetNetwork() != kept {
			return invalid("spec.file_shares[%d].network: machine %s mounts file share %q from network %s, and keeps it while it declares the share and is on that network",
				i, machine, f.GetFileShare(), kept)
		}
		f.Network = kept
		return nil
	}
	if n := f.GetNetwork(); n != "" {
		if !slices.Contains(networks, n) {
			return invalid("spec.file_shares[%d].network %q is not one of machine %s's networks", i, n, machine)
		}
		if live && !slices.Contains(shareNetworks(fs.GetSpec()), n) {
			return invalid("spec.file_shares[%d].network %q is not one of file share %q's networks", i, n, f.GetFileShare())
		}
		return nil
	}
	if live {
		f.Network = mountNetwork(networks, shareNetworks(fs.GetSpec()))
	}
	return nil
}

// machineShares reads the shares m declares: the node those on a node live
// on, to which m must go. A share on no node yet keeps a container's machine
// from being placed, and holds a VM's to no node; a VM's goes to the first
// of its shares' nodes, which its disks' node overrides (place), and
// reports the others unmounted (fileSharesOnNode). why, if not empty, says
// what keeps m from being placed at all.
func (s *Server) machineShares(ctx context.Context, m *computev1alpha1.Machine) (node, why string, err error) {
	project := m.GetMetadata().GetProject()
	first := ""
	for _, f := range m.GetSpec().GetFileShares() {
		name := f.GetFileShare()
		obj, err := s.get(ctx, registry.FileShare, resource.Key{Project: project, Name: name})
		if err != nil {
			return "", "", err
		}
		fs, ok := obj.(*storagev1alpha1.FileShare)
		switch {
		case !ok:
			return "", fmt.Sprintf("file share %s does not exist", name), nil
		case fs.GetMetadata().GetDeleteTime() != nil:
			return "", fmt.Sprintf("file share %s is being deleted", name), nil
		}
		switch n := fs.GetStatus().GetNode(); {
		case n == "" && m.GetSpec().GetContainer() != nil:
			return "", fmt.Sprintf("file share %s is on no node yet, and a container runs only with its file shares mounted", name), nil
		case n == "":
			// It does not hold a VM to a node: the share's server is to go
			// to its declarers'.
		case node == "":
			node, first = n, name
		case n != node && m.GetSpec().GetContainer() != nil:
			return "", fmt.Sprintf("file share %s lives on node %s and file share %s on node %s", first, node, name, n), nil
		}
	}
	return node, "", nil
}

// FileSharesOnNode is the VirtualMachine condition saying whether every
// file share its machine declares is on the machine's node, which a share
// must be to be mounted. Only a VM declaring a share has it. A container
// needs none: its machine never declares a share off its node.
const FileSharesOnNode = "FileSharesOnNode"

// fileSharesOnNode is the FileSharesOnNode condition of what m is made
// from; nil when m declares no share.
func (s *Server) fileSharesOnNode(ctx context.Context, m *computev1alpha1.Machine) (*metav1.Condition, error) {
	if len(m.GetSpec().GetFileShares()) == 0 {
		return nil, nil
	}
	node := m.GetSpec().GetNode()
	if node == "" {
		return &metav1.Condition{Type: FileSharesOnNode, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE,
			Reason: "MachineNotPlaced", Message: "the machine is on no node yet"}, nil
	}
	var off []string
	for _, f := range m.GetSpec().GetFileShares() {
		obj, err := s.get(ctx, registry.FileShare, resource.Key{Project: m.GetMetadata().GetProject(), Name: f.GetFileShare()})
		if err != nil {
			return nil, err
		}
		if fs, ok := obj.(*storagev1alpha1.FileShare); !ok || fs.GetStatus().GetNode() != node {
			off = append(off, f.GetFileShare())
		}
	}
	if len(off) > 0 {
		return &metav1.Condition{Type: FileSharesOnNode, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "FileShareNotOnNode",
			Message: fmt.Sprintf("file share %s is not on node %s, which the machine runs on, and is not mounted until it is", strings.Join(off, ", "), node)}, nil
	}
	return &metav1.Condition{Type: FileSharesOnNode, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "OnNode"}, nil
}

// ownSharesOnNode is fileSharesOnNode of m, at generation gen of the object
// whose uid is uid, if m is that object's machine; nil otherwise.
func (s *Server) ownSharesOnNode(ctx context.Context, uid string, m *computev1alpha1.Machine, gen int64) (*metav1.Condition, error) {
	if m == nil || !ownedBy(m.GetMetadata(), uid) {
		return nil, nil
	}
	c, err := s.fileSharesOnNode(ctx, m)
	if c != nil {
		c.ObservedGeneration = gen
	}
	return c, err
}

// setOrDrop sets c in list, or, with c nil, drops the condition of type typ.
func setOrDrop(list []*metav1.Condition, typ string, c *metav1.Condition, now time.Time) []*metav1.Condition {
	if c == nil {
		return slices.DeleteFunc(slices.Clone(list), func(x *metav1.Condition) bool { return x.GetType() == typ })
	}
	return conditions.Set(list, c, now)
}

// containersMountingShare maps a share to the containers whose volumes name
// it: one held back for it goes on once it is made.
func (s *Server) containersMountingShare(ctx context.Context, o store.Object) []controller.Key {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Container.Kind, Project: o.Project})
	if err != nil {
		s.log.Error("listing containers failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, co := range objs {
		obj, err := s.backend(registry.Container).decode(co)
		if err != nil {
			continue
		}
		if slices.ContainsFunc(obj.(*computev1alpha1.Container).GetSpec().GetVolumes(), func(v *computev1alpha1.ContainerVolume) bool {
			return v.GetSourceKind() == sourceKindFileShare && v.GetSource() == o.Name
		}) {
			keys = append(keys, controller.Key{Project: co.Project, Name: co.Name})
		}
	}
	return keys
}

// fileShareAmounts is one share and its size, which counts as disk. Its
// server is the platform's, and counts toward nothing.
func fileShareAmounts(obj proto.Message, _ projectObjects) amounts {
	var a amounts
	a[dimFileShares] = 1
	a[dimDisk] = int64(shareGi(obj.(*storagev1alpha1.FileShare)) * quantity.GiB)
	return a
}
