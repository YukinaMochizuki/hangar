package execws

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sys/unix"
)

// ErrPeerGone ends an exec whose other end has gone silent.
var ErrPeerGone = errors.New("the other end of the exec stopped answering")

// maxPings bounds the pings awaiting their pong: an end whose reader is
// held up answers none, and each waits in a goroutine of its own.
const maxPings = 8

// Liveness tells whether the other end of an exec's WebSocket is there:
// silence counts only while nothing is pending either way, as data backed
// up holds pongs up (docs/decisions/node-tunnel.md). Make it with NewLiveness.
type Liveness struct {
	every, timeout time.Duration
	busy           atomic.Int64
	// heard and idleSince are Unix nanoseconds: when the other end last
	// sent anything, and when the last thing pending ended.
	heard, idleSince atomic.Int64
	pings            atomic.Int64
}

// NewLiveness is a Liveness pinging every every, and taking the other end
// to be gone after every and timeout of silence with nothing pending.
func NewLiveness(every, timeout time.Duration) *Liveness {
	l := &Liveness{every: every, timeout: timeout}
	now := time.Now().UnixNano()
	l.heard.Store(now)
	l.idleSince.Store(now)
	return l
}

// Heard records that the other end sent something.
func (l *Liveness) Heard() { l.heard.Store(time.Now().UnixNano()) }

// Pending marks something under way that the other end, or the process,
// can hold up, such as a write to the other end; done ends it.
func (l *Liveness) Pending() (done func()) {
	l.busy.Add(1)
	return func() {
		l.idleSince.Store(time.Now().UnixNano())
		l.busy.Add(-1)
	}
}

// OnPing and OnPong are the WebSocket's callbacks, which count a ping or a
// pong as the other end's.
func (l *Liveness) OnPing(context.Context, []byte) bool { l.Heard(); return true }
func (l *Liveness) OnPong(context.Context, []byte)      { l.Heard() }

// Run pings c until ctx ends, then returns nil, or ErrPeerGone, or why a
// write to it failed. Each tick also writes an empty message, which waits
// on no pong, so a broken connection shows even while pongs are held up.
func (l *Liveness) Run(ctx context.Context, c *websocket.Conn) error {
	ticker := time.NewTicker(l.every)
	defer ticker.Stop()
	ws := context.WithoutCancel(ctx)
	failed := make(chan error, 1)
	fail := func(err error) {
		select {
		case failed <- err:
		default:
		}
	}
	var probing atomic.Bool
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failed:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case <-ticker.C:
		}
		if l.gone(time.Now()) {
			return ErrPeerGone
		}
		if probing.CompareAndSwap(false, true) {
			go func() {
				defer probing.Store(false)
				if err := c.Write(ws, websocket.MessageBinary, nil); err != nil {
					fail(err)
				}
			}()
		}
		// A ping never times out: coder/websocket would close the
		// connection under a write cut short.
		if l.pings.Load() < maxPings {
			l.pings.Add(1)
			go func() {
				defer l.pings.Add(-1)
				if err := c.Ping(ws); err != nil {
					fail(err)
				}
			}()
		}
	}
}

func (l *Liveness) gone(now time.Time) bool {
	window := (l.every + l.timeout).Nanoseconds()
	t := now.UnixNano()
	return l.busy.Load() == 0 && t-l.idleSince.Load() >= window && t-l.heard.Load() >= window
}

// KeepAlive and Control's user timeout drop, within about a minute, a
// peer that stops answering probes or leaves writes unacknowledged.
var KeepAlive = net.KeepAliveConfig{Enable: true, Idle: 30 * time.Second, Interval: 10 * time.Second, Count: 3}

const userTimeout = 60 * time.Second

// Control sets TCP's user timeout on a socket, as net.ListenConfig's and
// net.Dialer's Control take it; a listening socket's accepted sockets
// inherit it.
func Control(_, _ string, rc syscall.RawConn) error {
	var serr error
	err := rc.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(userTimeout.Milliseconds()))
	})
	if err != nil {
		return err
	}
	return serr
}
