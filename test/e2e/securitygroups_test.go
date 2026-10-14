package e2e

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"testing"

	"github.com/miekg/dns"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

func (w *world) applyGroup(name string, spec *networkv1alpha1.SecurityGroupSpec) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.SecurityGroup, &networkv1alpha1.SecurityGroup{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name}, Spec: spec,
	}); err != nil {
		w.t.Fatal(err)
	}
}

// policyOf waits for the host to hold a policy for the container's eth0 that
// ok accepts.
func (w *world) policyOf(name, what string, ok func(secgroup.Policy) bool) {
	w.t.Helper()
	w.machinePolicy("ct-"+name, what, ok)
}

// machinePolicy waits for the host to hold a policy for the machine's eth0
// that ok accepts.
func (w *world) machinePolicy(name, what string, ok func(secgroup.Policy) bool) {
	w.t.Helper()
	w.eventually(fmt.Sprintf("%s's policy %s", name, what), func() (bool, string) {
		m, found := get[*computev1alpha1.Machine](w, registry.Machine, name)
		if !found {
			return false, "no machine"
		}
		p, has := w.host.Firewall(w.networkUID("default"))[m.GetMetadata().GetUid()+"/eth0"]
		return has && ok(p), fmt.Sprintf("%+v", p)
	})
}

// TestSecurityGroupsReachTheHost: what an interface's groups allow is what
// its node enforces, kept current as groups and members change on running
// machines.
func TestSecurityGroupsReachTheHost(t *testing.T) {
	w := newWorld(t)
	tcp := networkv1alpha1.SecurityGroupProtocol_SECURITY_GROUP_PROTOCOL_TCP
	fromGroup := func(g string) *networkv1alpha1.SecurityGroupPeer {
		return &networkv1alpha1.SecurityGroupPeer{Peer: &networkv1alpha1.SecurityGroupPeer_Group{Group: g}}
	}
	w.applyGroup("db", &networkv1alpha1.SecurityGroupSpec{Network: "default",
		Ingress: []*networkv1alpha1.IngressRule{{Protocol: tcp, Ports: "5432", From: fromGroup("web")}}})

	db := container("db", "nginx")
	db.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: "default", SecurityGroups: []string{"db"}}}
	w.apply(db)
	w.apply(container("plain", "nginx"))
	w.waitReason("db", "Running")
	w.waitReason("plain", "Running")

	// No member of web yet: db's rule lets nobody in.
	w.policyOf("db", "letting nobody in", func(p secgroup.Policy) bool {
		return len(p.Ingress) == 1 && len(p.Ingress[0].Peers) == 0 && p.EgressAll
	})
	w.policyOf("plain", "in default", func(p secgroup.Policy) bool {
		return len(p.Ingress) == 1 && p.Ingress[0].Protocol == secgroup.All && p.EgressAll
	})

	web := container("api", "nginx")
	web.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: "default", SecurityGroups: []string{"web"}}}
	w.apply(web)
	w.waitReason("api", "Running")
	api := netip.MustParsePrefix(addrOf(w.machine("api")) + "/32")
	// api names a group not made yet: it sends nothing until it is, and
	// db's rule naming web lets nobody in, api included.
	w.policyOf("api", "sending nothing for a group not made", func(p secgroup.Policy) bool {
		return !p.EgressAll && len(p.Egress) == 0
	})
	w.policyOf("db", "letting nobody in from a group not made", func(p secgroup.Policy) bool {
		return len(p.Ingress) == 1 && len(p.Ingress[0].Peers) == 0
	})
	w.applyGroup("web", &networkv1alpha1.SecurityGroupSpec{Network: "default"})
	w.policyOf("api", "sending anything once web exists", func(p secgroup.Policy) bool { return p.EgressAll })
	w.policyOf("db", "letting api in on 5432", func(p secgroup.Policy) bool {
		return len(p.Ingress) == 1 && len(p.Ingress[0].Peers) == 1 && p.Ingress[0].Peers[0] == api &&
			p.Ingress[0].Ports[0] == secgroup.PortRange{From: 5432, To: 5432}
	})

	// A member on another node counts as much as one on this.
	remote := bare("remote", "10.0.0.50/24")
	remote.Spec.Node = "n2"
	remote.Spec.Interfaces[0].SecurityGroups = []string{"web"}
	w.applyMachine(remote)
	w.policyOf("db", "letting api and the remote member in", func(p secgroup.Policy) bool {
		return len(p.Ingress) == 1 && slices.Equal(p.Ingress[0].Peers, []netip.Prefix{api, netip.MustParsePrefix("10.0.0.50/32")})
	})

	// A group of another network is no group on this one, though group
	// names are the project's: a member naming it here is in a group that
	// does not exist, and sends nothing.
	w.applyGroup("lockdown", &networkv1alpha1.SecurityGroupSpec{Network: "other",
		Egress: []*networkv1alpha1.EgressRule{{Protocol: tcp, Ports: "443",
			To: &networkv1alpha1.SecurityGroupPeer{Peer: &networkv1alpha1.SecurityGroupPeer_Cidr{Cidr: "1.1.1.1/32"}}}}})
	stray := bare("stray", "10.0.0.51/24")
	stray.Spec.Interfaces[0].SecurityGroups = []string{"lockdown"}
	w.applyMachine(stray)
	w.machinePolicy("stray", "taking nothing of another network's group", func(p secgroup.Policy) bool {
		return !p.EgressAll && len(p.Egress) == 0 && len(p.Ingress) == 0
	})

	// Rules the host cannot hold close the network's interfaces rather
	// than leave the ones before in force, and its services still follow
	// its machines.
	w.host.LimitFirewall(1)
	late := bare("late", "10.0.0.52/24")
	w.applyMachine(late)
	w.policyOf("plain", "closed while the rules cannot be held", func(p secgroup.Policy) bool {
		return len(p.Ingress) == 0 && !p.EgressAll && len(p.Egress) == 0
	})
	w.answers("default", "10.0.0.1", "late."+project+".hangar.internal", dns.RcodeSuccess, "10.0.0.52")
	w.host.LimitFirewall(0)
	w.applyGroup("db", &networkv1alpha1.SecurityGroupSpec{Network: "default",
		Ingress: []*networkv1alpha1.IngressRule{{Protocol: tcp, Ports: "5433", From: fromGroup("web")}}})
	w.policyOf("plain", "open again once they can", func(p secgroup.Policy) bool { return p.EgressAll })
	for _, name := range []string{"remote", "stray", "late"} {
		if _, err := w.system.Delete(context.Background(), registry.Machine, client.Key{Project: project, Name: name}); err != nil {
			t.Fatal(err)
		}
	}

	// A group's rules change on running machines.
	w.applyGroup("db", &networkv1alpha1.SecurityGroupSpec{Network: "default"})
	w.policyOf("db", "letting nobody in once the rule is gone", func(p secgroup.Policy) bool { return len(p.Ingress) == 0 })

	for _, n := range []string{"db", "plain", "api"} {
		w.delete(n)
	}
	for _, n := range []string{"db", "plain", "api"} {
		w.waitGone(n)
	}
	w.waitHostEmpty()
}

// TestAVirtualMachinesInterfaceGetsItsPolicy: a VM's interface is in the
// groups it names, or in default, and its node enforces them as it does a
// container's.
func TestAVirtualMachinesInterfaceGetsItsPolicy(t *testing.T) {
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	tcp := networkv1alpha1.SecurityGroupProtocol_SECURITY_GROUP_PROTOCOL_TCP
	w.applyGroup("ssh", &networkv1alpha1.SecurityGroupSpec{Network: "default",
		Ingress: []*networkv1alpha1.IngressRule{{Protocol: tcp, Ports: "22",
			From: &networkv1alpha1.SecurityGroupPeer{Peer: &networkv1alpha1.SecurityGroupPeer_Cidr{Cidr: "0.0.0.0/0"}}}}})
	vm := func(name string, nics ...*computev1alpha1.NetworkInterface) {
		t.Helper()
		if _, err := w.root.Apply(context.Background(), registry.VirtualMachine, &computev1alpha1.VirtualMachine{
			Metadata: &metav1.ObjectMeta{Project: project, Name: name},
			Spec: &computev1alpha1.VirtualMachineSpec{
				RootDisk: &computev1alpha1.VirtualMachineRootDisk{Source: &computev1alpha1.VirtualMachineRootDisk_FromImage{
					FromImage: &computev1alpha1.VirtualMachineImageDisk{Image: &storagev1alpha1.ImageSource{Name: "noble"}},
				}},
				Interfaces: nics,
			},
		}); err != nil {
			t.Fatal(err)
		}
		w.waitVMReason(name, "Running")
	}
	vm("bastion", &computev1alpha1.NetworkInterface{Network: "default", SecurityGroups: []string{"ssh"}})
	vm("plain")
	w.machinePolicy(vmMachineOf("bastion"), "letting SSH in", func(p secgroup.Policy) bool {
		return len(p.Ingress) == 1 && p.Ingress[0].Protocol == secgroup.TCP &&
			p.Ingress[0].Ports[0] == secgroup.PortRange{From: 22, To: 22} && p.EgressAll
	})
	w.machinePolicy(vmMachineOf("plain"), "in default", func(p secgroup.Policy) bool {
		return len(p.Ingress) == 1 && p.Ingress[0].Protocol == secgroup.All && p.EgressAll
	})

	// Its groups change on the running VM, as a container's do.
	w.applyGroup("ssh", &networkv1alpha1.SecurityGroupSpec{Network: "default"})
	w.machinePolicy(vmMachineOf("bastion"), "letting nobody in once the rule is gone", func(p secgroup.Policy) bool {
		return len(p.Ingress) == 0 && p.EgressAll
	})
	for _, name := range []string{"bastion", "plain"} {
		if _, err := w.root.Delete(context.Background(), registry.VirtualMachine, client.Key{Project: project, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	w.deleteImage("noble")
	w.waitHostEmpty()
}
