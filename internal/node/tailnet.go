package node

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// A container joins and leaves a tailnet while it runs, its guest's
// hangar-init running tailscaled (docs/decisions/container-tailscale.md).
// The agent tells each running container's guest the tailnet its machine
// names, or none, and rewrites its boot disk to match, so a boot of its
// restart policy comes up as told. It asks each one that joins where its
// join stands, hands it the auth key, asked of the apiserver each time,
// only when the guest asks for one, and reports TailnetJoined on the
// machine. The guest keeps the time of its last failed join, so a retry
// waits as long whatever the agent does meanwhile. None of it holds the
// container back.

// DefaultTailnetPoll is how often a running container that joins a
// tailnet is asked where its join stands.
const DefaultTailnetPoll = 10 * time.Second

// noKeyMax bounds the wait before a Secret that gives no key is asked for
// again: first a poll later, then twice as long each time, up to it. The
// Secret may be applied at any time, but most likely soon, and the
// apiserver is not asked every poll for one that never comes.
const noKeyMax = 5 * time.Minute

// tailnetWatch is a running container that joins a tailnet, in one boot.
type tailnetWatch struct {
	key  controller.Key
	boot time.Time
	// cond is TailnetJoined as last found, nil until the guest is asked.
	cond *metav1.Condition
	// noKeyUntil is when a guest asking for the key while its Secret gives
	// none is next given it, and noKeyWait how long the wait was.
	noKeyUntil time.Time
	noKeyWait  time.Duration
}

// toldTailnet is the tailnet a container's guest was told in one boot, as
// its boot disk was rewritten to it, or that it cannot be told one.
type toldTailnet struct {
	boot   time.Time
	ts     *driver.TailscaleSpec
	cannot bool
}

func sameTailscale(a, b *driver.TailscaleSpec) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// ensureGuestTailnet tells a running container's guest the tailnet spec
// joins, or none: on the agent's first sight of each boot, since what the
// guest runs then is its boot disk's, which an earlier agent may have left
// behind what the machine says, and whenever spec's changes. Its boot disk
// is rewritten first, so a reboot by the restart policy meanwhile comes up
// as told. A guest that cannot be told, from before it could, keeps what it
// booted with; its TailnetJoined says so. The caller holds the host lock.
func (a *Agent) ensureGuestTailnet(ctx context.Context, spec driver.VMSpec, vm *driver.VM) error {
	uid := spec.MachineUID
	if t, ok := a.toldTailnets[uid]; ok && t.boot.Equal(vm.Started) && (t.cannot || sameTailscale(t.ts, spec.Tailscale)) {
		return nil
	}
	if err := a.cfg.Drivers.Hypervisor.SetBootTailscale(ctx, uid, spec.Tailscale); err != nil {
		return fmt.Errorf("the boot disk's tailnet: %w", err)
	}
	gctx, cancel := context.WithTimeout(ctx, guestTimeout)
	err := a.cfg.Drivers.Guest.SetTailnet(gctx, uid, spec.Tailscale)
	cancel()
	switch {
	case errors.Is(err, driver.ErrGuestCannot):
		a.toldTailnets[uid] = toldTailnet{boot: vm.Started, cannot: true}
		return nil
	case err != nil:
		return fmt.Errorf("telling the guest its tailnet: %w", err)
	}
	var ts *driver.TailscaleSpec
	if spec.Tailscale != nil {
		c := *spec.Tailscale
		ts = &c
	}
	a.toldTailnets[uid] = toldTailnet{boot: vm.Started, ts: ts}
	notify(a.tailnetTrigger)
	return nil
}

// tailscaleSpec is what the guest of container machine m joins its
// tailnet with, nil if it joins none.
func tailscaleSpec(m *computev1alpha1.Machine) *driver.TailscaleSpec {
	ts := m.GetSpec().GetContainer().GetTailscale()
	if ts == nil {
		return nil
	}
	return &driver.TailscaleSpec{Hostname: ts.GetHostname(), AcceptRoutes: ts.GetAcceptRoutes()}
}

// watchTailnet keeps, or drops, the watch of container machine m whose
// reconcile observed obs, its VM started with spec. The caller holds the
// host lock.
func (a *Agent) watchTailnet(m *computev1alpha1.Machine, spec driver.VMSpec, obs *observation) {
	uid := m.GetMetadata().GetUid()
	if spec.Tailscale == nil || obs == nil || obs.phase != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING || obs.started == nil {
		delete(a.tailnets, uid)
		return
	}
	boot := obs.started.AsTime()
	if w, ok := a.tailnets[uid]; ok && w.boot.Equal(boot) {
		return
	}
	a.tailnets[uid] = &tailnetWatch{key: controller.Key{Project: m.GetMetadata().GetProject(), Name: m.GetMetadata().GetName()}, boot: boot}
	notify(a.tailnetTrigger)
}

// tailnetCondition is the TailnetJoined of machine m whose reconcile
// observed obs: as its guest last said in the boot obs saw, or Starting.
// Nil for a machine that joins no tailnet. The caller holds the host lock.
func (a *Agent) tailnetCondition(m *computev1alpha1.Machine, obs *observation) *metav1.Condition {
	if tailscaleSpec(m) == nil {
		return nil
	}
	if obs.phase != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING {
		return tailnetFalse(computeapi.ReasonTailnetStarting, "the container is not running")
	}
	// A reconcile stuck on a step of a running VM sees no boot: the watch
	// is of the one running.
	w, ok := a.tailnets[m.GetMetadata().GetUid()]
	if ok && w.cond != nil && (obs.started == nil || w.boot.Equal(obs.started.AsTime())) {
		return proto.Clone(w.cond).(*metav1.Condition)
	}
	return tailnetFalse(computeapi.ReasonTailnetStarting, "waiting for the guest's tailscaled")
}

func tailnetFalse(reason, msg string) *metav1.Condition {
	return &metav1.Condition{Type: computeapi.TailnetJoined, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reason, Message: msg}
}

// tailnetConditionOf is TailnetJoined as the guest says st.
func tailnetConditionOf(st driver.TailnetStatus) *metav1.Condition {
	switch st.State {
	case driver.TailnetJoined:
		return &metav1.Condition{Type: computeapi.TailnetJoined, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: computeapi.ReasonTailnetJoined, Message: st.Message}
	case driver.TailnetKeyRejected:
		return tailnetFalse(computeapi.ReasonTailnetKeyRejected, st.Message)
	case driver.TailnetFailed:
		return tailnetFalse(computeapi.ReasonTailnetFailed, st.Message)
	case driver.TailnetNeedsKey:
		return tailnetFalse(computeapi.ReasonTailnetStarting, "the guest waits for the auth key")
	case driver.TailnetOff:
		return tailnetFalse(computeapi.ReasonTailnetStarting, "the guest is being told to join")
	}
	return tailnetFalse(computeapi.ReasonTailnetStarting, st.Message)
}

// noKeyWait is how long a guest asking for the key while its Secret gives
// none waits, after waiting last, polled every poll.
func noKeyWait(last, poll time.Duration) time.Duration {
	if last <= 0 {
		return min(poll, noKeyMax)
	}
	return min(2*last, noKeyMax)
}

// tailnetLoop asks, every TailnetPoll and when a watch begins, each
// running container that joins a tailnet where its join stands, until ctx
// ends.
func (a *Agent) tailnetLoop(ctx context.Context) {
	ticker := a.cfg.Clock.NewTicker(a.cfg.TailnetPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.Chan():
		case <-a.tailnetTrigger:
		}
		a.pollTailnets(ctx)
	}
}

// pollTailnets asks each watched guest, off the host lock.
func (a *Agent) pollTailnets(ctx context.Context) {
	a.host.Lock()
	watches := map[string]tailnetWatch{}
	for uid, w := range a.tailnets {
		if a.guests.slow[uid] == nil {
			watches[uid] = *w
		}
	}
	a.host.Unlock()
	for _, uid := range slices.Sorted(maps.Keys(watches)) {
		if ctx.Err() != nil {
			return
		}
		a.pollTailnet(ctx, uid, watches[uid])
	}
}

// pollTailnet asks the guest of the machine watched as w where its join
// stands, gives it the key if it is due one, and has the machine
// reconciled when TailnetJoined changes.
func (a *Agent) pollTailnet(ctx context.Context, uid string, w tailnetWatch) {
	gctx, cancel := context.WithTimeout(ctx, guestTimeout)
	st, err := a.cfg.Drivers.Guest.TailnetStatus(gctx, uid)
	cancel()
	var cond *metav1.Condition
	now := a.cfg.Clock.Now()
	until, wait := w.noKeyUntil, w.noKeyWait
	switch {
	case errors.Is(err, driver.ErrGuestCannot):
		cond = tailnetFalse(computeapi.ReasonTailnetFailed, "the guest's init, from before it could, cannot join a tailnet; it joins once the container starts again")
	case err != nil:
		a.cfg.Log.Debug("asking the machine's guest where its tailnet join stands", "machine_uid", uid, "error", err)
		return
	case st.WantsKey && now.Before(until):
		cond = w.cond
	case st.WantsKey:
		var noKey bool
		cond, noKey = a.giveTailnetKey(ctx, uid, w)
		until, wait = time.Time{}, 0
		if noKey {
			wait = noKeyWait(w.noKeyWait, a.cfg.TailnetPoll)
			until = now.Add(wait)
		}
	default:
		cond = tailnetConditionOf(st)
	}
	a.host.Lock()
	cur, ok := a.tailnets[uid]
	changed := false
	if ok && cur.boot.Equal(w.boot) {
		cur.noKeyUntil, cur.noKeyWait = until, wait
		changed = !proto.Equal(cur.cond, cond)
		cur.cond = cond
	}
	a.host.Unlock()
	if changed && a.requeue != nil {
		a.requeue(w.key)
	}
}

// giveTailnetKey asks the apiserver for the machine's auth key and gives
// it to its guest, and says how that went; noKey says the Secret gave none.
// The key is kept nowhere and logged nowhere; the errors here are the
// apiserver's and the guest's, which hold none.
func (a *Agent) giveTailnetKey(ctx context.Context, uid string, w tailnetWatch) (cond *metav1.Condition, noKey bool) {
	res, err := a.tailscaleKeys.ResolveTailscaleAuthKey(ctx, connect.NewRequest(&nodev1.ResolveTailscaleAuthKeyRequest{Project: w.key.Project, Machine: w.key.Name}))
	switch {
	case connect.CodeOf(err) == connect.CodeNotFound:
		var ce *connect.Error
		msg := err.Error()
		if errors.As(err, &ce) {
			msg = ce.Message()
		}
		return tailnetFalse(computeapi.ReasonTailnetNoKey, msg), true
	case err != nil:
		a.cfg.Log.Warn("asking for the machine's tailscale auth key", "machine_uid", uid, "project", w.key.Project, "machine", w.key.Name, "error", err)
		return tailnetFalse(computeapi.ReasonTailnetStarting, fmt.Sprintf("the node could not get the auth key: %v", err)), false
	}
	gctx, cancel := context.WithTimeout(ctx, guestTimeout)
	err = a.cfg.Drivers.Guest.JoinTailnet(gctx, uid, res.Msg.GetAuthKey())
	cancel()
	switch {
	case errors.Is(err, driver.ErrTailnetHasState), errors.Is(err, driver.ErrTailnetBusy):
		return tailnetFalse(computeapi.ReasonTailnetStarting, "joining the tailnet"), false
	case err != nil:
		return tailnetFalse(computeapi.ReasonTailnetFailed, fmt.Sprintf("giving the guest the auth key: %v", err)), false
	}
	a.cfg.Log.Info("gave the machine's guest its tailscale auth key", "machine_uid", uid, "project", w.key.Project, "machine", w.key.Name)
	return tailnetFalse(computeapi.ReasonTailnetStarting, "joining the tailnet"), false
}
