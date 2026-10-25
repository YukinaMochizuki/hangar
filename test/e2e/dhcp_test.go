package e2e

import (
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"

	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// TestEachNetworkHandsOutItsAddresses: a machine asking its network's DHCP
// is given the address the apiserver assigned its interface, and a MAC the
// network does not know is not answered.
func TestEachNetworkHandsOutItsAddresses(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	m := w.machine("api")
	vm := w.host.Snapshot().VMs[m.GetMetadata().GetUid()]
	mac, err := net.ParseMAC(vm.Spec.Interfaces[0].MAC)
	if err != nil {
		t.Fatal(err)
	}
	want := addrOf(m)

	ask := func(mac net.HardwareAddr) (*dhcpv4.DHCPv4, error) {
		at, ok := w.host.ServiceAddr(w.networkUID("default"), "udp", netip.AddrPortFrom(netip.IPv4Unspecified(), 67))
		if !ok {
			return nil, fmt.Errorf("nothing listens")
		}
		c, err := net.Dial("udp", at.String())
		if err != nil {
			return nil, err
		}
		defer func() { _ = c.Close() }()
		req, err := dhcpv4.NewDiscovery(mac)
		if err != nil {
			return nil, err
		}
		if _, err := c.Write(req.ToBytes()); err != nil {
			return nil, err
		}
		_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		buf := make([]byte, 1500)
		n, err := c.Read(buf)
		if err != nil {
			return nil, err
		}
		return dhcpv4.FromBytes(buf[:n])
	}
	testutil.Eventually(w.t, "api offered "+want, func() (bool, string) {
		offer, err := ask(mac)
		return err == nil && offer.MessageType() == dhcpv4.MessageTypeOffer && offer.YourIPAddr.String() == want &&
			offer.HostName() == "api", fmt.Sprintf("%v %v", offer, err)
	})
	if offer, err := ask(net.HardwareAddr{0x02, 0xde, 0xad, 0xbe, 0xef, 0x01}); err == nil {
		t.Errorf("an unknown MAC was answered: %v", offer)
	}
	w.delete("api")
	w.waitGone("api")
	w.waitHostEmpty()
}
