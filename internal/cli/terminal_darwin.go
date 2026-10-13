package cli

import "golang.org/x/sys/unix"

// The ioctls that read and set a terminal's termios.
const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)
