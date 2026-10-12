// Package console is the framing of what hangar-init writes to the guest's
// virtio console, which the hypervisor keeps in a file on the host: the
// container's stdout and stderr, hangar-init's own messages, and, last, how
// the container's process ended. The host reads the file whether or not the
// guest is still up. See docs/decisions/guest-agent.md.
package console

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Stream says what a record carries.
type Stream byte

const (
	Stdout Stream = 1
	Stderr Stream = 2
	// Init is hangar-init's own messages.
	Init Stream = 3
	// Exit is how the container's process ended: its exit code, then the
	// signal that ended it, as two big-endian int32s. It is the last record
	// of a boot.
	Exit Stream = 4
	// Boot starts a boot, and each log file the host keeps: its payload is
	// the boot's number, a big-endian uint64. Only the host writes it; one
	// from the guest is dropped, so it cannot be forged.
	Boot Stream = 5
)

// MaxPayload bounds one record; longer output is split.
const MaxPayload = 64 << 10

// The record header: two magic bytes, the stream, the time in Unix
// nanoseconds, and the payload's length. The magic lets a reader find the
// next record in a file cut short, such as one rotated mid-record.
var recordMagic = [2]byte{0xC4, 0x47}

const headerSize = 2 + 1 + 8 + 4

// Record is one framed piece of output.
type Record struct {
	Stream  Stream
	Time    time.Time
	Payload []byte
}

// ExitStatus is the payload of an Exit record.
type ExitStatus struct {
	Code, Signal int32
}

// Writer frames records onto w, one whole record at a time, safely from
// several goroutines.
type Writer struct {
	mu     sync.Mutex
	w      io.Writer
	now    func() time.Time
	exited bool
}

// ErrExited is what writing returns once the Exit record is written: it is
// the last record.
var ErrExited = errors.New("console: the exit is recorded")

// Raw makes the terminal f pass bytes through untouched, as frames need:
// a tty's defaults would turn each \n into \r\n.
func Raw(f *os.File) error {
	t, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		return err
	}
	// cfmakeraw(3)
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8
	return unix.IoctlSetTermios(int(f.Fd()), unix.TCSETS, t)
}

// NewWriter frames onto w.
func NewWriter(w io.Writer, now func() time.Time) *Writer {
	return &Writer{w: w, now: now}
}

// Write frames p as records of stream s, splitting it at MaxPayload.
func (w *Writer) Write(s Stream, p []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.write(s, p)
}

func (w *Writer) write(s Stream, p []byte) error {
	if w.exited {
		return ErrExited
	}
	for len(p) > 0 {
		n := min(len(p), MaxPayload)
		if _, err := w.w.Write(AppendRecord(nil, Record{Stream: s, Time: w.now(), Payload: p[:n]})); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// AppendRecord appends rec, framed, to b. Its payload must be at most
// MaxPayload.
func AppendRecord(b []byte, rec Record) []byte {
	var head [headerSize]byte
	copy(head[:], recordMagic[:])
	head[2] = byte(rec.Stream)
	binary.BigEndian.PutUint64(head[3:], uint64(rec.Time.UnixNano()))
	binary.BigEndian.PutUint32(head[11:], uint32(len(rec.Payload)))
	return append(append(b, head[:]...), rec.Payload...)
}

// WriteExit frames how the process ended, once: nothing is written after.
func (w *Writer) WriteExit(st ExitStatus) error {
	var p [8]byte
	binary.BigEndian.PutUint32(p[:], uint32(st.Code))
	binary.BigEndian.PutUint32(p[4:], uint32(st.Signal))
	w.mu.Lock()
	defer w.mu.Unlock()
	err := w.write(Exit, p[:])
	w.exited = true
	return err
}

// StreamWriter writes its bytes as records of one stream, for a process's
// stdout or stderr.
func (w *Writer) StreamWriter(s Stream) io.Writer {
	return streamWriter{w, s}
}

type streamWriter struct {
	w *Writer
	s Stream
}

func (sw streamWriter) Write(p []byte) (int, error) {
	if err := sw.w.Write(sw.s, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Reader reads records back.
type Reader struct {
	r *bufio.Reader
}

// NewReader reads records from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReader(r)}
}

// ErrCorrupt means bytes were skipped to find the next record.
var ErrCorrupt = errors.New("console: skipped bytes that are not a record")

// Next returns the next record, io.EOF at the end, or io.ErrUnexpectedEOF
// for one cut short. Bytes that are not a record are skipped, and reported
// as ErrCorrupt along with the record after them.
func (r *Reader) Next() (Record, error) {
	skipped := false
	for {
		b, err := r.r.Peek(2)
		if err != nil {
			if len(b) == 0 && errors.Is(err, io.EOF) && !skipped {
				return Record{}, io.EOF
			}
			return Record{}, io.ErrUnexpectedEOF
		}
		if b[0] == recordMagic[0] && b[1] == recordMagic[1] {
			break
		}
		_, _ = r.r.Discard(1)
		skipped = true
	}
	var head [headerSize]byte
	if _, err := io.ReadFull(r.r, head[:]); err != nil {
		return Record{}, io.ErrUnexpectedEOF
	}
	n := binary.BigEndian.Uint32(head[11:])
	if n > MaxPayload {
		return Record{}, fmt.Errorf("%w: a record of %d bytes", ErrCorrupt, n)
	}
	rec := Record{
		Stream:  Stream(head[2]),
		Time:    time.Unix(0, int64(binary.BigEndian.Uint64(head[3:]))),
		Payload: make([]byte, n),
	}
	if _, err := io.ReadFull(r.r, rec.Payload); err != nil {
		return Record{}, io.ErrUnexpectedEOF
	}
	if skipped {
		return rec, ErrCorrupt
	}
	return rec, nil
}

// ErrShort means b holds no whole record yet: more may complete it.
var ErrShort = errors.New("console: no whole record yet")

// Parse reads the record at the start of b, skipping bytes that are not
// one, and returns it with the bytes it took. It returns ErrShort, with
// the bytes that can be dropped, when b ends before a whole record, and
// ErrCorrupt with no record for a header claiming too long a payload;
// skipped bytes are reported as ErrCorrupt along with the record after
// them, as Reader.Next does.
func Parse(b []byte) (Record, int, error) {
	skip := 0
	for skip < len(b) && (b[skip] != recordMagic[0] || (skip+1 < len(b) && b[skip+1] != recordMagic[1])) {
		skip++
	}
	b = b[skip:]
	if len(b) < headerSize {
		return Record{}, skip, ErrShort
	}
	n := binary.BigEndian.Uint32(b[11:])
	if n > MaxPayload {
		return Record{}, skip + len(recordMagic), fmt.Errorf("%w: a record of %d bytes", ErrCorrupt, n)
	}
	if len(b) < headerSize+int(n) {
		return Record{}, skip, ErrShort
	}
	rec := Record{
		Stream:  Stream(b[2]),
		Time:    time.Unix(0, int64(binary.BigEndian.Uint64(b[3:]))),
		Payload: bytes.Clone(b[headerSize : headerSize+int(n)]),
	}
	if rec.Payload == nil {
		rec.Payload = []byte{}
	}
	if skip > 0 {
		return rec, skip + headerSize + int(n), ErrCorrupt
	}
	return rec, headerSize + int(n), nil
}

// BootRecord is the Boot record of boot number n.
func BootRecord(n uint64, t time.Time) Record {
	return Record{Stream: Boot, Time: t, Payload: binary.BigEndian.AppendUint64(nil, n)}
}

// BootOf decodes a Boot record's payload.
func BootOf(rec Record) (uint64, error) {
	if rec.Stream != Boot || len(rec.Payload) != 8 {
		return 0, fmt.Errorf("not a boot record: stream %d, %d bytes", rec.Stream, len(rec.Payload))
	}
	return binary.BigEndian.Uint64(rec.Payload), nil
}

// ExitRecord is the Exit record of st.
func ExitRecord(st ExitStatus, t time.Time) Record {
	p := binary.BigEndian.AppendUint32(nil, uint32(st.Code))
	return Record{Stream: Exit, Time: t, Payload: binary.BigEndian.AppendUint32(p, uint32(st.Signal))}
}

// ExitOf decodes an Exit record's payload.
func ExitOf(rec Record) (ExitStatus, error) {
	if rec.Stream != Exit || len(rec.Payload) != 8 {
		return ExitStatus{}, fmt.Errorf("not an exit record: stream %d, %d bytes", rec.Stream, len(rec.Payload))
	}
	return ExitStatus{
		Code:   int32(binary.BigEndian.Uint32(rec.Payload)),
		Signal: int32(binary.BigEndian.Uint32(rec.Payload[4:])),
	}, nil
}
