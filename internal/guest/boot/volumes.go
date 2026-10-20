package boot

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

// Mke2fs is where the initramfs carries mke2fs, which formats a blank
// volume.
const Mke2fs = "/sbin/mke2fs"

// stagingDir is where each volume is mounted whole, in the initramfs,
// before what the container sees of it is bound into the root disk.
const stagingDir = "/volumes"

// probeSize is how much of each end of a volume is read to tell it blank:
// every partition table, filesystem and RAID signature lies in one or the
// other.
const probeSize = 1 << 20

// platformDirs are where hangar-init mounts its own filesystems once the
// root disk is the root, over anything a volume would put there.
var platformDirs = []string{"/proc", "/sys", "/dev", "/run"}

// Volumes mounts the container's volumes in root, the root disk before it
// becomes /, as docs/decisions/container-volumes.md says. A FileShare's is
// mounted over NFS, as the client clientID, which the network must reach.
func Volumes(root string, vols []*guestv1.Volume, clientID string, logf func(format string, args ...any)) error {
	if slices.ContainsFunc(vols, func(v *guestv1.Volume) bool { return v.GetFileShare() != nil }) {
		if err := setNFSClientID(clientID); err != nil {
			return err
		}
	}
	// Where each target leads in the image as it is: what a new volume is
	// filled from, and the order the volumes are bound in, the outer first.
	in := make([]string, len(vols))
	for i, v := range vols {
		t, err := imageTarget(root, v.GetTarget())
		if err != nil {
			return fmt.Errorf("volume %s at %s: %w", v.GetName(), v.GetTarget(), err)
		}
		if j := slices.Index(in[:i], t); j >= 0 {
			return fmt.Errorf("volume %s at %s leads to %s, as volume %s does", v.GetName(), v.GetTarget(), t, vols[j].GetName())
		}
		in[i] = t
	}
	staged := make([]string, len(vols))
	defer func() {
		for _, dir := range staged {
			if dir != "" {
				_ = unix.Unmount(dir, unix.MNT_DETACH)
			}
		}
	}()
	for i, v := range vols {
		staged[i] = filepath.Join(stagingDir, fmt.Sprint(i))
		if err := stage(root, v, in[i], staged[i], logf); err != nil {
			return fmt.Errorf("volume %s at %s: %w", v.GetName(), v.GetTarget(), err)
		}
	}
	order := bindOrder(in)
	rootfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(rootfd) }()
	// Read-only only once all are bound, so an inner mountpoint can be made
	// in a read-only outer volume.
	bound := make([]int, len(vols))
	defer func() {
		for _, fd := range bound {
			if fd > 0 {
				_ = unix.Close(fd)
			}
		}
	}()
	for _, i := range order {
		fd, err := bind(root, rootfd, vols[i], staged[i])
		if err != nil {
			return fmt.Errorf("volume %s at %s: %w", vols[i].GetName(), vols[i].GetTarget(), err)
		}
		bound[i] = fd
	}
	for i, v := range vols {
		if !v.GetReadOnly() {
			continue
		}
		// Not a bind remount, which would clear nosuid and nodev.
		ro := &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY}
		if err := unix.MountSetattr(bound[i], "", unix.AT_EMPTY_PATH, ro); err != nil {
			return fmt.Errorf("volume %s at %s: making it read-only: %w", v.GetName(), v.GetTarget(), err)
		}
	}
	return nil
}

// imageTarget is where target leads in root, refused if that is / or where
// the platform mounts its own filesystems.
func imageTarget(root, target string) (string, error) {
	t, err := resolveIn(root, target)
	if err != nil {
		return "", err
	}
	if platformOwned(t) {
		return "", fmt.Errorf("it leads to %s, where the platform mounts its own", t)
	}
	return t, nil
}

// bindOrder is the order volumes leading to the paths in are bound in: the
// outer before the inner, by where each leads, not by its target as written.
func bindOrder(in []string) []int {
	order := make([]int, len(in))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return depth(in[a]) - depth(in[b]) })
	return order
}

// depth counts a path's components, so a volume is bound after the
// volumes it lies in.
func depth(p string) int {
	return strings.Count(filepath.Clean("/"+p), "/")
}

// stage mounts v whole at dir: a FileShare over NFS; a disk formatted if it
// is blank, grown to the device, and, when it is new, filled from src in
// root.
func stage(root string, v *guestv1.Volume, src, dir string, logf func(string, ...any)) error {
	if v.GetFileShare() != nil {
		m, err := shareMount(v, dir)
		if err != nil {
			return err
		}
		return m.do()
	}
	if v.GetDevice() == "" {
		return errors.New("it names neither a device nor a FileShare")
	}
	fresh, err := MountData(v.GetDevice(), dir, 0, "volume "+v.GetName(), logf)
	if err != nil {
		return err
	}
	if !fresh || v.GetNocopy() || v.GetSubpath() != "" {
		return nil
	}
	fi, err := os.Lstat(filepath.Join(root, src))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil // made at bind, as Docker makes it
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("%s in the image is not a directory", v.GetTarget())
	}
	if err := copyTree(filepath.Join(root, src), dir); err != nil {
		return fmt.Errorf("filling it from the image's %s: %w", v.GetTarget(), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 0 {
		logf("hangar-init: filled volume %s from the image's %s", v.GetName(), v.GetTarget())
	}
	return nil
}

// bind mounts what the container sees of a staged volume, the volume or its
// subpath, at its target in root, resolved again now the outer volumes are
// there, and returns an fd of the mount. Each is resolved to an fd that is
// then mounted, never by name again.
func bind(root string, rootfd int, v *guestv1.Volume, staged string) (int, error) {
	src, err := unix.Open(staged, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	defer func() { _ = unix.Close(src) }()
	if sub := v.GetSubpath(); sub != "" {
		fd, err := openDirIn(src, sub)
		if err != nil {
			return -1, fmt.Errorf("subpath %s is not a directory of the volume: %w", sub, err)
		}
		_ = unix.Close(src)
		src = fd
	}
	target, err := makeDirIn(root, rootfd, v.GetTarget(), platformOwned)
	if err != nil {
		return -1, fmt.Errorf("%s in the image is not a directory: %w", v.GetTarget(), err)
	}
	defer func() { _ = unix.Close(target) }()
	where, err := whereIn(root, target)
	if err != nil {
		return -1, err
	}
	if platformOwned(where) {
		return -1, fmt.Errorf("it leads to %s, where the platform mounts its own", where)
	}
	tree, err := unix.OpenTree(src, "", unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|unix.AT_EMPTY_PATH)
	if err != nil {
		return -1, fmt.Errorf("cloning it: %w", err)
	}
	if err := unix.MoveMount(tree, "", target, "", unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH); err != nil {
		_ = unix.Close(tree)
		return -1, fmt.Errorf("binding it: %w", err)
	}
	return tree, nil
}

// MountData mounts the ext4 filesystem on dev at dir with flags, making it
// first when dev is blank, and grows it to dev; what, such as "volume
// data", names it in logf's messages. A dev holding anything else is
// refused, never formatted. mkfsArgs go to mke2fs before the device. It
// reports whether the filesystem is new, holding nothing, an empty
// lost+found removed.
func MountData(dev, dir string, flags uintptr, what string, logf func(string, ...any), mkfsArgs ...string) (fresh bool, err error) {
	kind, err := probeDevice(dev)
	if err != nil {
		return false, err
	}
	switch kind {
	case contentBlank:
		if err := format(dev, mkfsArgs...); err != nil {
			return false, err
		}
		logf("hangar-init: %s is new: made an ext4 filesystem on %s", what, dev)
	case contentExt4:
	default:
		return false, fmt.Errorf("%s holds %s, neither ext4 nor zeros, and is never formatted", dev, kind)
	}
	if err := (mount{dev, dir, "ext4", "", flags}).do(); err != nil {
		return false, err
	}
	fresh, err = newVolume(dir)
	if err != nil {
		return false, err
	}
	if from, to, err := GrowRoot(dir, dev); err != nil {
		logf("hangar-init: %s stays its size: %v", what, err)
	} else if to > from {
		logf("hangar-init: grew %s from %d to %d blocks", what, from, to)
	}
	return fresh, nil
}

// format makes an ext4 filesystem on a blank device, with no root reserve,
// as the root disk has none. No reaper runs before the root switch, so
// os/exec may wait for mke2fs.
func format(dev string, args ...string) error {
	args = append(append([]string{"-q", "-F", "-t", "ext4", "-m", "0"}, args...), dev)
	out, err := exec.Command(Mke2fs, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mke2fs %s: %w: %s", dev, err, bytes.TrimSpace(out[:min(len(out), 1024)]))
	}
	return nil
}

// content is what a volume's device holds, as far as its ends tell.
type content string

const (
	contentBlank          content = "zeros"
	contentExt4           content = "ext4"
	contentPartitionTable content = "a partition table"
	contentUnknown        content = "data"
)

func probeDevice(dev string) (content, error) {
	f, err := os.Open(dev)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	var size uint64
	if err := ioctl(f, blkGetSize64, unsafe.Pointer(&size)); err != nil {
		return "", fmt.Errorf("the size of %s: %w", dev, err)
	}
	return probe(f, int64(size))
}

// probe says what a device of size bytes holds: zeros at both ends, an
// ext2/3/4 superblock, a partition table, or other data.
func probe(r io.ReaderAt, size int64) (content, error) {
	head := make([]byte, min(size, probeSize))
	if _, err := r.ReadAt(head, 0); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	tail := make([]byte, min(size, probeSize))
	if _, err := r.ReadAt(tail, size-int64(len(tail))); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	switch {
	case allZero(head) && allZero(tail):
		return contentBlank, nil
	case len(head) >= superblockAt+0x3a && head[superblockAt+0x38] == ext4Magic&0xff && head[superblockAt+0x39] == ext4Magic>>8:
		return contentExt4, nil
	case len(head) >= 520 && (string(head[512:520]) == "EFI PART" || head[510] == 0x55 && head[511] == 0xaa):
		return contentPartitionTable, nil
	}
	return contentUnknown, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// newVolume reports whether a volume's root is new: it holds nothing, or
// only an empty lost+found directory, which it removes, as a new Docker
// volume holds nothing (initdb, for one, refuses a lost+found).
func newVolume(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	switch {
	case len(entries) == 0:
		return true, nil
	case len(entries) > 1 || entries[0].Name() != "lost+found" || !entries[0].IsDir():
		return false, nil
	}
	// Refused, and so kept, when it holds what e2fsck found.
	switch err := unix.Rmdir(filepath.Join(dir, "lost+found")); {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST):
		return false, nil
	default:
		return false, err
	}
}
