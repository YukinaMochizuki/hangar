package cli

import (
	"io"
	"os"
	"sync"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// sizeChange is the signal watchSize raises when the console's size
// changes; Windows has none of its own. Relayed through Options.Signals as
// SIGWINCH is, signal.Notify ignores it.
type sizeChange struct{}

func (sizeChange) String() string { return "console resized" }
func (sizeChange) Signal()        {}

// resized is the signal that says the terminal's size changed.
var resized os.Signal = sizeChange{}

// sizePoll is how often watchSize looks at the console's size.
const sizePoll = 250 * time.Millisecond

// watchSize sends resized on c whenever term's size has changed since it
// last looked, until stop.
func watchSize(term Terminal, c chan<- os.Signal) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	rows, cols, _ := term.Size()
	wg.Go(func() {
		t := time.NewTicker(sizePoll)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			r, c2, err := term.Size()
			if err != nil || (r == rows && c2 == cols) {
				continue
			}
			rows, cols = r, c2
			select {
			case c <- resized:
			default: // one is waiting already, and says as much
			}
		}
	})
	return func() {
		close(done)
		wg.Wait()
	}
}

// terminalOf returns the console in is, if it is one.
func terminalOf(in any) (Terminal, bool) {
	f, ok := in.(*os.File)
	if !ok {
		return nil, false
	}
	var mode uint32
	if windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) != nil {
		return nil, false
	}
	out, err := consoleOut()
	if err != nil {
		return nil, false
	}
	return console{in: windows.Handle(f.Fd()), out: out}, true
}

// consoleOut is the console's screen buffer, which hangar writes the
// exec's output to through stdout and whose size is the terminal's. It is
// opened once and kept for the life of the process.
var consoleOut = sync.OnceValues(func() (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString("CONOUT$")
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
})

type console struct{ in, out windows.Handle }

// Input reads the keys typed, as Unicode in UTF-8. Not through os.File:
// Go's console read takes a Ctrl-Z typed first as the end of input, which
// in raw mode is a key like any other, to suspend a job or quit a REPL on
// the other end.
func (c console) Input() io.Reader { return &consoleReader{h: c.in} }

// consoleReader reads a console with ReadConsoleW.
type consoleReader struct {
	h windows.Handle
	// pending is UTF-8 not yet read; high is the first half of a
	// surrogate pair whose second half the next read brings.
	pending []byte
	high    uint16
}

func (r *consoleReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		var buf [512]uint16
		var n uint32
		if err := windows.ReadConsole(r.h, &buf[0], uint32(len(buf)), &n, nil); err != nil {
			return 0, err
		}
		r.take(buf[:n])
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// take adds what a read brought to pending, holding back a first half of
// a surrogate pair for the next.
func (r *consoleReader) take(u []uint16) {
	if r.high != 0 {
		u = append([]uint16{r.high}, u...)
		r.high = 0
	}
	if l := len(u); l > 0 && u[l-1] >= 0xD800 && u[l-1] < 0xDC00 {
		r.high, u = u[l-1], u[:l-1]
	}
	r.pending = append(r.pending, string(utf16.Decode(u))...)
}

// MakeRaw turns off the console's line editing, echo and its own handling
// of Ctrl-C, and has it pass keys on as a VT terminal's, and take the
// exec's output as one: the remote terminal does both.
func (c console) MakeRaw() (func(), error) {
	var inMode, outMode uint32
	if err := windows.GetConsoleMode(c.in, &inMode); err != nil {
		return nil, err
	}
	if err := windows.GetConsoleMode(c.out, &outMode); err != nil {
		return nil, err
	}
	rawIn := inMode&^(windows.ENABLE_ECHO_INPUT|windows.ENABLE_PROCESSED_INPUT|windows.ENABLE_LINE_INPUT) | windows.ENABLE_VIRTUAL_TERMINAL_INPUT
	if err := windows.SetConsoleMode(c.in, rawIn); err != nil {
		return nil, err
	}
	// Without DISABLE_NEWLINE_AUTO_RETURN a line feed also returns the
	// cursor, which a VT terminal leaves to the carriage return.
	rawOut := outMode | windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.DISABLE_NEWLINE_AUTO_RETURN
	if err := windows.SetConsoleMode(c.out, rawOut); err != nil {
		_ = windows.SetConsoleMode(c.in, inMode)
		return nil, err
	}
	return func() {
		_ = windows.SetConsoleMode(c.in, inMode)
		_ = windows.SetConsoleMode(c.out, outMode)
	}, nil
}

// Size is the size of the console's window, not of its scrollback.
func (c console) Size() (uint16, uint16, error) {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(c.out, &info); err != nil {
		return 0, 0, err
	}
	w := info.Window
	return uint16(w.Bottom - w.Top + 1), uint16(w.Right - w.Left + 1), nil
}
