package natlog

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

// hourFile writes an hour's plain file of n connections, and returns it
// and its content.
func hourFile(t *testing.T, dir, stem string, n int) (string, []byte) {
	t.Helper()
	at, _ := time.Parse(hourLayout, stem)
	l, err := Open(dir, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if err := l.Write(newRecord(at.Add(time.Duration(i)*time.Second), 40123, "web")); err != nil {
			t.Fatal(err)
		}
	}
	l.mu.Lock()
	_ = l.f.Close()
	l.f = nil
	l.mu.Unlock()
	path := filepath.Join(dir, fileName(stem))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, b
}

func gzipTo(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	_, _ = zw.Write(b)
	_ = zw.Close()
	_ = f.Close()
}

// connections counts the connections the log in dir holds for the hour.
func connections(t *testing.T, dir string) int {
	t.Helper()
	recs, err := Lookup(dir, Query{Port: 40123, From: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 17, 23, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range recs {
		if r.Event == EventNew {
			n++
		}
	}
	return n
}

// TestACompressionCutShortSettles: of an hour a crash left with both its
// plain file and its gzip, opening the log keeps the gzip if it holds the
// whole hour, else the plain file; a temporary file goes; each connection
// is then read once.
func TestACompressionCutShortSettles(t *testing.T) {
	for _, c := range []struct {
		name  string
		crash func(t *testing.T, plain string, b []byte)
		want  []string
	}{
		{"after the rename", func(t *testing.T, plain string, b []byte) { gzipTo(t, plain+gzSuffix, b) },
			[]string{"nat-2026-10-17-09.jsonl.gz"}},
		{"with the gzip cut short", func(t *testing.T, plain string, b []byte) {
			gzipTo(t, plain+gzSuffix, b)
			fi, _ := os.Stat(plain + gzSuffix)
			_ = os.Truncate(plain+gzSuffix, fi.Size()/2)
		}, []string{"nat-2026-10-17-09.jsonl"}},
		{"with the gzip of less", func(t *testing.T, plain string, b []byte) { gzipTo(t, plain+gzSuffix, b[:len(b)/2]) },
			[]string{"nat-2026-10-17-09.jsonl"}},
		{"with the gzip's trailer cut", func(t *testing.T, plain string, b []byte) {
			gzipTo(t, plain+gzSuffix, b)
			fi, _ := os.Stat(plain + gzSuffix)
			_ = os.Truncate(plain+gzSuffix, fi.Size()-4)
		}, []string{"nat-2026-10-17-09.jsonl"}},
		{"before the rename", func(t *testing.T, plain string, b []byte) { gzipTo(t, plain+tmpSuffix, b) },
			[]string{"nat-2026-10-17-09.jsonl"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			plain, b := hourFile(t, dir, "2026-10-17-09", 5)
			c.crash(t, plain, b)
			l, err := Open(dir, Options{Now: func() time.Time { return time.Date(2026, 10, 17, 11, 0, 0, 0, time.UTC) }})
			if err != nil {
				t.Fatal(err)
			}
			if got := files(t, dir); !slices.Equal(got, c.want) {
				t.Errorf("files %q, want %q", got, c.want)
			}
			if n := connections(t, dir); n != 5 {
				t.Errorf("%d connections read, want the 5 written, once each", n)
			}
			// And compressed in time, whatever was kept.
			if err := l.Maintain(); err != nil {
				t.Fatal(err)
			}
			if got := files(t, dir); !slices.Equal(got, []string{"nat-2026-10-17-09.jsonl.gz"}) {
				t.Errorf("after maintenance, files %q, want the hour compressed", got)
			}
			if n := connections(t, dir); n != 5 {
				t.Errorf("after maintenance, %d connections read, want 5", n)
			}
		})
	}
}

// TestALateRecordIsNotLost: a record late for the hour being compressed
// goes to the hour being written, and is read with the others; the hour
// compressed is not opened again.
func TestALateRecordIsNotLost(t *testing.T) {
	dir := t.TempDir()
	c := newClock(time.Date(2026, 10, 17, 9, 59, 0, 0, time.UTC))
	l, err := Open(dir, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Write(newRecord(c.now(), 40123, "nine")); err != nil {
		t.Fatal(err)
	}
	c.set(time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC))
	if err := l.Write(newRecord(c.now(), 40123, "ten")); err != nil { // the hour 09 is compressed meanwhile
		t.Fatal(err)
	}
	if err := l.Write(newRecord(time.Date(2026, 10, 17, 9, 59, 59, 0, time.UTC), 40123, "late")); err != nil {
		t.Fatal(err)
	}
	l.busy.Wait()
	if got := files(t, dir); !slices.Equal(got, []string{"nat-2026-10-17-09.jsonl.gz", "nat-2026-10-17-10.jsonl"}) {
		t.Errorf("files %q, want the hour 09 compressed alone and 10 written", got)
	}
	_ = l.Close("test")
	if n := connections(t, dir); n != 3 {
		t.Errorf("%d connections read, want 3, the late one too", n)
	}
}

// TestALateRecordAcrossMidnightIsFound: a record late for a day's last
// hour, written to the next day's first, is found by a window ending in
// that last hour.
func TestALateRecordAcrossMidnightIsFound(t *testing.T) {
	dir := t.TempDir()
	c := newClock(time.Date(2026, 10, 18, 0, 0, 5, 0, time.UTC))
	l, err := Open(dir, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	late := time.Date(2026, 10, 17, 23, 59, 59, 0, time.UTC)
	if err := l.Write(newRecord(late, 40123, "late")); err != nil {
		t.Fatal(err)
	}
	_ = l.Close("test")
	if got := files(t, dir); !slices.Equal(got, []string{"nat-2026-10-18-00.jsonl"}) {
		t.Fatalf("files %q, want the late record in the hour being written", got)
	}
	recs, err := Lookup(dir, Query{Port: 40123, From: late.Add(-time.Hour), To: late})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 || recs[0].Machine != "late" {
		t.Errorf("found %+v, want the late record", recs)
	}
}

// TestSettlingStreams: settling an hour of tens of MiB holds little of it
// in memory, as an hour of a flood may take gigabytes.
func TestSettlingStreams(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, fileName("2026-10-17-09"))
	line := []byte(`{"time":"2026-10-17T09:00:00Z","event":"new","proto":"tcp","public_port":40123}` + "\n")
	b := bytes.Repeat(line, (32<<20)/len(line))
	if err := os.WriteFile(plain, b, 0o600); err != nil {
		t.Fatal(err)
	}
	gzipTo(t, plain+gzSuffix, b)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	same, err := gzipHolds(plain+gzSuffix, plain)
	runtime.ReadMemStats(&after)
	if err != nil || !same {
		t.Fatalf("the hour's gzip is taken as other than it: %t %v", same, err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 4<<20 {
		t.Errorf("settling 32 MiB allocated %d MiB, want it streamed", alloc>>20)
	}
}
