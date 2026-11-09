package drivertest

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// Tailnet is a tailnet the host's guests can reach: its control server
// takes Key, a reusable auth key, and refuses Rejected.
type Tailnet struct {
	Key, Rejected string
}

// tailnetVM starts a container VM, m1, that joins no tailnet, and returns
// its spec, as it runs with its guest agent answering, and a wait for its
// guest to say what ok takes.
func tailnetVM(t *testing.T, h Host) (driver.VMSpec, driver.VM, func(what string, ok func(driver.TailnetStatus) bool) driver.TailnetStatus) {
	t.Helper()
	ctx := t.Context()
	spec := vmSpec(t, h, "m1", driver.RestartAlways)
	t.Cleanup(func() {
		_ = h.Hypervisor.Stop(ctx, "m1")
		_ = h.Hypervisor.DeleteLogs(ctx, "m1")
		_ = h.Network.DeleteInterfaces(ctx, "m1")
		_ = h.Storage.DeleteRoot(ctx, "m1")
	})
	must(t, h.Hypervisor.Start(ctx, spec))
	testutil.EventuallyWithin(t, vmWait, "the guest agent answering", func() bool { return h.Guest.Ready(ctx, "m1") == nil })
	started, _ := vmOf(t, h, "m1")
	status := func(what string, ok func(driver.TailnetStatus) bool) driver.TailnetStatus {
		t.Helper()
		var st driver.TailnetStatus
		testutil.EventuallyWithin(t, vmWait, what, func() (bool, string) {
			var err error
			st, err = h.Guest.TailnetStatus(ctx, "m1")
			return err == nil && ok(st), fmt.Sprintf("%+v %v", st, err)
		})
		return st
	}
	return spec, started, status
}

func tailnetIs(state driver.TailnetState) func(driver.TailnetStatus) bool {
	return func(st driver.TailnetStatus) bool { return st.State == state }
}

func askingForTheKey(st driver.TailnetStatus) bool {
	return st.State == driver.TailnetNeedsKey && st.WantsKey
}

// tailnetDisk needs no tailnet: a container told no tailnet runs none, and
// asks for no key; told one, it starts tailscaled from the disk every
// container gets, and asks for the key, all while its VM runs on; told
// none again, it runs none; started again by its restart policy, it joins
// as its boot disk says, not as it booted first. The boot disk of a
// machine with no VM takes no tailnet.
func tailnetDisk(t *testing.T, h Host) {
	ctx := t.Context()
	if err := h.Hypervisor.SetBootTailscale(ctx, "m1", nil); err == nil {
		t.Error("the boot disk of a machine with no VM was given a tailnet")
	}
	spec, started, status := tailnetVM(t, h)
	if st := status("no tailnet", tailnetIs(driver.TailnetOff)); st.WantsKey {
		t.Errorf("a guest told no tailnet asks for a key: %+v", st)
	}
	ts := &driver.TailscaleSpec{Hostname: "m1"}
	must(t, h.Hypervisor.SetBootTailscale(ctx, "m1", ts))
	must(t, h.Guest.SetTailnet(ctx, "m1", ts))
	status("asking for the key, tailscaled run from the disk", askingForTheKey)
	must(t, h.Guest.SetTailnet(ctx, "m1", nil))
	if st := status("no tailnet again", tailnetIs(driver.TailnetOff)); st.WantsKey {
		t.Errorf("a guest that left asks for a key: %+v", st)
	}
	must(t, h.Guest.SetTailnet(ctx, "m1", ts))
	status("asking for the key again", askingForTheKey)
	if vm, _ := vmOf(t, h, "m1"); !vm.Started.Equal(started.Started) || vm.SpecHash != spec.Hash() {
		t.Errorf("the VM started again for its tailnet: %+v, was %+v", vm, started)
	}
	if h.Exit == nil {
		return
	}
	h.Exit(t, "m1", 1)
	testutil.EventuallyWithin(t, vmWait, "the VM started again", func() bool {
		vm, ok := vmOf(t, h, "m1")
		return ok && vm.State == driver.VMRunning && vm.Started.After(started.Started) && h.Guest.Ready(ctx, "m1") == nil
	})
	status("its boot disk's tailnet", askingForTheKey)
}

// tailnets joins a tailnet: a refused key says so, and asks for no new one
// at once; a key taken joins, after which no key is taken again; the node
// is renamed on the tailnet as it runs; leaving deletes its state, so a
// join after is a new node, with a key; the VM runs on throughout.
func tailnets(t *testing.T, h Host) {
	ctx := t.Context()
	spec, started, status := tailnetVM(t, h)
	ts := &driver.TailscaleSpec{Hostname: "m1"}
	must(t, h.Guest.SetTailnet(ctx, "m1", ts))
	status("asking for the key", askingForTheKey)
	must(t, h.Guest.JoinTailnet(ctx, "m1", h.Tailnet.Rejected))
	if st := status("the key refused", tailnetIs(driver.TailnetKeyRejected)); st.WantsKey || strings.Contains(st.Message, h.Tailnet.Rejected) {
		t.Errorf("a refused key: %+v; want no new ask at once, and no key said", st)
	}
	must(t, h.Guest.JoinTailnet(ctx, "m1", h.Tailnet.Key))
	if st := status("joined", tailnetIs(driver.TailnetJoined)); st.WantsKey || strings.Contains(st.Message, h.Tailnet.Key) {
		t.Errorf("joined: %+v", st)
	}
	if err := h.Guest.JoinTailnet(ctx, "m1", h.Tailnet.Key); !errors.Is(err, driver.ErrTailnetHasState) {
		t.Errorf("a key given to a node that has joined: %v, want ErrTailnetHasState", err)
	}
	renamed := &driver.TailscaleSpec{Hostname: "m1-renamed", AcceptRoutes: true}
	must(t, h.Guest.SetTailnet(ctx, "m1", renamed))
	status("renamed", func(st driver.TailnetStatus) bool {
		return st.State == driver.TailnetJoined && strings.Contains(st.Message, "m1-renamed")
	})
	must(t, h.Guest.SetTailnet(ctx, "m1", nil))
	status("left", tailnetIs(driver.TailnetOff))
	must(t, h.Guest.SetTailnet(ctx, "m1", ts))
	status("a new node, asking for the key", askingForTheKey)
	if vm, _ := vmOf(t, h, "m1"); !vm.Started.Equal(started.Started) || vm.SpecHash != spec.Hash() {
		t.Errorf("the VM started again for its tailnet: %+v, was %+v", vm, started)
	}
}
