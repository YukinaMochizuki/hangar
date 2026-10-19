package login

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

// TestDeviceSource: a source is an IPv4 address, however it is written, an
// IPv6 /64, or an address alone in one of the host's own prefixes; no two
// sources' prefixes are prefixes of each other.
func TestDeviceSource(t *testing.T) {
	own := []netip.Prefix{netip.MustParsePrefix("2001:db8:0:1::/64")}
	for _, c := range []struct{ ip, want string }{
		{"192.0.2.1", "device-from:192.0.2.1:"},
		{"::ffff:192.0.2.1", "device-from:192.0.2.1:"},
		{"2001:db8:5:6:7::9", "device-from:2001:db8:5:6::/64:"},
		{"fe80::1%eth0", "device-from:fe80::/64:"},
		{"2001:db8:0:1::a", "device-from:2001:db8:0:1::a/128:"},
		{"", "device-from:unknown:"},
	} {
		var ip netip.Addr
		if c.ip != "" {
			ip = netip.MustParseAddr(c.ip)
		}
		if got := deviceSource(ip, own); got != c.want {
			t.Errorf("deviceSource(%q) = %q, want %q", c.ip, got, c.want)
		}
	}
	a, b := deviceSource(netip.MustParseAddr("192.0.2.1"), nil), deviceSource(netip.MustParseAddr("192.0.2.10"), nil)
	if strings.HasPrefix(b, a) || strings.HasPrefix(a, b) {
		t.Errorf("%q and %q would count each other's logins", a, b)
	}
}

// TestStartedTextIsUTC: the approval page says when a login started in
// UTC, as it says it does, on a host in any zone.
func TestStartedTextIsUTC(t *testing.T) {
	local := time.Local
	time.Local = time.FixedZone("UTC+9", 9*60*60)
	t.Cleanup(func() { time.Local = local })
	at := time.Date(2026, 10, 19, 3, 4, 5, 0, time.UTC)
	if got := startedText(at.UnixMilli()); got != "2026-10-19 03:04:05 UTC" {
		t.Errorf("startedText = %q", got)
	}
	if got := startedText(0); got != "an unknown time" {
		t.Errorf("startedText of a login kept before its time was = %q", got)
	}
}
