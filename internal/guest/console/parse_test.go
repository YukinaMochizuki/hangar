package console_test

import (
	"errors"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
)

// TestParseWaitsForAWholeRecord: every prefix of a record is short, and
// the whole of it parses, taking exactly its bytes.
func TestParseWaitsForAWholeRecord(t *testing.T) {
	b := console.AppendRecord(nil, console.Record{Stream: console.Stdout, Time: at(), Payload: []byte("hello")})
	b = console.AppendRecord(b, console.Record{Stream: console.Stderr, Time: at(), Payload: []byte("next")})
	first := len(b) - (15 + 4)
	for n := range first {
		if _, drop, err := console.Parse(b[:n]); !errors.Is(err, console.ErrShort) || drop != 0 {
			t.Fatalf("%d bytes: drop %d, %v; want short", n, drop, err)
		}
	}
	rec, n, err := console.Parse(b)
	if err != nil || n != first || rec.Stream != console.Stdout || string(rec.Payload) != "hello" || !rec.Time.Equal(at()) {
		t.Fatalf("parsed %+v, %d bytes, %v", rec, n, err)
	}
	b[15] = 'J' // the payload is a copy
	if string(rec.Payload) != "hello" {
		t.Error("the record shares the buffer")
	}
	if rec, _, err := console.Parse(b[n:]); err != nil || string(rec.Payload) != "next" {
		t.Errorf("then %+v %v", rec, err)
	}

	empty := console.AppendRecord(nil, console.Record{Stream: console.Init, Time: at()})
	if rec, n, err := console.Parse(empty); err != nil || n != len(empty) || rec.Payload == nil {
		t.Errorf("an empty record: %+v %d %v", rec, n, err)
	}
}

// TestParseSkipsWhatIsNoRecord: garbage before a record is skipped and
// reported; a header claiming more than a record holds is dropped by its
// magic alone, so what follows it is read.
func TestParseSkipsWhatIsNoRecord(t *testing.T) {
	rec := console.AppendRecord(nil, console.Record{Stream: console.Stdout, Time: at(), Payload: []byte("ok")})
	b := append([]byte("junk\xc4junk"), rec...)
	got, n, err := console.Parse(b)
	if !errors.Is(err, console.ErrCorrupt) || n != len(b) || string(got.Payload) != "ok" {
		t.Errorf("after garbage: %+v %d %v", got, n, err)
	}
	if _, drop, err := console.Parse([]byte("only junk")); !errors.Is(err, console.ErrShort) || drop != len("only junk") {
		t.Errorf("garbage alone: drop %d, %v", drop, err)
	}

	huge := []byte("\xc4\x47\x01\x00\x00\x00\x00\x00\x00\x00\x01\xff\xff\xff\xff")
	b = append(huge, rec...)
	got, n, err = console.Parse(b)
	if !errors.Is(err, console.ErrCorrupt) || got.Payload != nil || n != 2 {
		t.Fatalf("an oversized header: %+v %d %v", got, n, err)
	}
	if got, _, err := console.Parse(b[n:]); !errors.Is(err, console.ErrCorrupt) || string(got.Payload) != "ok" {
		t.Errorf("after it: %+v %v", got, err)
	}
}

func TestBootAndExitRecordsRoundTrip(t *testing.T) {
	n, err := console.BootOf(console.BootRecord(7, at()))
	if err != nil || n != 7 {
		t.Errorf("boot %d %v", n, err)
	}
	if _, err := console.BootOf(console.Record{Stream: console.Boot, Payload: []byte{1}}); err == nil {
		t.Error("a short boot record decoded")
	}
	st, err := console.ExitOf(console.ExitRecord(console.ExitStatus{Code: -1, Signal: 9}, at()))
	if err != nil || st != (console.ExitStatus{Code: -1, Signal: 9}) {
		t.Errorf("exit %+v %v", st, err)
	}
}
