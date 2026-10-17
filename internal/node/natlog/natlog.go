// Package natlog keeps the node's log of IPv4 connections out of its
// networks, a JSON line each, so an abuse report's public port and time
// lead to a machine. Windows it may miss are written as gaps. See
// docs/decisions/networking.md, "Abuse protection".
package natlog

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Retention is how long a connection stays in the log, at least, while
// there is room: a day's file goes once this long has passed since the day
// ended.
const Retention = 30 * 24 * time.Hour

// pendingFor is how long a removed day's space may take to come back
// before the log says it has not: ZFS freed it in 5 s on the testbed.
const pendingFor = time.Minute

// FullAt is how full the log's filesystem may be, its own dataset with a
// quota; past it, Trim removes the oldest days.
const FullAt = 0.9

// Unknown stands for what the node could not tell of a connection, such as
// the machine of one its network no longer knew.
const Unknown = "unknown"

// What a Record is.
const (
	// EventNew is a connection starting.
	EventNew = "new"
	// EventGap is a window, From to To, in which connections may be missing
	// from the log.
	EventGap = "gap"
	// EventStop is the node ceasing to log; the next start writes the gap.
	EventStop = "stop"
	// EventRetention says the log now holds Days days, fewer than
	// Retention, as it filled its filesystem.
	EventRetention = "retention"
)

// Record is one line of the log. Addresses are strings, so Unknown fits; a
// port is absent for a protocol without ports.
type Record struct {
	Time  time.Time `json:"time"`
	Event string    `json:"event"`

	Protocol    string `json:"proto,omitempty"`
	PublicIP    string `json:"public_ip,omitempty"`
	PublicPort  uint16 `json:"public_port,omitempty"`
	DstIP       string `json:"dst_ip,omitempty"`
	DstPort     uint16 `json:"dst_port,omitempty"`
	Project     string `json:"project,omitempty"`
	Network     string `json:"network,omitempty"`
	NetworkUID  string `json:"network_uid,omitempty"`
	Machine     string `json:"machine,omitempty"`
	MachineUID  string `json:"machine_uid,omitempty"`
	InnerIP     string `json:"inner_ip,omitempty"`
	InnerPort   uint16 `json:"inner_port,omitempty"`
	TransitIP   string `json:"transit_ip,omitempty"`
	TransitPort uint16 `json:"transit_port,omitempty"`

	From   *time.Time `json:"from,omitempty"`
	To     *time.Time `json:"to,omitempty"`
	Days   int        `json:"days,omitempty"`
	Reason string     `json:"reason,omitempty"`
}

const (
	filePrefix = "nat-"
	fileSuffix = ".jsonl"
	gzSuffix   = ".gz"
	tmpSuffix  = gzSuffix + ".tmp"
	dayLayout  = "2006-01-02"
	// hourLayout names a file: one an hour, compressed once the hour is
	// over, so a day being flooded takes its compressed size.
	hourLayout = "2006-01-02-15"
)

// Options are what a Log is opened with; zero values take the real ones.
type Options struct {
	Now func() time.Time
	// Usage says how many bytes of the log's filesystem are used, of how
	// many; by default, statfs's.
	Usage func() (used, total uint64, err error)
	// Create opens a day's file to append to; by default, 0600.
	Create func(path string) (io.WriteCloser, error)
	// Failing is told, under the log's lock, when writing starts failing
	// and when it works again: once each.
	Failing func(failing bool, err error)
	// Sync makes what was removed count as free; by default, syncfs, as
	// ZFS frees a file's space only when its transaction group commits.
	Sync func() error
	// Warn is told, once each, when removed days' space does not come
	// back within pendingFor, and when it does after all.
	Warn func(msg string)
}

// Log is the log in one directory, root's alone. It is safe for concurrent
// use.
type Log struct {
	dir  string
	o    Options
	last time.Time

	mu sync.Mutex
	f  io.WriteCloser
	// hour is the hour being written, f its file once opened: nothing is
	// written to an earlier hour, nor is this one compressed.
	hour string
	// failFrom is when writing started failing, with failErr; zero while it
	// works.
	failFrom time.Time
	failErr  error
	// held are records to write before the next one, once a write works:
	// what Trim says of the days it removed for a write the quota refused.
	held []Record
	// written counts the bytes written, which a trim takes into account
	// when it looks whether removed days' space came back.
	written int64
	busy    sync.WaitGroup
	// maint serializes Maintain and Trim, and guards pending: the bytes
	// Trim removed that usage has not shown free yet, when it read
	// pendingAt with pendingWritten written. ZFS frees a removed file's
	// space seconds later.
	maint          sync.Mutex
	pending        int64
	pendingAt      uint64
	pendingWritten int64
	pendingTime    time.Time
	// stuck says the pending space did not come back within pendingFor,
	// which Warn was told of.
	stuck bool
}

// Open opens the log in dir, made root's alone if it is not.
func Open(dir string, o Options) (*Log, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Usage == nil {
		o.Usage = func() (uint64, uint64, error) { return statfsUsage(dir) }
	}
	if o.Create == nil {
		o.Create = func(path string) (io.WriteCloser, error) {
			return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		}
	}
	if o.Failing == nil {
		o.Failing = func(bool, error) {}
	}
	if o.Warn == nil {
		o.Warn = func(string) {}
	}
	if o.Sync == nil {
		o.Sync = func() error { return syncfs(dir) }
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Only if it is not: on a full dataset even a chmod is refused, and the
	// log must open to make room.
	if fi, err := os.Stat(dir); err != nil {
		return nil, err
	} else if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if err := settle(dir); err != nil {
		return nil, err
	}
	last, err := lastTime(dir)
	if err != nil {
		return nil, err
	}
	// No record is written to an hour before this one: those may be
	// compressed.
	return &Log{dir: dir, o: o, last: last, hour: o.Now().UTC().Format(hourLayout)}, nil
}

// Last is the time of the log's last record when it was opened; zero for
// none.
func (l *Log) Last() time.Time { return l.last }

func syncfs(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return unix.Syncfs(int(f.Fd()))
}

func statfsUsage(dir string) (used, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	total = st.Blocks * uint64(st.Bsize)
	return total - st.Bavail*uint64(st.Bsize), total, nil
}

// Write appends r to the file of its day. When it fails, the oldest days
// are removed if the filesystem is full and it is tried once more; once
// writing works again, the gap since it failed is written first.
func (l *Log) Write(r Record) error {
	r.Time = r.Time.UTC()
	err := l.tryWrite(r)
	if err != nil {
		// A quota refuses a write before usage shows it full: then the
		// oldest day goes whatever usage says.
		_ = l.trim(errors.Is(err, unix.EDQUOT) || errors.Is(err, unix.ENOSPC))
		err = l.tryWrite(r)
	}
	if err != nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.failFrom.IsZero() {
			l.failFrom, l.failErr = r.Time, err
			l.o.Failing(true, err)
		}
	}
	return err
}

func (l *Log) tryWrite(r Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.failFrom.IsZero() {
		from, to := l.failFrom, r.Time
		gap := Record{Time: r.Time, Event: EventGap, From: &from, To: &to, Reason: "the log could not be written: " + l.failErr.Error()}
		if err := l.writeLocked(gap); err != nil {
			return err
		}
		l.failFrom, l.failErr = time.Time{}, nil
		l.o.Failing(false, nil)
	}
	if err := l.writeHeldLocked(); err != nil {
		return err
	}
	return l.writeLocked(r)
}

// writeHeldLocked writes the held records, keeping those it cannot.
func (l *Log) writeHeldLocked() error {
	for len(l.held) > 0 {
		if err := l.writeLocked(l.held[0]); err != nil {
			return err
		}
		l.held = l.held[1:]
	}
	return nil
}

func (l *Log) writeLocked(r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// A record late for an hour the log has moved on from goes to the
	// hour being written: the one before may be compressed, or being.
	hour := max(r.Time.Format(hourLayout), l.hour)
	if l.f == nil || hour != l.hour {
		if l.f != nil {
			_ = l.f.Close()
			l.f = nil
			// The hour before is done: compress it, without holding up
			// the connections being logged.
			l.busy.Go(func() { _ = l.Maintain() })
		}
		f, err := l.o.Create(filepath.Join(l.dir, fileName(hour)))
		if err != nil {
			return err
		}
		l.f, l.hour = f, hour
	}
	n, err := l.f.Write(b)
	l.written += int64(n)
	if err != nil {
		// Opened again at the next write, which may have room.
		_ = l.f.Close()
		l.f = nil
		return err
	}
	return nil
}

// Close writes that the node stops logging, and closes the log.
func (l *Log) Close(reason string) error {
	err := l.Write(Record{Time: l.o.Now(), Event: EventStop, Reason: reason})
	l.mu.Lock()
	if l.f != nil {
		err = errors.Join(err, l.f.Close())
		l.f = nil
	}
	l.mu.Unlock()
	l.busy.Wait()
	return err
}

// openHour is the hour being written.
func (l *Log) openHour() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hour
}

// openDay is the day being written, if any.
func (l *Log) openDay() string {
	if h := l.openHour(); h != "" {
		return h[:len(dayLayout)]
	}
	return ""
}

// Maintain compresses every hour's file before the current one, and
// removes the days past Retention.
func (l *Log) Maintain() error {
	l.maint.Lock()
	defer l.maint.Unlock()
	open := l.openHour()
	now := l.o.Now().UTC()
	thisHour := now.Format(hourLayout)
	days, err := listDays(l.dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range days {
		start, err := time.Parse(dayLayout, d.day)
		if err != nil {
			continue
		}
		if !now.Before(start.AddDate(0, 0, 1).Add(Retention)) {
			errs = append(errs, d.remove())
			continue
		}
		// The hour being written stays as it is until the next is.
		for _, p := range d.parts {
			if p.stem < thisHour && p.stem != open && p.plain != "" {
				errs = append(errs, compress(p.plain))
			}
		}
	}
	return errors.Join(errs...)
}

// Trim removes the oldest days, never today's or the one being written,
// while the filesystem is fuller than FullAt, and then writes how many
// days the log holds.
func (l *Log) Trim() error { return l.trim(false) }

// trim is Trim, removing at least one day if full says the filesystem
// refused a write.
func (l *Log) trim(full bool) error {
	l.maint.Lock()
	defer l.maint.Unlock()
	open := l.openDay()
	now := l.o.Now().UTC()
	today := now.Format(dayLayout)
	raw, total, err := l.o.Usage()
	if err != nil {
		return err
	}
	// What was removed counts as free until usage shows it so; the days
	// to remove follow from their own sizes, not usage measured again.
	used := raw
	l.mu.Lock()
	written := l.written
	l.mu.Unlock()
	if l.pending > 0 {
		// Usage then, less what was removed, more what was written since,
		// as a flood writes faster than a removal shows: the space came
		// back once usage is not past that by half of what was removed.
		expect := int64(l.pendingAt) - l.pending + (written - l.pendingWritten)
		if int64(raw) <= expect+l.pending/2 {
			l.pending = 0
			if l.stuck {
				l.stuck = false
				l.o.Warn("the removed days' space came back: days are removed past the cap again")
			}
		} else {
			used -= min(uint64(l.pending), used)
			// A refused write waits for that room, rather than take
			// another day; so does the cap if it never comes, as
			// removing more would free nothing either.
			full = false
			if !l.stuck && now.Sub(l.pendingTime) > pendingFor {
				l.stuck = true
				l.o.Warn(fmt.Sprintf("%d bytes of removed days have not come back %s later, past what was written since (is something else holding the dataset's space?): they count as free until they do, so days are removed only for usage past the cap without them, and a write the quota refuses takes none",
					l.pending, now.Sub(l.pendingTime).Round(time.Second)))
			}
		}
	}
	var excess int64
	if total > 0 {
		excess = int64(used) - int64(FullAt*float64(total))
	}
	days, err := listDays(l.dir)
	if err != nil {
		return err
	}
	removed := 0
	var freed int64
	for _, d := range days {
		if freed >= excess && (!full || removed > 0) {
			break
		}
		if d.day == today || d.day == open {
			continue
		}
		size := d.size()
		if err := d.remove(); err != nil {
			return err
		}
		freed += size
		removed++
	}
	if removed > 0 {
		l.pending, l.pendingAt, l.pendingWritten, l.pendingTime = l.pending+freed, raw, written, now
		_ = l.o.Sync()
	}
	if removed == 0 {
		return nil
	}
	if days, err = listDays(l.dir); err != nil {
		return err
	}
	kept := 0
	if len(days) > 0 {
		if oldest, err := time.Parse(dayLayout, days[0].day); err == nil {
			kept = int(now.Truncate(24*time.Hour).Sub(oldest)/(24*time.Hour)) + 1
		}
	}
	// Written now if it can be, else before the next record that is.
	l.mu.Lock()
	defer l.mu.Unlock()
	l.held = append(l.held, Record{Time: now, Event: EventRetention, Days: kept,
		Reason: fmt.Sprintf("the log's filesystem was %d%% full: the oldest %d day(s) were removed", int(FullAt*100), removed)})
	if l.failFrom.IsZero() {
		return l.writeHeldLocked()
	}
	return nil
}

// fileName is the file of an hour, or of a day as written before hours.
func fileName(stem string) string { return filePrefix + stem + fileSuffix }

// part is one hour's file, or a whole day's as written before hours: the
// plain one, the compressed one, or both while it is compressed.
type part struct {
	stem      string
	plain, gz string
}

// day is one day's parts, in order, and all their files.
type day struct {
	day   string
	parts []part
	paths []string
}

// size is what the day's files take on disk.
func (d day) size() int64 {
	var n int64
	for _, p := range d.paths {
		var st unix.Stat_t
		if unix.Stat(p, &st) == nil {
			n += st.Blocks * 512
		}
	}
	return n
}

// remove removes the day's files.
func (d day) remove() error {
	var errs []error
	for _, p := range d.paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// listDays lists the log's days, oldest first.
func listDays(dir string) ([]day, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	by := map[string]*day{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, filePrefix) || !e.Type().IsRegular() {
			continue
		}
		rest := strings.TrimPrefix(name, filePrefix)
		var stem string
		var gz bool
		switch {
		case strings.HasSuffix(rest, fileSuffix+gzSuffix):
			stem, gz = strings.TrimSuffix(rest, fileSuffix+gzSuffix), true
		case strings.HasSuffix(rest, fileSuffix):
			stem = strings.TrimSuffix(rest, fileSuffix)
		default:
			continue
		}
		if _, err := time.Parse(hourLayout, stem); err != nil {
			if _, err := time.Parse(dayLayout, stem); err != nil {
				continue
			}
		}
		d := stem[:len(dayLayout)]
		if by[d] == nil {
			by[d] = &day{day: d}
		}
		pt := by[d].part(stem)
		p := filepath.Join(dir, name)
		if gz {
			pt.gz = p
		} else {
			pt.plain = p
		}
		by[d].paths = append(by[d].paths, p)
	}
	out := make([]day, 0, len(by))
	for _, d := range by {
		slices.SortFunc(d.parts, func(a, b part) int { return strings.Compare(a.stem, b.stem) })
		out = append(out, *d)
	}
	slices.SortFunc(out, func(a, b day) int { return strings.Compare(a.day, b.day) })
	return out, nil
}

// part is the day's part of stem, added if it has none.
func (d *day) part(stem string) *part {
	for i := range d.parts {
		if d.parts[i].stem == stem {
			return &d.parts[i]
		}
	}
	d.parts = append(d.parts, part{stem: stem})
	return &d.parts[len(d.parts)-1]
}

// compress replaces a part's plain file with its gzip: written aside,
// synced, renamed into place and the directory synced, and only then the
// plain file removed, so a crash leaves one whole or the other, or both,
// which settle sorts out.
func compress(plain string) error {
	in, err := os.Open(plain)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp := plain + tmpSuffix
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(out)
	_, err = io.Copy(zw, in)
	err = errors.Join(err, zw.Close(), out.Sync(), out.Close())
	if err == nil {
		err = os.Rename(tmp, plain+gzSuffix)
	}
	if err == nil {
		err = syncDir(filepath.Dir(plain))
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Remove(plain)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// settle finishes what a compression a crash cut short left: a temporary
// file goes; of an hour with both its gzip and its plain file, the plain
// one goes if the gzip holds just what it does, else the gzip goes and the
// hour is compressed again in time.
func settle(dir string) error {
	tmps, _ := filepath.Glob(filepath.Join(dir, filePrefix+"*"+tmpSuffix))
	for _, p := range tmps {
		_ = os.Remove(p)
	}
	days, err := listDays(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range days {
		for _, p := range d.parts {
			if p.plain == "" || p.gz == "" {
				continue
			}
			if same, err := gzipHolds(p.gz, p.plain); err == nil && same {
				errs = append(errs, os.Remove(p.plain))
			} else {
				errs = append(errs, os.Remove(p.gz))
			}
		}
	}
	return errors.Join(errs...)
}

// gzipHolds says whether gz decompresses whole to plain's content.
// Both are streamed, a chunk at a time, so an hour of gigabytes takes no
// more memory than a small one.
func gzipHolds(gz, plain string) (bool, error) {
	p, err := os.Open(plain)
	if err != nil {
		return false, err
	}
	defer func() { _ = p.Close() }()
	f, err := os.Open(gz)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return false, err
	}
	return sameStream(zr, p)
}

// sameStream says whether a and b read the same bytes to their ends, a to
// a clean one: a gzip cut short, even only its trailer, is an error.
func sameStream(a, b io.Reader) (bool, error) {
	bufA, bufB := make([]byte, tailChunk), make([]byte, tailChunk)
	for {
		na, errA := io.ReadFull(a, bufA)
		nb, errB := io.ReadFull(b, bufB)
		if errA != nil && errA != io.EOF && errA != io.ErrUnexpectedEOF {
			return false, errA
		}
		if errB != nil && errB != io.EOF && errB != io.ErrUnexpectedEOF {
			return false, errB
		}
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}
		if errA != nil || errB != nil {
			// A short read ends a stream: both must end together, a
			// cleanly, which ReadFull cannot tell from a cut gzip.
			if errA == nil || errB == nil {
				return false, nil
			}
			if _, err := a.Read(make([]byte, 1)); err != io.EOF {
				return false, err
			}
			return true, nil
		}
	}
}

// open reads a part: its compressed file, then its plain one, either of
// which may be gone, as one is compressed into the other meanwhile.
func (d part) open() (io.ReadCloser, error) {
	var readers []io.Reader
	var files []*os.File
	closeAll := func() {
		for _, f := range files {
			_ = f.Close()
		}
	}
	if d.gz != "" {
		f, err := os.Open(d.gz)
		switch {
		case err == nil:
			files = append(files, f)
			zr, err := gzip.NewReader(f)
			if err != nil {
				closeAll()
				return nil, err
			}
			readers = append(readers, zr)
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}
	}
	if d.plain != "" {
		f, err := os.Open(d.plain)
		switch {
		case err == nil:
			files = append(files, f)
			readers = append(readers, f)
		case !errors.Is(err, fs.ErrNotExist):
			closeAll()
			return nil, err
		}
	}
	return readCloser{io.MultiReader(readers...), files}, nil
}

type readCloser struct {
	io.Reader
	files []*os.File
}

func (r readCloser) Close() error {
	var errs []error
	for _, f := range r.files {
		errs = append(errs, f.Close())
	}
	return errors.Join(errs...)
}

// lastTime is the time of the log's last record; zero for none.
func lastTime(dir string) (time.Time, error) {
	days, err := listDays(dir)
	if err != nil {
		return time.Time{}, err
	}
	for i := len(days) - 1; i >= 0; i-- {
		last, err := days[i].lastTime()
		if err != nil {
			return time.Time{}, err
		}
		if !last.IsZero() {
			return last, nil
		}
	}
	return time.Time{}, nil
}

// tailChunk is how much lastTime reads of a file's end at a time.
const tailChunk = 64 << 10

// lastTime is the time of a day's last record, zero for none: that of its
// last part holding one.
func (d day) lastTime() (time.Time, error) {
	for i := len(d.parts) - 1; i >= 0; i-- {
		t, err := d.parts[i].lastTime()
		if err != nil || !t.IsZero() {
			return t, err
		}
	}
	return time.Time{}, nil
}

// lastTime is the time of a part's last record, zero for none: from the
// end of its plain file, the newer, or else through its compressed one,
// which cannot be read from its end, only its last lines parsed.
func (d part) lastTime() (time.Time, error) {
	if d.plain != "" {
		t, err := plainLastTime(d.plain)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, err
		}
		if !t.IsZero() {
			return t, nil
		}
	}
	if d.gz == "" {
		return time.Time{}, nil
	}
	rc, err := part{gz: d.gz}.open()
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = rc.Close() }()
	var tail []byte
	trimmed := false
	buf := make([]byte, tailChunk)
	for {
		n, err := rc.Read(buf)
		tail = append(tail, buf[:n]...)
		if len(tail) > 2*tailChunk {
			tail, trimmed = tail[len(tail)-tailChunk:], true
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return time.Time{}, err
		}
	}
	t, _ := lastRecordTime(tail, !trimmed)
	return t, nil
}

// plainLastTime is the time of the last record of a plain file, read from
// its end.
func plainLastTime(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return time.Time{}, err
	}
	for size := int64(tailChunk); ; size *= 2 {
		start := max(fi.Size()-size, 0)
		b := make([]byte, fi.Size()-start)
		if _, err := f.ReadAt(b, start); err != nil && err != io.EOF {
			return time.Time{}, err
		}
		if t, ok := lastRecordTime(b, start == 0); ok || start == 0 {
			return t, nil
		}
	}
}

// lastRecordTime is the time of the last whole record in b; its first line
// is whole only if whole says so.
func lastRecordTime(b []byte, whole bool) (time.Time, bool) {
	lines := bytes.Split(b, []byte("\n"))
	if !whole && len(lines) > 0 {
		lines = lines[1:]
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var r Record
		if json.Unmarshal(bytes.TrimSpace(lines[i]), &r) == nil && !r.Time.IsZero() {
			return r.Time, true
		}
	}
	return time.Time{}, false
}

// scan calls f with each record of a day, in order. A line that is not a
// record, as one a crash cut short, is skipped.
func scan(d day, f func(Record)) error {
	for _, p := range d.parts {
		if err := scanPart(p, f); err != nil {
			return err
		}
	}
	return nil
}

func scanPart(d part, f func(Record)) error {
	rc, err := d.open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	s := bufio.NewScanner(rc)
	s.Buffer(make([]byte, 64<<10), 1<<20)
	for s.Scan() {
		var r Record
		if json.Unmarshal(bytes.TrimSpace(s.Bytes()), &r) == nil && !r.Time.IsZero() {
			f(r)
		}
	}
	return s.Err()
}

// Query is what Lookup searches for: the connections from a public source
// port starting between From and To, and the gaps touching that window.
type Query struct {
	Port uint16
	// IP, if set, is the public address too.
	IP       string
	From, To time.Time
}

// Oldest is the start of the oldest day the log holds; zero for none.
// Earlier days were removed: past Retention, or as the log filled its
// filesystem.
func Oldest(dir string) (time.Time, error) {
	days, err := listDays(dir)
	if err != nil || len(days) == 0 {
		return time.Time{}, err
	}
	return time.Parse(dayLayout, days[0].day)
}

// Lookup returns the records Query asks for, oldest first: the
// connections, and the gaps and retention records in the window.
func Lookup(dir string, q Query) ([]Record, error) {
	if q.Port == 0 {
		return nil, errors.New("a public port is needed")
	}
	if q.To.Before(q.From) {
		return nil, fmt.Errorf("the window ends (%s) before it starts (%s)", q.To, q.From)
	}
	days, err := listDays(dir)
	if err != nil {
		return nil, err
	}
	// A record late for the last hour of a day may be in the next day's
	// first.
	first, last := q.From.UTC().Format(dayLayout), q.To.Add(time.Hour).UTC().Format(dayLayout)
	var out []Record
	for _, d := range days {
		if d.day < first || d.day > last {
			continue
		}
		err := scan(d, func(r Record) {
			switch r.Event {
			case EventNew:
				if r.PublicPort == q.Port && (q.IP == "" || r.PublicIP == q.IP) && !r.Time.Before(q.From) && !r.Time.After(q.To) {
					out = append(out, r)
				}
			case EventRetention:
				if !r.Time.Before(q.From) && !r.Time.After(q.To) {
					out = append(out, r)
				}
			case EventGap:
				if r.To == nil {
					return
				}
				from := q.From
				if r.From != nil {
					from = *r.From
				}
				if !r.To.Before(q.From) && !from.After(q.To) {
					out = append(out, r)
				}
			}
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
