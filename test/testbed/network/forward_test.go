//go:build testbed

package network

import (
	"bufio"
	"fmt"
	"io"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// holdSharedAddr holds the testbed's shared address on the uplink, as
// deploy/host does, until the test ends.
func holdSharedAddr(t *testing.T) {
	t.Helper()
	run(t, "ip", "addr", "add", sharedAddr+"/32", "dev", upstreamLink, "preferred_lft", "0")
	t.Cleanup(func() {
		_ = exec.Command("ip", "addr", "del", sharedAddr+"/32", "dev", upstreamLink).Run()
		// The upstream counts the shared address's replies as foreign; the
		// tests after this one start from none.
		count(t, upstreamNS, "foreign")
	})
}

// holdShared holds the shared address, and returns a driver with it.
func holdShared(t *testing.T) *hostnet.Driver {
	t.Helper()
	holdSharedAddr(t)
	return newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr), SharedIPv4: netip.MustParseAddr(sharedAddr)})
}

// whoServer serves, in a guest until the test ends, on each of ports such
// as "tcp/8080" or "udp/5353", an answer of label and the address and port
// a connection or datagram came from, to each message a client sends.
func whoServer(t *testing.T, g *guest, label string, ports ...string) {
	t.Helper()
	script := `import socket,sys,threading
label=sys.argv[1]
def conn(c,a):
    while True:
        try: d=c.recv(64)
        except OSError: d=b""
        if not d: return c.close()
        c.sendall(("%s %s:%d"%(label,a[0],a[1])).encode())
def tcp(p):
    s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(("0.0.0.0",p)); s.listen(50)
    while True:
        c,a=s.accept(); threading.Thread(target=conn,args=(c,a),daemon=True).start()
def udp(p):
    s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(("0.0.0.0",p))
    while True:
        d,a=s.recvfrom(64); s.sendto(("%s %s:%d"%(label,a[0],a[1])).encode(),a)
for spec in sys.argv[2:]:
    proto,p=spec.split("/")
    threading.Thread(target=tcp if proto=="tcp" else udp,args=(int(p),),daemon=True).start()
threading.Event().wait()
`
	cmd := exec.Command("ip", append([]string{"netns", "exec", g.ns, "python3", "-c", script, label}, ports...)...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	listening(t, g.ns, ports...)
}

// listening waits until a socket listens in namespace ns on each of ports,
// such as "tcp/8080", as ss lists them.
func listening(t *testing.T, ns string, ports ...string) {
	t.Helper()
	testutil.Eventually(t, fmt.Sprintf("%s listening in %s", ports, ns), func() (bool, string) {
		for _, spec := range ports {
			proto, port, _ := strings.Cut(spec, "/")
			out, err := exec.Command("ip", "netns", "exec", ns, "ss", "-Hln", "--"+proto).Output()
			if err != nil {
				return false, err.Error()
			}
			if !slices.ContainsFunc(strings.Fields(string(out)), func(f string) bool { return strings.HasSuffix(f, ":"+port) }) {
				return false, spec + " not yet: " + string(out)
			}
		}
		return true, ""
	})
}

// clientScript is a client on the internet of a guest's whoServer: given
// proto, host and port, it opens one socket, and for each line it reads
// sends a message on it and prints the label of the guest that answered,
// or "none".
const clientScript = `import socket,sys
proto,host,port=sys.argv[1],sys.argv[2],int(sys.argv[3])
if proto=="tcp":
    s=socket.create_connection((host,port),timeout=1.5)
else:
    s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.settimeout(1.5); s.connect((host,port))
for _ in sys.stdin:
    try:
        s.send(b"?"); d=s.recv(128).decode()
    except OSError:
        d=""
    print(d.split(" ")[0] or "none", flush=True)
`

// fromInternet asks addr:port over proto ("tcp" or "udp") from the
// upstream, as a client on the internet, and returns the label of the
// guest that answered and the address it saw the client from; ok is false
// for no answer.
func fromInternet(proto, addr string, port uint16) (label string, seen netip.AddrPort, ok bool) {
	script := `import socket,sys
proto,host,port=sys.argv[1],sys.argv[2],int(sys.argv[3])
if proto=="tcp":
    s=socket.create_connection((host,port),timeout=1.5); s.send(b"?"); print(s.recv(128).decode())
else:
    s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.settimeout(1.5); s.sendto(b"?",(host,port)); print(s.recv(128).decode())
`
	out, err := exec.Command("ip", "netns", "exec", upstreamNS, "python3", "-c", script, proto, addr, fmt.Sprint(port)).Output()
	if err != nil {
		return "", netip.AddrPort{}, false
	}
	label, at, found := strings.Cut(strings.TrimSpace(string(out)), " ")
	seen, perr := netip.ParseAddrPort(at)
	return label, seen, found && perr == nil
}

// heldClient is one socket of a client on the internet, kept open across
// changes to the forwards.
type heldClient struct {
	what string
	in   io.Writer
	out  *bufio.Reader
}

// holdClient opens a socket over proto to addr:port from the upstream,
// until the test ends.
func holdClient(t *testing.T, proto, addr string, port uint16) *heldClient {
	t.Helper()
	cmd := exec.Command("ip", "netns", "exec", upstreamNS, "python3", "-c", clientScript, proto, addr, fmt.Sprint(port))
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return &heldClient{what: fmt.Sprintf("%s socket to %s:%d", proto, addr, port), in: in, out: bufio.NewReader(out)}
}

// ask sends a message on the client's socket and returns the label of the
// guest that answered it, "none" for no answer.
func (c *heldClient) ask() string {
	if _, err := io.WriteString(c.in, "?\n"); err != nil {
		return "none"
	}
	line, err := c.out.ReadString('\n')
	if err != nil {
		return "none"
	}
	return strings.TrimSpace(line)
}

// reachedFromInternet fails t unless the guest labelled want answers
// addr:port over proto from the internet, seeing the upstream's own
// address.
func reachedFromInternet(t *testing.T, proto, addr string, port uint16, want string) {
	t.Helper()
	label, seen, ok := fromInternet(proto, addr, port)
	if !ok || label != want || seen.Addr().String() != upstreamAddr {
		t.Errorf("%s %s:%d from the internet: answered %t by %q seeing %s, want %q seeing %s", proto, addr, port, ok, label, seen, want, upstreamAddr)
	}
}

// unreachedFromInternet fails t if anything answers addr:port over proto
// from the internet.
func unreachedFromInternet(t *testing.T, proto, addr string, port uint16, why string) {
	t.Helper()
	if label, seen, ok := fromInternet(proto, addr, port); ok {
		t.Errorf("%s %s:%d from the internet was answered by %q seeing %s: %s", proto, addr, port, label, seen, why)
	}
}

func ensureForwards(t *testing.T, d *hostnet.Driver, fs ...driver.Forward) {
	t.Helper()
	if err := d.EnsureForwards(t.Context(), fs); err != nil {
		t.Fatal(err)
	}
}

// forwardTables lists, with their handles, the root namespace's tables of
// family ip, its static one with the way in for forwards among them, and
// the forward tables of the networks' namespaces: a table written again
// has other handles.
func forwardTables(t *testing.T, nets ...driver.NetworkSpec) string {
	t.Helper()
	var b strings.Builder
	list := func(args ...string) {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			fmt.Fprintf(&b, "%v: none\n", args)
			return
		}
		b.Write(out)
	}
	list("nft", "-a", "list", "table", "ip", hostnet.Table)
	list("nft", "-a", "list", "table", "ip", hostnet.ForwardTable)
	for _, n := range nets {
		list("ip", "netns", "exec", hostnet.NamespaceOf(n.UID), "nft", "-a", "list", "table", "ip", hostnet.ForwardTable)
	}
	return b.String()
}

func forwardTo(g *guest, proto driver.ForwardProtocol, port, to uint16) driver.Forward {
	return driver.Forward{Protocol: proto, Port: port, NetworkUID: g.nic.NetworkUID, To: netip.AddrPortFrom(g.nic.Address.Addr(), to)}
}

// TestForwardsReachAGuest: the internet reaches a guest's TCP and UDP
// through the shared address's forwarded ports, and the guest sees the
// client's own address; no other port, nor a forwarded port at the host's
// own address, reaches anything. Removing a forward closes it, and the
// same set ensured again changes no table.
func TestForwardsReachAGuest(t *testing.T) {
	d := holdShared(t)
	g := newGuest(t, ensure(t, d, []driver.NetworkSpec{netA}, nic("a1", netA, 2))[0])
	whoServer(t, g, "a1", "tcp/8080", "udp/5353")
	web, dns := forwardTo(g, driver.TCP, 10080, 8080), forwardTo(g, driver.UDP, 10053, 5353)
	ensureForwards(t, d, web, dns)
	logHost(t)

	reachedFromInternet(t, "tcp", sharedAddr, 10080, "a1")
	reachedFromInternet(t, "udp", sharedAddr, 10053, "a1")
	unreachedFromInternet(t, "tcp", sharedAddr, 10081, "no forward has the port")
	unreachedFromInternet(t, "udp", sharedAddr, 10080, "the port is forwarded for TCP alone")
	unreachedFromInternet(t, "tcp", hostAddr, 10080, "forwards are at the shared address alone")
	unreachedFromInternet(t, "udp", hostAddr, 10053, "forwards are at the shared address alone")

	// Ensured again as it is, the network too, which reads the root
	// table's rules back, the way in for forwards among them, no table is
	// written.
	before := forwardTables(t, netA)
	if err := d.EnsureNetwork(t.Context(), netA); err != nil {
		t.Fatal(err)
	}
	ensureForwards(t, d, dns, web)
	if after := forwardTables(t, netA); after != before {
		t.Errorf("ensuring the same forwards again rewrote the tables:\n%s\nthen\n%s", before, after)
	}

	ensureForwards(t, d, dns)
	unreachedFromInternet(t, "tcp", sharedAddr, 10080, "its forward was removed")
	reachedFromInternet(t, "udp", sharedAddr, 10053, "a1")
	ensureForwards(t, d)
	unreachedFromInternet(t, "udp", sharedAddr, 10053, "every forward was removed")
	if out, err := exec.Command("nft", "list", "table", "ip", hostnet.ForwardTable).CombinedOutput(); err == nil {
		t.Errorf("with no forward the root forward table is left:\n%s", out)
	}
	// The guest still reaches the internet as the shared address.
	if seen, err := g.seenAs(); err != nil || seen != sharedAddr {
		t.Errorf("the guest is seen as %q (%v), want %s", seen, err, sharedAddr)
	}
}

// TestOpenFlowsEndWithTheirForward: a client keeping one TCP connection
// and one UDP socket open reaches, once its forward is moved or removed,
// never the machine it led to before: within a network, to another
// network, and removed. What conntrack held of the old forward is gone
// once EnsureForwards returns, as the node reports the port closed then.
func TestOpenFlowsEndWithTheirForward(t *testing.T) {
	d := holdShared(t)
	made := ensure(t, d, []driver.NetworkSpec{netA, netB}, nic("a1", netA, 2), nic("a2", netA, 3), nic("b1", netB, 2))
	gs := map[string]*guest{}
	for _, m := range made {
		g := newGuest(t, m)
		whoServer(t, g, m.MachineUID, "tcp/8080", "udp/5353")
		gs[m.MachineUID] = g
	}
	to := func(machine string) []driver.Forward {
		return []driver.Forward{forwardTo(gs[machine], driver.TCP, 10080, 8080), forwardTo(gs[machine], driver.UDP, 10053, 5353)}
	}
	want := func(c *heldClient, label, why string) {
		t.Helper()
		if got := c.ask(); got != label {
			t.Errorf("%s answered by %q, want %q: %s", c.what, got, label, why)
		}
	}
	notReaching := func(c *heldClient, label, why string) {
		t.Helper()
		if got := c.ask(); got == label {
			t.Errorf("%s still answered by %s: %s", c.what, label, why)
		}
	}

	ensureForwards(t, d, to("a1")...)
	tcpA1, udp := holdClient(t, "tcp", sharedAddr, 10080), holdClient(t, "udp", sharedAddr, 10053)
	want(tcpA1, "a1", "the forward leads to a1")
	want(udp, "a1", "the forward leads to a1")

	// Within the network: the namespace's flows are what must go.
	ensureForwards(t, d, to("a2")...)
	notReaching(tcpA1, "a1", "the forward moved to a2 on the same network")
	want(udp, "a2", "the same UDP socket follows the forward to a2")
	tcpA2 := holdClient(t, "tcp", sharedAddr, 10080)
	want(tcpA2, "a2", "a new connection reaches a2")

	// To another network: the root namespace's flows lead elsewhere too.
	ensureForwards(t, d, to("b1")...)
	notReaching(tcpA2, "a2", "the forward moved to b1 on another network")
	want(udp, "b1", "the same UDP socket follows the forward to b1")
	tcpB1 := holdClient(t, "tcp", sharedAddr, 10080)
	want(tcpB1, "b1", "a new connection reaches b1")

	// Removed, nothing is reached any more.
	ensureForwards(t, d)
	notReaching(tcpB1, "b1", "every forward was removed")
	want(udp, "none", "every forward was removed")
}

// TestAForwardIsJudgedByTheGroups: what comes in by a forward is judged by
// the interface's security groups from the client's own address, as
// public IPv6 is: a port they do not admit, or a client they do not, gets
// nothing.
func TestAForwardIsJudgedByTheGroups(t *testing.T) {
	d := holdShared(t)
	g := newGuest(t, ensure(t, d, []driver.NetworkSpec{netA}, nic("a1", netA, 2))[0])
	whoServer(t, g, "a1", "tcp/8080", "tcp/9090", "tcp/7070")
	anywhere := netip.MustParsePrefix("0.0.0.0/0")
	elsewhere := netip.MustParsePrefix("203.0.113.0/24")
	if err := d.EnsureFirewall(t.Context(), netA.UID, []driver.InterfacePolicy{{MachineUID: "a1", Name: "eth0", Policy: secgroup.Policy{
		Ingress: []secgroup.Rule{
			{Protocol: secgroup.TCP, Ports: []secgroup.PortRange{{From: 8080, To: 8080}}, Peers: []netip.Prefix{anywhere}},
			{Protocol: secgroup.TCP, Ports: []secgroup.PortRange{{From: 7070, To: 7070}}, Peers: []netip.Prefix{elsewhere}},
		},
		EgressAll: true,
	}}}); err != nil {
		t.Fatal(err)
	}
	ensureForwards(t, d, forwardTo(g, driver.TCP, 10080, 8080), forwardTo(g, driver.TCP, 10090, 9090), forwardTo(g, driver.TCP, 10070, 7070))
	reachedFromInternet(t, "tcp", sharedAddr, 10080, "a1")
	unreachedFromInternet(t, "tcp", sharedAddr, 10090, "the groups do not admit 9090")
	unreachedFromInternet(t, "tcp", sharedAddr, 10070, "the groups admit 7070 from another range than the client's")
}

// TestOverlappingNetworksEachGetTheirForward: two networks of one range,
// with a guest of one address each, each get their own forward.
func TestOverlappingNetworksEachGetTheirForward(t *testing.T) {
	d := holdShared(t)
	made := ensure(t, d, []driver.NetworkSpec{netA, netC}, nic("a1", netA, 2), nic("c1", netC, 2))
	a, c := newGuest(t, made[0]), newGuest(t, made[1])
	if a.nic.Address != c.nic.Address {
		t.Fatalf("the guests are at %s and %s, want one address", a.nic.Address, c.nic.Address)
	}
	whoServer(t, a, "a1", "tcp/8080", "udp/5353")
	whoServer(t, c, "c1", "tcp/8080", "udp/5353")
	ensureForwards(t, d,
		forwardTo(a, driver.TCP, 10080, 8080), forwardTo(c, driver.TCP, 10081, 8080),
		forwardTo(a, driver.UDP, 10053, 5353), forwardTo(c, driver.UDP, 10054, 5353))
	reachedFromInternet(t, "tcp", sharedAddr, 10080, "a1")
	reachedFromInternet(t, "tcp", sharedAddr, 10081, "c1")
	reachedFromInternet(t, "udp", sharedAddr, 10053, "a1")
	reachedFromInternet(t, "udp", sharedAddr, 10054, "c1")
}

// TestNoForwardWithoutASharedAddress: a host without a shared address
// refuses forwards, and takes none.
func TestNoForwardWithoutASharedAddress(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	g := newGuest(t, ensure(t, d, []driver.NetworkSpec{netA}, nic("a1", netA, 2))[0])
	if err := d.EnsureForwards(t.Context(), []driver.Forward{forwardTo(g, driver.TCP, 10080, 8080)}); err == nil {
		t.Error("a host without a shared address took a forward")
	}
	ensureForwards(t, d)
	if out, err := exec.Command("nft", "list", "table", "ip", hostnet.ForwardTable).CombinedOutput(); err == nil {
		t.Errorf("a host without a shared address has a forward table:\n%s", out)
	}
}
