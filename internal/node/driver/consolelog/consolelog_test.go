package consolelog_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/consolelog"
)

var t0 = time.Unix(1700000000, 0)

func at(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

func out(s int, text string) console.Record {
	return console.Record{Stream: console.Stdout, Time: at(s), Payload: []byte(text)}
}

// twoBoots is a log of two boots, the first ending in exit 3.
func twoBoots() []console.Record {
	return []console.Record{
		console.BootRecord(4, at(0)),
		out(1, "a\nb\n"),
		{Stream: console.Stderr, Time: at(2), Payload: []byte("c\n")},
		console.ExitRecord(console.ExitStatus{Code: 3}, at(3)),
		console.BootRecord(5, at(4)),
		{Stream: console.Init, Time: at(5), Payload: []byte("hangar-init: up\n")},
		out(6, "d\ne"),
		{Stream: 9, Time: at(7), Payload: []byte("unknown stream")},
		{Stream: console.Exit, Time: at(7), Payload: []byte("short")},
		{Stream: console.Boot, Time: at(7), Payload: []byte("short")},
	}
}

// read runs a selection over recs, backlog only, and renders what it
// sends as boot:stream:text, an exit as boot:exit:code.
func read(opts driver.LogOptions, recs []console.Record) []string {
	s := consolelog.New(opts)
	var got []driver.LogRecord
	for _, rec := range recs {
		if r, ok := s.Add(rec); ok {
			got = append(got, r)
		}
	}
	got = append(got, s.EndBacklog()...)
	var lines []string
	for _, r := range got {
		if r.Stream == console.Exit {
			lines = append(lines, strings.Join([]string{itoa(r.Boot), "exit", itoa(int64(r.Exit.Code))}, ":"))
			continue
		}
		lines = append(lines, strings.Join([]string{itoa(r.Boot), itoa(int64(r.Stream)), string(r.Data)}, ":"))
	}
	return lines
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func equal(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

func TestEveryLineAcrossBoots(t *testing.T) {
	equal(t, "all", read(driver.LogOptions{Tail: driver.AllLines}, twoBoots()), []string{
		"4:1:a\nb\n", "4:2:c\n", "4:exit:3", "5:3:hangar-init: up\n", "5:1:d\ne",
	})
}

// TestTailCountsLines: a record holding several lines is cut to the ones
// the tail keeps; an exit is one line, an unfinished line is one.
func TestTailCountsLines(t *testing.T) {
	equal(t, "tail 1", read(driver.LogOptions{Tail: 1}, twoBoots()), []string{"5:1:e"})
	equal(t, "tail 3", read(driver.LogOptions{Tail: 3}, twoBoots()), []string{"5:3:hangar-init: up\n", "5:1:d\ne"})
	equal(t, "tail 4", read(driver.LogOptions{Tail: 4}, twoBoots()), []string{
		"4:exit:3", "5:3:hangar-init: up\n", "5:1:d\ne",
	})
	equal(t, "tail 6", read(driver.LogOptions{Tail: 6}, twoBoots()), []string{
		"4:1:b\n", "4:2:c\n", "4:exit:3", "5:3:hangar-init: up\n", "5:1:d\ne",
	})
	equal(t, "tail 100", read(driver.LogOptions{Tail: 100}, twoBoots()), read(driver.LogOptions{Tail: driver.AllLines}, twoBoots()))
	equal(t, "tail 0", read(driver.LogOptions{Tail: 0}, twoBoots()), nil)
}

func TestSinceDropsOlderRecordsButKeepsTheirBoot(t *testing.T) {
	equal(t, "since", read(driver.LogOptions{Tail: driver.AllLines, Since: at(5)}, twoBoots()), []string{
		"5:3:hangar-init: up\n", "5:1:d\ne",
	})
	equal(t, "since and tail", read(driver.LogOptions{Tail: 1, Since: at(2)}, twoBoots()), []string{"5:1:e"})
}

// TestRecordsBeforeAnyBootAreOfNoKnownBoot: a log written before boots
// were numbered.
func TestRecordsBeforeAnyBootAreOfNoKnownBoot(t *testing.T) {
	equal(t, "unnumbered", read(driver.LogOptions{Tail: driver.AllLines}, []console.Record{out(0, "old\n"), console.BootRecord(1, at(1)), out(2, "new\n")}),
		[]string{"0:1:old\n", "1:1:new\n"})
}

// TestATailIsBoundedInBytes: lines without end do not make a tail keep
// more than MaxLogTail.
func TestATailIsBoundedInBytes(t *testing.T) {
	s := consolelog.New(driver.LogOptions{Tail: 1 << 40})
	chunk := strings.Repeat("x", console.MaxPayload)
	for range 4 * driver.MaxLogTail / console.MaxPayload {
		s.Add(out(0, chunk))
	}
	s.Add(out(1, "last\n"))
	total := 0
	tail := s.EndBacklog()
	for _, r := range tail {
		total += len(r.Data)
	}
	if total > driver.MaxLogTail || string(tail[len(tail)-1].Data) != "last\n" {
		t.Errorf("kept %d bytes in %d records, ending %q", total, len(tail), tail[len(tail)-1].Data)
	}
}

// TestFollowingPassesEverythingOn: after the backlog, records are sent
// as they come, whatever the tail.
func TestFollowingPassesEverythingOn(t *testing.T) {
	s := consolelog.New(driver.LogOptions{Tail: 0, Follow: true})
	s.Add(console.BootRecord(2, at(0)))
	s.Add(out(1, "old\n"))
	if tail := s.EndBacklog(); len(tail) != 0 {
		t.Errorf("tail 0 kept %v", tail)
	}
	r, ok := s.Add(out(2, "new\n"))
	if !ok || string(r.Data) != "new\n" || r.Boot != 2 {
		t.Errorf("followed %+v %t", r, ok)
	}
}

func TestBatchesAreBounded(t *testing.T) {
	var sizes []int
	b := consolelog.NewBatch(func(recs []driver.LogRecord) error {
		n := 0
		for _, r := range recs {
			n += len(r.Data)
		}
		sizes = append(sizes, n)
		return nil
	})
	for range 20 {
		_ = b.Add(driver.LogRecord{Data: make([]byte, console.MaxPayload)})
	}
	_ = b.Flush()
	_ = b.Flush()
	total := 0
	for _, n := range sizes {
		if n > driver.MaxLogBatch {
			t.Errorf("a batch of %d bytes", n)
		}
		total += n
	}
	if total != 20*console.MaxPayload || len(sizes) < 2 {
		t.Errorf("sent %d bytes in %d batches", total, len(sizes))
	}
}
