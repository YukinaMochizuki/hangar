// Package ra sends each network's router advertisements: the gateway,
// GatewayIPv6, is a default router whose addresses come from DHCPv6
// (managed, M set) and which offers no prefix to configure an address from,
// as clouds do (docs/decisions/networking.md). It sends them now and then
// and when a machine solicits one, never more often than RFC 4861 lets a
// router answer solicitations, however many arrive.
package ra

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/ipv6pkt"
)

// Timing of what is sent. RouterLifetime outlives several missed
// advertisements; MinInterval is RFC 4861's MIN_DELAY_BETWEEN_RAS.
const (
	RouterLifetime = 30 * time.Minute
	Interval       = 200 * time.Second
	MinInterval    = 3 * time.Second
	// maxResponseDelay is RFC 4861's MAX_RA_DELAY_TIME.
	maxResponseDelay = 500 * time.Millisecond
)

// allNodes is where advertisements go, and its MAC.
var (
	allNodes    = netip.MustParseAddr("ff02::1")
	allNodesMAC = net.HardwareAddr{0x33, 0x33, 0, 0, 0, 1}
)

// Message is the router advertisement every network's gateway sends: hop
// limit 64, managed, router lifetime RouterLifetime, and no options.
func Message() []byte {
	m := make([]byte, 16)
	m[0] = 134 // router advertisement
	m[4] = 64
	m[5] = 0x80 // M: addresses by DHCPv6
	binary.BigEndian.PutUint16(m[6:8], uint16(RouterLifetime/time.Second))
	return m
}

// Packet is Message from the gateway to every node of the link.
func Packet() []byte {
	return ipv6pkt.Packet{
		Src: driver.GatewayIPv6, Dst: allNodes, Next: ipv6pkt.ICMPv6, HopLimit: 255, Payload: Message(),
	}.Marshal()
}

// Serve advertises the gateway on c until ctx is done or c fails.
func Serve(ctx context.Context, c driver.PacketConn, log *slog.Logger) error {
	solicited := make(chan struct{}, 1)
	errc := make(chan error, 1)
	go func() { errc <- readSolicitations(ctx, c, solicited) }()
	var last time.Time
	send := func() {
		if err := c.WritePacket(Packet(), allNodesMAC); err != nil {
			log.Debug("sending a router advertisement failed", "error", err)
		}
		last = time.Now()
	}
	send()
	periodic := time.NewTimer(jitter(Interval))
	defer periodic.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			return err
		case <-periodic.C:
			send()
			periodic.Reset(jitter(Interval))
		case <-solicited:
			// Answered by one multicast advertisement, held back so that
			// solicitations arriving together get one answer between them.
			wait := max(time.Until(last.Add(MinInterval)), rand.N(maxResponseDelay))
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(wait):
			}
			select {
			case <-solicited: // asked again meanwhile: answered by this one
			default:
			}
			send()
		}
	}
}

// readSolicitations signals solicited for each valid router solicitation c
// reads, until ctx is done.
func readSolicitations(ctx context.Context, c driver.PacketConn, solicited chan<- struct{}) error {
	stop := context.AfterFunc(ctx, func() { _ = c.SetReadDeadline(time.Unix(1, 0)) })
	defer stop()
	buf := make([]byte, 1500)
	for {
		n, _, err := c.ReadPacket(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if IsSolicitation(buf[:n]) {
			select {
			case solicited <- struct{}{}:
			default:
			}
		}
	}
}

// IsSolicitation says whether b is a router solicitation as RFC 4861 has a
// router take one: ICMPv6 type 133, code 0, hop limit 255.
func IsSolicitation(b []byte) bool {
	p, err := ipv6pkt.Parse(b)
	return err == nil && p.Next == ipv6pkt.ICMPv6 && p.HopLimit == 255 && len(p.Payload) >= 8 &&
		p.Payload[0] == 133 && p.Payload[1] == 0
}

// jitter is d give or take a quarter, so networks' advertisements spread.
func jitter(d time.Duration) time.Duration {
	return d*3/4 + rand.N(d/2)
}
