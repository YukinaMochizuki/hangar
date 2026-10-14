package e2e

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/miekg/dns"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
	"github.com/YukinaMochizuki/hangar/internal/node/ipv6pkt"
	"github.com/YukinaMochizuki/hangar/internal/node/ra"
)

var hostPrefix = netip.MustParsePrefix("2001:db8:0:1::/64")

func ipv6Of(m *computev1alpha1.Machine) netip.Addr {
	p, err := netip.ParsePrefix(m.GetSpec().GetInterfaces()[0].GetIpv6())
	if err != nil {
		return netip.Addr{}
	}
	return p.Addr()
}

// TestMachinesGetPublicIPv6: on a host with a prefix, the node reports it,
// each machine interface gets a /128 of it, the host is ensured with it,
// the network advertises its router and leases the address by DHCPv6 to
// the interface's MAC alone, its resolver answers AAAA, and the machine's
// groups hold the address.
func TestMachinesGetPublicIPv6(t *testing.T) {
	w := newIdleWorld(t)
	w.ipv6Prefix = hostPrefix
	w.startAgent()
	w.eventually("the node reporting its prefix", func() (bool, string) {
		n, ok := get[*computev1alpha1.Node](w, registry.Node, apiservertest.Node)
		return ok && n.GetStatus().GetIpv6Prefix() == hostPrefix.String(), fmt.Sprint(n.GetStatus())
	})
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	m := w.machine("api")
	addr := ipv6Of(m)
	if !hostPrefix.Contains(addr) {
		t.Fatalf("machine api has IPv6 %q, not of %s", m.GetSpec().GetInterfaces()[0].GetIpv6(), hostPrefix)
	}
	vm := w.host.Snapshot().VMs[m.GetMetadata().GetUid()]
	nic := vm.Spec.Interfaces[0]
	if nic.IPv6 != addr {
		t.Errorf("the VM runs with IPv6 %s, the machine has %s", nic.IPv6, addr)
	}
	mac, _ := net.ParseMAC(nic.MAC)
	uid := w.networkUID("default")

	w.eventually("a router advertisement", func() (bool, string) {
		sent, ok := w.host.SentPackets(uid, driver.RouterSolicitations)
		return ok && slices.ContainsFunc(sent, func(p fake.Packet) bool { return slices.Equal(p.Data, ra.Packet()) }), fmt.Sprint(ok, len(sent))
	})

	solicit := func(from net.HardwareAddr) *dhcpv6.Message {
		sol, err := dhcpv6.NewSolicit(from)
		if err != nil {
			t.Fatal(err)
		}
		pkt := ipv6pkt.Packet{
			Src: netip.MustParseAddr("fe80::5054:ff:fe00:1"), Dst: netip.MustParseAddr("ff02::1:2"), Next: ipv6pkt.UDP, HopLimit: 1,
			Payload: ipv6pkt.UDPDatagram(546, 547, sol.ToBytes()),
		}.Marshal()
		if !w.host.SendPacket(uid, driver.DHCPv6, from, pkt) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
		sent, _ := w.host.SentPackets(uid, driver.DHCPv6)
		for _, p := range sent {
			got, err := ipv6pkt.Parse(p.Data)
			if err != nil || !slices.Equal(p.MAC, from) {
				continue
			}
			if m, err := dhcpv6.MessageFromBytes(got.Payload[8:]); err == nil {
				return m
			}
		}
		return nil
	}
	w.eventually("api leased "+addr.String(), func() (bool, string) {
		adv := solicit(mac)
		if adv == nil {
			return false, "no answer"
		}
		ia := adv.GetOneOption(dhcpv6.OptionIANA)
		if ia == nil {
			return false, fmt.Sprint(adv)
		}
		got := ia.(*dhcpv6.OptIANA).Options.OneAddress()
		return got != nil && got.IPv6Addr.Equal(addr.AsSlice()), fmt.Sprint(adv)
	})
	if adv := solicit(net.HardwareAddr{0x02, 0xde, 0xad, 0xbe, 0xef, 0x01}); adv != nil {
		t.Errorf("an unknown MAC was answered: %v", adv)
	}

	w.eventually("api resolving to its /128", func() (bool, string) {
		at, ok := w.host.ServiceAddr(uid, "udp", netip.AddrPortFrom(netip.MustParseAddr("10.0.0.1"), 53))
		if !ok {
			return false, "nothing listens"
		}
		resp, _, err := (&dns.Client{Timeout: time.Second}).Exchange(new(dns.Msg).SetQuestion("api."+project+".hangar.internal.", dns.TypeAAAA), at.String())
		if err != nil || len(resp.Answer) != 1 {
			return false, fmt.Sprint(resp, err)
		}
		a, ok := resp.Answer[0].(*dns.AAAA)
		return ok && a.AAAA.Equal(addr.AsSlice()), fmt.Sprint(resp)
	})

	w.eventually("default's ingress holding api's /128", func() (bool, string) {
		p := w.host.Firewall(uid)[m.GetMetadata().GetUid()+"/eth0"]
		return len(p.Ingress) == 1 && slices.Contains(p.Ingress[0].Peers, netip.PrefixFrom(addr, 128)), fmt.Sprint(p)
	})
	w.delete("api")
	w.waitGone("api")
	w.waitHostEmpty()
}

// leasedTo has the network's DHCPv6 answer a solicitation from mac, and
// returns the address it leases, if any.
func (w *world) leasedTo(networkUID string, mac net.HardwareAddr) netip.Addr {
	w.t.Helper()
	sol, err := dhcpv6.NewSolicit(mac)
	if err != nil {
		w.t.Fatal(err)
	}
	pkt := ipv6pkt.Packet{
		Src: netip.MustParseAddr("fe80::5054:ff:fe00:1"), Dst: netip.MustParseAddr("ff02::1:2"), Next: ipv6pkt.UDP, HopLimit: 1,
		Payload: ipv6pkt.UDPDatagram(546, 547, sol.ToBytes()),
	}.Marshal()
	if !w.host.SendPacket(networkUID, driver.DHCPv6, mac, pkt) {
		return netip.Addr{}
	}
	time.Sleep(100 * time.Millisecond)
	sent, _ := w.host.SentPackets(networkUID, driver.DHCPv6)
	for _, p := range sent {
		got, err := ipv6pkt.Parse(p.Data)
		if err != nil || !slices.Equal(p.MAC, mac) {
			continue
		}
		m, err := dhcpv6.MessageFromBytes(got.Payload[8:])
		if err != nil || m.Options.OneIANA() == nil || m.Options.OneIANA().Options.OneAddress() == nil {
			continue
		}
		a, _ := netip.AddrFromSlice(m.Options.OneIANA().Options.OneAddress().IPv6Addr)
		return a
	}
	return netip.Addr{}
}

// TestAHostGainingAPrefix: a host given an IPv6 prefix gives its machines
// addresses of it, and none starts again: a VM is leased its address by
// DHCPv6, and a container's guest is told it.
func TestAHostGainingAPrefix(t *testing.T) {
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	w.applyVM("web", 0)
	w.waitVMReason("web", "Running")
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	vmUID := w.machine2(vmMachineOf("web")).GetMetadata().GetUid()
	ctUID := w.machine("api").GetMetadata().GetUid()
	started := w.started(vmUID, ctUID)

	w.stopAgent()
	w.ipv6Prefix = hostPrefix
	w.startAgent()
	var vm *computev1alpha1.VirtualMachine
	w.eventually("the VM's status showing a /128 of the prefix", func() (bool, string) {
		vm, _ = get[*computev1alpha1.VirtualMachine](w, registry.VirtualMachine, "web")
		ifs := vm.GetStatus().GetInterfaces()
		if len(ifs) != 1 {
			return false, fmt.Sprint(vm.GetStatus())
		}
		p, err := netip.ParsePrefix(ifs[0].GetIpv6())
		return err == nil && hostPrefix.Contains(p.Addr()), fmt.Sprint(vm.GetStatus())
	})
	addr := netip.MustParsePrefix(vm.GetStatus().GetInterfaces()[0].GetIpv6()).Addr()
	uid := w.networkUID("default")
	snap := w.host.Snapshot()
	vmNIC := snap.VMs[vmUID].Spec.Interfaces[0]
	mac, _ := net.ParseMAC(vmNIC.MAC)
	w.eventually("the VM leased "+addr.String()+" by DHCPv6", func() (bool, string) {
		got := w.leasedTo(uid, mac)
		return got == addr, got.String()
	})
	w.eventually("the VM's interface on the host holding its /128", func() (bool, string) {
		ifs := w.host.Snapshot().Interfaces[vmUID]
		return len(ifs) == 1 && ifs[0].IPv6 == addr, fmt.Sprint(ifs)
	})
	w.eventually("the container's guest told its /128", func() (bool, string) {
		ctAddr := ipv6Of(w.machine("api"))
		g := w.host.GuestIPv6(ctUID)
		return ctAddr.IsValid() && len(g) == 1 && g[0].Address == ctAddr && g[0].Gateway == driver.GatewayIPv6, fmt.Sprint(ctAddr, g)
	})
	w.checkNotRestarted(started)
}

// TestNoPrefixNoIPv6: on a host without a prefix, nothing of IPv6 exists:
// no address, no packet socket.
func TestNoPrefixNoIPv6(t *testing.T) {
	w := newWorld(t)
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	m := w.machine("api")
	if m.GetSpec().GetInterfaces()[0].GetIpv6() != "" {
		t.Errorf("machine api has IPv6 %s on a host without a prefix", m.GetSpec().GetInterfaces()[0].GetIpv6())
	}
	snap := w.host.Snapshot()
	if nic := snap.VMs[m.GetMetadata().GetUid()].Spec.Interfaces[0]; nic.IPv6.IsValid() {
		t.Errorf("the VM runs with IPv6 %s", nic.IPv6)
	}
	for uid, l := range snap.Listening {
		if slices.ContainsFunc(l, func(s string) bool { return strings.HasPrefix(s, "packets") }) {
			t.Errorf("network %s has packet sockets: %v", uid, l)
		}
	}
}
