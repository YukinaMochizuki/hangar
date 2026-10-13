package e2e

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
)

// TestAContainerOnTwoNetworks: a container asking for two networks has an
// interface on each, in order, is named on both, and asks its first
// network's resolver.
func TestAContainerOnTwoNetworks(t *testing.T) {
	w := newWorld(t)
	if _, err := w.root.Apply(context.Background(), registry.Network, &networkv1alpha1.Network{
		Metadata: &metav1.ObjectMeta{Project: project, Name: "backend"},
		Spec:     &networkv1alpha1.NetworkSpec{Ipv4Cidr: "10.1.0.0/24"},
	}); err != nil {
		t.Fatal(err)
	}
	c := container("api", "nginx")
	c.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: "backend"}, {Network: "default"}}
	w.apply(c)
	w.waitReason("api", "Running")

	m := w.machine("api")
	vm := w.host.Snapshot().VMs[m.GetMetadata().GetUid()]
	var names []string
	for _, i := range vm.Spec.Interfaces {
		names = append(names, fmt.Sprintf("%s %s", i.Name, i.Address))
	}
	nics := m.GetSpec().GetInterfaces()
	want := []string{"eth0 " + nics[0].GetIpv4(), "eth1 " + nics[1].GetIpv4()}
	if !slices.Equal(names, want) || !strings.HasPrefix(nics[0].GetIpv4(), "10.1.0.") || !strings.HasPrefix(nics[1].GetIpv4(), "10.0.0.") {
		t.Errorf("interfaces on the host %v, machine %v", names, nics)
	}
	if !slices.Equal(vm.Spec.Nameservers, []string{"10.1.0.1"}) {
		t.Errorf("nameservers %v, want the first network's resolver", vm.Spec.Nameservers)
	}
	zone := project + ".hangar.internal"
	ip := func(p string) string { a, _, _ := strings.Cut(p, "/"); return a }
	w.answers("backend", "10.1.0.1", "api."+zone, dns.RcodeSuccess, ip(nics[0].GetIpv4()))
	w.answers("default", "10.0.0.1", "api."+zone, dns.RcodeSuccess, ip(nics[1].GetIpv4()))

	w.delete("api")
	w.waitGone("api")
	w.waitHostEmpty()
}
