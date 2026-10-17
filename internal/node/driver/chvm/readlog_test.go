package chvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// logHost is a driver over a directory of logs, all ReadLog needs.
func logHost(t *testing.T, poll time.Duration) (*Hypervisor, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "m1"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &Hypervisor{cfg: Config{Dir: dir, logPoll: poll}}, filepath.Join(dir, "m1", ConsoleLog)
}

var t0 = time.Unix(1700000000, 0)

func line(n int) console.Record {
	return console.Record{Stream: console.Stdout, Time: t0.Add(time.Duration(n) * time.Second), Payload: fmt.Appendf(nil, "%d\n", n)}
}

func appendRecords(t *testing.T, path string, recs ...console.Record) {
	t.Helper()
	var b []byte
	for _, r := range recs {
		b = console.AppendRecord(b, r)
	}
	appendBytes(t, path, b)
}

func appendBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

// collector gathers what ReadLog sends.
type collector struct {
	mu   sync.Mutex
	recs []driver.LogRecord
}

func (c *collector) send(recs []driver.LogRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, recs...)
	return nil
}

// text renders what was sent as boot:data, an exit as boot:exit=code.
func (c *collector) text() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, r := range c.recs {
		if r.Stream == console.Exit {
			out = append(out, fmt.Sprintf("%d:exit=%d", r.Boot, r.Exit.Code))
			continue
		}
		out = append(out, fmt.Sprintf("%d:%s", r.Boot, strings.TrimSuffix(string(r.Data), "\n")))
	}
	return out
}

func (c *collector) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.recs)
}

func read(t *testing.T, h *Hypervisor, opts driver.LogOptions) []string {
	t.Helper()
	var c collector
	if err := h.ReadLog(t.Context(), "m1", opts, c.send); err != nil {
		t.Fatal(err)
	}
	return c.text()
}

func same(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

// rotatedLog is a log whose rotated file holds boot 1, its exit and the
// start of boot 2, and whose current file goes on with boot 2, as the
// launcher leaves them.
func rotatedLog(t *testing.T, path string) {
	t.Helper()
	appendRecords(t, path+".1", console.BootRecord(1, t0), line(1), line(2),
		console.ExitRecord(console.ExitStatus{Code: 3}, t0.Add(3*time.Second)),
		console.BootRecord(2, t0.Add(4*time.Second)), line(5))
	appendRecords(t, path, console.BootRecord(2, t0.Add(4*time.Second)), line(6), line(7))
}

func TestReadingBothFiles(t *testing.T) {
	h, path := logHost(t, 10*time.Millisecond)
	rotatedLog(t, path)
	same(t, "all", read(t, h, driver.LogOptions{Tail: driver.AllLines}), []string{"1:1", "1:2", "1:exit=3", "2:5", "2:6", "2:7"})
	same(t, "a tail across both", read(t, h, driver.LogOptions{Tail: 4}), []string{"1:exit=3", "2:5", "2:6", "2:7"})
	same(t, "since", read(t, h, driver.LogOptions{Tail: driver.AllLines, Since: t0.Add(5 * time.Second)}), []string{"2:5", "2:6", "2:7"})
	same(t, "since and tail", read(t, h, driver.LogOptions{Tail: 1, Since: t0.Add(2 * time.Second)}), []string{"2:7"})
	same(t, "the current file alone", func() []string {
		if err := os.Remove(path + ".1"); err != nil {
			t.Fatal(err)
		}
		return read(t, h, driver.LogOptions{Tail: driver.AllLines})
	}(), []string{"2:6", "2:7"})
}

func TestNoLogIsNoRecords(t *testing.T) {
	h, _ := logHost(t, 10*time.Millisecond)
	same(t, "none", read(t, h, driver.LogOptions{Tail: driver.AllLines}), nil)
	if err := h.ReadLog(t.Context(), "../m1", driver.LogOptions{}, (&collector{}).send); err == nil {
		t.Error("read the log of a uid that names no VM")
	}
}

// TestForgedAndOversizedFrames: whatever the file holds, the reader goes
// on to the next record, and never holds more than a record.
func TestForgedAndOversizedFrames(t *testing.T) {
	h, path := logHost(t, 10*time.Millisecond)
	appendRecords(t, path, console.BootRecord(1, t0), line(1))
	appendBytes(t, path, []byte("garbage\xc4\x47"))
	appendBytes(t, path, []byte("\xc4\x47\x01\x00\x00\x00\x00\x00\x00\x00\x01\xff\xff\xff\xff"))
	appendBytes(t, path, []byte("\xc4\x47\x04\x00\x00\x00\x00\x00\x00\x00\x01\x00\x00\x00\x03abc")) // an exit of the wrong size
	appendRecords(t, path, console.Record{Stream: 77, Time: t0, Payload: []byte("unknown")}, line(2))
	appendBytes(t, path, []byte(strings.Repeat("\xc4", 3*console.MaxPayload)))
	appendRecords(t, path, line(3))
	same(t, "read past it all", read(t, h, driver.LogOptions{Tail: driver.AllLines}), []string{"1:1", "1:2", "1:3"})
}

func follow(t *testing.T, h *Hypervisor, opts driver.LogOptions) (*collector, func()) {
	t.Helper()
	var c collector
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- h.ReadLog(ctx, "m1", opts, c.send) }()
	return &c, func() {
		t.Helper()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("following ended with %v", err)
		}
	}
}

// has says whether c has n records, for testutil.Eventually.
func has(c *collector, n int) func() (bool, string) {
	return func() (bool, string) {
		return c.len() >= n, fmt.Sprintf("%d records: %q", c.len(), c.text())
	}
}

// TestFollowingAHalfWrittenRecord: a record seen in part is read once it
// is whole.
func TestFollowingAHalfWrittenRecord(t *testing.T) {
	h, path := logHost(t, 5*time.Millisecond)
	appendRecords(t, path, console.BootRecord(1, t0))
	c, stop := follow(t, h, driver.LogOptions{Tail: driver.AllLines, Follow: true})
	defer stop()
	rec := console.AppendRecord(nil, line(1))
	appendBytes(t, path, rec[:9])
	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // time for the follower to read the half record
	appendBytes(t, path, rec[9:])
	testutil.Eventually(t, fmt.Sprint(1, " records"), has(c, 1))
	same(t, "followed", c.text(), []string{"1:1"})
}

// TestFollowingThroughRotations: the launcher's own log, rotating every
// ten records or so, made after the follow began, is followed from its
// start without a record lost or sent twice. The writer waits for the
// follower every seven records, so the log rotates at most once between
// two reads, however loaded the machine.
func TestFollowingThroughRotations(t *testing.T) {
	h, path := logHost(t, 2*time.Millisecond)
	c, stop := follow(t, h, driver.LogOptions{Tail: driver.AllLines, Follow: true})
	defer stop()
	time.Sleep(20 * time.Millisecond) //nolint:forbidigo // time for the follow to begin before the log exists
	clog, err := openCapped(path, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clog.Close() }()
	mark := console.AppendRecord(nil, console.BootRecord(1, t0))
	clog.head = mark
	if _, err := clog.Write(mark); err != nil {
		t.Fatal(err)
	}
	const n = 500
	for i := range n {
		if _, err := clog.Write(console.AppendRecord(nil, line(i))); err != nil {
			t.Fatal(err)
		}
		if i%7 == 6 {
			testutil.Eventually(t, fmt.Sprint(i+1, " records"), has(c, i+1))
		}
	}
	testutil.Eventually(t, fmt.Sprint(n, " records"), has(c, n))
	time.Sleep(20 * time.Millisecond) //nolint:forbidigo // lets background work run
	got := c.text()
	if len(got) != n {
		t.Fatalf("%d records, want %d", len(got), n)
	}
	for i, s := range got {
		if s != "1:"+strconv.Itoa(i) {
			t.Fatalf("record %d is %q", i, s)
		}
	}
}

// TestFollowingThroughTwoRotationsAtOnce: the file rotated between the
// one being read and the new one is read too.
func TestFollowingThroughTwoRotationsAtOnce(t *testing.T) {
	_, path := logHost(t, time.Hour)
	appendRecords(t, path, console.BootRecord(1, t0), line(0))
	var c collector
	cur, old, err := openLogs(path)
	if err != nil || old != nil {
		t.Fatal(err)
	}
	emit := func(rec console.Record) error {
		if rec.Stream == console.Stdout {
			return c.send([]driver.LogRecord{{Boot: 1, Stream: rec.Stream, Data: rec.Payload}})
		}
		return nil
	}
	if cur, err = followLog(path, cur, emit); err != nil {
		t.Fatal(err)
	}
	clog, err := openCapped(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clog.Close() }()
	for i := 1; i <= 2; i++ { // each write rotates
		if _, err := clog.Write(console.AppendRecord(nil, line(i))); err != nil {
			t.Fatal(err)
		}
	}
	if cur, err = followLog(path, cur, emit); err != nil {
		t.Fatal(err)
	}
	cur.close()
	same(t, "after two rotations", c.text(), []string{"1:0", "1:1", "1:2"})
}
