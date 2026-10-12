// Package consolelog turns a VM's console records, read in order, into
// what a read of its log sends (driver.Hypervisor.ReadLog): boots numbered
// by the host's Boot records, records older than Since dropped, and only
// the last lines kept for a tail, within driver.MaxLogTail of memory.
// Reading the records is each hypervisor driver's own; this package holds
// what they share, so they select alike.
package consolelog

import (
	"bytes"
	"math"
	"unsafe"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// Selection selects from the records of one read. Records go in with Add:
// first those the log holds, the backlog, then, once EndBacklog has
// returned the tail, what is written while following.
type Selection struct {
	opts driver.LogOptions
	boot int64
	live bool
	// tail is the backlog's last lines, when Tail is not AllLines.
	tail      []driver.LogRecord
	tailLines int64
	tailBytes int
}

// New returns a selection by opts.
func New(opts driver.LogOptions) *Selection {
	return &Selection{opts: opts}
}

// Add takes the next record and returns the record to send at once, if
// any: every selected one but those a tail keeps back until EndBacklog.
func (s *Selection) Add(rec console.Record) (driver.LogRecord, bool) {
	r, ok := s.convert(rec)
	if !ok {
		return driver.LogRecord{}, false
	}
	if s.live || s.opts.Tail == driver.AllLines {
		return r, true
	}
	if s.opts.Tail <= 0 {
		return driver.LogRecord{}, false
	}
	s.tail = append(s.tail, r)
	s.tailLines += lines(r)
	s.tailBytes += cost(r)
	for len(s.tail) > 1 && (s.tailLines-lines(s.tail[0]) >= s.opts.Tail || s.tailBytes > driver.MaxLogTail) {
		s.tailLines -= lines(s.tail[0])
		s.tailBytes -= cost(s.tail[0])
		s.tail[0] = driver.LogRecord{}
		s.tail = s.tail[1:]
	}
	return driver.LogRecord{}, false
}

// EndBacklog returns what the tail kept of the backlog, and makes Add pass
// every selected record on from then on.
func (s *Selection) EndBacklog() []driver.LogRecord {
	s.live = true
	tail := s.tail
	s.tail = nil
	if len(tail) == 0 {
		return nil
	}
	if extra := s.tailLines - s.opts.Tail; extra > 0 {
		tail[0].Data = dropLines(tail[0].Data, extra)
	}
	if len(tail[0].Data) > driver.MaxLogTail {
		tail[0].Data = tail[0].Data[len(tail[0].Data)-driver.MaxLogTail:]
	}
	return tail
}

func (s *Selection) convert(rec console.Record) (driver.LogRecord, bool) {
	r := driver.LogRecord{Boot: s.boot, Stream: rec.Stream, Time: rec.Time}
	switch rec.Stream {
	case console.Boot:
		if n, err := console.BootOf(rec); err == nil {
			s.boot = int64(min(n, math.MaxInt64))
		}
		return r, false
	case console.Stdout, console.Stderr, console.Init:
		if len(rec.Payload) == 0 {
			return r, false
		}
		r.Data = rec.Payload
	case console.Exit:
		st, err := console.ExitOf(rec)
		if err != nil {
			return r, false
		}
		r.Exit = st
	default:
		return r, false
	}
	if !s.opts.Since.IsZero() && rec.Time.Before(s.opts.Since) {
		return r, false
	}
	return r, true
}

// recordCost is what a record kept for a tail takes beyond its data: the
// record itself, and the rounding of its data's allocation. Without it a
// log of one-byte records would keep millions of them.
const recordCost = int(unsafe.Sizeof(driver.LogRecord{})) + 16

// cost is the memory a record kept for a tail counts as.
func cost(r driver.LogRecord) int { return len(r.Data) + recordCost }

// lines is how many lines a record counts as: an exit is one, and output
// one per line, the last one even unfinished.
func lines(r driver.LogRecord) int64 {
	if r.Stream == console.Exit {
		return 1
	}
	n := int64(bytes.Count(r.Data, []byte{'\n'}))
	if len(r.Data) > 0 && r.Data[len(r.Data)-1] != '\n' {
		n++
	}
	return n
}

// dropLines drops the first n lines of data.
func dropLines(data []byte, n int64) []byte {
	for ; n > 0 && len(data) > 0; n-- {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return nil
		}
		data = data[i+1:]
	}
	return data
}

// Batch gathers records into batches of about driver.MaxLogBatch for
// send.
type Batch struct {
	send  func([]driver.LogRecord) error
	recs  []driver.LogRecord
	bytes int
}

// NewBatch returns a batch sending with send.
func NewBatch(send func([]driver.LogRecord) error) *Batch {
	return &Batch{send: send}
}

// Add adds a record, sending the batch once it is full.
func (b *Batch) Add(r driver.LogRecord) error {
	b.recs = append(b.recs, r)
	b.bytes += len(r.Data) + 32
	if b.bytes >= driver.MaxLogBatch {
		return b.Flush()
	}
	return nil
}

// Flush sends what the batch holds, if anything.
func (b *Batch) Flush() error {
	if len(b.recs) == 0 {
		return nil
	}
	recs := b.recs
	b.recs, b.bytes = nil, 0
	return b.send(recs)
}
