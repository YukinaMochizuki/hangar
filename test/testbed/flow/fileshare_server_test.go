//go:build testbed

package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/vishvananda/netns"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/diskimgtest"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// upstreamAddr4 is the IPv4 internet as TestAShareServerInitiatesNothing
// has it: an address of the upstream namespace on the uplink's range.
const upstreamAddr4 = "198.51.100.1"

// echoIn answers TCP and UDP on port 7 of every address of the namespace,
// both families, with what it is sent, until the test ends.
func echoIn(t *testing.T, ns string) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = orig.Close() }()
	target, err := netns.GetFromName(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Close() }()
	if err := netns.Set(target); err != nil {
		t.Fatal(err)
	}
	ln, lerr := net.Listen("tcp", ":7")
	pc, perr := net.ListenPacket("udp", ":7")
	if err := netns.Set(orig); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(lerr, perr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close(); _ = pc.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], from)
		}
	}()
}

// probeTarget is what the server's netprobe tries, and the container that
// reaches it, the control, from controlTarget where its name differs (a
// link-local address's zone); routed is whether a packet to it leaves the
// server's network, through a gateway.
type probeTarget struct {
	target, control, controlTarget string
	routed                         bool
}

// proto and family are a target's protocol, tcp, udp or icmp, and its
// address family, v4 or v6.
func (p probeTarget) proto() string { s, _, _ := strings.Cut(p.target, ":"); return s }

func (p probeTarget) family() string {
	_, rest, _ := strings.Cut(p.target, ":")
	if strings.Contains(rest, "[") || strings.Count(rest, ":") > 1 {
		return "v6"
	}
	return "v4"
}

// controlReaches says whether the control container reaches the target as
// netprobe names it, with busybox's tools.
func controlReaches(t *testing.T, ct, target string) (bool, string) {
	t.Helper()
	proto, rest, _ := strings.Cut(target, ":")
	if proto == "icmp" {
		six := ""
		if strings.Contains(rest, ":") {
			six = "-6 "
		}
		return pings(t, "", ct, six+rest)
	}
	host, port, err := net.SplitHostPort(rest)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case proto == "tcp":
		return connects(t, "", ct, host, port)
	case port == "53":
		out, code := sh(t, "", ct, "nslookup hangar.internal "+host)
		return strings.Contains(out, "Server:"), fmt.Sprintf("dns %s from %s: exit %d: %s", host, ct, code, out)
	default:
		return udpAnswered(t, "", ct, host, port)
	}
}

// netprobe runs the testbed server's /init netprobe and returns each
// target's verdict and error (cmd/hangar-init/server_testbed.go).
func netprobe(t *testing.T, srv string, args ...string) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, code, err := execIn(ctx, srv, append([]string{"/init", "netprobe"}, args...)...)
	if err != nil || code != 0 {
		t.Fatalf("netprobe in the server: exit %d, %v: %s", code, err, out)
	}
	res := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		target, rest, _ := strings.Cut(l, " ")
		res[target] = rest
	}
	return res
}

// linkLocal is a container's link-local IPv6 on its eth0, once the kernel
// has made it, which may be after the container is Ready.
func linkLocal(t *testing.T, ct string) string {
	t.Helper()
	re := regexp.MustCompile(`inet6 (fe80::[0-9a-f:]+)/64`)
	var ll string
	eventually(t, 30*time.Second, ct+"'s link-local address", func() (bool, string) {
		out, _ := sh(t, "", ct, "ip -6 -o addr show dev eth0 scope link")
		if m := re.FindStringSubmatch(out); m != nil && !strings.Contains(out, "tentative") {
			ll = m[1]
			return true, ""
		}
		return false, out
	})
	return ll
}

// TestAShareServerInitiatesNothing: from inside a share's server, booted
// from the testbed initramfs whose servers run what the host asks, nothing
// new gets out, over TCP, UDP or ICMP, on IPv4 or IPv6, even with a
// default route and an IPv6 address taken by hand: not to a client, to
// another machine of its networks, at its address or its link-local one,
// to a machine of another network, to the gateways' DNS, metadata or
// ICMP, nor to the internet. Each probe must time out or find no
// neighbour; a refusal or a local error fails the test. A container
// reaches each target, the control. Then, with the agent killed and the
// server's taps let through by hand in bridge hangar and hangar_sg, the
// same probe gets out over each protocol, each family, on-link and
// routed: the test can fail on each.
func TestAShareServerInitiatesNothing(t *testing.T) {
	if installed {
		t.Skip("the installed agent's uplink is the host's own")
	}
	probeInitrd := vmtest.Artifacts().ServerProbeInitramfs
	if _, err := os.Stat(probeInitrd); err != nil {
		t.Fatalf("%v (make guest-initramfs-testbed builds it)", err)
	}
	c := startClusterWith(t, func(c *cluster) {
		upstream6(t)
		mustRun(t, "ip", "-n", upstream6NS, "addr", "add", upstreamAddr4+"/24", "dev", "tbfupx")
		echoIn(t, upstream6NS)
		c.uplink = upstream6Link
		c.nodeArgs = []string{"--public-ipv6-prefix", hostPrefix6, "--guest-initramfs", probeInitrd}
	})
	open := func(group, network string) string {
		return fmt.Sprintf(`apiVersion: network.hangarlab.cloud/v1alpha1
kind: SecurityGroup
metadata: {name: %s}
spec:
  network: %s
  ingress:
  - {protocol: SECURITY_GROUP_PROTOCOL_ALL, from: {cidr: 0.0.0.0/0}}
  - {protocol: SECURITY_GROUP_PROTOCOL_ALL, from: {cidr: "::/0"}}
---
`, group, network)
	}
	c.applyYAML(`apiVersion: network.hangarlab.cloud/v1alpha1
kind: Network
metadata: {name: backend}
spec: {ipv4Cidr: 10.1.0.0/24}
---
apiVersion: network.hangarlab.cloud/v1alpha1
kind: Network
metadata: {name: far}
spec: {ipv4Cidr: 10.2.0.0/24}
---
` + open("open", "default") + open("openb", "backend") + open("openf", "far"))
	c.applyYAML(shareYAML("data", "1Gi", "default", "backend"))
	c.shareAddr("data", "default")
	c.shareAddr("data", "backend")
	machines := []struct{ name, network, group string }{
		{"client", "default", "open"}, {"peer", "default", "open"},
		{"clientb", "backend", "openb"}, {"otherb", "backend", "openb"},
		{"far1", "far", "openf"}, {"far2", "far", "openf"},
	}
	for _, m := range machines {
		ifaces := fmt.Sprintf("[{network: %s, securityGroups: [default, %s]}]", m.network, m.group)
		if strings.HasPrefix(m.name, "client") {
			c.applyYAML(ctYAML(m.name, responderScript, ifaces, shareVolume("data", false)))
		} else {
			c.applyYAML(ctYAML(m.name, responderScript, ifaces))
		}
	}
	for _, m := range machines {
		c.waitReady(m.name, 3*time.Minute)
	}
	srv := c.serverMachine("data").GetMetadata().GetUid()

	t.Run("the server is a testbed build", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cl, done := guest().Client(srv)
		defer done()
		resp, err := cl.Ping(ctx, connect.NewRequest(&guestv1.PingRequest{}))
		if err != nil || !strings.HasSuffix(resp.Msg.GetVersion(), "+testbed") {
			t.Fatalf("the server's agent: %v, %v; want a version ending +testbed", resp, err)
		}
	})

	// The server's own interfaces: eth0 on default, eth1 on backend.
	zone := map[string]string{"default": "eth0", "backend": "eth1"}
	var targets []probeTarget
	for _, m := range machines {
		nic := c.machine(m.name).GetSpec().GetInterfaces()[0]
		v4, _, _ := strings.Cut(nic.GetIpv4(), "/")
		v6, _, _ := strings.Cut(nic.GetIpv6(), "/")
		if v6 == "" {
			t.Fatalf("%s has no IPv6: %v", m.name, nic)
		}
		// The control of an IPv4 target is a neighbour on its network; a
		// /128 is routed, from anywhere, through a gateway.
		var neighbour string
		for _, o := range machines {
			if o.network == m.network && o.name != m.name {
				neighbour = o.name
				break
			}
		}
		control6 := "peer"
		if m.name == "peer" {
			control6 = "client"
		}
		routed4 := m.network == "far"
		for _, p := range []string{"7", "2049"} {
			targets = append(targets,
				probeTarget{target: "tcp:" + net.JoinHostPort(v4, p), control: neighbour, routed: routed4},
				probeTarget{target: "udp:" + net.JoinHostPort(v4, p), control: neighbour, routed: routed4},
				probeTarget{target: "tcp:" + net.JoinHostPort(v6, p), control: control6, routed: true},
				probeTarget{target: "udp:" + net.JoinHostPort(v6, p), control: control6, routed: true})
		}
		targets = append(targets,
			probeTarget{target: "icmp:" + v4, control: neighbour, routed: routed4},
			probeTarget{target: "icmp:" + v6, control: control6, routed: true})
		// On-link IPv6: the link-local address of a machine on one of
		// the server's networks, reached from the server's interface there.
		if z, ok := zone[m.network]; ok {
			ll := linkLocal(t, m.name)
			for _, p := range []string{"tcp:", "udp:"} {
				targets = append(targets, probeTarget{
					target:  p + net.JoinHostPort(ll+"%"+z, "7"),
					control: neighbour, controlTarget: p + net.JoinHostPort(ll+"%eth0", "7"),
				})
			}
			targets = append(targets, probeTarget{target: "icmp:" + ll + "%" + z, control: neighbour, controlTarget: "icmp:" + ll + "%eth0"})
		}
	}
	targets = append(targets,
		// The gateways: DNS, metadata and ICMP.
		probeTarget{target: "tcp:10.0.0.1:53", control: "peer"}, probeTarget{target: "udp:10.0.0.1:53", control: "peer"},
		probeTarget{target: "icmp:10.0.0.1", control: "peer"}, probeTarget{target: "tcp:169.254.169.254:80", control: "peer", routed: true},
		probeTarget{target: "tcp:10.1.0.1:53", control: "otherb"}, probeTarget{target: "udp:10.1.0.1:53", control: "otherb"},
		probeTarget{target: "icmp:10.1.0.1", control: "otherb"},
		probeTarget{target: "icmp:fe80::1%eth0", control: "peer"},
		// The internet.
		probeTarget{target: "tcp:" + upstreamAddr4 + ":7", control: "peer", routed: true},
		probeTarget{target: "udp:" + upstreamAddr4 + ":7", control: "peer", routed: true},
		probeTarget{target: "icmp:" + upstreamAddr4, control: "peer", routed: true},
		probeTarget{target: "tcp:[" + upstreamAddr6 + "]:7", control: "peer", routed: true},
		probeTarget{target: "udp:[" + upstreamAddr6 + "]:7", control: "peer", routed: true},
		probeTarget{target: "icmp:" + upstreamAddr6, control: "peer", routed: true},
	)

	t.Run("the controls reach each", func(t *testing.T) {
		for _, x := range targets {
			target := x.target
			if x.controlTarget != "" {
				target = x.controlTarget
			}
			var last string
			ok := false
			for range 3 {
				if ok, last = controlReaches(t, x.control, target); ok {
					break
				}
			}
			if !ok {
				t.Errorf("control: %s", last)
			}
		}
	})

	names := make([]string, len(targets))
	for i, x := range targets {
		names[i] = x.target
	}
	// An address of the prefix no machine has, for the server to take.
	taken := strings.TrimSuffix(hostPrefix6, "/64") + "dead:beef"
	probeAll := func() map[string]string {
		t.Helper()
		res := netprobe(t, srv, append([]string{"-route4", "-addr6", taken}, names...)...)
		for _, n := range names {
			if strings.HasPrefix(res[n], "local") || res[n] == "" {
				t.Errorf("the probe of %s failed in the server itself: %q", n, res[n])
			}
		}
		return res
	}
	t.Run("the server reaches nothing", func(t *testing.T) {
		res := probeAll()
		for _, n := range names {
			if !strings.HasPrefix(res[n], "blocked") {
				t.Errorf("from the server, %s: %q, not blocked", n, res[n])
			}
		}
		t.Logf("the server's probes:\n%s", fmt.Sprint(res))
	})
	if t.Failed() {
		return
	}

	// The mutation: the server's taps let through by hand, ahead of both
	// their binding and their servers' chain.
	c.killAgent()
	t.Run("with the server's taps let through, it gets out", func(t *testing.T) {
		for _, network := range []string{"default", "backend"} {
			tp := c.serverTap("data", network)
			letThrough(t, tp.netns, "hangar", tp.name)
			letThrough(t, tp.netns, "hangar_sg", tp.name)
		}
		res := probeAll()
		t.Logf("the server's probes, its taps let through:\n%s", fmt.Sprint(res))
		// Each kind of target the test refuses must now be reached by one
		// probe at least: else that kind of refusal could not fail.
		got := map[string]bool{}
		for _, x := range targets {
			if res[x.target] != "ok" {
				continue
			}
			where := "on-link"
			if x.routed {
				where = "routed"
			}
			for _, k := range []string{x.proto(), x.family(), where, x.proto() + " " + x.family() + " " + where} {
				got[k] = true
			}
		}
		for _, k := range []string{"tcp", "udp", "icmp", "v4", "v6", "on-link", "routed",
			"tcp v4 on-link", "udp v4 on-link", "icmp v4 on-link",
			"tcp v6 on-link", "udp v6 on-link", "icmp v6 on-link",
			"tcp v4 routed", "udp v4 routed", "icmp v4 routed"} {
			if !got[k] {
				t.Errorf("with the server's taps let through, no %s probe gets out: its refusal cannot fail", k)
			}
		}
	})
}

// letThrough inserts, first in each base chain of a bridge table in the
// namespace, rules accepting all from and to the tap.
func letThrough(t *testing.T, ns, table, tapName string) {
	t.Helper()
	out := mustRun(t, "ip", "netns", "exec", ns, "nft", "-j", "list", "table", "bridge", table)
	var listing struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(out), &listing); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range listing.Nftables {
		raw, ok := o["chain"]
		if !ok {
			continue
		}
		var ch struct {
			Name string `json:"name"`
			Hook string `json:"hook"`
		}
		if err := json.Unmarshal(raw, &ch); err != nil {
			t.Fatal(err)
		}
		if ch.Hook == "" {
			continue
		}
		for _, dir := range []string{"iifname", "oifname"} {
			mustRun(t, "ip", "netns", "exec", ns, "nft", "insert", "rule", "bridge", table, ch.Name, dir, tapName, "accept")
		}
		n++
	}
	if n == 0 {
		t.Fatalf("bridge %s in %s has no base chain:\n%s", table, ns, out)
	}
}

// writerAcked appends "N UPTIME" to /data/NAME.log, fsyncs it, and only
// then says "acked N" on its stdout, every 100 ms; a line it fails to write
// or sync it says "failed N" of, with the error.
func writerAcked(name string) string {
	return `trap 'exit 0' TERM
i=0
while :; do
  i=$((i+1))
  if { echo "$i $(cut -d' ' -f1 /proc/uptime)" >> /data/` + name + `.log && fsync /data/` + name + `.log; } 2>/tmp/err; then
    echo "acked $i"
  else
    echo "failed $i at $(cut -d' ' -f1 /proc/uptime): $(cat /tmp/err)"
  fi
  sleep 0.1 & wait $!
done`
}

// TestAShareServerKilledUnderLoad: the server's VM killed outright while
// two clients write, each line synced before it is acknowledged on the
// writer's console: the writers pause, and carry on once the node has
// started the server again, within the 30 s TestAServerRestartPausesClientsSeconds
// bounds it to (docs/runbook/testbed.md), and no acknowledged line is lost,
// none written twice.
func TestAShareServerKilledUnderLoad(t *testing.T) {
	c := startCluster(t)
	c.applyYAML(shareYAML("data", "1Gi", "default"))
	c.waitSharePhase("data", storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE)
	writers := []string{"w1", "w2"}
	for _, w := range writers {
		c.applyYAML(ctYAML(w, writerAcked(w), "", shareVolume("data", false)))
	}
	for _, w := range writers {
		c.waitReady(w, 3*time.Minute)
	}
	keepsWriting(t, "w1", "w2", 20, time.Minute)
	keepsWriting(t, "w2", "w1", 20, time.Minute)

	srv := c.serverMachine("data")
	booted := srv.GetStatus().GetBootTime().AsTime()
	pid := chPID(t, srv.GetMetadata().GetUid())
	if pid == 0 {
		t.Fatal("no Cloud Hypervisor for the server")
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	killed := time.Now()
	eventually(t, 3*time.Minute, "the server started again", func() (bool, string) {
		m := c.serverMachine("data")
		return m.GetStatus().GetBootTime().AsTime().After(booted), m.GetStatus().String()
	})
	t.Logf("the server booted again %v after the kill, at %s", time.Since(killed).Round(time.Millisecond), killed.Format(time.StampMicro))
	defer func() {
		for _, r := range consoleOf(t, srv.GetMetadata().GetUid()) {
			if r.Stream == console.Init {
				t.Logf("the server at %s: %s", r.Time.Format(time.StampMicro), strings.TrimSpace(string(r.Payload)))
			}
		}
	}()
	for _, w := range writers {
		keepsWriting(t, w, writers[0], 20, 2*time.Minute)
	}

	for _, w := range writers {
		stdout := consoleText(t, c.machine(w).GetMetadata().GetUid())
		acked := map[int]bool{}
		for _, m := range regexp.MustCompile(`(?m)^acked (\d+)$`).FindAllStringSubmatch(stdout, -1) {
			n, _ := strconv.Atoi(m[1])
			acked[n] = true
		}
		if failed := regexp.MustCompile(`(?m)^failed .*$`).FindAllString(stdout, -1); len(failed) > 0 {
			t.Errorf("%s's writes failed, where a hard mount only waits for its server:\n%s", w, strings.Join(failed, "\n"))
			for _, r := range consoleOf(t, c.machine(w).GetMetadata().GetUid()) {
				if r.Stream == console.Stdout && strings.HasPrefix(string(r.Payload), "failed") {
					t.Logf("%s at %s: %s", w, r.Time.Format(time.StampMicro), strings.TrimSpace(string(r.Payload)))
				}
			}
		}
		out := mustSh(t, writers[0], "cat /data/"+w+".log")
		held := map[int]bool{}
		var gap, gapAt, prev float64
		last := 0
		for i, l := range strings.Split(strings.TrimSpace(out), "\n") {
			f := strings.Fields(l)
			n, err := strconv.Atoi(f[0])
			if err != nil || len(f) != 2 || n <= last {
				t.Fatalf("%s's line %d is %q, after %d: a line written twice, or out of order", w, i+1, l, last)
			}
			last, held[n] = n, true
			up, _ := strconv.ParseFloat(f[1], 64)
			if i > 0 && up-prev > gap {
				gap, gapAt = up-prev, prev
			}
			prev = up
		}
		var lost []int
		for n := range acked {
			if !held[n] {
				lost = append(lost, n)
			}
		}
		t.Logf("%s: %d lines, %d acknowledged, its longest pause %.1f s from %.2f s of its uptime", w, len(held), len(acked), gap, gapAt)
		if len(lost) > 0 {
			slices.Sort(lost)
			t.Errorf("%s lost %d lines it acknowledged: %v", w, len(lost), lost)
		}
		if gap > 30 {
			t.Errorf("%s paused %.1f s, past 30 s", w, gap)
		}
		if gap < 1 {
			t.Errorf("%s never paused (%.1f s at most): was the server killed while it wrote?", w, gap)
		}
	}
}

// revokeVMUserData mounts the share with the console's commands, then, from
// a unit of its own, creates a file on it every half second, each create
// given 3 s, saying on the serial console how each went: its exit status,
// 137 for a create killed by timeout, and its error.
const revokeVMUserData = `#cloud-config
package_update: true
packages: [nfs-common]
runcmd:
- - sh
  - -c
  - |
    mkdir -p /mnt/data
    modprobe nfs
    echo $(cat /run/cloud-init/.instance-id) > /sys/module/nfs/parameters/nfs4_unique_id
    mount -t nfs4 -o nosuid,nodev,timeo=50 ADDRESS:/ /mnt/data && echo "hangar-fs mounted" > /dev/ttyS0
    systemd-run --unit=hangar-fs-probe sh -c 'i=0; while :; do i=$((i+1)); err=$(timeout -s KILL 3 sh -c "echo $i > /mnt/data/vm-$i" 2>&1); rc=$?; if [ $rc = 0 ]; then r=ok; else r=fail; fi; echo "hangar-fs probe $i $r rc=$rc" $err "."  > /dev/ttyS0; sleep 0.5; done'
`

// vmProbe is one of the VM's creates as its serial console says.
type vmProbe struct {
	n      int
	ok     bool
	rc     int
	errMsg string
}

// refused is whether a failed create was refused by the server, an error
// at once, rather than cut off, killed by its timeout.
func (p vmProbe) refused() bool {
	return !p.ok && p.rc != 137 && (strings.Contains(p.errMsg, "Permission denied") || strings.Contains(p.errMsg, "Operation not permitted"))
}

// TestAVMLosesARevokedShare: an Ubuntu VM declaring a share mounts it by
// hand and creates files on it; once its declaration is removed, its
// creates on the share it still has mounted are refused by the server
// (EACCES or EPERM, not a create hanging until its timeout) within 10 s,
// and none succeeds again, while a container declaring the share still
// writes and the VM was never started again. Its creates before, the
// control, show the probe sees one succeed. Then, with the agent killed,
// the VM's address put back in the server's export over vsock and the
// server's tap let through, a create succeeds again: the test can fail.
func TestAVMLosesARevokedShare(t *testing.T) {
	if installed {
		t.Skip("the installed agent trusts no test server's certificate")
	}
	srv := diskimgtest.New(t)
	url, _ := srv.Serve(t, diskimgtest.UbuntuName, diskimgtest.Ubuntu(t))
	fw, certs := diskimgtest.Firmware(t), srv.CertFile(t)
	c := startClusterWith(t, func(c *cluster) {
		c.nodeArgs = []string{"--vm-firmware", fw}
		c.nodeEnv = []string{"SSL_CERT_FILE=" + certs}
	})
	c.applyYAML(fmt.Sprintf(`apiVersion: storage.hangarlab.cloud/v1alpha1
kind: Image
metadata: {name: noble}
spec: {url: %q, sha256: %s, os: ubuntu, version: "24.04"}
`, url, diskimgtest.UbuntuSHA256))
	c.applyYAML(shareYAML("data", "1Gi", "default"))
	at := c.shareAddr("data", "default")
	c.applyYAML(sharingYAML("writer", "data", idleScript))
	c.waitReady("writer", 3*time.Minute)

	ud, _ := json.Marshal(strings.ReplaceAll(revokeVMUserData, "ADDRESS", at))
	vm := func(fileShares string) string {
		return fmt.Sprintf(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: VirtualMachine
metadata: {name: mounter}
spec:
  rootDisk: {fromImage: {image: {name: noble}}}
  resources: {cpu: 2, memory: 1Gi}
  fileShares: %s
  userData: %s
`, fileShares, ud)
	}
	c.applyYAML(vm("[{name: data}]"))
	c.cli("wait", "vm", "mounter", "--for=condition=Ready", "--timeout=10m")
	vmMachine := func() *computev1alpha1.Machine {
		m, ok := get(c, "machine", "vm-mounter", &computev1alpha1.Machine{})
		if !ok {
			t.Fatal("the VM has no machine vm-mounter")
		}
		return m
	}
	vmUID := vmMachine().GetMetadata().GetUid()
	// A firmware VM's serial console is its console log, read on the host
	// as hangar logs reads it, which the mutation of an agent killed would
	// leave nothing to go through.
	probeRe := regexp.MustCompile(`hangar-fs probe (\d+) (ok|fail) rc=(\d+)(.*?) \.`)
	probes := func() []vmProbe {
		var b strings.Builder
		for _, r := range consoleOf(t, vmUID) {
			b.Write(r.Payload)
		}
		var out []vmProbe
		for _, m := range probeRe.FindAllStringSubmatch(b.String(), -1) {
			n, _ := strconv.Atoi(m[1])
			rc, _ := strconv.Atoi(m[3])
			out = append(out, vmProbe{n: n, ok: m[2] == "ok", rc: rc, errMsg: strings.TrimSpace(m[4])})
		}
		return out
	}
	eventually(t, 15*time.Minute, "the VM's creates on the share", func() (bool, string) {
		p := probes()
		ok := 0
		for _, x := range p {
			if x.ok {
				ok++
			}
		}
		return ok >= 5, fmt.Sprint(p)
	})
	if !strings.Contains(c.cli("logs", "vm", "mounter"), "hangar-fs probe") {
		t.Error("hangar logs lacks the VM's probes its serial log has")
	}
	booted := vmMachine().GetStatus().GetBootTime().AsTime()
	vmAddr, _, _ := strings.Cut(vmMachine().GetSpec().GetInterfaces()[0].GetIpv4(), "/")

	before := len(probes())
	c.applyYAML(vm("[]"))
	revoked := time.Now()
	var first vmProbe
	eventually(t, 30*time.Second, "a create of the VM failing", func() (bool, string) {
		p := probes()
		for _, x := range p[before:] {
			if !x.ok {
				first = x
				return true, ""
			}
		}
		return false, fmt.Sprint(p[before:])
	})
	t.Logf("the VM's creates failed %v after its declaration was removed, the first %+v", time.Since(revoked).Round(time.Millisecond), first)
	if d := time.Since(revoked); d > 10*time.Second {
		t.Errorf("the VM kept the share %v after its declaration was removed", d)
	}
	if !first.refused() {
		t.Errorf("the VM's first failed create, %+v, was not refused by the server (EACCES or EPERM at once)", first)
	}
	eventually(t, 3*time.Minute, "ten more of the VM's creates", func() (bool, string) {
		p := probes()
		return p[len(p)-1].n >= first.n+10, fmt.Sprint(p[len(p)-1])
	})
	kinds := map[string]int{}
	leaked := false
	for _, x := range probes() {
		if x.n <= first.n {
			continue
		}
		switch {
		case x.ok:
			leaked = true
			t.Errorf("the VM's create %d succeeded after it lost the share", x.n)
		case x.refused():
			kinds["refused by the server"]++
		case x.rc == 137:
			kinds["cut off, killed at its 3 s timeout"]++
		default:
			kinds[fmt.Sprintf("rc=%d %s", x.rc, x.errMsg)]++
		}
	}
	t.Logf("the VM's creates after the first failure: %v", kinds)
	mustSh(t, "writer", "echo still > /data/after-revocation")
	if now := vmMachine().GetStatus().GetBootTime().AsTime(); !now.Equal(booted) {
		t.Errorf("the VM was started again, at %v, its shares changing", now)
	}
	// The mutation runs whichever way the VM lost the share, which the
	// check above may fail on, unless it did not lose it at all.
	if leaked {
		return
	}

	// The mutation: nothing puts back what it does.
	writerAddr := c.addrIn("", "writer", 0)
	srvUID := c.serverMachine("data").GetMetadata().GetUid()
	srvTap := c.serverTap("data", "default")
	c.killAgent()
	t.Run("a create succeeds again with the VM let back in by hand", func(t *testing.T) {
		// First the server's tap alone, the export left as the agent made
		// it: what the server itself then says to the VM is logged.
		letThrough(t, srvTap.netns, "hangar", srvTap.name)
		letThrough(t, srvTap.netns, "hangar_sg", srvTap.name)
		from := len(probes())
		var seen string
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) && seen == "" {
			for _, x := range probes()[from:] {
				if x.ok {
					t.Errorf("the server served the VM it no longer exports: create %+v", x)
				}
				if x.refused() || x.ok {
					seen = fmt.Sprintf("%+v", x)
				}
			}
			time.Sleep(time.Second)
		}
		t.Logf("with the server's tap let through and the VM not in its export, its create: %q", seen)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		export := driver.ShareExport{Squash: driver.SquashNone, Clients: []driver.ShareClient{
			{Address: netip.MustParseAddr(writerAddr)}, {Address: netip.MustParseAddr(vmAddr)},
		}}
		if err := guest().SetExport(ctx, srvUID, export); err != nil {
			t.Fatal(err)
		}
		from = len(probes())
		eventually(t, 2*time.Minute, "a create of the VM succeeding", func() (bool, string) {
			p := probes()
			for _, x := range p[from:] {
				if x.ok {
					return true, ""
				}
			}
			return false, fmt.Sprint(p[len(p)-1])
		})
	})
}
