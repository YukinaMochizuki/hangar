package console_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
)

// TestRecordsCrossATerminalWhole: the console is a tty, whose defaults
// would turn each \n into \r\n.
func TestRecordsCrossATerminalWhole(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ptmx.Close() }()
	if err := console.Raw(tty); err != nil {
		t.Fatal(err)
	}
	read := make(chan []byte)
	go func() {
		var all []byte
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			all = append(all, buf[:n]...)
			if err != nil {
				read <- all
				return
			}
		}
	}()
	w := console.NewWriter(tty, at)
	_ = w.Write(console.Stdout, []byte("hello\nworld\r\n\x03\x04"))
	_ = w.WriteExit(console.ExitStatus{Code: 10})
	time.Sleep(100 * time.Millisecond) //nolint:forbidigo // lets background work run
	_ = tty.Close()

	r := console.NewReader(bytes.NewReader(<-read))
	out, err := r.Next()
	if err != nil || string(out.Payload) != "hello\nworld\r\n\x03\x04" {
		t.Errorf("output %q %v", out.Payload, err)
	}
	rec, err := r.Next()
	if st, xerr := console.ExitOf(rec); err != nil || xerr != nil || st.Code != 10 {
		t.Errorf("exit %+v %v %v", st, err, xerr)
	}
}

func at() time.Time { return time.Unix(1700000000, 42) }

func TestRecordsReadBackInOrder(t *testing.T) {
	var buf bytes.Buffer
	w := console.NewWriter(&buf, at)
	_, _ = w.StreamWriter(console.Stdout).Write([]byte("hello\n"))
	_, _ = w.StreamWriter(console.Stderr).Write([]byte("oops\n"))
	big := strings.Repeat("y", console.MaxPayload+10)
	_ = w.Write(console.Stdout, []byte(big))
	_ = w.WriteExit(console.ExitStatus{Code: 137, Signal: 9})
	if err := w.Write(console.Init, []byte("after")); !errors.Is(err, console.ErrExited) {
		t.Errorf("a write after the exit: %v", err)
	}
	if err := w.WriteExit(console.ExitStatus{}); !errors.Is(err, console.ErrExited) {
		t.Errorf("a second exit: %v", err)
	}

	r := console.NewReader(&buf)
	var got []console.Record
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, rec)
	}
	if len(got) != 5 {
		t.Fatalf("%d records, want 5 (the long write split in two)", len(got))
	}
	if got[0].Stream != console.Stdout || string(got[0].Payload) != "hello\n" || !got[0].Time.Equal(at()) {
		t.Errorf("first %+v", got[0])
	}
	if got[1].Stream != console.Stderr || string(got[1].Payload) != "oops\n" {
		t.Errorf("second %+v", got[1])
	}
	if len(got[2].Payload) != console.MaxPayload || string(got[2].Payload)+string(got[3].Payload) != big {
		t.Error("the long write did not come back whole")
	}
	st, err := console.ExitOf(got[4])
	if err != nil || st != (console.ExitStatus{Code: 137, Signal: 9}) {
		t.Errorf("exit %+v %v", st, err)
	}
}

// TestAFileCutShortIsReadOnFromTheNextRecord: a reader started inside a
// record, as after rotation, skips to the next one.
func TestAFileCutShortIsReadOnFromTheNextRecord(t *testing.T) {
	var buf bytes.Buffer
	w := console.NewWriter(&buf, at)
	_ = w.Write(console.Stdout, []byte("first"))
	_ = w.Write(console.Stdout, []byte("second"))
	cut := buf.Bytes()[7:] // inside the first record's header

	r := console.NewReader(bytes.NewReader(cut))
	rec, err := r.Next()
	if !errors.Is(err, console.ErrCorrupt) || string(rec.Payload) != "second" {
		t.Errorf("after a cut: %q, %v", rec.Payload, err)
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("then %v, want EOF", err)
	}

	// A record cut short at the end is not a record.
	whole := buf.Bytes()
	r = console.NewReader(bytes.NewReader(whole[:len(whole)-2]))
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a record cut short: %v", err)
	}
}
