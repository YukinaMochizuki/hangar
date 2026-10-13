//go:build testbed

package network

import (
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

// echoServer serves TCP echo on the ports in a guest until the test ends.
func echoServer(t *testing.T, g *guest, ports ...int) {
	t.Helper()
	script := `import socket,threading
def serve(p):
    s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(("0.0.0.0",p)); s.listen(50)
    while True:
        c,_=s.accept()
        threading.Thread(target=lambda c=c:[c.sendall(d) for d in iter(lambda:c.recv(4096),b"")],daemon=True).start()
for p in [` + strings.Trim(strings.Join(strings.Fields(fmt.Sprint(ports)), ","), "[]") + `]:
    threading.Thread(target=serve,args=(p,),daemon=True).start()
threading.Event().wait()
`
	cmd := exec.Command("ip", "netns", "exec", g.ns, "python3", "-c", script)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	time.Sleep(300 * time.Millisecond)
}

// connects says whether a guest's new TCP connection to addr gets its echo.
func connects(g *guest, addr string, port int) bool {
	script := fmt.Sprintf(`import socket
s=socket.create_connection((%q,%d),timeout=1.5); s.sendall(b"hi"); assert s.recv(2)==b"hi"`, addr, port)
	return exec.Command("ip", "netns", "exec", g.ns, "python3", "-c", script).Run() == nil
}

// udpArrives says whether UDP that from sends from port sport reaches to on
// port dport.
func udpArrives(t *testing.T, from, to *guest, sport, dport int) bool {
	t.Helper()
	recv := exec.Command("ip", "netns", "exec", to.ns, "python3", "-c", fmt.Sprintf(`import socket
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(("0.0.0.0",%d)); s.settimeout(2)
try: print(s.recvfrom(64)[0].decode())
except socket.timeout: pass`, dport))
	var out strings.Builder
	recv.Stdout = &out
	if err := recv.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	send := exec.Command("ip", "netns", "exec", from.ns, "python3", "-c", fmt.Sprintf(`import socket,time
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(("0.0.0.0",%d))
for i in range(5): s.sendto(b"probe",(%q,%d)); time.sleep(0.1)`, sport, to.nic.Address.Addr(), dport))
	if b, err := send.CombinedOutput(); err != nil {
		t.Fatalf("sending UDP: %v: %s", err, b)
	}
	_ = recv.Wait()
	return strings.Contains(out.String(), "probe")
}

// TestFirewall: each interface receives what its policy lets in and sends
// what it lets out, replies always pass, an interface with no policy is
// closed, and a policy changing keeps the connections it had admitted.
func TestFirewall(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	nics := ensure(t, d, []driver.NetworkSpec{netA}, nic("web", netA, 2), nic("plain", netA, 3), nic("sandbox", netA, 4), nic("new", netA, 5), nic("open", netA, 6))
	web, plain, sandbox, unpoliced, open := newGuest(t, nics[0]), newGuest(t, nics[1]), newGuest(t, nics[2]), newGuest(t, nics[3]), newGuest(t, nics[4])
	addr := func(g *guest) string { return g.nic.Address.Addr().String() }
	anywhere := netip.MustParsePrefix("0.0.0.0/0")
	host := func(g *guest) netip.Prefix { return netip.PrefixFrom(g.nic.Address.Addr(), 32) }
	policies := func(webIn []secgroup.Rule) []driver.InterfacePolicy {
		return []driver.InterfacePolicy{
			{MachineUID: "web", Name: "eth0", Policy: secgroup.Policy{Ingress: webIn, EgressAll: true}},
			{MachineUID: "plain", Name: "eth0", Policy: secgroup.Policy{
				Ingress: []secgroup.Rule{{Protocol: secgroup.All, Peers: []netip.Prefix{host(web)}}}, EgressAll: true}},
			{MachineUID: "sandbox", Name: "eth0", Policy: secgroup.Policy{
				Egress: []secgroup.Rule{{Protocol: secgroup.TCP, Ports: []secgroup.PortRange{{From: 8080, To: 8080}}, Peers: []netip.Prefix{host(web)}}}}},
			{MachineUID: "open", Name: "eth0", Policy: secgroup.Policy{
				Ingress: []secgroup.Rule{{Protocol: secgroup.All, Peers: []netip.Prefix{anywhere}}}, EgressAll: true}},
			// "new" has none.
		}
	}
	web8080 := []secgroup.Rule{{Protocol: secgroup.TCP, Ports: []secgroup.PortRange{{From: 8080, To: 8080}}, Peers: []netip.Prefix{anywhere}}}
	if err := d.EnsureFirewall(t.Context(), netA.UID, policies(web8080)); err != nil {
		t.Fatal(err)
	}
	for _, g := range []*guest{web, plain, sandbox, unpoliced} {
		echoServer(t, g, 8080, 9090)
	}
	for _, c := range []struct {
		from, to *guest
		port     int
		want     bool
		why      string
	}{
		{plain, web, 8080, true, "web lets in 8080 from anywhere"},
		{plain, web, 9090, false, "web lets in nothing else"},
		{web, plain, 9090, true, "plain lets in web, on any port"},
		{sandbox, plain, 8080, false, "sandbox sends only to web, and plain lets in web alone"},
		{sandbox, web, 8080, true, "sandbox may send to web on 8080, and its reply comes back though sandbox lets nothing in"},
		{sandbox, web, 9090, false, "sandbox may send to web on 8080 only"},
		{web, sandbox, 8080, false, "sandbox lets nothing in"},
		{unpoliced, web, 8080, false, "an interface with no policy sends nothing"},
		{web, unpoliced, 8080, false, "an interface with no policy receives nothing"},
	} {
		if got := connects(c.from, addr(c.to), c.port); got != c.want {
			t.Errorf("%s -> %s:%d: connected %t, want %t: %s", c.from.nic.MachineUID, c.to.nic.MachineUID, c.port, got, c.want, c.why)
		}
	}
	// The DHCP exceptions let through DHCP alone: from a tap to the
	// broadcast address or the gateway, and to a tap from the gateway.
	for _, c := range []struct {
		from, to     *guest
		sport, dport int
		want         bool
		why          string
	}{
		{web, open, 68, 67, true, "web sends anything, and open lets anything in"},
		{sandbox, open, 68, 67, false, "DHCP-shaped UDP from sandbox to anything but the gateway is no DHCP"},
		{sandbox, open, 40000, 5353, false, "sandbox sends no UDP"},
		{open, sandbox, 67, 68, false, "DHCP-shaped UDP from a guest is no DHCP reply"},
	} {
		if got := udpArrives(t, c.from, c.to, c.sport, c.dport); got != c.want {
			t.Errorf("udp %s:%d -> %s:%d: arrived %t, want %t: %s", c.from.nic.MachineUID, c.sport, c.to.nic.MachineUID, c.dport, got, c.want, c.why)
		}
	}
	if _, err := sandbox.seenAs(); err == nil {
		t.Error("sandbox reached the upstream; it may send only to web")
	}
	if got, err := plain.seenAs(); err != nil || got != hostAddr {
		t.Errorf("plain, sending anything, does not reach the upstream as the host: %q %v", got, err)
	}

	// A connection admitted stays when the rules change under it; a new one
	// follows the new rules.
	live := exec.Command("ip", "netns", "exec", plain.ns, "python3", "-c", fmt.Sprintf(`import socket,time
s=socket.create_connection((%q,8080),timeout=2)
for i in range(40):
    s.sendall(b"x"*10); d=b""
    while len(d)<10: d+=s.recv(10-len(d))
    time.sleep(0.05)`, addr(web)))
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := d.EnsureFirewall(t.Context(), netA.UID, policies(nil)); err != nil {
		t.Fatal(err)
	}
	if err := live.Wait(); err != nil {
		t.Errorf("a connection admitted was cut when the rules changed: %v", err)
	}
	if connects(plain, addr(web), 8080) {
		t.Error("a new connection was let in after the rule allowing it went")
	}

	// What a guest routes through the gateway to another is let in by the
	// same rules as what it sends across the bridge.
	if err := d.EnsureFirewall(t.Context(), netA.UID, policies(web8080)); err != nil {
		t.Fatal(err)
	}
	gateway := netA.CIDR.Addr().Next().String()
	// The namespace redirects open to the bridge; taking no redirects, it
	// keeps routing.
	if b, err := exec.Command("ip", "netns", "exec", open.ns, "sh", "-c",
		"for f in /proc/sys/net/ipv4/conf/*/accept_redirects; do echo 0 > $f; done").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	for _, to := range []*guest{web, sandbox} {
		if b, err := exec.Command("ip", "-n", open.ns, "route", "add", addr(to)+"/32", "via", gateway).CombinedOutput(); err != nil {
			t.Fatalf("routing through the gateway: %v: %s", err, b)
		}
	}
	if !connects(open, addr(web), 8080) {
		t.Error("open -> web:8080 through the gateway: refused, though web lets 8080 in from anywhere")
	}
	if connects(open, addr(sandbox), 8080) {
		t.Error("open -> sandbox:8080 through the gateway: connected, though sandbox lets nothing in")
	}
	if udpArrives(t, open, sandbox, 67, 68) {
		t.Error("DHCP-shaped UDP routed through the gateway reached sandbox, which lets nothing in")
	}
	if udpArrives(t, open, sandbox, 40000, 9999) {
		t.Error("UDP routed through the gateway reached sandbox, which lets nothing in")
	}
}
