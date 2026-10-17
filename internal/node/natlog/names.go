package natlog

import (
	"net/netip"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// Names is what the log names connections by: each network on the host by
// its uid, as the agent last read it from the API. It is safe for
// concurrent use.
type Names struct {
	mu sync.Mutex
	by map[string]Network
}

// Network is a network's names and its machines by their address on it.
type Network struct {
	Project, Name string
	Machines      map[netip.Addr]Machine
}

// Machine is a machine's name and uid.
type Machine struct{ Name, UID string }

// NewNames returns Names knowing no network.
func NewNames() *Names { return &Names{by: map[string]Network{}} }

// Set names network uid's connections after net.
func (n *Names) Set(uid string, net Network) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.by[uid] = net
}

// Forget forgets network uid.
func (n *Names) Forget(uid string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.by, uid)
}

// Record is the log's line for f, named as the networks are now; what is
// not known is Unknown, never left out.
func (n *Names) Record(f driver.NATFlow) Record {
	r := Record{
		Time: f.Time, Event: EventNew, Protocol: protocolName(f.Protocol),
		PublicIP: f.Public.Addr().String(), PublicPort: f.Public.Port(),
		DstIP: f.Destination.Addr().String(), DstPort: f.Destination.Port(),
		TransitIP: f.Transit.Addr().String(), TransitPort: f.Transit.Port(),
		Project: Unknown, Network: Unknown, NetworkUID: Unknown,
		Machine: Unknown, MachineUID: Unknown, InnerIP: Unknown,
	}
	if f.NetworkUID != "" {
		r.NetworkUID = f.NetworkUID
	}
	if f.Inner.IsValid() {
		r.InnerIP, r.InnerPort = f.Inner.Addr().String(), f.Inner.Port()
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	net, ok := n.by[f.NetworkUID]
	if !ok {
		return r
	}
	r.Project, r.Network = net.Project, net.Name
	if m, ok := net.Machines[f.Inner.Addr()]; ok && f.Inner.IsValid() {
		r.Machine, r.MachineUID = m.Name, m.UID
	}
	return r
}

func protocolName(p uint8) string {
	switch p {
	case unix.IPPROTO_TCP:
		return "tcp"
	case unix.IPPROTO_UDP:
		return "udp"
	case unix.IPPROTO_ICMP:
		return "icmp"
	case unix.IPPROTO_UDPLITE:
		return "udplite"
	case unix.IPPROTO_SCTP:
		return "sctp"
	case unix.IPPROTO_DCCP:
		return "dccp"
	}
	return strconv.Itoa(int(p))
}
