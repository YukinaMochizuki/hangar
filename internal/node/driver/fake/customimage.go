package fake

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// capture is a snapshot of a volume taken for a CustomImage: the volume's
// disk and its size when taken.
type capture struct {
	diskUID string
	size    int64
}

// FailCapture makes capturing CustomImages' bases fail with err; nil
// clears it.
func (h *Host) FailCapture(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.captureErr = err
}

// HoldCaptures keeps every capture of a CustomImage's base from ending,
// from now until release is called, as a slow send does.
func (h *Host) HoldCaptures() (release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hold := make(chan struct{})
	h.captureHold = hold
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.captureHold == hold {
			h.captureHold = nil
		}
		close(hold)
	}
}

// CapturesUnderWay counts the captures of CustomImages' bases begun and
// not yet returned.
func (h *Host) CapturesUnderWay() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.capturing
}

// SetImageUsed makes each CustomImage's base captured from now on hold
// perMille thousandths of its size; it starts at half.
func (h *Host) SetImageUsed(perMille int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.usedPerMille = perMille
}

func (d storage) SnapshotVolume(_ context.Context, diskUID, imageUID string) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if c, ok := d.h.captures[imageUID]; ok {
		if c.diskUID != diskUID {
			return fmt.Errorf("custom image %s has a snapshot of disk %s, not %s", imageUID, c.diskUID, diskUID)
		}
		return nil
	}
	v, ok := d.h.volumes[diskUID]
	if !ok {
		return fmt.Errorf("disk %s has no volume", diskUID)
	}
	d.h.captures[imageUID] = capture{diskUID: diskUID, size: v.Size}
	return nil
}

func (d storage) CaptureImageBase(ctx context.Context, imageUID string) (driver.ImageBase, error) {
	key := driver.CustomImageBase(imageUID)
	d.h.mu.Lock()
	if size, ok := d.h.vmBases[key]; ok {
		defer d.h.mu.Unlock()
		return driver.ImageBase{Key: key, Size: size, Used: d.h.imageUsed[key]}, nil
	}
	c, ok := d.h.captures[imageUID]
	if !ok {
		d.h.mu.Unlock()
		return driver.ImageBase{}, fmt.Errorf("custom image %s: %w", imageUID, driver.ErrNoCapture)
	}
	used := c.size * d.h.usedPerMille / 1000
	failed, hold := d.h.captureErr, d.h.captureHold
	if failed == nil && (d.h.full || used > d.h.storageSize-d.h.usedLocked()) {
		failed = driver.ErrNoSpace
	}
	d.h.capturing++
	d.h.mu.Unlock()
	defer func() {
		d.h.mu.Lock()
		defer d.h.mu.Unlock()
		d.h.capturing--
	}()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return driver.ImageBase{}, ctx.Err()
		}
	}
	if failed != nil {
		return driver.ImageBase{}, fmt.Errorf("capturing custom image %s: %w", imageUID, failed)
	}
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	d.h.vmBases[key], d.h.imageUsed[key] = c.size, used
	delete(d.h.captures, imageUID)
	return driver.ImageBase{Key: key, Size: c.size, Used: used}, nil
}

func (d storage) Captures(context.Context) ([]driver.Capture, error) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if err := d.h.listErr[ListImageBases]; err != nil {
		return nil, err
	}
	var out []driver.Capture
	for _, image := range slices.Sorted(maps.Keys(d.h.captures)) {
		out = append(out, driver.Capture{ImageUID: image, DiskUID: d.h.captures[image].diskUID})
	}
	return out, nil
}

func (d storage) DeleteCapture(_ context.Context, imageUID string) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.removalErr != nil {
		return d.h.removalErr
	}
	delete(d.h.captures, imageUID)
	return nil
}
