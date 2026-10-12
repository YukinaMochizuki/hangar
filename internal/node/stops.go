package node

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
)

// stops runs Hypervisor.Stop off the host lock, as it waits on the guest
// (docs/decisions/node-agent.md). While a machine's stop runs, nothing else
// touches its VM, disk, interfaces or logs. Guarded by the host lock.
type stops struct {
	// running holds the machines whose stop is under way.
	running map[string]bool
	// ended holds the stops ended since their machine was last reconciled,
	// with how each ended. A reconcile takes what it needs and drops the
	// rest, so a stop's outcome is never read once the host may have moved
	// on from it.
	ended map[string]error
	// failed holds the machines whose last stop failed, until a stop of
	// theirs succeeds or a reconcile of theirs does.
	failed map[string]*stopFailure
	// wg counts the stops under way, for the agent to wait for them as it
	// stops.
	wg sync.WaitGroup
}

// stopFailure is a machine's run of failed stops: the last error, how many
// in a row, and when another may begin.
type stopFailure struct {
	err     error
	n       int
	retryAt time.Time
}

// A stop that failed is begun again after a second, twice as long after
// each failure in a row, up to a minute, however often its machine is
// reconciled meanwhile.
const (
	firstStopRetry = time.Second
	maxStopRetry   = time.Minute
)

func stopRetryDelay(failures int) time.Duration {
	return min(firstStopRetry<<min(failures-1, 16), maxStopRetry)
}

func newStops() *stops {
	return &stops{running: map[string]bool{}, ended: map[string]error{}, failed: map[string]*stopFailure{}}
}

// stopVM has the machine's VM stopped off the host lock, keeping its disk,
// interfaces and logs. It returns nil once a stop has ended well since the
// machine was last reconciled, and the error of one that failed, again
// until the next may begin; otherwise it begins one and says the machine
// waits on it. The caller holds the host lock.
func (a *Agent) stopVM(ctx context.Context, uid string) error {
	if err, ok := a.stops.ended[uid]; ok {
		delete(a.stops.ended, uid)
		if err != nil {
			return blocked(computeapi.ReasonBootFailed, err)
		}
		return nil
	}
	if a.backingOff(uid) {
		return blocked(computeapi.ReasonBootFailed, a.stops.failed[uid].err)
	}
	a.beginStop(ctx, uid)
	return stopping(uid)
}

// backingOff reports a machine whose last stop failed too recently to begin
// another. The caller holds the host lock.
func (a *Agent) backingOff(uid string) bool {
	f := a.stops.failed[uid]
	return f != nil && a.cfg.Clock.Now().Before(f.retryAt)
}

func stopping(uid string) error {
	return fmt.Errorf("%w: machine %s's VM stopping", controller.ErrWaiting, uid)
}

// beginStop stops the machine's VM in the background, unless a stop of it
// is under way. When it succeeds, every machine is checked again; a failure
// is left to the machine's own retry, so a stop that keeps failing costs the
// others nothing. The caller holds the host lock.
func (a *Agent) beginStop(ctx context.Context, uid string) {
	// Unreachable while reconciles and the sweep pass by machines being
	// stopped; kept as a safety net against a second stop.
	if a.stops.running[uid] {
		return
	}
	a.stops.running[uid] = true
	a.stops.wg.Add(1)
	go func() {
		defer a.stops.wg.Done()
		err := a.cfg.Drivers.Hypervisor.Stop(ctx, uid)
		if err != nil && ctx.Err() == nil {
			a.cfg.Log.Warn("stopping a VM failed", "machine_uid", uid, "error", err)
		}
		a.host.Lock()
		delete(a.stops.running, uid)
		a.stops.ended[uid] = err
		if err == nil {
			delete(a.stops.failed, uid)
		} else {
			f := a.stops.failed[uid]
			if f == nil {
				f = &stopFailure{}
				a.stops.failed[uid] = f
			}
			f.err, f.n = err, f.n+1
			f.retryAt = a.cfg.Clock.Now().Add(stopRetryDelay(f.n))
		}
		a.host.Unlock()
		if err == nil {
			a.recheck()
		}
	}()
}

// recheck has every machine checked again, as soon as the worker is free.
func (a *Agent) recheck() {
	select {
	case a.trigger <- struct{}{}:
	default:
	}
}
