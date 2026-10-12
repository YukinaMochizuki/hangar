// Package ocilayer applies OCI image layers, uncompressed tar streams, to a
// directory. Layers come from tenants and are applied as root on the host,
// so they are hostile input: every path is resolved inside the directory
// as the image's own processes would see it, the directory being their /,
// and nothing is ever followed out of it. Device nodes are skipped, and
// what is written, and the work done, is bounded. Fetching and
// decompressing layers is the caller's. See docs/decisions/storage-zfs.md.
package ocilayer

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// Limits bound what an image may write, over all its layers, and the work
// applying it takes.
type Limits struct {
	// MaxBytes bounds the bytes written: file contents, names, symlink
	// targets and xattrs.
	MaxBytes int64
	// MaxEntries bounds the entries read and the nodes made, implicit dirs
	// and whiteouts included.
	MaxEntries int64
	// MaxDepth bounds the components of a name, and how deep dirs are made
	// inside the root. Zero means DefaultMaxDepth.
	MaxDepth int
	// MaxWork bounds the steps applying the image takes: each name looked
	// up, link read, node made or removed, and dir entry listed. Zero means
	// DefaultMaxWork.
	MaxWork int64
	// MaxXattrs bounds the xattrs kept, over all the image's entries: each
	// holds kernel memory while the unpacked tree lasts. Zero means
	// DefaultMaxXattrs.
	MaxXattrs int64
}

// Defaults of Limits, deeper and larger than any real image needs. What
// DefaultMaxWork costs at worst is measured in docs/decisions/storage-zfs.md.
const (
	DefaultMaxDepth  = 256
	DefaultMaxWork   = 15_000_000
	DefaultMaxXattrs = 1 << 16
)

// Bounds on one entry: its name, the bytes of a path made inside the root
// (PATH_MAX less room for where the root is), its xattrs, names and values,
// which a Linux xattr value never exceeds, and the symlinks it follows.
const (
	maxNameBytes  = 4096
	maxPathBytes  = 3072
	maxXattrBytes = 64 << 10
	maxHops       = 40
	// checkEvery is how many steps of work pass between context checks.
	checkEvery = 1024
	// entryWork is the steps an entry takes before its names are walked:
	// reading it and looking at what is at its name cost about as much as
	// ten lookups on the testbed.
	entryWork = 10
	// metaWork is the steps making a node with its owner, mode and times
	// takes, on the testbed.
	metaWork = 10
	// xattrWork is the steps setting one xattr takes, and xattrBytesPerStep
	// the bytes of its name and value a step more covers, on the testbed's
	// ZFS (docs/decisions/storage-zfs.md).
	xattrWork         = 14
	xattrBytesPerStep = 1024
	// paxBytesPerStep is the bytes of an entry's PAX records, which the tar
	// reader parses before Apply sees the entry, a step covers.
	paxBytesPerStep = 1024
	// maxCached is how many dirs a layer keeps open between entries.
	maxCached = 4096
)

// Whiteout names, from the OCI image spec's layer format.
const (
	whiteoutPrefix = ".wh."
	opaqueMarker   = ".wh..wh..opq"
)

// errEscape refuses a name that climbs out of the root by itself, as Docker
// refuses it.
var errEscape = invalid("the name leads out of the root")

// invalid is an error in what a layer holds, which trying again never
// fixes.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", driver.ErrInvalidImage, fmt.Sprintf(format, args...))
}

// Applier applies the layers of one image to a directory, in order.
type Applier struct {
	root                         *os.File
	lim                          Limits
	bytes, entries, work, xattrs int64
}

// Open prepares to apply layers to dir, which should be empty. dir is left
// as a root dir is, mode 0755, unless a layer says otherwise.
func Open(dir string, lim Limits) (*Applier, error) {
	if lim.MaxDepth == 0 {
		lim.MaxDepth = DefaultMaxDepth
	}
	if lim.MaxWork == 0 {
		lim.MaxWork = DefaultMaxWork
	}
	if lim.MaxXattrs == 0 {
		lim.MaxXattrs = DefaultMaxXattrs
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", dir, err)
	}
	root := os.NewFile(uintptr(fd), dir)
	if err := unix.Fchmod(fd, 0o755); err != nil {
		_ = root.Close()
		return nil, err
	}
	return &Applier{root: root, lim: lim}, nil
}

// Close releases the directory.
func (a *Applier) Close() error { return a.root.Close() }

// Work returns the steps of work applying has taken so far.
func (a *Applier) Work() int64 { return a.work }

// charge counts what is about to be made against the limits.
func (a *Applier) charge(entries, bytes int64) error {
	a.entries += entries
	a.bytes += bytes
	switch {
	case a.entries > a.lim.MaxEntries:
		return fmt.Errorf("%w: more than %d entries", driver.ErrImageTooLarge, a.lim.MaxEntries)
	case a.bytes > a.lim.MaxBytes:
		return fmt.Errorf("%w: more than %d bytes", driver.ErrImageTooLarge, a.lim.MaxBytes)
	}
	return nil
}

// step counts n steps of work, and stops the layer once the work is over
// its bound or the layer's context is done.
func (a *Applier) step(l *layer, n int64) error {
	before := a.work
	a.work += n
	if a.work > a.lim.MaxWork {
		return fmt.Errorf("%w: more than %d steps of work", driver.ErrImageTooLarge, a.lim.MaxWork)
	}
	if before/checkEvery != a.work/checkEvery {
		return l.ctx.Err()
	}
	return nil
}

// layer is what applying one layer keeps track of.
type layer struct {
	ctx context.Context
	// written holds the entries this layer made or changed, which an opaque
	// marker of the same layer keeps.
	written map[nodeName]bool
	// dirTimes holds the times this layer gave its dirs, kept as the layer
	// makes what they hold.
	dirTimes map[node][2]unix.Timespec
	// opaqued holds the dirs an opaque marker has emptied: once is enough.
	opaqued map[node]bool
	// top holds the dirs the layer has walked to, open, until anything is
	// removed: where each name leads, a symlink resolved once.
	top    *tdir
	cached int
}

// A tdir is a dir inside the root by its path without symlinks.
type tdir struct {
	parent *tdir
	f      *os.File
	// depth and pathLen are how many components, and bytes, its path has.
	depth, pathLen int
	// at holds what a name in it leads to: a dir, or where a symlink
	// leads.
	at map[string]*tdir
}

func (l *layer) forget() {
	var close func(d *tdir)
	close = func(d *tdir) {
		for name, sub := range d.at {
			if sub.parent == d && sub.f != nil {
				close(sub)
				_ = sub.f.Close()
			}
			delete(d.at, name)
		}
	}
	close(l.top)
	l.cached = 0
}

// node is a file on the host; nodeName, a name in a directory.
type node struct{ dev, ino uint64 }

type nodeName struct {
	dir  node
	name string
}

// Apply applies one layer, until ctx is done.
func (a *Applier) Apply(ctx context.Context, r io.Reader) error {
	l := &layer{
		ctx: ctx, written: map[nodeName]bool{}, dirTimes: map[node][2]unix.Timespec{}, opaqued: map[node]bool{},
		top: &tdir{f: a.root, at: map[string]*tdir{}},
	}
	defer l.forget()
	tr := tar.NewReader(ctxReader{ctx, r})
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return readFault(err)
		}
		if err := a.step(l, paxBytes(hdr)/paxBytesPerStep); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if err := a.charge(1, 0); err != nil {
			return err
		}
		if err := a.step(l, entryWork); err != nil {
			return err
		}
		if l.cached > maxCached {
			l.forget()
		}
		if err := a.entry(ctx, l, tr, hdr); err != nil {
			return fmt.Errorf("%q: %w", hdr.Name, err)
		}
	}
}

// readFault is an error reading the layer: the host's, such as EIO from the
// disk it is read from, which may pass, or else in what the layer holds
// (its compression or its tar), which never does.
func readFault(err error) error {
	var perr *os.PathError
	var errno unix.Errno
	if errors.As(err, &perr) || errors.As(err, &errno) {
		return fmt.Errorf("reading the layer: %w", err)
	}
	return fmt.Errorf("%w: reading the layer: %w", driver.ErrInvalidImage, err)
}

// paxBytes returns the bytes of an entry's PAX records.
func paxBytes(hdr *tar.Header) int64 {
	n := 0
	for k, v := range hdr.PAXRecords {
		n += len(k) + len(v)
	}
	return int64(n)
}

// readErr remembers the error its reader failed with, telling a failure to
// read a layer from one to write what it holds.
type readErr struct {
	r   io.Reader
	err error
}

func (r *readErr) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = err
	}
	return n, err
}

// ctxReader stops reading once its context is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// split returns the components of a name inside the root; none for the root
// itself. An absolute name is taken from the root.
func (a *Applier) split(name string) ([]string, error) {
	if strings.IndexByte(name, 0) >= 0 {
		return nil, invalid("a NUL in the name")
	}
	if len(name) > maxNameBytes {
		return nil, fmt.Errorf("%w: a name of %d bytes", driver.ErrImageTooLarge, len(name))
	}
	c := path.Clean(strings.TrimLeft(name, "/"))
	switch {
	case c == ".." || strings.HasPrefix(c, "../"):
		return nil, errEscape
	case c == "." || c == "/":
		return nil, nil
	}
	parts := strings.Split(c, "/")
	if len(parts) > a.lim.MaxDepth {
		return nil, fmt.Errorf("%w: a name deeper than %d", driver.ErrImageTooLarge, a.lim.MaxDepth)
	}
	return parts, nil
}

// fits fails unless a node named name can be made in d within the bounds on
// depth and path length.
func (a *Applier) fits(d *tdir, name string) error {
	if d.depth+1 > a.lim.MaxDepth || d.pathLen+1+len(name) > maxPathBytes {
		return fmt.Errorf("%w: a path deeper than %d or longer than %d bytes", driver.ErrImageTooLarge, a.lim.MaxDepth, maxPathBytes)
	}
	return nil
}

func (a *Applier) entry(ctx context.Context, l *layer, tr *tar.Reader, hdr *tar.Header) error {
	parts, err := a.split(hdr.Name)
	if err != nil {
		return err
	}
	if hdr.Uid < 0 || hdr.Gid < 0 || hdr.Uid >= 1<<32-1 || hdr.Gid >= 1<<32-1 {
		return invalid("owner %d:%d out of range", hdr.Uid, hdr.Gid)
	}
	if len(parts) == 0 {
		if hdr.Typeflag != tar.TypeDir {
			return invalid("the root is not a dir")
		}
		return a.setMeta(l, a.root, hdr)
	}
	parent, base := parts[:len(parts)-1], parts[len(parts)-1]
	if strings.HasPrefix(base, whiteoutPrefix) {
		return a.whiteout(l, parent, base)
	}
	td, err := a.dirOf(l, parent, true)
	if errors.Is(err, unix.ENOTDIR) {
		return invalid("a parent is not a dir")
	}
	if err != nil {
		return err
	}
	if err := a.fits(td, base); err != nil {
		return err
	}
	// Its own descriptor: a removal below closes the walked dirs.
	dir, err := dup(td.f)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	dirNode, err := nodeOf(dir)
	if err != nil {
		return err
	}
	defer l.keepTimes(dir, dirNode)

	var st unix.Stat_t
	err = unix.Fstatat(int(dir.Fd()), base, &st, unix.AT_SYMLINK_NOFOLLOW)
	exists := err == nil
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	isDir := exists && st.Mode&unix.S_IFMT == unix.S_IFDIR
	if exists && (hdr.Typeflag != tar.TypeDir || !isDir) {
		if err := a.removeAll(l, dir, base); err != nil {
			return err
		}
	}
	l.written[nodeName{dirNode, base}] = true
	if hdr.Typeflag == tar.TypeChar || hdr.Typeflag == tar.TypeBlock {
		return nil // never a device node on the host; what was there is gone
	}
	if err := a.charge(0, int64(len(base))); err != nil {
		return err
	}
	if err := a.step(l, metaWork); err != nil {
		return err
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		if !isDir {
			if err := unix.Mkdirat(int(dir.Fd()), base, 0o700); err != nil {
				return err
			}
		}
		f, err := openAt(dir, base, unix.O_RDONLY|unix.O_DIRECTORY)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		return a.setMeta(l, f, hdr)
	case tar.TypeReg:
		return a.writeFile(ctx, l, dir, base, tr, hdr)
	case tar.TypeSymlink:
		if err := a.charge(0, int64(len(hdr.Linkname))); err != nil {
			return err
		}
		if err := unix.Symlinkat(hdr.Linkname, int(dir.Fd()), base); err != nil {
			return err
		}
		if err := unix.Fchownat(int(dir.Fd()), base, hdr.Uid, hdr.Gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		return unix.UtimesNanoAt(int(dir.Fd()), base, times(hdr), unix.AT_SYMLINK_NOFOLLOW)
	case tar.TypeLink:
		return a.hardlink(l, dir, base, hdr.Linkname)
	case tar.TypeFifo:
		if err := unix.Mknodat(int(dir.Fd()), base, unix.S_IFIFO|0o600, 0); err != nil {
			return err
		}
		f, err := openAt(dir, base, unix.O_PATH)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		return a.setMeta(l, f, hdr)
	}
	return invalid("entries of type %q cannot be unpacked", hdr.Typeflag)
}

// writeFile makes a regular file of the entry's contents, within the bound.
func (a *Applier) writeFile(ctx context.Context, l *layer, dir *os.File, base string, tr *tar.Reader, hdr *tar.Header) error {
	if hdr.Size < 0 || hdr.Size > a.lim.MaxBytes-a.bytes {
		return fmt.Errorf("%w: more than %d bytes", driver.ErrImageTooLarge, a.lim.MaxBytes)
	}
	f, err := openAt(dir, base, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	r := &readErr{r: ctxReader{ctx, tr}}
	n, err := io.Copy(f, r)
	if cerr := a.charge(0, n); err == nil {
		err = cerr
	}
	if err != nil {
		if r.err != nil && ctx.Err() == nil {
			return readFault(r.err)
		}
		return err
	}
	return a.setMeta(l, f, hdr)
}

// hardlink links base in dir to target, found inside the root without
// following its last component, as link(2) does not.
func (a *Applier) hardlink(l *layer, dir *os.File, base, target string) error {
	parts, err := a.split(target)
	if err != nil {
		return fmt.Errorf("hardlink to %q: %w", target, err)
	}
	if len(parts) == 0 {
		return invalid("a hardlink to the root")
	}
	from, err := a.dirOf(l, parts[:len(parts)-1], false)
	if err != nil {
		return fmt.Errorf("hardlink to %q: %w", target, err)
	}
	if err := unix.Linkat(int(from.f.Fd()), parts[len(parts)-1], int(dir.Fd()), base, 0); err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EPERM) {
			return invalid("hardlink to %q: %v", target, err)
		}
		return fmt.Errorf("hardlink to %q: %w", target, err)
	}
	return nil
}

// setMeta gives f, just made or a dir merged into, the entry's owner, mode,
// xattrs and times, in that order: a change of owner clears the setuid bits
// and file capabilities.
func (a *Applier) setMeta(l *layer, f *os.File, hdr *tar.Header) error {
	fd := int(f.Fd())
	if err := unix.Fchownat(fd, "", hdr.Uid, hdr.Gid, unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("owner: %w", err)
	}
	// Through /proc, as an O_PATH descriptor takes no fchmod.
	proc := "/proc/self/fd/" + strconv.Itoa(fd)
	if err := unix.Fchmodat(unix.AT_FDCWD, proc, uint32(hdr.Mode&0o7777), 0); err != nil {
		return fmt.Errorf("mode: %w", err)
	}
	if err := a.setXattrs(l, fd, hdr); err != nil {
		return err
	}
	ts := times(hdr)
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, proc, ts, 0); err != nil {
		return fmt.Errorf("times: %w", err)
	}
	if hdr.Typeflag == tar.TypeDir {
		n, err := nodeOf(f)
		if err != nil {
			return err
		}
		l.dirTimes[n] = [2]unix.Timespec(ts)
	}
	return nil
}

// setXattrs sets the xattrs an entry carries that are kept, counting them
// and the work each takes.
func (a *Applier) setXattrs(l *layer, fd int, hdr *tar.Header) error {
	total := 0
	for k, v := range hdr.PAXRecords {
		if strings.HasPrefix(k, "SCHILY.xattr.") {
			total += len(k) + len(v)
		}
	}
	if total > maxXattrBytes {
		return fmt.Errorf("%w: %d bytes of xattrs", driver.ErrImageTooLarge, total)
	}
	if hdr.Typeflag == tar.TypeFifo {
		return nil
	}
	for k, v := range hdr.PAXRecords {
		name, ok := strings.CutPrefix(k, "SCHILY.xattr.")
		if !ok || !keptXattr(name) {
			continue
		}
		size := int64(len(name) + len(v))
		if err := a.charge(0, size); err != nil {
			return err
		}
		if a.xattrs++; a.xattrs > a.lim.MaxXattrs {
			return fmt.Errorf("%w: more than %d xattrs", driver.ErrImageTooLarge, a.lim.MaxXattrs)
		}
		if err := a.step(l, xattrWork+size/xattrBytesPerStep); err != nil {
			return err
		}
		if err := unix.Fsetxattr(fd, name, []byte(v), 0); err != nil {
			return fmt.Errorf("xattr %q: %w", name, err)
		}
	}
	return nil
}

// keptXattr says whether an xattr in a layer is kept: file capabilities and
// the user namespace. Others mean something to the host (trusted, such as
// overlayfs's) or to a security module it may not run.
func keptXattr(name string) bool {
	return name == "security.capability" || strings.HasPrefix(name, "user.")
}

func times(hdr *tar.Header) []unix.Timespec {
	atime := hdr.AccessTime
	if atime.IsZero() {
		atime = hdr.ModTime
	}
	return []unix.Timespec{unix.NsecToTimespec(atime.UnixNano()), unix.NsecToTimespec(hdr.ModTime.UnixNano())}
}

// keepTimes gives dir back the times its layer gave it, after what it holds
// changed.
func (l *layer) keepTimes(dir *os.File, n node) {
	if ts, ok := l.dirTimes[n]; ok {
		_ = unix.UtimesNanoAt(unix.AT_FDCWD, "/proc/self/fd/"+strconv.Itoa(int(dir.Fd())), ts[:], 0)
	}
}

// whiteout applies a whiteout entry: it removes what the layers below made
// at its name, or, as an opaque marker, everything they made in its dir.
func (a *Applier) whiteout(l *layer, parent []string, base string) error {
	name := strings.TrimPrefix(base, whiteoutPrefix)
	if base != opaqueMarker && (name == "" || name == "." || name == "..") {
		return invalid("a whiteout of no entry")
	}
	td, err := a.dirOf(l, parent, false)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
		return nil // nothing below to hide
	}
	if err != nil {
		return err
	}
	dir, err := dup(td.f)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	n, err := nodeOf(dir)
	if err != nil {
		return err
	}
	defer l.keepTimes(dir, n)
	if base == opaqueMarker {
		return a.opaque(l, dir, 0)
	}
	if l.written[nodeName{n, name}] {
		return nil // a whiteout hides only what is below
	}
	return a.removeAll(l, dir, name)
}

// opaque removes from dir what the layers below made: whatever this layer
// did not write, and, in the dirs it did, what they held from below.
func (a *Applier) opaque(l *layer, dir *os.File, depth int) error {
	n, err := nodeOf(dir)
	if err != nil {
		return err
	}
	if l.opaqued[n] {
		return nil
	}
	l.opaqued[n] = true
	if depth > a.lim.MaxDepth {
		return fmt.Errorf("%w: dirs nested deeper than %d", driver.ErrImageTooLarge, a.lim.MaxDepth)
	}
	names, err := a.readNames(l, dir)
	if err != nil {
		return err
	}
	for _, name := range names {
		if !l.written[nodeName{n, name}] {
			if err := a.removeAll(l, dir, name); err != nil {
				return err
			}
			continue
		}
		sub, err := openAt(dir, name, unix.O_RDONLY|unix.O_DIRECTORY)
		if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			continue // not a dir: nothing below it
		}
		if err != nil {
			return err
		}
		err = a.opaque(l, sub, depth+1)
		l.keepTimes(sub, mustNode(sub))
		_ = sub.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// dirOf returns the dir the components lead to inside the root, making the
// missing ones, 0755 and root's, if create is set. What the layer walked
// before is not walked again, and a symlink is resolved once.
func (a *Applier) dirOf(l *layer, parts []string, create bool) (*tdir, error) {
	hops := 0
	cur := l.top
	for _, name := range parts {
		next, err := a.lookup(l, cur, name, create, &hops)
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// lookup returns the dir name leads to in d, following a symlink as the
// image's processes would, the root being their /.
func (a *Applier) lookup(l *layer, d *tdir, name string, create bool, hops *int) (*tdir, error) {
	if err := a.step(l, 1); err != nil {
		return nil, err
	}
	if to, ok := d.at[name]; ok {
		return to, nil
	}
	// Opening a dir costs a step more than finding it walked.
	if err := a.step(l, 1); err != nil {
		return nil, err
	}
	f, err := openAt(d.f, name, unix.O_RDONLY|unix.O_DIRECTORY)
	switch {
	case err == nil:
		return l.keep(d, name, f, d), nil
	case errors.Is(err, unix.ENOENT) && create:
		f, err := a.mkdir(l, d, name)
		if err != nil {
			return nil, err
		}
		return l.keep(d, name, f, d), nil
	case !errors.Is(err, unix.ELOOP) && !errors.Is(err, unix.ENOTDIR):
		return nil, err
	}
	target, err := a.readlink(l, d, name)
	if err != nil {
		return nil, err
	}
	if *hops++; *hops > maxHops {
		return nil, fmt.Errorf("%w: more than %d symlinks in a name", driver.ErrImageTooLarge, maxHops)
	}
	cur := d
	if strings.HasPrefix(target, "/") {
		cur = l.top
	}
	for _, c := range strings.Split(target, "/") {
		switch c {
		case "", ".":
			continue
		case "..":
			if cur.parent != nil {
				cur = cur.parent
			}
			continue
		}
		next, err := a.lookup(l, cur, c, false, hops)
		if errors.Is(err, unix.ENOENT) && create {
			// Not followed to make what it points at.
			return nil, invalid("%q is a symlink to nothing", name)
		}
		if err != nil {
			return nil, err
		}
		cur = next
	}
	d.at[name] = cur
	return cur, nil
}

// keep records the dir f at name in d, its parent being parent.
func (l *layer) keep(d *tdir, name string, f *os.File, parent *tdir) *tdir {
	t := &tdir{parent: parent, f: f, depth: parent.depth + 1, pathLen: parent.pathLen + 1 + len(name), at: map[string]*tdir{}}
	d.at[name] = t
	l.cached++
	return t
}

// readlink returns where the symlink name in d points, or ENOTDIR if it is
// no symlink.
func (a *Applier) readlink(l *layer, d *tdir, name string) (string, error) {
	if err := a.step(l, 1); err != nil {
		return "", err
	}
	buf := make([]byte, maxNameBytes+1)
	n, err := unix.Readlinkat(int(d.f.Fd()), name, buf)
	if errors.Is(err, unix.EINVAL) {
		return "", unix.ENOTDIR
	}
	if err != nil {
		return "", err
	}
	if n > maxNameBytes {
		return "", fmt.Errorf("%w: a symlink of more than %d bytes", driver.ErrImageTooLarge, maxNameBytes)
	}
	return string(buf[:n]), nil
}

// mkdir makes name in d, as dirOf does, and opens it.
func (a *Applier) mkdir(l *layer, d *tdir, name string) (*os.File, error) {
	if err := a.fits(d, name); err != nil {
		return nil, err
	}
	if err := a.charge(1, int64(len(name))); err != nil {
		return nil, err
	}
	if err := a.step(l, 1); err != nil {
		return nil, err
	}
	n, err := nodeOf(d.f)
	if err != nil {
		return nil, err
	}
	defer l.keepTimes(d.f, n)
	if err := unix.Mkdirat(int(d.f.Fd()), name, 0o700); err != nil {
		return nil, fmt.Errorf("making %q: %w", name, err)
	}
	l.written[nodeName{n, name}] = true
	f, err := openAt(d.f, name, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	if err := unix.Fchmod(int(f.Fd()), 0o755); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// openAt opens name in dir, never following it if it is a symlink.
func openAt(dir *os.File, name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func dup(f *os.File) (*os.File, error) {
	fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), f.Name()), nil
}

// removeAll removes name from dir, and all it holds if it is a dir, never
// following a symlink. What the layer walked is walked again after.
func (a *Applier) removeAll(l *layer, dir *os.File, name string) error {
	var st unix.Stat_t
	err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	l.forget()
	return a.remove(l, dir, name, 0)
}

func (a *Applier) remove(l *layer, dir *os.File, name string, depth int) error {
	if err := a.step(l, 1); err != nil {
		return err
	}
	var st unix.Stat_t
	err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.Unlinkat(int(dir.Fd()), name, 0)
	}
	// Through symlinks, a layer can nest dirs deeper than its names are.
	if depth > a.lim.MaxDepth {
		return fmt.Errorf("%w: dirs nested deeper than %d", driver.ErrImageTooLarge, a.lim.MaxDepth)
	}
	sub, err := openAt(dir, name, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	names, err := a.readNames(l, sub)
	for _, n := range names {
		if err != nil {
			break
		}
		err = a.remove(l, sub, n, depth+1)
	}
	_ = sub.Close()
	if err != nil {
		return err
	}
	return unix.Unlinkat(int(dir.Fd()), name, unix.AT_REMOVEDIR)
}

// readNames lists dir, from a descriptor of its own so dir's offset is left
// alone, counting a step of work for each name.
func (a *Applier) readNames(l *layer, dir *os.File) ([]string, error) {
	f, err := openAt(dir, ".", unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	return names, a.step(l, int64(len(names)))
}

func nodeOf(f *os.File) (node, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return node{}, err
	}
	return node{st.Dev, st.Ino}, nil
}

// mustNode is nodeOf for a descriptor already known good.
func mustNode(f *os.File) node {
	n, _ := nodeOf(f)
	return n
}
