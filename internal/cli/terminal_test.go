package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// TestRawModeIsPutBack: on a real terminal, raw mode stops echo and
// Ctrl-C's signal, and restoring puts back every setting as it was.
func TestRawModeIsPutBack(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer func() { _ = ptmx.Close(); _ = tty.Close() }()
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: 33, Cols: 101}); err != nil {
		t.Fatal(err)
	}
	if _, ok := terminalOf(strings.NewReader("")); ok {
		t.Error("a reader taken as a terminal")
	}
	if f, err := os.CreateTemp(t.TempDir(), "file"); err == nil {
		if _, ok := terminalOf(f); ok {
			t.Error("a file taken as a terminal")
		}
		_ = f.Close()
	}
	term, ok := terminalOf(tty)
	if !ok {
		t.Fatal("a pty not taken as a terminal")
	}
	if rows, cols, err := term.Size(); err != nil || rows != 33 || cols != 101 {
		t.Errorf("size %dx%d %v", rows, cols, err)
	}
	before, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	restore, err := term.MakeRaw()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Lflag&(unix.ECHO|unix.ICANON|unix.ISIG) != 0 || raw.Oflag&unix.OPOST != 0 {
		t.Errorf("raw mode left lflag %#x oflag %#x", raw.Lflag, raw.Oflag)
	}
	restore()
	after, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if *after != *before {
		t.Errorf("restored %+v, was %+v", *after, *before)
	}
}
