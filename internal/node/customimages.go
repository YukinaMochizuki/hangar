package node

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

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

// captureMaker copies CustomImages' snapshots into their bases off the host
// lock, one job per image, so a copy of many gigabytes holds up no other
// disk or machine (docs/decisions/custom-images.md).
type captureMaker struct {
	storage driver.Storage
	// slots bounds the copies made at once.
	slots   *fifo
	running sync.WaitGroup

	mu     sync.Mutex
	ctx    context.Context
	notify func()
	jobs   map[string]*captureJob // by image uid
}

type captureJob struct {
	running, done bool
	cancel        context.CancelFunc
	base          driver.ImageBase
	// err is why the capture failed for good, as it is never tried again.
	err error
}

func newCaptureMaker(storage driver.Storage, slots int) *captureMaker {
	return &captureMaker{storage: storage, slots: newFIFO(slots), ctx: context.Background(), notify: func() {}, jobs: map[string]*captureJob{}}
}

// start runs jobs under ctx from now on and calls notify as each finishes.
func (m *captureMaker) start(ctx context.Context, notify func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ctx, m.notify = ctx, notify
}

// get says whether the base of the CustomImage of uid is made, and what it
// is; until it is, get starts making it. A capture that failed returns its
// error from then on: the agent never tries it again.
func (m *captureMaker) get(uid string) (driver.ImageBase, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[uid]
	switch {
	case j == nil:
		ctx, cancel := context.WithCancel(m.ctx)
		j = &captureJob{running: true, cancel: cancel}
		m.jobs[uid] = j
		m.running.Add(1)
		go m.run(ctx, j, uid)
		return driver.ImageBase{}, false, nil
	case j.running:
		return driver.ImageBase{}, false, nil
	}
	return j.base, j.done, j.err
}

func (m *captureMaker) run(ctx context.Context, j *captureJob, uid string) {
	defer m.running.Done()
	base, err := m.capture(ctx, uid)
	cut := ctx.Err() != nil
	m.mu.Lock()
	j.cancel()
	j.running = false
	switch {
	case err == nil:
		j.done, j.base = true, base
	case cut:
		// Cut short, not failed: the next get starts again.
		if m.jobs[uid] == j {
			delete(m.jobs, uid)
		}
	default:
		j.err = err
	}
	notify := m.notify
	m.mu.Unlock()
	notify()
}

// capture copies the image's snapshot into its base, a slot at a time. A
// failure leaves no base, and takes the snapshot with it.
func (m *captureMaker) capture(ctx context.Context, uid string) (driver.ImageBase, error) {
	if err := m.slots.acquire(ctx); err != nil {
		return driver.ImageBase{}, err
	}
	defer m.slots.release()
	base, err := m.storage.CaptureImageBase(ctx, uid)
	if err != nil && ctx.Err() == nil {
		if derr := m.storage.DeleteCapture(context.WithoutCancel(ctx), uid); derr != nil {
			err = errors.Join(err, fmt.Errorf("and its snapshot stays, for the sweep: %w", derr))
		}
	}
	return base, err
}

// keep ends and drops the jobs of images not in wanted, by uid, and
// returns the uids of the jobs left that still run.
func (m *captureMaker) keep(wanted map[string]bool) map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	running := map[string]bool{}
	for uid, j := range m.jobs {
		if !wanted[uid] {
			if j.running {
				j.cancel()
			}
			delete(m.jobs, uid)
			continue
		}
		if j.running {
			running[uid] = true
		}
	}
	return running
}

// wait returns once every job has ended.
func (m *captureMaker) wait() { m.running.Wait() }

// customImageController captures the CustomImages that live on this node:
// it snapshots the source disk's volume, reports Snapshotted, copies the
// snapshot into the image's base off the host lock, and reports Ready, or
// the failure, for good. What a deleted image leaves the sweep removes.
func (a *Agent) customImageController() *controller.Controller {
	return &controller.Controller{
		Name:       "customimages",
		Source:     controller.APISource{Client: a.api},
		Reconciler: controller.ReconcilerFunc(a.reconcileCustomImage),
		Watches: []controller.Watch{
			{Kind: registry.CustomImage.Kind, Map: a.customImageIfMine},
			// The source disk made lets it be snapshotted. On a resync
			// every image is queued already.
			{Kind: registry.Disk.Kind, Map: a.customImagesOfDisk, SkipResync: true},
		},
		// A capture ended is looked at as soon as it is.
		Trigger: a.captureTrigger,
		Clock:   a.cfg.Clock,
		Log:     a.cfg.Log,
	}
}

func (a *Agent) customImageIfMine(_ context.Context, o store.Object) []controller.Key {
	var ci storagev1alpha1.CustomImage
	if proto.Unmarshal(o.Data, &ci) != nil || ci.GetStatus().GetNode() != a.cfg.Name {
		return nil
	}
	return []controller.Key{{Project: o.Project, Name: o.Name}}
}

// customImagesOfDisk maps a disk to the images on this node captured from
// it.
func (a *Agent) customImagesOfDisk(ctx context.Context, o store.Object) []controller.Key {
	objs, _, err := a.api.ListAll(ctx, registry.CustomImage, client.ListOptions{Project: o.Project})
	if err != nil {
		a.cfg.Log.Error("listing custom images failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, obj := range objs {
		ci := obj.(*storagev1alpha1.CustomImage)
		if ci.GetStatus().GetNode() == a.cfg.Name && ci.GetSpec().GetSource().GetDisk().GetName() == o.Name {
			keys = append(keys, controller.Key{Project: o.Project, Name: ci.GetMetadata().GetName()})
		}
	}
	return keys
}

// captureFailed is a CustomImage's Ready condition once its capture failed
// for good.
func captureFailed(err error) *metav1.Condition {
	return &metav1.Condition{
		Type: computeapi.CustomImageReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE,
		Reason: computeapi.ReasonCaptureFailed, Message: err.Error(),
	}
}

func capturing(msg string) *metav1.Condition {
	return &metav1.Condition{
		Type: computeapi.CustomImageReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE,
		Reason: computeapi.ReasonCapturing, Message: msg,
	}
}

// reconcileCustomImage brings the capture of one CustomImage on this node
// a step on.
func (a *Agent) reconcileCustomImage(ctx context.Context, k controller.Key) error {
	a.host.Lock()
	defer a.host.Unlock()
	ci, err := a.getCustomImage(ctx, k.Project, k.Name)
	if err != nil || ci == nil || ci.GetStatus().GetNode() != a.cfg.Name || ci.GetMetadata().GetDeleteTime() != nil {
		return err
	}
	if computeapi.CustomImageFailed(ci) || computeapi.CustomImageReadyNow(ci) {
		// Done, for good or ill: only its generation is followed.
		return a.writeCustomImageStatus(ctx, ci, ci.GetStatus().GetUsedSize())
	}
	uid := ci.GetMetadata().GetUid()
	if !conditions.IsTrue(ci.GetStatus().GetConditions(), computeapi.CustomImageSnapshotted) {
		err := a.snapshotSource(ctx, ci)
		var s *stuck
		switch {
		case errors.As(err, &s):
			return a.writeCustomImageStatus(ctx, ci, "", captureFailed(s.err))
		case errors.Is(err, controller.ErrWaiting):
			return errors.Join(a.writeCustomImageStatus(ctx, ci, "", capturing(err.Error())), err)
		case err != nil:
			return err
		}
		// Written before the copy starts: the source disk is free from now.
		if err := a.writeCustomImageStatus(ctx, ci, "", &metav1.Condition{
			Type: computeapi.CustomImageSnapshotted, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: computeapi.ReasonCaptured,
			Message: "the source disk is snapshotted; it can be started, changed or deleted",
		}, capturing("copying the source disk's snapshot into the image")); err != nil {
			return err
		}
		return a.followCapture(ctx, k, uid)
	}
	return a.followCapture(ctx, k, uid)
}

// followCapture reports the capture of the image of uid, started if need
// be, once it has ended; the image is read again, as the caller may have
// written it.
func (a *Agent) followCapture(ctx context.Context, k controller.Key, uid string) error {
	base, done, err := a.captures.get(uid)
	if err == nil && !done {
		return fmt.Errorf("%w: custom image %s being captured", controller.ErrWaiting, k.Name)
	}
	ci, gerr := a.getCustomImage(ctx, k.Project, k.Name)
	if gerr != nil || ci == nil || ci.GetMetadata().GetUid() != uid {
		return gerr
	}
	if err != nil {
		if errors.Is(err, driver.ErrNoCapture) {
			err = errors.New("the snapshot of the source disk is gone; delete the image and capture it again")
		}
		return a.writeCustomImageStatus(ctx, ci, "", captureFailed(err))
	}
	return a.writeCustomImageStatus(ctx, ci, quantity.FormatDisk(uint64(max(base.Used, 1))), &metav1.Condition{
		Type: computeapi.CustomImageReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: computeapi.ReasonCaptured,
		Message: "disks can be made from the image",
	})
}

// snapshotSource snapshots the volume of ci's source disk. A source that
// can never be captured here is a *stuck; one not made yet is waited for.
func (a *Agent) snapshotSource(ctx context.Context, ci *storagev1alpha1.CustomImage) error {
	name := ci.GetSpec().GetSource().GetDisk().GetName()
	d, err := a.getDisk(ctx, ci.GetMetadata().GetProject(), name)
	switch {
	case err != nil:
		return err
	case d == nil:
		return blocked(computeapi.ReasonCaptureFailed, fmt.Errorf("the source disk %s is gone", name))
	case d.GetStatus().GetNode() != a.cfg.Name:
		return blocked(computeapi.ReasonCaptureFailed, fmt.Errorf("the source disk %s lives on node %s, not this one", name, d.GetStatus().GetNode()))
	}
	if _, made, err := a.volumeOn(ctx, d.GetMetadata().GetUid()); err != nil {
		return err
	} else if !made {
		return fmt.Errorf("%w: the source disk %s is not made on its node yet", controller.ErrWaiting, name)
	}
	return a.cfg.Drivers.Storage.SnapshotVolume(ctx, d.GetMetadata().GetUid(), ci.GetMetadata().GetUid())
}

// writeCustomImageStatus sets the node's conditions on ci as read, and its
// used size, keeping what the apiserver owns, and writes nothing if
// nothing changed. The write is conditional on ci's resource version.
func (a *Agent) writeCustomImageStatus(ctx context.Context, ci *storagev1alpha1.CustomImage, usedSize string, conds ...*metav1.Condition) error {
	status := &storagev1alpha1.CustomImageStatus{}
	if ci.GetStatus() != nil {
		status = proto.Clone(ci.GetStatus()).(*storagev1alpha1.CustomImageStatus)
	}
	gen := ci.GetMetadata().GetGeneration()
	for _, c := range conds {
		c.ObservedGeneration = gen
		status.Conditions = conditions.Set(status.GetConditions(), c, a.cfg.Clock.Now())
	}
	for _, c := range status.GetConditions() {
		c.ObservedGeneration = gen
	}
	status.ObservedGeneration, status.UsedSize = gen, usedSize
	if proto.Equal(status, ci.GetStatus()) {
		return nil
	}
	rv := ci.GetMetadata().GetResourceVersion()
	_, err := a.customImages.UpdateCustomImageStatus(ctx, connect.NewRequest(&storagev1alpha1.UpdateCustomImageStatusRequest{
		Project: ci.GetMetadata().GetProject(), Name: ci.GetMetadata().GetName(), Status: status, ExpectedResourceVersion: &rv,
	}))
	return a.metrics.statusWritten(err)
}

func (a *Agent) getCustomImage(ctx context.Context, project, name string) (*storagev1alpha1.CustomImage, error) {
	res, err := a.customImages.GetCustomImage(ctx, connect.NewRequest(&storagev1alpha1.GetCustomImageRequest{Project: project, Name: name}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return res.Msg.GetCustomImage(), nil
}

// customImageBase is the key of the base a disk of the custom image src
// names is cloned from, once the image is Ready on this node.
func (a *Agent) customImageBase(ctx context.Context, d *storagev1alpha1.Disk) (string, error) {
	src := d.GetSpec().GetSource().GetImage()
	ci, err := a.getCustomImage(ctx, src.GetProject(), src.GetName())
	switch {
	case err != nil:
		return "", err
	case ci == nil || ci.GetMetadata().GetDeleteTime() != nil:
		return "", blocked(computeapi.ReasonImageNotReady, fmt.Errorf("custom image %s is gone", src.GetName()))
	case !computeapi.CustomImageReadyNow(ci):
		return "", blocked(computeapi.ReasonImageNotReady, fmt.Errorf("custom image %s is not Ready", src.GetName()))
	case ci.GetStatus().GetNode() != a.cfg.Name:
		return "", blocked(computeapi.ReasonImageNotReady, fmt.Errorf(
			"custom image %s lives on node %s; disks of it are made only there", src.GetName(), ci.GetStatus().GetNode()))
	}
	return driver.CustomImageBase(ci.GetMetadata().GetUid()), nil
}
