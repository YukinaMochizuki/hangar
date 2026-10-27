package boot

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// What a running guest does to the filesystems it mounted from its disks:
// grows them when the node has grown a disk (GrowFilesystems), and trims
// them, so what is deleted in them goes back to the host's storage
// (TrimEvery). Both find the filesystems in the mount table, by the device
// each is on, not by where the guest mounted it: a container may mount over
// a volume, or move it.

// fitrim is FITRIM, _IOWR('X', 121, struct fstrim_range): discard the free
// blocks of the filesystem a file is in.
const fitrim = 0xc0185879

// fstrimRange is struct fstrim_range.
type fstrimRange struct {
	start, length, minLength uint64
}

// DefaultTrimInterval is how often the guest trims its filesystems unless
// its boot disk says otherwise.
const DefaultTrimInterval = time.Hour

// diskMount is an ext4 filesystem the guest mounted from one of its disks.
type diskMount struct {
	// dev is the disk's device number, and source its device node.
	dev    uint64
	source string
	// dirs are where it is mounted, as mountinfo lists them.
	dirs []string
}

// diskMounts are the ext4 filesystems mountinfo lists as mounted from a
// virtio disk, /dev/vd*, each once, in the order first listed.
func diskMounts(mountinfo string) []diskMount {
	var out []diskMount
	at := map[uint64]int{}
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || len(fields) < sep+3 || fields[sep+1] != "ext4" || !strings.HasPrefix(fields[sep+2], "/dev/vd") {
			continue
		}
		majMin := strings.SplitN(fields[2], ":", 2)
		if len(majMin) != 2 {
			continue
		}
		maj, err1 := strconv.ParseUint(majMin[0], 10, 32)
		mnr, err2 := strconv.ParseUint(majMin[1], 10, 32)
		if err1 != nil || err2 != nil {
			continue
		}
		dev := unix.Mkdev(uint32(maj), uint32(mnr))
		dir := unescape(fields[4])
		if i, ok := at[dev]; ok {
			out[i].dirs = append(out[i].dirs, dir)
			continue
		}
		at[dev] = len(out)
		out = append(out, diskMount{dev: dev, source: unescape(fields[sep+2]), dirs: []string{dir}})
	}
	return out
}

// readDiskMounts lists the guest's disk filesystems, as diskMounts.
func readDiskMounts() ([]diskMount, error) {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	return diskMounts(string(b)), nil
}

// open opens the filesystem's root read-write, whatever its mounts: a
// detached copy of the first of them that is still the filesystem's, made
// writable as the volume a container sees read-only is not. The copy goes
// once the file is closed.
func (m diskMount) open() (*os.File, error) {
	for _, dir := range m.dirs {
		tree, err := unix.OpenTree(unix.AT_FDCWD, dir, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
		if err != nil {
			continue // gone since mountinfo was read
		}
		f, err := m.openTree(tree, dir)
		_ = unix.Close(tree)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, errShadowed) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%s is no longer mounted where it was", m.source)
}

// errShadowed is a mount point another filesystem was mounted over.
var errShadowed = errors.New("another filesystem is mounted over it")

func (m diskMount) openTree(tree int, dir string) (*os.File, error) {
	var st unix.Stat_t
	if err := unix.Fstat(tree, &st); err != nil {
		return nil, err
	}
	if st.Dev != m.dev {
		return nil, errShadowed
	}
	rw := &unix.MountAttr{Attr_clr: unix.MOUNT_ATTR_RDONLY}
	if err := unix.MountSetattr(tree, "", unix.AT_EMPTY_PATH, rw); err != nil {
		return nil, fmt.Errorf("making a copy of %s writable: %w", dir, err)
	}
	fd, err := unix.Openat(tree, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), dir), nil
}

// checkSource refuses a device node that is not the filesystem's disk.
func (m diskMount) checkSource() error {
	var st unix.Stat_t
	if err := unix.Stat(m.source, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFBLK || st.Rdev != m.dev {
		return fmt.Errorf("%s is not the disk %s is on", m.source, m.dirs[0])
	}
	return nil
}

// GrowFilesystems grows each ext4 filesystem mounted from one of the
// guest's disks to fill its disk, as SwitchRoot and MountData do at boot,
// and says in logf which it grew. It returns what failed, every filesystem
// tried all the same.
func GrowFilesystems(logf func(format string, args ...any)) error {
	mounts, err := readDiskMounts()
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range mounts {
		if err := m.checkSource(); err != nil {
			errs = append(errs, err)
			continue
		}
		from, to, err := grow(m.source, m.dirs[0], m.open)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("growing %s at %s: %w", m.source, m.dirs[0], err))
		case to > from:
			logf("hangar-init: grew the filesystem on %s at %s from %d to %d blocks", m.source, m.dirs[0], from, to)
		}
	}
	return errors.Join(errs...)
}

// Trim trims each ext4 filesystem mounted from one of the guest's disks:
// its free blocks are discarded, which the host's storage then frees. It
// returns the bytes trimmed, and what failed, every filesystem tried all
// the same.
func Trim() (int64, error) {
	mounts, err := readDiskMounts()
	if err != nil {
		return 0, err
	}
	var trimmed int64
	var errs []error
	for _, m := range mounts {
		n, err := m.trim()
		if err != nil {
			errs = append(errs, fmt.Errorf("trimming %s at %s: %w", m.source, m.dirs[0], err))
			continue
		}
		trimmed += n
	}
	return trimmed, errors.Join(errs...)
}

func (m diskMount) trim() (int64, error) {
	f, err := m.open()
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	r := fstrimRange{length: math.MaxUint64}
	if err := ioctl(f, fitrim, unsafe.Pointer(&r)); err != nil {
		return 0, err
	}
	// The kernel answers with the bytes it trimmed.
	return int64(min(r.length, math.MaxInt64)), nil
}

// TrimEvery trims the guest's filesystems every interval, DefaultTrimInterval
// if it is not positive, for as long as the guest runs: first at a random
// point in the first interval, so a host's guests started together do not
// trim together. It says in logf what failed, once until it fails another
// way.
func TrimEvery(interval time.Duration, logf func(format string, args ...any)) {
	if interval <= 0 {
		interval = DefaultTrimInterval
	}
	time.Sleep(rand.N(interval))
	last := ""
	for {
		_, err := Trim()
		switch {
		case err != nil && err.Error() != last:
			last = err.Error()
			logf("hangar-init: %v", err)
		case err == nil:
			last = ""
		}
		time.Sleep(interval)
	}
}
