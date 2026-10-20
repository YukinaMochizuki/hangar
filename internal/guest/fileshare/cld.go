package fileshare

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"golang.org/x/sys/unix"
)

// nfsd's client tracking upcalls (include/uapi/linux/nfsd/cld.h, version
// 1), nfsdcld's otherwise: which clients hold state, so that after a
// restart those alone may reclaim it in the grace period.
const (
	cldCreate     = 0
	cldRemove     = 1
	cldCheck      = 2
	cldGraceDone  = 3
	cldGraceStart = 4
	cldGetVersion = 5

	// cldMsgLen is struct cld_msg: version, command, status, xid, then a
	// union whose largest member is a name of up to 1024 bytes after its
	// 16-bit length.
	cldMsgLen  = 8 + 2 + cldNameMax
	cldNameMax = 1024
)

// records are the clients nfsd tracks, as kept on the share.
type records struct {
	// may reclaim after a restart.
	may map[string]bool
	// has state, established or reclaimed, since the grace period began;
	// it becomes may when the grace period ends.
	has map[string]bool
	// inGrace: the grace period began and has not ended. A restart before
	// it ends lets both sets reclaim.
	inGrace bool
	// age orders the clients of either set, the least recently created or
	// reclaimed first, for eviction; next is the next age to give.
	age  map[string]uint64
	next uint64
}

// The records are bounded, so the extended attribute never outgrows what
// ext4 holds (64 KiB): past either bound, the oldest clients are dropped,
// which costs them only their reclaim after a restart.
const (
	maxRecords     = 512
	maxRecordsSize = 60 << 10
)

// recordStore keeps the records where a restart finds them.
type recordStore interface {
	load() ([]byte, error)
	save([]byte) error
	// setAside keeps records that cannot be read where they are never
	// read again, or drops them.
	setAside([]byte) error
}

// tracker answers nfsd's client tracking upcalls, saving every change
// before it answers, since nfsd acts on the answer.
type tracker struct {
	store recordStore
	logf  func(string, ...any)

	mu  sync.Mutex
	rec records
}

// newTracker starts from the records store holds. Records it cannot read,
// corrupt or written by a newer version, never stop the server: they are
// set aside, and the grace period runs with no client to reclaim.
func newTracker(store recordStore, logf func(string, ...any)) *tracker {
	t := &tracker{store: store, logf: logf, rec: emptyRecords()}
	b, err := store.load()
	if err != nil {
		logf("hangar-init: the NFS client records cannot be read, so no client reclaims: %v", err)
		return t
	}
	rec, err := decodeRecords(b)
	if err != nil {
		logf("hangar-init: the NFS client records are set aside, so no client reclaims: %v", err)
		if err := store.setAside(b); err != nil {
			logf("hangar-init: setting the NFS client records aside: %v", err)
		}
		return t
	}
	t.rec = rec
	return t
}

// handle answers one message from nfsd, returning the replies to write in
// order: for GraceStart, one per client that may reclaim, then its own.
func (t *tracker) handle(msg []byte) [][]byte {
	if len(msg) < 8 {
		return nil
	}
	m := make([]byte, cldMsgLen)
	copy(m, msg)
	reply := func(status int16) []byte {
		putStatus(m, status)
		return m
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	next := t.rec.clone()
	switch m[1] {
	case cldGetVersion:
		m[8] = 1
		return [][]byte{reply(0)}
	case cldCheck:
		if t.rec.may[cldName(m)] {
			return [][]byte{reply(0)}
		}
		return [][]byte{reply(-int16(unix.EACCES))}
	case cldCreate:
		id := cldName(m)
		next.has[id] = true
		if !next.inGrace {
			next.may[id] = true
		}
		next.age[id] = next.next
		next.next++
		if evicted := next.bound(); len(evicted) > 0 {
			t.logf("hangar-init: %d NFS client records dropped, the oldest, to make room: they cannot reclaim after a restart", len(evicted))
		}
	case cldRemove:
		id := cldName(m)
		delete(next.has, id)
		if !next.inGrace {
			delete(next.may, id)
		}
	case cldGraceStart:
		if next.inGrace {
			maps.Copy(next.may, next.has)
		}
		next.has = map[string]bool{}
		next.inGrace = true
	case cldGraceDone:
		next.may = maps.Clone(next.has)
		next.inGrace = false
	default:
		return [][]byte{reply(-int16(unix.EOPNOTSUPP))}
	}
	next.prune()
	if err := t.store.save(next.encode()); err != nil {
		t.logf("hangar-init: saving the NFS client records: %v", err)
		return [][]byte{reply(-int16(unix.EIO))}
	}
	t.rec = next
	var out [][]byte
	if m[1] == cldGraceStart {
		for _, id := range slices.Sorted(maps.Keys(next.may)) {
			c := make([]byte, cldMsgLen)
			copy(c, m[:8])
			putStatus(c, -int16(unix.EINPROGRESS))
			binary.LittleEndian.PutUint16(c[8:10], uint16(len(id)))
			copy(c[10:], id)
			out = append(out, c)
		}
		t.logf("hangar-init: NFS grace period: %d clients may reclaim", len(next.may))
	}
	return append(out, reply(0))
}

// putStatus sets a message's status, an errno negated as the kernel's are.
func putStatus(m []byte, status int16) {
	binary.LittleEndian.PutUint16(m[2:4], uint16(status))
}

func cldName(m []byte) string {
	n := min(int(binary.LittleEndian.Uint16(m[8:10])), cldNameMax)
	return string(m[10 : 10+n])
}

func emptyRecords() records {
	return records{may: map[string]bool{}, has: map[string]bool{}, age: map[string]uint64{}}
}

func (r records) clone() records {
	return records{may: maps.Clone(r.may), has: maps.Clone(r.has), inGrace: r.inGrace, age: maps.Clone(r.age), next: r.next}
}

// prune forgets the age of clients in neither set.
func (r records) prune() {
	for id := range r.age {
		if !r.may[id] && !r.has[id] {
			delete(r.age, id)
		}
	}
}

// byAge lists the clients of either set, the oldest first.
func (r records) byAge() []string {
	ids := slices.Collect(maps.Keys(r.age))
	slices.SortFunc(ids, func(a, b string) int { return cmp.Compare(r.age[a], r.age[b]) })
	return ids
}

// bound drops the oldest clients until the records fit both bounds,
// returning those dropped.
func (r records) bound() []string {
	r.prune()
	size := len(recordsMagic) + 1
	for id := range r.age {
		size += 3 + len(id)
	}
	var evicted []string
	for _, id := range r.byAge() {
		if len(r.age) <= maxRecords && size <= maxRecordsSize {
			break
		}
		delete(r.may, id)
		delete(r.has, id)
		delete(r.age, id)
		size -= 3 + len(id)
		evicted = append(evicted, id)
	}
	return evicted
}

// recordsMagic opens the encoding: its version, then whether in grace, then
// each client, the oldest first, as a byte of flags (1 may, 2 has), a
// 16-bit length and its name.
var recordsMagic = []byte("HNC1")

const (
	flagMay = 1
	flagHas = 2
)

func (r records) encode() []byte {
	var b bytes.Buffer
	b.Write(recordsMagic)
	if r.inGrace {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	for _, id := range r.byAge() {
		var f byte
		if r.may[id] {
			f |= flagMay
		}
		if r.has[id] {
			f |= flagHas
		}
		b.WriteByte(f)
		_ = binary.Write(&b, binary.BigEndian, uint16(len(id)))
		b.WriteString(id)
	}
	return b.Bytes()
}

func decodeRecords(b []byte) (records, error) {
	r := emptyRecords()
	if len(b) == 0 {
		return r, nil
	}
	if len(b) < len(recordsMagic)+1 || !bytes.Equal(b[:len(recordsMagic)], recordsMagic) {
		return r, errors.New("not records this version wrote")
	}
	switch b[len(recordsMagic)] {
	case 0:
	case 1:
		r.inGrace = true
	default:
		return r, errors.New("records with a grace flag this version never wrote")
	}
	for rest := b[len(recordsMagic)+1:]; len(rest) > 0; {
		if len(rest) < 3 {
			return r, errors.New("records cut short")
		}
		f, n := rest[0], int(binary.BigEndian.Uint16(rest[1:3]))
		if len(rest) < 3+n || n == 0 || n > cldNameMax || f&^(flagMay|flagHas) != 0 || f == 0 {
			return r, errors.New("records cut short or garbled")
		}
		id := string(rest[3 : 3+n])
		if _, dup := r.age[id]; dup {
			return r, errors.New("a client recorded twice")
		}
		r.may[id], r.has[id] = f&flagMay != 0, f&flagHas != 0
		if !r.may[id] {
			delete(r.may, id)
		}
		if !r.has[id] {
			delete(r.has, id)
		}
		r.age[id] = r.next
		r.next++
		rest = rest[3+n:]
	}
	return r, nil
}

// xattrStore keeps the records in a trusted.* extended attribute of the
// share's root: on the share's own disk, so they travel with it, and never
// shown to a client, as nfsd serves user.* attributes alone.
type xattrStore struct{ dir string }

const recordsXattr = "trusted.hangar.nfsd-clients"

func (s xattrStore) load() ([]byte, error) {
	n, err := unix.Getxattr(s.dir, recordsXattr, nil)
	if errors.Is(err, unix.ENODATA) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b := make([]byte, n)
	n, err = unix.Getxattr(s.dir, recordsXattr, b)
	return b[:n], err
}

const asideXattr = recordsXattr + ".bad"

// setAside moves records that cannot be read to their own attribute, kept
// for a person to look at, or, where it does not fit, drops them.
func (s xattrStore) setAside(b []byte) error {
	if err := unix.Setxattr(s.dir, asideXattr, b, 0); err != nil {
		err = fmt.Errorf("dropped, as they do not fit beside them: %w", err)
		return errors.Join(err, unix.Removexattr(s.dir, recordsXattr))
	}
	return unix.Removexattr(s.dir, recordsXattr)
}

// save returns once the records are on the disk.
func (s xattrStore) save(b []byte) error {
	if err := unix.Setxattr(s.dir, recordsXattr, b, 0); err != nil {
		return err
	}
	fd, err := unix.Open(s.dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	return unix.Fsync(fd)
}
