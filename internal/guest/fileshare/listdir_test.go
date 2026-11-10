package fileshare

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sys/unix"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

func names(es []DirEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

func write(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListSortsAndDescribesEntries(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "b.txt"), 42)
	if err := os.Mkdir(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/proc/self/root", filepath.Join(root, "c")); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "b.txt"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	got, more, err := NewDirs(root).List(context.Background(), nil, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if more || !slices.Equal(names(got), []string{"a", "b.txt", "c"}) {
		t.Fatalf("got %v more=%v", names(got), more)
	}
	want := []struct {
		typ  guestv1.FileType
		size uint64
	}{
		{guestv1.FileType_FILE_TYPE_DIRECTORY, 0},
		{guestv1.FileType_FILE_TYPE_REGULAR, 42},
		{guestv1.FileType_FILE_TYPE_SYMLINK, 0},
	}
	for i, w := range want {
		if got[i].Type != w.typ || got[i].Size != w.size {
			t.Errorf("%s: type %v size %d, want %v %d", got[i].Name, got[i].Type, got[i].Size, w.typ, w.size)
		}
	}
	if !got[1].ModTime.Equal(mtime) {
		t.Errorf("b.txt modified %v, want %v", got[1].ModTime, mtime)
	}
}

func TestListPagesThroughEveryNameOnce(t *testing.T) {
	root := t.TempDir()
	var want []string
	for i := range 2500 {
		n := fmt.Sprintf("f%05d", i)
		want = append(want, n)
		write(t, filepath.Join(root, n), 0)
	}
	d := NewDirs(root)
	var seen []string
	after := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("too many pages")
		}
		page, more, err := d.List(context.Background(), nil, after, 1000)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, names(page)...)
		if !more {
			break
		}
		after = page[len(page)-1].Name
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("paged %d names, want %d in order", len(seen), len(want))
	}
}

func TestListKeepsNamesThatAreNotUTF8(t *testing.T) {
	root := t.TempDir()
	bad := "caf\xe9\xff"
	write(t, filepath.Join(root, bad), 1)
	got, _, err := NewDirs(root).List(context.Background(), nil, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != bad {
		t.Fatalf("got %q, want %q", names(got), bad)
	}
}

func TestListResolvesNoSymlinkAndStaysBeneath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret"), 1)
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"out": outside, "proc": "/proc", "in": "dir", "abs": "/"} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "file"), 1)
	d := NewDirs(root)
	for _, tc := range []struct {
		path []string
		code connect.Code
	}{
		{[]string{"out"}, connect.CodeFailedPrecondition},
		{[]string{"proc"}, connect.CodeFailedPrecondition},
		{[]string{"proc", "self"}, connect.CodeFailedPrecondition},
		{[]string{"in"}, connect.CodeFailedPrecondition},
		{[]string{"abs", "etc"}, connect.CodeFailedPrecondition},
		{[]string{"file"}, connect.CodeFailedPrecondition},
		{[]string{"missing"}, connect.CodeNotFound},
		{[]string{".."}, connect.CodeInvalidArgument},
		{[]string{"."}, connect.CodeInvalidArgument},
		{[]string{""}, connect.CodeInvalidArgument},
		{[]string{"dir/../.."}, connect.CodeInvalidArgument},
		{[]string{"a\x00b"}, connect.CodeInvalidArgument},
	} {
		_, _, err := d.List(context.Background(), tc.path, "", 0)
		if connect.CodeOf(err) != tc.code {
			t.Errorf("%q: %v, want %v", tc.path, err, tc.code)
		}
	}
	if _, _, err := d.List(context.Background(), []string{"dir"}, "", 0); err != nil {
		t.Errorf("a real directory: %v", err)
	}
}

func TestListRefusesBadLimitsAndDeepPaths(t *testing.T) {
	d := NewDirs(t.TempDir())
	for _, limit := range []int{-1, MaxListLimit + 1} {
		if _, _, err := d.List(context.Background(), nil, "", limit); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("limit %d: %v", limit, err)
		}
	}
	deep := slices.Repeat([]string{"a"}, MaxPathDepth+1)
	if _, _, err := d.List(context.Background(), deep, "", 0); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("deep path: %v", err)
	}
}

func TestListWaitsItsTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := NewDirs(t.TempDir())
		d.turn <- struct{}{}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, _, err := d.List(ctx, nil, "", 0)
		if connect.CodeOf(err) != connect.CodeDeadlineExceeded || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v, want a deadline while another lists", err)
		}
	})
}

// TestListAfterNamesThatWentReadsOn: a page whose every name was removed
// before its lstat is read again after them, so a page with more after it
// is never empty; a name gone from a page is left out of it.
func TestListAfterNamesThatWentReadsOn(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		write(t, filepath.Join(root, n), 0)
	}
	d := NewDirs(root)
	gone := map[string]bool{"a": true, "b": true, "d": true}
	stat := d.lstat
	d.lstat = func(fd int, name string, st *unix.Stat_t) error {
		if gone[name] {
			return unix.ENOENT
		}
		return stat(fd, name, st)
	}
	got, more, err := d.List(context.Background(), nil, "", 2)
	if err != nil || !slices.Equal(names(got), []string{"c"}) || !more {
		t.Fatalf("the first page is %v, more %v, %v; want c, more", names(got), more, err)
	}
	got, more, err = d.List(context.Background(), nil, "c", 2)
	if err != nil || !slices.Equal(names(got), []string{"e"}) || more {
		t.Errorf("the second page is %v, more %v, %v; want e alone", names(got), more, err)
	}
}

// TestListRefusesAPathTooLong for the kernel as a bad request.
func TestListRefusesAPathTooLong(t *testing.T) {
	long := slices.Repeat([]string{strings.Repeat("a", 255)}, 20)
	if _, _, err := NewDirs(t.TempDir()).List(context.Background(), long, "", 0); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("got %v", err)
	}
}

// TestCloseWaitsForTheListingAndRefusesMore: Close returns only once the
// listing under way has ended, and every later listing is refused.
func TestCloseWaitsForTheListingAndRefusesMore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := NewDirs(t.TempDir())
		d.turn <- struct{}{}
		closed := make(chan struct{})
		go func() { d.Close(); close(closed) }()
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("Close returned while a listing held its turn")
		default:
		}
		<-d.turn
		<-closed
		if _, _, err := d.List(t.Context(), nil, "", 0); connect.CodeOf(err) != connect.CodeUnavailable {
			t.Errorf("a listing after Close: %v", err)
		}
	})
}

func TestFirstAfterRefusesHugeDirectories(t *testing.T) {
	if testing.Short() {
		t.Skip("makes 100,001 files")
	}
	root := t.TempDir()
	for i := range MaxDirEntries + 1 {
		f, err := os.Create(filepath.Join(root, fmt.Sprint(i)))
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	if _, _, err := NewDirs(root).List(context.Background(), nil, "", 0); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("got %v", err)
	}
}
