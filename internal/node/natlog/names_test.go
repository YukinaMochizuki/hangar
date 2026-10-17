package natlog

import (
	"net/netip"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// TestNamesNameEveryField: a connection is named with its project, network
// and machine; what is not known is Unknown, never left out.
func TestNamesNameEveryField(t *testing.T) {
	n := NewNames()
	n.Set("net-uid", Network{Project: "p", Name: "default", Machines: map[netip.Addr]Machine{
		netip.MustParseAddr("10.0.0.2"): {Name: "web", UID: "web-uid"},
	}})
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	flow := func(network, inner string) driver.NATFlow {
		f := driver.NATFlow{
			Time: at, Protocol: unix.IPPROTO_TCP,
			Public:      netip.MustParseAddrPort("192.0.2.1:40001"),
			Transit:     netip.MustParseAddrPort("100.64.0.1:40000"),
			Destination: netip.MustParseAddrPort("198.51.100.7:25"),
			NetworkUID:  network,
		}
		if inner != "" {
			f.Inner = netip.MustParseAddrPort(inner)
		}
		return f
	}
	want := Record{
		Time: at, Event: EventNew, Protocol: "tcp", PublicIP: "192.0.2.1", PublicPort: 40001, DstIP: "198.51.100.7", DstPort: 25,
		Project: "p", Network: "default", NetworkUID: "net-uid", Machine: "web", MachineUID: "web-uid",
		InnerIP: "10.0.0.2", InnerPort: 51000, TransitIP: "100.64.0.1", TransitPort: 40000,
	}
	if got := n.Record(flow("net-uid", "10.0.0.2:51000")); got != want {
		t.Errorf("named %+v, want %+v", got, want)
	}
	if got := n.Record(flow("", "")); got.Project != Unknown || got.Network != Unknown || got.NetworkUID != Unknown ||
		got.Machine != Unknown || got.MachineUID != Unknown || got.InnerIP != Unknown {
		t.Errorf("a connection of no known network is named %+v", got)
	}
	if got := n.Record(flow("net-uid", "")); got.Network != "default" || got.Machine != Unknown || got.InnerIP != Unknown {
		t.Errorf("a connection of no known machine is named %+v", got)
	}
	n.Forget("net-uid")
	if got := n.Record(flow("net-uid", "10.0.0.2:51000")); got.Project != Unknown || got.NetworkUID != "net-uid" || got.Machine != Unknown {
		t.Errorf("a connection of a network forgotten is named %+v", got)
	}
}
