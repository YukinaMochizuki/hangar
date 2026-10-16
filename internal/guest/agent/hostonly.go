package agent

import (
	"net"
	"sync"
	"time"

	"github.com/mdlayher/vsock"
)

// RefusalLogInterval is how often at most HostOnly logs refused peers; the
// refusals in between are counted into a line once it has passed, or at Close.
const RefusalLogInterval = time.Minute

// HostOnly wraps a vsock listener so that Accept returns only the host's
// connections; any other peer is closed unread and logged, rate-limited.
// Why: docs/decisions/guest-agent.md.
func HostOnly(ln net.Listener, logf func(format string, args ...any)) net.Listener {
	return &hostOnly{Listener: ln, logf: logf, now: time.Now, afterFunc: time.AfterFunc}
}

type hostOnly struct {
	net.Listener
	logf      func(string, ...any)
	now       func() time.Time
	afterFunc func(time.Duration, func()) *time.Timer

	mu         sync.Mutex
	lastLog    time.Time
	suppressed int
	// flush, while set, logs the suppressed refusals once the interval ends.
	flush *time.Timer
}

func (l *hostOnly) Close() error {
	l.mu.Lock()
	if l.flush != nil {
		l.flush.Stop()
		l.flush = nil
	}
	l.logSuppressed()
	l.mu.Unlock()
	return l.Listener.Close()
}

func (l *hostOnly) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if a, ok := c.RemoteAddr().(*vsock.Addr); ok && a.ContextID == vsock.Host {
			return c, nil
		}
		peer := c.RemoteAddr()
		_ = c.Close()
		l.refused(peer)
	}
}

func (l *hostOnly) refused(peer net.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if !l.lastLog.IsZero() && now.Sub(l.lastLog) < RefusalLogInterval {
		l.suppressed++
		if l.flush == nil {
			l.flush = l.afterFunc(l.lastLog.Add(RefusalLogInterval).Sub(now), func() {
				l.mu.Lock()
				defer l.mu.Unlock()
				l.flush = nil
				l.logSuppressed()
			})
		}
		return
	}
	if l.suppressed > 0 {
		l.logf("hangar-init: guest agent: refused a connection from %v, not the host (and %d more since the last line)", peer, l.suppressed)
	} else {
		l.logf("hangar-init: guest agent: refused a connection from %v, not the host", peer)
	}
	l.lastLog, l.suppressed = now, 0
}

// logSuppressed logs the refusals counted since the last line, if any. l.mu
// is held.
func (l *hostOnly) logSuppressed() {
	if l.suppressed == 0 {
		return
	}
	l.logf("hangar-init: guest agent: refused %d more connections not from the host since the last line", l.suppressed)
	l.lastLog, l.suppressed = l.now(), 0
}
