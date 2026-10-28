package chvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
)

// rootDiskID is a container's root disk's id in Cloud Hypervisor, which
// vm.resize-disk names it by.
const rootDiskID = "root"

func (h *Hypervisor) ResizeRoot(ctx context.Context, uid string) (int64, error) {
	if err := checkUID(uid); err != nil {
		return 0, err
	}
	unlock := h.locks.Lock(uid)
	defer unlock()
	unit, err := h.unit(ctx, uid)
	if err != nil {
		return 0, err
	}
	vm, listed, err := h.vm(uid, unit)
	if err != nil {
		return 0, err
	}
	if !listed || vm.State != driver.VMRunning {
		return 0, fmt.Errorf("machine %s has no running VM", uid)
	}
	dir := h.dir(uid)
	ln, err := readLaunch(dir)
	if err != nil {
		return 0, err
	}
	if ln.Spec.Firmware || blankVDA(ln.Spec) {
		return 0, fmt.Errorf("machine %s's VM has no root disk", uid)
	}
	// A container's VM started before Cloud Hypervisor served its API has
	// no socket.
	api := filepath.Join(dir, apiFile)
	if _, err := os.Lstat(api); errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("machine %s: %w", uid, driver.ErrNoLiveResize)
	}
	// The node the launcher made of the device Cloud Hypervisor has open,
	// in a directory only root writes in: Cloud Hypervisor refuses any size
	// but the device's.
	size, err := h.deviceSize(filepath.Join(dir, diskNode))
	if err != nil {
		return 0, fmt.Errorf("machine %s's root disk: %w", uid, err)
	}
	if err := h.resizeDisk(ctx, api, rootDiskID, size); err != nil {
		return 0, fmt.Errorf("machine %s: %w", uid, err)
	}
	return size, nil
}

// blockSize is the size in bytes of the block device at path, as the
// kernel has it now, refusing anything but a block device.
func blockSize(path string) (int64, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFBLK {
		return 0, fmt.Errorf("%s is no block device", path)
	}
	var size uint64
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.BLKGETSIZE64, uintptr(unsafe.Pointer(&size))); errno != 0 {
		return 0, fmt.Errorf("the size of %s: %w", path, errno)
	}
	if size > 1<<62 {
		return 0, fmt.Errorf("%s has %d bytes", path, size)
	}
	return int64(size), nil
}

// resizeDisk tells the VM whose Cloud Hypervisor serves its API at socket
// that its disk id is size bytes, which must be its device's size. The
// socket is dialled as pressPowerButton dials it, never through a link.
func resizeDisk(ctx context.Context, socket, id string, size int64) error {
	body, err := json.Marshal(struct {
		ID          string `json:"id"`
		DesiredSize int64  `json:"desired_size"`
	}{id, size})
	if err != nil {
		return err
	}
	c := http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return chguest.DialSocket(ctx, socket)
		},
	}}
	defer c.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost/api/v1/vm.resize-disk", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("resizing disk %s: %w", id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("resizing disk %s to %d bytes: %s: %s", id, size, resp.Status, b)
	}
	return nil
}
