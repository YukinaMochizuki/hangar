package node

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	// DefaultConvertTimeout bounds one conversion: room for the largest
	// disk, written at 100 MB/s, as a pool of spinning disks does, and the
	// converter VM's boots.
	DefaultConvertTimeout = 2 * time.Hour
	// reportEvery is how often, at most, a chunk counted has an upload's
	// received_bytes reported.
	reportEvery = 250 * time.Millisecond
)

// newConvertMaker converts uploaded files into CustomImages' bases, each
// in converter VMs (driver.Converter): first what the file is, which the
// node judges, then the file written into the base. A failure is a
// *stuck naming why, the base removed; the staged file stays, for the
// controller to remove.
func newConvertMaker(conv driver.Converter, storage driver.Storage, staged func(uid string) string, maxDisk int64, timeout time.Duration, slots int) *baseMaker {
	return newBaseMaker("the conversion", func(ctx context.Context, uid string) (driver.ImageBase, error) {
		path := staged(uid)
		if _, err := os.Stat(path); err != nil {
			return driver.ImageBase{}, blocked(computeapi.ReasonConvertFailed, fmt.Errorf("the uploaded file is gone: %w", err))
		}
		info, err := conv.Inspect(ctx, uid, path)
		if errors.Is(err, driver.ErrInvalidImage) {
			return driver.ImageBase{}, blocked(computeapi.ReasonInvalidImage, err)
		}
		if err != nil {
			return driver.ImageBase{}, blocked(computeapi.ReasonConvertFailed, fmt.Errorf("reading what the file is: %w", err))
		}
		if err := checkImageInfo(info, maxDisk); err != nil {
			return driver.ImageBase{}, err
		}
		base, err := storage.FillImageBase(ctx, uid, info.VirtualSize, func(dev string) error {
			return conv.Convert(ctx, uid, path, info.Format, dev)
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
// file's sha256, has it converted off the host lock, and reports Ready,
// or the failure, for good. The staged file goes once the image is Ready
// or failed. The caller holds the host lock.
func (a *Agent) reconcileUpload(ctx context.Context, ci *storagev1alpha1.CustomImage) error {
	uid := ci.GetMetadata().GetUid()
	up := ci.GetSpec().GetSource().GetUpload()
	if a.stager == nil {
		a.cfg.Log.Warn("a custom image to upload lives on this node, which takes no uploads", "image", ci.GetMetadata().GetName())
		return nil
	}
	status := statusOf(ci)
	if computeapi.CustomImageFailed(ci) || computeapi.CustomImageReadyNow(ci) {
		if err := a.stager.remove(uid); err != nil {
			return err
		}
		return a.writeStatusAgainst(ctx, ci, status)
	}
	if status.UploadExpires == nil {
		status.UploadExpires = timestamppb.New(ci.GetMetadata().GetCreateTime().AsTime().Add(a.cfg.UploadExpiry))
	}
	if !conditions.IsTrue(status.GetConditions(), computeapi.CustomImageUploaded) {
		a.stager.expect(uid, up.GetSize())
		st, err := a.stager.state(uid, up.GetSize())
		if err != nil {
			return err
		}
		status.ReceivedBytes = st.Received
		if st.Received < up.GetSize() {
			if !a.cfg.Clock.Now().Before(status.GetUploadExpires().AsTime()) {
				if err := a.stager.remove(uid); err != nil {
					return err
				}
				return a.writeStatusAgainst(ctx, ci, status, uploadFailed(ci, computeapi.ReasonUploadExpired,
					fmt.Errorf("the file was not whole by %s: %d of its %d bytes came", status.GetUploadExpires().AsTime().Format(time.RFC3339), st.Received, up.GetSize()))...)
			}
			msg := fmt.Sprintf("%d of %d bytes uploaded", st.Received, up.GetSize())
			return a.writeStatusAgainst(ctx, ci, status,
				uploadCondition(metav1.ConditionStatus_CONDITION_STATUS_FALSE, computeapi.ReasonAwaitingUpload, msg),
				readyCondition(metav1.ConditionStatus_CONDITION_STATUS_FALSE, computeapi.ReasonAwaitingUpload, msg))
		}
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
		// Written before the conversion starts: the file is the node's
		// from now.
		if err := a.writeStatusAgainst(ctx, ci, status,
			uploadCondition(metav1.ConditionStatus_CONDITION_STATUS_TRUE, computeapi.ReasonUploaded, "the whole file is in"),
			readyCondition(metav1.ConditionStatus_CONDITION_STATUS_FALSE, computeapi.ReasonConverting, "converting the file into the image")); err != nil {
			return err
		}
	}
	return a.followConversion(ctx, ci.GetMetadata().GetProject(), ci.GetMetadata().GetName(), uid)
}

// followConversion reports the conversion of the image of uid, started if
// need be, once it has ended; the image is read again, as the caller may
// have written it.
func (a *Agent) followConversion(ctx context.Context, project, name, uid string) error {
	base, done, err := a.converts.get(uid)
	if err == nil && !done {
		return fmt.Errorf("%w: custom image %s being converted", controller.ErrWaiting, name)
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

// statusOf is a copy of ci's status to change.
func statusOf(ci *storagev1alpha1.CustomImage) *storagev1alpha1.CustomImageStatus {
	if ci.GetStatus() == nil {
		return &storagev1alpha1.CustomImageStatus{}
	}
	return proto.Clone(ci.GetStatus()).(*storagev1alpha1.CustomImageStatus)
}
