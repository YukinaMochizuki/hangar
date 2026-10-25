package serialmux

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
)

// text is the screen's cells as lines of text, and where its cursor is.
func text(e *vt.Emulator) ([]string, int, int) {
	var lines []string
	for y := range e.Height() {
		var b strings.Builder
		for x := range e.Width() {
			c := e.CellAt(x, y)
			switch {
			case c == nil:
			case c.Width == 0 && c.Content == "":
				// The second half of a wide character.
			case c.Content == "":
				b.WriteByte(' ')
			default:
				b.WriteString(c.Content)
			}
		}
		lines = append(lines, b.String())
	}
	pos := e.CursorPosition()
	return lines, pos.X, pos.Y
}

// TestARedrawDrawsTheScreen: a terminal fed only the redraw of a screen,
// after a recorded htop or vim session (wide characters, scroll regions, 256
// colours and the alternate screen among them), shows what the screen does,
// cursor included.
func TestARedrawDrawsTheScreen(t *testing.T) {
	for _, rec := range []string{"htop.bin", "vim.bin"} {
		b, err := os.ReadFile("testdata/" + rec)
		if err != nil {
			t.Fatal(err)
		}
		s := newScreen()
		for len(b) > 0 { // in the chunks a serial read gives
			n := min(len(b), 512)
			s.write(b[:n])
			b = b[n:]
		}
		drawn := vt.NewEmulator(DefaultColumns, DefaultRows)
		go func() { _, _ = drawn.Read(make([]byte, 1<<20)) }()
		_, _ = drawn.Write(s.redraw())
		want, wx, wy := text(s.emu)
		got, gx, gy := text(drawn)
		for y := range want {
			if got[y] != want[y] {
				t.Errorf("%s: line %d drawn %q, the screen has %q", rec, y, got[y], want[y])
			}
		}
		if gx != wx || gy != wy || drawn.IsAltScreen() != s.emu.IsAltScreen() {
			t.Errorf("%s: cursor drawn at %d,%d (alt %t), the screen's at %d,%d (alt %t)", rec, gx, gy, drawn.IsAltScreen(), wx, wy, s.emu.IsAltScreen())
		}
		if s.resets != 0 {
			t.Errorf("%s: the screen was started afresh %d times", rec, s.resets)
		}
	}
}

// malicious are inputs that panicked, held or bloated an emulator, unless
// guarded (from the PoC's fuzzing and a hand-made list).
var malicious = []string{
	"a\x1b[1000000000b",
	"\x1b[0700000Z",
	"\x1b[0729019\x10X",
	"\x1b[10000000X",
	"\x1b[1000000000@",
	"\x1b[1000000000P",
	"\x1b[1000000000L",
	"\x1b[1000000000M",
	"\x1b[1000000000S",
	"\x1b[1000000000T",
	"\x1b[1000000000I",
	"\x1b[-1@",
	"\x1b[-1P",
	"\x1b[99999999999;99999999999Hx",
	"\x1b[9223372036854775808Cx",
	"\x1b[8;100000;100000t",
	"\x1b[20;5r\x1b[5;20r\x1b[0;0r\x1b[99999;1r\n\n\n",
	"\x1b[?69h\x1b[70;10s\x1b[1000000000@x",
	"\x1b]0;" + strings.Repeat("A", 1<<20),
	"\x1bP" + strings.Repeat("q", 1<<20),
	strings.Repeat("\xff\xc3\x28\xe2\x82\xf0\x9f", 20000),
	"e" + strings.Repeat("́", 1<<16),
	strings.Repeat("\x1b[c\x1b[>c\x1b[6n\x1b[?2026$p\x1bP$qm\x1b\\", 2000),
	strings.Repeat("line\r\n", 10000),
}

// TestTheScreenTakesWhatHoldsAnEmulator: each malicious input, in the
// chunks a serial read gives, is taken without the emulator panicking or a
// chunk holding it for the minute it is given here, and the screen still
// draws. Unguarded, a count of a billion held it for seconds; how fast a
// chunk goes is not measured, as a test's clock is no CPU's.
func TestTheScreenTakesWhatHoldsAnEmulator(t *testing.T) {
	for _, in := range malicious {
		s := newScreen()
		s.timeout = time.Minute
		b := []byte(in)
		for len(b) > 0 {
			n := min(len(b), 512)
			s.write(b[:n])
			b = b[n:]
		}
		if s.resets != 0 {
			t.Errorf("%.40q: the screen started afresh %d times, %d of them panics", in, s.resets, s.panics)
		}
		if len(s.redraw()) == 0 {
			t.Errorf("%.40q: no redraw", in)
		}
	}
}

// TestCountsPastTheBoundAreDropped: a sequence that repeats, moves or
// inserts by a count within maxCount does so; one past it is dropped whole,
// never done cell by cell.
func TestCountsPastTheBoundAreDropped(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"a\x1b[5b", "aaaaaa"},
		{"a\x1b[1001b", "a"},
		{"\x1b[10Cx", "          x"},
		{"\x1b[1000000000Cx", "x"},
		{"bc\x1b[1;1H\x1b[2@a", "a bc"},
		{"bc\x1b[1;1H\x1b[2000@a", "ac"},
	} {
		s := newScreen()
		s.write([]byte(tt.in))
		if line, _, _ := text(s.emu); strings.TrimRight(line[0], " ") != tt.want {
			t.Errorf("%q draws %q, want %q", tt.in, strings.TrimRight(line[0], " "), tt.want)
		}
		s.close()
	}
}

// TestAResizeIsBounded: a size past the screen's bounds, or below one cell,
// is taken as the bound.
func TestAResizeIsBounded(t *testing.T) {
	s := newScreen()
	for _, tt := range []struct{ c, r, wc, wr int }{
		{-1, -1, 1, 1}, {0, 0, 1, 1}, {100000, 100000, MaxColumns, MaxRows}, {200, 50, 200, 50},
	} {
		s.resize(tt.c, tt.r)
		if c, r := s.size(); c != tt.wc || r != tt.wr || s.emu.Width() != tt.wc || s.emu.Height() != tt.wr {
			t.Errorf("resized to %dx%d: %dx%d, emulator %dx%d, want %dx%d", tt.c, tt.r, c, r, s.emu.Width(), s.emu.Height(), tt.wc, tt.wr)
		}
	}
}

// TestAStalledWriteStartsTheScreenAfresh: a write the emulator takes past
// the screen's timeout over is given up on, the screen started afresh.
func TestAStalledWriteStartsTheScreenAfresh(t *testing.T) {
	s := newScreen()
	s.timeout = time.Nanosecond
	s.write([]byte(strings.Repeat("x", 1<<20)))
	if s.resets != 1 {
		t.Fatalf("started afresh %d times", s.resets)
	}
	s.timeout = slowWrite
	s.write([]byte("after"))
	if lines, _, _ := text(s.emu); !strings.HasPrefix(lines[0], "after") {
		t.Errorf("the fresh screen shows %q", lines[0])
	}
}

// FuzzScreen: whatever the guest writes, the emulator does not panic and
// the screen draws. How long it takes is TestTheScreenTakesWhatHoldsAnEmulator's
// to check: fuzzing workers share the CPU.
func FuzzScreen(f *testing.F) {
	for _, in := range malicious {
		if len(in) < 4096 {
			f.Add([]byte(in))
		}
	}
	f.Add([]byte("\x1b[?1049h\x1b[2J\x1b[1;1Hhello\x1b[?25l"))
	f.Fuzz(func(t *testing.T, b []byte) {
		s := newScreen()
		s.timeout = time.Minute
		s.write(b)
		s.resize(len(b)%300, len(b)%120)
		s.write(b)
		if len(s.redraw()) == 0 || s.panics != 0 {
			t.Errorf("the emulator panicked %d times", s.panics)
		}
		s.close()
	})
}
