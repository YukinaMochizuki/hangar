package cli

import (
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf16"
)

// TestConsoleReaderKeepsCtrlZAndPairs: what the console reader gives is
// the keys as typed, a Ctrl-Z among them, and a character outside the
// BMP whole even when a read splits its surrogate pair.
func TestConsoleReaderKeepsCtrlZAndPairs(t *testing.T) {
	r := &consoleReader{}
	u := utf16.Encode([]rune("\x1aé😀x"))
	r.take(u[:3]) // Ctrl-Z, é, the pair's first half
	if string(r.pending) != "\x1aé" || r.high == 0 {
		t.Fatalf("after a split pair: pending %q, high %#x", r.pending, r.high)
	}
	r.take(u[3:])
	buf := make([]byte, 64)
	n, err := r.Read(buf)
	if err != nil || string(buf[:n]) != "\x1aé😀x" {
		t.Errorf("read %q, %v", buf[:n], err)
	}
}

// fakeConsole is a terminal whose size the test sets.
type fakeConsole struct {
	mu         sync.Mutex
	rows, cols uint16
}

func (*fakeConsole) MakeRaw() (func(), error) { return func() {}, nil }

func (f *fakeConsole) Size() (uint16, uint16, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows, f.cols, nil
}

func (f *fakeConsole) resize(rows, cols uint16) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows, f.cols = rows, cols
}

// TestWatchSizeSaysOnlyChanges: watchSize raises resized once a poll sees
// a size other than the last, and not while it stays.
func TestWatchSizeSaysOnlyChanges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		term := &fakeConsole{rows: 24, cols: 80}
		c := make(chan os.Signal, 4)
		stop := watchSize(term, c)
		defer stop()
		time.Sleep(10 * sizePoll) //nolint:forbidigo // virtual time
		synctest.Wait()
		if len(c) != 0 {
			t.Fatalf("%d signals with the size unchanged", len(c))
		}
		term.resize(30, 100)
		time.Sleep(sizePoll) //nolint:forbidigo // virtual time
		synctest.Wait()
		if n := len(c); n != 1 || <-c != resized {
			t.Fatalf("after one resize: %d signals", n)
		}
		time.Sleep(10 * sizePoll) //nolint:forbidigo // virtual time
		synctest.Wait()
		if len(c) != 0 {
			t.Errorf("%d signals after the size settled", len(c))
		}
	})
}
