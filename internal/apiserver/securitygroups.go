package apiserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

func validateSecurityGroup(ctx context.Context, s *Server, _ auth.Principal, _, obj proto.Message) error {
	g := obj.(*networkv1alpha1.SecurityGroup)
	if g.GetMetadata().GetName() == secgroup.Default {
		return invalid("%q is every network's implicit group, and no object may take its name", secgroup.Default)
	}
	if err := ValidName(g.GetSpec().GetNetwork()); err != nil {
		return invalid("spec.network: %v", err)
	}
	if _, err := secgroup.FromSpec(g.GetSpec()); err != nil {
		return invalid("spec: %v", err)
	}
	var peers []string
	for _, p := range slices.Concat(fromPeers(g.GetSpec()), toPeers(g.GetSpec())) {
		if name := p.GetGroup(); name != "" {
			if err := ValidName(name); err != nil {
				return invalid("spec: group %q: %v", name, err)
			}
			peers = append(peers, name)
		}
	}
	return s.refuseGroupsOfOtherNetworks(ctx, g.GetMetadata().GetProject(), g.GetSpec().GetNetwork(), peers, "spec")
}

// refuseGroupsOfOtherNetworks refuses names of groups that exist on another
// network than network: a group is of one network, and on any other its
// name stands for no one. A name no group has yet is let through, so
// objects can be applied in any order; until it is made on network, rules
// naming it match no one, and an interface in it sends only what its other
// groups' egress rules allow.
func (s *Server) refuseGroupsOfOtherNetworks(ctx context.Context, project, network string, names []string, field string) error {
	for _, name := range names {
		if name == secgroup.Default {
			continue
		}
		o, err := s.store.Get(ctx, store.Key{Kind: registry.SecurityGroup.Kind, Project: project, Name: name})
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		var g networkv1alpha1.SecurityGroup
		if err := proto.Unmarshal(o.Data, &g); err != nil {
			return err
		}
		if other := g.GetSpec().GetNetwork(); other != network {
			return invalid("%s: security group %q is of network %q, not %q", field, name, other, network)
		}
	}
	return nil
}

func fromPeers(spec *networkv1alpha1.SecurityGroupSpec) []*networkv1alpha1.SecurityGroupPeer {
	var out []*networkv1alpha1.SecurityGroupPeer
	for _, r := range spec.GetIngress() {
		out = append(out, r.GetFrom())
	}
	return out
}

func toPeers(spec *networkv1alpha1.SecurityGroupSpec) []*networkv1alpha1.SecurityGroupPeer {
	var out []*networkv1alpha1.SecurityGroupPeer
	for _, r := range spec.GetEgress() {
		out = append(out, r.GetTo())
	}
	return out
}

// validateSecurityGroupNames checks the groups an interface names.
func validateSecurityGroupNames(i int, names []string) error {
	if len(names) > secgroup.MaxGroupsPerInterface {
		return invalid("spec.interfaces[%d].security_groups: %d groups, at most %d", i, len(names), secgroup.MaxGroupsPerInterface)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if err := ValidName(n); err != nil {
			return invalid("spec.interfaces[%d].security_groups: %v", i, err)
		}
		if seen[n] {
			return invalid("spec.interfaces[%d].security_groups: %q twice", i, n)
		}
		seen[n] = true
	}
	return nil
}

// refuseDeletingGroupInUse refuses to delete a group an interface is in, as
// AWS does: interfaces it restricts would open, or, for one it is missing
// from, close, without anyone having asked for either. A group another
// group's rules name may go; those rules then allow nothing.
func refuseDeletingGroupInUse(ctx context.Context, s *Server, obj proto.Message) error {
	g := obj.(*networkv1alpha1.SecurityGroup)
	project, name, network := g.GetMetadata().GetProject(), g.GetMetadata().GetName(), g.GetSpec().GetNetwork()
	var users []string
	for _, kind := range []registry.Resource{registry.Container, registry.VirtualMachine, registry.Machine} {
		objs, _, err := s.store.List(ctx, store.ListOptions{Kind: kind.Kind, Project: project})
		if err != nil {
			return err
		}
		for _, o := range objs {
			if inGroup(kind, o, network, name) {
				users = append(users, fmt.Sprintf("%s %s", strings.ToLower(kind.Kind), o.Name))
			}
		}
	}
	if len(users) == 0 {
		return nil
	}
	slices.Sort(users)
	return failedPrecondition("security group %q is in use by %s; take it off their interfaces first", name, strings.Join(users, ", "))
}

// inGroup says whether a stored Container, VirtualMachine or Machine has an
// interface on network in group.
func inGroup(kind registry.Resource, o store.Object, network, group string) bool {
	switch kind.Kind {
	case registry.Container.Kind:
		var c computev1alpha1.Container
		if proto.Unmarshal(o.Data, &c) != nil {
			return false
		}
		return slices.ContainsFunc(c.GetSpec().GetInterfaces(), func(i *computev1alpha1.NetworkInterface) bool {
			return i.GetNetwork() == network && slices.Contains(i.GetSecurityGroups(), group)
		})
	case registry.VirtualMachine.Kind:
		var vm computev1alpha1.VirtualMachine
		if proto.Unmarshal(o.Data, &vm) != nil {
			return false
		}
		return slices.ContainsFunc(vm.GetSpec().GetInterfaces(), func(i *computev1alpha1.NetworkInterface) bool {
			return i.GetNetwork() == network && slices.Contains(i.GetSecurityGroups(), group)
		})
	case registry.Machine.Kind:
		var m computev1alpha1.Machine
		if proto.Unmarshal(o.Data, &m) != nil {
			return false
		}
		return slices.ContainsFunc(m.GetSpec().GetInterfaces(), func(i *computev1alpha1.MachineInterface) bool {
			return i.GetNetwork() == network && slices.Contains(i.GetSecurityGroups(), group)
		})
	}
	return false
}
