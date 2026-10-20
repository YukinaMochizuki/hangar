package hostnet

import (
	"fmt"
	"syscall"

	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// portFlags are the flags of a bridge port the driver sets on a tap.
type portFlags struct {
	learning, flood, mcastFlood, bcastFlood bool
}

// tapPort is how a tap's port is set: it neither learns nor takes flooded
// unknown unicast, and takes broadcast and multicast, for ARP, DHCP and
// neighbour discovery.
var tapPort = portFlags{mcastFlood: true, bcastFlood: true}

// portSocket is a netlink socket in a network's namespace for its bridge
// ports' flags, which the netlink package neither reads nor sets whole.
type portSocket struct{ sh *nl.SocketHandle }

func openPortSocket(ns netns.NsHandle) (*portSocket, error) {
	s, err := nl.GetNetlinkSocketAt(ns, netns.None(), unix.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	return &portSocket{sh: &nl.SocketHandle{Socket: s}}, nil
}

func (p *portSocket) Close() { p.sh.Socket.Close() }

func (p *portSocket) request(typ, flags int) *nl.NetlinkRequest {
	req := nl.NewNetlinkRequest(typ, flags)
	req.Sockets = map[int]*nl.SocketHandle{unix.NETLINK_ROUTE: p.sh}
	return req
}

// flags returns the flags of every bridge port in the namespace, by index.
func (p *portSocket) flags() (map[int]portFlags, error) {
	msgs, err := dumped(func() ([][]byte, error) {
		req := p.request(unix.RTM_GETLINK, unix.NLM_F_DUMP)
		req.AddData(nl.NewIfInfomsg(unix.AF_BRIDGE))
		return req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWLINK)
	})
	if err != nil {
		return nil, err
	}
	out := map[int]portFlags{}
	for _, m := range msgs {
		ifi := nl.DeserializeIfInfomsg(m)
		attrs, err := nl.ParseRouteAttr(m[ifi.Len():])
		if err != nil {
			return nil, err
		}
		for _, a := range attrs {
			if a.Attr.Type&^unix.NLA_F_NESTED != unix.IFLA_PROTINFO {
				continue
			}
			infos, err := nl.ParseRouteAttr(a.Value)
			if err != nil {
				return nil, err
			}
			out[int(ifi.Index)] = portFlagsOf(infos)
		}
	}
	return out, nil
}

func portFlagsOf(infos []syscall.NetlinkRouteAttr) portFlags {
	var f portFlags
	for _, i := range infos {
		if len(i.Value) != 1 {
			continue
		}
		on := i.Value[0] != 0
		switch i.Attr.Type {
		case unix.IFLA_BRPORT_LEARNING:
			f.learning = on
		case unix.IFLA_BRPORT_UNICAST_FLOOD:
			f.flood = on
		case unix.IFLA_BRPORT_MCAST_FLOOD:
			f.mcastFlood = on
		case unix.IFLA_BRPORT_BCAST_FLOOD:
			f.bcastFlood = on
		}
	}
	return f
}

// set sets a port's flags in one message.
func (p *portSocket) set(index int, f portFlags) error {
	req := p.request(unix.RTM_SETLINK, unix.NLM_F_ACK)
	msg := nl.NewIfInfomsg(unix.AF_BRIDGE)
	msg.Index = int32(index)
	req.AddData(msg)
	pi := nl.NewRtAttr(unix.IFLA_PROTINFO|unix.NLA_F_NESTED, nil)
	for attr, on := range map[int]bool{
		unix.IFLA_BRPORT_LEARNING: f.learning, unix.IFLA_BRPORT_UNICAST_FLOOD: f.flood,
		unix.IFLA_BRPORT_MCAST_FLOOD: f.mcastFlood, unix.IFLA_BRPORT_BCAST_FLOOD: f.bcastFlood,
	} {
		b := byte(0)
		if on {
			b = 1
		}
		pi.AddRtAttr(attr, []byte{b})
	}
	req.AddData(pi)
	if _, err := req.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		return fmt.Errorf("port flags of link %d: %w", index, err)
	}
	return nil
}
