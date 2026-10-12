package chguest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/YukinaMochizuki/hangar/internal/guest/agent"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
)

func TestStopReachesTheGuest(t *testing.T) {
	asked := make(chan time.Duration, 4)
	socket := serveCH(t, agent.Port, &agent.Server{
		Reaper: reaper(), Root: "/", Version: "test", Started: time.Now(),
		StopMain: func(grace time.Duration) { asked <- grace },
	}, func() {})
	g := chguest.Guest{Socket: func(string) string { return socket }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := g.Stop(ctx, "m1", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := <-asked; got != 10*time.Second {
		t.Errorf("the guest was asked for a grace of %v", got)
	}
	for _, bad := range []time.Duration{-time.Second, agent.MaxGrace + time.Second} {
		err := g.Stop(ctx, "m1", bad)
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("a grace of %v: %v", bad, err)
		}
	}
	if len(asked) != 0 {
		t.Error("a refused Stop reached the process")
	}
}

func TestStopOfAGuestWithoutAProcess(t *testing.T) {
	socket := fakeCH(t, agent.Port)
	g := chguest.Guest{Socket: func(string) string { return socket }}
	err := g.Stop(context.Background(), "m1", time.Second)
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("%v, want unimplemented", err)
	}
}

func TestStopOfNoGuest(t *testing.T) {
	g := chguest.Guest{Socket: func(string) string { return t.TempDir() + "/absent" }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.Stop(ctx, "m1", time.Second); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("stopping a guest whose socket is absent: %v, want a quick failure", err)
	}
}
