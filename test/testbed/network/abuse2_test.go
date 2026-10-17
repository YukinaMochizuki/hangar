//go:build testbed

package network

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"net/netip"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/internal/node/natlog"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// invalidCounters count, in the upstream before its conntrack, the TCP
// TestInvalidNeverLeaves sends by family: SYN with FIN, which conntrack
// finds invalid, and a plain SYN.
const invalidCounters = `
table inet tbinvalid {
	counter synfin4 {}
	counter synfin6 {}
	counter syn4 {}
	counter syn6 {}
	chain pre {
		type filter hook prerouting priority -300; policy accept;
		meta nfproto ipv4 tcp dport 9997 tcp flags & (fin | syn) == fin | syn counter name synfin4
		meta nfproto ipv6 tcp dport 9997 tcp flags & (fin | syn) == fin | syn counter name synfin6
		meta nfproto ipv4 tcp dport 9997 tcp flags == syn counter name syn4
		meta nfproto ipv6 tcp dport 9997 tcp flags == syn counter name syn6
	}
}
`

// rawTCPScript sends from a raw socket, for each family, three TCP
// segments with SYN and FIN set, then one with SYN alone, to port 9997.
const rawTCPScript = `import socket,struct
def csum(b):
    if len(b)%%2: b+=b"\0"
    s=sum(struct.unpack("!%%dH"%%(len(b)//2),b))
    while s>>16: s=(s&0xffff)+(s>>16)
    return (~s)&0xffff
def seg(fam,src,dst,flags):
    h=struct.pack("!HHIIBBHHH",40555,9997,1,0,5<<4,flags,64240,0,0)
    a=socket.inet_pton(fam,src)+socket.inet_pton(fam,dst)
    ph=a+(struct.pack("!BBH",0,6,len(h)) if fam==socket.AF_INET else struct.pack("!I3xB",len(h),6))
    return h[:16]+struct.pack("!H",csum(ph+h))+h[18:]
for fam,src,dst in ((socket.AF_INET,%q,%q),(socket.AF_INET6,%q,%q)):
    s=socket.socket(fam,socket.SOCK_RAW,socket.IPPROTO_TCP)
    for flags in (3,3,3,2):
        s.sendto(seg(fam,src,dst,flags),(dst,0))
`

// TestInvalidNeverLeaves: TCP conntrack finds invalid, SYN with FIN, from a
// network never reaches the upstream, over IPv6, where the host drops it,
// or IPv4, where the network's NAT leaves it to the host's reverse-path
// filter; a plain SYN from the same socket does.
func TestInvalidNeverLeaves(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr), IPv6Prefix: prefix6, PublicIPv6: netip.MustParseAddr(hostAddr6)})
	g := dualStackGuest(t, d, netA, "a1")
	c := exec.Command("ip", "netns", "exec", upstreamNS, "nft", "-f", "-")
	c.Stdin = strings.NewReader(invalidCounters)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("counters: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("ip", "netns", "exec", upstreamNS, "nft", "delete", "table", "inet", "tbinvalid").Run()
	})
	run(t, "ip", "netns", "exec", g.ns, "python3", "-c", fmt.Sprintf(rawTCPScript,
		g.nic.Address.Addr().String(), upstreamAddr, g.nic.IPv6.String(), upstreamAddr6))
	counted := func(name string) int {
		out := run(t, "ip", "netns", "exec", upstreamNS, "nft", "list", "counter", "inet", "tbinvalid", name)
		var n int
		if i := strings.Index(out, "packets "); i >= 0 {
			_, _ = fmt.Sscanf(out[i:], "packets %d", &n)
		}
		return n
	}
	// The plain SYNs, sent last, have arrived: so would have the others.
	testutil.EventuallyWithin(t, 10*time.Second, "the plain SYN of each family at the upstream", func() bool {
		return counted("syn4") > 0 && counted("syn6") > 0
	})
	for _, fam := range []string{"4", "6"} {
		if n := counted("synfin" + fam); n != 0 {
			t.Errorf("IPv%s: %d invalid segments reached the upstream", fam, n)
		}
	}
}

// TestTheKernelFiltersEvents: the host's own connections never reach the
// agent's socket, only those of networks, which the socket filter passes.
func TestTheKernelFiltersEvents(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	made := ensure(t, d, []driver.NetworkSpec{netA}, nic("a1", netA, 2))
	g := newGuest(t, made[0])
	flows := make(chan driver.NATFlow, 1024)
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- d.FollowNAT(ctx, driver.NATHandler{
			Started: func(time.Time) { close(started) },
			Flow:    func(f driver.NATFlow) { flows <- f },
			Lost:    func(l driver.NATLost) { t.Errorf("lost: %+v", l) },
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-errc; err != nil {
			t.Errorf("FollowNAT: %v", err)
		}
	})
	<-started
	before := hostnet.EventsRead()
	// 100 connections of the host's own, each from a socket of its own.
	run(t, "python3", "-c", fmt.Sprintf(`import socket
for i in range(100):
    s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.sendto(b"x",(%q,9999)); s.close()`, upstreamAddr))
	// Then the guest's, which comes after them on the socket.
	run(t, "ip", "netns", "exec", g.ns, "curl", "-sS", "-m", "3", upstreamURL)
	select {
	case f := <-flows:
		if f.NetworkUID != netA.UID {
			t.Errorf("reported %+v, want a1's connection", f)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a1's connection not reported")
	}
	if n := hostnet.EventsRead() - before; n != 1 {
		t.Errorf("%d events read for the host's 100 connections and a1's one, want a1's alone", n)
	}
}

const (
	natlogDataset = "hangar/testbed-natlog"
	natlogDir     = "/var/tmp/hangar-testbed-natlog"
)

// fill writes 8 MiB files named by name, in dir, until the dataset's quota
// refuses one, and returns how many it wrote whole; then smaller ones,
// named apart, until the quota refuses even 4 KiB.
func fill(t *testing.T, dir string, name func(i int) string) int {
	t.Helper()
	whole := 0
	for _, size := range []int{8 << 20, 1 << 20, 64 << 10, 4 << 10} {
		chunk := make([]byte, size)
		_, _ = rand.Read(chunk)
		for i := 0; ; i++ {
			n := name(i)
			if size != 8<<20 {
				n = fmt.Sprintf("rest-%d-%d", size, i)
			}
			err := os.WriteFile(filepath.Join(dir, n), chunk, 0o600)
			if errors.Is(err, syscall.EDQUOT) || errors.Is(err, syscall.ENOSPC) {
				_ = os.Remove(filepath.Join(dir, n))
				run(t, "sync", "-f", dir)
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if i > 256 {
				t.Fatal("the quota refuses nothing")
			}
			if size == 8<<20 {
				whole++
			}
		}
	}
	return whole
}

// TestNATLogOnItsDataset: on a dataset with a quota, as deploy/host makes
// hangar/natlog, a write the quota refuses removes the oldest day, or more
// while it is over 90%, and the log says how many days it holds; with
// nothing left to remove, writes fail, said once, and the gap is written
// once there is room again.
func TestNATLogOnItsDataset(t *testing.T) {
	if exec.Command("zpool", "list", "hangar").Run() != nil {
		t.Skip("no pool hangar (test/testbed/setup.sh makes it)")
	}
	_ = exec.Command("zfs", "destroy", "-r", natlogDataset).Run()
	run(t, "zfs", "create", "-o", "refquota=64M", "-o", "compression=on", "-o", "mountpoint="+natlogDir, natlogDataset)
	// Root's alone before anything is written, as deploy/host makes it.
	run(t, "chmod", "700", natlogDir)
	t.Cleanup(func() { _ = exec.Command("zfs", "destroy", "-r", natlogDataset).Run() })

	day := func(i int) string { return fmt.Sprintf("nat-2026-01-%02d.jsonl", i+1) }
	old := fill(t, natlogDir, day)
	if old < 4 {
		t.Fatalf("only %d days fit under the quota", old)
	}
	var told []bool
	now := time.Now()
	l, err := natlog.Open(natlogDir, natlog.Options{Failing: func(f bool, _ error) { told = append(told, f) }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close("test") }()
	used, total, err := statfs(natlogDir)
	// ZFS refuses a write somewhat short of the quota: what statfs says
	// may be under the cap, as it was at 88%.
	if err != nil || total > 80<<20 {
		t.Fatalf("the dataset is %d of %d bytes (%v): statfs does not see the quota", used, total, err)
	}
	t.Logf("filled: %d old days, %d of %d bytes used", old, used, total)
	// The quota refuses it, and the oldest day goes. If ZFS frees that
	// day's space later than the write is tried again, the write fails,
	// said once, and a later one writes the gap.
	first := l.Write(natlog.Record{Time: now, Event: natlog.EventNew, PublicIP: hostAddr, PublicPort: 40123})
	if first != nil {
		// What holds the space: files removed but open, ours or anyone's.
		t.Logf("refused after the trim: %v\n%s", first, sh("ls -la "+natlogDir+"; echo our fds:; ls -l /proc/"+strconv.Itoa(os.Getpid())+"/fd | grep "+natlogDir+"; echo deleted, open:; lsof +L1 2>/dev/null | grep "+natlogDir))
	}
	refused := time.Now()
	if first != nil {
		testutil.EventuallyWithin(t, 30*time.Second, "a write once ZFS frees the removed day", func() (bool, string) {
			err := l.Write(natlog.Record{Time: time.Now(), Event: natlog.EventNew, PublicIP: hostAddr, PublicPort: 40124})
			return err == nil, fmt.Sprintf("%v; %s", err, sh("zfs get -Hp -o value used,available "+natlogDataset+" | tr '\\n' ' '; lsof +L1 2>/dev/null | grep "+natlogDir))
		})
		t.Logf("room made %s after the trim", time.Since(refused).Round(100*time.Millisecond))
	}
	late := len(told) > 0
	t.Logf("made room %s", map[bool]string{false: "at once", true: "once ZFS freed the removed day"}[late])
	used, total, _ = statfs(natlogDir)
	left := 0
	for i := range old {
		if _, err := os.Stat(filepath.Join(natlogDir, day(i))); err == nil {
			left++
		} else if i == old-1 {
			t.Errorf("the newest old day went before an older one")
		}
	}
	t.Logf("%d of %d old days left, %d of %d bytes used", left, old, used, total)
	// One day of 8 MiB covers the 10% of 64 MiB past the cap; writes
	// refused while ZFS frees it take no more.
	if left != old-1 {
		t.Errorf("%d of %d old days left, want the oldest alone gone", left, old)
	}
	today := now.UTC().Format("2006-01-02")
	if b := natText(t, natlogDir, today); late && !strings.Contains(b, `"event":"gap"`) {
		t.Errorf("writes failed for a while, but no gap in today's file: %s", b)
	}
	// Written as the day went, or held until a write was made.
	if b := natText(t, natlogDir, today); !strings.Contains(b, `"event":"retention"`) {
		t.Errorf("no retention record in today's file: %s", b)
	}

	// Nothing left to remove but today's: writes fail, said once.
	told = nil
	for i := range old {
		_ = os.Remove(filepath.Join(natlogDir, day(i)))
	}
	fill(t, natlogDir, func(i int) string { return fmt.Sprintf("filler-%d", i) })
	for i := range 3 {
		if err := l.Write(natlog.Record{Time: now.Add(time.Duration(i) * time.Second), Event: natlog.EventNew, PublicPort: 40200}); err == nil {
			// The quota lets a little past it; fill what is left.
			fill(t, natlogDir, func(i int) string { return fmt.Sprintf("filler-more-%d", i) })
		}
	}
	for _, glob := range []string{"filler-*", "rest-*"} {
		matches, _ := filepath.Glob(filepath.Join(natlogDir, glob))
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
	run(t, "sync", "-f", natlogDir)
	// ZFS may free what was removed a moment later.
	testutil.EventuallyWithin(t, 30*time.Second, "writing with room again", func() (bool, string) {
		err := l.Write(natlog.Record{Time: now.Add(time.Minute), Event: natlog.EventNew, PublicPort: 40300})
		return err == nil, fmt.Sprint(err)
	})
	if len(told) != 2 || !told[0] || told[1] {
		t.Errorf("told %v, want failing once, then working once", told)
	}
	lines := strings.Split(strings.TrimSpace(natText(t, natlogDir, today)), "\n")
	if n := len(lines); n < 2 || !strings.Contains(lines[n-2], `"event":"gap"`) || !strings.Contains(lines[n-1], `"public_port":40300`) {
		t.Errorf("today's file ends %q, want the gap, then the connection written once there was room", lines[max(0, len(lines)-2):])
	}
}

// natText is what the log in dir holds of a day, its hours in order, each
// one's compressed file before its plain one.
func natText(t *testing.T, dir, day string) string {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(dir, "nat-"+day+"*"))
	key := func(p string) string {
		if strings.HasSuffix(p, ".gz") {
			return strings.TrimSuffix(p, ".gz") + "0"
		}
		return p + "1"
	}
	slices.SortFunc(paths, func(a, b string) int { return strings.Compare(key(a), key(b)) })
	var out strings.Builder
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		var r io.Reader = f
		if strings.HasSuffix(p, ".gz") {
			if r, err = gzip.NewReader(f); err != nil {
				t.Fatalf("%s: %v", p, err)
			}
		}
		_, _ = io.Copy(&out, r)
		_ = f.Close()
	}
	return out.String()
}

// sh is what a shell line prints, errors included.
func sh(line string) string {
	out, _ := exec.Command("sh", "-c", line).CombinedOutput()
	return string(out)
}

func statfs(dir string) (used, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	total = st.Blocks * uint64(st.Bsize)
	return total - st.Bavail*uint64(st.Bsize), total, nil
}
