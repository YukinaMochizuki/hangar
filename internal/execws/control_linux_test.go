package execws

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

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
