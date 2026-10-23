package computeapi

import (
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
)

// CustomImageFailed says the image's capture failed for good.
func CustomImageFailed(ci *storagev1alpha1.CustomImage) bool {
	c := conditions.Find(ci.GetStatus().GetConditions(), CustomImageReady)
	return c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE && c.GetReason() == ReasonCaptureFailed
}

// CustomImageReadyNow says the image's base is made: disks can be made
// from it.
func CustomImageReadyNow(ci *storagev1alpha1.CustomImage) bool {
	return conditions.IsTrue(ci.GetStatus().GetConditions(), CustomImageReady)
}

// CustomImageHoldsSource says the image still needs its source disk as it
// is: it is not snapshotted yet, nor failed, nor being deleted. Until then
// no machine with the disk may start, and none may attach it.
func CustomImageHoldsSource(ci *storagev1alpha1.CustomImage) bool {
	return ci.GetMetadata().GetDeleteTime() == nil &&
		!conditions.IsTrue(ci.GetStatus().GetConditions(), CustomImageSnapshotted) &&
		!CustomImageFailed(ci)
}
