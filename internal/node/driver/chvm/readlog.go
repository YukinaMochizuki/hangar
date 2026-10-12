package chvm

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"syscall"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/consolelog"
)

// defaultLogPoll is how often a followed log is checked for more.
const defaultLogPoll = 250 * time.Millisecond

// logFile reads the records of one file of a console log, as far as it
// is written: a record not yet whole stays unread until it is.
type logFile struct {
	f    *os.File
	info fs.FileInfo
	// buf[off:] is read from the file and not yet parsed: never more than
	// a record and a read, so a file of any content is read in bounded
	// memory.
	buf []byte
	off int
}

const (
	logRead    = 64 << 10
	maxRecord  = 15 + console.MaxPayload
	logBufSize = maxRecord + logRead
)

// openLogFile opens a log file to read, or returns nil if there is none.
func openLogFile(path string) (*logFile, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &logFile{f: f, info: fi, buf: make([]byte, 0, logBufSize)}, nil
}

func (l *logFile) close() {
	if l != nil {
		_ = l.f.Close()
	}
}

// next returns the next whole record, or false once the file holds no
// more yet. What is not a record is skipped, as the tenant's own garbage.
func (l *logFile) next() (console.Record, bool, error) {
	for {
		rec, n, err := console.Parse(l.buf[l.off:])
		l.off += n
		switch {
		case err == nil, errors.Is(err, console.ErrCorrupt) && rec.Payload != nil:
			return rec, true, nil
		case errors.Is(err, console.ErrCorrupt):
			continue
		}
		more, err := l.fill()
		if !more || err != nil {
			return console.Record{}, false, err
		}
	}
}

// fill reads more of the file, reporting whether it got any.
func (l *logFile) fill() (bool, error) {
	if l.off > 0 {
		n := copy(l.buf[:cap(l.buf)], l.buf[l.off:])
		l.buf, l.off = l.buf[:n], 0
	}
	n, err := l.f.Read(l.buf[len(l.buf):cap(l.buf)])
	l.buf = l.buf[:len(l.buf)+n]
	if n > 0 {
		return true, nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		return false, nil
	}
	return false, err
}

// drain passes every whole record the file holds to emit.
func (l *logFile) drain(emit func(console.Record) error) error {
	for {
		rec, ok, err := l.next()
		if err != nil || !ok {
			return err
		}
		if err := emit(rec); err != nil {
			return err
		}
	}
}

// openLogs opens a machine's console log, its file and the one rotated
// from it, either nil if there is none, as one consistent pair: a
// rotation between the two opens would pair the new file with the file
// that was the current one, skipping the file between.
func openLogs(path string) (cur, old *logFile, err error) {
	for range 10 {
		if cur, err = openLogFile(path); err != nil {
			return nil, nil, err
		}
		if old, err = openLogFile(path + ".1"); err != nil {
			cur.close()
			return nil, nil, err
		}
		now, err := os.Lstat(path)
		switch {
		case cur == nil && errors.Is(err, fs.ErrNotExist),
			cur != nil && err == nil && os.SameFile(cur.info, now):
			return cur, old, nil
		}
		cur.close()
		old.close()
	}
	return nil, nil, errors.New("the console log rotates too fast to read")
}

func (h *Hypervisor) ReadLog(ctx context.Context, uid string, opts driver.LogOptions, send func([]driver.LogRecord) error) error {
	if err := checkUID(uid); err != nil {
		return err
	}
	path := h.LogPath(uid)
	sel := consolelog.New(opts)
	batch := consolelog.NewBatch(send)
	emit := func(rec console.Record) error {
		if r, ok := sel.Add(rec); ok {
			return batch.Add(r)
		}
		return nil
	}
	cur, old, err := openLogs(path)
	if err != nil {
		return err
	}
	defer func() { cur.close() }()
	if old != nil {
		err := old.drain(emit)
		old.close()
		if err != nil {
			return err
		}
	}
	if cur != nil {
		if err := cur.drain(emit); err != nil {
			return err
		}
	}
	for _, r := range sel.EndBacklog() {
		if err := batch.Add(r); err != nil {
			return err
		}
	}
	if err := batch.Flush(); err != nil || !opts.Follow {
		return err
	}

	poll := h.cfg.logPoll
	if poll == 0 {
		poll = defaultLogPoll
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		if cur, err = followLog(path, cur, emit); err != nil {
			return err
		}
		if err := batch.Flush(); err != nil {
			return err
		}
	}
}

// followLog reads what was written to the log since cur, the file last read,
// and returns the file to read on from. A rotation leaves cur whole, as
// the launcher writes only to the new file once it has renamed the old;
// cur is read to its end, then the file rotated after it, if the log
// rotated twice meanwhile, then the new file from its start. A log
// removed is followed again once it is made.
func followLog(path string, cur *logFile, emit func(console.Record) error) (*logFile, error) {
	if cur == nil {
		next, err := openLogFile(path)
		if err != nil || next == nil {
			return nil, err
		}
		return next, next.drain(emit)
	}
	if err := cur.drain(emit); err != nil {
		return cur, err
	}
	now, err := os.Lstat(path)
	if err == nil && os.SameFile(cur.info, now) {
		return cur, nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return cur, err
	}
	if err := cur.drain(emit); err != nil {
		return cur, err
	}
	if between, err := os.Lstat(path + ".1"); err == nil && !os.SameFile(cur.info, between) {
		f, err := openLogFile(path + ".1")
		if err != nil {
			return cur, err
		}
		if f != nil {
			err = f.drain(emit)
			f.close()
			if err != nil {
				return cur, err
			}
		}
	}
	cur.close()
	return followLog(path, nil, emit)
}

// lastBoot returns the number of the last boot recorded in the log at
// path, the current file first, 0 if none is.
func lastBoot(path string) (uint64, error) {
	for _, p := range []string{path, path + ".1"} {
		f, err := openLogFile(p)
		if err != nil {
			return 0, err
		}
		if f == nil {
			continue
		}
		var last uint64
		found := false
		err = f.drain(func(rec console.Record) error {
			if n, err := console.BootOf(rec); err == nil {
				last, found = n, true
			}
			return nil
		})
		f.close()
		if err != nil || found {
			return last, err
		}
	}
	return 0, nil
}
