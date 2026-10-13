package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// Terminal is the local terminal an exec with -t runs on.
type Terminal interface {
	// MakeRaw passes every key through as it is typed, Ctrl-C included,
	// and echoes nothing; restore puts the terminal back as it was.
	MakeRaw() (restore func(), err error)
	// Size is the terminal's rows and columns.
	Size() (rows, cols uint16, err error)
}

// terminalOf returns the terminal in is, if it is one.
func terminalOf(in any) (Terminal, bool) {
	f, ok := in.(*os.File)
	if !ok {
		return nil, false
	}
	if _, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGetTermios); err != nil {
		return nil, false
	}
	return ttyFile{f}, true
}

type ttyFile struct{ f *os.File }

func (t ttyFile) MakeRaw() (func(), error) {
	fd := int(t.f.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, err
	}
	raw := *old
	// As cfmakeraw(3).
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &raw); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, old) }, nil
}

func (t ttyFile) Size() (uint16, uint16, error) {
	ws, err := unix.IoctlGetWinsize(int(t.f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, err
	}
	return ws.Row, ws.Col, nil
}
