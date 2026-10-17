package driver

import (
	"context"
	"net/netip"
	"time"
)

// NATFlows is a Network that reports how each new IPv4 connection out of
// its networks left the host: the public source it took, joined with the
// machine address it came from (docs/decisions/networking.md, "Abuse
// protection").
type NATFlows interface {
	// FollowNAT reports to h until ctx ends or following fails. It calls
	// h's functions from one goroutine, in order: Started once, when no
	// connection can go unreported any more but within a Lost window.
	FollowNAT(ctx context.Context, h NATHandler) error
}

// NATHandler is what FollowNAT reports to.
type NATHandler struct {
	Started func(at time.Time)
	Flow    func(NATFlow)
	Lost    func(NATLost)
}

// NATFlow is one new IPv4 connection out of a network, as both NATs made
// it. A port is zero for a protocol without ports, such as ICMP.
type NATFlow struct {
	// Time is when the host reported the connection started.
	Time     time.Time
	Protocol uint8
	// Public is the source the internet sees.
	Public netip.AddrPort
	// Transit is the source after the network's NAT, as the host sees it.
	Transit     netip.AddrPort
	Destination netip.AddrPort
	// NetworkUID is the network the connection came from; "" if unknown.
	NetworkUID string
	// Inner is the machine's source; invalid if unknown.
	Inner netip.AddrPort
}

// NATLost is a window in which connections may have gone unreported.
type NATLost struct {
	From, To time.Time
	Reason   string
}
