package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// stagerFor is a stager in a fresh directory expecting uid's upload of
// size bytes, and what it counted.
func stagerFor(t *testing.T, dir, uid string, size int64) (*stager, *[]bool) {
	t.Helper()
	var mu sync.Mutex
	var counted []bool
	s, err := newStager(dir, func(got string, whole bool) {
		mu.Lock()
		defer mu.Unlock()
		if got == uid {
			counted = append(counted, whole)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	s.expect(uid, size)
	return s, &counted
}

func TestAnUploadIsStagedChunkByChunk(t *testing.T) {
	dir := t.TempDir()
	file := bytes.Repeat([]byte("0123456789abcdef"), 1000)
	size := int64(len(file))
	s, counted := stagerFor(t, dir, "img1", size)
	ctx := t.Context()
	write := func(off, n int64, sha string, body []byte) (int64, error) {
		return s.write(ctx, "img1", off, n, sha, bytes.NewReader(body))
	}
	if n, err := write(0, 6000, hexSum(file[:6000]), file[:6000]); err != nil || n != 6000 {
		t.Fatalf("the first chunk: %d %v", n, err)
	}

	var off *offsetError
	if n, err := write(0, 1000, hexSum(file[:1000]), file[:1000]); !errors.As(err, &off) || off.received != 6000 || n != 6000 {
		t.Errorf("a chunk again from 0: %d %v, want an offset error at 6000", n, err)
	}
	if _, err := write(7000, 1000, hexSum(file[7000:8000]), file[7000:8000]); !errors.As(err, &off) {
		t.Errorf("a chunk past where the upload is: %v, want an offset error", err)
	}
	if _, err := write(6000, 11000, hexSum(file[6000:]), file[6000:]); !errors.Is(err, errChunkShape) {
		t.Errorf("a chunk past the file's size: %v, want it malformed", err)
	}
	if _, err := write(6000, 1000, hexSum(file[6000:7000]), file[6000:6500]); !errors.Is(err, errChunkShape) {
		t.Errorf("a chunk short of its length: %v, want it malformed", err)
	}
	if _, err := write(6000, 1000, hexSum(file[6000:7000]), file[6000:7001]); !errors.Is(err, errChunkShape) {
		t.Errorf("a chunk past its length: %v, want it malformed", err)
	}
	// A chunk whose bytes are not its sha256's is dropped, though written
	// over the file: what counts stays as it was, and its hash with it.
	bad := bytes.Repeat([]byte{'x'}, 1000)
	if n, err := write(6000, 1000, hexSum(file[6000:7000]), bad); !errors.Is(err, errChunkDigest) || n != 6000 {
		t.Errorf("a chunk not matching its sha256: %d %v, want it dropped at 6000", n, err)
	}
	if got, err := s.received("img1"); err != nil || got != 6000 {
		t.Errorf("received %d %v after the chunks refused, want 6000", got, err)
	}

	// The agent restarts: a new stager on the same directory goes on, its
	// hash right.
	s, counted2 := stagerFor(t, dir, "img1", size)
	if n, err := write(6000, size-6000, hexSum(file[6000:]), file[6000:]); err != nil || n != size {
		t.Fatalf("the last chunk after a restart: %d %v", n, err)
	}
	sum, whole, err := s.sum("img1", size)
	if err != nil || !whole || sum != hexSum(file) {
		t.Errorf("sum %s %t %v, want the file's %s", sum, whole, err, hexSum(file))
	}
	// Zeros, the tenant's bytes at UploadOffset, then zeros to the next
	// 4 KiB and 4 KiB more: the hypervisor reads neither end of theirs.
	b, err := os.ReadFile(s.path("img1"))
	if err != nil {
		t.Fatal(err)
	}
	end := driver.UploadOffset + len(file)
	if len(b) != int(driver.UploadedLength(size)) || len(b)%4096 != 0 || len(b)-end < 4096 {
		t.Errorf("the staged file is %d bytes, want %d: whole 4 KiB, 4 KiB at least after the file's %d", len(b), driver.UploadedLength(size), len(file))
	}
	if !bytes.Equal(b[:driver.UploadOffset], make([]byte, driver.UploadOffset)) || !bytes.Equal(b[driver.UploadOffset:end], file) ||
		!bytes.Equal(b[end:], make([]byte, len(b)-end)) {
		t.Error("the staged file is not zeros, the file uploaded, and zeros")
	}
	if f, err := s.whole("img1"); err != nil || f.Offset != driver.UploadOffset || f.Size != size || f.Path != s.path("img1") {
		t.Errorf("the whole file %+v %v", f, err)
	}
	if !slices.Equal(*counted, []bool{false}) || !slices.Equal(*counted2, []bool{true}) {
		t.Errorf("counted %v then %v, want a chunk, then the file whole", *counted, *counted2)
	}
	if staged, err := s.staged(); err != nil || !slices.Equal(staged, []string{"img1"}) {
		t.Errorf("staged %v %v", staged, err)
	}
	if err := s.remove("img1"); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left %v once removed", left)
	}
	if _, err := s.received("img1"); !errors.Is(err, errNotExpected) {
		t.Errorf("received of an upload removed: %v, want not expected", err)
	}
}

func TestAnUploadTakesOneChunkAtATime(t *testing.T) {
	s, _ := stagerFor(t, t.TempDir(), "img1", 2000)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := s.write(t.Context(), "img1", 0, 1000, hexSum(make([]byte, 1000)), pr)
		done <- err
	}()
	if _, err := pw.Write(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.write(t.Context(), "img1", 0, 1000, hexSum(make([]byte, 1000)), bytes.NewReader(make([]byte, 1000))); !errors.Is(err, errChunkBusy) {
		t.Errorf("a second chunk while one is written: %v, want busy", err)
	}
	// Removing the upload ends the chunk under way, which leaves nothing.
	if err := s.remove("img1"); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	if err := <-done; err == nil {
		t.Error("a chunk of an upload removed while written counted")
	}
	if _, err := os.Stat(s.statePath("img1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the state of an upload removed while written: %v", err)
	}
	if _, err := s.write(context.Background(), "img2", 0, 1, hexSum([]byte{0}), bytes.NewReader([]byte{0})); !errors.Is(err, errNotExpected) {
		t.Errorf("a chunk of an upload not expected: %v", err)
	}
}

// removeAtEnd reads data, then, asked for more, removes uid's upload, as
// the sweep may as a chunk's last bytes come.
type removeAtEnd struct {
	r   io.Reader
	s   *stager
	uid string
}

func (e removeAtEnd) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		_ = e.s.remove(e.uid)
	}
	return n, err
}

func TestAChunkWhoseUploadWentLeavesNothing(t *testing.T) {
	s, counted := stagerFor(t, t.TempDir(), "img1", 2000)
	data := make([]byte, 1000)
	if _, err := s.write(t.Context(), "img1", 0, 1000, hexSum(data), removeAtEnd{bytes.NewReader(data), s, "img1"}); err == nil {
		t.Error("a chunk of an upload removed as it ended counted")
	}
	if _, err := os.Stat(s.statePath("img1")); !errors.Is(err, os.ErrNotExist) || len(*counted) != 0 {
		t.Errorf("an upload removed as a chunk ended kept its state (%v), counted %v", err, *counted)
	}
}

// TestCountedChunksAreReportedAFewTimesASecond: a throttle runs at once,
// then at most once per its period, however many pokes come, and runs
// once more after the last.
func TestCountedChunksAreReportedAFewTimesASecond(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var runs []time.Duration
		start := time.Now()
		th := newThrottle(clockwork.NewRealClock(), reportEvery, func() {
			mu.Lock()
			defer mu.Unlock()
			runs = append(runs, time.Since(start))
		})
		for range 100 {
			th.poke()
			time.Sleep(10 * time.Millisecond) //nolint:forbidigo // virtual time
		}
		time.Sleep(time.Second) //nolint:forbidigo // virtual time
		synctest.Wait()
		mu.Lock()
		defer mu.Unlock()
		if len(runs) < 4 || len(runs) > 6 {
			t.Fatalf("ran %d times over a second of pokes: %v", len(runs), runs)
		}
		for i := 1; i < len(runs); i++ {
			if gap := runs[i] - runs[i-1]; gap < reportEvery {
				t.Errorf("ran %v after the last run, less than %v: %v", gap, reportEvery, runs)
			}
		}
		if last := runs[len(runs)-1]; last < 990*time.Millisecond {
			t.Errorf("last ran at %v, before the last poke at 990ms", last)
		}
	})
}

// TestKeptStateThatDoesNotFitItsImageIsRefused: kept state of another
// size than the image declares, or counting bytes outside the file, or
// that is no state at all, is refused rather than resumed from; state that
// fits is resumed from.
func TestKeptStateThatDoesNotFitItsImageIsRefused(t *testing.T) {
	dir := t.TempDir()
	s, _ := stagerFor(t, dir, "img1", 1000)
	if _, err := s.write(t.Context(), "img1", 0, 400, hexSum(make([]byte, 400)), bytes.NewReader(make([]byte, 400))); err != nil {
		t.Fatal(err)
	}
	if st, err := s.state("img1", 1000); err != nil || st.Received != 400 {
		t.Fatalf("the state kept: %+v %v", st, err)
	}
	if _, err := s.state("img1", 999); err == nil {
		t.Error("state kept of a file of 1000 bytes, read for one of 999")
	}
	kept, err := os.ReadFile(s.statePath("img1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, received := range []string{"-1", "1001"} {
		bad := bytes.Replace(kept, []byte(`"Received":400`), []byte(`"Received":`+received), 1)
		if bytes.Equal(bad, kept) {
			t.Fatalf("no Received in %s", kept)
		}
		if err := os.WriteFile(s.statePath("img1"), bad, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.state("img1", 1000); err == nil {
			t.Errorf("state counting %s of 1000 bytes was taken", received)
		}
	}
	if err := os.WriteFile(s.statePath("img1"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.state("img1", 1000); err == nil {
		t.Error("state that is no JSON was taken")
	}
}
