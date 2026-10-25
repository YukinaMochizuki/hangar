package serialmux

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// The screen's bounds. DefaultColumns and DefaultRows are what a guest's
// serial terminal is taken to be until its user says otherwise with stty:
// terminfo's size, which programs use while the tty's is 0x0.
const (
	DefaultColumns, DefaultRows = 80, 24
	MaxColumns, MaxRows         = 500, 200
)

// maxCount is the largest count a control sequence may repeat or move by.
// The emulator does what a count says cell by cell, so a count of a billion
// holds it for seconds; no screen of MaxColumns x MaxRows needs more.
const maxCount = 1000

// countSequences are the final bytes of the CSI sequences whose parameters
// are counts: insert, delete, erase, repeat, move and scroll.
const countSequences = "b@PXLMSTIZEFABCDGde`ac"

// slowWrite is how long the emulator may take over one write before the
// screen gives up on it and starts afresh: past it, some input it was not
// guarded against holds it.
const slowWrite = time.Second

// screen is the guest's serial terminal as the host draws it: an emulator
// fed what the guest writes, guarded against what makes it panic or stall,
// from which an attaching client's terminal is drawn whole. Its methods
// are called with the hub's lock held, never concurrently.
type screen struct {
	emu           *vt.Emulator
	columns, rows int
	// modes are the DEC private modes the guest set, which the attaching
	// terminal is given too: they change what its keys send.
	modes map[int]bool
	// resets counts the times the emulator was started afresh, panics
	// those of them it panicked.
	resets, panics int
	// timeout is slowWrite, shorter in tests.
	timeout time.Duration
}

func newScreen() *screen {
	s := &screen{columns: DefaultColumns, rows: DefaultRows, timeout: slowWrite}
	s.reset()
	return s
}

// reset starts the emulator afresh at the screen's size.
func (s *screen) reset() {
	s.close()
	e := vt.NewEmulator(s.columns, s.rows)
	// Only the screen is drawn; lines scrolled off it would only hold
	// memory, up to 10000 of MaxColumns cells by default.
	e.SetScrollbackSize(1)
	// It answers queries (DA, DSR, DECRQM) into a pipe, and a write blocks
	// on the pipe until someone reads it. The attached terminals answer
	// queries themselves.
	go func() { _, _ = io.Copy(io.Discard, e) }()
	for _, c := range countSequences {
		e.RegisterCsiHandler(int(c), func(p ansi.Params) bool {
			for i := range p {
				if n, _, ok := p.Param(i, 0); ok && n > maxCount {
					return true // handled: dropped
				}
			}
			return false
		})
	}
	modes := map[int]bool{}
	e.SetCallbacks(vt.Callbacks{
		EnableMode: func(m ansi.Mode) {
			if _, ok := m.(ansi.DECMode); ok {
				modes[m.Mode()] = true
			}
		},
		DisableMode: func(m ansi.Mode) {
			if _, ok := m.(ansi.DECMode); ok {
				delete(modes, m.Mode())
			}
		},
	})
	s.emu, s.modes = e, modes
}

// close lets go of the emulator, ending its pipe's drain: by closing the
// pipe, which is safe beside a write still running on it, as the
// emulator's Close, racing with its Read, is not.
func (s *screen) close() {
	if s.emu == nil {
		return
	}
	if w, ok := s.emu.InputPipe().(io.Closer); ok {
		_ = w.Close()
	}
}

// write feeds the emulator p. One that panics, or takes past the timeout,
// is started afresh: the screen is lost, the guest's output is not.
func (s *screen) write(p []byte) {
	done := make(chan bool, 1)
	e := s.emu
	go func() {
		ok := false
		defer func() {
			_ = recover()
			done <- ok
		}()
		_, _ = e.Write(p)
		ok = true
	}()
	select {
	case ok := <-done:
		if !ok {
			s.resets++
			s.panics++
			s.reset()
		}
	case <-time.After(s.timeout):
		// The stalled write runs on in its goroutine, on the unit's CPU,
		// the VM's own, until it ends.
		s.resets++
		s.reset()
	}
}

// resize sets the screen's size, bounded, which the emulator reflows to.
func (s *screen) resize(columns, rows int) {
	s.columns, s.rows = min(max(columns, 1), MaxColumns), min(max(rows, 1), MaxRows)
	s.emu.Resize(s.columns, s.rows)
}

// size is the screen's size.
func (s *screen) size() (columns, rows int) { return s.columns, s.rows }

// redraw is what an attaching terminal is sent first: its state cleared,
// the screen the guest is on, each line at its row, the modes that change
// what its keys send, and the cursor.
func (s *screen) redraw() []byte {
	var b bytes.Buffer
	e := s.emu
	b.WriteString("\x1b[0m\x1b[?1049l")
	if e.IsAltScreen() {
		b.WriteString("\x1b[?1049h")
	}
	b.WriteString("\x1b[H\x1b[2J")
	for y := range s.rows {
		line := make(uv.Line, s.columns)
		for x := range line {
			if c := e.CellAt(x, y); c != nil {
				line[x] = *c
			}
		}
		fmt.Fprintf(&b, "\x1b[%d;1H%s", y+1, line.Render())
	}
	var modes []int
	for m := range s.modes {
		switch m {
		case 25, 47, 1047, 1049: // the cursor and the screen, set apart
		default:
			modes = append(modes, m)
		}
	}
	slices.Sort(modes)
	for _, m := range modes {
		fmt.Fprintf(&b, "\x1b[?%dh", m)
	}
	pos := e.CursorPosition()
	fmt.Fprintf(&b, "\x1b[%d;%dH", pos.Y+1, pos.X+1)
	if e.CursorHidden() {
		b.WriteString("\x1b[?25l")
	} else {
		b.WriteString("\x1b[?25h")
	}
	return b.Bytes()
}
