//go:build testbed

package network

import (
	"runtime"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
)

// changed fails t unless the driver signals a change within five seconds.
func changed(t *testing.T, d *hostnet.Driver, what string) {
	t.Helper()
	select {
	case <-d.Changes():
	case <-time.After(5 * time.Second):
		t.Errorf("%s was never signalled", what)
	}
}

// quiet fails t if the driver signals a change within a second.
func quiet(t *testing.T, d *hostnet.Driver, what string) {
	t.Helper()
	select {
	case <-d.Changes():
		t.Errorf("%s was signalled as a change", what)
	case <-time.After(time.Second):
	}
}

// TestWatchRecovers breaks the driver's subscriptions as a failing socket
// does: that is signalled, the next ensure watches again, and drift after
// it is signalled.
func TestWatchRecovers(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink})
	nets := []driver.NetworkSpec{netA}
	ensure(t, d, nets)
	ns := hostnet.NamespaceOf(netA.UID)
	for _, c := range []struct {
		name, uid string
		breakIt   func(*hostnet.Driver, string) bool
		drift     []string
	}{
		{"a network's namespace", netA.UID, hostnet.BreakWatch, []string{"ip", "-n", ns, "link", "del", "hn-br"}},
		{"the root namespace", "", hostnet.BreakWatch, []string{"nft", "flush", "chain", "ip", hostnet.Table, "postrouting"}},
		{"the nft monitor alone", netA.UID, hostnet.BreakNftWatch,
			[]string{"ip", "netns", "exec", ns, "nft", "flush", "chain", "ip", hostnet.Table, "postrouting"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			settle(d)
			if !c.breakIt(d, c.uid) {
				t.Fatal("no watch to break")
			}
			changed(t, d, "a watch breaking")
			ensure(t, d, nets)
			settle(d)
			run(t, c.drift...)
			changed(t, d, "drift after the watch broke")
			ensure(t, d, nets)
		})
	}
}

// TestFailedSubscriptions makes each subscription of a network's watch fail
// to start: every ensure fails until it starts, without leaving goroutines
// behind, and then drift is signalled.
func TestFailedSubscriptions(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink})
	nets := []driver.NetworkSpec{netA}
	ensure(t, d, nets)
	for _, what := range []string{"links", "addrs", "routes", "neighs"} {
		t.Run(what, func(t *testing.T) {
			if err := hostnet.FailSubscriptions(what); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = hostnet.FailSubscriptions() }()
			hostnet.BreakWatch(d, netA.UID)
			settle(d)
			before := runtime.NumGoroutine()
			for range 20 {
				if err := d.EnsureNetwork(t.Context(), netA); err == nil {
					t.Fatal("ensured a network whose watch did not start")
				}
			}
			deadline := time.Now().Add(5 * time.Second)
			for n := runtime.NumGoroutine(); n > before+2; n = runtime.NumGoroutine() {
				if time.Now().After(deadline) {
					t.Errorf("%d goroutines after 20 failed subscriptions, %d before", n, before)
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			_ = hostnet.FailSubscriptions()
			ensure(t, d, nets)
			settle(d)
			run(t, "ip", "-n", hostnet.NamespaceOf(netA.UID), "link", "del", "hn-br")
			changed(t, d, "drift once the watch started")
			ensure(t, d, nets)
		})
	}
}

// TestStopIsQuiet stops the driver: its watches ending is no change.
func TestStopIsQuiet(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink})
	ensure(t, d, []driver.NetworkSpec{netA, netB})
	settle(d)
	_ = d.Close()
	quiet(t, d, "the driver closing")
}
