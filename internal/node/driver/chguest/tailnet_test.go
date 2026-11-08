package chguest_test

import (
	"errors"
	"fmt"
	"testing"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/guest/agent"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// TestTheTailnetCalls: the driver carries a tailnet to join, or none, to
// the agent, reads its status, whether it wants the key included, and maps
// the agent's refusals to the driver's errors; a guest from before
// tailnets cannot.
func TestTheTailnetCalls(t *testing.T) {
	old := guestOn(serveCH(t, agent.Port, &agent.Server{}, func() {}))
	if err := old.SetTailnet(t.Context(), "m1", nil); !errors.Is(err, driver.ErrGuestCannot) {
		t.Errorf("set on an old guest: %v, want ErrGuestCannot", err)
	}
	if _, err := old.TailnetStatus(t.Context(), "m1"); !errors.Is(err, driver.ErrGuestCannot) {
		t.Errorf("status of an old guest: %v, want ErrGuestCannot", err)
	}
	if err := old.JoinTailnet(t.Context(), "m1", "k"); !errors.Is(err, driver.ErrGuestCannot) {
		t.Errorf("join on an old guest: %v, want ErrGuestCannot", err)
	}

	var told []*guestv1.Tailscale
	var joinErr error
	var gotKey string
	g := guestOn(serveCH(t, agent.Port, &agent.Server{
		TailnetState: func() *guestv1.TailnetStatusResponse {
			return &guestv1.TailnetStatusResponse{State: guestv1.TailnetState_TAILNET_STATE_KEY_REJECTED, Message: "refused", WantsKey: true}
		},
		TailnetSet:  func(ts *guestv1.Tailscale) { told = append(told, ts) },
		TailnetJoin: func(key string) error { gotKey = key; return joinErr },
	}, func() {}))
	if err := g.SetTailnet(t.Context(), "m1", &driver.TailscaleSpec{Hostname: "box", AcceptRoutes: true}); err != nil {
		t.Fatal(err)
	}
	if err := g.SetTailnet(t.Context(), "m1", nil); err != nil {
		t.Fatal(err)
	}
	if len(told) != 2 || told[0].GetHostname() != "box" || !told[0].GetAcceptRoutes() || told[1] != nil {
		t.Errorf("the guest was told %v, want box with routes, then none", told)
	}
	st, err := g.TailnetStatus(t.Context(), "m1")
	if err != nil || st != (driver.TailnetStatus{State: driver.TailnetKeyRejected, Message: "refused", WantsKey: true}) {
		t.Errorf("status %+v, %v", st, err)
	}
	if err := g.JoinTailnet(t.Context(), "m1", "tskey-x"); err != nil || gotKey != "tskey-x" {
		t.Errorf("join: %v, the guest got %q", err, gotKey)
	}
	for _, c := range []struct {
		guest, want error
	}{
		{fmt.Errorf("%w: x", agent.ErrTailnetHasState), driver.ErrTailnetHasState},
		{fmt.Errorf("%w: x", agent.ErrTailnetBusy), driver.ErrTailnetBusy},
	} {
		joinErr = c.guest
		if err := g.JoinTailnet(t.Context(), "m1", "tskey-x"); !errors.Is(err, c.want) {
			t.Errorf("the guest refusing with %v: %v, want %v", c.guest, err, c.want)
		}
	}
}
