package fake

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"time"

	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

var _ driver.NATFlows = network{}

// PublicIPv4 is the address the fake host's networks leave as.
var PublicIPv4 = netip.MustParseAddr("192.0.2.1")

// natFollower is one FollowNAT's queue of flows, and done, closed once it
// returns.
type natFollower struct {
	flows chan driver.NATFlow
	done  chan struct{}
}

func (d network) FollowNAT(ctx context.Context, h driver.NATHandler) error {
	f := &natFollower{flows: make(chan driver.NATFlow, 64), done: make(chan struct{})}
	d.h.mu.Lock()
	if d.h.netErr != nil {
		defer d.h.mu.Unlock()
		return d.h.netErr
	}
	if d.h.natFollowers == nil {
		d.h.natFollowers = map[*natFollower]bool{}
	}
	d.h.natFollowers[f] = true
	d.h.mu.Unlock()
	defer func() {
		d.h.mu.Lock()
		delete(d.h.natFollowers, f)
		d.h.mu.Unlock()
		close(f.done)
	}()
	h.Started(time.Now())
	for {
		select {
		case <-ctx.Done():
			return nil
		case flow := <-f.flows:
			h.Flow(flow)
		}
	}
}

// Connect has the interface's machine open a TCP connection from srcPort to
// dst out of the host, as both NATs of a real one would carry it, and
// returns the source dst sees. Each FollowNAT is told of it.
func (h *Host) Connect(nic driver.Interface, srcPort uint16, dst netip.AddrPort) (netip.AddrPort, error) {
	h.mu.Lock()
	n, ok := h.networks[nic.NetworkUID]
	if !ok {
		h.mu.Unlock()
		return netip.AddrPort{}, fmt.Errorf("no network %s", nic.NetworkUID)
	}
	if i, ok := h.interfaces[nic.MachineUID][nic.Name]; !ok || i.NetworkUID != nic.NetworkUID || i.Address != nic.Address {
		h.mu.Unlock()
		return netip.AddrPort{}, fmt.Errorf("machine %s has no %s on network %s at %s", nic.MachineUID, nic.Name, nic.NetworkUID, nic.Address)
	}
	h.natPort++
	public := netip.AddrPortFrom(PublicIPv4, 30000+h.natPort%30000)
	flow := driver.NATFlow{
		Time: time.Now(), Protocol: unix.IPPROTO_TCP, Public: public,
		Transit:     netip.AddrPortFrom(n.Transit.Addr().Next(), srcPort),
		Destination: dst, NetworkUID: n.UID,
		Inner: netip.AddrPortFrom(nic.Address.Addr(), srcPort),
	}
	followers := slices.Collect(maps.Keys(h.natFollowers))
	h.mu.Unlock()
	// Sent off the lock: a follower may be busy, and the host's other
	// calls must not wait on it.
	for _, f := range followers {
		select {
		case f.flows <- flow:
		case <-f.done:
		}
	}
	return public, nil
}
