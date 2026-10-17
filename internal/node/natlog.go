package node

import (
	"context"
	"sync"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/natlog"
)

// natRetry is how long the agent waits to follow the host's connections
// again after following failed.
const natRetry = 5 * time.Second

// natMaintain is how often the NAT log's past days are compressed, and its
// expired and, past its cap, oldest ones removed.
const natMaintain = time.Minute

// followNAT logs every new IPv4 connection out of the host's networks, if
// the network driver can tell them and the agent has a NAT log, until ctx
// is done. A window it may have missed connections in is logged as a gap,
// closed only once following has started again.
func (a *Agent) followNAT(ctx context.Context) {
	src, ok := a.cfg.Drivers.Network.(driver.NATFlows)
	if !ok || a.cfg.NATLog == "" {
		return
	}
	var l *natlog.Log
	for {
		var err error
		if l, err = natlog.Open(a.cfg.NATLog, natlog.Options{
			Now: a.cfg.Clock.Now,
			Failing: func(failing bool, err error) {
				if failing {
					a.cfg.Log.Error("writing the NAT log failed; it is logged as a gap once it works", "dir", a.cfg.NATLog, "error", err)
				} else {
					a.cfg.Log.Info("writing the NAT log works again", "dir", a.cfg.NATLog)
				}
			},
			Warn: func(msg string) { a.cfg.Log.Warn("the NAT log: "+msg, "dir", a.cfg.NATLog) },
		}); err == nil {
			break
		}
		a.cfg.Log.Error("opening the NAT log failed", "dir", a.cfg.NATLog, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-a.cfg.Clock.After(natRetry):
		}
	}
	// Write's failures are told through Failing, once.
	write := func(r natlog.Record) { _ = l.Write(r) }
	defer func() { _ = l.Close("the node agent stopped") }()
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		t := a.cfg.Clock.NewTicker(natMaintain)
		defer t.Stop()
		for {
			if err := l.Maintain(); err != nil {
				a.cfg.Log.Error("maintaining the NAT log failed", "error", err)
			}
			if err := l.Trim(); err != nil {
				a.cfg.Log.Error("trimming the NAT log failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.Chan():
			}
		}
	})
	// The open window: from what was last accounted for, until following
	// starts.
	from, reason := l.Last(), "the node was not logging connections"
	if from.IsZero() {
		reason = "the log starts: no earlier connection was logged"
	}
	for {
		last := time.Time{}
		h := driver.NATHandler{
			Started: func(at time.Time) {
				to := at.UTC()
				gap := natlog.Record{Time: to, Event: natlog.EventGap, To: &to, Reason: reason}
				if !from.IsZero() {
					f := from.UTC()
					gap.From = &f
				}
				write(gap)
				last = at
			},
			Flow: func(f driver.NATFlow) {
				last = f.Time
				write(a.netsvc.nat.Record(f))
			},
			Lost: func(g driver.NATLost) {
				f, t := g.From.UTC(), g.To.UTC()
				write(natlog.Record{Time: t, Event: natlog.EventGap, From: &f, To: &t, Reason: g.Reason})
				last = g.To
			},
		}
		err := src.FollowNAT(ctx, h)
		if ctx.Err() != nil {
			return
		}
		a.cfg.Log.Error("following the host's connections failed", "error", err)
		if !last.IsZero() {
			from = last
		}
		reason = "following the host's connections failed"
		if err != nil {
			reason += ": " + err.Error()
		}
		select {
		case <-ctx.Done():
			return
		case <-a.cfg.Clock.After(natRetry):
		}
	}
}
