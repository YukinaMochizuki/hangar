package execws

import (
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestSilenceCountsOnlyWithNothingPending: the other end is gone once it
// has said nothing, and nothing has been pending, for a whole window.
func TestSilenceCountsOnlyWithNothingPending(t *testing.T) {
	l := NewLiveness(100*time.Millisecond, 300*time.Millisecond)
	t0 := time.Now()
	if l.gone(t0.Add(399 * time.Millisecond)) {
		t.Error("gone before a window of silence")
	}
	if !l.gone(t0.Add(401 * time.Millisecond)) {
		t.Error("not gone after a window of silence")
	}
	done := l.Pending()
	if l.gone(t0.Add(time.Hour)) {
		t.Error("gone with something pending")
	}
	done()
	if l.gone(time.Now().Add(300 * time.Millisecond)) {
		t.Error("gone less than a window after the pending ended")
	}
	l.Heard()
	l.idleSince.Store(0)
	if l.gone(time.Now().Add(300 * time.Millisecond)) {
		t.Error("gone less than a window after the other end was heard")
	}
}

// TestAcceptedSocketsTimeOut: a listener made with KeepAlive and Control
// accepts sockets with both.
func TestAcceptedSocketsTimeOut(t *testing.T) {
	ln, err := (&net.ListenConfig{KeepAliveConfig: KeepAlive, Control: Control}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	a, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	rc, err := a.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var timeout, keepalive, idle int
	var serr error
	_ = rc.Control(func(fd uintptr) {
		if timeout, serr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT); serr != nil {
			return
		}
		if keepalive, serr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_KEEPALIVE); serr != nil {
			return
		}
		idle, serr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_KEEPIDLE)
	})
	if serr != nil {
		t.Fatal(serr)
	}
	if timeout != 60000 || keepalive != 1 || idle != 30 {
		t.Errorf("user timeout %d ms, keepalive %d, idle %d s", timeout, keepalive, idle)
	}
}
