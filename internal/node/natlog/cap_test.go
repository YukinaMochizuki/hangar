package natlog

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// dayFile makes a day's file in dir holding one connection at noon.
func dayFile(t *testing.T, dir, d string) {
	t.Helper()
	at, _ := time.Parse(dayLayout, d)
	l, err := Open(dir, Options{Now: func() time.Time { return at.Add(12 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Write(newRecord(at.Add(12*time.Hour), 40123, "web")); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	_ = l.f.Close()
	l.f = nil
	l.mu.Unlock()
}

// dayIn is the day d of the log in dir.
func dayIn(t *testing.T, dir, d string) day {
	t.Helper()
	days, err := listDays(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range days {
		if x.day == d {
			return x
		}
	}
	t.Fatalf("no day %s in %s", d, dir)
	return day{}
}

// TestTrimKeepsUnderTheCap: past FullAt of its filesystem, the oldest days
// go first, never today's, as many as their sizes cover the excess, though
// the filesystem's usage does not drop yet, and the log says how many days
// it now holds.
func TestTrimKeepsUnderTheCap(t *testing.T) {
	dir := t.TempDir()
	pad := make([]byte, 64<<10)
	for _, d := range []string{"2026-10-10", "2026-10-11", "2026-10-12", "2026-10-13", "2026-10-14"} {
		if err := os.WriteFile(filepath.Join(dir, fileName(d)), pad, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dayFile(t, dir, "2026-10-15")
	days, _ := listDays(dir)
	size := uint64(days[0].size())
	// Six days of size used of five: 1.5 days past 90%, so the oldest two
	// must go. Usage reads the same after: a filesystem frees later.
	usage := func() (uint64, uint64, error) { return 6 * size, 5 * size, nil }
	now := time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)
	l, err := Open(dir, Options{Now: func() time.Time { return now }, Usage: usage})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close("test") }()
	if err := l.Trim(); err != nil {
		t.Fatal(err)
	}
	days, _ = listDays(dir)
	var left []string
	for _, d := range days {
		left = append(left, d.day)
	}
	if !slices.Equal(left, []string{"2026-10-12", "2026-10-13", "2026-10-14", "2026-10-15"}) {
		t.Errorf("days left %q, want the oldest two gone and no more", left)
	}
	var said []Record
	_ = scan(days[len(days)-1], func(r Record) {
		if r.Event == EventRetention {
			said = append(said, r)
		}
	})
	if len(said) != 1 || said[0].Days != 4 {
		t.Errorf("retention records %+v, want one saying 4 days", said)
	}
	// Under the cap, nothing goes and nothing is said.
	l.o.Usage = func() (uint64, uint64, error) { return 4 * size, 5 * size, nil }
	if err := l.Trim(); err != nil {
		t.Fatal(err)
	}
	if again, _ := listDays(dir); len(again) != 4 {
		t.Errorf("%d days after a trim under the cap, want 4", len(again))
	}
}

// TestTrimLeavesToday: however full, today stays, all its hours, the
// compressed with the one being written.
func TestTrimLeavesToday(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)
	c := newClock(time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC))
	w, err := Open(dir, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []int{9, 12} {
		c.set(time.Date(2026, 10, 15, h, 0, 0, 0, time.UTC))
		if err := w.Write(newRecord(c.now(), 40123, "web")); err != nil {
			t.Fatal(err)
		}
	}
	w.busy.Wait()
	if got := files(t, dir); !slices.Equal(got, []string{"nat-2026-10-15-09.jsonl.gz", "nat-2026-10-15-12.jsonl"}) {
		t.Fatalf("files %q, want today's 09 compressed and 12 being written", got)
	}
	l, err := Open(dir, Options{Now: func() time.Time { return now }, Usage: func() (uint64, uint64, error) { return 100, 100, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Trim(); err != nil {
		t.Fatal(err)
	}
	days, _ := listDays(dir)
	kept := 0
	for _, d := range days {
		_ = scan(d, func(r Record) {
			if r.Event == EventNew {
				kept++
			}
		})
	}
	if kept != 2 {
		t.Errorf("%d connections left, want today's two", kept)
	}
}

// TestAFullFilesystemTakesADay: a write refused as the quota is reached,
// though usage reads under the cap, as ZFS's does, removes the oldest day
// and is made.
func TestAFullFilesystemTakesADay(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"2026-10-13", "2026-10-14"} {
		dayFile(t, dir, d)
	}
	now := time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)
	refuse := true
	l, err := Open(dir, Options{
		Now:   func() time.Time { return now },
		Usage: func() (uint64, uint64, error) { return 88, 100, nil },
		Create: func(path string) (io.WriteCloser, error) {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
			if refuse {
				refuse = false
				_ = f.Close()
				return nil, syscall.EDQUOT
			}
			return f, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close("test") }()
	if err := l.Write(newRecord(now, 40123, "web")); err != nil {
		t.Fatalf("the refused write was not made room for: %v", err)
	}
	days, _ := listDays(dir)
	var left []string
	for _, d := range days {
		left = append(left, d.day)
	}
	if !slices.Equal(left, []string{"2026-10-14", "2026-10-15"}) {
		t.Errorf("days left %q, want the oldest gone and no more", left)
	}
}

// TestSpaceFreedLateTakesOneDay: a filesystem that frees a removed day's
// space seconds later, as ZFS does, refuses the writes tried meanwhile;
// they wait for that room rather than each take another day, and if it
// takes past pendingFor, the log says so once and still takes no day for
// a refused write. Once usage shows it, the next write is made, with the
// gap and what was said of the day removed.
func TestSpaceFreedLateTakesOneDay(t *testing.T) {
	dir := t.TempDir()
	pad := make([]byte, 64<<10)
	for _, d := range []string{"2026-10-11", "2026-10-12", "2026-10-13", "2026-10-14"} {
		if err := os.WriteFile(filepath.Join(dir, fileName(d)), pad, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	days, _ := listDays(dir)
	size := uint64(days[0].size())
	now := time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)
	// Full until freed says the removed day is free.
	freed := false
	refuse := true
	var warned []string
	l, err := Open(dir, Options{
		Now:  func() time.Time { return now },
		Warn: func(msg string) { warned = append(warned, msg) },
		Usage: func() (uint64, uint64, error) {
			if freed {
				return 3 * size, 4 * size, nil
			}
			return 4 * size, 4 * size, nil
		},
		Create: func(path string) (io.WriteCloser, error) {
			if refuse {
				return nil, syscall.EDQUOT
			}
			return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := l.Write(newRecord(now.Add(time.Duration(i)*time.Second), 40123, "web")); err == nil {
			t.Fatalf("write %d made though the room is not free yet", i)
		}
	}
	if left, _ := listDays(dir); len(left) != 3 {
		t.Errorf("%d of 4 days left after 3 refused writes, want the oldest alone gone", len(left))
	}
	// Past pendingFor, still not free: said once, and no more removed.
	now = now.Add(2 * pendingFor)
	_ = l.Write(newRecord(now, 40125, "web"))
	_ = l.Trim()
	if left, _ := listDays(dir); len(left) != 3 {
		t.Errorf("%d of 4 days left once the first removal was long past and still not free, want three", len(left))
	}
	if len(warned) != 1 {
		t.Errorf("warned %q, want once that the space did not come back", warned)
	}
	freed, refuse = true, false
	if err := l.Write(newRecord(now.Add(time.Minute), 40124, "web")); err != nil {
		t.Fatal(err)
	}
	_ = l.Close("test")
	if left, _ := listDays(dir); len(left) != 4 { // three old, and today's
		t.Errorf("%d days left once the room was free, want the three old and today's", len(left))
	}
	var gaps, retentions int
	_ = scan(dayIn(t, dir, "2026-10-15"), func(r Record) {
		switch r.Event {
		case EventGap:
			gaps++
		case EventRetention:
			retentions++
		}
	})
	if gaps != 1 || retentions != 1 {
		t.Errorf("%d gaps and %d retention records written, want the gap for the refused writes and what was said of the day removed", gaps, retentions)
	}
}

// flakyFile fails its writes while full says so.
type flakyFile struct {
	f    *os.File
	full *bool
}

func (w flakyFile) Write(b []byte) (int, error) {
	if *w.full {
		return 0, syscall.ENOSPC
	}
	return w.f.Write(b)
}

func (w flakyFile) Close() error { return w.f.Close() }

// TestFailedWritesBecomeAGap: writes failing are told once, the gap from
// the first that failed is written once writing works again, before what
// is written then, and that is told once too.
func TestFailedWritesBecomeAGap(t *testing.T) {
	dir := t.TempDir()
	full := false
	var told []bool
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	l, err := Open(dir, Options{
		Now:   func() time.Time { return at },
		Usage: func() (uint64, uint64, error) { return 0, 1, nil },
		Create: func(path string) (io.WriteCloser, error) {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
			return flakyFile{f, &full}, err
		},
		Failing: func(failing bool, _ error) { told = append(told, failing) },
	})
	if err != nil {
		t.Fatal(err)
	}
	full = true
	for i := range 3 {
		if err := l.Write(newRecord(at.Add(time.Duration(i)*time.Second), uint16(40000+i), "lost")); !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("write %d: %v, want ENOSPC", i, err)
		}
	}
	full = false
	if err := l.Write(newRecord(at.Add(time.Minute), 40100, "kept")); err != nil {
		t.Fatal(err)
	}
	_ = l.Close("test")
	if !slices.Equal(told, []bool{true, false}) {
		t.Errorf("told %v, want failing once, then working once", told)
	}
	var recs []Record
	_ = scan(dayIn(t, dir, "2026-10-17"), func(r Record) { recs = append(recs, r) })
	if len(recs) < 2 || recs[0].Event != EventGap || recs[1].Machine != "kept" {
		t.Fatalf("records %+v, want the gap, then the connection written once it worked", recs)
	}
	if g := recs[0]; g.From == nil || !g.From.Equal(at) || !g.To.Equal(at.Add(time.Minute)) {
		t.Errorf("the gap is %+v, want from the first failed, %s, to %s", g, at, at.Add(time.Minute))
	}
	for _, r := range recs {
		if r.Machine == "lost" {
			t.Errorf("a failed write is in the log: %+v", r)
		}
	}
}
