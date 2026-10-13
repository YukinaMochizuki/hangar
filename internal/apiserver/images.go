package apiserver

import (
	"context"
	"net/url"
	"regexp"

	"google.golang.org/protobuf/proto"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validateImage(_ context.Context, _ *Server, _ auth.Principal, _, obj proto.Message) error {
	spec := obj.(*storagev1alpha1.Image).GetSpec()
	u, err := url.Parse(spec.GetUrl())
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return invalid("spec.url %q is not an https URL without credentials", spec.GetUrl())
	}
	if !sha256Hex.MatchString(spec.GetSha256()) {
		return invalid("spec.sha256 %q is not 64 lowercase hex digits", spec.GetSha256())
	}
	if a := spec.GetArch(); a != "" && a != "amd64" {
		return invalid("spec.arch %q: only amd64 is supported", a)
	}
	if m := spec.GetMinDiskSize(); m != "" {
		b, err := quantity.Bytes(m)
		if err != nil {
			return invalid("spec.min_disk_size: %v", err)
		}
		if b < minDisk || b > maxDisk {
			return invalid("spec.min_disk_size %s is not between 1Gi and 64Ti", m)
		}
	}
	return nil
}

// imageFloor is the smallest disk, in whole Gi, an image fits on: its
// min_disk_size, and its own size once a node has found it.
func imageFloor(img *storagev1alpha1.Image) uint64 {
	var floor uint64
	for _, q := range []string{img.GetSpec().GetMinDiskSize(), img.GetStatus().GetDiskSize()} {
		if b, err := quantity.Bytes(q); err == nil {
			floor = max(floor, wholeGi(b))
		}
	}
	return floor
}

// validateDiskSource checks a disk's image, if it has one: on create, a
// live image no node found at fault; on create and on a change of size, a
// size the image fits on. A size kept passes, whatever the image became.
func (s *Server) validateDiskSource(ctx context.Context, old, d *storagev1alpha1.Disk, size uint64) error {
	src := d.GetSpec().GetSource().GetImage()
	if src == nil {
		return nil
	}
	if err := ValidName(src.GetName()); err != nil {
		return invalid("spec.source.image.name: %v", err)
	}
	obj, err := s.get(ctx, registry.Image, resource.Key{Name: src.GetName()})
	if err != nil {
		return err
	}
	if old == nil {
		if obj == nil {
			return failedPrecondition("image %q does not exist", src.GetName())
		}
		img := obj.(*storagev1alpha1.Image)
		if img.GetMetadata().GetDeleteTime() != nil {
			return failedPrecondition("image %q is being deleted", src.GetName())
		}
		if c := conditions.Find(img.GetStatus().GetConditions(), computeapi.ImageVerified); c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE &&
			c.GetObservedGeneration() == img.GetMetadata().GetGeneration() {
			return failedPrecondition("image %q cannot be used: %s: %s", src.GetName(), c.GetReason(), c.GetMessage())
		}
	}
	if old != nil {
		if was, err := diskBytes(old.GetSpec()); err == nil && wholeGi(was) == wholeGi(size) {
			return nil
		}
	}
	if img, ok := obj.(*storagev1alpha1.Image); ok {
		if floor := imageFloor(img); wholeGi(size) < floor {
			return invalid("spec.size %s is smaller than image %q, which needs %dGi", d.GetSpec().GetSize(), src.GetName(), floor)
		}
	}
	return nil
}
