package drivertest

import (
	"context"
	"io"
	"net/netip"
	"testing"
	"time"
)

// dialSSH: DialSSH reaches port 22 of a machine's address in its network,
// from the gateway, whatever the interface's firewall says, and nothing
// when no guest answers there, in a network not on the host, or at an
// address of nobody.
func dialSSH(t *testing.T, h Host) {
	ctx := t.Context()
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	nic, err := h.Network.EnsureInterface(ctx, eth0("m1"))
	must(t, err)
	t.Cleanup(func() {
		_ = h.Network.DeleteInterfaces(ctx, "m1")
		_ = h.Network.DeleteNetwork(ctx, testNet.UID)
	})
	refused := func(what, network string, addr netip.Addr) {
		t.Helper()
		dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if c, err := h.Network.DialSSH(dctx, network, addr); err == nil {
			_ = c.Close()
			t.Errorf("dialled %s", what)
		}
	}
	refused("a network not on the host", "nonet", nic.Address.Addr())
	refused("an address no guest answers at", testNet.UID, nic.Address.Addr().Next())

	// A guest on the interface; the interface has no policy, so its
	// firewall lets in nothing but the platform's SSH.
	h.ServeGuest(t, nic, 22)
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := h.Network.DialSSH(dctx, testNet.UID, nic.Address.Addr())
	if err != nil {
		t.Fatalf("dialling the guest's port 22: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hi" {
		t.Errorf("the guest echoed %q %v", buf, err)
	}
}
