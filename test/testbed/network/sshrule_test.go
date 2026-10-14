//go:build testbed

package network

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

// fromGateway runs a Python script in the network's namespace, as the
// platform's own sockets there are, and says whether it succeeded.
func fromGateway(n driver.NetworkSpec, script string) bool {
	return exec.Command("ip", "netns", "exec", hostnet.NamespaceOf(n.UID), "python3", "-c", script).Run() == nil
}

// TestThePlatformsSSHPath: a guest whose policy lets nothing in is reached
// on its port 22 by DialSSH, from the gateway, and by nothing else: not by
// another guest, nor UDP from the gateway to its port 22, nor TCP from the
// gateway's port 22 to its other ports. A policy letting everything in
// shows each of those would arrive if let.
func TestThePlatformsSSHPath(t *testing.T) {
	d := newDriver(t, hostnet.Config{Uplink: upstreamLink, PublicIPv4: netip.MustParseAddr(hostAddr)})
	nics := ensure(t, d, []driver.NetworkSpec{netA}, nic("locked", netA, 2), nic("other", netA, 3))
	locked, other := newGuest(t, nics[0]), newGuest(t, nics[1])
	gw := nics[0].Gateway.String()
	addr := locked.nic.Address.Addr().String()
	policy := func(in []secgroup.Rule) {
		t.Helper()
		open := secgroup.Policy{Ingress: []secgroup.Rule{{Protocol: secgroup.All, Peers: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}}, EgressAll: true}
		if err := d.EnsureFirewall(t.Context(), netA.UID, []driver.InterfacePolicy{
			{MachineUID: "locked", Name: "eth0", Policy: secgroup.Policy{Ingress: in, EgressAll: true}},
			{MachineUID: "other", Name: "eth0", Policy: open},
		}); err != nil {
			t.Fatal(err)
		}
	}
	echoServer(t, locked, 22, 2222)
	udp22 := func() bool {
		return udpFromGateway(t, locked, gw)
	}
	fromGateway22To2222 := func() bool {
		return fromGateway(netA, fmt.Sprintf(`import socket
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind((%q,22)); s.settimeout(1.5)
s.connect((%q,2222)); s.sendall(b"hi"); assert s.recv(2)==b"hi"`, gw, addr))
	}
	dialSSH := func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		c, err := d.DialSSH(ctx, netA.UID, locked.nic.Address.Addr())
		if err != nil {
			return false
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte("hi")); err != nil {
			return false
		}
		b := make([]byte, 2)
		_, err = io.ReadFull(c, b)
		return err == nil && string(b) == "hi"
	}

	// Nothing let in: the platform's path alone.
	policy(nil)
	for _, c := range []struct {
		what string
		got  bool
		want bool
	}{
		{"DialSSH from the gateway", dialSSH(), true},
		{"another guest to port 22", connects(other, addr, 22), false},
		{"UDP from the gateway to port 22", udp22(), false},
		{"TCP from the gateway's port 22 to port 2222", fromGateway22To2222(), false},
	} {
		if c.got != c.want {
			t.Errorf("nothing let in: %s: arrived %t, want %t", c.what, c.got, c.want)
		}
	}

	// Everything let in: each probe arrives, so each can tell.
	policy([]secgroup.Rule{{Protocol: secgroup.All, Peers: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}})
	for what, got := range map[string]bool{
		"another guest to port 22":                    connects(other, addr, 22),
		"UDP from the gateway to port 22":             udp22(),
		"TCP from the gateway's port 22 to port 2222": fromGateway22To2222(),
	} {
		if !got {
			t.Errorf("everything let in: %s did not arrive", what)
		}
	}
}

// udpFromGateway says whether UDP the namespace sends from the gateway
// reaches the guest's port 22.
func udpFromGateway(t *testing.T, to *guest, gw string) bool {
	t.Helper()
	recv := exec.Command("ip", "netns", "exec", to.ns, "python3", "-c", `import socket
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(("0.0.0.0",22)); s.settimeout(2)
try: print(s.recvfrom(64)[0].decode())
except socket.timeout: pass`)
	var out strings.Builder
	recv.Stdout = &out
	if err := recv.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	fromGateway(netA, fmt.Sprintf(`import socket,time
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind((%q,0))
for i in range(5): s.sendto(b"probe",(%q,22)); time.sleep(0.1)`, gw, to.nic.Address.Addr()))
	_ = recv.Wait()
	return strings.Contains(out.String(), "probe")
}
