//go:build testbed

package vms

import (
	"bufio"
	"context"
	"net/netip"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/drivertest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
)

// v6Uplink is the uplink of the hosts with an IPv6 prefix: a dummy link of
// the tests' own, so the host's real uplink is left as it is.
const v6Uplink = "tbv6up"

var v6Prefix = netip.MustParsePrefix("2001:db8:0:1::/64")

// TestConformanceWithIPv6 holds the real drivers to the contract on a host
// with an IPv6 prefix, its guests' IPv6 among it.
func TestConformanceWithIPv6(t *testing.T) {
	for _, args := range [][]string{
		{"ip", "link", "add", v6Uplink, "type", "dummy"},
		{"ip", "addr", "add", "192.0.2.1/24", "dev", v6Uplink},
		{"ip", "-6", "addr", "add", "2001:db8:0:1::1/64", "dev", v6Uplink, "nodad"},
		{"ip", "link", "set", v6Uplink, "up"},
		// As deploy/host has the host; forwarding is left on, as the network
		// and flow tests leave it.
		{"sysctl", "-q", "-w", "net.ipv6.conf." + v6Uplink + ".proxy_ndp=1"},
		{"sysctl", "-q", "-w", "net.ipv6.conf.all.forwarding=1"},
	} {
		mustRun(t, args[0], args[1:]...)
	}
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", v6Uplink).Run() })
	drivertest.Run(t, func(t *testing.T) drivertest.Host {
		d := conformanceHost(t, newHostOn(t, hostnet.Config{Uplink: v6Uplink, IPv6Prefix: v6Prefix}))
		d.IPv6Prefix, d.GuestIPv6 = v6Prefix, guestIPv6
		// What has nothing of IPv6 is TestConformance's alone; disk images
		// convert, the heaviest writes the pool takes.
		d.DiskImages, d.PublishDiskImage, d.Firmware = nil, nil, false
		return d
	})
}

var (
	linkLine    = regexp.MustCompile(`^link (\S+) (\S+)$`)
	addrDevLine = regexp.MustCompile(`^\d+: ([^:@]+)[@:]`)
	globalLine  = regexp.MustCompile(`^\s+inet6 (\S+)/128 scope global`)
	defaultLine = regexp.MustCompile(`^default via (\S+) dev (\S+)`)
)

// guestIPv6 reads what the machine's guest holds of its public IPv6, from
// inside it: each link of a device, as the platform's NICs are and the
// bridges and tunnels the guest's image makes are not, by name, with its
// MAC, global /128 and the default route's gateway if by it. A guest out of
// reach holds nothing.
func guestIPv6(t *testing.T, uid string) []driver.GuestIPv6 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, code, err := execIn(ctx, uid, "sh", "-c",
		`for d in /sys/class/net/*; do if [ -e "$d/device" ]; then echo "link ${d##*/} $(cat $d/address)"; fi; done; ip -6 addr show; ip -6 route show default`)
	if err != nil || code != 0 {
		t.Logf("reading the guest's IPv6: %v, exit %d: %s", err, code, out)
		return nil
	}
	byName := map[string]*driver.GuestIPv6{}
	var names []string
	dev := ""
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		l := sc.Text()
		if m := linkLine.FindStringSubmatch(l); m != nil {
			byName[m[1]] = &driver.GuestIPv6{MAC: m[2]}
			names = append(names, m[1])
		} else if m := addrDevLine.FindStringSubmatch(l); m != nil {
			dev = m[1]
		} else if m := globalLine.FindStringSubmatch(l); m != nil && byName[dev] != nil {
			byName[dev].Address, _ = netip.ParseAddr(m[1])
		} else if m := defaultLine.FindStringSubmatch(l); m != nil && byName[m[2]] != nil {
			byName[m[2]].Gateway, _ = netip.ParseAddr(m[1])
		}
	}
	slices.Sort(names)
	got := make([]driver.GuestIPv6, 0, len(names))
	for _, n := range names {
		got = append(got, *byName[n])
	}
	return got
}
