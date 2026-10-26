//go:build testbed

package network

import (
	"bufio"
	"fmt"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"testing"

	mdnetlink "github.com/mdlayher/netlink"
	"github.com/ti-mo/conntrack"
	"github.com/vishvananda/netns"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

// fromGuest asks addr:port over proto ("tcp" or "udp") from guest g, and
// returns the label of the guest that answered and the address it saw the
// client from; ok is false for no answer.
func fromGuest(g *guest, proto string, addr netip.Addr, port uint16) (label string, seen netip.AddrPort, ok bool) {
	script := `import socket,sys
proto,host,port=sys.argv[1],sys.argv[2],int(sys.argv[3])
if proto=="tcp":
    s=socket.create_connection((host,port),timeout=1.5); s.send(b"?"); print(s.recv(128).decode())
else:
    s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.settimeout(1.5); s.sendto(b"?",(host,port)); print(s.recv(128).decode())
`
	out, err := exec.Command("ip", "netns", "exec", g.ns, "python3", "-c", script, proto, addr.String(), fmt.Sprint(port)).Output()
	if err != nil {
		return "", netip.AddrPort{}, false
	}
	label, at, found := strings.Cut(strings.TrimSpace(string(out)), " ")
	seen, perr := netip.ParseAddrPort(at)
	return label, seen, found && perr == nil
}

// holdGuestClient opens a socket over proto to addr:port from guest g,
// until the test ends.
func holdGuestClient(t *testing.T, g *guest, proto string, addr netip.Addr, port uint16) *heldClient {
	t.Helper()
	cmd := exec.Command("ip", "netns", "exec", g.ns, "python3", "-c", clientScript, proto, addr.String(), fmt.Sprint(port))
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
	return &heldClient{what: fmt.Sprintf("%s socket from %s to %s:%d", proto, g.ns, addr, port), in: in, out: bufio.NewReader(out)}
}

// nsTransit is the namespace end of a network's transit link, the address
// its traffic leaves its namespace as.
func nsTransit(n driver.NetworkSpec) netip.Addr { return n.Transit.Addr().Next() }

// endpointTo is network n's endpoint at addr to guest g's port, through
// transit port transit of g's network.
func endpointTo(n driver.NetworkSpec, proto driver.ForwardProtocol, addr string, g *guest, port, transit uint16) driver.Endpoint {
	return driver.Endpoint{
		Protocol: proto, NetworkUID: n.UID, Addr: netip.MustParseAddr(addr), Port: port,
		ServiceNetworkUID: g.nic.NetworkUID, TransitPort: transit, To: netip.AddrPortFrom(g.nic.Address.Addr(), port),
	}
}

func ensureEndpoints(t *testing.T, d *hostnet.Driver, es ...driver.Endpoint) {
	t.Helper()
	if err := d.EnsureEndpoints(t.Context(), es); err != nil {
		t.Fatal(err)
	}
}

// endpointTables lists, with their handles, the root namespace's static
// table and endpoint table, and the networks' endpoint tables: a table
// written again has other handles.
func endpointTables(t *testing.T, nets ...driver.NetworkSpec) string {
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
	list("nft", "-a", "list", "table", "ip", hostnet.EndpointTable)
	for _, n := range nets {
		list("ip", "netns", "exec", hostnet.NamespaceOf(n.UID), "nft", "-a", "list", "table", "ip", hostnet.EndpointTable)
		list("ip", "-n", hostnet.NamespaceOf(n.UID), "-4", "addr", "show", "dev", "hn-br")
	}
	return b.String()
}

// TestEndpointsReachAnotherNetwork: a guest reaches another network's guest,
// of the same range, at its endpoint's address of its own range, over TCP
// and UDP, and is seen from its network's transit address; no other port of
// the address answers, and a network without an endpoint of the service
// reaches nothing of it, even at the service's transit address. The same
// set ensured again changes no table, and the set reads back as ensured.
func TestEndpointsReachAnotherNetwork(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	made := ensure(t, d, []driver.NetworkSpec{netA, netB, netC}, nic("a1", netA, 2), nic("b1", netB, 2), nic("c1", netC, 2))
	a1, b1, c1 := newGuest(t, made[0]), newGuest(t, made[1]), newGuest(t, made[2])
	whoServer(t, a1, "a1", "tcp/22", "udp/53", "tcp/8080")
	git := endpointTo(netC, driver.TCP, "10.0.0.7", a1, 22, 1024)
	dns := endpointTo(netC, driver.UDP, "10.0.0.7", a1, 53, 1025)
	ensureEndpoints(t, d, git, dns)
	logHost(t)

	ep := git.Addr
	for _, proto := range []string{"tcp", "udp"} {
		port := map[string]uint16{"tcp": 22, "udp": 53}[proto]
		label, seen, ok := fromGuest(c1, proto, ep, port)
		if !ok || label != "a1" || seen.Addr() != nsTransit(netC) {
			t.Errorf("%s %s:%d from c1: answered %t by %q seeing %s, want a1 seeing %s", proto, ep, port, ok, label, seen, nsTransit(netC))
		}
	}
	if label, _, ok := fromGuest(c1, "tcp", ep, 8080); ok {
		t.Errorf("a port of the endpoint's address it does not carry was answered by %s", label)
	}
	if c1.reaches(ep.String()) {
		t.Error("the endpoint's address answers a ping")
	}
	// netA's transit address and port are let in for netC alone.
	if label, _, ok := fromGuest(b1, "tcp", nsTransit(netA), 1024); ok {
		t.Errorf("a network without an endpoint of the service reached it at its transit address, answered by %s", label)
	}
	if label, _, ok := fromGuest(b1, "tcp", netip.MustParseAddr("10.1.0.7"), 22); ok {
		t.Errorf("an address of another network's endpoint was answered by %s in netB", label)
	}

	got, err := d.Endpoints(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []driver.Endpoint{git, dns}
	driver.SortEndpoints(want)
	if !slices.Equal(got, want) {
		t.Errorf("the endpoints read back as %+v, want %+v", got, want)
	}
	before := endpointTables(t, netA, netC)
	ensureEndpoints(t, d, dns, git)
	if err := d.EnsureNetwork(t.Context(), netC); err != nil {
		t.Fatal(err)
	}
	if after := endpointTables(t, netA, netC); after != before {
		t.Errorf("the same endpoints ensured again, with their network, changed the host:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	ensureEndpoints(t, d)
	if label, _, ok := fromGuest(c1, "tcp", ep, 22); ok {
		t.Errorf("a removed endpoint was answered by %s", label)
	}
	if out := endpointTables(t, netC); strings.Contains(out, ep.String()) {
		t.Errorf("a removed endpoint's address or rules remain:\n%s", out)
	}
}

// TestAnEndpointIsJudgedByTheGroups: what comes in through an endpoint is
// judged by the service's machine's security groups, from its network's
// transit address.
func TestAnEndpointIsJudgedByTheGroups(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	made := ensure(t, d, []driver.NetworkSpec{netA, netC}, nic("a1", netA, 2), nic("c1", netC, 2))
	a1, c1 := newGuest(t, made[0]), newGuest(t, made[1])
	whoServer(t, a1, "a1", "tcp/22")
	ensureEndpoints(t, d, endpointTo(netC, driver.TCP, "10.0.0.7", a1, 22, 1024))
	from := func(peer netip.Addr) {
		t.Helper()
		if err := d.EnsureFirewall(t.Context(), netA.UID, []driver.InterfacePolicy{{MachineUID: "a1", Name: "eth0", Policy: secgroup.Policy{
			Ingress:   []secgroup.Rule{{Protocol: secgroup.TCP, Ports: []secgroup.PortRange{{From: 22, To: 22}}, Peers: []netip.Prefix{netip.PrefixFrom(peer, 32)}}},
			EgressAll: true,
		}}}); err != nil {
			t.Fatal(err)
		}
	}
	from(nsTransit(netC))
	if label, _, ok := fromGuest(c1, "tcp", netip.MustParseAddr("10.0.0.7"), 22); !ok || label != "a1" {
		t.Errorf("the groups admit netC, and c1 was answered %t by %q", ok, label)
	}
	from(nsTransit(netB))
	if label, _, ok := fromGuest(c1, "tcp", netip.MustParseAddr("10.0.0.7"), 22); ok {
		t.Errorf("the groups admit netB alone, and c1 was answered by %s", label)
	}
}

// udpSources runs, in guest g until stop is called, a listener on UDP port
// that notes the source address of each datagram it takes; stop returns
// them.
func udpSources(t *testing.T, g *guest, port uint16) (stop func() []string) {
	t.Helper()
	// The listener reads its datagrams on a thread, and stops when its
	// stdin closes.
	script := `import socket,sys,threading
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(("0.0.0.0",int(sys.argv[1])))
def rd():
    while True:
        d,a=s.recvfrom(64); print(a[0],flush=True)
threading.Thread(target=rd,daemon=True).start()
sys.stdin.read()
`
	cmd := exec.Command("ip", "netns", "exec", g.ns, "python3", "-c", script, fmt.Sprint(port))
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	listening(t, g.ns, fmt.Sprintf("udp/%d", port))
	return func() []string {
		_ = in.Close()
		_ = cmd.Wait()
		return strings.Fields(out.String())
	}
}

// sendUDPFrom sends three datagrams from the upstream, from source src, to
// addr:port.
func sendUDPFrom(t *testing.T, src, addr string, port uint16) {
	t.Helper()
	script := `import socket,sys,time
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind((sys.argv[1],0))
for _ in range(3):
    s.sendto(b"?",(sys.argv[2],int(sys.argv[3]))); time.sleep(0.1)
`
	run(t, "ip", "netns", "exec", upstreamNS, "python3", "-c", script, src, addr, fmt.Sprint(port))
}

// TestNoTransitSourceComesInByTheUplink: a datagram from the internet with
// a network's transit address as its source, which a group's network peer
// trusts, does not reach a machine through a port forward, where one from
// the upstream's own address does.
func TestNoTransitSourceComesInByTheUplink(t *testing.T) {
	d := holdShared(t)
	made := ensure(t, d, []driver.NetworkSpec{netA, netC}, nic("a1", netA, 2), nic("c1", netC, 2))
	a1 := newGuest(t, made[0])
	stop := udpSources(t, a1, 5353)
	peer := func(a string) netip.Prefix { return netip.PrefixFrom(netip.MustParseAddr(a), 32) }
	if err := d.EnsureFirewall(t.Context(), netA.UID, []driver.InterfacePolicy{{MachineUID: "a1", Name: "eth0", Policy: secgroup.Policy{
		Ingress: []secgroup.Rule{{Protocol: secgroup.UDP, Ports: []secgroup.PortRange{{From: 5353, To: 5353}},
			Peers: []netip.Prefix{peer(nsTransit(netC).String()), peer(upstreamAddr)}}},
		EgressAll: true,
	}}}); err != nil {
		t.Fatal(err)
	}
	ensureForwards(t, d, forwardTo(a1, driver.UDP, 10053, 5353))
	forged := nsTransit(netC).String()
	run(t, "ip", "-n", upstreamNS, "addr", "add", forged+"/32", "dev", "tbupx")
	t.Cleanup(func() { _ = exec.Command("ip", "-n", upstreamNS, "addr", "del", forged+"/32", "dev", "tbupx").Run() })
	sendUDPFrom(t, forged, sharedAddr, 10053)
	sendUDPFrom(t, upstreamAddr, sharedAddr, 10053)
	got := stop()
	if slices.Contains(got, forged) || !slices.Contains(got, upstreamAddr) {
		t.Errorf("a1 took datagrams from %v: want %s's, the control, and none from the forged %s", got, upstreamAddr, forged)
	}
}

// TestARevokedEndpointGoesWhateverAnotherNetwork: a network's way to a
// service is taken away even while another network's namespace cannot be
// written, which the ensure then reports.
func TestARevokedEndpointGoesWhateverAnotherNetwork(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	made := ensure(t, d, []driver.NetworkSpec{netA, netB, netC}, nic("a1", netA, 2), nic("b1", netB, 2), nic("c1", netC, 2))
	a1, c1 := newGuest(t, made[0]), newGuest(t, made[2])
	whoServer(t, a1, "a1", "tcp/22")
	fromB := endpointTo(netB, driver.TCP, "10.1.0.7", a1, 22, 1024)
	fromC := endpointTo(netC, driver.TCP, "10.0.0.7", a1, 22, 1024)
	ensureEndpoints(t, d, fromB, fromC)
	if label, _, ok := fromGuest(c1, "tcp", fromC.Addr, 22); !ok || label != "a1" {
		t.Fatalf("c1 through its endpoint: answered %t by %q", ok, label)
	}
	// netB's namespace loses its bridge, so its endpoint's address cannot
	// be held.
	run(t, "ip", "-n", hostnet.NamespaceOf(netB.UID), "link", "del", "hn-br")
	if err := d.EnsureEndpoints(t.Context(), []driver.Endpoint{fromB}); err == nil {
		t.Error("the endpoints were ensured with a network's bridge gone")
	}
	if label, _, ok := fromGuest(c1, "tcp", nsTransit(netA), 1024); ok {
		t.Errorf("c1, whose endpoint was taken away, reached the service's transit port, answered by %s", label)
	}
}

// endpointFlowsFrom counts the connections an endpoint let in, in network
// n's namespace, from src.
func endpointFlowsFrom(t *testing.T, n driver.NetworkSpec, src netip.Addr) int {
	t.Helper()
	ns, err := netns.GetFromName(hostnet.NamespaceOf(n.UID))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ns.Close() }()
	c, err := conntrack.Dial(&mdnetlink.Config{NetNS: int(ns)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	flows, err := c.DumpFilter(conntrack.NewFilter().Mark(1<<29).MarkMask(1<<29), nil)
	if err != nil {
		t.Fatal(err)
	}
	k := 0
	for _, f := range flows {
		if f.TupleOrig.IP.SourceAddress == src {
			k++
		}
	}
	return k
}

// TestARemovedNetworksFlowsEndAtTheService: with two networks reaching one
// service, the service's namespace ends the flows of the one whose endpoint
// is removed, and keeps the other's.
func TestARemovedNetworksFlowsEndAtTheService(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	made := ensure(t, d, []driver.NetworkSpec{netA, netB, netC}, nic("a1", netA, 2), nic("b1", netB, 2), nic("c1", netC, 2))
	a1, b1, c1 := newGuest(t, made[0]), newGuest(t, made[1]), newGuest(t, made[2])
	whoServer(t, a1, "a1", "udp/53")
	fromB := endpointTo(netB, driver.UDP, "10.1.0.7", a1, 53, 1024)
	fromC := endpointTo(netC, driver.UDP, "10.0.0.7", a1, 53, 1024)
	ensureEndpoints(t, d, fromB, fromC)
	b, c := holdGuestClient(t, b1, "udp", fromB.Addr, 53), holdGuestClient(t, c1, "udp", fromC.Addr, 53)
	if gb, gc := b.ask(), c.ask(); gb != "a1" || gc != "a1" {
		t.Fatalf("b1 answered by %q, c1 by %q, want a1", gb, gc)
	}
	if fb, fc := endpointFlowsFrom(t, netA, nsTransit(netB)), endpointFlowsFrom(t, netA, nsTransit(netC)); fb != 1 || fc != 1 {
		t.Fatalf("the service's namespace holds %d of netB's flows and %d of netC's, want 1 each", fb, fc)
	}
	ensureEndpoints(t, d, fromB)
	if fb, fc := endpointFlowsFrom(t, netA, nsTransit(netB)), endpointFlowsFrom(t, netA, nsTransit(netC)); fb != 1 || fc != 0 {
		t.Errorf("once netC's endpoint went, the service's namespace holds %d of netB's flows and %d of netC's, want 1 and 0", fb, fc)
	}
	if got := b.ask(); got != "a1" {
		t.Errorf("b1's socket answered by %q once netC's endpoint went, want a1", got)
	}
}

// TestOpenFlowsEndWithTheirEndpoint: a TCP connection and a UDP socket kept
// open through an endpoint never reach the machine it led to once it leads
// to another, nor anything once it is removed.
func TestOpenFlowsEndWithTheirEndpoint(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	made := ensure(t, d, []driver.NetworkSpec{netA, netC}, nic("a1", netA, 2), nic("a2", netA, 3), nic("c1", netC, 2))
	a1, a2, c1 := newGuest(t, made[0]), newGuest(t, made[1]), newGuest(t, made[2])
	whoServer(t, a1, "a1", "tcp/22", "udp/53")
	whoServer(t, a2, "a2", "tcp/22", "udp/53")
	to := func(g *guest) []driver.Endpoint {
		return []driver.Endpoint{endpointTo(netC, driver.TCP, "10.0.0.7", g, 22, 1024), endpointTo(netC, driver.UDP, "10.0.0.7", g, 53, 1025)}
	}
	ep := netip.MustParseAddr("10.0.0.7")
	ensureEndpoints(t, d, to(a1)...)
	tcp, udp := holdGuestClient(t, c1, "tcp", ep, 22), holdGuestClient(t, c1, "udp", ep, 53)
	if got := tcp.ask(); got != "a1" {
		t.Fatalf("%s answered by %q, want a1", tcp.what, got)
	}
	if got := udp.ask(); got != "a1" {
		t.Fatalf("%s answered by %q, want a1", udp.what, got)
	}
	ensureEndpoints(t, d, to(a2)...)
	if got := tcp.ask(); got == "a1" {
		t.Errorf("%s still answered by a1 after the endpoint moved to a2", tcp.what)
	}
	if got := udp.ask(); got != "a2" {
		t.Errorf("%s answered by %q after the endpoint moved, want a2", udp.what, got)
	}
	ensureEndpoints(t, d)
	if got := udp.ask(); got != "none" {
		t.Errorf("%s answered by %q after the endpoint was removed", udp.what, got)
	}
}
