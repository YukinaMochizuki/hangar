//go:build testbed

// Package network holds the real network driver (internal/node/driver/hostnet)
// to the drivers' contract and to the topology of
// docs/decisions/networking.md on a real host: the conformance suite, then
// traffic from stand-in guests through both NATs, isolation between
// networks, recovery from drift, and leaving nothing but what was there.
// It needs root and no KVM; make e2e-host-network runs it, on a disposable
// testbed VM only (docs/runbook/testbed.md).
package network

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
)

// The upstream stands in for the internet behind an uplink the tests can
// take down without cutting the testbed off: a namespace on the far end of
// a veth, which answers HTTP with the address it sees a request come from.
const (
	upstreamNS   = "tb-upstream"
	upstreamLink = "tbup0"
	hostAddr     = "198.51.100.2"
	upstreamAddr = "198.51.100.1"
	upstreamURL  = "http://" + upstreamAddr + ":8080/"
	// The upstream's IPv6 is OVH's: the host's /64 is on-link at the
	// upstream, which asks for each address by neighbour solicitation and
	// delivers only to the MAC that answers, and the upstream's own
	// address, the gateway, is outside it. The host holds ::1 of it.
	hostPrefix     = "2001:db8:0:1::/64"
	hostAddr6      = "2001:db8:0:1::1"
	upstreamAddr6  = "2001:db8:0:ff::ffff"
	upstreamURL6   = "http://[" + upstreamAddr6 + "]:8080/"
	upstreamOnLink = "2001:db8:0:ff::/64"
	// guestPrefix names the namespaces of stand-in guests.
	guestPrefix = "tbg-"
	lockFile    = "/run/lock/hangar-testbed.lock"
	// markSize is the payload of the pings tests count, of IP length
	// markLength; no other traffic has it.
	markSize   = "1111"
	markLength = "1139"
)

// counters is the nft table counting, in the upstream or a guest, what
// tests look for: anything from a source other than the host, and marked
// pings.
const counters = `
table ip tb {
	counter foreign {}
	counter marked-request {}
	counter marked-reply {}
	chain pre {
		type filter hook prerouting priority 0; policy accept;
		iifname != "lo" ip saddr != ` + hostAddr + ` counter name foreign
		icmp type echo-request ip length ` + markLength + ` counter name marked-request
		icmp type echo-reply ip length ` + markLength + ` counter name marked-reply
	}
}
`

func TestMain(m *testing.M) {
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "network:", err)
		os.Exit(1)
	}
	if os.Geteuid() != 0 {
		fail(fmt.Errorf("must run as root (make e2e-host-network does)"))
	}
	lock, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fail(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		fail(err)
	}
	// deploy/host turns forwarding on; a testbed is not bootstrapped.
	for _, f := range []string{"/proc/sys/net/ipv4/ip_forward", "/proc/sys/net/ipv6/conf/all/forwarding"} {
		if err := os.WriteFile(f, []byte("1"), 0); err != nil {
			fail(err)
		}
	}
	sweep()
	if err := upstreamUp(); err != nil {
		sweep()
		fail(fmt.Errorf("upstream: %w", err))
	}
	code := m.Run()
	sweep()
	os.Exit(code)
}

// sweep removes what a run makes, the driver's objects included.
func sweep() {
	if d, err := hostnet.New(hostnet.Config{Uplink: "lo"}); err == nil {
		ctx := context.Background()
		nets, _ := d.Networks(ctx)
		for _, n := range nets {
			_ = d.DeleteNetwork(ctx, n.UID)
		}
		_ = d.Close()
	}
	names, _ := os.ReadDir("/run/netns")
	for _, n := range names {
		if strings.HasPrefix(n.Name(), guestPrefix) || n.Name() == upstreamNS || strings.HasPrefix(n.Name(), decoyPrefix) {
			_ = exec.Command("ip", "netns", "del", n.Name()).Run()
		}
	}
	links, _ := netlink.LinkList()
	for _, l := range links {
		if n := l.Attrs().Name; n == upstreamLink || strings.HasPrefix(n, decoyPrefix) {
			_ = netlink.LinkDel(l)
		}
	}
	for _, t := range []string{"ip " + decoyPrefix, "inet " + decoyPrefix, "ip " + decoyPrefix + "_dnat"} {
		_ = exec.Command("nft", append([]string{"delete", "table"}, strings.Fields(t)...)...).Run()
	}
}

func upstreamUp() error {
	for _, args := range [][]string{
		{"ip", "netns", "add", upstreamNS},
		{"ip", "link", "add", upstreamLink, "type", "veth", "peer", "name", "tbupx", "netns", upstreamNS},
		{"ip", "addr", "add", hostAddr + "/24", "dev", upstreamLink},
		{"ip", "link", "set", upstreamLink, "up"},
		{"ip", "-n", upstreamNS, "addr", "add", upstreamAddr + "/24", "dev", "tbupx"},
		{"ip", "-n", upstreamNS, "link", "set", "tbupx", "up"},
		{"ip", "-n", upstreamNS, "link", "set", "lo", "up"},
		// deploy/host sets proxy NDP on the uplink.
		{"sysctl", "-q", "-w", "net.ipv6.conf." + upstreamLink + ".proxy_ndp=1"},
		{"ip", "-6", "addr", "add", hostAddr6 + "/64", "dev", upstreamLink, "nodad"},
		{"ip", "-6", "route", "add", upstreamOnLink, "dev", upstreamLink},
		{"ip", "-n", upstreamNS, "-6", "addr", "add", upstreamAddr6 + "/64", "dev", "tbupx", "nodad"},
		{"ip", "-n", upstreamNS, "-6", "route", "add", hostPrefix, "dev", "tbupx"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, out)
		}
	}
	if err := addCounters(upstreamNS); err != nil {
		return err
	}
	ns, err := netns.GetFromName(upstreamNS)
	if err != nil {
		return err
	}
	defer func() { _ = ns.Close() }()
	// Port 25 too, which a network may not reach.
	for _, at := range []string{upstreamAddr + ":8080", "[" + upstreamAddr6 + "]:8080", "[" + upstreamAddr6 + "]:25"} {
		var ln net.Listener
		if err := inNS(ns, func() (err error) {
			ln, err = net.Listen("tcp", at)
			return err
		}); err != nil {
			return err
		}
		go func() {
			_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				host, _, _ := net.SplitHostPort(r.RemoteAddr)
				_, _ = fmt.Fprint(w, host)
			}))
		}()
	}
	return nil
}

func addCounters(ns string) error {
	c := exec.Command("ip", "netns", "exec", ns, "nft", "-f", "-")
	c.Stdin = strings.NewReader(counters)
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("counters in %s: %w: %s", ns, err, out)
	}
	return nil
}

// count returns a counter of namespace ns's table of counters, and resets
// it.
func count(t *testing.T, ns, name string) int {
	t.Helper()
	out := run(t, "ip", "netns", "exec", ns, "nft", "reset", "counter", "ip", "tb", name)
	m := regexp.MustCompile(`packets (\d+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no packets in counter %s of %s: %s", name, ns, out)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// noneForeign fails t if the upstream saw anything from a source other than
// the host since it was last asked.
func noneForeign(t *testing.T) {
	t.Helper()
	if n := count(t, upstreamNS, "foreign"); n > 0 {
		t.Errorf("the upstream saw %d packets from a source other than the host, %s", n, hostAddr)
	}
}

// inNS runs f on a thread in ns that is never used again.
func inNS(ns netns.NsHandle, f func() error) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := netns.Set(ns); err != nil {
			errc <- err
			return
		}
		errc <- f()
	}()
	return <-errc
}
