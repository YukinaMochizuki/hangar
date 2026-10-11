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
// (docs/decisions/node-agent.md).
type puller struct {
	images  driver.Images
	storage driver.Storage
	clock   clockwork.Clock
	// timeout bounds one job, so a stalled pull fails and is tried again.
	timeout time.Duration
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
	// digest is known once the reference is resolved, before the base is
	// made, so the sweep spares the base while it is being made.
	digest string
	config driver.ImageConfig
	err    error
	// failures counts the failed attempts in a row; the next waits until
	// retryAt.
	failures int
	retryAt  time.Time
}

func newPuller(images driver.Images, storage driver.Storage, clock clockwork.Clock, timeout time.Duration) *puller {
	return &puller{
		images: images, storage: storage, clock: clock, timeout: timeout,
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

// get returns the image ref names, pinned to digest unless digest is empty,
// with its base made, for the machine uid. Until it is ready get says so and
// prepares it; a failure is returned until the next attempt is due.
func (p *puller) get(uid, ref, digest string) (string, driver.ImageConfig, bool, error) {
	key := ref + "@" + digest
	p.mu.Lock()
	defer p.mu.Unlock()
	p.wants[uid] = key
	j := p.jobs[key]
	if j == nil {
		j = &pull{digest: digest}
		p.jobs[key] = j
	}
	switch {
	case j.running:
		return "", driver.ImageConfig{}, false, nil
	case j.ready:
		return j.digest, j.config, true, nil
	case j.err != nil && p.clock.Now().Before(j.retryAt):
		return "", driver.ImageConfig{}, false, j.err
	}
	j.running, j.err = true, nil
	p.running.Add(1)
	go p.run(p.ctx, key, ref, digest)
	return "", driver.ImageConfig{}, false, nil
}

// wait returns once every job has ended.
func (p *puller) wait() {
	p.running.Wait()
}

// run prepares one image. Its failure says which step failed, as a stuck
// error: the registry's, or the disk's.
func (p *puller) run(ctx context.Context, key, ref, digest string) {
	defer p.running.Done()
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	err := p.prepare(ctx, key, ref, &digest)
	p.mu.Lock()
	j := p.jobs[key]
	j.running = false
	now := p.clock.Now()
	if err != nil {
		j.err = err
		j.retryAt = now.Add(min(time.Second<<min(j.failures, 6), time.Minute))
		j.failures++
	} else {
		j.err, j.failures, j.ready = nil, 0, true
	}
	notify := p.notify
	p.mu.Unlock()
	notify()
}

func (p *puller) prepare(ctx context.Context, key, ref string, digest *string) error {
	var err error
	if *digest == "" {
		if *digest, err = p.images.Resolve(ctx, ref); err != nil {
			return blocked(computeapi.ReasonImagePullFailed, err)
		}
	}
	p.mu.Lock()
	p.jobs[key].digest = *digest
	p.mu.Unlock()
	config, err := p.images.Config(ctx, ref, *digest)
	if err != nil {
		return blocked(computeapi.ReasonImagePullFailed, err)
	}
	p.mu.Lock()
	p.jobs[key].config = config
	p.mu.Unlock()
	err = p.storage.EnsureBase(ctx, *digest, func(dir string) error {
		err := p.images.Unpack(ctx, ref, *digest, dir)
		switch {
		case errors.Is(err, driver.ErrNoSpace):
			return blocked(computeapi.ReasonDiskFailed, err)
		case err != nil:
			return blocked(computeapi.ReasonImagePullFailed, err)
		}
		return nil
	})
	var s *stuck
	if err != nil && !errors.As(err, &s) {
		return blocked(computeapi.ReasonDiskFailed, err)
	}
	return err
}

// drop forgets what the machine uid waited on.
func (p *puller) drop(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.wants, uid)
}

// keep forgets the machines not in mine and the finished jobs no machine
// waits on, and returns the digests whose bases the remaining jobs need:
// those being made, and those of machines not built yet.
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
		if !wanted[key] && !j.running {
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
