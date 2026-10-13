// Package zfsstore is the host's Storage driver, on ZFS through the zfs
// command. Each image digest gets a base: a zvol filled with mkfs.ext4 -d
// from the unpacked image, so the host never mounts it, and snapshotted.
// Each machine's root disk is a sparse clone of its image's base, grown to
// the size asked for; the guest grows its filesystem as it boots. Each disk
// image, such as a distribution's cloud image, gets a base too, written
// raw onto a zvol and snapshotted. Each API Disk gets a volume of its own,
// a sparse zvol the host never reads, blank or cloned from a disk image's
// base. Every
// dataset the driver makes carries a user property naming what it serves,
// which is how it is listed and how the driver knows it as its own: a
// dataset without it is never changed or destroyed. See
// docs/decisions/storage-zfs.md.
package zfsstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/keylock"
)

// User properties on the driver's datasets.
const (
	propDigest  = "hangar:digest"
	propMachine = "hangar:machine"
	propDisk    = "hangar:disk"
)

// baseSnapshot names the snapshot a base is cloned from; a base without it
// was never finished.
const baseSnapshot = "base"

// Sizes of a base: what its files take, rounded to ext4's 4 KiB blocks, plus
// a fifth and fixed room for the journal and metadata; and at least an
// inode for every 16 KiB, ext4's default, with room for what it holds.
const (
	blockSize    = 4096
	inodeRatio   = 16384
	baseOverhead = 64 << 20
	// align is what sizes are rounded up to: a multiple of any volblocksize.
	align = 1 << 20
)

// Config configures the driver.
type Config struct {
	// Dataset is the parent of the driver's datasets, such as the pool
	// hangar; it must exist. The driver keeps bases in Dataset/bases, root
	// disks in Dataset/roots, volumes in Dataset/disks and disk images'
	// bases in Dataset/vmbases, and makes those if need be.
	Dataset string
	// Scratch is a directory, root's alone, to unpack images in before they
	// go into their base. The driver owns all of it.
	Scratch string
	// DeviceTimeout bounds waiting for a zvol's device node. Zero means 30
	// seconds.
	DeviceTimeout time.Duration
}

// Store is the driver. It is safe for concurrent use.
type Store struct {
	cfg Config

	// building holds a lock per digest, so a base is made once at a time
	// and not deleted while it is made.
	building keylock.Map
}

// New checks the parent dataset, makes the driver's own datasets in it if
// need be, and clears what an interrupted base left behind.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DeviceTimeout == 0 {
		cfg.DeviceTimeout = 30 * time.Second
	}
	s := &Store{cfg: cfg}
	if _, err := zfs(ctx, "list", "-H", "-o", "name", cfg.Dataset); err != nil {
		return nil, fmt.Errorf("the dataset for disks: %w", err)
	}
	for _, ds := range []string{s.bases(), s.roots(), s.disks(), s.vmBases()} {
		_, err := zfs(ctx, "list", "-H", "-o", "name", ds)
		if errors.Is(err, errNotExist) {
			_, err = zfs(ctx, "create", "-o", "mountpoint=none", "-o", "canmount=off", ds)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(cfg.Scratch, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.Scratch, 0o700); err != nil {
		return nil, err
	}
	leftovers, err := os.ReadDir(cfg.Scratch)
	if err != nil {
		return nil, err
	}
	for _, e := range leftovers {
		if err := os.RemoveAll(filepath.Join(cfg.Scratch, e.Name())); err != nil {
			return nil, err
		}
	}
	return s, s.clearUnfinished(ctx)
}

func (s *Store) bases() string   { return s.cfg.Dataset + "/bases" }
func (s *Store) roots() string   { return s.cfg.Dataset + "/roots" }
func (s *Store) disks() string   { return s.cfg.Dataset + "/disks" }
func (s *Store) vmBases() string { return s.cfg.Dataset + "/vmbases" }

var (
	digestRE = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)
	uidRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
)

// baseName returns the volume of a digest's base, or fails for what is not
// a sha256 digest.
func (s *Store) baseName(digest string) (string, error) {
	m := digestRE.FindStringSubmatch(digest)
	if m == nil {
		return "", fmt.Errorf("%q is not a sha256 digest", digest)
	}
	return s.bases() + "/" + m[1], nil
}

// vmBaseName returns the volume of a disk image's base, or fails for what
// is not a sha256 digest.
func (s *Store) vmBaseName(digest string) (string, error) {
	m := digestRE.FindStringSubmatch(digest)
	if m == nil {
		return "", fmt.Errorf("%q is not a sha256 digest", digest)
	}
	return s.vmBases() + "/" + m[1], nil
}

// rootName returns the volume of a machine's root disk, or fails for a uid
// that cannot name one.
func (s *Store) rootName(uid string) (string, error) {
	if !uidRE.MatchString(uid) {
		return "", fmt.Errorf("%q cannot name a root disk", uid)
	}
	return s.roots() + "/" + uid, nil
}

// volumeName returns the volume of a Disk, or fails for a uid that cannot
// name one.
func (s *Store) volumeName(uid string) (string, error) {
	if !uidRE.MatchString(uid) {
		return "", fmt.Errorf("%q cannot name a disk's volume", uid)
	}
	return s.disks() + "/" + uid, nil
}

func device(volume string) string { return "/dev/zvol/" + volume }

// What zfs says of a dataset that does not exist, and of one it will not
// destroy while it is open.
var (
	errNotExist = errors.New("dataset does not exist")
	errBusy     = errors.New("dataset is busy")
)

// busyGrace bounds retrying a destroy refused as busy: udev opens a zvol to
// probe it whenever a writer closes it, as a VM that stopped did.
const busyGrace = 3 * time.Second

// destroy destroys a dataset, never recursively, waiting out a probe that
// holds it open; one held longer stays and fails.
func destroy(ctx context.Context, ds string) error {
	deadline := time.Now().Add(busyGrace)
	for delay := 20 * time.Millisecond; ; delay = min(delay*2, 500*time.Millisecond) {
		_, err := zfs(ctx, "destroy", ds)
		if !errors.Is(err, errBusy) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
	}
}

// zfs runs the zfs command and returns its output.
func zfs(ctx context.Context, args ...string) ([]byte, error) {
	return run(ctx, "zfs", args...)
}

func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	// Errors are told apart by what the command says, in English.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		switch {
		case strings.Contains(msg, "dataset does not exist"):
			return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), errNotExist)
		case strings.Contains(msg, "dataset is busy"):
			return nil, fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), msg, errBusy)
		case strings.Contains(msg, "out of space") || strings.Contains(msg, "No space left"):
			return nil, fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), msg, driver.ErrNoSpace)
		}
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return stdout.Bytes(), nil
}

// props returns the named properties of a dataset, by name, parsed from
// zfs get -Hp; ok is false if it does not exist.
func props(ctx context.Context, ds string, names ...string) (map[string]string, bool, error) {
	out, err := zfs(ctx, "get", "-Hp", "-o", "property,value", strings.Join(names, ","), ds)
	if errors.Is(err, errNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if k, v, ok := strings.Cut(line, "\t"); ok {
			got[k] = v
		}
	}
	return got, true, nil
}

// list returns the rows of zfs list -Hp for the columns, of datasets of the
// type under parent.
func list(ctx context.Context, typ, parent string, columns ...string) ([][]string, error) {
	out, err := zfs(ctx, "list", "-Hp", "-r", "-t", typ, "-o", strings.Join(columns, ","), parent)
	if err != nil {
		return nil, err
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		row := strings.Split(line, "\t")
		if len(row) != len(columns) {
			return nil, fmt.Errorf("zfs list: %q has %d columns, want %d", line, len(row), len(columns))
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// lock takes a digest's lock.
func (s *Store) lock(digest string) func() { return s.building.Lock(digest) }

// clearUnfinished destroys the bases of the driver's own, of OCI and disk
// images, that never got their snapshot, as a crash while making one leaves
// them.
func (s *Store) clearUnfinished(ctx context.Context) error {
	if err := s.clearUnfinishedIn(ctx, s.bases(), s.baseName, s.Bases); err != nil {
		return err
	}
	return s.clearUnfinishedIn(ctx, s.vmBases(), s.vmBaseName, s.ImageBases)
}

func (s *Store) clearUnfinishedIn(ctx context.Context, parent string, nameOf func(string) (string, error),
	finished func(context.Context) ([]string, error)) error {
	vols, err := list(ctx, "volume", parent, "name", propDigest)
	if err != nil {
		return err
	}
	done, err := finished(ctx)
	if err != nil {
		return err
	}
	for _, v := range vols {
		name, digest := v[0], v[1]
		if want, err := nameOf(digest); err != nil || want != name {
			continue // not ours
		}
		finished := false
		for _, d := range done {
			finished = finished || d == digest
		}
		if !finished {
			if err := destroy(ctx, name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) EnsureBase(ctx context.Context, digest string, size int64, fill func(dir string) error) error {
	vol, err := s.baseName(digest)
	if err != nil {
		return err
	}
	defer s.lock(digest)()
	snap := vol + "@" + baseSnapshot
	p, ok, err := props(ctx, snap, propDigest)
	if err != nil {
		return err
	}
	if ok && p[propDigest] == digest {
		return nil
	}
	// An unfinished base of ours is made again; anything else at its name
	// is left alone.
	if p, ok, err := props(ctx, vol, propDigest); err != nil {
		return err
	} else if ok {
		if p[propDigest] != digest {
			return fmt.Errorf("%s exists and is not a base of %s", vol, digest)
		}
		if err := destroy(ctx, vol); err != nil {
			return err
		}
	}

	if err := s.room(ctx, digest, size); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(s.cfg.Scratch, "base-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := fill(dir); err != nil {
		return fmt.Errorf("filling the base of %s: %w", digest, err)
	}
	size, err = baseSize(dir)
	if err != nil {
		return err
	}
	c, err := s.Capacity(ctx)
	if err != nil {
		return err
	}
	if c.Free < size {
		return fmt.Errorf("the base of %s needs %d bytes, %d are free: %w", digest, size, c.Free, driver.ErrNoSpace)
	}
	if _, err := zfs(ctx, "create", "-s", "-V", strconv.FormatInt(size, 10),
		"-o", "volmode=dev", "-o", propDigest+"="+digest, vol); err != nil {
		return err
	}
	if err := s.writeBase(ctx, vol, snap, digest, dir); err != nil {
		// Without its snapshot it is unfinished: nothing can use it.
		_ = destroy(context.WithoutCancel(ctx), vol)
		return err
	}
	return nil
}

// room fails with ErrNoSpace, before anything is unpacked, unless the image
// of size bytes, compressed, fits both where it is unpacked and in the pool.
func (s *Store) room(ctx context.Context, digest string, size int64) error {
	var st unix.Statfs_t
	if err := unix.Statfs(s.cfg.Scratch, &st); err != nil {
		return err
	}
	if free := int64(st.Bavail) * st.Bsize; free < size {
		return fmt.Errorf("unpacking %s takes at least %d bytes, %d are free: %w", digest, size, free, driver.ErrNoSpace)
	}
	c, err := s.Capacity(ctx)
	if err != nil {
		return err
	}
	if c.Free < size+baseOverhead {
		return fmt.Errorf("the base of %s takes at least %d bytes, %d are free: %w", digest, size+baseOverhead, c.Free, driver.ErrNoSpace)
	}
	return nil
}

// writeBase makes the filesystem of a base from dir and finishes the base.
func (s *Store) writeBase(ctx context.Context, vol, snap, digest, dir string) error {
	dev, err := s.waitDevice(ctx, vol)
	if err != nil {
		return err
	}
	// The block size and inode ratio a grown disk wants, not a small one's;
	// no root reserve; inode tables and journal written once here, not by
	// every clone. docs/decisions/storage-zfs.md.
	if _, err := run(ctx, "mkfs.ext4", "-q", "-F", "-b", strconv.Itoa(blockSize), "-I", "256",
		"-i", strconv.Itoa(inodeRatio), "-m", "0", "-E", "lazy_itable_init=0,lazy_journal_init=0",
		"-d", dir, dev); err != nil {
		return fmt.Errorf("making the base of %s: %w", digest, err)
	}
	if _, err := zfs(ctx, "snapshot", "-o", propDigest+"="+digest, snap); err != nil {
		return err
	}
	_, err = zfs(ctx, "set", "readonly=on", vol)
	return err
}

// baseSize returns the size of the base for what dir holds.
func baseSize(dir string) (int64, error) {
	var data, inodes int64
	seen := map[uint64]bool{}
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || seen[st.Ino] {
			return nil // a hardlink takes no more room
		}
		seen[st.Ino] = true
		inodes++
		switch {
		case fi.Mode().IsRegular(), fi.IsDir():
			data += roundUp(max(fi.Size(), 1), blockSize)
		case fi.Mode()&fs.ModeSymlink != 0 && fi.Size() >= 60:
			data += blockSize // shorter ones fit in the inode
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	size := data + data/5 + baseOverhead
	return roundUp(max(size, inodes*inodeRatio*6/5), align), nil
}

func roundUp(n, to int64) int64 { return (n + to - 1) / to * to }

// waitDevice waits for a zvol's /dev/zvol link to lead to the zvol itself,
// as a stale one may lead to another (docs/decisions/storage-zfs.md), and
// returns the device.
func (s *Store) waitDevice(ctx context.Context, vol string) (string, error) {
	deadline := time.Now().Add(s.cfg.DeviceTimeout)
	for {
		dev, err := filepath.EvalSymlinks(device(vol))
		if err == nil {
			var name string
			if name, err = zvolName(dev); err == nil && name == vol {
				return dev, nil
			}
			if err == nil {
				err = fmt.Errorf("it leads to %s, which is %s", dev, name)
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%s did not lead to %s in %v: %w", device(vol), vol, s.cfg.DeviceTimeout, err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// blkZName is ZFS's BLKZNAME, _IOR(0x12, 125, char[256]): the dataset a
// zvol's device is, as udev's zvol_id asks it.
const blkZName = 0x8100127d

// zvolName returns the dataset the zvol device dev is.
func zvolName(dev string) (string, error) {
	f, err := os.Open(dev)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return VolumeOf(f)
}

// VolumeOf returns the dataset the open zvol device f is, by BLKZNAME: what
// the device is, whatever link led to it.
func VolumeOf(f *os.File) (string, error) {
	var buf [256]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), blkZName, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return "", fmt.Errorf("%s: BLKZNAME: %w", f.Name(), errno)
	}
	name, _, _ := bytes.Cut(buf[:], []byte{0})
	return string(name), nil
}

func (s *Store) Bases(ctx context.Context) ([]string, error) {
	snaps, err := list(ctx, "snapshot", s.bases(), "name", propDigest)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, row := range snaps {
		name, digest := row[0], row[1]
		if vol, err := s.baseName(digest); err == nil && name == vol+"@"+baseSnapshot {
			out = append(out, digest)
		}
	}
	return out, nil
}

func (s *Store) DeleteBase(ctx context.Context, digest string) error {
	vol, err := s.baseName(digest)
	if err != nil {
		return err
	}
	defer s.lock(digest)()
	p, ok, err := props(ctx, vol, propDigest)
	if err != nil || !ok {
		return err
	}
	if p[propDigest] != digest {
		return fmt.Errorf("%s is not a base of %s; left alone", vol, digest)
	}
	// The snapshot goes first, and not while a root disk is cloned from
	// it.
	if err := destroy(ctx, vol+"@"+baseSnapshot); err != nil && !errors.Is(err, errNotExist) {
		return fmt.Errorf("deleting the base of %s: %w", digest, err)
	}
	return destroy(ctx, vol)
}

// root is a root disk as its properties say.
type root struct {
	machine, digest string
	size            int64
}

func (s *Store) rootOf(ctx context.Context, vol string) (root, bool, error) {
	p, ok, err := props(ctx, vol, propMachine, propDigest, "volsize")
	if err != nil || !ok {
		return root{}, ok, err
	}
	size, err := strconv.ParseInt(p["volsize"], 10, 64)
	if err != nil {
		return root{}, false, fmt.Errorf("%s: volsize %q", vol, p["volsize"])
	}
	return root{p[propMachine], p[propDigest], size}, true, nil
}

func (s *Store) EnsureRoot(ctx context.Context, machineUID, digest string, size int64) (driver.Disk, error) {
	vol, err := s.rootName(machineUID)
	if err != nil {
		return driver.Disk{}, err
	}
	base, err := s.baseName(digest)
	if err != nil {
		return driver.Disk{}, err
	}
	want := roundUp(max(size, 1), align)
	r, ok, err := s.rootOf(ctx, vol)
	if err != nil {
		return driver.Disk{}, err
	}
	switch {
	case ok && r.machine != machineUID:
		return driver.Disk{}, fmt.Errorf("%s is not machine %s's root disk; left alone", vol, machineUID)
	case ok && r.digest != digest:
		return driver.Disk{}, fmt.Errorf("machine %s has a root disk of %s, not %s", machineUID, r.digest, digest)
	case !ok:
		snap := base + "@" + baseSnapshot
		p, found, err := props(ctx, snap, propDigest, "volsize")
		if err != nil {
			return driver.Disk{}, err
		}
		if !found || p[propDigest] != digest {
			return driver.Disk{}, fmt.Errorf("no base for %s", digest)
		}
		baseSize, err := strconv.ParseInt(p["volsize"], 10, 64)
		if err != nil {
			return driver.Disk{}, fmt.Errorf("%s: volsize %q", snap, p["volsize"])
		}
		if want < baseSize {
			return driver.Disk{}, fmt.Errorf("%w: %d bytes asked, the base of %s has %d", driver.ErrDiskTooSmall, size, digest, baseSize)
		}
		if _, err := zfs(ctx, "clone", "-o", "volmode=dev", "-o", "refreservation=none",
			"-o", propMachine+"="+machineUID, "-o", propDigest+"="+digest, snap, vol); err != nil {
			return driver.Disk{}, err
		}
		r = root{machineUID, digest, baseSize}
	}
	// A root smaller than asked is grown, as one cloned above is; never
	// shrunk.
	if r.size < want {
		if _, err := zfs(ctx, "set", "volsize="+strconv.FormatInt(want, 10), vol); err != nil {
			return driver.Disk{}, err
		}
		r.size = want
	}
	if _, err := s.waitDevice(ctx, vol); err != nil {
		return driver.Disk{}, err
	}
	return driver.Disk{MachineUID: machineUID, Digest: digest, Path: device(vol), Size: r.size}, nil
}

func (s *Store) Roots(ctx context.Context) ([]driver.Disk, error) {
	rows, err := list(ctx, "volume", s.roots(), "name", propMachine, propDigest, "volsize")
	if err != nil {
		return nil, err
	}
	var out []driver.Disk
	for _, row := range rows {
		name, machine, digest := row[0], row[1], row[2]
		vol, err := s.rootName(machine)
		if err != nil || vol != name || !digestRE.MatchString(digest) {
			continue // not ours
		}
		size, err := strconv.ParseInt(row[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: volsize %q", name, row[3])
		}
		out = append(out, driver.Disk{MachineUID: machine, Digest: digest, Path: device(name), Size: size})
	}
	return out, nil
}

func (s *Store) DeleteRoot(ctx context.Context, machineUID string) error {
	vol, err := s.rootName(machineUID)
	if err != nil {
		return err
	}
	r, ok, err := s.rootOf(ctx, vol)
	if err != nil || !ok {
		return err
	}
	if r.machine != machineUID {
		return fmt.Errorf("%s is not machine %s's root disk; left alone", vol, machineUID)
	}
	// zfs refuses while the device is open, as a running VM holds it.
	return destroy(ctx, vol)
}

func (s *Store) EnsureImageBase(ctx context.Context, digest string, size int64, fill func(dev string) error) error {
	vol, err := s.vmBaseName(digest)
	if err != nil {
		return err
	}
	defer s.lock(digest)()
	snap := vol + "@" + baseSnapshot
	if p, ok, err := props(ctx, snap, propDigest); err != nil || ok && p[propDigest] == digest {
		return err
	}
	// An unfinished base of ours is made again; anything else at its name
	// is left alone.
	if p, ok, err := props(ctx, vol, propDigest); err != nil {
		return err
	} else if ok {
		if p[propDigest] != digest {
			return fmt.Errorf("%s exists and is not a base of disk image %s", vol, digest)
		}
		if err := destroy(ctx, vol); err != nil {
			return err
		}
	}
	size = roundUp(max(size, 1), align)
	c, err := s.Capacity(ctx)
	if err != nil {
		return err
	}
	// The fill writes all but the zeros, so the base may take up to its
	// size: how much less is not known before.
	if c.Free < size {
		return fmt.Errorf("the base of disk image %s needs up to %d bytes, %d are free: %w", digest, size, c.Free, driver.ErrNoSpace)
	}
	if _, err := zfs(ctx, "create", "-s", "-V", strconv.FormatInt(size, 10),
		"-o", "volmode=dev", "-o", propDigest+"="+digest, vol); err != nil {
		return err
	}
	finish := func() error {
		dev, err := s.waitDevice(ctx, vol)
		if err != nil {
			return err
		}
		if err := fill(dev); err != nil {
			return fmt.Errorf("filling the base of disk image %s: %w", digest, err)
		}
		if _, err := zfs(ctx, "snapshot", "-o", propDigest+"="+digest, snap); err != nil {
			return err
		}
		_, err = zfs(ctx, "set", "readonly=on", vol)
		return err
	}
	if err := finish(); err != nil {
		// Without its snapshot it is unfinished: nothing can use it.
		_ = destroy(context.WithoutCancel(ctx), vol)
		return err
	}
	return nil
}

func (s *Store) ImageBases(ctx context.Context) ([]string, error) {
	snaps, err := list(ctx, "snapshot", s.vmBases(), "name", propDigest)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, row := range snaps {
		name, digest := row[0], row[1]
		if vol, err := s.vmBaseName(digest); err == nil && name == vol+"@"+baseSnapshot {
			out = append(out, digest)
		}
	}
	return out, nil
}

func (s *Store) DeleteImageBase(ctx context.Context, digest string) error {
	vol, err := s.vmBaseName(digest)
	if err != nil {
		return err
	}
	defer s.lock(digest)()
	p, ok, err := props(ctx, vol, propDigest)
	if err != nil || !ok {
		return err
	}
	if p[propDigest] != digest {
		return fmt.Errorf("%s is not a base of disk image %s; left alone", vol, digest)
	}
	// The snapshot goes first, and not while a volume is cloned from it.
	if err := destroy(ctx, vol+"@"+baseSnapshot); err != nil && !errors.Is(err, errNotExist) {
		return fmt.Errorf("deleting the base of disk image %s: %w", digest, err)
	}
	return destroy(ctx, vol)
}

// volumeOf returns the Disk a volume serves, the disk image it was cloned
// from, empty for none, and its size, as its properties say.
func volumeOf(ctx context.Context, vol string) (disk, digest string, size int64, ok bool, err error) {
	p, ok, err := props(ctx, vol, propDisk, propDigest, "volsize")
	if err != nil || !ok {
		return "", "", 0, ok, err
	}
	size, err = strconv.ParseInt(p["volsize"], 10, 64)
	if err != nil {
		return "", "", 0, false, fmt.Errorf("%s: volsize %q", vol, p["volsize"])
	}
	return p[propDisk], unset(p[propDigest]), size, true, nil
}

// unset is a user property's value, empty for one not set, which zfs gives
// as "-".
func unset(v string) string {
	if v == "-" {
		return ""
	}
	return v
}

func (s *Store) EnsureVolume(ctx context.Context, diskUID string, size int64, digest string) (driver.Volume, error) {
	vol, err := s.volumeName(diskUID)
	if err != nil {
		return driver.Volume{}, err
	}
	want := roundUp(max(size, 1), align)
	disk, origin, have, ok, err := volumeOf(ctx, vol)
	switch {
	case err != nil:
		return driver.Volume{}, err
	case ok && disk != diskUID:
		return driver.Volume{}, fmt.Errorf("%s is not disk %s's volume; left alone", vol, diskUID)
	case ok && origin != digest:
		return driver.Volume{}, fmt.Errorf("disk %s has a volume of %q, not %q", diskUID, origin, digest)
	case !ok && digest == "":
		if _, err := zfs(ctx, "create", "-s", "-V", strconv.FormatInt(want, 10),
			"-o", "volmode=dev", "-o", propDisk+"="+diskUID, vol); err != nil {
			return driver.Volume{}, err
		}
		have = want
	case !ok:
		base, err := s.vmBaseName(digest)
		if err != nil {
			return driver.Volume{}, err
		}
		snap := base + "@" + baseSnapshot
		p, found, err := props(ctx, snap, propDigest, "volsize")
		if err != nil {
			return driver.Volume{}, err
		}
		if !found || p[propDigest] != digest {
			return driver.Volume{}, fmt.Errorf("no base for disk image %s", digest)
		}
		baseSize, err := strconv.ParseInt(p["volsize"], 10, 64)
		if err != nil {
			return driver.Volume{}, fmt.Errorf("%s: volsize %q", snap, p["volsize"])
		}
		if want < baseSize {
			return driver.Volume{}, fmt.Errorf("%w: %d bytes asked, disk image %s has %d", driver.ErrDiskTooSmall, size, digest, baseSize)
		}
		if _, err := zfs(ctx, "clone", "-o", "volmode=dev", "-o", "refreservation=none",
			"-o", propDisk+"="+diskUID, "-o", propDigest+"="+digest, snap, vol); err != nil {
			return driver.Volume{}, err
		}
		have = baseSize
	}
	// A volume smaller than asked is grown, as one cloned above is; never
	// shrunk.
	if have < want {
		if _, err := zfs(ctx, "set", "volsize="+strconv.FormatInt(want, 10), vol); err != nil {
			return driver.Volume{}, err
		}
		have = want
	}
	if _, err := s.waitDevice(ctx, vol); err != nil {
		return driver.Volume{}, err
	}
	return driver.Volume{DiskUID: diskUID, Path: device(vol), Size: have, Digest: digest}, nil
}

func (s *Store) Volumes(ctx context.Context) ([]driver.Volume, error) {
	rows, err := list(ctx, "volume", s.disks(), "name", propDisk, propDigest, "volsize")
	if err != nil {
		return nil, err
	}
	var out []driver.Volume
	for _, row := range rows {
		name, disk, digest := row[0], row[1], unset(row[2])
		if vol, err := s.volumeName(disk); err != nil || vol != name || digest != "" && !digestRE.MatchString(digest) {
			continue // not ours
		}
		size, err := strconv.ParseInt(row[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: volsize %q", name, row[3])
		}
		out = append(out, driver.Volume{DiskUID: disk, Path: device(name), Size: size, Digest: digest})
	}
	return out, nil
}

func (s *Store) DeleteVolume(ctx context.Context, diskUID string) error {
	vol, err := s.volumeName(diskUID)
	if err != nil {
		return err
	}
	disk, _, _, ok, err := volumeOf(ctx, vol)
	if err != nil || !ok {
		return err
	}
	if disk != diskUID {
		return fmt.Errorf("%s is not disk %s's volume; left alone", vol, diskUID)
	}
	// zfs refuses while the device is open, as a running VM holds it.
	return destroy(ctx, vol)
}

func (s *Store) Capacity(ctx context.Context) (driver.Capacity, error) {
	p, ok, err := props(ctx, s.cfg.Dataset, "used", "available")
	if err == nil && !ok {
		err = fmt.Errorf("%s: %w", s.cfg.Dataset, errNotExist)
	}
	if err != nil {
		return driver.Capacity{}, err
	}
	used, err1 := strconv.ParseInt(p["used"], 10, 64)
	avail, err2 := strconv.ParseInt(p["available"], 10, 64)
	if err := errors.Join(err1, err2); err != nil {
		return driver.Capacity{}, fmt.Errorf("%s: %w", s.cfg.Dataset, err)
	}
	return driver.Capacity{Total: used + avail, Free: avail}, nil
}
