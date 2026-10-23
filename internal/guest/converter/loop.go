package converter

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// LoopControl is the kernel's loop control device.
const LoopControl = "/dev/loop-control"

// MapFile maps the size bytes of dev at offset, the uploaded file between
// the zeros the node staged it with, as a read-only loop device of that
// size alone, rounded up to a whole sector, as a block device holds, with
// the staged zeros after it; and returns the device. qemu-img then reads
// the file as the tenant sent it, and nothing of dev before it. A dev
// shorter than offset and that size together is refused.
func MapFile(dev string, offset, size int64) (string, error) {
	if offset < 0 || offset%512 != 0 || size <= 0 {
		return "", fmt.Errorf("the file at %d, of %d bytes, is not where a disk holds one", offset, size)
	}
	backing, err := os.OpenFile(dev, os.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer func() { _ = backing.Close() }()
	have, err := backing.Seek(0, io.SeekEnd)
	if err != nil {
		return "", err
	}
	size = (size + 511) &^ 511
	if err := fits(have, offset, size); err != nil {
		return "", fmt.Errorf("%s: %w", dev, err)
	}
	ctl, err := os.OpenFile(LoopControl, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer func() { _ = ctl.Close() }()
	n, err := unix.IoctlRetInt(int(ctl.Fd()), unix.LOOP_CTL_GET_FREE)
	if err != nil {
		return "", fmt.Errorf("a free loop device: %w", err)
	}
	loop := fmt.Sprintf("/dev/loop%d", n)
	// devtmpfs makes the node as the kernel adds the device.
	var lf *os.File
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if lf, err = os.OpenFile(loop, os.O_RDONLY, 0); err == nil || !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = lf.Close() }()
	if err := unix.IoctlLoopConfigure(int(lf.Fd()), &unix.LoopConfig{
		Fd:   uint32(backing.Fd()),
		Info: unix.LoopInfo64{Offset: uint64(offset), Sizelimit: uint64(size), Flags: unix.LO_FLAGS_READ_ONLY},
	}); err != nil {
		return "", fmt.Errorf("mapping %s on %s: %w", dev, loop, err)
	}
	got, err := lf.Seek(0, io.SeekEnd)
	if err != nil {
		return "", fmt.Errorf("%s once mapped: %w", loop, err)
	}
	if got != size {
		return "", fmt.Errorf("%s holds %d bytes once mapped, not the file's %d", loop, got, size)
	}
	return loop, nil
}

// fits refuses a disk of have bytes that cannot hold size bytes at offset.
func fits(have, offset, size int64) error {
	if have < offset || have-offset < size {
		return fmt.Errorf("a disk of %d bytes holds no file of %d bytes at %d", have, size, offset)
	}
	return nil
}
