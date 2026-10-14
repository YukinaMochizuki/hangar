// Package dhcp6 leases each machine interface of a network its public IPv6
// address, the /128 the apiserver assigned it, by DHCPv6 (IA_NA), as clouds
// do, for images that configure themselves by router advertisement and
// DHCPv6 (docs/decisions/networking.md). A client is known by the MAC its
// frames come from, which the interface's binding makes its own, not by its
// DUID: a MAC the network does not know is not answered, and there is no
// pool. Leases carry the address only; DNS stays the network's IPv4
// resolver.
package dhcp6

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/iana"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/ipv6pkt"
)

// LeaseTime is each address's valid and preferred lifetime. An address
// never changes while its machine has it, so a long lease only spares
// renewals.
const LeaseTime = 24 * time.Hour

// Bounds on the requests answered in a second, by one MAC and by one
// network, so no machine takes the server from the others.
const (
	PerMAC     = 10
	PerNetwork = 100
)

// Server answers the networks' DHCPv6 requests. It is safe for concurrent
// use.
type Server struct {
	log *slog.Logger
	now func() time.Time

	mu   sync.RWMutex
	nets map[string]map[string]netip.Addr // by network uid, then MAC
}

// New returns a server knowing no network.
func New(log *slog.Logger) *Server {
	return &Server{log: log, now: time.Now, nets: map[string]map[string]netip.Addr{}}
}

// Set replaces a network's leases: by MAC, in lower case such as
// 02:00:00:00:00:01, each interface's IPv6 address.
func (s *Server) Set(networkUID string, byMAC map[string]netip.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nets[networkUID] = byMAC
}

// Forget drops a network's leases.
func (s *Server) Forget(networkUID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nets, networkUID)
}

// Serve answers a network's requests on c until ctx is done or c is closed.
func (s *Server) Serve(ctx context.Context, networkUID string, c driver.PacketConn) error {
	stop := context.AfterFunc(ctx, func() { _ = c.SetReadDeadline(time.Unix(1, 0)) })
	defer stop()
	lim := newLimiter(s.now)
	buf := make([]byte, 1500)
	for {
		n, from, err := c.ReadPacket(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		p, err := ipv6pkt.Parse(buf[:n])
		if err != nil || p.Next != ipv6pkt.UDP || port(p.Payload[2:4]) != dhcpv6.DefaultServerPort {
			continue
		}
		mac := strings.ToLower(from.String())
		if !s.known(networkUID, mac) || !lim.allow(mac) {
			continue
		}
		req, err := dhcpv6.MessageFromBytes(p.Payload[8:])
		if err != nil {
			continue
		}
		reply := s.Reply(networkUID, from, req)
		if reply == nil {
			continue
		}
		out := ipv6pkt.Packet{
			Src: driver.GatewayIPv6, Dst: p.Src, Next: ipv6pkt.UDP, HopLimit: 64,
			Payload: ipv6pkt.UDPDatagram(dhcpv6.DefaultServerPort, port(p.Payload[0:2]), reply.ToBytes()),
		}
		if err := c.WritePacket(out.Marshal(), from); err != nil {
			s.log.Debug("answering a DHCPv6 request failed", "network_uid", networkUID, "error", err)
		}
	}
}

func port(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }

func (s *Server) known(networkUID, mac string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.nets[networkUID][mac]
	return ok
}

// ServerID is the network's server identifier: a DUID-LL of a MAC derived
// from the network's uid, the same whenever the server starts.
func ServerID(networkUID string) dhcpv6.DUID {
	sum := sha256.Sum256([]byte("dhcp6/" + networkUID))
	return &dhcpv6.DUIDLL{HWType: iana.HWTypeEthernet, LinkLayerAddr: net.HardwareAddr{0x02, sum[0], sum[1], sum[2], sum[3], sum[4]}}
}

// Reply is the answer to req from the MAC from on a network, or nil for
// none: a request of a MAC the network does not know, one for another
// server, or one of a kind a server never answers.
func (s *Server) Reply(networkUID string, from net.HardwareAddr, req *dhcpv6.Message) *dhcpv6.Message {
	s.mu.RLock()
	addr, ok := s.nets[networkUID][strings.ToLower(from.String())]
	s.mu.RUnlock()
	if !ok || req.Options.ClientID() == nil {
		return nil
	}
	sid := ServerID(networkUID)
	asked := req.Options.ServerID()
	var reply *dhcpv6.Message
	var err error
	switch req.Type() {
	case dhcpv6.MessageTypeSolicit:
		if asked != nil {
			return nil
		}
		if req.GetOneOption(dhcpv6.OptionRapidCommit) != nil {
			reply, err = dhcpv6.NewReplyFromMessage(req)
		} else {
			reply, err = dhcpv6.NewAdvertiseFromSolicit(req)
		}
		if err == nil {
			addIANA(reply, req, addr)
		}
	case dhcpv6.MessageTypeRequest, dhcpv6.MessageTypeRenew:
		if asked == nil || !asked.Equal(sid) {
			return nil
		}
		if reply, err = dhcpv6.NewReplyFromMessage(req); err == nil {
			addIANA(reply, req, addr)
		}
	case dhcpv6.MessageTypeRebind:
		if asked != nil {
			return nil
		}
		if reply, err = dhcpv6.NewReplyFromMessage(req); err == nil {
			addIANA(reply, req, addr)
		}
	case dhcpv6.MessageTypeConfirm:
		if reply, err = dhcpv6.NewReplyFromMessage(req); err == nil {
			code := iana.StatusSuccess
			for _, ia := range req.Options.IANA() {
				for _, a := range ia.Options.Addresses() {
					if got, _ := netip.AddrFromSlice(a.IPv6Addr); got.Unmap() != addr {
						code = iana.StatusNotOnLink
					}
				}
			}
			reply.AddOption(&dhcpv6.OptStatusCode{StatusCode: code})
		}
	case dhcpv6.MessageTypeRelease, dhcpv6.MessageTypeDecline, dhcpv6.MessageTypeInformationRequest:
		if asked != nil && !asked.Equal(sid) {
			return nil
		}
		reply, err = dhcpv6.NewReplyFromMessage(req)
	default:
		return nil
	}
	if err != nil {
		s.log.Debug("making a DHCPv6 reply failed", "network_uid", networkUID, "error", err)
		return nil
	}
	reply.AddOption(dhcpv6.OptServerID(sid))
	return reply
}

// addIANA answers the request's first IA_NA with the address and every
// other with none; a request with no IA_NA is told none is available.
func addIANA(reply, req *dhcpv6.Message, addr netip.Addr) {
	ias := req.Options.IANA()
	if len(ias) == 0 {
		reply.AddOption(&dhcpv6.OptStatusCode{StatusCode: iana.StatusNoAddrsAvail})
		return
	}
	for i, ia := range ias {
		out := &dhcpv6.OptIANA{IaId: ia.IaId}
		if i == 0 {
			out.T1, out.T2 = LeaseTime/2, LeaseTime*4/5
			out.Options.Add(&dhcpv6.OptIAAddress{IPv6Addr: addr.AsSlice(), PreferredLifetime: LeaseTime, ValidLifetime: LeaseTime})
		} else {
			out.Options.Add(&dhcpv6.OptStatusCode{StatusCode: iana.StatusNoAddrsAvail})
		}
		reply.AddOption(out)
	}
}

// limiter bounds the requests taken in each second, by MAC and in all.
type limiter struct {
	now    func() time.Time
	second int64
	all    int
	byMAC  map[string]int
}

func newLimiter(now func() time.Time) *limiter { return &limiter{now: now, byMAC: map[string]int{}} }

func (l *limiter) allow(mac string) bool {
	if s := l.now().Unix(); s != l.second {
		l.second, l.all = s, 0
		clear(l.byMAC)
	}
	if l.all >= PerNetwork || l.byMAC[mac] >= PerMAC {
		return false
	}
	l.all++
	l.byMAC[mac]++
	return true
}
