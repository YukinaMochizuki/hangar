// Package netsvc runs the services the node serves each of its networks,
// such as the network's resolver, on sockets the network driver opens inside
// the network's namespace (docs/decisions/networking.md). Sockets are kept
// by network uid and live only as long as the agent: there is nothing of
// them on the host to read back, and a socket whose namespace was made again
// serves nothing, so each Ensure checks the namespace's instance and opens
// the sockets again when it changed.
package netsvc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// Network is a network as its services see it.
type Network struct {
	UID string
	// Gateway is the network's first address, which its machines route
	// through and the node's services answer on.
	Gateway netip.Addr
	// IPv6 says the network's machines have public IPv6 addresses, routed
	// through driver.GatewayIPv6, so its IPv6 services are served.
	IPv6 bool
}

// Service is one service the node serves every network, on UDP, TCP or
// both, or every network with IPv6, on IPv6 packets of one kind.
// A Serve returns soon after its ctx is done, whatever its clients hold
// open, as stopping a network's services waits for it; its socket is closed
// then.
type Service struct {
	Name string
	// Addr is where the service listens in a network.
	Addr func(n Network) netip.AddrPort
	// ServeUDP and ServeTCP serve one network's socket. Either may be nil.
	ServeUDP func(ctx context.Context, n Network, c *net.UDPConn)
	ServeTCP func(ctx context.Context, n Network, l *net.TCPListener)
	// ServePackets, with Packets the kind it serves, serves a network with
	// IPv6; such a service has no Addr.
	Packets      driver.PacketKind
	ServePackets func(ctx context.Context, n Network, c driver.PacketConn)
}

// Manager keeps every service listening on each network it is told of.
type Manager struct {
	net      driver.Network
	services []Service
	log      *slog.Logger

	mu   sync.Mutex
	nets map[string]*serving
}

// serving is one network's sockets and what serves them.
type serving struct {
	network  Network
	instance string
	sockets  []io.Closer
	stop     context.CancelFunc
	wg       sync.WaitGroup
}

// New returns a manager opening sockets through net.
func New(net driver.Network, log *slog.Logger, services ...Service) *Manager {
	return &Manager{net: net, services: services, log: log, nets: map[string]*serving{}}
}

// Ensure has every service listen on n, in the namespace n has now. A
// network not on the host has nothing listening on it.
func (m *Manager) Ensure(ctx context.Context, n Network) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	instance, err := m.net.Instance(ctx, n.UID)
	if err != nil {
		return fmt.Errorf("network %s: %w", n.UID, err)
	}
	s := m.nets[n.UID]
	if s != nil && s.instance == instance && s.network == n {
		return nil
	}
	m.stopLocked(n.UID)
	if instance == "" {
		return nil
	}
	s, err = m.open(ctx, n)
	if err != nil {
		return fmt.Errorf("network %s: %w", n.UID, err)
	}
	m.nets[n.UID] = s
	return nil
}

// open opens every service's sockets on n, all in one instance of its
// namespace, and starts serving them.
func (m *Manager) open(ctx context.Context, n Network) (*serving, error) {
	serveCtx, stop := context.WithCancel(context.Background())
	s := &serving{network: n, stop: stop}
	type start func()
	var starts []start
	opened := func(instance string, c io.Closer, serve start) error {
		s.sockets = append(s.sockets, c)
		if s.instance == "" {
			s.instance = instance
		} else if instance != s.instance {
			return errors.New("its namespace was made again while its sockets were opened")
		}
		starts = append(starts, serve)
		return nil
	}
	for _, svc := range m.services {
		if svc.ServePackets != nil {
			if !n.IPv6 {
				continue
			}
			c, instance, err := m.net.ListenPackets(ctx, n.UID, svc.Packets)
			if err == nil {
				err = opened(instance, c, func() { svc.ServePackets(serveCtx, n, c) })
			}
			if err != nil {
				stop()
				closeAll(s.sockets)
				return nil, fmt.Errorf("%s: %w", svc.Name, err)
			}
			continue
		}
		addr := svc.Addr(n)
		var err error
		if svc.ServeUDP != nil {
			var c *net.UDPConn
			var instance string
			if c, instance, err = m.net.ListenUDP(ctx, n.UID, addr); err == nil {
				err = opened(instance, c, func() { svc.ServeUDP(serveCtx, n, c) })
			}
		}
		if err == nil && svc.ServeTCP != nil {
			var l *net.TCPListener
			var instance string
			if l, instance, err = m.net.ListenTCP(ctx, n.UID, addr); err == nil {
				err = opened(instance, l, func() { svc.ServeTCP(serveCtx, n, l) })
			}
		}
		if err != nil {
			stop()
			closeAll(s.sockets)
			return nil, fmt.Errorf("%s on %s: %w", svc.Name, addr, err)
		}
	}
	for _, serve := range starts {
		s.wg.Go(serve)
	}
	m.log.Info("serving a network", "network_uid", n.UID, "instance", s.instance)
	return s, nil
}

// Close stops serving a network.
func (m *Manager) Close(networkUID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked(networkUID)
}

// Keep stops serving every network keep says no to.
func (m *Manager) Keep(keep func(networkUID string) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for uid := range m.nets {
		if !keep(uid) {
			m.stopLocked(uid)
		}
	}
}

// CloseAll stops serving every network, as the agent ends.
func (m *Manager) CloseAll() { m.Keep(func(string) bool { return false }) }

// Serving lists the networks served.
func (m *Manager) Serving() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	uids := make([]string, 0, len(m.nets))
	for uid := range m.nets {
		uids = append(uids, uid)
	}
	slices.Sort(uids)
	return uids
}

func (m *Manager) stopLocked(uid string) {
	s := m.nets[uid]
	if s == nil {
		return
	}
	delete(m.nets, uid)
	s.stop()
	closeAll(s.sockets)
	s.wg.Wait()
	m.log.Info("no longer serving a network", "network_uid", uid, "instance", s.instance)
}

func closeAll(cs []io.Closer) {
	for _, c := range cs {
		_ = c.Close()
	}
}
