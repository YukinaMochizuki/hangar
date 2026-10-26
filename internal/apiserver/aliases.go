package apiserver

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// validateAliases checks the aliases of the interface at spec.interfaces[i]
// on their own: each a DNS label, once, and at most MaxAliases.
func validateAliases(i int, aliases []string) error {
	if len(aliases) > MaxAliases {
		return invalid("spec.interfaces[%d].aliases: %d aliases, at most %d", i, len(aliases), MaxAliases)
	}
	seen := map[string]bool{}
	for _, a := range aliases {
		if err := ValidName(a); err != nil {
			return invalid("spec.interfaces[%d].aliases: %v", i, err)
		}
		if seen[a] {
			return invalid("spec.interfaces[%d].aliases: %q twice", i, a)
		}
		seen[a] = true
	}
	return nil
}

// nameHolder is what the names on its networks are held by, and those
// names: on each network it has an interface on, its name and that
// interface's aliases. It is a Container or VirtualMachine by its spec, or
// one's Machine, which is what the resolvers answer from and may not yet
// have followed its owner's spec, or may never, as when a resize does not
// fit.
type nameHolder struct {
	// kind is the owner's in words, name its name, which is the name the
	// resolvers answer, and uid its uid, empty for one not yet made.
	kind, name, uid string
	// machine names the Machine the names are read from; empty for the
	// owner's spec.
	machine string
	aliases map[string][]string // by network
}

func holderOf(kind string, meta *metav1.ObjectMeta, nics []*computev1alpha1.NetworkInterface) nameHolder {
	h := nameHolder{kind: kind, name: meta.GetName(), uid: meta.GetUid(), aliases: map[string][]string{}}
	for _, nic := range machineInterfaces(nics) {
		h.aliases[nic.GetNetwork()] = nic.GetAliases()
	}
	return h
}

// applying is the holder an apply of obj, a Container or VirtualMachine,
// would make, by the stored object's uid, old's, nil on create.
func applying(kind string, old, obj proto.Message, nics []*computev1alpha1.NetworkInterface) nameHolder {
	h := holderOf(kind, object.Meta(obj), nics)
	h.uid = ""
	if old != nil {
		h.uid = object.Meta(old).GetUid()
	}
	return h
}

func (h nameHolder) String() string {
	if h.machine != "" && h.kind == "machine" {
		return "machine " + h.machine
	}
	if h.machine != "" {
		return fmt.Sprintf("%s %s's machine %s", h.kind, h.name, h.machine)
	}
	return h.kind + " " + h.name
}

// isOf reports whether h is self's spec or self's own machine.
func (h nameHolder) isOf(self nameHolder) bool {
	if h.machine == "" {
		return h.kind == self.kind && h.name == self.name
	}
	return self.uid != "" && h.kind == self.kind && h.uid == self.uid
}

// checkAliasesFree refuses a Container or VirtualMachine, self, a name on
// one of its networks that another machine with an interface there holds,
// by its owner's spec or by the Machine the resolvers answer from: an alias
// of self's that is another's alias or name, or self's name that is
// another's alias. One being deleted holds its names until it is gone.
// self's uid is the stored object's, empty on create, so a machine left by
// an earlier object of its name holds its names too. The caller holds
// attach, which every write of a Container, VirtualMachine or Machine
// holds, from the check to the write.
func (s *Server) checkAliasesFree(ctx context.Context, project string, self nameHolder) error {
	others, err := s.nameHolders(ctx, project)
	if err != nil {
		return err
	}
	for _, o := range others {
		if o.isOf(self) {
			continue
		}
		for network, mine := range self.aliases {
			theirs, on := o.aliases[network]
			if !on {
				continue
			}
			for _, a := range mine {
				if a == o.name {
					return failedPrecondition("alias %q is the name of %s, on network %q", a, o, network)
				}
				for _, b := range theirs {
					if a == b {
						return failedPrecondition("alias %q is held on network %q by %s", a, network, o)
					}
				}
			}
			for _, b := range theirs {
				if b == self.name {
					return failedPrecondition("name %q is an alias %s holds on network %q", self.name, o, network)
				}
			}
		}
	}
	return nil
}

// nameHolders are the project's Containers and VirtualMachines, by their
// specs, and their Machines. A machine the resolvers do not name, as a
// file share's server, holds no name.
func (s *Server) nameHolders(ctx context.Context, project string) ([]nameHolder, error) {
	var out []nameHolder
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Container.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		obj, err := s.backend(registry.Container).decode(o)
		if err != nil {
			return nil, err
		}
		c := obj.(*computev1alpha1.Container)
		out = append(out, holderOf("container", c.GetMetadata(), c.GetSpec().GetInterfaces()))
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
		out = append(out, holderOf("virtual machine", vm.GetMetadata(), vm.GetSpec().GetInterfaces()))
	}
	objs, _, err = s.store.List(ctx, store.ListOptions{Kind: registry.Machine.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		obj, err := s.backend(registry.Machine).decode(o)
		if err != nil {
			return nil, err
		}
		m := obj.(*computev1alpha1.Machine)
		// The node names a machine by its owner's name, or its own, and a
		// file share's server not at all.
		if m.GetSpec().GetFileShareServer() != nil {
			continue
		}
		h := nameHolder{kind: "machine", name: o.Name, machine: o.Name, aliases: map[string][]string{}}
		for _, ref := range m.GetMetadata().GetOwnerReferences() {
			if !ref.GetController() {
				continue
			}
			h.name = ref.GetName()
			switch ref.GetKind() {
			case registry.Container.Kind:
				h.kind, h.uid = "container", ref.GetUid()
			case registry.VirtualMachine.Kind:
				h.kind, h.uid = "virtual machine", ref.GetUid()
			}
		}
		for _, nic := range m.GetSpec().GetInterfaces() {
			h.aliases[nic.GetNetwork()] = nic.GetAliases()
		}
		out = append(out, h)
	}
	return out, nil
}
