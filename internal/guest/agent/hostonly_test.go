package agent

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/mdlayher/vsock"
)

// peerConn is an accepted connection from a peer; reading from it is a
// failure, since a refused peer must be closed unread.
type peerConn struct {
	net.Conn
	remote net.Addr
	closed bool
	read   bool
}

func (c *peerConn) RemoteAddr() net.Addr { return c.remote }
func (c *peerConn) Close() error         { c.closed = true; return nil }
func (c *peerConn) Read([]byte) (int, error) {
	c.read = true
	return 0, errors.New("read")
}

// queueListener hands out its conns in order, then errClosed.
type queueListener struct {
	net.Listener
	conns []*peerConn
}

var errClosed = errors.New("listener closed")

func (l *queueListener) Close() error { return nil }

func (l *queueListener) Accept() (net.Conn, error) {
	if len(l.conns) == 0 {
		return nil, errClosed
	}
	c := l.conns[0]
	l.conns = l.conns[1:]
	return c, nil
}

func TestHostOnlyServesTheHostAlone(t *testing.T) {
	peers := []net.Addr{
		&vsock.Addr{ContextID: vsock.Local, Port: 50000},  // loopback
		&vsock.Addr{ContextID: 3, Port: 50001},            // the guest's own CID
		&vsock.Addr{ContextID: vsock.Hypervisor, Port: 9}, // CID 0
		&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, // not vsock at all
		nil,
		&vsock.Addr{ContextID: vsock.Host, Port: 1234},
		&vsock.Addr{ContextID: 4, Port: 50002},
	}
	ql := &queueListener{}
	for _, p := range peers {
		ql.conns = append(ql.conns, &peerConn{remote: p})
	}
	all := append([]*peerConn(nil), ql.conns...)
	var lines []string
	ln := HostOnly(ql, func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })

	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if c != all[5] {
		t.Fatalf("accepted %v, want the host's connection", c.RemoteAddr())
	}
	if _, err := ln.Accept(); !errors.Is(err, errClosed) {
		t.Fatalf("after the last peer: %v, want the listener's error", err)
	}
	for i, pc := range all {
		if i == 5 {
			if pc.closed {
				t.Error("the host's connection was closed")
			}
			continue
		}
		if !pc.closed {
			t.Errorf("the connection from %v was left open", pc.remote)
		}
		if pc.read {
			t.Errorf("the connection from %v was read from", pc.remote)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "refused") {
		t.Errorf("logged %q, want one line on the first refusal", lines)
	}
}

func TestHostOnlyLogsRefusalsRateLimited(t *testing.T) {
	ql := &queueListener{}
	ln := HostOnly(ql, nil).(*hostOnly)
	var lines []string
	ln.logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	clock := time.Unix(1000, 0)
	ln.now = func() time.Time { return clock }
	// The flushes armed, fired by hand.
	var armed []func()
	var delays []time.Duration
	ln.afterFunc = func(d time.Duration, f func()) *time.Timer {
		armed, delays = append(armed, f), append(delays, d)
		return time.NewTimer(time.Hour)
	}
	refuse := func(n int) {
		for range n {
			ql.conns = append(ql.conns, &peerConn{remote: &vsock.Addr{ContextID: 3, Port: 50000}})
		}
		if _, err := ln.Accept(); !errors.Is(err, errClosed) {
			t.Fatalf("Accept: %v", err)
		}
	}

	refuse(1000)
	if len(lines) != 1 {
		t.Fatalf("a burst of 1000 logged %d lines, want 1: %q", len(lines), lines)
	}
	if len(armed) != 1 || delays[0] != RefusalLogInterval {
		t.Fatalf("flushes armed after the burst: %v, want one in %v", delays, RefusalLogInterval)
	}
	clock = clock.Add(RefusalLogInterval / 2)
	refuse(10)
	if len(lines) != 1 || len(armed) != 1 {
		t.Fatalf("within the interval: %d lines and %d flushes, want still 1 and 1", len(lines), len(armed))
	}
	clock = clock.Add(RefusalLogInterval / 2)
	refuse(1)
	if len(lines) != 2 || !strings.Contains(lines[1], "and 1009 more") {
		t.Fatalf("after the interval: %q, want a second line counting the 1009 in between", lines)
	}
	armed[0]() // nothing left to count
	if len(lines) != 2 {
		t.Fatalf("a flush with nothing suppressed logged %q", lines[2:])
	}

	clock = clock.Add(2 * RefusalLogInterval)
	refuse(1)
	if len(lines) != 3 || strings.Contains(lines[2], "more") {
		t.Fatalf("after a quiet interval: %q, want a plain third line", lines)
	}
	// A burst followed by quiet is still counted, once the interval ends.
	refuse(5)
	if len(armed) != 2 {
		t.Fatalf("no flush armed for the second burst")
	}
	clock = clock.Add(RefusalLogInterval)
	armed[1]()
	if len(lines) != 4 || !strings.Contains(lines[3], "refused 5 more") {
		t.Fatalf("the flush logged %q, want the 5 counted", lines[3:])
	}
	// And at Close, within the interval of the flush's line.
	refuse(4)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 5 || !strings.Contains(lines[4], "refused 4 more") {
		t.Errorf("Close logged %q, want the 4 counted", lines[4:])
	}
}
