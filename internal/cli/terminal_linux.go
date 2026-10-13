package cli

import "golang.org/x/sys/unix"

// The ioctls that read and set a terminal's termios.
const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)
