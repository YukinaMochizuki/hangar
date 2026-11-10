package fileshare

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/timestamppb"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

// Bounds of a listing.
const (
	// MaxDirEntries is the most entries a directory may have to be listed:
	// each page reads the whole directory to sort it.
	MaxDirEntries = 100_000
	// DefaultListLimit and MaxListLimit bound a page.
	DefaultListLimit = 500
	MaxListLimit     = 1000
	// MaxPathDepth bounds the names in a path.
	MaxPathDepth = 256
)

// Dirs lists the directories beneath a root, one listing at a time, since
// the server lists on the vCPU its clients' requests need too.
type Dirs struct {
	root string
	// turn is held by the listing under way; closed is read and written
	// holding it.
	turn   chan struct{}
	closed bool
	// lstat is unix.Fstatat with AT_SYMLINK_NOFOLLOW but in tests.
	lstat func(dirfd int, name string, st *unix.Stat_t) error
}

// NewDirs lists the directories beneath root.
func NewDirs(root string) *Dirs {
	return &Dirs{root: root, turn: make(chan struct{}, 1), lstat: func(dirfd int, name string, st *unix.Stat_t) error {
		return unix.Fstatat(dirfd, name, st, unix.AT_SYMLINK_NOFOLLOW)
	}}
}

// Close waits for the listing under way and refuses every later one, so
// nothing holds the root open once it returns.
func (d *Dirs) Close() {
	d.turn <- struct{}{}
	d.closed = true
	<-d.turn
}

// DirEntry is an entry of a directory as lstat sees it.
type DirEntry struct {
	Name    string
	Type    guestv1.FileType
	Size    uint64
	ModTime time.Time
}

// List lists the entries of the directory path names beneath the root
// whose names sort after after, at most limit of them, and whether more
// sort after the last. The path is resolved through no symbolic link and
// never leaves the root. Its errors are connect errors.
func (d *Dirs) List(ctx context.Context, path []string, after string, limit int) ([]DirEntry, bool, error) {
	if err := checkPath(path); err != nil {
		return nil, false, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if limit < 0 || limit > MaxListLimit {
		return nil, false, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("limit %d is not 0 to %d", limit, MaxListLimit))
	}
	if limit == 0 {
		limit = DefaultListLimit
	}
	select {
	case d.turn <- struct{}{}:
	case <-ctx.Done():
		return nil, false, connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("waiting for another listing: %w", ctx.Err()))
	}
	defer func() { <-d.turn }()
	if d.closed {
		return nil, false, connect.NewError(connect.CodeUnavailable, errors.New("the share's server is stopping"))
	}

	fd, err := d.open(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = unix.Close(fd) }()
	// A page whose every name was removed between reading the directory and
	// its lstat is read again after those names, so a page with more after
	// it is never empty.
	for {
		names, more, err := firstAfter(ctx, fd, after, limit)
		if err != nil {
			return nil, false, err
		}
		entries := make([]DirEntry, 0, len(names))
		for _, name := range names {
			var st unix.Stat_t
			if err := d.lstat(fd, name, &st); errors.Is(err, unix.ENOENT) {
				continue
			} else if err != nil {
				return nil, false, connect.NewError(connect.CodeInternal, fmt.Errorf("lstat %q: %w", name, err))
			}
			entries = append(entries, entryOf(name, &st))
		}
		if len(entries) > 0 || !more {
			return entries, more, nil
		}
		after = names[len(names)-1]
		if _, err := unix.Seek(fd, 0, 0); err != nil {
			return nil, false, connect.NewError(connect.CodeInternal, fmt.Errorf("rewinding the directory: %w", err))
		}
	}
}

// checkPath refuses a path whose names could mean anything but one entry
// of the directory before, or that the kernel would not take whole.
func checkPath(path []string) error {
	if len(path) > MaxPathDepth {
		return fmt.Errorf("the path has %d names, past %d", len(path), MaxPathDepth)
	}
	for _, name := range path {
		if err := checkName(name); err != nil {
			return fmt.Errorf("path: %w", err)
		}
	}
	if n := len(strings.Join(path, "/")); n >= unix.PathMax {
		return fmt.Errorf("the path is %d bytes, past %d", n, unix.PathMax-1)
	}
	return nil
}

func checkName(name string) error {
	switch {
	case name == "", name == ".", name == "..":
		return fmt.Errorf("name %q is not an entry's", name)
	case strings.ContainsAny(name, "/\x00"):
		return fmt.Errorf("name %q holds a slash or NUL", name)
	case len(name) > unix.NAME_MAX:
		return fmt.Errorf("a name is %d bytes, past %d", len(name), unix.NAME_MAX)
	}
	return nil
}

// open opens the directory path names beneath the root, for reading.
func (d *Dirs) open(path []string) (int, error) {
	rootFD, err := unix.Open(d.root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, connect.NewError(connect.CodeUnavailable, fmt.Errorf("opening the share: %w", err))
	}
	defer func() { _ = unix.Close(rootFD) }()
	rel := "."
	if len(path) > 0 {
		rel = strings.Join(path, "/")
	}
	how := &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
	}
	for range 3 {
		fd, err := unix.Openat2(rootFD, rel, how)
		switch {
		case err == nil:
			return fd, nil
		case errors.Is(err, unix.EAGAIN):
			// A rename raced the walk; RESOLVE_BENEATH asks for another.
			continue
		case errors.Is(err, unix.ENOENT):
			return -1, connect.NewError(connect.CodeNotFound, errors.New("no such directory"))
		case errors.Is(err, unix.ENOTDIR):
			return -1, connect.NewError(connect.CodeFailedPrecondition, errors.New("not a directory"))
		case errors.Is(err, unix.ELOOP):
			return -1, connect.NewError(connect.CodeFailedPrecondition, errors.New("the path goes through a symbolic link"))
		case errors.Is(err, unix.ENAMETOOLONG):
			return -1, connect.NewError(connect.CodeInvalidArgument, errors.New("the path is too long"))
		case errors.Is(err, unix.EXDEV):
			return -1, connect.NewError(connect.CodeFailedPrecondition, errors.New("the path leaves the share"))
		default:
			return -1, connect.NewError(connect.CodeInternal, fmt.Errorf("opening the directory: %w", err))
		}
	}
	return -1, connect.NewError(connect.CodeUnavailable, errors.New("the directory kept moving while it was opened"))
}

// firstAfter reads the whole directory fd and returns its first limit
// names, sorted as bytes, of those after after, and whether there were more.
func firstAfter(ctx context.Context, fd int, after string, limit int) ([]string, bool, error) {
	h := &maxHeap{}
	more := false
	total := 0
	buf := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, connect.NewError(connect.CodeDeadlineExceeded, err)
		}
		n, err := unix.Getdents(fd, buf)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, false, connect.NewError(connect.CodeInternal, fmt.Errorf("reading the directory: %w", err))
		}
		if n == 0 {
			break
		}
		var names []string
		_, _, names = unix.ParseDirent(buf[:n], -1, names)
		for _, name := range names {
			if total++; total > MaxDirEntries {
				return nil, false, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("the directory has more than %d entries", MaxDirEntries))
			}
			if name <= after {
				continue
			}
			if h.Len() < limit {
				heap.Push(h, name)
				continue
			}
			more = true
			if name < (*h)[0] {
				(*h)[0] = name
				heap.Fix(h, 0)
			}
		}
	}
	out := make([]string, h.Len())
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(h).(string)
	}
	return out, more, nil
}

// maxHeap holds the smallest names seen, the largest of them on top.
type maxHeap []string

func (h maxHeap) Len() int           { return len(h) }
func (h maxHeap) Less(i, j int) bool { return h[i] > h[j] }
func (h maxHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *maxHeap) Push(x any)        { *h = append(*h, x.(string)) }
func (h *maxHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

func entryOf(name string, st *unix.Stat_t) DirEntry {
	e := DirEntry{Name: name, ModTime: time.Unix(st.Mtim.Unix())}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		e.Type = guestv1.FileType_FILE_TYPE_REGULAR
		e.Size = uint64(st.Size)
	case unix.S_IFDIR:
		e.Type = guestv1.FileType_FILE_TYPE_DIRECTORY
	case unix.S_IFLNK:
		e.Type = guestv1.FileType_FILE_TYPE_SYMLINK
	default:
		e.Type = guestv1.FileType_FILE_TYPE_OTHER
	}
	return e
}

// ListDir lists one directory of the share.
func (s *Server) ListDir(ctx context.Context, req *connect.Request[guestv1.ListDirRequest]) (*connect.Response[guestv1.ListDirResponse], error) {
	if s.dirs == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the share is not mounted"))
	}
	path := make([]string, len(req.Msg.GetPath()))
	for i, p := range req.Msg.GetPath() {
		path[i] = string(p)
	}
	entries, more, err := s.dirs.List(ctx, path, string(req.Msg.GetAfter()), int(req.Msg.GetLimit()))
	if err != nil {
		return nil, err
	}
	res := &guestv1.ListDirResponse{More: more}
	for _, e := range entries {
		res.Entries = append(res.Entries, &guestv1.DirEntry{Name: []byte(e.Name), Type: e.Type, Size: e.Size, ModifyTime: timestamppb.New(e.ModTime)})
	}
	return connect.NewResponse(res), nil
}
