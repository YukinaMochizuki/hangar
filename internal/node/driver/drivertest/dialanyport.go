package drivertest

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// dialAnyPort: DialAnyPort reaches any port of a machine's interface whose
// policy lets nothing in and that names no edge port, where Dial reaches
// only port 22; and reaches nothing of a file share's server, whose guest
// listens all the same, as its client reaching it shows where the host can
// tell (Reaches).
func dialAnyPort(t *testing.T, h Host) {
	ctx := t.Context()
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	var nic, srv driver.Interface
	for _, p := range []struct {
		nic *driver.Interface
		at  driver.Interface
	}{{&nic, nicAt("m1", 5, false)}, {&srv, nicAt("srv", 9, true)}} {
		got, err := h.Network.EnsureInterface(ctx, p.at)
		must(t, err)
		*p.nic = got
	}
	t.Cleanup(func() {
		_ = h.Network.DeleteInterfaces(ctx, "m1")
		_ = h.Network.DeleteInterfaces(ctx, "srv")
		_ = h.Network.DeleteNetwork(ctx, testNet.UID)
	})
	// No policy: the interface lets nothing in, and names no edge port; it
	// is the server's client, and the server's only one.
	must(t, h.Network.EnsureFirewall(ctx, testNet.UID, []driver.InterfacePolicy{
		{MachineUID: "m1", Name: "eth0", Shares: []netip.Addr{srv.Address.Addr()}},
		{MachineUID: "srv", Name: "eth0", ShareClients: []netip.Addr{nic.Address.Addr()}},
	}))
	h.ServeGuest(t, nic, 22, 8080, 50000)
	h.ServeGuest(t, srv, driver.ShareServerPort, 22, 8080)
	type dialer func(context.Context, string, netip.AddrPort) (net.Conn, error)
	// reaches says whether a dial to addr gets its echo.
	reaches := func(dial dialer, addr netip.AddrPort) bool {
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		c, err := dial(dctx, testNet.UID, addr)
		if err != nil {
			return false
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write([]byte("hi")); err != nil {
			return false
		}
		b := make([]byte, 2)
		_, err = io.ReadFull(c, b)
		return err == nil && string(b) == "hi"
	}
	at := func(i driver.Interface, port uint16) netip.AddrPort {
		return netip.AddrPortFrom(i.Address.Addr(), port)
	}
	for _, p := range []uint16{22, 8080, 50000} {
		testutil.Eventually(t, fmt.Sprintf("port %d reached by DialAnyPort", p), func() bool {
			return reaches(h.Network.DialAnyPort, at(nic, p))
		})
	}
	if reaches(h.Network.Dial, at(nic, 8080)) {
		t.Error("Dial reached port 8080, which no edge port names")
	}
	if h.Reaches != nil {
		testutil.Eventually(t, "the control: the server's client reaches it", func() bool {
			return h.Reaches(t, nic, at(srv, driver.ShareServerPort))
		})
	}
	for _, p := range []uint16{driver.ShareServerPort, 22, 8080} {
		if reaches(h.Network.DialAnyPort, at(srv, p)) {
			t.Errorf("DialAnyPort reached a file share's server's port %d", p)
		}
	}
}
