package metadata

import (
	"net"
	"net/netip"
	"sync"
)

// perSource lets each address hold at most max connections at once, so no
// machine takes the connections its network's others need at boot. A
// connection past it is closed as it is accepted.
type perSource struct {
	net.Listener
	max int

	mu   sync.Mutex
	open map[netip.Addr]int
}

func limitPerSource(l net.Listener, max int) *perSource {
	return &perSource{Listener: l, max: max, open: map[netip.Addr]int{}}
}

func (l *perSource) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ap, err := netip.ParseAddrPort(c.RemoteAddr().String())
		if err != nil {
			_ = c.Close()
			continue
		}
		addr := ap.Addr().Unmap()
		l.mu.Lock()
		if l.open[addr] >= l.max {
			l.mu.Unlock()
			_ = c.Close()
			continue
		}
		l.open[addr]++
		l.mu.Unlock()
		return &countedConn{Conn: c, release: func() { l.release(addr) }}, nil
	}
}

func (l *perSource) release(addr netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open[addr]--; l.open[addr] <= 0 {
		delete(l.open, addr)
	}
}

type countedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *countedConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}
