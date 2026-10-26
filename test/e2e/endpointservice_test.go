package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// applyNetworkEndpoint gives network a way to EndpointService svc, labelled
// with labels.
func (w *world) applyNetworkEndpoint(name, network, svc string, labels map[string]string) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.NetworkEndpoint, &networkv1alpha1.NetworkEndpoint{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name, Labels: labels},
		Spec:     &networkv1alpha1.NetworkEndpointSpec{Network: network, Service: svc},
	}); err != nil {
		w.t.Fatal(err)
	}
}

// waitNetworkEndpoint waits until NetworkEndpoint name's Ready condition has
// reason and returns it.
func (w *world) waitNetworkEndpoint(name, reason string) *networkv1alpha1.NetworkEndpoint {
	w.t.Helper()
	var ne *networkv1alpha1.NetworkEndpoint
	testutil.Eventually(w.t, "network endpoint "+name+" "+reason, func() (bool, string) {
		ne, _ = get[*networkv1alpha1.NetworkEndpoint](w, registry.NetworkEndpoint, name)
		return conditions.Find(ne.GetStatus().GetConditions(), "Ready").GetReason() == reason, fmt.Sprint(ne.GetStatus())
	})
	return ne
}

// waitEndpoints waits until the host carries exactly want.
func (w *world) waitEndpoints(what string, want ...driver.Endpoint) {
	w.t.Helper()
	driver.SortEndpoints(want)
	testutil.Eventually(w.t, what, func() (bool, string) {
		got := w.host.Endpoints()
		return slices.Equal(got, want), fmt.Sprint(got)
	})
}

// serveGit runs container git on network svc, and EndpointService git to
// its port 22 for the networks selector picks, and container api on the
// default network, which is on the host only with a machine of its own.
func (w *world) serveGit(selector string) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.Network, &networkv1alpha1.Network{
		Metadata: &metav1.ObjectMeta{Project: project, Name: "svc"},
		Spec:     &networkv1alpha1.NetworkSpec{Ipv4Cidr: "10.0.0.0/24"},
	}); err != nil {
		w.t.Fatal(err)
	}
	git := container("git", "nginx")
	git.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: "svc"}}
	w.apply(git)
	w.apply(container("api", "nginx"))
	w.waitReason("git", "Running")
	w.waitReason("api", "Running")
	if _, err := w.root.Apply(context.Background(), registry.EndpointService, &networkv1alpha1.EndpointService{
		Metadata: &metav1.ObjectMeta{Project: project, Name: "git"},
		Spec: &networkv1alpha1.EndpointServiceSpec{
			Target:   &networkv1alpha1.EndpointServiceTarget{Object: &networkv1alpha1.EndpointServiceTarget_Container{Container: "git"}},
			Protocol: networkv1alpha1.Protocol_PROTOCOL_TCP, Port: 22, NetworkSelector: selector,
		},
	}); err != nil {
		w.t.Fatal(err)
	}
}

// labelDefault sets the default network's labels.
func (w *world) labelDefault(labels map[string]string) {
	w.t.Helper()
	n, _ := get[*networkv1alpha1.Network](w, registry.Network, "default")
	n.Metadata.Labels = labels
	n.Status = nil
	if _, err := w.root.Apply(context.Background(), registry.Network, n); err != nil {
		w.t.Fatal(err)
	}
}

// TestTheHostCarriesOnlyAllowedEndpoints: an endpoint of a network the
// service's selector does not pick is carried nowhere on the host, is
// once the network's labels match, and is no more once they do not.
func TestTheHostCarriesOnlyAllowedEndpoints(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.serveGit("tier=ws")
	w.applyNetworkEndpoint("git", "default", "git", nil)
	w.waitNetworkEndpoint("git", "NotAllowed")
	w.waitSettled()
	if got := w.host.Endpoints(); len(got) > 0 {
		t.Errorf("the host carries %v for a network the selector does not pick", got)
	}
	w.labelDefault(map[string]string{"tier": "ws"})
	w.waitNetworkEndpoint("git", "Connected")
	testutil.Eventually(w.t, "the endpoint carried once allowed", func() (bool, string) {
		got := w.host.Endpoints()
		return len(got) == 1, fmt.Sprint(got)
	})
	w.labelDefault(nil)
	w.waitNetworkEndpoint("git", "NotAllowed")
	w.waitEndpoints("the endpoint no longer carried once not allowed")
}

// TestADeletedEndpointWaitsForTheHost: an endpoint being deleted stays, and
// keeps its address, for as long as its node cannot stop carrying it.
func TestADeletedEndpointWaitsForTheHost(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.serveGit("")
	w.applyNetworkEndpoint("git", "default", "git", nil)
	w.waitNetworkEndpoint("git", "Connected")
	testutil.Eventually(w.t, "the endpoint carried", func() (bool, string) {
		got := w.host.Endpoints()
		return len(got) == 1, fmt.Sprint(got)
	})
	w.host.FailNetworking(errors.New("injected"))
	if _, err := w.root.Delete(context.Background(), registry.NetworkEndpoint, client.Key{Project: project, Name: "git"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(w.t, "the endpoint being deleted", func() (bool, string) {
		ne, _ := get[*networkv1alpha1.NetworkEndpoint](w, registry.NetworkEndpoint, "git")
		return ne.GetMetadata().GetDeleteTime() != nil, fmt.Sprint(ne.GetMetadata())
	})
	testutil.Unchanged(w.t, "the deleted endpoint and what the host carries, while the host cannot let go", 400*time.Millisecond, func() (string, error) {
		ne, ok := get[*networkv1alpha1.NetworkEndpoint](w, registry.NetworkEndpoint, "git")
		return fmt.Sprint(ok, ne.GetStatus().GetIpv4(), w.host.Endpoints()), nil
	})
	if _, ok := get[*networkv1alpha1.NetworkEndpoint](w, registry.NetworkEndpoint, "git"); !ok || len(w.host.Endpoints()) != 1 {
		t.Errorf("with the host failing, the deleted endpoint is there %t and the host carries %v", ok, w.host.Endpoints())
	}
	w.host.FailNetworking(nil)
	w.waitEndpoints("the endpoint no longer carried")
	testutil.Eventually(w.t, "the deleted endpoint gone", func() (bool, string) {
		ne, ok := get[*networkv1alpha1.NetworkEndpoint](w, registry.NetworkEndpoint, "git")
		return !ok, fmt.Sprint(ne.GetStatus())
	})
}

// TestNetworkEndpointsReachTheHost: a network reaches a container of another
// network of the same range through its endpoint, at an address of its own
// range its resolver names; the container's security group lets in that
// network by its transit address; a set ensured again as it is is not
// written again; and the endpoint and the service go once deleted, each
// once the host no longer carries it.
func TestNetworkEndpointsReachTheHost(t *testing.T) {
	t.Parallel()
	w := newIdleWorld(t)
	w.dnsUpstreams = []netip.AddrPort{upstreamDNS(t)}
	w.startAgent()
	if _, err := w.root.Apply(context.Background(), registry.Network, &networkv1alpha1.Network{
		Metadata: &metav1.ObjectMeta{Project: project, Name: "svc"},
		Spec:     &networkv1alpha1.NetworkSpec{Ipv4Cidr: "10.0.0.0/24"},
	}); err != nil {
		t.Fatal(err)
	}
	w.applyGroup("git", &networkv1alpha1.SecurityGroupSpec{Network: "svc", Ingress: []*networkv1alpha1.IngressRule{{
		Protocol: networkv1alpha1.SecurityGroupProtocol_SECURITY_GROUP_PROTOCOL_TCP, Ports: "22",
		From: &networkv1alpha1.SecurityGroupPeer{Peer: &networkv1alpha1.SecurityGroupPeer_Network{Network: "default"}},
	}}})
	git := container("git", "nginx")
	git.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: "svc", SecurityGroups: []string{"git"}}}
	w.apply(git)
	w.apply(container("api", "nginx"))
	w.waitReason("git", "Running")
	w.waitReason("api", "Running")

	if _, err := w.root.Apply(context.Background(), registry.EndpointService, &networkv1alpha1.EndpointService{
		Metadata: &metav1.ObjectMeta{Project: project, Name: "git"},
		Spec: &networkv1alpha1.EndpointServiceSpec{
			Target:   &networkv1alpha1.EndpointServiceTarget{Object: &networkv1alpha1.EndpointServiceTarget_Container{Container: "git"}},
			Protocol: networkv1alpha1.Protocol_PROTOCOL_TCP, Port: 22,
		},
	}); err != nil {
		t.Fatal(err)
	}
	w.applyNetworkEndpoint("git", "default", "git", nil)
	ne := w.waitNetworkEndpoint("git", "Connected")
	es, _ := get[*networkv1alpha1.EndpointService](w, registry.EndpointService, "git")
	addr := netip.MustParseAddr(ne.GetStatus().GetIpv4())
	if addr == w.ipv4Of("api") || !netip.MustParsePrefix("10.0.0.0/24").Contains(addr) {
		t.Errorf("the endpoint's address %s is not a free one of default", addr)
	}
	want := driver.Endpoint{
		Protocol: driver.TCP, NetworkUID: w.networkUID("default"), Addr: addr, Port: 22,
		ServiceNetworkUID: w.networkUID("svc"), TransitPort: uint16(es.GetStatus().GetTransitPort()),
		To: netip.AddrPortFrom(w.ipv4Of("git"), 22),
	}
	w.waitEndpoints("the endpoint carried", want)
	w.answers("default", "10.0.0.1", "git."+project+".hangar.internal", dns.RcodeSuccess, addr.String())

	// The group's network peer is default's transit address, as the
	// service's machine sees default's connections come from.
	n, _ := get[*networkv1alpha1.Network](w, registry.Network, "default")
	transit := netip.MustParsePrefix(n.GetStatus().GetTransitIpv4()).Addr().Next()
	testutil.Eventually(w.t, "git's policy lets default in", func() (bool, string) {
		p := w.host.Firewall(w.networkUID("svc"))[w.machine("git").GetMetadata().GetUid()+"/eth0"]
		return len(p.Ingress) == 1 && slices.Equal(p.Ingress[0].Peers, []netip.Prefix{netip.PrefixFrom(transit, 32)}), fmt.Sprintf("%+v", p)
	})

	// Applied again unchanged but for a label, nothing is written.
	w.waitSettled()
	writes := w.host.EndpointWrites()
	w.applyNetworkEndpoint("git", "default", "git", map[string]string{"tier": "ws"})
	w.waitSettled()
	if got := w.host.EndpointWrites(); got != writes {
		t.Errorf("the same endpoints were written %d more times", got-writes)
	}

	if _, err := w.root.Delete(context.Background(), registry.NetworkEndpoint, client.Key{Project: project, Name: "git"}); err != nil {
		t.Fatal(err)
	}
	w.waitEndpoints("the endpoint no longer carried")
	testutil.Eventually(w.t, "the deleted endpoint gone", func() (bool, string) {
		ne, ok := get[*networkv1alpha1.NetworkEndpoint](w, registry.NetworkEndpoint, "git")
		return !ok, fmt.Sprint(ne.GetStatus())
	})
	w.answers("default", "10.0.0.1", "git."+project+".hangar.internal", dns.RcodeNameError)
	if _, err := w.root.Delete(context.Background(), registry.EndpointService, client.Key{Project: project, Name: "git"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(w.t, "the deleted service gone", func() (bool, string) {
		es, ok := get[*networkv1alpha1.EndpointService](w, registry.EndpointService, "git")
		return !ok, fmt.Sprint(es.GetStatus())
	})
}
