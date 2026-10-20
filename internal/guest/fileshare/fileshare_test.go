package fileshare

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

// fakeServer records what the server writes to nfsd, and its waits, on a
// boot clock at 300ms.
type fakeServer struct {
	*Server
	wrote, flushed []string
	failFlush      bool
}

func newFake(t *testing.T) *fakeServer {
	f := &fakeServer{}
	f.Server = &Server{
		mu:     &countingLocker{},
		logf:   t.Logf,
		uptime: func() time.Duration { return 300 * time.Millisecond },
		sleep:  func(d time.Duration) { f.wrote = append(f.wrote, "sleep "+d.String()) },
		ctl:    func(name, value string) error { f.wrote = append(f.wrote, name+"="+value); return nil },
		flush: func(cache string) error {
			if f.failFlush {
				return errors.New("no such file")
			}
			f.flushed = append(f.flushed, cache)
			return nil
		},
	}
	return f
}

func setExport(t *testing.T, f *fakeServer, e *guestv1.FileShareExport) error {
	_, err := f.SetExport(t.Context(), connect.NewRequest(&guestv1.SetExportRequest{Export: e}))
	return err
}

// TestNFSDStartsWithTheFirstExport: nothing reaches nfsd before an export,
// then its settings in the order it needs them, once, and never in the
// guest's first second, whose clients nfsd takes for expired.
func TestNFSDStartsWithTheFirstExport(t *testing.T) {
	f := newFake(t)
	if len(f.wrote) != 0 {
		t.Fatalf("nfsd was set up before any export: %q", f.wrote)
	}
	if err := f.Export(&guestv1.FileShareExport{Squash: squashNone, Clients: clients(rw("10.0.0.2"))}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"sleep 800ms",
		"versions=-3 +4 -4.0 -4.1 +4.2",
		"max_block_size=1048576",
		"nfsv4leasetime=15",
		"nfsv4gracetime=15",
		"portlist=tcp 2049",
		"threads=8",
	}
	if !slices.Equal(f.wrote, want) {
		t.Errorf("nfsd was set up with\n%q\nwant\n%q", f.wrote, want)
	}
	if err := setExport(t, f, &guestv1.FileShareExport{Squash: squashNone}); err != nil {
		t.Fatal(err)
	}
	if len(f.wrote) != len(want) {
		t.Errorf("a second export set nfsd up again: %q", f.wrote[len(want):])
	}
	if err := f.Stop(); err == nil || f.wrote[len(f.wrote)-1] != "threads=0" {
		// Stop's unmount fails here, there being no share; nfsd stops first.
		t.Errorf("Stop wrote %q (%v), want threads=0 last", f.wrote, err)
	}
}

// TestEachExportFlushesTheCaches: an export takes over every cache at once,
// and one that cannot is refused before it changes anything.
func TestEachExportFlushesTheCaches(t *testing.T) {
	f := newFake(t)
	first := &guestv1.FileShareExport{Squash: squashNone, Clients: clients(rw("10.0.0.2"), rw("10.0.0.3"))}
	if err := setExport(t, f, first); err != nil {
		t.Fatal(err)
	}
	if err := setExport(t, f, &guestv1.FileShareExport{Squash: squashNone, Clients: clients(ro("10.0.0.2"))}); err != nil {
		t.Fatal(err)
	}
	if want := slices.Concat(upcallCaches, upcallCaches); !slices.Equal(f.flushed, want) {
		t.Errorf("flushed %q, want %q", f.flushed, want)
	}
	now := time.Now()
	if got, _ := answer("nfsd.export", "c-10.0.0.3 /share\n", f.export, Root, now); len(words(got)) != 3 {
		t.Errorf("a client the export left out is still answered: %q", got)
	}
	if got, _ := answer("nfsd.export", "c-10.0.0.2 /share\n", f.export, Root, now); words(got)[3] != "9217" {
		t.Errorf("a client the export made read-only is answered %q", got)
	}

	f.failFlush = true
	if err := setExport(t, f, first); connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("an export whose flush failed: %v, want Internal", err)
	}
}

// TestABadExportChangesNothing: SetExport refuses an export nfsd would not
// take, keeping the last one, unflushed; and from the boot disk it never
// starts nfsd.
func TestABadExportChangesNothing(t *testing.T) {
	f := newFake(t)
	if err := setExport(t, f, &guestv1.FileShareExport{Squash: squashNone, Clients: clients(rw("10.0.0.2"))}); err != nil {
		t.Fatal(err)
	}
	before, flushed := f.export, len(f.flushed)
	for name, e := range badExports {
		if err := setExport(t, f, e); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
		if f.export != before || len(f.flushed) != flushed {
			t.Errorf("%s: the export was replaced (%t) or the caches flushed (%d)", name, f.export != before, len(f.flushed)-flushed)
		}
	}

	fresh := newFake(t)
	for name, e := range badExports {
		if err := fresh.Export(e); err == nil {
			t.Errorf("the boot disk's export %s: taken", name)
		}
	}
	if len(fresh.wrote) != 0 || fresh.export != nil {
		t.Errorf("a bad boot disk export set nfsd up: %q", fresh.wrote)
	}
}

// TestAnAnswerNeverOutlivesItsExport: an upcall's answer is written, and an
// export replaced and flushed, under one lock, so no answer from an export
// replaced lands in a cache after its flush.
func TestAnAnswerNeverOutlivesItsExport(t *testing.T) {
	f := newFake(t)
	lk := f.mu.(*countingLocker)
	var replies, flushes, unheld []string
	f.reply = func(_ int, b []byte) error {
		replies = append(replies, string(b))
		if !lk.held() {
			unheld = append(unheld, "an answer")
		}
		return nil
	}
	f.flush = func(c string) error {
		flushes = append(flushes, c)
		if !lk.held() {
			unheld = append(unheld, "a flush of "+c)
		}
		return nil
	}
	if err := setExport(t, f, &guestv1.FileShareExport{Squash: squashNone, Clients: clients(rw("10.0.0.2"))}); err != nil {
		t.Fatal(err)
	}
	f.answer(0, "nfsd.export", "c-10.0.0.2 /share\n")
	if len(replies) != 1 || len(flushes) != len(upcallCaches) {
		t.Fatalf("%d answers and %d flushes, want 1 and %d", len(replies), len(flushes), len(upcallCaches))
	}
	if len(unheld) != 0 {
		t.Errorf("without the lock: %q", unheld)
	}
}

// TestAnExportIsSwappedAndFlushedAtOnce: the new export is in place when
// each cache is flushed, in the one hold of the lock that put it there, so
// no upcall answered in between from the old export outlives the flush.
func TestAnExportIsSwappedAndFlushedAtOnce(t *testing.T) {
	f := newFake(t)
	lk := f.mu.(*countingLocker)
	if err := setExport(t, f, &guestv1.FileShareExport{Squash: squashNone, Clients: clients(rw("10.0.0.2"))}); err != nil {
		t.Fatal(err)
	}
	before := lk.locks
	var wrong []string
	f.flush = func(c string) error {
		if _, swapped := f.export.readOnly[netip.MustParseAddr("10.0.0.3")]; !swapped {
			wrong = append(wrong, c+": flushed before the export was replaced")
		}
		if lk.locks != before+1 || !lk.held() {
			wrong = append(wrong, fmt.Sprintf("%s: flushed in hold %d of the lock, held %t; the export was replaced in hold %d", c, lk.locks-before, lk.held(), 1))
		}
		return nil
	}
	if err := setExport(t, f, &guestv1.FileShareExport{Squash: squashNone, Clients: clients(rw("10.0.0.3"))}); err != nil {
		t.Fatal(err)
	}
	if len(wrong) != 0 {
		t.Errorf("%q", wrong)
	}
}

// TestNoExportStartsAStoppedServer: once Stop has begun, an export changes
// nothing that would start nfsd again.
func TestNoExportStartsAStoppedServer(t *testing.T) {
	f := newFake(t)
	_ = f.Stop() // its unmount fails here, there being no share
	err := setExport(t, f, &guestv1.FileShareExport{Squash: squashNone, Clients: clients(rw("10.0.0.2"))})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("SetExport after Stop: %v, want FailedPrecondition", err)
	}
	if err := f.Export(&guestv1.FileShareExport{Squash: squashNone}); err == nil {
		t.Error("an export from the boot disk after Stop was taken")
	}
	if len(f.wrote) != 0 {
		t.Errorf("nfsd was set up after Stop: %q", f.wrote)
	}
}

// countingLocker is the server's lock, counting how often it is taken.
type countingLocker struct {
	mu    sync.Mutex
	locks int
}

func (l *countingLocker) Lock()   { l.mu.Lock(); l.locks++ }
func (l *countingLocker) Unlock() { l.mu.Unlock() }

// held reports whether the lock is held, by someone.
func (l *countingLocker) held() bool {
	if l.mu.TryLock() {
		l.mu.Unlock()
		return false
	}
	return true
}
