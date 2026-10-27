package boot

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Linux ioctls the growth needs: the block device's size in bytes, and
// ext4's online resize to a block count.
const (
	blkGetSize64   = 0x80081272 // BLKGETSIZE64, _IOR(0x12, 114, size_t)
	ext4ResizeFS   = 0x40086610 // EXT4_IOC_RESIZE_FS, _IOW('f', 16, __u64)
	superblockAt   = 1024
	ext4Magic      = 0xef53
	incompat64bit  = 0x80
	superblockSize = 1024
)

// GrowRoot grows the ext4 filesystem on dev, mounted at dir, to fill dev,
// and returns its block count before and after: the same when it filled
// dev already. It never shrinks one.
func GrowRoot(dir, dev string) (from, to uint64, err error) {
	return grow(dev, dir, func() (*os.File, error) { return os.Open(dir) })
}

// grow grows the ext4 filesystem on dev, named dir in errors, to fill dev,
// through a file of it that open opens, as GrowRoot does.
func grow(dev, dir string, open func() (*os.File, error)) (from, to uint64, err error) {
	d, err := os.Open(dev)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = d.Close() }()
	var size uint64
	if err := ioctl(d, blkGetSize64, unsafe.Pointer(&size)); err != nil {
		return 0, 0, fmt.Errorf("the size of %s: %w", dev, err)
	}
	blocks, blockSize, err := ext4Blocks(io.NewSectionReader(d, superblockAt, superblockSize))
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", dev, err)
	}
	want := size / blockSize
	if want <= blocks {
		return blocks, blocks, nil
	}
	f, err := open()
	if err != nil {
		return blocks, blocks, err
	}
	defer func() { _ = f.Close() }()
	if err := ioctl(f, ext4ResizeFS, unsafe.Pointer(&want)); err != nil {
		return blocks, blocks, fmt.Errorf("resizing %s from %d to %d blocks: %w", dir, blocks, want, err)
	}
	return blocks, want, nil
}

// ext4Blocks reads an ext4 superblock's block count and block size.
func ext4Blocks(r io.Reader) (blocks, blockSize uint64, err error) {
	sb := make([]byte, superblockSize)
	if _, err := io.ReadFull(r, sb); err != nil {
		return 0, 0, fmt.Errorf("reading the superblock: %w", err)
	}
	le := binary.LittleEndian
	if le.Uint16(sb[0x38:]) != ext4Magic {
		return 0, 0, fmt.Errorf("no ext4 superblock")
	}
	logSize := le.Uint32(sb[0x18:])
	if logSize > 6 {
		return 0, 0, fmt.Errorf("a block size of 2^%d KiB", logSize)
	}
	blocks = uint64(le.Uint32(sb[0x04:]))
	if le.Uint32(sb[0x60:])&incompat64bit != 0 {
		blocks |= uint64(le.Uint32(sb[0x150:])) << 32
	}
	return blocks, 1024 << logSize, nil
}

func ioctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}
