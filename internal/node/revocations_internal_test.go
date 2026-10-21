package node

import (
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// TestARemovedClientLingersBounded: a removed client lingers while its
// server's running boot holds it, at most revokeBound and never again
// until held anew, and revokeGrace once that boot takes an export without it.
func TestARemovedClientLingersBounded(t *testing.T) {
	k := controller.Key{Project: "p", Name: "default"}
	net := netip.MustParsePrefix("10.0.0.0/24")
	web, reader, other := netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("10.0.0.6"), netip.MustParseAddr("10.1.0.6")
	owners := map[netip.Addr]string{web: "w/eth0", reader: "r/eth0"}
	boot := time.Unix(1000, 0)
	now := time.Unix(2000, 0)
	export := func(addrs ...netip.Addr) driver.ShareExport {
		var e driver.ShareExport
		for _, a := range addrs {
			e.Clients = append(e.Clients, driver.ShareClient{Address: a})
		}
		return e
	}
	var scheduled []time.Duration
	schedule := func(got controller.Key, d time.Duration) {
		if got != k {
			t.Errorf("scheduled %v, want %v", got, k)
		}
		scheduled = append(scheduled, d)
	}
	var r *revocations
	fresh := func() {
		r = newRevocations()
		r.on("srv", []controller.Key{k}, owners)
		scheduled = nil
	}
	lingering := func(boot time.Time, want ...netip.Addr) []netip.Addr {
		return r.lingering(k, "srv", boot, net, want, owners, now, schedule)
	}
	is := func(what string, got []netip.Addr, want ...netip.Addr) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s: %v linger, want %v", what, got, want)
		}
	}

	fresh()
	r.took("srv", boot, export(web, reader, other), now)
	is("none removed", lingering(boot, web, reader))
	is("reader removed", lingering(boot, web), reader)
	if !slices.Equal(scheduled, []time.Duration{revokeBound}) {
		t.Errorf("scheduled %v, want the bound", scheduled)
	}
	is("the server not running", lingering(time.Time{}, web))
	is("another boot", lingering(boot.Add(time.Second), web))
	now = now.Add(revokeBound - time.Millisecond)
	is("before the bound", lingering(boot, web), reader)
	now = now.Add(time.Millisecond)
	is("at the bound", lingering(boot, web))
	// The server never takes it: cut for good, however often looked at.
	for range 3 {
		now = now.Add(revokeBound)
		is("past the bound, still held", lingering(boot, web))
	}
	if len(scheduled) != 1 {
		t.Errorf("scheduled %v: a cut past its bound armed again", scheduled)
	}
	r.took("srv", boot, export(web), now)
	is("an export without it taken past its bound: no grace", lingering(boot, web))
	// Held anew, then removed again: a bound of its own.
	r.took("srv", boot, export(web, reader), now)
	is("reader back", lingering(boot, web, reader))
	is("reader removed again", lingering(boot, web), reader)
	if len(scheduled) != 2 {
		t.Errorf("scheduled %v, want a bound of its own", scheduled)
	}
	if keys := r.took("srv", boot, export(web), now); !slices.Equal(keys, []controller.Key{k}) {
		t.Errorf("an export without it taken asks for %v, want the network", keys)
	}
	now = now.Add(revokeGrace - time.Millisecond)
	is("the export without it taken, in the grace", lingering(boot, web), reader)
	now = now.Add(time.Millisecond)
	is("after the grace", lingering(boot, web))

	// Taken before the firewall saw the removal; held again and taken
	// without again, soon after: a grace each time.
	fresh()
	r.took("srv", boot, export(web, reader), now)
	r.took("srv", boot, export(web), now)
	is("taken first, in the grace", lingering(boot, web), reader)
	now = now.Add(revokeGrace - time.Second)
	r.took("srv", boot, export(web, reader), now)
	r.took("srv", boot, export(web), now)
	now = now.Add(2 * time.Second)
	is("removed again soon after, in a grace of its own", lingering(boot, web), reader)
	if len(scheduled) != 0 {
		t.Errorf("scheduled %v at a bound, the export taken", scheduled)
	}

	// A new boot took it without, the old one holding it: nothing does.
	fresh()
	r.took("srv", boot, export(web, reader), now)
	if keys := r.took("srv", boot.Add(time.Second), export(web), now); keys != nil {
		t.Errorf("a new boot's export asks for %v", keys)
	}
	is("a new boot", lingering(boot.Add(time.Second), web))

	// Its address now another interface's: cut, and no new bound.
	fresh()
	r.took("srv", boot, export(web, reader), now)
	is("removed", lingering(boot, web), reader)
	owners[reader] = "new/eth0"
	is("its address taken by another", lingering(boot, web))
	if len(scheduled) != 1 {
		t.Errorf("scheduled %v: a reused address armed a cut", scheduled)
	}
	owners[reader] = "r/eth0"

	// Refused, also in the grace of an export taken.
	fresh()
	r.took("srv", boot, export(web, reader), now)
	is("removed", lingering(boot, web), reader)
	if keys := r.refused("srv"); !slices.Equal(keys, []controller.Key{k}) {
		t.Errorf("a refusal asks for %v, want the network", keys)
	}
	is("refused", lingering(boot, web))
	r.took("srv", boot, export(web, reader), now)
	r.took("srv", boot, export(web), now)
	r.refused("srv")
	is("refused in the grace", lingering(boot, web))
	r.forget("srv")
	if len(r.nets) != 0 || len(r.owners) != 0 {
		t.Errorf("forgotten, it keeps %v and %v", r.nets, r.owners)
	}
}

// TestACutIsLookedAtAgain: a network whose cut is due is queued again then.
func TestACutIsLookedAtAgain(t *testing.T) {
	clock := clockwork.NewFakeClock()
	var queued []controller.Key
	var mu sync.Mutex
	a := &Agent{cfg: Config{Clock: clock}, requeueNetworks: func(keys ...controller.Key) {
		mu.Lock()
		defer mu.Unlock()
		queued = append(queued, keys...)
	}}
	k := controller.Key{Project: "p", Name: "default"}
	a.scheduleNetwork(k, revokeBound)
	clock.Advance(revokeBound - time.Millisecond)
	mu.Lock()
	if len(queued) != 0 {
		t.Errorf("queued %v before its cut", queued)
	}
	mu.Unlock()
	clock.Advance(time.Millisecond)
	testutil.Eventually(t, "the network queued at its cut", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Equal(queued, []controller.Key{k})
	})
}
