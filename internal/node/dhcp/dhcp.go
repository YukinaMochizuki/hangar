// Package dhcp hands each machine of a network the address the apiserver
// assigned it, as clouds do, so an image that configures itself by DHCP,
// as cloud images do, gets the right one (docs/decisions/networking.md).
// It only ever answers with an assigned address: a MAC it does not know is
// not answered, and there is no pool. Container guests never ask; their
// network comes on their boot disk.
package dhcp

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// LeaseTime is what every lease says. An address never changes while its
// machine has it, so a long lease only spares renewals.
const LeaseTime = 24 * time.Hour

// Lease is one machine interface's address.
type Lease struct {
	// Addr is the address with the network's prefix length.
	Addr     netip.Prefix
	Hostname string
}

// Leases are one network's: its gateway, which is also its resolver, the
// search domains it hands out, and a lease per MAC.
type Leases struct {
	Gateway netip.Addr
	Search  []string
	// ByMAC is keyed by MAC in lower case, such as 02:00:00:00:00:01.
	ByMAC map[string]Lease
}

// Server answers the networks' DHCP requests. It is safe for concurrent use.
type Server struct {
	log *slog.Logger

	mu   sync.RWMutex
	nets map[string]Leases // by network uid
}

// New returns a server knowing no network.
func New(log *slog.Logger) *Server {
	return &Server{log: log, nets: map[string]Leases{}}
}

// Set replaces a network's leases.
func (s *Server) Set(networkUID string, l Leases) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nets[networkUID] = l
}

// Forget drops a network's leases.
func (s *Server) Forget(networkUID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nets, networkUID)
}

// broadcast is where a reply goes to a client that has no address yet.
var broadcast = &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpv4.ClientPort}

// Serve answers a network's requests on c until ctx is done or c is closed.
func (s *Server) Serve(ctx context.Context, networkUID string, c *net.UDPConn) error {
	stop := context.AfterFunc(ctx, func() { _ = c.SetReadDeadline(time.Unix(1, 0)) })
	defer stop()
	buf := make([]byte, 1500)
	for {
		n, peer, err := c.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		req, err := dhcpv4.FromBytes(buf[:n])
		if err != nil {
			continue // not DHCP: nothing to say
		}
		reply := s.Reply(networkUID, req)
		if reply == nil {
			continue
		}
		to := broadcast
		if !peer.IP.IsUnspecified() && !peer.IP.Equal(net.IPv4bcast) {
			// A client renewing asks from its address, and is answered
			// there.
			to = peer
		}
		if _, err := c.WriteToUDP(reply.ToBytes(), to); err != nil {
			s.log.Debug("answering a DHCP request failed", "network_uid", networkUID, "error", err)
		}
	}
}

// Reply is the answer to req on a network, or nil for none: a request of a
// MAC the network does not know, of a network not yet known, or one that
// takes no answer, such as a release.
func (s *Server) Reply(networkUID string, req *dhcpv4.DHCPv4) *dhcpv4.DHCPv4 {
	if req.OpCode != dhcpv4.OpcodeBootRequest || len(req.ClientHWAddr) != 6 {
		return nil
	}
	s.mu.RLock()
	leases, ok := s.nets[networkUID]
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	lease, ok := leases.ByMAC[strings.ToLower(req.ClientHWAddr.String())]
	if !ok {
		return nil
	}
	var answer dhcpv4.MessageType
	switch req.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		answer = dhcpv4.MessageTypeOffer
	case dhcpv4.MessageTypeRequest:
		answer = dhcpv4.MessageTypeAck
		if asked := requested(req); asked.IsValid() && asked != lease.Addr.Addr() {
			answer = dhcpv4.MessageTypeNak
		}
	case dhcpv4.MessageTypeInform:
		answer = dhcpv4.MessageTypeAck
	default:
		return nil
	}
	gw := net.IP(leases.Gateway.AsSlice())
	mods := []dhcpv4.Modifier{dhcpv4.WithMessageType(answer), dhcpv4.WithServerIP(gw),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(gw))}
	if answer != dhcpv4.MessageTypeNak {
		mods = append(mods,
			dhcpv4.WithNetmask(net.CIDRMask(lease.Addr.Bits(), 32)),
			dhcpv4.WithRouter(gw),
			dhcpv4.WithDNS(gw),
		)
		if len(leases.Search) > 0 {
			mods = append(mods, dhcpv4.WithDomainSearchList(leases.Search...))
		}
		if lease.Hostname != "" {
			mods = append(mods, dhcpv4.WithOption(dhcpv4.OptHostName(lease.Hostname)))
		}
		if req.MessageType() != dhcpv4.MessageTypeInform {
			mods = append(mods, dhcpv4.WithYourIP(lease.Addr.Addr().AsSlice()), dhcpv4.WithLeaseTime(uint32(LeaseTime/time.Second)))
		}
	}
	reply, err := dhcpv4.NewReplyFromRequest(req, mods...)
	if err != nil {
		s.log.Debug("making a DHCP reply failed", "network_uid", networkUID, "error", err)
		return nil
	}
	return reply
}

// requested is the address a request asks for: its requested-address
// option, or else the address it says it has.
func requested(req *dhcpv4.DHCPv4) netip.Addr {
	ip := req.RequestedIPAddress()
	if ip == nil || ip.IsUnspecified() {
		ip = req.ClientIPAddr
	}
	a, ok := netip.AddrFromSlice(ip.To4())
	if !ok || a.IsUnspecified() {
		return netip.Addr{}
	}
	return a
}
