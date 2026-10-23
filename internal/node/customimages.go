package node

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

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

// jobs run work of CustomImages off the host lock, one job per image, so
// a copy or a conversion of many gigabytes holds up no other disk or
// machine (docs/decisions/custom-images.md): captures copy a snapshot into
// the base, inspections read what an uploaded file is, conversions write
// it into the base. T is what a job makes.
type jobs[T any] struct {
	// build makes what the job of the image of uid makes. A failure leaves
	// nothing behind; ctx ending, as the agent stopping or the image going,
	// leaves what the next build starts again from.
	build func(ctx context.Context, uid string) (T, error)
	// what names a build in errors, such as "the copy".
	what string
	// timeout bounds one build, so a stalled one fails rather than leaving
	// its image waiting for ever.
	timeout time.Duration
	// slots bounds the builds at once, which other jobs may share.
	slots   *fifo
	running sync.WaitGroup

	mu     sync.Mutex
	ctx    context.Context
	notify func()
	jobs   map[string]*job[T] // by image uid
}

type job[T any] struct {
	running, done bool
	cancel        context.CancelFunc
	made          T
	// err is why the build failed for good, as it is never tried again.
	err error
}

// baseMaker makes CustomImages' bases.
type baseMaker = jobs[driver.ImageBase]

func newJobs[T any](what string, build func(context.Context, string) (T, error), timeout time.Duration, slots *fifo) *jobs[T] {
	return &jobs[T]{what: what, build: build, timeout: timeout, slots: slots, ctx: context.Background(), notify: func() {}, jobs: map[string]*job[T]{}}
}

// newCaptureMaker copies CustomImages' snapshots into their bases. A copy
// that fails, running out of time among it, takes the snapshot with it.
func newCaptureMaker(storage driver.Storage, timeout time.Duration, slots int) *baseMaker {
	return newJobs("the copy", func(ctx context.Context, uid string) (driver.ImageBase, error) {
		base, err := storage.CaptureImageBase(ctx, uid)
		if err != nil && !errors.Is(context.Cause(ctx), errBuildCut) {
			if derr := storage.DeleteCapture(context.WithoutCancel(ctx), uid); derr != nil {
				err = errors.Join(err, fmt.Errorf("and its snapshot stays, for the sweep: %w", derr))
			}
		}
		return base, err
	}, timeout, newFIFO(slots))
}

// errBuildCut is the cause of a build's context ending other than by its
// timeout: the agent stopping or the image going.
var errBuildCut = errors.New("cut short")

// start runs jobs under ctx from now on and calls notify as each finishes.
func (m *jobs[T]) start(ctx context.Context, notify func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ctx, m.notify = ctx, notify
}

// get says whether the job of the CustomImage of uid is done, and what it
// made; until it is, get starts it. A build that failed returns its error
// from then on: the agent never tries it again.
func (m *jobs[T]) get(uid string) (T, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var none T
	j := m.jobs[uid]
	switch {
	case j == nil:
		ctx, cancel := context.WithCancel(m.ctx)
		j = &job[T]{running: true, cancel: cancel}
		m.jobs[uid] = j
		m.running.Add(1)
		go m.run(ctx, j, uid)
		return none, false, nil
	case j.running:
		return none, false, nil
	}
	return j.made, j.done, j.err
}

// done returns what the job of uid made, if it is done, and starts none.
func (m *jobs[T]) done(uid string) (T, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.jobs[uid]; j != nil && j.done {
		return j.made, true
	}
	var none T
	return none, false
}

func (m *jobs[T]) run(ctx context.Context, j *job[T], uid string) {
	defer m.running.Done()
	made, err := m.make(ctx, uid)
	cut := ctx.Err() != nil
	m.mu.Lock()
	j.cancel()
	j.running = false
	switch {
	case err == nil:
		j.done, j.made = true, made
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

// make runs the image's build, a slot at a time, within m.timeout of
// starting; past it, the build fails.
func (m *jobs[T]) make(ctx context.Context, uid string) (T, error) {
	if err := m.slots.acquire(ctx); err != nil {
		var none T
		return none, err
	}
	defer m.slots.release()
	// Ended by ctx only through errBuildCut, so a build tells it from its
	// timeout.
	cuttable, cut := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cut(nil)
	stop := context.AfterFunc(ctx, func() { cut(errBuildCut) })
	defer stop()
	bounded, cancel := context.WithTimeout(cuttable, m.timeout)
	defer cancel()
	made, err := m.build(bounded, uid)
	if err != nil && ctx.Err() == nil && bounded.Err() != nil {
		// A *stuck keeps its reason.
		if s := (*stuck)(nil); errors.As(err, &s) {
			err = blocked(s.reason, fmt.Errorf("%s took longer than %v: %w", m.what, m.timeout, s.err))
		} else {
			err = fmt.Errorf("%s took longer than %v: %w", m.what, m.timeout, err)
		}
	}
	return made, err
}

// keep ends and drops the jobs of images not in wanted, by uid, and
// returns the uids of the jobs left that still run.
func (m *jobs[T]) keep(wanted map[string]bool) map[string]bool {
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
func (m *jobs[T]) wait() { m.running.Wait() }

// customImageController captures the CustomImages that live on this node:
// it snapshots the source disk's volume, reports Snapshotted, copies the
// snapshot into the image's base off the host lock, and reports Ready, or
// the failure, for good. It takes the uploaded ones' files and has them
// converted (reconcileUpload). What a deleted image leaves the sweep
// removes.
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
	if ci.GetSpec().GetSource().GetUpload() != nil {
		return a.reconcileUpload(ctx, ci)
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
	status.UsedSize = usedSize
	return a.writeStatusAgainst(ctx, ci, status, conds...)
}

// writeStatusAgainst writes status, with conds set and every condition at
// ci's generation, unless that is ci's status already. The write is
// conditional on ci's resource version.
func (a *Agent) writeStatusAgainst(ctx context.Context, ci *storagev1alpha1.CustomImage, status *storagev1alpha1.CustomImageStatus, conds ...*metav1.Condition) error {
	gen := ci.GetMetadata().GetGeneration()
	for _, c := range conds {
		c.ObservedGeneration = gen
		status.Conditions = conditions.Set(status.GetConditions(), c, a.cfg.Clock.Now())
	}
	for _, c := range status.GetConditions() {
		c.ObservedGeneration = gen
	}
	status.ObservedGeneration = gen
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

// customImageBase is the key of the base a disk of a custom image is
// cloned from: that of the image it was admitted from (the CustomImageOf
// annotation), while the base is on this node, even once the image is
// gone; otherwise its image's, once Ready on this node.
func (a *Agent) customImageBase(ctx context.Context, d *storagev1alpha1.Disk) (string, error) {
	src := d.GetSpec().GetSource().GetImage()
	// The image the disk was admitted from: its base, kept here while the
	// disk is still to be made, outlives the image.
	if uid := d.GetMetadata().GetAnnotations()[computeapi.CustomImageOf]; uid != "" {
		key := driver.CustomImageBase(uid)
		bases, err := a.cfg.Drivers.Storage.ImageBases(ctx)
		if err != nil {
			return "", err
		}
		if slices.Contains(bases, key) {
			return key, nil
		}
	}
	ci, err := a.getCustomImage(ctx, src.GetProject(), src.GetName())
	switch {
	case err != nil:
		return "", err
	case ci == nil || ci.GetMetadata().GetDeleteTime() != nil:
		return "", blocked(computeapi.ReasonImageNotReady, fmt.Errorf("custom image %s is gone, and its base with it", src.GetName()))
	case d.GetMetadata().GetAnnotations()[computeapi.CustomImageOf] != "" &&
		d.GetMetadata().GetAnnotations()[computeapi.CustomImageOf] != ci.GetMetadata().GetUid():
		return "", blocked(computeapi.ReasonImageNotReady, fmt.Errorf(
			"custom image %s is another image of the name than the disk was made from, whose base is gone", src.GetName()))
	case !computeapi.CustomImageReadyNow(ci):
		return "", blocked(computeapi.ReasonImageNotReady, fmt.Errorf("custom image %s is not Ready", src.GetName()))
	case ci.GetStatus().GetNode() != a.cfg.Name:
		return "", blocked(computeapi.ReasonImageNotReady, fmt.Errorf(
			"custom image %s lives on node %s; disks of it are made only there", src.GetName(), ci.GetStatus().GetNode()))
	}
	return driver.CustomImageBase(ci.GetMetadata().GetUid()), nil
}
