package chvm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
)

func record(s console.Stream, payload string) []byte {
	return console.AppendRecord(nil, console.Record{Stream: s, Time: time.Unix(1, 0), Payload: []byte(payload)})
}

func exitRec(code, sig int32) []byte {
	var p [8]byte
	binary.BigEndian.PutUint32(p[:], uint32(code))
	binary.BigEndian.PutUint32(p[4:], uint32(sig))
	return console.AppendRecord(nil, console.Record{Stream: console.Exit, Time: time.Unix(1, 0), Payload: p[:]})
}

// records reads every record in the file, failing on anything else.
func records(t *testing.T, path string) []console.Record {
	t.Helper()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	r := console.NewReader(f)
	var out []console.Record
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("%s: %v after %d records", path, err, len(out))
		}
		out = append(out, rec)
	}
}

func size(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func TestTheLogRotatesAtRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConsoleLog)
	const max = 1000
	l, err := openCapped(path, max)
	if err != nil {
		t.Fatal(err)
	}
	rec := record(console.Stdout, string(bytes.Repeat([]byte("x"), 85))) // 100 bytes framed
	for range 25 {
		if _, err := l.Write(rec); err != nil {
			t.Fatal(err)
		}
	}
	_ = l.Close()
	if s := size(t, path); s != 500 {
		t.Errorf("the log holds %d bytes, want the last 5 records", s)
	}
	if s := size(t, path+".1"); s != max {
		t.Errorf("the rotated log holds %d bytes, want %d", s, max)
	}
	if n := len(records(t, path)) + len(records(t, path+".1")); n != 15 {
		t.Errorf("%d whole records kept, want 15", n)
	}

	// Started again, it appends, and rotates at the cap as before.
	l, err = openCapped(path, max)
	if err != nil {
		t.Fatal(err)
	}
	for range 6 {
		if _, err := l.Write(rec); err != nil {
			t.Fatal(err)
		}
	}
	_ = l.Close()
	if s := size(t, path); s != 100 {
		t.Errorf("after starting again the log holds %d bytes, want 100", s)
	}
	if s := size(t, path+".1"); s != max {
		t.Errorf("after starting again the rotated log holds %d bytes", s)
	}

	// A record larger than the cap takes a file of its own.
	l, err = openCapped(path, max)
	if err != nil {
		t.Fatal(err)
	}
	big := record(console.Stdout, string(bytes.Repeat([]byte("y"), 2000)))
	if _, err := l.Write(big); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write(rec); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	if got := records(t, path+".1"); len(got) != 1 || len(got[0].Payload) != 2000 {
		t.Errorf("the large record was not kept whole: %d records", len(got))
	}
}

// TestTheLogFollowsNoLink: a link at the log's place is not written
// through.
func TestTheLogFollowsNoLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.Symlink(target, filepath.Join(dir, ConsoleLog)); err != nil {
		t.Fatal(err)
	}
	if _, err := openCapped(filepath.Join(dir, ConsoleLog), 100); err == nil {
		t.Error("opened the log through a symlink")
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("wrote through the symlink")
	}
}

func TestThePumpKeepsWellFormedRecords(t *testing.T) {
	var in bytes.Buffer
	in.Write(record(console.Init, "booting\n"))
	in.WriteString("garbage before a record")
	in.Write(record(console.Stdout, "out\n"))
	// A header claiming a payload larger than any record may have.
	var huge [15]byte
	copy(huge[:], record(console.Stdout, "")[:2])
	huge[2] = byte(console.Stdout)
	binary.BigEndian.PutUint32(huge[11:], console.MaxPayload+1)
	in.Write(huge[:])
	in.Write(record(console.Stderr, "err\n"))
	in.Write(record(console.Stream(9), "an unknown stream"))
	in.Write(exitRec(3, 0))
	// A forged exit of the wrong size is dropped; a later one wins.
	in.Write(console.AppendRecord(nil, console.Record{Stream: console.Exit, Payload: []byte{1, 2, 3}}))
	in.Write(exitRec(7, 9))
	in.Write(record(console.Stdout, "cut")[:10]) // cut short as the VM died

	var log bytes.Buffer
	st := pumpConsole(&in, &log, nil)
	if st == nil || *st != (console.ExitStatus{Code: 7, Signal: 9}) {
		t.Errorf("exit %+v, want the last well-formed one", st)
	}
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, log.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range records(t, path) {
		got = append(got, string(rune('0'+r.Stream))+":"+string(r.Payload))
	}
	want := []string{"3:booting\n", "1:out\n", "2:err\n", "4:" + string(exitRec(3, 0)[15:]), "4:" + string(exitRec(7, 9)[15:])}
	if len(got) != len(want) {
		t.Fatalf("kept %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d: %q, want %q", i, got[i], want[i])
		}
	}
}

// TestAGuestCannotNumberItsBoots: a Boot record the guest writes is
// dropped, so only the launcher's number the boots.
func TestAGuestCannotNumberItsBoots(t *testing.T) {
	var in bytes.Buffer
	in.Write(record(console.Stdout, "before\n"))
	in.Write(console.AppendRecord(nil, console.BootRecord(99, time.Unix(1, 0))))
	in.Write(record(console.Stdout, "after\n"))
	var log bytes.Buffer
	pumpConsole(&in, &log, nil)
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, log.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	got := records(t, path)
	if len(got) != 2 || string(got[0].Payload) != "before\n" || string(got[1].Payload) != "after\n" {
		t.Errorf("kept %+v, want the output without the forged boot", got)
	}
}

// TestAShortWriteLeavesNoPartOfARecord: a write the disk takes only part
// of, as when it fills, is cut back off the log, and so is a rotated
// file's head, so the log holds whole records only and what comes after
// is read.
func TestAShortWriteLeavesNoPartOfARecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConsoleLog)
	l, err := openCapped(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	var short atomic.Bool
	l.write = func(f *os.File, p []byte) (int, error) {
		if short.Load() {
			n, _ := f.Write(p[:len(p)/2])
			return n, syscall.ENOSPC
		}
		return f.Write(p)
	}
	l.head = record(console.Init, "head")
	if _, err := l.Write(record(console.Stdout, "one\n")); err != nil {
		t.Fatal(err)
	}
	short.Store(true)
	if n, err := l.Write(record(console.Stdout, "lost\n")); n != 0 || !errors.Is(err, syscall.ENOSPC) {
		t.Errorf("a short write: %d, %v", n, err)
	}
	short.Store(false)
	if _, err := l.Write(record(console.Stdout, "two\n")); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range records(t, path) {
		got = append(got, string(r.Payload))
	}
	if len(got) != 2 || got[0] != "one\n" || got[1] != "two\n" {
		t.Errorf("the log holds %q", got)
	}

	// A rotation whose head falls short starts the file with nothing.
	short.Store(true)
	if err := l.rotate(); err != nil {
		t.Fatal(err)
	}
	short.Store(false)
	if _, err := l.Write(record(console.Stdout, "three\n")); err != nil {
		t.Fatal(err)
	}
	if got := records(t, path); len(got) != 1 || string(got[0].Payload) != "three\n" || l.size != size(t, path) {
		t.Errorf("after a short head: %+v, size %d of %d", got, l.size, size(t, path))
	}
}

// TestThePumpSurvivesAFailingLog: records the log cannot take are dropped,
// and the pump reads on to the end.
func TestThePumpSurvivesAFailingLog(t *testing.T) {
	var in bytes.Buffer
	for range 100 {
		in.Write(record(console.Stdout, "x"))
	}
	in.Write(exitRec(0, 0))
	failures := 0
	st := pumpConsole(&in, failingWriter{}, func(error) { failures++ })
	if st == nil || st.Code != 0 {
		t.Errorf("exit %+v", st)
	}
	if failures != 1 {
		t.Errorf("the failure was reported %d times, want once", failures)
	}
}

// flakyWriter fails while broken is set, keeps what it takes, and says
// on wrote when each write is done.
type flakyWriter struct {
	broken atomic.Bool
	got    bytes.Buffer
	wrote  chan struct{}
}

func (w *flakyWriter) Write(p []byte) (int, error) {
	defer func() { w.wrote <- struct{}{} }()
	if w.broken.Load() {
		return 0, errors.New("immutable directory")
	}
	return w.got.Write(p)
}

// TestTheSerialPumpDrainsThroughFailures: a log failing for a while drops
// what it could not take, but the pump reads on, so the writer is never
// held up, and writes again once the log works.
func TestTheSerialPumpDrainsThroughFailures(t *testing.T) {
	r, w := io.Pipe()
	log := &flakyWriter{wrote: make(chan struct{}, 16)}
	failures := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		pumpRaw(r, log, func(error) { failures++ })
	}()
	write := func(s string) {
		t.Helper()
		wrote := make(chan error, 1)
		go func() {
			_, err := w.Write([]byte(s))
			wrote <- err
		}()
		select {
		case err := <-wrote:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("writing %q was held up: the pump stopped reading", s)
		}
		<-log.wrote
	}
	write("before\n")
	log.broken.Store(true)
	for range 3 {
		write("dropped\n")
	}
	log.broken.Store(false)
	write("after\n")
	_ = w.Close()
	<-done
	if got := log.got.String(); got != "before\nafter\n" {
		t.Errorf("the log holds %q", got)
	}
	if failures != 1 {
		t.Errorf("the failure was reported %d times, want once", failures)
	}
}

// TestARotationThatCannotReopen: a rotation that moved the log away but
// could not open it again is recovered by the next write, which opens it.
func TestARotationThatCannotReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConsoleLog)
	l, err := openCapped(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	rec := record(console.Stdout, string(bytes.Repeat([]byte("x"), 85))) // 100 bytes
	if _, err := l.Write(rec); err != nil {
		t.Fatal(err)
	}
	fail := true
	l.open = func(p string, flag int) (*os.File, error) {
		if fail {
			return nil, errors.New("EMFILE")
		}
		return openLog(p, flag)
	}
	if _, err := l.Write(rec); err == nil {
		t.Fatal("a rotation that could not open the log said nothing")
	}
	fail = false
	for range 2 {
		if _, err := l.Write(rec); err != nil {
			t.Fatalf("a write after the failed rotation: %v", err)
		}
	}
	if n := len(records(t, path)); n != 1 {
		t.Errorf("the log holds %d records, want 1", n)
	}
	if n := len(records(t, path+".1")); n != 1 {
		t.Errorf("the rotated log holds %d records, want 1", n)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestExitCodes(t *testing.T) {
	for _, tt := range []struct {
		st   *console.ExitStatus
		want int
	}{
		{nil, ExitNoRecord},
		{&console.ExitStatus{Code: 0}, 0},
		{&console.ExitStatus{Code: 3}, 3},
		{&console.ExitStatus{Code: 143, Signal: 15}, 143},
		{&console.ExitStatus{Code: 255}, 255},
		{&console.ExitStatus{Code: 256}, ExitNoRecord},
		{&console.ExitStatus{Code: -1}, ExitNoRecord},
	} {
		if got := exitCode(tt.st); got != tt.want {
			t.Errorf("exitCode(%+v) = %d, want %d", tt.st, got, tt.want)
		}
	}
}
