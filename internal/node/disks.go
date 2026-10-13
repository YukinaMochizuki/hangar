package node

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// diskController keeps the volumes of the Disks bound to this node: each at
// the size its status says, made from its image if it has one, and gone
// once its deletion no longer waits for a machine.
func (a *Agent) diskController() *controller.Controller {
	return &controller.Controller{
		Name:       "disks",
		Source:     controller.APISource{Client: a.api},
		Reconciler: controller.ReconcilerFunc(a.reconcileDisk),
		Watches: []controller.Watch{
			{Kind: registry.Disk.Kind, Map: a.diskIfMine},
			// An image changing may let its disks be made. On a resync
			// every disk is queued already.
			{Kind: registry.Image.Kind, Map: a.disksOfImage, SkipResync: true},
		},
		// An image's base made is looked at as soon as it is.
		Trigger: a.diskTrigger,
		Clock:   a.cfg.Clock,
		Log:     a.cfg.Log,
	}
}

func (a *Agent) diskIfMine(_ context.Context, o store.Object) []controller.Key {
	var d storagev1alpha1.Disk
	if proto.Unmarshal(o.Data, &d) != nil || d.GetStatus().GetNode() != a.cfg.Name {
		return nil
	}
	return []controller.Key{{Project: o.Project, Name: o.Name}}
}

// disksOfImage maps an image to the disks bound to this node made from it.
func (a *Agent) disksOfImage(ctx context.Context, o store.Object) []controller.Key {
	objs, _, err := a.api.ListAll(ctx, registry.Disk, client.ListOptions{})
	if err != nil {
		a.cfg.Log.Error("listing disks failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, obj := range objs {
		d := obj.(*storagev1alpha1.Disk)
		if d.GetStatus().GetNode() == a.cfg.Name && d.GetSpec().GetSource().GetImage().GetName() == o.Name {
			keys = append(keys, controller.Key{Project: d.GetMetadata().GetProject(), Name: d.GetMetadata().GetName()})
		}
	}
	return keys
}

// reconcileDisk brings the host in line with one disk bound to this node.
func (a *Agent) reconcileDisk(ctx context.Context, k controller.Key) error {
	a.host.Lock()
	defer a.host.Unlock()
	d, err := a.getDisk(ctx, k.Project, k.Name)
	if err != nil || d == nil || d.GetStatus().GetNode() != a.cfg.Name {
		return err
	}
	uid := d.GetMetadata().GetUid()
	if d.GetMetadata().GetDeleteTime() != nil {
		if conditions.IsTrue(d.GetStatus().GetConditions(), computeapi.DiskReleased) ||
			slices.Contains(d.GetMetadata().GetFinalizers(), computeapi.DiskInUseFinalizer) {
			return nil
		}
		if err := a.cfg.Drivers.Storage.DeleteVolume(ctx, uid); err != nil {
			return err
		}
		return a.writeDiskStatus(ctx, d, &metav1.Condition{
			Type: computeapi.DiskReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "Deleted",
			Message: "the disk is deleted",
		}, &metav1.Condition{
			Type: computeapi.DiskReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned",
			Message: "the disk's volume is gone from the host",
		})
	}
	_, err = a.makeVolume(ctx, d)
	ready := &metav1.Condition{Type: computeapi.DiskReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Created"}
	notReady := func(reason, msg string) *metav1.Condition {
		return &metav1.Condition{Type: computeapi.DiskReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reason, Message: msg}
	}
	var s *stuck
	switch {
	case errors.Is(err, controller.ErrWaiting):
		ready = notReady(computeapi.ReasonPulling, "making the disk from image "+d.GetSpec().GetSource().GetImage().GetName())
	case errors.As(err, &s):
		ready = notReady(s.reason, s.err.Error())
	case errors.Is(err, driver.ErrNoSpace):
		ready = notReady("NoSpace", err.Error())
	case err != nil:
		ready = notReady(computeapi.ReasonDiskFailed, err.Error())
	}
	if werr := a.writeDiskStatus(ctx, d, ready); werr != nil {
		return werr
	}
	return err
}

// statusSize is the size d has on its node, which binding sets.
func statusSize(d *storagev1alpha1.Disk) (int64, error) {
	size, err := quantity.Bytes(d.GetStatus().GetSize())
	if err != nil {
		return 0, fmt.Errorf("disk %s: status.size: %w", d.GetMetadata().GetName(), err)
	}
	if size > 1<<62 {
		return 0, fmt.Errorf("disk %s: status.size %s is too large", d.GetMetadata().GetName(), d.GetStatus().GetSize())
	}
	return int64(size), nil
}

// volumeOn returns the host's volume of the disk uid, if it has one.
func (a *Agent) volumeOn(ctx context.Context, uid string) (driver.Volume, bool, error) {
	vols, err := a.cfg.Drivers.Storage.Volumes(ctx)
	if err != nil {
		return driver.Volume{}, false, err
	}
	i := slices.IndexFunc(vols, func(v driver.Volume) bool { return v.DiskUID == uid })
	if i < 0 {
		return driver.Volume{}, false, nil
	}
	return vols[i], true, nil
}

// makeVolume makes d's volume at the size its status says: blank, or from
// its image once the image's base is made, which a job does off the host
// lock; until then it returns controller.ErrWaiting. A volume made is kept
// whatever its image has become since.
func (a *Agent) makeVolume(ctx context.Context, d *storagev1alpha1.Disk) (driver.Volume, error) {
	st := a.cfg.Drivers.Storage
	uid := d.GetMetadata().GetUid()
	size, err := statusSize(d)
	if err != nil {
		return driver.Volume{}, err
	}
	have, ok, err := a.volumeOn(ctx, uid)
	if err != nil {
		return driver.Volume{}, err
	}
	src := d.GetSpec().GetSource().GetImage()
	if ok || src == nil {
		return st.EnsureVolume(ctx, uid, size, have.Digest)
	}
	img, err := a.getImage(ctx, src.GetName())
	if err != nil {
		return driver.Volume{}, err
	}
	if img == nil {
		return driver.Volume{}, blocked(computeapi.ReasonImagePullFailed, fmt.Errorf("image %s does not exist", src.GetName()))
	}
	gen := img.GetMetadata().GetGeneration()
	if c := conditions.Find(img.GetStatus().GetConditions(), computeapi.ImageVerified); c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE &&
		c.GetObservedGeneration() == gen {
		return driver.Volume{}, blocked(computeapi.ReasonImagePullFailed, fmt.Errorf("image %s: %s", src.GetName(), c.GetMessage()))
	}
	digest := "sha256:" + img.GetSpec().GetSha256()
	bases, err := st.ImageBases(ctx)
	if err != nil {
		return driver.Volume{}, err
	}
	if !slices.Contains(bases, digest) {
		if a.cfg.Drivers.DiskImages == nil {
			return driver.Volume{}, blocked(computeapi.ReasonImagePullFailed, errors.New("this node has no disk image driver"))
		}
		made, _, err := a.images.get(img.GetSpec().GetUrl(), digest, gen)
		switch {
		case err != nil && isImageFault(err):
			a.reportImage(ctx, img, &metav1.Condition{
				Type: computeapi.ImageVerified, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: faultReason(err), Message: err.Error(),
			}, 0)
			return driver.Volume{}, blocked(computeapi.ReasonImagePullFailed, err)
		case err != nil:
			return driver.Volume{}, blocked(computeapi.ReasonImagePullFailed, err)
		case !made:
			return driver.Volume{}, fmt.Errorf("%w: disk %s's image %s", controller.ErrWaiting, d.GetMetadata().GetName(), src.GetName())
		}
	}
	// The base is there: the image is good, at this generation too, as its
	// digest is the same. Its size is known if this agent made the base.
	a.reportImage(ctx, img, &metav1.Condition{
		Type: computeapi.ImageVerified, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Made",
		Message: "a node fetched the image, found it matches its sha256, and made a disk image of it",
	}, a.images.madeSize(digest))
	v, err := st.EnsureVolume(ctx, uid, size, digest)
	if errors.Is(err, driver.ErrDiskTooSmall) {
		return driver.Volume{}, blocked(computeapi.ReasonDiskTooSmall, err)
	}
	return v, err
}

// reportImage records what the node found of img, as read: its Verified
// condition and, if known, the size of the disk it holds. A failure to
// write is logged: the disk does not wait on it, and the next disk of the
// image writes it again.
func (a *Agent) reportImage(ctx context.Context, img *storagev1alpha1.Image, c *metav1.Condition, size int64) {
	status := &storagev1alpha1.ImageStatus{}
	if img.GetStatus() != nil {
		status = proto.Clone(img.GetStatus()).(*storagev1alpha1.ImageStatus)
	}
	gen := img.GetMetadata().GetGeneration()
	c.ObservedGeneration = gen
	status.Conditions = conditions.Set(status.GetConditions(), c, a.cfg.Clock.Now())
	status.ObservedGeneration = gen
	if size > 0 {
		status.DiskSize = quantity.FormatDisk(uint64(size))
	}
	if proto.Equal(status, img.GetStatus()) {
		return
	}
	rv := img.GetMetadata().GetResourceVersion()
	if _, err := a.imagesAPI.UpdateImageStatus(ctx, connect.NewRequest(&storagev1alpha1.UpdateImageStatusRequest{
		Name: img.GetMetadata().GetName(), Status: status, ExpectedResourceVersion: &rv,
	})); err != nil {
		a.cfg.Log.Warn("reporting an image failed", "image", img.GetMetadata().GetName(), "error", err)
	}
}

func (a *Agent) getImage(ctx context.Context, name string) (*storagev1alpha1.Image, error) {
	res, err := a.imagesAPI.GetImage(ctx, connect.NewRequest(&storagev1alpha1.GetImageRequest{Name: name}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return res.Msg.GetImage(), nil
}

// writeDiskStatus sets the node's conditions on d as read, keeping what the
// apiserver owns, and writes nothing if nothing changed. The write is
// conditional on d's resource version.
func (a *Agent) writeDiskStatus(ctx context.Context, d *storagev1alpha1.Disk, conds ...*metav1.Condition) error {
	status := &storagev1alpha1.DiskStatus{}
	if d.GetStatus() != nil {
		status = proto.Clone(d.GetStatus()).(*storagev1alpha1.DiskStatus)
	}
	for _, c := range conds {
		c.ObservedGeneration = d.GetMetadata().GetGeneration()
		status.Conditions = conditions.Set(status.GetConditions(), c, a.cfg.Clock.Now())
	}
	if proto.Equal(status, d.GetStatus()) {
		return nil
	}
	rv := d.GetMetadata().GetResourceVersion()
	_, err := a.disks.UpdateDiskStatus(ctx, connect.NewRequest(&storagev1alpha1.UpdateDiskStatusRequest{
		Project: d.GetMetadata().GetProject(), Name: d.GetMetadata().GetName(), Status: status, ExpectedResourceVersion: &rv,
	}))
	return err
}

// volumes makes the volumes of the disks m lists, in its order. A disk not
// yet bound to this node holds the machine up.
func (a *Agent) volumes(ctx context.Context, m *computev1alpha1.Machine) ([]driver.Volume, error) {
	var out []driver.Volume
	for _, md := range m.GetSpec().GetDisks() {
		d, err := a.getDisk(ctx, m.GetMetadata().GetProject(), md.GetDisk())
		if err != nil {
			return nil, err
		}
		if d == nil {
			return nil, blocked(computeapi.ReasonDiskNotReady, fmt.Errorf("disk %s does not exist", md.GetDisk()))
		}
		if d.GetStatus().GetNode() != a.cfg.Name {
			return nil, blocked(computeapi.ReasonDiskNotReady, fmt.Errorf("disk %s is not bound to this node", md.GetDisk()))
		}
		// A disk made from an image is the disk controller's to make; the
		// machine waits for it, and is queued again once it is ready.
		have, made, err := a.volumeOn(ctx, d.GetMetadata().GetUid())
		if err != nil {
			return nil, blocked(computeapi.ReasonDiskFailed, err)
		}
		if !made && d.GetSpec().GetSource().GetImage() != nil {
			msg := conditions.Find(d.GetStatus().GetConditions(), computeapi.DiskReady).GetMessage()
			return nil, blocked(computeapi.ReasonDiskNotReady, fmt.Errorf("disk %s is not made yet: %s", md.GetDisk(), msg))
		}
		size, err := statusSize(d)
		if err != nil {
			return nil, blocked(computeapi.ReasonDiskFailed, err)
		}
		v, err := a.cfg.Drivers.Storage.EnsureVolume(ctx, d.GetMetadata().GetUid(), size, have.Digest)
		if err != nil {
			return nil, blocked(computeapi.ReasonDiskFailed, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// machinesWithDisk maps a disk to the machines on this node that list it: a
// disk bound or grown unblocks or changes them.
func (a *Agent) machinesWithDisk(ctx context.Context, o store.Object) []controller.Key {
	objs, _, err := a.api.ListAll(ctx, registry.Machine, client.ListOptions{Project: o.Project})
	if err != nil {
		a.cfg.Log.Error("listing machines failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, obj := range objs {
		m := obj.(*computev1alpha1.Machine)
		if m.GetSpec().GetNode() != a.cfg.Name {
			continue
		}
		if slices.ContainsFunc(m.GetSpec().GetDisks(), func(d *computev1alpha1.MachineDisk) bool { return d.GetDisk() == o.Name }) {
			keys = append(keys, controller.Key{Project: o.Project, Name: m.GetMetadata().GetName()})
		}
	}
	return keys
}

func (a *Agent) getDisk(ctx context.Context, project, name string) (*storagev1alpha1.Disk, error) {
	res, err := a.disks.GetDisk(ctx, connect.NewRequest(&storagev1alpha1.GetDiskRequest{Project: project, Name: name}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return res.Msg.GetDisk(), nil
}
