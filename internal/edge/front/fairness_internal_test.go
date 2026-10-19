package front

import (
	"fmt"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"
)

// fairDoor is counts of conns places, each client allowed perClient, with
// nothing else in the way.
func fairDoor(conns, perClient int) *counts {
	l := limitsFor(1<<20, 1<<20)
	l.Conns, l.ConnsPerClient, l.ConnsPer24 = conns, perClient, 1<<20
	return newCounts(l)
}

// hold opens a connection from addr, busy unless idle, or fails the test.
func hold(t *testing.T, k *counts, addr string, idle bool) *conn {
	t.Helper()
	x, why := k.openConn(netip.MustParseAddr(addr), nil)
	if x == nil {
		t.Fatalf("%s refused: %q", addr, why)
	}
	if !idle {
		k.setBusy(x, true)
	}
	return x
}

// holding is how many open connections client addr holds.
func holding(k *counts, addr string) int {
	return k.byClient.count(k.l.client(netip.MustParseAddr(addr)))
}

// TestAHeavyClientIsTrimmedToItsShare: newcomers each take a place from
// the client holding most, until it holds no more than they would; then a
// newcomer is refused, every connection being busy.
func TestAHeavyClientIsTrimmedToItsShare(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := fairDoor(8, 8)
		for range 8 {
			hold(t, k, "203.0.113.1", false)
		}
		for i := range 7 {
			hold(t, k, fmt.Sprintf("198.51.%d.1", i), false)
			if got, want := holding(k, "203.0.113.1"), 7-i; got != want {
				t.Fatalf("after newcomer %d the heavy client holds %d; want %d", i, got, want)
			}
		}
		if x, why := k.openConn(netip.MustParseAddr("198.51.99.1"), nil); x != nil || why != "too many connections" {
			t.Errorf("with every client at one busy connection: admitted %t, %q", x != nil, why)
		}
	})
}

// TestALightClientIsKeptWhileAHeavierOneHoldsMore: a client with a few
// connections loses none while another holds more.
func TestALightClientIsKeptWhileAHeavierOneHoldsMore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := fairDoor(10, 10)
		light := []*conn{hold(t, k, "192.0.2.7", false), hold(t, k, "192.0.2.7", true)}
		for range 8 {
			hold(t, k, "203.0.113.1", false)
		}
		for i := 0; holding(k, "203.0.113.1") > 2; i++ {
			hold(t, k, fmt.Sprintf("198.51.%d.1", i), false)
			for _, x := range light {
				if x.released {
					t.Fatalf("newcomer %d closed the light client's connection while the heavy one held %d", i, holding(k, "203.0.113.1"))
				}
			}
		}
	})
}

// TestAHeavyClientGivesUpItsIdlestThenItsNewest: the connection a holder
// gives up is the one it uses least: an idle one, the longest idle, else
// its newest.
func TestAHeavyClientGivesUpItsIdlestThenItsNewest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := fairDoor(4, 4)
		oldBusy := hold(t, k, "203.0.113.1", false)
		time.Sleep(time.Second) //nolint:forbidigo // virtual time
		idleOld := hold(t, k, "203.0.113.1", true)
		time.Sleep(time.Second) //nolint:forbidigo // virtual time
		idleNew := hold(t, k, "203.0.113.1", true)
		time.Sleep(time.Second) //nolint:forbidigo // virtual time
		newBusy := hold(t, k, "203.0.113.1", false)

		hold(t, k, "198.51.100.1", false)
		if !idleOld.released || idleNew.released {
			t.Fatalf("first: the longest idle closed %t, the other idle %t; want the longest idle alone", idleOld.released, idleNew.released)
		}
		hold(t, k, "198.51.101.1", false)
		if !idleNew.released {
			t.Fatal("second: the remaining idle connection was kept")
		}
		hold(t, k, "198.51.102.1", false)
		if !newBusy.released || oldBusy.released {
			t.Errorf("with none idle: newest closed %t, oldest %t; want the newest", newBusy.released, oldBusy.released)
		}
	})
}

// TestANetworkSpreadOverItsAddressesGivesWayFirst: an attacker holding one
// busy connection from each of many addresses of one /24, or /48, gives a
// place to a newcomer from another network.
func TestANetworkSpreadOverItsAddressesGivesWayFirst(t *testing.T) {
	for _, tt := range []struct{ name, attacker, newcomer string }{
		{"IPv4 /24", "203.0.113.%d", "198.51.100.1"},
		{"IPv6 /48", "2001:db8:5:%x::1", "2001:db8:6::1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				k := fairDoor(8, 8)
				for i := range 8 {
					hold(t, k, fmt.Sprintf(tt.attacker, i+1), false)
				}
				hold(t, k, tt.newcomer, false)
				if n := k.buckets.count(k.l.bucket(netip.MustParseAddr(fmt.Sprintf(tt.attacker, 1)))); n != 7 {
					t.Errorf("the attacker's network holds %d; want 7", n)
				}
			})
		})
	}
}

// TestANewcomerOfTheHeaviestNetworkGetsIn: a newcomer whose own network
// holds most still gets a place from the client of it holding more than
// it would.
func TestANewcomerOfTheHeaviestNetworkGetsIn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := fairDoor(8, 8)
		for range 8 {
			hold(t, k, "203.0.113.1", false)
		}
		hold(t, k, "203.0.113.2", false)
		if n := holding(k, "203.0.113.1"); n != 7 {
			t.Errorf("the heavy neighbour holds %d; want 7", n)
		}
	})
}

// TestTheHostsOwnSourceIsAClientLikeAnyOther: a machine's transit address,
// or an address of the host's own prefix, holding most gives way too.
func TestTheHostsOwnSourceIsAClientLikeAnyOther(t *testing.T) {
	for _, heavy := range []string{"100.64.0.2", "2001:db8:1:2::5"} {
		t.Run(heavy, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				k := fairDoor(8, 8)
				k.l.OwnPrefixes = []netip.Prefix{netip.MustParsePrefix("2001:db8:1:2::/64")}
				for range 8 {
					hold(t, k, heavy, false)
				}
				hold(t, k, "198.51.100.1", false)
				if n := holding(k, heavy); n != 7 {
					t.Errorf("%s holds %d; want 7", heavy, n)
				}
			})
		})
	}
}

// TestATallyFindsTheMost: the tally's top follows counts up and down.
func TestATallyFindsTheMost(t *testing.T) {
	tl := newTally[string]()
	if _, n := tl.top(); n != 0 {
		t.Fatalf("empty: top %d", n)
	}
	for _, k := range []string{"a", "b", "b", "c", "b"} {
		tl.add(k, 1)
	}
	if k, n := tl.top(); k != "b" || n != 3 {
		t.Errorf("top %q %d; want b 3", k, n)
	}
	tl.add("b", -1)
	tl.add("b", -1)
	tl.add("a", 1)
	if k, n := tl.top(); k != "a" && k != "b" || n != 2 {
		t.Errorf("top %q %d; want a or b, 2", k, n)
	}
	for _, k := range []string{"a", "a", "b", "b", "c"} {
		tl.add(k, -1)
	}
	if !tl.empty() {
		t.Errorf("left %v", tl.n)
	}
	if _, n := tl.top(); n != 0 {
		t.Errorf("emptied: top %d", n)
	}
}
