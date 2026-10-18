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
// becomes /, as docs/decisions/container-volumes.md says.
func Volumes(root string, vols []*guestv1.Volume, logf func(format string, args ...any)) error {
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
	// Read-only only once all are bound, so an inner mountpoint can be made
	// in a read-only outer volume.
	bound := make([]string, len(vols))
	for _, i := range order {
		full, err := bind(root, vols[i], staged[i])
		if err != nil {
			return fmt.Errorf("volume %s at %s: %w", vols[i].GetName(), vols[i].GetTarget(), err)
		}
		bound[i] = full
	}
	for i, v := range vols {
		if v.GetReadOnly() {
			if err := unix.Mount("", bound[i], "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
				return fmt.Errorf("volume %s at %s: making it read-only: %w", v.GetName(), v.GetTarget(), err)
			}
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
	if t == "/" || slices.ContainsFunc(platformDirs, func(d string) bool { return t == d || strings.HasPrefix(t, d+"/") }) {
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

// stage mounts v's device whole at dir, formatting it if it is blank, grows
// it to the device, and, when it is new, fills it from src in root.
func stage(root string, v *guestv1.Volume, src, dir string, logf func(string, ...any)) error {
	dev := v.GetDevice()
	kind, err := probeDevice(dev)
	if err != nil {
		return err
	}
	switch kind {
	case contentBlank:
		if err := format(dev); err != nil {
			return err
		}
		logf("hangar-init: volume %s is new: made an ext4 filesystem on %s", v.GetName(), dev)
	case contentExt4:
	default:
		return fmt.Errorf("%s holds %s, neither ext4 nor zeros, and is never formatted", dev, kind)
	}
	if err := (mount{dev, dir, "ext4", "", 0}).do(); err != nil {
		return err
	}
	fresh, err := newVolume(dir)
	if err != nil {
		return err
	}
	if from, to, err := GrowRoot(dir, dev); err != nil {
		logf("hangar-init: volume %s stays its size: %v", v.GetName(), err)
	} else if to > from {
		logf("hangar-init: grew volume %s from %d to %d blocks", v.GetName(), from, to)
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

// bind mounts what the container sees of a staged volume at its target in
// root, resolved again now the outer volumes are there, and returns where:
// the volume, or its subpath.
func bind(root string, v *guestv1.Volume, staged string) (string, error) {
	src := staged
	if sub := v.GetSubpath(); sub != "" {
		in, err := resolveIn(staged, sub)
		if err != nil {
			return "", err
		}
		src = filepath.Join(staged, in)
		if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
			return "", fmt.Errorf("subpath %s is not a directory of the volume", sub)
		}
	}
	target, err := imageTarget(root, v.GetTarget())
	if err != nil {
		return "", err
	}
	full := filepath.Join(root, target)
	if err := os.MkdirAll(full, 0o755); err != nil {
		return "", err
	}
	if fi, err := os.Stat(full); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s in the image is not a directory", v.GetTarget())
	}
	if err := unix.Mount(src, full, "", unix.MS_BIND, ""); err != nil {
		return "", fmt.Errorf("binding it: %w", err)
	}
	return full, nil
}

// format makes an ext4 filesystem on a blank device, with no root reserve,
// as the root disk has none. No reaper runs before the root switch, so
// os/exec may wait for mke2fs.
func format(dev string) error {
	out, err := exec.Command(Mke2fs, "-q", "-F", "-t", "ext4", "-m", "0", dev).CombinedOutput()
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
