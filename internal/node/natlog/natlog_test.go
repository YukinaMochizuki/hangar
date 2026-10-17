package natlog

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// clock is a settable clock for a Log, which reads it from the goroutine
// that compresses a day too.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func newClock(t time.Time) *clock { return &clock{t: t} }

func newRecord(at time.Time, port uint16, machine string) Record {
	return Record{
		Time: at, Event: EventNew, Protocol: "tcp", PublicIP: "192.0.2.1", PublicPort: port,
		DstIP: "198.51.100.7", DstPort: 443, Project: "p", Network: "default", NetworkUID: "n1",
		Machine: machine, MachineUID: machine + "-uid", InnerIP: "10.0.0.2", InnerPort: 51000,
	}
}

func files(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestTheLogIsRootsAlone: the directory and its files are no one else's to
// read, whatever they were before.
func TestTheLogIsRootsAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nat")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	c := newClock(time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC))
	l, err := Open(dir, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Write(newRecord(c.now(), 40123, "web")); err != nil {
		t.Fatal(err)
	}
	if err := l.Close("test"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the directory is %v (%v), want 0700", fi.Mode().Perm(), err)
	}
	for _, name := range files(t, dir) {
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v (%v), want 0600", name, fi.Mode().Perm(), err)
		}
	}
}

// TestLastIsTheStop: a log opened again knows its last record, the stop
// the node wrote, which the next gap starts from.
func TestLastIsTheStop(t *testing.T) {
	dir := t.TempDir()
	c := newClock(time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC))
	l, err := Open(dir, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	if !l.Last().IsZero() {
		t.Errorf("a new log's last record is at %s", l.Last())
	}
	if err := l.Write(newRecord(c.now(), 40123, "web")); err != nil {
		t.Fatal(err)
	}
	stopped := c.now().Add(time.Minute)
	c.set(stopped)
	if err := l.Close("test"); err != nil {
		t.Fatal(err)
	}
	l, err = Open(dir, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close("test") }()
	if !l.Last().Equal(stopped) {
		t.Errorf("the last record is at %s, want the stop at %s", l.Last(), stopped)
	}
}

// TestLastIsOfTheLastHour: of a day of several hours, the last record is
// that of the latest hour's file, the earlier ones compressed.
func TestLastIsOfTheLastHour(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	c := newClock(at)
	l, err := Open(dir, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []time.Duration{0, time.Hour} {
		c.set(at.Add(h))
		if err := l.Write(newRecord(c.now(), 40123, "web")); err != nil {
			t.Fatal(err)
		}
	}
	l.busy.Wait()
	if got := files(t, dir); !slices.Equal(got, []string{"nat-2026-10-17-09.jsonl.gz", "nat-2026-10-17-10.jsonl"}) {
		t.Fatalf("files %q, want two hours", got)
	}
	l.mu.Lock()
	_ = l.f.Close()
	l.f = nil
	l.mu.Unlock()
	if got, err := lastTime(dir); err != nil || !got.Equal(at.Add(time.Hour)) {
		t.Errorf("last at %s (%v), want the 10 o'clock record's", got, err)
	}
}

// TestLastReadsFromTheEnd: the last record of a day far larger than what
// is read of its end is found past a line a crash cut short, and that of
// a compressed day too.
func TestLastReadsFromTheEnd(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	stop := at.Add(30 * time.Minute)
	l, err := Open(dir, Options{Now: func() time.Time { return stop }})
	if err != nil {
		t.Fatal(err)
	}
	n := 3 * tailChunk / 200
	for i := range n {
		if err := l.Write(newRecord(at.Add(time.Duration(i)*time.Millisecond), 40123, "web")); err != nil {
			t.Fatal(err)
		}
	}
	_ = l.Close("test") // a stop, at the clock's time
	path := filepath.Join(dir, "nat-2026-10-17-09.jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"time":"2026-10-17T09:59:00Z","event":"new","pub`)
	_ = f.Close()
	if fi, _ := os.Stat(path); fi.Size() < 2*tailChunk {
		t.Fatalf("the day is %d bytes, not past what is read at once", fi.Size())
	}
	want := stop
	for _, compressed := range []bool{false, true} {
		if compressed {
			if err := compress(path); err != nil {
				t.Fatal(err)
			}
		}
		got, err := lastTime(dir)
		if err != nil || !got.Equal(want) {
			t.Errorf("compressed %t: last at %s (%v), want %s", compressed, got, err, want)
		}
	}
}

// TestLookupFindsThePort: the connections from one public port in the
// window, of either day's files, and nothing else; each hour is compressed
// once the next begins, and a record late for an hour the log has moved on
// from goes to the hour being written, and is found.
func TestLookupFindsThePort(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 10, 16, 23, 59, 0, 0, time.UTC)
	c := newClock(start)
	l, err := Open(dir, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []Record{
		newRecord(start.Add(10*time.Second), 40123, "before-midnight"),
		newRecord(start.Add(30*time.Second), 40124, "other-port"),
		newRecord(start.Add(2*time.Minute), 40123, "after-midnight"),
		newRecord(start.Add(3*time.Hour), 40123, "too-late"),
	} {
		c.set(r.Time)
		if err := l.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	l.busy.Wait() // the compressions the new hours began
	other := newRecord(start.Add(3*time.Minute), 40123, "other-address")
	other.PublicIP = "192.0.2.9"
	if err := l.Write(other); err != nil {
		t.Fatal(err)
	}
	l.busy.Wait()
	// other-address, of the hour 00, came after the log moved on to 02.
	if got := files(t, dir); !slices.Equal(got, []string{"nat-2026-10-16-23.jsonl.gz", "nat-2026-10-17-00.jsonl.gz", "nat-2026-10-17-02.jsonl"}) {
		t.Errorf("files %q, want each hour but the last compressed once the next began, and no hour opened again", got)
	}
	at := start.Add(5 * time.Minute)
	find := func(ip string) []string {
		t.Helper()
		recs, err := Lookup(dir, Query{Port: 40123, IP: ip, From: at.Add(-time.Hour), To: at.Add(5 * time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		var machines []string
		for _, r := range recs {
			if r.Event == EventNew {
				machines = append(machines, r.Machine)
			}
		}
		return machines
	}
	if got := find("192.0.2.1"); !slices.Equal(got, []string{"before-midnight", "after-midnight"}) {
		t.Errorf("found %q, want before-midnight and after-midnight", got)
	}
	if got := find(""); !slices.Equal(got, []string{"before-midnight", "after-midnight", "other-address"}) {
		t.Errorf("found %q, want the late record too", got)
	}
	_ = l.Close("test")
}

// TestMaintainKeepsThirtyDays: an hour's file is compressed once the hour
// is over, a day kept while any of it is younger than Retention, then
// removed.
func TestMaintainKeepsThirtyDays(t *testing.T) {
	dir := t.TempDir()
	// A compression cut short leaves this; opening the log removes it.
	stale := filepath.Join(dir, "nat-2026-08-31.jsonl.gz.tmp")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newClock(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	l, err := Open(dir, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Write(newRecord(c.now(), 40123, "web")); err != nil {
		t.Fatal(err)
	}
	c.set(time.Date(2026, 9, 2, 0, 0, 1, 0, time.UTC))
	if err := l.Write(newRecord(c.now(), 40124, "web")); err != nil {
		t.Fatal(err)
	}
	// The compression the new day began, then ours.
	l.busy.Wait()
	if err := l.Maintain(); err != nil {
		t.Fatal(err)
	}
	if got := files(t, dir); !slices.Equal(got, []string{"nat-2026-09-01-12.jsonl.gz", "nat-2026-09-02-00.jsonl"}) {
		t.Errorf("after a day: %q, want the 1st compressed and the 2nd open", got)
	}
	// The 1st ended at the start of the 2nd: 30 days later, less a second,
	// its last connection is not yet 30 days old.
	c.set(time.Date(2026, 10, 1, 23, 59, 59, 0, time.UTC))
	if err := l.Maintain(); err != nil {
		t.Fatal(err)
	}
	if got := files(t, dir); !slices.Contains(got, "nat-2026-09-01-12.jsonl.gz") {
		t.Errorf("before 30 days were over: %q, the 1st gone", got)
	}
	c.set(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	if err := l.Maintain(); err != nil {
		t.Fatal(err)
	}
	// The 2nd is still the day being written: no record came since.
	if got := files(t, dir); !slices.Equal(got, []string{"nat-2026-09-02-00.jsonl"}) {
		t.Errorf("once 30 days were over: %q, want the 1st gone and the 2nd kept", got)
	}
	_ = l.Close("test")
}

// TestACutLineIsSkipped: a line a crash cut short neither fails a lookup
// nor hides the records around it.
func TestACutLineIsSkipped(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	c := newClock(at)
	l, err := Open(dir, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Write(newRecord(at, 40123, "first")); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "nat-2026-10-17-09.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"time":"2026-10-17T09:00:01Z","event":"new","public_po` + "\n")
	_ = f.Close()
	if err := l.Write(newRecord(at.Add(2*time.Second), 40123, "second")); err != nil {
		t.Fatal(err)
	}
	_ = l.Close("test")
	recs, err := Lookup(dir, Query{Port: 40123, From: at.Add(-time.Minute), To: at.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for _, r := range recs {
		if r.Event == EventNew {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d connections found around a cut line, want 2: %+v", n, recs)
	}
}
