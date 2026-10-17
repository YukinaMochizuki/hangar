package node

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jonboulle/clockwork"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/natlog"
)

// natSource is a network driver whose FollowNAT runs follow, once a call;
// it has nothing else.
type natSource struct {
	driver.Network
	calls  int
	follow func(ctx context.Context, call int, h driver.NATHandler) error
}

func (s *natSource) FollowNAT(ctx context.Context, h driver.NATHandler) error {
	s.calls++
	return s.follow(ctx, s.calls, h)
}

func flowAt(at time.Time, port uint16) driver.NATFlow {
	return driver.NATFlow{
		Time: at, Protocol: unix.IPPROTO_TCP,
		Public:      netip.AddrPortFrom(netip.MustParseAddr("192.0.2.1"), port),
		Transit:     netip.MustParseAddrPort("100.64.0.1:40000"),
		Destination: netip.MustParseAddrPort("198.51.100.7:25"),
		NetworkUID:  "net-uid", Inner: netip.MustParseAddrPort("10.0.0.2:51000"),
	}
}

// TestTheNATLogLeavesNoWindowUnsaid: the gap since the log's last record
// ends only when following has started, as does the one after following
// failed, from the last thing accounted for; connections between are named
// by the network services' names, and a window the driver lost is logged.
func TestTheNATLogLeavesNoWindowUnsaid(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		// A log a node before wrote, stopped an hour ago.
		before := time.Now().Add(-time.Hour)
		l, err := natlog.Open(dir, natlog.Options{Now: func() time.Time { return before }})
		if err != nil {
			t.Fatal(err)
		}
		_ = l.Close("an earlier node")

		var started [2]time.Time
		var lostTo, failedAt time.Time
		src := &natSource{follow: func(ctx context.Context, call int, h driver.NATHandler) error {
			// Subscribing takes a while; nothing before Started is covered.
			time.Sleep(time.Second) //nolint:forbidigo // virtual time
			started[call-1] = time.Now()
			h.Started(started[call-1])
			if call > 1 {
				<-ctx.Done()
				return nil
			}
			h.Flow(flowAt(time.Now(), 40001))
			time.Sleep(time.Second) //nolint:forbidigo // virtual time
			lostTo = time.Now()
			h.Lost(driver.NATLost{From: started[0], To: lostTo, Reason: "overflowed"})
			time.Sleep(time.Second) //nolint:forbidigo // virtual time
			failedAt = time.Now()
			return errors.New("socket gone")
		}}
		s := &netServices{nat: natlog.NewNames()}
		s.nat.Set("net-uid", natlog.Network{Project: "p", Name: "default", Machines: map[netip.Addr]natlog.Machine{
			netip.MustParseAddr("10.0.0.2"): {Name: "web", UID: "web-uid"},
		}})
		a := &Agent{
			cfg: Config{
				Drivers: &Drivers{Network: src}, NATLog: dir,
				Clock: clockwork.NewRealClock(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			},
			netsvc: s,
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { a.followNAT(ctx); close(done) }()
		time.Sleep(10 * time.Second) //nolint:forbidigo // virtual time
		synctest.Wait()
		cancel()
		<-done
		if src.calls != 2 {
			t.Fatalf("FollowNAT called %d times, want again after it failed", src.calls)
		}

		recs, err := natlog.Lookup(dir, natlog.Query{Port: 40001, From: before.Add(-time.Minute), To: time.Now().Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		var gaps []natlog.Record
		var named int
		for _, r := range recs {
			switch r.Event {
			case natlog.EventGap:
				gaps = append(gaps, r)
			case natlog.EventNew:
				named++
				if r.Machine != "web" || r.Project != "p" {
					t.Errorf("the connection is named %+v, want web of p", r)
				}
			}
		}
		if named != 1 {
			t.Errorf("%d connections logged, want 1", named)
		}
		if len(gaps) != 3 {
			t.Fatalf("gaps %+v, want the start, the loss and the failure", gaps)
		}
		if g := gaps[0]; g.From == nil || !g.From.Equal(before) || !g.To.Equal(started[0]) {
			t.Errorf("the start's gap is %+v, want from the earlier stop, %s, to when following started, %s", g, before, started[0])
		}
		if g := gaps[1]; !g.From.Equal(started[0]) || !g.To.Equal(lostTo) || g.Reason != "overflowed" {
			t.Errorf("the loss is logged as %+v", g)
		}
		// From the loss's end, the last thing accounted for, to when
		// following started again, a retry and a subscription later.
		if g := gaps[2]; g.From == nil || !g.From.Equal(lostTo) || !g.To.Equal(started[1]) || !started[1].After(failedAt.Add(natRetry)) {
			t.Errorf("the failure's gap is %+v, want from %s to when following started again, %s", g, lostTo, started[1])
		}
	})
}
