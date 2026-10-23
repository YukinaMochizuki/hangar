package fake

import (
	"context"
	"fmt"
	"os"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// converter is the fake Converter. No VM boots: Inspect says what
// SetImageInfo set, or by default that the file is raw and holds a disk of
// its own size in whole sectors, as qemu-img says of a raw file. A staged
// file that is not whole sectors, or too short for its bytes, is refused,
// as Cloud Hypervisor would.
type converter struct{ h *Host }

// SetImageInfo makes Inspect say info of every file from now on; nil
// makes it read each file's size again.
func (h *Host) SetImageInfo(info *driver.ImageInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.imageInfo = info
}

// FailInspect and FailConvert make the converter's Inspect and Convert
// fail with err; nil clears it.
func (h *Host) FailInspect(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inspectErr = err
}

func (h *Host) FailConvert(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.convertErr = err
}

// HoldInspects keeps every Inspect from ending, from now until release is
// called, as a converter VM that stalls does.
func (h *Host) HoldInspects() (release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hold := make(chan struct{})
	h.inspectHold = hold
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.inspectHold == hold {
			h.inspectHold = nil
		}
		close(hold)
	}
}

// HoldConverts keeps every Convert from ending, from now until release
// is called, as a long conversion does.
func (h *Host) HoldConverts() (release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hold := make(chan struct{})
	h.convertHold = hold
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.convertHold == hold {
			h.convertHold = nil
		}
		close(hold)
	}
}

// ConvertsUnderWay counts the Converts begun and not yet returned.
func (h *Host) ConvertsUnderWay() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.converting
}

// Inspected and Converted count the converter's Inspect and Convert calls
// for the CustomImage of uid, failed ones included.
func (h *Host) Inspected(uid string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inspects[uid]
}

func (h *Host) Converted(uid string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.converts[uid]
}

func (c converter) Inspect(ctx context.Context, imageUID string, file driver.UploadedFile) (driver.ImageInfo, error) {
	c.h.mu.Lock()
	c.h.inspects[imageUID]++
	info, err, hold := c.h.imageInfo, c.h.inspectErr, c.h.inspectHold
	c.h.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return driver.ImageInfo{}, ctx.Err()
		}
	}
	if err != nil {
		return driver.ImageInfo{}, err
	}
	if err := checkStaged(file); err != nil {
		return driver.ImageInfo{}, err
	}
	if info != nil {
		return *info, nil
	}
	return driver.ImageInfo{Format: "raw", VirtualSize: (file.Size + 511) &^ 511}, nil
}

// checkStaged refuses a staged file a converter VM could not be given.
func checkStaged(file driver.UploadedFile) error {
	fi, err := os.Stat(file.Path)
	if err != nil {
		return err
	}
	if fi.Size()%512 != 0 || file.Offset%512 != 0 || fi.Size() < file.Offset+file.Size {
		return fmt.Errorf("%s, of %d bytes, holds no file of %d at %d in whole sectors", file.Path, fi.Size(), file.Size, file.Offset)
	}
	return nil
}

func (c converter) Convert(ctx context.Context, imageUID string, file driver.UploadedFile, format, dev string) error {
	c.h.mu.Lock()
	c.h.converts[imageUID]++
	c.h.converting++
	failed, hold := c.h.convertErr, c.h.convertHold
	c.h.mu.Unlock()
	defer func() {
		c.h.mu.Lock()
		defer c.h.mu.Unlock()
		c.h.converting--
	}()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if failed != nil {
		return failed
	}
	if err := checkStaged(file); err != nil {
		return err
	}
	if format != "raw" && format != "qcow2" {
		return fmt.Errorf("qemu-img convert -f %s: %w", format, driver.ErrInvalidImage)
	}
	if dev != "/dev/fake/vmbases/"+driver.CustomImageBase(imageUID) {
		return fmt.Errorf("%s is not the base of custom image %s", dev, imageUID)
	}
	return nil
}

func (d storage) FillImageBase(ctx context.Context, imageUID string, size int64, fill func(dev string) error) (driver.ImageBase, error) {
	key := driver.CustomImageBase(imageUID)
	d.h.mu.Lock()
	if made, ok := d.h.vmBases[key]; ok {
		defer d.h.mu.Unlock()
		return driver.ImageBase{Key: key, Size: made, Used: d.h.imageUsed[key]}, nil
	}
	// The fills under way count at their whole size, as the real pool's.
	full := d.h.full || size > d.h.storageSize-d.h.usedLocked()-d.h.filling
	used := size * d.h.usedPerMille / 1000
	if !full {
		d.h.filling += size
	}
	d.h.mu.Unlock()
	if full {
		return driver.ImageBase{}, fmt.Errorf("the base of custom image %s: %w", imageUID, driver.ErrNoSpace)
	}
	defer func() {
		d.h.mu.Lock()
		defer d.h.mu.Unlock()
		d.h.filling -= size
	}()
	if err := fill("/dev/fake/vmbases/" + key); err != nil {
		return driver.ImageBase{}, fmt.Errorf("filling the base of custom image %s: %w", imageUID, err)
	}
	if err := ctx.Err(); err != nil {
		return driver.ImageBase{}, err
	}
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	d.h.vmBases[key], d.h.imageUsed[key] = size, used
	return driver.ImageBase{Key: key, Size: size, Used: used}, nil
}
