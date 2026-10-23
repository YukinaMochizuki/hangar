package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// Defaults of an upload's bounds, unless Config says otherwise.
const (
	// DefaultUploadExpiry is how long after its image is made an upload
	// may take to be whole.
	DefaultUploadExpiry = 24 * time.Hour
	// DefaultMaxImageDiskSize bounds the disk an uploaded file holds, as
	// diskimage.DefaultMaxDiskSize bounds an Image's.
	DefaultMaxImageDiskSize = 256 << 30
	// DefaultInspectTimeout bounds reading what one uploaded file is: a
	// converter VM's boot and qemu-img reading the file's headers.
	DefaultInspectTimeout = 5 * time.Minute
	// DefaultConvertTimeout bounds one conversion: room for the largest
	// disk, written at 100 MB/s, as a pool of spinning disks does, and the
	// converter VM's boot.
	DefaultConvertTimeout = 2 * time.Hour
	// reportEvery is how often, at most, a chunk counted has an upload's
	// received_bytes reported.
	reportEvery = 250 * time.Millisecond
)

// newInspector reads what uploaded files are, each in a converter VM
// (driver.Converter), and judges it (checkImageInfo). A failure is a
// *stuck naming why.
func newInspector(conv driver.Converter, staged func(uid string) (driver.UploadedFile, error), maxDisk int64, timeout time.Duration, slots *fifo) *jobs[driver.ImageInfo] {
	return newJobs("reading the file", func(ctx context.Context, uid string) (driver.ImageInfo, error) {
		file, err := staged(uid)
		if err != nil {
			return driver.ImageInfo{}, blocked(computeapi.ReasonConvertFailed, fmt.Errorf("the uploaded file: %w", err))
		}
		info, err := conv.Inspect(ctx, uid, file)
		if errors.Is(err, driver.ErrInvalidImage) {
			return driver.ImageInfo{}, blocked(computeapi.ReasonInvalidImage, err)
		}
		if err != nil {
			return driver.ImageInfo{}, blocked(computeapi.ReasonConvertFailed, fmt.Errorf("reading what the file is: %w", err))
		}
		return info, checkImageInfo(info, maxDisk)
	}, timeout, slots)
}

// newConvertMaker converts uploaded files into CustomImages' bases, each
// in a converter VM, once inspected reads what the file is: the file is
// written into a base of the disk's size. A failure is a *stuck naming
// why, the base removed; the staged file stays, for the controller to
// remove.
func newConvertMaker(conv driver.Converter, storage driver.Storage, staged func(uid string) (driver.UploadedFile, error), inspected func(uid string) (driver.ImageInfo, bool), timeout time.Duration, slots *fifo) *baseMaker {
	return newJobs("the conversion", func(ctx context.Context, uid string) (driver.ImageBase, error) {
		info, ok := inspected(uid)
		if !ok {
			return driver.ImageBase{}, blocked(computeapi.ReasonConvertFailed, errors.New("what the file is was not read before converting it"))
		}
		file, err := staged(uid)
		if err != nil {
			return driver.ImageBase{}, blocked(computeapi.ReasonConvertFailed, fmt.Errorf("the uploaded file: %w", err))
		}
		base, err := storage.FillImageBase(ctx, uid, info.VirtualSize, func(dev string) error {
			return conv.Convert(ctx, uid, file, info.Format, dev)
		})
		if err != nil {
			return driver.ImageBase{}, blocked(computeapi.ReasonConvertFailed, err)
		}
		return base, nil
	}, timeout, slots)
}

// checkImageInfo refuses what the host does not take of what a converter
// said of a file: a format other than raw or qcow2, one that would read
// another file, or a disk larger than maxDisk.
func checkImageInfo(info driver.ImageInfo, maxDisk int64) error {
	switch {
	case info.Format != "raw" && info.Format != "qcow2":
		return blocked(computeapi.ReasonInvalidImage, fmt.Errorf("the file is %q, not raw or qcow2", info.Format))
	case info.BackingFile != "" || info.DataFile != "":
		return blocked(computeapi.ReasonInvalidImage, errors.New("the image names another file, a backing or a data file, which is not taken"))
	case info.VirtualSize <= 0:
		return blocked(computeapi.ReasonInvalidImage, fmt.Errorf("the image holds a disk of %d bytes", info.VirtualSize))
	case info.VirtualSize > maxDisk:
		return blocked(computeapi.ReasonImageTooLarge, fmt.Errorf("the image holds a disk of %d bytes, more than %d (%s)",
			info.VirtualSize, maxDisk, quantity.FormatDisk(uint64(maxDisk))))
	}
	return nil
}

func uploadCondition(status metav1.ConditionStatus, reason, msg string) *metav1.Condition {
	return &metav1.Condition{Type: computeapi.CustomImageUploaded, Status: status, Reason: reason, Message: msg}
}

func readyCondition(status metav1.ConditionStatus, reason, msg string) *metav1.Condition {
	return &metav1.Condition{Type: computeapi.CustomImageReady, Status: status, Reason: reason, Message: msg}
}

// uploadFailed is what an upload that failed for good, as reason says,
// reports: Ready False and, if the file was never whole, Uploaded False.
func uploadFailed(ci *storagev1alpha1.CustomImage, reason string, err error) []*metav1.Condition {
	out := []*metav1.Condition{readyCondition(metav1.ConditionStatus_CONDITION_STATUS_FALSE, reason, err.Error())}
	if !conditions.IsTrue(ci.GetStatus().GetConditions(), computeapi.CustomImageUploaded) {
		out = append(out, uploadCondition(metav1.ConditionStatus_CONDITION_STATUS_FALSE, reason, err.Error()))
	}
	return out
}

// reconcileUpload brings an uploaded CustomImage on this node a step on:
// it takes chunks until the file is whole, by upload_expires, checks the
// file's sha256, has what it is read off the host lock and reports its
// virtual size, which the apiserver keeps only within the disk quota; once
// kept, it has the file converted off the host lock, and reports Ready, or
// the failure, for good. The staged file goes once the image is Ready or
// failed. On a node that takes no uploads the image only expires. The
// caller holds the host lock.
func (a *Agent) reconcileUpload(ctx context.Context, ci *storagev1alpha1.CustomImage) error {
	uid := ci.GetMetadata().GetUid()
	up := ci.GetSpec().GetSource().GetUpload()
	status := statusOf(ci)
	if computeapi.CustomImageFailed(ci) || computeapi.CustomImageReadyNow(ci) {
		if a.stager != nil {
			if err := a.stager.remove(uid); err != nil {
				return err
			}
		}
		return a.writeStatusAgainst(ctx, ci, status)
	}
	if status.UploadExpires == nil {
		status.UploadExpires = timestamppb.New(ci.GetMetadata().GetCreateTime().AsTime().Add(a.cfg.UploadExpiry))
	}
	uploaded := conditions.IsTrue(status.GetConditions(), computeapi.CustomImageUploaded)
	if !uploaded && a.stager != nil {
		a.stager.expect(uid, up.GetSize())
		st, err := a.stager.state(uid, up.GetSize())
		if err != nil {
			return err
		}
		status.ReceivedBytes = st.Received
	}
	if !uploaded && status.GetReceivedBytes() < up.GetSize() {
		if !a.cfg.Clock.Now().Before(status.GetUploadExpires().AsTime()) {
			if a.stager != nil {
				if err := a.stager.remove(uid); err != nil {
					return err
				}
			}
			return a.writeStatusAgainst(ctx, ci, status, uploadFailed(ci, computeapi.ReasonUploadExpired,
				fmt.Errorf("the file was not whole by %s: %d of its %d bytes came", status.GetUploadExpires().AsTime().Format(time.RFC3339), status.GetReceivedBytes(), up.GetSize()))...)
		}
		// Looked at again once it expires, if nothing comes before.
		a.wakeAt(uid, status.GetUploadExpires().AsTime())
		msg := fmt.Sprintf("%d of %d bytes uploaded", status.GetReceivedBytes(), up.GetSize())
		if a.stager == nil {
			msg = "this node takes no uploads; the image fails once its upload expires"
		}
		return a.writeStatusAgainst(ctx, ci, status,
			uploadCondition(metav1.ConditionStatus_CONDITION_STATUS_FALSE, computeapi.ReasonAwaitingUpload, msg),
			readyCondition(metav1.ConditionStatus_CONDITION_STATUS_FALSE, computeapi.ReasonAwaitingUpload, msg))
	}
	if a.stager == nil {
		a.cfg.Log.Warn("a custom image uploaded lives on this node, which takes no uploads", "image", ci.GetMetadata().GetName())
		return nil
	}
	if !uploaded {
		a.stager.stop(uid)
		sum, _, err := a.stager.sum(uid, up.GetSize())
		if err != nil {
			return err
		}
		if want := up.GetSha256(); want != "" && sum != want {
			if err := a.stager.remove(uid); err != nil {
				return err
			}
			return a.writeStatusAgainst(ctx, ci, status, uploadFailed(ci, computeapi.ReasonDigestMismatch,
				fmt.Errorf("the file has sha256 %s, not %s", sum, want))...)
		}
		status.Sha256 = sum
		// Written before the file is read: it is the node's from now.
		if err := a.writeStatusAgainst(ctx, ci, status,
			uploadCondition(metav1.ConditionStatus_CONDITION_STATUS_TRUE, computeapi.ReasonUploaded, "the whole file is in"),
			readyCondition(metav1.ConditionStatus_CONDITION_STATUS_FALSE, computeapi.ReasonConverting, "reading what the file is")); err != nil {
			return err
		}
	}
	return a.followConversion(ctx, ci.GetMetadata().GetProject(), ci.GetMetadata().GetName(), uid)
}

// followConversion has the file of the image of uid read, reports the
// virtual size found and, once the apiserver has kept it, has the file
// converted; it reports the conversion once it has ended. The image is
// read again, as the caller may have written it.
func (a *Agent) followConversion(ctx context.Context, project, name, uid string) error {
	info, inspected, err := a.inspects.get(uid)
	if err == nil && !inspected {
		return fmt.Errorf("%w: the file of custom image %s being read", controller.ErrWaiting, name)
	}
	var base driver.ImageBase
	if err == nil {
		var done bool
		if base, done, err = a.convertOnceKept(ctx, project, name, uid, info); !done {
			return err
		}
	}
	ci, gerr := a.getCustomImage(ctx, project, name)
	if gerr != nil || ci == nil || ci.GetMetadata().GetUid() != uid {
		return gerr
	}
	// The staged file goes once this is written, as the image, Ready or
	// failed, is reconciled again.
	status := statusOf(ci)
	if err != nil {
		reason := computeapi.ReasonConvertFailed
		var s *stuck
		if errors.As(err, &s) {
			reason, err = s.reason, s.err
		}
		return a.writeStatusAgainst(ctx, ci, status, uploadFailed(ci, reason, err)...)
	}
	status.UsedSize = quantity.FormatDisk(uint64(max(base.Used, 1)))
	status.DiskSize = quantity.FormatDisk((uint64(base.Size) + quantity.GiB - 1) / quantity.GiB * quantity.GiB)
	return a.writeStatusAgainst(ctx, ci, status,
		readyCondition(metav1.ConditionStatus_CONDITION_STATUS_TRUE, computeapi.ReasonConverted, "disks can be made from the image"))
}

// convertOnceKept has the file of the image of uid, found to hold info,
// converted once the apiserver has kept its virtual size, which the node
// reports first; done says the conversion has ended, as base, or err if
// it failed, which also fails a file found to hold another size than
// kept. Not done, err is nil or why the caller waits.
func (a *Agent) convertOnceKept(ctx context.Context, project, name, uid string, info driver.ImageInfo) (base driver.ImageBase, done bool, err error) {
	ci, err := a.getCustomImage(ctx, project, name)
	if err != nil || ci == nil || ci.GetMetadata().GetUid() != uid || computeapi.CustomImageFailed(ci) {
		return driver.ImageBase{}, false, err
	}
	switch kept := ci.GetStatus().GetVirtualSize(); {
	case kept == 0:
		// The apiserver keeps it within the disk quota, or fails the
		// image as QuotaExceeded: either way, the image is reconciled
		// again, and its base is made only once it is kept.
		status := statusOf(ci)
		status.VirtualSize = info.VirtualSize
		return driver.ImageBase{}, false, a.writeStatusAgainst(ctx, ci, status,
			readyCondition(metav1.ConditionStatus_CONDITION_STATUS_FALSE, computeapi.ReasonConverting,
				fmt.Sprintf("the file holds a disk of %d bytes, to convert once the disk quota has room for it", info.VirtualSize)))
	case kept != info.VirtualSize:
		return driver.ImageBase{}, true, blocked(computeapi.ReasonConvertFailed,
			fmt.Errorf("the file holds a disk of %d bytes, not the %d it was read to hold before", info.VirtualSize, kept))
	}
	if err := a.writeStatusAgainst(ctx, ci, statusOf(ci),
		readyCondition(metav1.ConditionStatus_CONDITION_STATUS_FALSE, computeapi.ReasonConverting, "converting the file into the image")); err != nil {
		return driver.ImageBase{}, false, err
	}
	base, done, err = a.converts.get(uid)
	if err == nil && !done {
		return driver.ImageBase{}, false, fmt.Errorf("%w: custom image %s being converted", controller.ErrWaiting, name)
	}
	return base, true, err
}

// wakeAt has the custom images' controller look again at t, once per
// image and time.
func (a *Agent) wakeAt(uid string, t time.Time) {
	a.wakeMu.Lock()
	defer a.wakeMu.Unlock()
	if a.wakes[uid].Equal(t) {
		return
	}
	a.wakes[uid] = t
	a.cfg.Clock.AfterFunc(t.Sub(a.cfg.Clock.Now()), func() {
		a.wakeMu.Lock()
		if a.wakes[uid].Equal(t) {
			delete(a.wakes, uid)
		}
		a.wakeMu.Unlock()
		notify(a.captureTrigger)
	})
}

// statusOf is a copy of ci's status to change.
func statusOf(ci *storagev1alpha1.CustomImage) *storagev1alpha1.CustomImageStatus {
	if ci.GetStatus() == nil {
		return &storagev1alpha1.CustomImageStatus{}
	}
	return proto.Clone(ci.GetStatus()).(*storagev1alpha1.CustomImageStatus)
}
