//go:build testbed

package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/encoding/protojson"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// The isolation of FileShares through the whole system
// (docs/decisions/file-shares.md): who may mount, forged sources, readOnly,
// revocation, a server that initiates nothing, a share on two networks, an
// egress-locked sandbox, overlapping networks and the quota. Each test can
// be seen to fail: a control succeeds beside each refusal, and some end
// with the property broken by hand, the agent killed so nothing puts it
// back, and what they asserted then failing (docs/runbook/testbed.md).

// ctYAML is a busybox container running script, with the interfaces (a
// YAML list, or "" for the default) and volumes (YAML flow maps) given.
func ctYAML(name, script, interfaces string, volumes ...string) string {
	y := fmt.Sprintf(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: Container
metadata: {name: %s}
spec:
  image: %s
  command: ["/bin/sh", "-c", %q]
  resources: {cpu: 1, memory: 256Mi, disk: 1Gi}
`, name, busyboxRef, script)
	if interfaces != "" {
		y += "  interfaces: " + interfaces + "\n"
	}
	if len(volumes) > 0 {
		y += "  volumes:\n"
		for _, v := range volumes {
			y += "  - " + v + "\n"
		}
	}
	return y
}

// shareVolume declares a share at /data.
func shareVolume(share string, readOnly bool) string {
	return fmt.Sprintf("{source: %s, sourceKind: FileShare, target: /data, readOnly: %t}", share, readOnly)
}

// responderScript answers TCP and UDP on ports 7 and 2049 by echoing what
// it is sent, over IPv6 where the guest has it and IPv4 otherwise, and
// idles.
const responderScript = `trap 'exit 0' TERM
for p in 7 2049; do
  (tcpsvd -E :: $p cat 2>/dev/null || tcpsvd -E 0 $p cat) &
  (udpsvd -E :: $p cat 2>/dev/null || udpsvd -E 0 $p cat) &
done
echo ready
while :; do sleep 1 & wait $!; done`

// inProject is the CLI's flag naming the project, or none for the
// context's.
func inProject(proj string) []string {
	if proj == "" {
		return nil
	}
	return []string{"-p", proj}
}

// sh runs a shell script in a container of the project ("" the context's),
// returning its output and exit code.
func sh(t *testing.T, proj, ct, script string) (string, int) {
	t.Helper()
	out, errOut, code := execCLI(t, nil, append(inProject(proj), "ct", ct, "--", "sh", "-c", script)...)
	return out + errOut, code
}

// mustSh is sh, failing the test unless the script exits 0.
func mustSh(t *testing.T, ct, script string) string {
	t.Helper()
	out, code := sh(t, "", ct, script)
	if code != 0 {
		t.Fatalf("in %s, %q: exit %d: %s", ct, script, code, out)
	}
	return out
}

// connects says whether the container makes a TCP connection to
// host:port within 3 s.
func connects(t *testing.T, proj, ct, host, port string) (bool, string) {
	t.Helper()
	out, code := sh(t, proj, ct, fmt.Sprintf("nc -w 3 %s %s </dev/null", host, port))
	return code == 0, fmt.Sprintf("tcp %s:%s from %s: exit %d: %s", host, port, ct, code, out)
}

// udpAnswered says whether a datagram the container sends to host:port is
// answered within 2 s.
func udpAnswered(t *testing.T, proj, ct, host, port string) (bool, string) {
	t.Helper()
	out, code := sh(t, proj, ct, fmt.Sprintf("echo hangar-probe | nc -u -w 2 %s %s", host, port))
	return strings.Contains(out, "hangar-probe"), fmt.Sprintf("udp %s:%s from %s: exit %d: %s", host, port, ct, code, out)
}

// pings says whether the container's ping of host is answered.
func pings(t *testing.T, proj, ct, host string) (bool, string) {
	t.Helper()
	out, code := sh(t, proj, ct, "ping -c 1 -W 2 "+host)
	return code == 0, fmt.Sprintf("ping %s from %s: exit %d: %s", host, ct, code, out)
}

// mountsByHand says whether the container, as root in its guest, mounts
// the share at addr itself, as a VM does, and lists it.
func mountsByHand(t *testing.T, proj, ct, addr string) (bool, string) {
	t.Helper()
	out, code := sh(t, proj, ct, fmt.Sprintf(
		"mkdir -p /mnt/byhand && timeout -s KILL 30 mount -t nfs4 -o vers=4.2,soft,timeo=30,retrans=1,addr=%s %s:/ /mnt/byhand && ls /mnt/byhand >/dev/null; r=$?; umount /mnt/byhand 2>/dev/null; exit $r", addr, addr))
	return code == 0, fmt.Sprintf("mount %s:/ from %s: exit %d: %s", addr, ct, code, out)
}

// addrIn is the IPv4 of a container's interface n in a project.
func (c *cluster) addrIn(proj, ct string, n int) string {
	c.t.Helper()
	var m computev1alpha1.Machine
	eventually(c.t, time.Minute, "container "+ct+"'s address", func() (bool, string) {
		out, err := c.tryCLI(nil, append(inProject(proj), "get", "machine", "ct-"+ct, "-o", "json")...)
		if err != nil {
			return false, err.Error()
		}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(out), &m); err != nil {
			return false, err.Error()
		}
		return len(m.GetSpec().GetInterfaces()) > n && m.GetSpec().GetInterfaces()[n].GetIpv4() != "", m.GetSpec().String()
	})
	a, _, _ := strings.Cut(m.GetSpec().GetInterfaces()[n].GetIpv4(), "/")
	return a
}

// serverTap is the share's server's tap on a network's namespace.
func (c *cluster) serverTap(share, network string) tap {
	c.t.Helper()
	ns := "hn-" + c.network(network).GetMetadata().GetUid()
	for _, tp := range tapsOf(c.t, c.serverMachine(share).GetMetadata().GetUid()) {
		if tp.netns == ns {
			return tp
		}
	}
	c.t.Fatalf("share %s's server has no tap on %s", share, network)
	return tap{}
}

// ctTap is a container's tap.
func (c *cluster) ctTap(ct string) tap {
	c.t.Helper()
	tps := tapsOf(c.t, c.machine(ct).GetMetadata().GetUid())
	if len(tps) == 0 {
		c.t.Fatalf("container %s has no tap", ct)
	}
	return tps[0]
}

// tcpCapture is tcpdump on a tap, in immediate mode, printing each frame
// as it comes.
type tcpCapture struct {
	cmd *exec.Cmd
	out *lockedBuffer
}

// capture starts tcpdump on the tap, for the frames matching filter that
// pass it the way given, "out" to its guest or "in" from it.
func capture(t *testing.T, tp tap, way, filter string) *tcpCapture {
	t.Helper()
	cmd := exec.Command("ip", "netns", "exec", tp.netns, "tcpdump", "--immediate-mode", "-i", tp.name, "-nn", "-l", "-e", "-Q", way, filter)
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "tcpdump listening on "+tp.name, func() (bool, string) {
		return strings.Contains(out.String(), "listening on"), out.String()
	})
	return &tcpCapture{cmd: cmd, out: out}
}

// count stops the capture once send has sent a sentinel frame, which the
// filter takes and sentinel matches as tcpdump prints it, and tcpdump has
// printed it: every frame that passed before it is in the capture then. It
// returns the frames before the sentinel that counted matches, and all
// tcpdump printed.
func (c *tcpCapture) count(t *testing.T, send func(), sentinel, counted *regexp.Regexp) (int, string) {
	t.Helper()
	defer func() { _ = c.cmd.Process.Signal(syscall.SIGINT); _ = c.cmd.Wait() }()
	send()
	eventually(t, 10*time.Second, "the sentinel frame in the capture", func() (bool, string) {
		return sentinel.MatchString(c.out.String()), c.out.String()
	})
	n := 0
	for _, l := range strings.Split(c.out.String(), "\n") {
		if sentinel.MatchString(l) {
			break
		}
		if counted.MatchString(l) {
			n++
		}
	}
	return n, c.out.String()
}

// sendARP sends, on a network's bridge from its namespace, an ARP request
// from the gateway for addr: a frame the bridge floods to every tap, a
// server's included, whatever the taps' rules.
func sendARP(t *testing.T, ns, gateway, addr string) {
	t.Helper()
	js := mustRun(t, "ip", "-n", ns, "-j", "link", "show", "hn-br")
	var links []struct {
		Index   int    `json:"ifindex"`
		Address string `json:"address"`
	}
	if err := json.Unmarshal([]byte(js), &links); err != nil || len(links) != 1 {
		t.Fatalf("hn-br in %s: %v: %s", ns, err, js)
	}
	mac, err := net.ParseMAC(links[0].Address)
	if err != nil {
		t.Fatal(err)
	}
	frame := slices.Concat([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, mac, []byte{0x08, 0x06},
		[]byte{0, 1, 0x08, 0x00, 6, 4, 0, 1}, mac, netip.MustParseAddr(gateway).AsSlice(),
		make([]byte, 6), netip.MustParseAddr(addr).AsSlice())
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
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, 0)
	if serr := netns.Set(orig); serr != nil {
		t.Fatal(serr)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Sendto(fd, frame, 0, &unix.SockaddrLinklayer{Ifindex: links[0].Index, Protocol: 0x0608}); err != nil {
		t.Fatal(err)
	}
}

// lockedBuffer is a buffer a process writes while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// hostSh runs a shell script in a machine's guest through its agent's
// socket on the host, as root, with no node agent needed, returning its
// output and exit code; -1 if it could not be run.
func hostSh(t *testing.T, uid, script string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, code, err := execIn(ctx, uid, "/bin/sh", "-c", script)
	if err != nil {
		return fmt.Sprintf("%s: %v", out, err), -1
	}
	return out, int(code)
}

// killAgent kills the node agent for the mutations at a test's end, so it
// puts back nothing they break; its VMs run on.
func (c *cluster) killAgent() {
	c.t.Helper()
	if installed {
		c.t.Skip("the mutations kill the agent, which an installed run's systemd starts again")
	}
	c.killNode()
}

// TestWhoMayMountAShare: of the machines that declare nothing, none on
// another network of the project, nor in another project on the same range
// at a client's very address, reaches the share's 2049 or mounts it, and a
// client declaring it reaches it on the network it mounts from alone. A
// guest forging a client's address on its interface gets nothing to the
// server: the bridge's binding drops it before the server's tap. A client
// declaring the share readOnly cannot write, even remounted read-write as
// root. A running container whose declaration is removed is started again
// without it and reaches the share no more. The server cannot be exec'd
// into. Then, with the agent killed, the binding removed lets the forged
// frames reach the server, and an export made read-write by hand lets the
// reader write.
func TestWhoMayMountAShare(t *testing.T) {
	const other = "flow2"
	c := startCluster(t)
	c.applyYAML(`apiVersion: network.hangarlab.cloud/v1alpha1
kind: Network
metadata: {name: backend}
spec: {ipv4Cidr: 10.1.0.0/24}
---
apiVersion: network.hangarlab.cloud/v1alpha1
kind: Network
metadata: {name: elsewhere}
spec: {ipv4Cidr: 10.2.0.0/24}
`)
	c.applyYAML(shareYAML("data", "1Gi", "default", "backend"))
	at, atB := c.shareAddr("data", "default"), c.shareAddr("data", "backend")
	c.applyYAML(ctYAML("alpha", idleScript, "", shareVolume("data", false)))
	alpha := c.addrIn("", "alpha", 0)
	c.applyYAML(ctYAML("gamma", idleScript, ""))
	c.applyYAML(ctYAML("reader", idleScript, "", shareVolume("data", true)))
	c.applyYAML(ctYAML("stranger", idleScript, "[{network: elsewhere}]"))
	c.applyYAML(ctYAML("twohomed", idleScript, "[{network: default}, {network: backend}]", shareVolume("data", false)))
	c.applyYAML(ctYAML("bpeer", idleScript, "[{network: backend}]"))

	// Another project of the tenant, whose default network has the same
	// range: mimic takes alpha's address, the lowest free, once x1 holds
	// the one below it, which then goes, leaving the server's address free
	// there too.
	if out, err := c.tryCLI(strings.NewReader(fmt.Sprintf(`apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Project
metadata: {name: %s}
spec: {tenant: %s}
`, other, tenant)), "apply", "-f", "-"); err != nil {
		t.Fatalf("making project %s: %v: %s", other, err, out)
	}
	applyIn := func(doc string) {
		t.Helper()
		if out, err := c.tryCLI(strings.NewReader(doc), "-p", other, "apply", "-f", "-"); err != nil {
			t.Fatalf("hangar -p %s apply: %v: %s\n%s", other, err, out, doc)
		}
	}
	applyIn(ctYAML("x1", idleScript, ""))
	c.addrIn(other, "x1", 0)
	applyIn(ctYAML("mimic", idleScript, ""))
	if got := c.addrIn(other, "mimic", 0); got != alpha {
		t.Fatalf("mimic in %s has %s, not alpha's %s", other, got, alpha)
	}
	c.cli("-p", other, "delete", "container", "x1")
	c.cli("-p", other, "wait", "container", "x1", "--for=delete", "--timeout=3m")

	for _, n := range []string{"alpha", "gamma", "reader", "stranger", "twohomed", "bpeer"} {
		c.waitReady(n, 3*time.Minute)
	}
	if _, err := c.tryCLI(nil, "-p", other, "wait", "container", "mimic", "--for=condition=Ready", "--timeout=3m"); err != nil {
		t.Fatalf("mimic never Ready: %v", err)
	}
	c.waitSharePhase("data", storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_IN_USE)

	// The controls: a client reaches its share's 2049 and mounts it by
	// hand; twohomed mounts it from default, the first of its networks
	// the share is on.
	if ok, last := connects(t, "", "alpha", at, "2049"); !ok {
		t.Fatalf("a client does not reach its share's 2049: %s", last)
	}
	if ok, last := mountsByHand(t, "", "alpha", at); !ok {
		t.Fatalf("a client cannot mount its share by hand: %s", last)
	}
	if out := mustSh(t, "twohomed", "cat /proc/mounts"); !strings.Contains(out, at+":/ /data nfs4") {
		t.Errorf("twohomed does not mount the share from default, at %s:\n%s", at, out)
	}
	// And its backend interface works: it reaches bpeer there, by eth1.
	bpeer := c.addrIn("", "bpeer", 0)
	if out := mustSh(t, "twohomed", "ip route get "+bpeer); !strings.Contains(out, "dev eth1") {
		t.Errorf("twohomed does not reach backend by eth1: %s", out)
	}
	if ok, last := pings(t, "", "twohomed", bpeer); !ok {
		t.Fatalf("control: twohomed does not reach bpeer on backend: %s", last)
	}

	t.Run("who may mount", func(t *testing.T) {
		for _, x := range []struct{ proj, ct, addr, why string }{
			{"", "gamma", at, "a machine of the share's network declaring nothing"},
			{"", "stranger", at, "a machine on another network of the project"},
			{"", "stranger", atB, "a machine on another network of the project"},
			{other, "mimic", at, "a machine of another project at a client's address"},
			{"", "twohomed", atB, "a client, on the network it does not mount from"},
		} {
			if ok, last := connects(t, x.proj, x.ct, x.addr, "2049"); ok {
				t.Errorf("%s reaches the server's 2049: %s", x.why, last)
			}
			if ok, last := mountsByHand(t, x.proj, x.ct, x.addr); ok {
				t.Errorf("%s mounts the share: %s", x.why, last)
			}
		}
	})

	t.Run("readOnly", func(t *testing.T) {
		mustSh(t, "alpha", "echo from-alpha > /data/alpha.txt")
		if out := mustSh(t, "reader", "cat /data/alpha.txt"); strings.TrimSpace(out) != "from-alpha" {
			t.Errorf("reader reads %q, not alpha's write", out)
		}
		if out, code := sh(t, "", "reader", "touch /data/reader.txt 2>&1"); code == 0 {
			t.Errorf("reader wrote to a share it declares readOnly: %s", out)
		}
		mustSh(t, "reader", "mount -o remount,rw /data && grep ' /data ' /proc/mounts | grep -q ' rw,'")
		if out, code := sh(t, "", "reader", "touch /data/reader.txt 2>&1"); code == 0 || !strings.Contains(out, "Read-only") {
			t.Errorf("reader, remounted read-write, wrote, or failed otherwise (exit %d): %s", code, out)
		}
	})

	// gamma, declaring nothing, takes the address of reader, a client
	// throughout, and sends to the server's 2049 from it, the server's MAC
	// set by hand: its own ARP for the server is a non-client's, which the
	// server's tap does not take.
	victim := c.addrIn("", "reader", 0)
	srvTap := c.serverTap("data", "default")
	gammaTap := c.ctTap("gamma")
	forge := func(run func(script string) (string, int)) {
		t.Helper()
		setUp := fmt.Sprintf("ip addr add %s/32 dev eth0 2>/dev/null; arp -i eth0 -s %s %s", victim, at, srvTap.mac)
		if out, code := run(setUp); code != 0 {
			t.Fatalf("in gamma, %q: exit %d: %s", setUp, code, out)
		}
		for range 3 {
			run(fmt.Sprintf("nc -s %s -w 2 %s 2049 </dev/null", victim, at))
		}
	}
	viaCLI := func(ct string) func(string) (string, int) {
		return func(script string) (string, int) { return sh(t, "", ct, script) }
	}
	// Each capture ends on a sentinel frame sent after the forged ones:
	// from gamma, a ping of its gateway from its own address; to the
	// server, an ARP request the bridge floods, from an address no one has.
	forged := fmt.Sprintf("ether src %s and src host %s and tcp dst port 2049", gammaTap.mac, victim)
	isForged := regexp.MustCompile(regexp.QuoteMeta(victim) + `\.\d+ > ` + regexp.QuoteMeta(at) + `\.2049: Flags \[S\]`)
	gammaAddr := c.addrIn("", "gamma", 0)
	const arpSender = "10.0.0.254"
	toServer := func() *tcpCapture {
		return capture(t, srvTap, "out", "("+forged+") or (arp and arp[14:4] = 0x0a0000fe)")
	}
	serverSentinel := regexp.MustCompile("Request who-has " + regexp.QuoteMeta(at) + " tell " + arpSender)
	sendServerSentinel := func() { sendARP(t, srvTap.netns, arpSender, at) }
	t.Run("forged sources", func(t *testing.T) {
		sent := capture(t, gammaTap, "in", "("+forged+") or (icmp and src host "+gammaAddr+")")
		got := toServer()
		forge(viaCLI("gamma"))
		if n, out := sent.count(t, func() { sh(t, "", "gamma", "ping -c 1 -W 1 -I "+gammaAddr+" 10.0.0.1") },
			regexp.MustCompile(regexp.QuoteMeta(gammaAddr)+" > 10.0.0.1: ICMP echo request"), isForged); n == 0 {
			t.Errorf("control: gamma sent no forged frame:\n%s", out)
		}
		if n, out := got.count(t, sendServerSentinel, serverSentinel, isForged); n != 0 {
			t.Errorf("the server's tap passed %d frames gamma forged as reader:\n%s", n, out)
		}
	})

	t.Run("revocation", func(t *testing.T) {
		booted := c.machine("alpha").GetStatus().GetBootTime().AsTime()
		c.applyYAML(ctYAML("alpha", idleScript, ""))
		eventually(t, 3*time.Minute, "alpha started again without its share", func() (bool, string) {
			m := c.machine("alpha")
			return m.GetStatus().GetBootTime().AsTime().After(booted) && m.GetStatus().GetPhase() == computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetStatus().String()
		})
		c.waitReady("alpha", 3*time.Minute)
		if out := mustSh(t, "alpha", "cat /proc/mounts"); strings.Contains(out, " nfs4 ") {
			t.Errorf("alpha, its declaration removed, still mounts a share:\n%s", out)
		}
		if ok, last := connects(t, "", "alpha", at, "2049"); ok {
			t.Errorf("alpha, its declaration removed, reaches the server's 2049: %s", last)
		}
		if ok, last := mountsByHand(t, "", "alpha", at); ok {
			t.Errorf("alpha, its declaration removed, mounts the share: %s", last)
		}
	})

	srv := c.serverMachine("data").GetMetadata().GetUid()
	t.Run("no exec into the server", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if out, code, err := execIn(ctx, srv, "/init"); err == nil || !strings.Contains(err.Error(), "unimplemented") {
			t.Errorf("Exec into the server: exit %d, %v: %s", code, err, out)
		}
	})
	if t.Failed() {
		return
	}

	// The mutations: nothing puts back what they break.
	// The agent killed, hangar exec has no node to go through: the host
	// execs in the guests itself.
	gammaUID, readerUID := c.machine("gamma").GetMetadata().GetUid(), c.machine("reader").GetMetadata().GetUid()
	twohomedAddr := c.addrIn("", "twohomed", 0)
	c.killAgent()
	t.Run("forged sources pass without the binding", func(t *testing.T) {
		// A tap with no rules sends nothing: gamma's is let through ahead of
		// its rules instead.
		letThrough(t, gammaTap.netns, "hangar", gammaTap.name)
		got := toServer()
		forge(func(script string) (string, int) { return hostSh(t, gammaUID, script) })
		if n, out := got.count(t, sendServerSentinel, serverSentinel, isForged); n == 0 {
			t.Errorf("with gamma's binding removed, its forged frames still never reach the server's tap: the check cannot fail:\n%s", out)
		}
	})
	t.Run("readOnly fails once the export is read-write", func(t *testing.T) {
		clients := []driver.ShareClient{
			{Address: netip.MustParseAddr(victim)},
			{Address: netip.MustParseAddr(twohomedAddr)},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := guest().SetExport(ctx, srv, driver.ShareExport{Clients: clients, Squash: driver.SquashNone}); err != nil {
			t.Fatal(err)
		}
		eventually(t, 30*time.Second, "reader writing, its export read-write", func() (bool, string) {
			out, code := hostSh(t, readerUID, "touch /data/reader.txt 2>&1")
			return code == 0, out
		})
	})
}

// TestAShareOnTwoNetworks: a share on two networks is mounted from each by
// a client on it, and each reads the other's files; neither client reaches
// the other, directly, through the server's address on its own network
// routed to by hand, or at the server's ports but 2049 or its address on
// the other network. An egress-locked sandbox declaring the share mounts
// it and reaches nothing else. A share on two overlapping networks is
// refused, as made and as a network is added. Past the plan's fileShares,
// or its disk because of a share's size, a share is refused; with the plan
// raised, the same share is taken. The server boots from the testbed
// initramfs and listens on the ports but 2049 the clients try, with IPv4
// forwarding on, so that what refuses them is the host's firewall, not a
// closed port or stack: the last mutation, the server's and the clients'
// taps let through by hand, lets a reach the server's 2050 and b through
// it.
func TestAShareOnTwoNetworks(t *testing.T) {
	probeInitrd := vmtest.Artifacts().ServerProbeInitramfs
	if _, err := os.Stat(probeInitrd); err != nil {
		t.Fatalf("%v (make guest-initramfs-testbed builds it)", err)
	}
	c := startClusterWith(t, func(c *cluster) {
		c.nodeArgs = []string{"--guest-initramfs", probeInitrd}
	})
	c.applyYAML(`apiVersion: network.hangarlab.cloud/v1alpha1
kind: Network
metadata: {name: backend}
spec: {ipv4Cidr: 10.1.0.0/24}
---
apiVersion: network.hangarlab.cloud/v1alpha1
kind: Network
metadata: {name: inside}
spec: {ipv4Cidr: 10.1.0.128/25}
---
apiVersion: network.hangarlab.cloud/v1alpha1
kind: SecurityGroup
metadata: {name: locked}
spec:
  network: default
  egress:
  - {protocol: SECURITY_GROUP_PROTOCOL_ICMP, to: {cidr: 192.0.2.1/32}}
`)
	c.applyYAML(shareYAML("twonets", "1Gi", "default", "backend"))
	at, atB := c.shareAddr("twonets", "default"), c.shareAddr("twonets", "backend")
	c.applyYAML(ctYAML("a", responderScript, "", shareVolume("twonets", false)))
	c.applyYAML(ctYAML("b", responderScript, "[{network: backend}]", shareVolume("twonets", false)))
	c.applyYAML(ctYAML("b2", idleScript, "[{network: backend}]"))
	c.applyYAML(ctYAML("plain", idleScript, ""))
	c.applyYAML(ctYAML("sandbox", idleScript, "[{network: default, securityGroups: [locked]}]", shareVolume("twonets", false)))
	for _, n := range []string{"a", "b", "b2", "plain", "sandbox"} {
		c.waitReady(n, 3*time.Minute)
	}
	a, b := c.addrIn("", "a", 0), c.addrIn("", "b", 0)
	srv := c.serverMachine("twonets").GetMetadata().GetUid()
	listen := []string{"tcp:22", "tcp:111", "tcp:2048", "tcp:2050", "udp:2049"}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		out, code, err := execIn(ctx, srv, append([]string{"/init", "netlisten", "-forward"}, listen...)...)
		if ctx.Err() == nil {
			t.Errorf("netlisten in the server ended: exit %d, %v: %s", code, err, out)
		}
	}()
	// The control: the server answers on each, from itself.
	var local []string
	for _, l := range listen {
		proto, port, _ := strings.Cut(l, ":")
		local = append(local, proto+":127.0.0.1:"+port)
	}
	eventually(t, 30*time.Second, "the server listening on "+strings.Join(listen, ", "), func() (bool, string) {
		res := netprobe(t, srv, local...)
		for _, l := range local {
			if res[l] != "ok" {
				return false, fmt.Sprint(res)
			}
		}
		return true, ""
	})

	t.Run("both mount and read each other's files", func(t *testing.T) {
		for ct, want := range map[string]string{"a": at, "b": atB, "sandbox": at} {
			if out := mustSh(t, ct, "cat /proc/mounts"); !strings.Contains(out, want+":/ /data nfs4") {
				t.Errorf("%s does not mount the share at %s:\n%s", ct, want, out)
			}
		}
		mustSh(t, "a", "echo from-a > /data/a.txt")
		mustSh(t, "b", "echo from-b > /data/b.txt")
		mustSh(t, "sandbox", "echo from-sandbox > /data/sandbox.txt")
		for ct, files := range map[string][]string{"a": {"b", "sandbox"}, "b": {"a", "sandbox"}, "sandbox": {"a", "b"}} {
			for _, f := range files {
				eventually(t, 30*time.Second, ct+" reading "+f+"'s file", func() (bool, string) {
					out, code := sh(t, "", ct, "cat /data/"+f+".txt")
					return code == 0 && strings.TrimSpace(out) == "from-"+f, out
				})
			}
		}
	})

	t.Run("no other path between the networks", func(t *testing.T) {
		// The controls: b's responders answer on its network, and a
		// reaches the server's 2049.
		for _, check := range []func() (bool, string){
			func() (bool, string) { return connects(t, "", "b2", b, "7") },
			func() (bool, string) { return connects(t, "", "b2", b, "2049") },
			func() (bool, string) { return udpAnswered(t, "", "b2", b, "7") },
			func() (bool, string) { return pings(t, "", "b2", b) },
			func() (bool, string) { return connects(t, "", "a", at, "2049") },
			func() (bool, string) { return connects(t, "", "b", atB, "2049") },
		} {
			if ok, last := check(); !ok {
				t.Fatalf("a control fails: %s", last)
			}
		}
		blocked := func(why string, ok bool, last string) {
			t.Helper()
			if ok {
				t.Errorf("%s: %s", why, last)
			}
		}
		for _, x := range []struct{ from, to string }{{"a", b}, {"b", a}} {
			for _, port := range []string{"7", "2049"} {
				ok, last := connects(t, "", x.from, x.to, port)
				blocked(x.from+" reaches the other network's client", ok, last)
				ok, last = udpAnswered(t, "", x.from, x.to, port)
				blocked(x.from+" reaches the other network's client", ok, last)
			}
			ok, last := pings(t, "", x.from, x.to)
			blocked(x.from+" reaches the other network's client", ok, last)
		}
		for _, x := range []struct{ from, near, far string }{{"a", at, atB}, {"b", atB, at}} {
			for _, port := range []string{"22", "111", "2048", "2050"} {
				ok, last := connects(t, "", x.from, x.near, port)
				blocked(x.from+" reaches the server on a port but 2049", ok, last)
			}
			ok, last := udpAnswered(t, "", x.from, x.near, "2049")
			blocked(x.from+" reaches the server over UDP", ok, last)
			ok, last = pings(t, "", x.from, x.near)
			blocked(x.from+" pings the server", ok, last)
			for _, port := range []string{"2049", "22"} {
				ok, last = connects(t, "", x.from, x.far, port)
				blocked(x.from+" reaches the server's address on the other network", ok, last)
			}
			ok, last = pings(t, "", x.from, x.far)
			blocked(x.from+" pings the server's address on the other network", ok, last)
		}
		// Through the server: the other network routed to by hand through
		// its address on a's own.
		mustSh(t, "a", "ip route replace 10.1.0.0/24 via "+at)
		for _, port := range []string{"7", "2049"} {
			ok, last := connects(t, "", "a", b, port)
			blocked("a reaches b through the server", ok, last)
			ok, last = udpAnswered(t, "", "a", b, port)
			blocked("a reaches b through the server", ok, last)
		}
		ok, last := pings(t, "", "a", b)
		blocked("a reaches b through the server", ok, last)
		mustSh(t, "a", "ip route del 10.1.0.0/24")
	})

	t.Run("an egress-locked sandbox reaches its share alone", func(t *testing.T) {
		// The controls: plain, on the same network unlocked, reaches each.
		targets := []struct {
			what  string
			probe func(ct string) (bool, string)
		}{
			{"a's TCP", func(ct string) (bool, string) { return connects(t, "", ct, a, "7") }},
			{"a's UDP", func(ct string) (bool, string) { return udpAnswered(t, "", ct, a, "7") }},
			{"a's ping", func(ct string) (bool, string) { return pings(t, "", ct, a) }},
			{"the internet's ping", func(ct string) (bool, string) { return pings(t, "", ct, "1.1.1.1") }},
			{"the internet's HTTP", func(ct string) (bool, string) {
				out, code := sh(t, "", ct, "wget -q -T 5 -O- http://cloudflare.com/cdn-cgi/trace")
				return code == 0, out
			}},
		}
		for _, x := range targets {
			if ok, last := x.probe("plain"); !ok {
				t.Fatalf("control: plain does not reach %s: %s", x.what, last)
			}
			if ok, last := x.probe("sandbox"); ok {
				t.Errorf("the sandbox reaches %s: %s", x.what, last)
			}
		}
		for _, port := range []string{"22", "2050"} {
			if ok, last := connects(t, "", "sandbox", at, port); ok {
				t.Errorf("the sandbox reaches the server's %s: %s", port, last)
			}
		}
		if ok, last := connects(t, "", "sandbox", atB, "2049"); ok {
			t.Errorf("the sandbox reaches the server's address on the other network: %s", last)
		}
	})

	t.Run("overlapping networks are refused", func(t *testing.T) {
		out, err := c.tryCLI(strings.NewReader(shareYAML("overlap", "1Gi", "backend", "inside")), "apply", "-f", "-")
		if err == nil || !strings.Contains(err.Error(), "overlap") {
			t.Errorf("a share on backend and inside, which overlap, was not refused for it: %v: %s", err, out)
		}
		out, err = c.tryCLI(strings.NewReader(shareYAML("twonets", "1Gi", "default", "backend", "inside")), "apply", "-f", "-")
		if err == nil || !strings.Contains(err.Error(), "overlap") {
			t.Errorf("adding inside, which overlaps backend, to a share was not refused for it: %v: %s", err, out)
		}
		if got := c.share("twonets").GetSpec().GetNetworks(); len(got) != 2 {
			t.Errorf("the refused share's networks are %v", got)
		}
	})

	refused := func(doc, why string) {
		t.Helper()
		out, err := c.tryCLI(strings.NewReader(doc), "apply", "-f", "-")
		if err == nil || !strings.Contains(err.Error(), "quota exceeded") || !strings.Contains(err.Error(), why) {
			t.Errorf("not refused for %s: %v: %s\n%s", why, err, out, doc)
		}
	}
	t.Run("quota", func(t *testing.T) {
		// 5 containers' 1Gi and 1Gi of twonets: a share of 95Gi is past the
		// disk limit of 100Gi.
		c.applyYAML(shareYAML("q2", "1Gi", "default"))
		refused(shareYAML("q3", "95Gi", "default"), "disk")
		c.applyYAML(shareYAML("q3", "1Gi", "default"))
		refused(shareYAML("q4", "1Gi", "default"), "file_shares")
		refused(shareYAML("q3", "95Gi", "default"), "disk")
	})

	// The mutation: the plan raised, the same shares are taken.
	t.Run("taken with the plan raised", func(t *testing.T) {
		c.applyYAML(`apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Plan
metadata: {name: default}
spec: {limits: {disk: 300Gi, fileShares: 10}}
`)
		c.applyYAML(shareYAML("q4", "1Gi", "default"))
		c.applyYAML(shareYAML("q5", "95Gi", "default"))
		c.cli("delete", "fileshare", "q5")
		c.cli("wait", "fileshare", "q5", "--for=delete", "--timeout=3m")
	})
	if t.Failed() {
		return
	}

	// The last mutation: the server's taps, and a's and b's, let through by
	// hand ahead of their bindings and their groups, with routes through
	// the server set on both sides, the agent killed so nothing puts them
	// back. The host execs in the guests itself.
	aUID, bUID := c.machine("a").GetMetadata().GetUid(), c.machine("b").GetMetadata().GetUid()
	taps := []tap{c.serverTap("twonets", "default"), c.serverTap("twonets", "backend"), c.ctTap("a"), c.ctTap("b")}
	c.killAgent()
	t.Run("with the taps let through, a reaches the server's other ports and b through it", func(t *testing.T) {
		for _, tp := range taps {
			letThrough(t, tp.netns, "hangar", tp.name)
			letThrough(t, tp.netns, "hangar_sg", tp.name)
		}
		for uid, route := range map[string]string{aUID: "10.1.0.0/24 via " + at, bUID: "10.0.0.0/24 via " + atB} {
			if out, code := hostSh(t, uid, "ip route replace "+route); code != 0 {
				t.Fatalf("ip route replace %s: exit %d: %s", route, code, out)
			}
		}
		for _, x := range []struct{ what, script string }{
			{"the server's 2050", "nc -w 3 " + at + " 2050 </dev/null"},
			{"the server's UDP 2049", "echo hangar-probe | nc -u -w 2 " + at + " 2049 | grep -q hangar-probe"},
			{"the server's ping", "ping -c 1 -W 2 " + at},
			{"b's TCP, through the server", "nc -w 3 " + b + " 7 </dev/null"},
			{"b's ping, through the server", "ping -c 1 -W 2 " + b},
		} {
			ok := false
			var out string
			for range 3 {
				var code int
				if out, code = hostSh(t, aUID, x.script); code == 0 {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("with the taps let through, a still does not reach %s: %s: the check cannot fail", x.what, out)
			}
		}
	})
}
