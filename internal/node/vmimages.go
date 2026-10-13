package node

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// imageMaker makes the bases of disk images off the host lock, one job per
// digest however many disks want it, so a slow download holds up no other
// disk or machine (docs/decisions/vm-images.md).
type imageMaker struct {
	images  driver.DiskImages
	storage driver.Storage
	clock   clockwork.Clock
	// timeout bounds fetching an image and then writing its base, so a
	// stalled job fails and is tried again.
	timeout time.Duration
	// slots bounds the bases written at once, as the puller's do unpacks.
	slots   *fifo
	running sync.WaitGroup

	mu     sync.Mutex
	ctx    context.Context
	notify func()
	jobs   map[string]*imageJob // by digest
}

type imageJob struct {
	running, ready bool
	cancel         context.CancelFunc
	// url is what the job fetches, and generation the Image's when it was
	// asked for: an image found at fault is fetched again only for another
	// generation.
	url        string
	generation int64
	// size is the disk the image holds, once made.
	size     int64
	err      error
	failures int
	retryAt  time.Time
}

func newImageMaker(images driver.DiskImages, storage driver.Storage, clock clockwork.Clock, timeout time.Duration, slots int) *imageMaker {
	return &imageMaker{
		images: images, storage: storage, clock: clock, timeout: timeout, slots: newFIFO(slots),
		ctx: context.Background(), notify: func() {}, jobs: map[string]*imageJob{},
	}
}

// start runs jobs under ctx from now on and calls notify as each finishes.
func (m *imageMaker) start(ctx context.Context, notify func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ctx, m.notify = ctx, notify
}

// get says whether the base of the disk image digest, at url, is made, and
// the size of the disk it holds if this agent made it. Until it is made get
// starts making it. A failure is returned until the next attempt is due;
// one of the image's own (isImageFault) until the Image's generation
// changes.
func (m *imageMaker) get(url, digest string, generation int64) (bool, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[digest]
	if j != nil && !j.running && (j.url != url || j.generation != generation) && !j.ready {
		j = nil
	}
	if j == nil {
		j = &imageJob{url: url, generation: generation}
		m.jobs[digest] = j
	}
	switch {
	case j.running:
		return false, 0, nil
	case j.ready:
		return true, j.size, nil
	case j.err != nil && isImageFault(j.err):
		return false, 0, j.err
	case j.err != nil && m.clock.Now().Before(j.retryAt):
		return false, 0, j.err
	}
	ctx, cancel := context.WithCancel(m.ctx)
	j.running, j.err, j.cancel = true, nil, cancel
	m.running.Add(1)
	go m.run(ctx, j, digest)
	return false, 0, nil
}

// isImageFault says the image itself is at fault: fetching it again never
// helps.
func isImageFault(err error) bool {
	return errors.Is(err, driver.ErrDigestMismatch) || errors.Is(err, driver.ErrInvalidImage) || errors.Is(err, driver.ErrImageTooLarge)
}

// faultReason is the Image's Verified reason for an image at fault.
func faultReason(err error) string {
	switch {
	case errors.Is(err, driver.ErrDigestMismatch):
		return computeapi.ReasonHashMismatch
	case errors.Is(err, driver.ErrImageTooLarge):
		return computeapi.ReasonImageTooLarge
	}
	return computeapi.ReasonInvalidImage
}

func (m *imageMaker) run(ctx context.Context, j *imageJob, digest string) {
	defer m.running.Done()
	size, err := m.make(ctx, j.url, digest)
	m.mu.Lock()
	j.cancel()
	j.running, j.cancel = false, nil
	if err == nil {
		j.ready, j.size, j.err, j.failures = true, size, nil, 0
	} else {
		j.err = err
		j.retryAt = m.clock.Now().Add(min(time.Second<<min(j.failures, 6), maxRetryDelay))
		j.failures++
	}
	notify := m.notify
	m.mu.Unlock()
	notify()
}

// make fetches the image and writes its base, a slot at a time.
func (m *imageMaker) make(ctx context.Context, url, digest string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	img, err := m.images.Fetch(ctx, url, digest)
	if err != nil {
		return 0, err
	}
	defer func() { _ = img.Close() }()
	if err := m.slots.acquire(ctx); err != nil {
		return 0, err
	}
	defer m.slots.release()
	if err := m.storage.EnsureImageBase(ctx, digest, img.Size(), func(dev string) error { return img.WriteTo(ctx, dev) }); err != nil {
		return 0, err
	}
	return img.Size(), nil
}

// madeSize is the size of the disk the image digest holds, if a job of
// this agent's made its base; zero if not.
func (m *imageMaker) madeSize(digest string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.jobs[digest]; j != nil && j.ready {
		return j.size
	}
	return 0
}

// wait returns once every job has ended.
func (m *imageMaker) wait() { m.running.Wait() }

// keep drops the jobs of digests not in wanted, ending those still running,
// and returns the digests of the jobs left that still run: their bases are
// being made.
func (m *imageMaker) keep(wanted map[string]bool) map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	making := map[string]bool{}
	for digest, j := range m.jobs {
		if !wanted[digest] {
			if j.running {
				j.cancel()
			}
			delete(m.jobs, digest)
			continue
		}
		if j.running {
			making[digest] = true
		}
	}
	return making
}

// forget drops the finished job of a digest whose base is gone, so asking
// for the image again makes the base again.
func (m *imageMaker) forget(digest string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.jobs[digest]; j != nil && !j.running {
		delete(m.jobs, digest)
	}
}
