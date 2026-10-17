package natlog

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// lagFS is a filesystem whose usage is what the files in dir take, but for
// a removed file's space, which it shows free only after lag readings, as
// ZFS frees it seconds later.
type lagFS struct {
	dir   string
	total uint64
	lag   int
	known map[string]uint64
	freed []struct {
		size  uint64
		after int
	}
}

func (f *lagFS) size() (now uint64, files map[string]uint64) {
	files = map[string]uint64{}
	entries, _ := os.ReadDir(f.dir)
	for _, e := range entries {
		var st syscall.Stat_t
		if syscall.Stat(filepath.Join(f.dir, e.Name()), &st) == nil {
			files[e.Name()] = uint64(st.Blocks) * 512
			now += uint64(st.Blocks) * 512
		}
	}
	return now, files
}

// held is what removed files still take.
func (f *lagFS) held() uint64 {
	var n uint64
	for _, x := range f.freed {
		n += x.size
	}
	return n
}

// usage is one reading: files removed since the last one are held for lag
// readings.
func (f *lagFS) usage() (uint64, uint64, error) {
	now, files := f.size()
	for name, size := range f.known {
		if _, ok := files[name]; !ok {
			f.freed = append(f.freed, struct {
				size  uint64
				after int
			}{size, f.lag})
		}
	}
	f.known = files
	kept := f.freed[:0]
	for _, x := range f.freed {
		if x.after > 0 {
			x.after--
			kept = append(kept, x)
		}
	}
	f.freed = kept
	return now + f.held(), f.total, nil
}

// quotaFile refuses a write that would take the filesystem past its total.
type quotaFile struct {
	f  *os.File
	fs *lagFS
}

func (q quotaFile) Write(b []byte) (int, error) {
	now, _ := q.fs.size()
	if now+q.fs.held()+uint64(len(b)) > q.fs.total {
		return 0, syscall.EDQUOT
	}
	return q.f.Write(b)
}

func (q quotaFile) Close() error { return q.f.Close() }

// TestAFloodKeepsTrimming: while today grows faster than a removed day's
// space shows free, as a flood makes it, each trim still sees the space
// come back, counting what was written since, and goes on removing the
// oldest days, never today, so that no write is refused while older days
// are left to remove.
func TestAFloodKeepsTrimming(t *testing.T) {
	dir := t.TempDir()
	pad := make([]byte, 32<<10)
	old := []string{"2026-10-05", "2026-10-06", "2026-10-07", "2026-10-08", "2026-10-09", "2026-10-10", "2026-10-11", "2026-10-12", "2026-10-13", "2026-10-14"}
	for _, d := range old {
		if err := os.WriteFile(filepath.Join(dir, fileName(d)), pad, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	days, _ := listDays(dir)
	daySize := uint64(days[0].size())
	// Room for the old days and a little more: today's flood must push
	// them out.
	fs := &lagFS{dir: dir, total: 12 * daySize, lag: 2}
	now := time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)
	var warned []string
	l, err := Open(dir, Options{
		Now:   func() time.Time { return now },
		Usage: fs.usage,
		Create: func(path string) (io.WriteCloser, error) {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
			return quotaFile{f, fs}, err
		},
		Warn: func(msg string) { warned = append(warned, msg) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close("test") }()
	refused, written := 0, 0
	// Each round, a burst half a day's size, then the minute's trim.
	for round := range 16 {
		for i := range int(daySize/2) / 400 {
			if err := l.Write(newRecord(now, uint16(round*1000+i), "flood")); err != nil {
				refused++
			} else {
				written++
			}
		}
		if err := l.Trim(); err != nil {
			t.Fatal(err)
		}
	}
	left, _ := listDays(dir)
	var today day
	for _, d := range left {
		if d.day == "2026-10-15" {
			today = d
		}
	}
	kept := 0
	_ = scan(today, func(r Record) {
		if r.Event == EventNew {
			kept++
		}
	})
	t.Logf("%d old days left of %d, %d written, %d refused", len(left)-1, len(old), written, refused)
	if refused > 0 {
		t.Errorf("%d writes refused while older days were left to remove", refused)
	}
	if kept != written {
		t.Errorf("today holds %d of the %d connections written", kept, written)
	}
	if len(left)-1 >= len(old)-3 {
		t.Errorf("%d old days left of %d: the trims stopped removing them", len(left)-1, len(old))
	}
	if len(warned) > 0 {
		t.Errorf("warned %q, though the space came back each time", warned)
	}
}
