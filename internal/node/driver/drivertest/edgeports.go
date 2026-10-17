package drivertest

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// dialEdgePorts: Dial reaches a port other than 22 of a machine's interface
// only while the interface's EdgePorts names it, whatever its policy lets
// in (here, nothing); a port taken out is closed again, and a port never
// named stays closed though the guest listens on it.
func dialEdgePorts(t *testing.T, h Host) {
	ctx := t.Context()
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	nic, err := h.Network.EnsureInterface(ctx, eth0("m1"))
	must(t, err)
	t.Cleanup(func() {
		_ = h.Network.DeleteInterfaces(ctx, "m1")
		_ = h.Network.DeleteNetwork(ctx, testNet.UID)
	})
	h.ServeGuest(t, nic, 8080, 8081, 8082)
	edge := func(ports ...uint16) {
		t.Helper()
		must(t, h.Network.EnsureFirewall(ctx, testNet.UID, []driver.InterfacePolicy{
			{MachineUID: "m1", Name: "eth0", Policy: secgroup.Policy{EgressAll: true}, EdgePorts: ports},
		}))
	}
	// reaches says whether a dial to port gets its echo.
	reaches := func(port uint16) bool {
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		c, err := h.Network.Dial(dctx, testNet.UID, netip.AddrPortFrom(nic.Address.Addr(), port))
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
	// Both named: both reached, once the guest listens.
	edge(8080, 8081)
	for _, p := range []uint16{8080, 8081} {
		testutil.Eventually(t, fmt.Sprintf("port %d, named, reached", p), func() bool { return reaches(p) })
	}
	// 8082, never named: closed, though the guest listens on it.
	if reaches(8082) {
		t.Error("port 8082, never named, reached")
	}
	// 8081 taken out: closed, though the guest listens on it.
	edge(8080)
	if !reaches(8080) {
		t.Error("port 8080, named, not reached")
	}
	if reaches(8081) {
		t.Error("port 8081, no longer named, reached")
	}
	// None named: closed.
	edge()
	if reaches(8080) {
		t.Error("port 8080, no longer named, reached")
	}
	// Named again: open again.
	edge(8080)
	if !reaches(8080) {
		t.Error("port 8080, named again, not reached")
	}
}
