package e2e

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/client"
)

// upstreamDNS serves the world's upstream resolver on the loopback: every A
// question gets 198.51.100.7.
func upstreamDNS(t *testing.T) netip.AddrPort {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		resp := new(dns.Msg).SetReply(q)
		resp.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.IPv4(198, 51, 100, 7),
		}}
		_ = w.WriteMsg(resp)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return netip.MustParseAddrPort(pc.LocalAddr().String())
}

func (w *world) networkUID(name string) string {
	w.t.Helper()
	n, ok := get[*networkv1alpha1.Network](w, registry.Network, name)
	if !ok {
		w.t.Fatalf("no network %s", name)
	}
	return n.GetMetadata().GetUid()
}

// resolve asks the resolver of the network, at its gateway, for name's
// addresses, as a machine on the network would; ok is false if nothing
// answers there.
func (w *world) resolve(network, gateway, name string) (rcode int, addrs []string, ok bool) {
	w.t.Helper()
	at, ok := w.host.ServiceAddr(w.networkUID(network), "udp", netip.AddrPortFrom(netip.MustParseAddr(gateway), 53))
	if !ok {
		return 0, nil, false
	}
	c := &dns.Client{Net: "udp", Timeout: time.Second}
	resp, _, err := c.Exchange(new(dns.Msg).SetQuestion(dns.Fqdn(name), dns.TypeA), at.String())
	if err != nil {
		return 0, nil, false
	}
	for _, rr := range resp.Answer {
		if a, ok := rr.(*dns.A); ok {
			addrs = append(addrs, a.A.String())
		}
	}
	return resp.Rcode, addrs, true
}

// answers waits until the network's resolver says what is wanted of name.
func (w *world) answers(network, gateway, name string, rcode int, want ...string) {
	w.t.Helper()
	w.eventually(fmt.Sprintf("%s on %s resolving to %s %v", name, network, dns.RcodeToString[rcode], want), func() (bool, string) {
		got, addrs, ok := w.resolve(network, gateway, name)
		return ok && got == rcode && slices.Equal(addrs, want), fmt.Sprintf("answered %v: %s %v", ok, dns.RcodeToString[got], addrs)
	})
}

func addrOf(m *computev1alpha1.Machine) string {
	a, _, _ := strings.Cut(m.GetSpec().GetInterfaces()[0].GetIpv4(), "/")
	return a
}

// TestEachNetworkHasItsResolver: a network's resolver answers for the
// machines on it, and only those, forwards everything else, follows its
// namespace being made again, and goes with its network.
func TestEachNetworkHasItsResolver(t *testing.T) {
	w := newIdleWorld(t)
	w.dnsUpstreams = []netip.AddrPort{upstreamDNS(t)}
	w.startAgent()

	w.apply(container("api", "nginx"))
	w.apply(container("db", "nginx"))
	w.waitReason("api", "Running")
	w.waitReason("db", "Running")
	api, db := addrOf(w.machine("api")), addrOf(w.machine("db"))
	zone := project + ".hangar.internal"
	w.answers("default", "10.0.0.1", "api."+zone, dns.RcodeSuccess, api)
	w.answers("default", "10.0.0.1", "db."+zone, dns.RcodeSuccess, db)
	w.answers("default", "10.0.0.1", "nobody."+zone, dns.RcodeNameError)
	w.answers("default", "10.0.0.1", "example.com", dns.RcodeSuccess, "198.51.100.7")

	// The guest is told to ask its network's resolver, and to search its
	// project.
	vm := w.host.Snapshot().VMs[w.machine("api").GetMetadata().GetUid()]
	if !slices.Equal(vm.Spec.Nameservers, []string{"10.0.0.1"}) || !slices.Equal(vm.Spec.Search, []string{zone}) {
		t.Errorf("the guest resolves with %v, searching %v", vm.Spec.Nameservers, vm.Spec.Search)
	}

	// A machine on another network of the project sees nothing of these,
	// nor they of it.
	if _, err := w.root.Apply(context.Background(), registry.Network, &networkv1alpha1.Network{
		Metadata: &metav1.ObjectMeta{Project: project, Name: "other"},
		Spec:     &networkv1alpha1.NetworkSpec{Ipv4Cidr: "10.1.0.0/24"},
	}); err != nil {
		t.Fatal(err)
	}
	lone := bare("lone", "10.1.0.5/24")
	lone.Spec.Interfaces[0].Network = "other"
	w.eventually("network other given its transit link", func() (bool, string) {
		n, _ := get[*networkv1alpha1.Network](w, registry.Network, "other")
		return n.GetStatus().GetTransitIpv4() != "", fmt.Sprintf("%v", n.GetStatus())
	})
	w.applyMachine(lone)
	keeper := bare("keeper", "10.1.0.6/24")
	keeper.Spec.Interfaces[0].Network = "other"
	w.applyMachine(keeper)
	w.waitMachine("lone", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, "Running")
	w.answers("other", "10.1.0.1", "lone."+zone, dns.RcodeSuccess, "10.1.0.5")
	w.answers("other", "10.1.0.1", "api."+zone, dns.RcodeNameError)
	w.answers("default", "10.0.0.1", "lone."+zone, dns.RcodeNameError)

	// A machine of another node is named too, and leaves a network's names
	// as it moves to another network, which changes nothing on this host.
	remote := bare("remote", "10.1.0.9/24")
	remote.Spec.Node = "n2"
	remote.Spec.Interfaces[0].Network = "other"
	w.applyMachine(remote)
	w.answers("other", "10.1.0.1", "remote."+zone, dns.RcodeSuccess, "10.1.0.9")
	w.editMachine("remote", func(m *computev1alpha1.Machine) {
		m.Spec.Interfaces[0].Network, m.Spec.Interfaces[0].Ipv4 = "default", "10.0.0.77/24"
	})
	w.answers("default", "10.0.0.1", "remote."+zone, dns.RcodeSuccess, "10.0.0.77")
	w.answers("other", "10.1.0.1", "remote."+zone, dns.RcodeNameError)
	w.answers("other", "10.1.0.1", "keeper."+zone, dns.RcodeSuccess, "10.1.0.6")

	// A container gone is a name gone.
	w.delete("db")
	w.waitGone("db")
	w.answers("default", "10.0.0.1", "db."+zone, dns.RcodeNameError)

	// A namespace made again behind the agent's back is served again.
	w.host.RemakeNamespace(w.networkUID("default"))
	w.answers("default", "10.0.0.1", "api."+zone, dns.RcodeSuccess, api)
	w.eventually("the old namespace's sockets closed", func() (bool, string) {
		l := w.host.Snapshot().Listening[w.networkUID("default")]
		return len(l) == 4, fmt.Sprintf("%v", l) // the resolver's two, DHCP's and the metadata service's
	})

	// An agent restarting leaves nothing listening, then serves every
	// network again.
	w.stopAgent()
	if l := w.host.Snapshot().Listening; len(l) != 0 {
		t.Errorf("an agent stopped left %v listening", l)
	}
	w.startAgent()
	w.answers("default", "10.0.0.1", "api."+zone, dns.RcodeSuccess, api)
	w.answers("other", "10.1.0.1", "keeper."+zone, dns.RcodeSuccess, "10.1.0.6")

	// Networks no machine uses leave the host, their resolvers with them.
	w.delete("api")
	for _, name := range []string{"lone", "keeper", "remote"} {
		if _, err := w.system.Delete(context.Background(), registry.Machine, client.Key{Project: project, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	w.waitGone("api")
	w.waitHostEmpty()
}
