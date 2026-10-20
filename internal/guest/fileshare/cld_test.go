package fileshare

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// memStore keeps the records as the share would, or fails to.
type memStore struct {
	b, aside []byte
	fail     bool
	loadErr  error
}

func (m *memStore) load() ([]byte, error) { return m.b, m.loadErr }
func (m *memStore) setAside(b []byte) error {
	m.aside, m.b = b, nil
	return nil
}
func (m *memStore) save(b []byte) error {
	if m.fail {
		return errors.New("disk full")
	}
	m.b = append([]byte(nil), b...)
	return nil
}

func cldMsg(cmd byte, xid uint32, name string) []byte {
	m := make([]byte, cldMsgLen)
	m[0], m[1] = 1, cmd
	binary.LittleEndian.PutUint32(m[4:8], xid)
	binary.LittleEndian.PutUint16(m[8:10], uint16(len(name)))
	copy(m[10:], name)
	return m
}

func status(m []byte) int16 { return int16(binary.LittleEndian.Uint16(m[2:4])) }

// boot is a server starting on what store holds: its tracker, past nfsd's
// GetVersion and GraceStart, and the clients GraceStart said may reclaim.
func bootTracker(t *testing.T, store *memStore) (*tracker, []string) {
	t.Helper()
	tr := newTracker(store, t.Logf)
	v := tr.handle(cldMsg(cldGetVersion, 1, ""))
	if len(v) != 1 || status(v[0]) != 0 || v[0][8] != 1 {
		t.Fatalf("GetVersion: %v", v)
	}
	replies := tr.handle(cldMsg(cldGraceStart, 2, ""))
	var reclaim []string
	for i, r := range replies {
		if binary.LittleEndian.Uint32(r[4:8]) != 2 {
			t.Errorf("a GraceStart reply has xid %d", binary.LittleEndian.Uint32(r[4:8]))
		}
		if i == len(replies)-1 {
			if status(r) != 0 {
				t.Errorf("GraceStart ended with %d", status(r))
			}
			break
		}
		if status(r) != -int16(unix.EINPROGRESS) {
			t.Errorf("a client record came with %d", status(r))
		}
		reclaim = append(reclaim, cldName(r))
	}
	return tr, reclaim
}

func check(tr *tracker, name string) bool {
	r := tr.handle(cldMsg(cldCheck, 9, name))
	return len(r) == 1 && status(r[0]) == 0
}

func do(t *testing.T, tr *tracker, cmd byte, name string) {
	t.Helper()
	if r := tr.handle(cldMsg(cmd, 7, name)); len(r) != 1 || status(r[0]) != 0 {
		t.Fatalf("command %d %q: %v", cmd, name, r)
	}
}

// TestClientsReclaimAcrossRestarts: the clients with state when the server
// stops, and only those, may reclaim it when it starts again, a restart
// within the grace period included.
func TestClientsReclaimAcrossRestarts(t *testing.T) {
	store := &memStore{}
	tr, reclaim := bootTracker(t, store)
	if len(reclaim) != 0 {
		t.Fatalf("a new share has clients to reclaim: %q", reclaim)
	}
	do(t, tr, cldCreate, "a") // within the first grace period
	if check(tr, "a") {
		t.Error("a client with no state before may reclaim")
	}
	do(t, tr, cldGraceDone, "")
	do(t, tr, cldCreate, "b")
	do(t, tr, cldCreate, "gone")
	do(t, tr, cldRemove, "gone")

	tr, reclaim = bootTracker(t, store)
	if want := []string{"a", "b"}; !slices.Equal(reclaim, want) {
		t.Errorf("after a restart, %q may reclaim, want %q", reclaim, want)
	}
	if !check(tr, "a") || !check(tr, "b") || check(tr, "gone") || check(tr, "c") {
		t.Error("Check does not answer from the clients that may reclaim")
	}
	do(t, tr, cldCreate, "a") // a reclaims; b does not, yet
	do(t, tr, cldCreate, "c") // a client new in the grace period

	// Stopped again within the grace period: b keeps its chance, and c has
	// state to reclaim.
	tr, reclaim = bootTracker(t, store)
	if want := []string{"a", "b", "c"}; !slices.Equal(reclaim, want) {
		t.Errorf("after a restart in grace, %q may reclaim, want %q", reclaim, want)
	}
	do(t, tr, cldCreate, "b")
	do(t, tr, cldGraceDone, "")

	// a did not reclaim before the grace period ended: its state is gone.
	_, reclaim = bootTracker(t, store)
	if want := []string{"b"}; !slices.Equal(reclaim, want) {
		t.Errorf("after the grace period, %q may reclaim, want %q", reclaim, want)
	}
}

// TestARecordNotSavedIsNotAnswered: nfsd acts on an answer, so a change
// that cannot be saved fails, and changes nothing.
func TestARecordNotSavedIsNotAnswered(t *testing.T) {
	store := &memStore{}
	tr, _ := bootTracker(t, store)
	do(t, tr, cldGraceDone, "")
	store.fail = true
	if r := tr.handle(cldMsg(cldCreate, 3, "a")); len(r) != 1 || status(r[0]) != -int16(unix.EIO) {
		t.Errorf("an unsaved Create answered %v", r)
	}
	store.fail = false
	if check(tr, "a") {
		t.Error("an unsaved Create was kept")
	}
}

func TestRecordsReadBack(t *testing.T) {
	r := records{
		may: map[string]bool{"a": true, "Linux NFSv4.2 x/y": true}, has: map[string]bool{"a": true, "c": true}, inGrace: true,
		age: map[string]uint64{"c": 3, "a": 9, "Linux NFSv4.2 x/y": 5},
	}
	got, err := decodeRecords(r.encode())
	if err != nil || !got.inGrace || len(got.may) != 2 || !got.may["Linux NFSv4.2 x/y"] || len(got.has) != 2 || !got.has["c"] {
		t.Errorf("read back as %+v (%v)", got, err)
	}
	if got.age["c"] >= got.age["Linux NFSv4.2 x/y"] || got.age["Linux NFSv4.2 x/y"] >= got.age["a"] || got.next <= got.age["a"] {
		t.Errorf("read back with the ages %v, next %d; want c, then Linux NFSv4.2 x/y, then a, oldest first", got.age, got.next)
	}
	for _, b := range [][]byte{[]byte("{}"), append([]byte("HNC1\x00\x01\x00"), 'x')} {
		if _, err := decodeRecords(b); err == nil {
			t.Errorf("%q read as records", b)
		}
	}
}

// TestUnreadableRecordsAreSetAside: records the server cannot read never
// stop it: they are set aside, nobody reclaims, and tracking goes on.
func TestUnreadableRecordsAreSetAside(t *testing.T) {
	good := records{may: map[string]bool{"a": true}, has: map[string]bool{}, age: map[string]uint64{"a": 0}}.encode()
	for name, b := range map[string][]byte{
		"garbage":               []byte("{not records}"),
		"a newer version":       append([]byte("HNC2"), good[4:]...),
		"cut short":             good[:len(good)-1],
		"an unknown grace flag": append(append([]byte("HNC1"), 7), good[5:]...),
		"an unknown flag":       append(append([]byte("HNC1\x00"), 4), good[6:]...),
		"a client twice":        append(good, good[5:]...),
	} {
		t.Run(name, func(t *testing.T) {
			store := &memStore{b: b}
			tr, reclaim := bootTracker(t, store)
			if len(reclaim) != 0 || string(store.aside) != string(b) {
				t.Errorf("%q may reclaim, %q set aside", reclaim, store.aside)
			}
			do(t, tr, cldCreate, "c")
			do(t, tr, cldGraceDone, "")
			if _, reclaim := bootTracker(t, store); !slices.Equal(reclaim, []string{"c"}) {
				t.Errorf("after records were set aside, %q may reclaim, want c", reclaim)
			}
		})
	}
	store := &memStore{loadErr: unix.EIO}
	if _, reclaim := bootTracker(t, store); len(reclaim) != 0 {
		t.Errorf("records that cannot be read: %q may reclaim", reclaim)
	}
}

// TestRecordsAreBounded: past maxRecords clients, or maxRecordsSize bytes,
// the least recently created or reclaimed are dropped.
func TestRecordsAreBounded(t *testing.T) {
	store := &memStore{}
	tr, _ := bootTracker(t, store)
	do(t, tr, cldGraceDone, "")
	name := func(i int) string { return fmt.Sprintf("client-%04d", i) }
	for i := range maxRecords {
		do(t, tr, cldCreate, name(i))
	}
	do(t, tr, cldCreate, name(0)) // again, so it is the newest
	do(t, tr, cldCreate, name(maxRecords))
	_, reclaim := bootTracker(t, store)
	if len(reclaim) != maxRecords || slices.Contains(reclaim, name(1)) || !slices.Contains(reclaim, name(0)) || !slices.Contains(reclaim, name(maxRecords)) {
		t.Errorf("%d may reclaim (client-0001 %t, client-0000 %t), want %d without client-0001", len(reclaim), slices.Contains(reclaim, name(1)), slices.Contains(reclaim, name(0)), maxRecords)
	}

	store = &memStore{}
	tr, _ = bootTracker(t, store)
	do(t, tr, cldGraceDone, "")
	long := func(i int) string { return fmt.Sprintf("%04d", i) + strings.Repeat("x", cldNameMax-4) }
	for i := range 100 {
		do(t, tr, cldCreate, long(i))
	}
	if len(store.b) > maxRecordsSize {
		t.Errorf("the records are %d bytes, past %d", len(store.b), maxRecordsSize)
	}
	_, reclaim = bootTracker(t, store)
	if len(reclaim) < 50 || !slices.Contains(reclaim, long(99)) || slices.Contains(reclaim, long(0)) {
		t.Errorf("%d long names may reclaim; want the newest about 59", len(reclaim))
	}
}
