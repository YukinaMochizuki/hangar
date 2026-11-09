package node

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// TestTailnetConditions: each state a guest says is a TailnetJoined of the
// contract's reasons, True only once it has joined.
func TestTailnetConditions(t *testing.T) {
	for st, want := range map[driver.TailnetState]string{
		driver.TailnetJoined:      computeapi.ReasonTailnetJoined,
		driver.TailnetKeyRejected: computeapi.ReasonTailnetKeyRejected,
		driver.TailnetFailed:      computeapi.ReasonTailnetFailed,
		driver.TailnetNeedsKey:    computeapi.ReasonTailnetStarting,
		driver.TailnetStarting:    computeapi.ReasonTailnetStarting,
	} {
		c := tailnetConditionOf(driver.TailnetStatus{State: st, Message: "m"})
		wantStatus := metav1.ConditionStatus_CONDITION_STATUS_FALSE
		if st == driver.TailnetJoined {
			wantStatus = metav1.ConditionStatus_CONDITION_STATUS_TRUE
		}
		if c.GetType() != computeapi.TailnetJoined || c.GetReason() != want || c.GetStatus() != wantStatus || c.GetMessage() == "" {
			t.Errorf("state %d: %v, want %s %v", st, c, want, wantStatus)
		}
	}
}

// tailnetGuest answers TailnetStatus with st and records the keys given.
type tailnetGuest struct {
	driver.Guest
	st   driver.TailnetStatus
	keys []string
}

func (g *tailnetGuest) TailnetStatus(context.Context, string) (driver.TailnetStatus, error) {
	return g.st, nil
}

func (g *tailnetGuest) JoinTailnet(_ context.Context, _, key string) error {
	g.keys = append(g.keys, key)
	return nil
}

// keySource answers ResolveTailscaleAuthKey with key, or err, and counts
// the asks.
type keySource struct {
	key  string
	err  error
	asks int
}

func (k *keySource) ResolveTailscaleAuthKey(context.Context, *connect.Request[nodev1.ResolveTailscaleAuthKeyRequest]) (*connect.Response[nodev1.ResolveTailscaleAuthKeyResponse], error) {
	k.asks++
	if k.err != nil {
		return nil, k.err
	}
	return connect.NewResponse(&nodev1.ResolveTailscaleAuthKeyResponse{AuthKey: k.key}), nil
}

func tailnetAgent(g driver.Guest, keys *keySource, clock clockwork.Clock, log *bytes.Buffer) (*Agent, tailnetWatch) {
	a := &Agent{
		cfg: Config{
			Clock: clock, Drivers: &Drivers{Guest: g}, TailnetPoll: DefaultTailnetPoll,
			Log: slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		},
		tailscaleKeys: keys,
		tailnets:      map[string]*tailnetWatch{},
	}
	w := tailnetWatch{key: controller.Key{Project: "web", Name: "ct-box"}, boot: clock.Now()}
	a.tailnets["m1"] = &w
	return a, w
}

// TestTheKeyGoesWhereTheGuestAsks: the guest is given the key when it asks
// for one, never when it does not; the node's log says it gave one, and
// never holds it.
func TestTheKeyGoesWhereTheGuestAsks(t *testing.T) {
	const key = "tskey-auth-kNODE1CNTRL-nodeSECRETpart"
	g := &tailnetGuest{st: driver.TailnetStatus{State: driver.TailnetKeyRejected, Message: "refused"}}
	keys := &keySource{key: key}
	var log bytes.Buffer
	a, _ := tailnetAgent(g, keys, clockwork.NewFakeClock(), &log)
	watch := func() tailnetWatch { return *a.tailnets["m1"] }
	a.pollTailnet(t.Context(), "m1", watch())
	if keys.asks != 0 || len(g.keys) != 0 {
		t.Errorf("a guest not asking was given %d keys, the apiserver asked %d times", len(g.keys), keys.asks)
	}
	if c := watch().cond; c.GetReason() != computeapi.ReasonTailnetKeyRejected {
		t.Errorf("a refused key: %v", c)
	}
	g.st.WantsKey = true
	a.pollTailnet(t.Context(), "m1", watch())
	if len(g.keys) != 1 || g.keys[0] != key {
		t.Errorf("a guest asking was given %q", g.keys)
	}
	if !strings.Contains(log.String(), "gave the machine's guest its tailscale auth key") {
		t.Errorf("the log does not say a key was given:\n%s", log.String())
	}
	if strings.Contains(log.String(), "nodeSECRET") {
		t.Errorf("the log holds the key:\n%s", log.String())
	}
}

// TestASecretThatGivesNoKeyIsAskedForLessOften: a guest asking for the key
// while its Secret gives none says NoKey, and the apiserver is asked again
// a poll later, 10s, then twice as long each time, up to 5 minutes, not at
// every poll; once a key comes, the wait starts over.
func TestASecretThatGivesNoKeyIsAskedForLessOften(t *testing.T) {
	g := &tailnetGuest{st: driver.TailnetStatus{State: driver.TailnetNeedsKey, WantsKey: true}}
	keys := &keySource{err: connect.NewError(connect.CodeNotFound, errors.New(`tailscale auth secret "ts" in project "web" does not exist`))}
	clock := clockwork.NewFakeClock()
	var log bytes.Buffer
	a, _ := tailnetAgent(g, keys, clock, &log)
	poll := func() { a.pollTailnet(t.Context(), "m1", *a.tailnets["m1"]) }
	poll()
	if c := a.tailnets["m1"].cond; keys.asks != 1 || c.GetReason() != computeapi.ReasonTailnetNoKey || !strings.Contains(c.GetMessage(), `"ts"`) {
		t.Fatalf("asked %d times, %v; want once, NoKey naming the Secret", keys.asks, c)
	}
	poll()
	if keys.asks != 1 || a.tailnets["m1"].cond.GetReason() != computeapi.ReasonTailnetNoKey {
		t.Errorf("polled again at once: asked %d times, %v", keys.asks, a.tailnets["m1"].cond)
	}
	for _, wait := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second} {
		clock.Advance(wait - time.Second)
		before := keys.asks
		poll()
		if keys.asks != before {
			t.Errorf("asked again %v into a wait of %v", wait-time.Second, wait)
		}
		clock.Advance(time.Second)
		poll()
		if keys.asks != before+1 {
			t.Errorf("not asked again after a wait of %v", wait)
		}
	}
	for range 10 {
		clock.Advance(noKeyMax)
		poll()
	}
	if w := a.tailnets["m1"].noKeyWait; w != noKeyMax {
		t.Errorf("the wait grew to %v, want at most %v", w, noKeyMax)
	}
	keys.err, keys.key = nil, "tskey-auth-k"
	clock.Advance(noKeyMax)
	poll()
	if len(g.keys) != 1 || a.tailnets["m1"].noKeyWait != 0 {
		t.Errorf("a Secret applied: given %q, wait %v; want the key, and the wait over", g.keys, a.tailnets["m1"].noKeyWait)
	}
}
