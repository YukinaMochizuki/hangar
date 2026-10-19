package front

import (
	"errors"
	"maps"
	"net"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"
)

// open admits a connection from each address in turn and says which were.
func open(k *counts, addrs ...string) (admitted []bool, releases []func(), whys []string) {
	for _, a := range addrs {
		x, why := k.openConn(netip.MustParseAddr(a), nil)
		admitted = append(admitted, x != nil)
		releases = append(releases, func() { k.release(x) })
		whys = append(whys, why)
	}
	return admitted, releases, whys
}

func limitsFor(per56, per48 int) Limits {
	l := DefaultLimits
	l.ConnRate, l.ConnBurst = 1000, 1000
	l.ConnsPer56, l.ConnsPer48 = per56, per48
	return l
}

// TestAClientIsAnIPv4AddressOrAnIPv6Slash64: the prefix every per-client
// limit is kept for.
func TestAClientIsAnIPv4AddressOrAnIPv6Slash64(t *testing.T) {
	c := func(a string) netip.Prefix { return DefaultLimits.client(netip.MustParseAddr(a)) }
	for in, want := range map[string]string{
		"192.0.2.7":                  "192.0.2.7/32",
		"::ffff:192.0.2.7":           "192.0.2.7/32",
		"2001:db8:1:2:3:4:5:6":       "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:ffff::1":  "2001:db8:1:2::/64",
		"2001:db8:1:3::1":            "2001:db8:1:3::/64",
		"2001:db8:1:2:8000::1":       "2001:db8:1:2::/64",
		"fe80::1":                    "fe80::/64",
		"2001:db8:1:2::":             "2001:db8:1:2::/64",
		"2001:db8:ffff:ffff:ffff::1": "2001:db8:ffff:ffff::/64",
	} {
		if got := c(in).String(); got != want {
			t.Errorf("client(%s) = %s, want %s", in, got, want)
		}
	}
	// One address and its mapped form are one client; two IPv4 addresses
	// are two.
	if c("192.0.2.7") != c("::ffff:192.0.2.7") || c("192.0.2.7") == c("192.0.2.8") {
		t.Error("IPv4 clients are not one an address")
	}
	// Two addresses of one /64 are one client; the next /64 is another.
	if c("2001:db8:1:2::1") != c("2001:db8:1:2:ffff::1") {
		t.Error("two addresses of one /64 are two clients")
	}
	if c("2001:db8:1:2::1") == c("2001:db8:1:3::1") {
		t.Error("two /64s are one client")
	}
}

// TestTheHostsMachinesAreEachAClient: in the host's own prefix, where every
// machine on the host has its address, each address is a client of its
// own, so one machine cannot spend the others' limits; outside it a /64 is
// still one.
func TestTheHostsMachinesAreEachAClient(t *testing.T) {
	l := DefaultLimits
	l.OwnPrefixes = []netip.Prefix{netip.MustParsePrefix("2001:db8:1:2::/64")}
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	c := func(a string) netip.Prefix { return l.client(netip.MustParseAddr(a)) }
	if got := c("2001:db8:1:2::5").String(); got != "2001:db8:1:2::5/128" {
		t.Errorf("a machine's client is %s", got)
	}
	if c("2001:db8:1:2::5") == c("2001:db8:1:2::6") {
		t.Error("two machines of the host are one client")
	}
	if got := c("2001:db8:1:3::5").String(); got != "2001:db8:1:3::/64" {
		t.Errorf("outside the own prefix: %s", got)
	}
	if got := c("192.0.2.7").String(); got != "192.0.2.7/32" {
		t.Errorf("IPv4: %s", got)
	}

	// Requests: one machine past its rate leaves another its own.
	l.RequestRate, l.RequestBurst = 0.001, 2
	k := newCounts(l)
	for i := range 2 {
		if _, _, ok := k.startRequest(c("2001:db8:1:2::5")); !ok {
			t.Fatalf("request %d within the burst refused", i)
		}
	}
	if _, _, ok := k.startRequest(c("2001:db8:1:2::5")); ok {
		t.Fatal("a request past the burst was let through")
	}
	if _, _, ok := k.startRequest(c("2001:db8:1:2::6")); !ok {
		t.Error("another machine of the host was held to the first one's rate")
	}

	for _, bad := range []string{"::/0", "2001:db8::/32", "192.0.2.0/24", "2001:db8:1:2::1/64"} {
		l := DefaultLimits
		l.OwnPrefixes = []netip.Prefix{netip.MustParsePrefix(bad)}
		if l.Validate() == nil {
			t.Errorf("own prefix %s accepted", bad)
		}
	}
}

// TestRefusalsAreTakenOnceEach: a summary has every refusal since the
// last one, and nothing when there was none.
func TestRefusalsAreTakenOnceEach(t *testing.T) {
	r := newRefusals()
	if by := r.take(); by != nil {
		t.Fatalf("a summary with nothing refused: %v", by)
	}
	for _, why := range []string{"a", "b", "a"} {
		r.note(why)
	}
	if by := r.take(); !maps.Equal(by, map[string]int{"a": 2, "b": 1}) {
		t.Errorf("the summary: %v; want a 2, b 1", by)
	}
	if by := r.take(); by != nil {
		t.Errorf("a second summary with nothing new: %v", by)
	}
}

// TestAnIPv6NetworkIsCappedAboveItsClients: clients each within their own
// /64 cap still meet the cap of the /56 and of the /48 they share.
func TestAnIPv6NetworkIsCappedAboveItsClients(t *testing.T) {
	t.Run("/56", func(t *testing.T) {
		k := newCounts(limitsFor(3, 100))
		// Three /64s of 2001:db8:0:100::/56, one connection each.
		admitted, releases, _ := open(k, "2001:db8:0:100::1", "2001:db8:0:101::1", "2001:db8:0:1fe::1")
		for i, ok := range admitted {
			if !ok {
				t.Fatalf("connection %d within the /56's cap refused", i)
			}
		}
		admitted, _, whys := open(k, "2001:db8:0:1ff::1")
		if admitted[0] || whys[0] != "too many connections from the client's /56" {
			t.Errorf("a fourth /64 in the /56: admitted %t, %q", admitted[0], whys[0])
		}
		// Another /56 of the same /48 is apart.
		if admitted, _, whys := open(k, "2001:db8:0:200::1"); !admitted[0] {
			t.Errorf("another /56 refused: %q", whys[0])
		}
		releases[0]()
		if admitted, _, whys := open(k, "2001:db8:0:1ff::1"); !admitted[0] {
			t.Errorf("a connection closed did not free the /56's place: %q", whys[0])
		}
	})
	t.Run("/48", func(t *testing.T) {
		k := newCounts(limitsFor(100, 2))
		// Two /56s of 2001:db8:5::/48.
		admitted, _, _ := open(k, "2001:db8:5:100::1", "2001:db8:5:200::1")
		if !admitted[0] || !admitted[1] {
			t.Fatalf("connections within the /48's cap refused: %v", admitted)
		}
		admitted, _, whys := open(k, "2001:db8:5:300::1")
		if admitted[0] || whys[0] != "too many connections from the client's /48" {
			t.Errorf("a third /56 in the /48: admitted %t, %q", admitted[0], whys[0])
		}
		if admitted, _, whys := open(k, "2001:db8:6::1"); !admitted[0] {
			t.Errorf("another /48 refused: %q", whys[0])
		}
	})
	t.Run("the host's own /64 has none", func(t *testing.T) {
		l := limitsFor(1, 1)
		l.OwnPrefixes = []netip.Prefix{netip.MustParsePrefix("2001:db8:7:1::/64")}
		k := newCounts(l)
		admitted, _, whys := open(k, "2001:db8:7:1::10", "2001:db8:7:1::11", "2001:db8:7:1::12")
		for i, ok := range admitted {
			if !ok {
				t.Errorf("machine %d of the host refused: %q", i, whys[i])
			}
		}
		// Its neighbours outside the /64 are capped as ever.
		admitted, _, _ = open(k, "2001:db8:7:2::1", "2001:db8:7:3::1")
		if !admitted[0] || admitted[1] {
			t.Errorf("outside the own /64: %v, want the /56's cap of one", admitted)
		}
	})
	t.Run("the IPv6 caps do not count IPv4", func(t *testing.T) {
		k := newCounts(limitsFor(1, 1))
		admitted, _, whys := open(k, "192.0.2.1", "192.0.2.2", "::ffff:192.0.2.3")
		for i, ok := range admitted {
			if !ok {
				t.Errorf("IPv4 connection %d refused: %q", i, whys[i])
			}
		}
	})
}

// TestAnIPv4Slash24IsCappedAboveItsClients: addresses each within their own
// cap still meet the cap of the /24 they share, as IPv6 clients do their
// /48's; the host's machines' IPv4 does not.
func TestAnIPv4Slash24IsCappedAboveItsClients(t *testing.T) {
	if DefaultLimits.ConnsPer24 != DefaultLimits.ConnsPer48 {
		t.Errorf("an IPv4 /24 caps %d, an IPv6 /48 %d; want them alike", DefaultLimits.ConnsPer24, DefaultLimits.ConnsPer48)
	}
	l := limitsFor(100, 100)
	l.ConnsPer24 = 3
	k := newCounts(l)
	admitted, releases, _ := open(k, "192.0.2.1", "192.0.2.2", "::ffff:192.0.2.255")
	for i, ok := range admitted {
		if !ok {
			t.Fatalf("connection %d within the /24's cap refused", i)
		}
	}
	admitted, _, whys := open(k, "192.0.2.3")
	if admitted[0] || whys[0] != "too many connections from the client's /24" {
		t.Errorf("a fourth address in the /24: admitted %t, %q", admitted[0], whys[0])
	}
	if admitted, _, whys := open(k, "192.0.3.1"); !admitted[0] {
		t.Errorf("another /24 refused: %q", whys[0])
	}
	releases[0]()
	if admitted, _, whys := open(k, "192.0.2.3"); !admitted[0] {
		t.Errorf("a connection closed did not free the /24's place: %q", whys[0])
	}

	// The host's networks' transit addresses, packed in one /24.
	admitted, _, whys = open(k, "100.64.0.0", "100.64.0.2", "100.64.0.4", "100.64.0.6")
	for i, ok := range admitted {
		if !ok {
			t.Errorf("network %d of the host refused: %q", i, whys[i])
		}
	}
}

// TestATransitAddressIsStillAClient: the exemption is from the /24's cap
// alone; one network of the host is held to a client's.
func TestATransitAddressIsStillAClient(t *testing.T) {
	l := limitsFor(100, 100)
	l.ConnsPerClient = 2
	k := newCounts(l)
	admitted, _, whys := open(k, "100.64.0.2", "100.64.0.2", "100.64.0.2")
	if !admitted[0] || !admitted[1] || admitted[2] || whys[2] != "too many connections from the client" {
		t.Errorf("three from one transit address: %v, %q; want the third refused as the client's", admitted, whys[2])
	}
}

// TestAFullDoorEvictsTheIdlest: past the cap on every connection, the one
// idle longest is closed for a new one; a busy one never is, and with
// every one busy the new one is refused.
func TestAFullDoorEvictsTheIdlest(t *testing.T) {
	synctest.Test(t, testAFullDoorEvictsTheIdlest)
}

func testAFullDoorEvictsTheIdlest(t *testing.T) {
	l := limitsFor(100, 100)
	l.Conns = 3
	k := newCounts(l)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	k.now = func() time.Time { return now }
	addr := netip.MustParseAddr("192.0.2.1")
	openPipe := func() (*conn, net.Conn) {
		t.Helper()
		c, s := net.Pipe()
		t.Cleanup(func() { _ = c.Close() })
		x, why := k.openConn(addr, s)
		if x == nil {
			t.Fatalf("refused: %q", why)
		}
		return x, c
	}
	closed := func(c net.Conn) bool {
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		_, err := c.Read(make([]byte, 1))
		var ne net.Error
		return err != nil && (!errors.As(err, &ne) || !ne.Timeout())
	}

	busy, busyC := openPipe()
	k.setBusy(busy, true)
	now = now.Add(time.Second)
	// Opened first, but busy since, and idle again last: its idleness
	// counts from then.
	again, againC := openPipe()
	k.setBusy(again, true)
	now = now.Add(time.Second)
	_, idlestC := openPipe()
	now = now.Add(time.Second)
	k.setBusy(again, false)

	openPipe()
	if !closed(idlestC) {
		t.Error("the connection idle longest was kept")
	}
	if closed(busyC) || closed(againC) {
		t.Error("a busy connection, or one idle less long, was closed")
	}
	if k.conns != 3 {
		t.Errorf("%d connections counted; want 3", k.conns)
	}

	for x := range k.open {
		k.setBusy(x, true)
	}
	if x, why := k.openConn(addr, nil); x != nil || why != "too many connections" {
		t.Errorf("with every connection busy: admitted %t, %q", x != nil, why)
	}
}
