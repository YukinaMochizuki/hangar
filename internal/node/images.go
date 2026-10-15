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

// puller prepares images off the host lock, one job per reference however
// many machines want it, so a slow image holds up no other machine
// (docs/decisions/node-agent.md). A machine with credentials that has not
// proven its image yet has a job of its own, which resolves the reference
// with its credentials; the Images driver serves cached content only to the
// repository it came from (docs/decisions/secrets.md).
type puller struct {
	images  driver.Images
	storage driver.Storage
	clock   clockwork.Clock
	// timeout bounds fetching an image, and then unpacking it, so a stalled
	// job fails and is tried again.
	timeout time.Duration
	// slots bounds the images unpacked at once, the host's disks' and CPUs'
	// work, served in turn. Fetching takes none: a registry that stalls
	// holds up no other image.
	slots *fifo
	// running counts the jobs, for the agent to wait for them as it stops.
	running sync.WaitGroup

	mu sync.Mutex
	// ctx and notify are set when the agent runs: jobs run under ctx, and
	// notify tells the agent a job has finished.
	ctx    context.Context
	notify func()
	jobs   map[string]*pull  // by reference, and digest when pinned
	wants  map[string]string // machine uid to the job it waits on
}

type pull struct {
	running, ready bool
	// unpacking says the job holds a slot and is making the base; cancel
	// ends the job while it runs.
	unpacking bool
	cancel    context.CancelFunc
	// pinned says the reference names its digest, which never changes.
	pinned bool
	// creds are what the job pulls with: the logins of the machine it is
	// its own for, or of the machine that last started it.
	creds driver.Credentials
	// digest is known once the reference is resolved, before the base is
	// made, so the sweep spares the base while it is being made.
	digest string
	config driver.ImageConfig
	err    error
	// failures counts the failed attempts in a row, and timeouts those that
	// timed out; the next waits until retryAt.
	failures, timeouts int
	retryAt            time.Time
	// bad is a digest found at fault (too large, or invalid), with its
	// error: it is never unpacked again while the job lasts, but a tag is
	// resolved again, as it may come to name a fixed image.
	bad    string
	badErr error
}

// Backoff of a job that failed: a failure that may pass at once is tried
// again within a minute; a timeout, which held a registry or a slot for
// long, from a minute, twice as long each time, up to the timeout.
const (
	maxRetryDelay   = time.Minute
	firstTimeoutGap = time.Minute
)

func newPuller(images driver.Images, storage driver.Storage, clock clockwork.Clock, timeout time.Duration, slots int) *puller {
	return &puller{
		images: images, storage: storage, clock: clock, timeout: timeout, slots: newFIFO(slots),
		ctx: context.Background(), notify: func() {},
		jobs: map[string]*pull{}, wants: map[string]string{},
	}
}

// start runs jobs under ctx from now on and calls notify as each finishes.
func (p *puller) start(ctx context.Context, notify func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ctx, p.notify = ctx, notify
}

// get returns ref's image for machine uid, pinned to digest (one its status
// proves) unless empty, with its base made, pulling with creds; own gives a
// machine with credentials a job of its own (docs/decisions/secrets.md).
func (p *puller) get(uid, ref, digest string, creds driver.Credentials, own bool) (string, driver.ImageConfig, bool, error) {
	key := ref + "@" + digest
	if own && digest == "" {
		key += "#" + uid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.wants[uid] = key
	j := p.jobs[key]
	if j == nil {
		j = &pull{digest: digest, pinned: digest != ""}
		p.jobs[key] = j
	}
	switch {
	case j.running:
		return "", driver.ImageConfig{}, false, nil
	case j.ready:
		return j.digest, j.config, true, nil
	case j.err != nil && j.pinned && j.bad != "":
		return "", driver.ImageConfig{}, false, j.err
	case j.err != nil && p.clock.Now().Before(j.retryAt):
		return "", driver.ImageConfig{}, false, j.err
	}
	ctx, cancel := context.WithCancel(p.ctx)
	j.running, j.err, j.cancel, j.creds = true, nil, cancel, creds
	p.running.Add(1)
	go p.run(ctx, j, ref)
	return "", driver.ImageConfig{}, false, nil
}

// wait returns once every job has ended.
func (p *puller) wait() {
	p.running.Wait()
}

// run prepares one image. Its failure says which step failed, as a stuck
// error: the registry's, or the disk's.
func (p *puller) run(ctx context.Context, j *pull, ref string) {
	defer p.running.Done()
	err := p.prepare(ctx, j, ref)
	p.mu.Lock()
	j.cancel()
	j.running, j.unpacking, j.cancel = false, false, nil
	now := p.clock.Now()
	switch {
	case err == nil:
		j.err, j.failures, j.timeouts, j.ready = nil, 0, 0, true
	case isFault(err):
		j.err, j.bad, j.badErr = err, j.digest, err
		j.retryAt = now.Add(min(time.Second<<min(j.failures, 6), maxRetryDelay))
		j.failures, j.timeouts = j.failures+1, 0
	case timedOut(err):
		j.err = err
		j.retryAt = now.Add(min(firstTimeoutGap<<min(j.timeouts, 10), p.timeout))
		j.failures, j.timeouts = j.failures+1, j.timeouts+1
	default:
		j.err = err
		j.retryAt = now.Add(min(time.Second<<min(j.failures, 6), maxRetryDelay))
		j.failures, j.timeouts = j.failures+1, 0
	}
	notify := p.notify
	p.mu.Unlock()
	notify()
}

// isFault says the image itself is at fault: trying it again never helps.
func isFault(err error) bool {
	return errors.Is(err, driver.ErrImageTooLarge) || errors.Is(err, driver.ErrInvalidImage)
}

// timedOut says a job ran out of time, its own or a registry's.
func timedOut(err error) bool {
	var t interface{ Timeout() bool }
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &t) && t.Timeout()
}

func (p *puller) prepare(ctx context.Context, j *pull, ref string) error {
	p.mu.Lock()
	creds := j.creds
	p.mu.Unlock()
	digest, config, err := p.fetch(ctx, j, ref, creds)
	if err != nil {
		return err
	}
	// The registry is done with: what is left is the host's work, a slot
	// at a time.
	if err := p.slots.acquire(ctx); err != nil {
		return err
	}
	defer p.slots.release()
	p.mu.Lock()
	j.unpacking = true
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	err = p.storage.EnsureBase(ctx, digest, config.Size, func(dir string) error {
		err := p.images.Unpack(ctx, ref, digest, dir, creds)
		switch {
		case errors.Is(err, driver.ErrNoSpace):
			return blocked(computeapi.ReasonDiskFailed, err)
		case err != nil:
			return pullFailed(err)
		}
		return nil
	})
	var s *stuck
	if err != nil && !errors.As(err, &s) {
		return blocked(computeapi.ReasonDiskFailed, err)
	}
	return err
}

// pullFailed says a pull failed, and whether the registry refused it.
func pullFailed(err error) error {
	if errors.Is(err, driver.ErrUnauthorized) {
		return blocked(computeapi.ReasonImagePullUnauthorized, err)
	}
	return blocked(computeapi.ReasonImagePullFailed, err)
}

// fetch resolves the reference and fetches the image into the cache, so
// unpacking it needs no registry.
func (p *puller) fetch(ctx context.Context, j *pull, ref string, creds driver.Credentials) (string, driver.ImageConfig, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	p.mu.Lock()
	digest := j.digest
	pinned := j.pinned
	p.mu.Unlock()
	var err error
	if !pinned {
		if digest, err = p.images.Resolve(ctx, ref, creds); err != nil {
			return "", driver.ImageConfig{}, pullFailed(err)
		}
	}
	p.mu.Lock()
	j.digest = digest
	bad, badErr := j.bad, j.badErr
	p.mu.Unlock()
	if digest == bad {
		return "", driver.ImageConfig{}, badErr
	}
	config, err := p.images.Config(ctx, ref, digest, creds)
	if err != nil {
		return "", driver.ImageConfig{}, pullFailed(err)
	}
	p.mu.Lock()
	j.config = config
	p.mu.Unlock()
	return digest, config, nil
}

// drop forgets what the machine uid waited on.
func (p *puller) drop(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.wants, uid)
}

// keep forgets the machines not in mine and the jobs no machine waits on,
// ending those still fetching or waiting for a slot, and returns the
// digests whose bases the remaining jobs need: those being made, and those
// of machines not built yet.
func (p *puller) keep(mine map[string]bool) map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	wanted := map[string]bool{}
	for uid, key := range p.wants {
		if !mine[uid] {
			delete(p.wants, uid)
			continue
		}
		wanted[key] = true
	}
	digests := map[string]bool{}
	for key, j := range p.jobs {
		if !wanted[key] && (!j.running || !j.unpacking) {
			if j.running {
				j.cancel()
			}
			delete(p.jobs, key)
			continue
		}
		if j.digest != "" {
			digests[j.digest] = true
		}
	}
	return digests
}

// forget drops the finished jobs of a digest whose base is gone, so asking
// for the image again makes the base again.
func (p *puller) forget(digest string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, j := range p.jobs {
		if j.digest == digest && !j.running {
			delete(p.jobs, key)
		}
	}
}
