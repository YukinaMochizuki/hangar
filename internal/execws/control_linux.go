package execws

import (
	"syscall"

	"golang.org/x/sys/unix"
)

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
