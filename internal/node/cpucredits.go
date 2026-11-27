package node

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// DefaultCreditCheckpoint is how often each running burstable machine's
// CPU credits are written to its status by default
// (docs/decisions/cpu-credits.md).
const DefaultCreditCheckpoint = 5 * time.Minute

const (
	// creditCapMinutes is how many minutes of what a machine earns its
	// balance holds at most: a day's.
	creditCapMinutes = 24 * 60
	// launchMinutes is how many minutes of what a machine earns it is
	// given as it first starts.
	launchMinutes = 30
	// creditKeep is how long a balance outlives the last time it was
	// accounted, as while its machine is stopped.
	creditKeep = 7 * 24 * time.Hour
)

// creditTerms are what a machine's credits follow: its vCPUs and its
// baseline, in percent of each, zero for a machine that is not burstable.
type creditTerms struct {
	vcpus    int32
	baseline int32
}

// creditTermsOf reads a machine's credit terms from its spec. A file
// share's server is never burstable.
func creditTermsOf(m *computev1alpha1.Machine) creditTerms {
	vcpus, _, err := vmSize(m.GetSpec().GetResources())
	if err != nil || m.GetSpec().GetFileShareServer() != nil {
		return creditTerms{}
	}
	return creditTerms{vcpus: vcpus, baseline: m.GetSpec().GetResources().GetCpuBaseline()}
}

func (t creditTerms) burstable() bool { return t.vcpus > 0 && t.baseline > 0 && t.baseline < 100 }

// rate is what the machine earns each minute, in CPU-minutes.
func (t creditTerms) rate() float64 { return float64(t.vcpus) * float64(t.baseline) / 100 }

// limit is the most its balance holds.
func (t creditTerms) limit() float64 { return t.rate() * creditCapMinutes }

// milliCPU is the CPUs its unit is held to once its credits are spent.
func (t creditTerms) milliCPU() int64 { return int64(t.vcpus) * int64(t.baseline) * 10 }

// ledger is one machine's CPU credits on the node. The node accounts a
// span of time only from a read of the unit's CPU time to the next in the
// same boot, each span once, and only after asOf: a span it did not see
// whole, such as one across an agent restart, is charged nothing and earns
// nothing.
type ledger struct {
	key   controller.Key
	terms creditTerms
	// balance is in CPU-minutes, from zero to terms.limit().
	balance float64
	// asOf is the time the balance is accounted up to.
	asOf time.Time
	// boot and cpu are the VM's boot and its unit's CPU time read at asOf,
	// if read: the next read is charged from them.
	boot time.Time
	cpu  time.Duration
	read bool
	// unwritten says the balance holds launch credits not written to the
	// machine's status yet. Until they are, the machine is held to its
	// baseline, so an agent that restarts before writing them gives them
	// once only.
	unwritten bool
	// epoch counts the launch credits given, so writing a balance from
	// before them does not count as writing them.
	epoch int
	// written is the asOf of the balance last written to the machine's
	// status, or loaded from it; writtenAt is when it was written.
	written, writtenAt time.Time
	// running says its VM ran at the last sample.
	running bool
	// lift says its unit may still hold its baseline's limit though the
	// machine is no longer burstable.
	lift bool
	// launch is the apiserver's launch decision for the machine
	// (Machine.spec.cpu_launch); nil starts it from none.
	launch *computev1alpha1.CPULaunchDecision
}

// newLedger is the ledger of a machine the node holds none for, as of now:
// the balance its status last had, clipped to its limit, accounted from its
// as_of on; or, with none, or one past creditKeep, a balance to start
// from (ledger.start).
func newLedger(key controller.Key, terms creditTerms, launch *computev1alpha1.CPULaunchDecision, st *computev1alpha1.MachineCPUCredits, now time.Time) *ledger {
	l := &ledger{key: key, terms: terms, launch: launch}
	if st.GetAsOf() != nil && now.Sub(st.GetAsOf().AsTime()) <= creditKeep {
		b := st.GetBalance()
		if !(b > 0) { // NaN included
			b = 0
		}
		l.balance, l.asOf = min(b, terms.limit()), st.GetAsOf().AsTime()
		l.written, l.writtenAt = l.asOf, now
		return l
	}
	var expired time.Time
	if st.GetAsOf() != nil {
		expired = st.GetAsOf().AsTime()
	}
	l.start(now, expired)
	return l
}

// start starts the machine's balance afresh, accounted from at on, in
// place of one last accounted at expired, zero for none: its launch
// credits if the apiserver granted them, for one gone past creditKeep only
// by a decision after it, so each grant starts one balance; none
// otherwise.
func (l *ledger) start(at, expired time.Time) {
	l.balance, l.asOf, l.read, l.written, l.unwritten = 0, at, false, time.Time{}, false
	if l.launch.GetGranted() && (expired.IsZero() || l.launch.GetDecided().AsTime().After(expired)) {
		l.balance, l.unwritten = l.terms.rate()*launchMinutes, true
	}
	l.epoch++
}

// retarget takes the machine's terms now. A resize or a tier change keeps
// the balance, clipped to the new limit. A machine no longer burstable is
// accounted no more, and its unit's limit is lifted; burstable again, its
// first read is charged nothing.
func (l *ledger) retarget(t creditTerms) {
	if t == l.terms {
		return
	}
	if !t.burstable() {
		l.lift = l.lift || l.terms.burstable()
		l.read = false
	}
	l.terms = t
	if t.burstable() {
		l.balance = min(l.balance, t.limit())
	}
}

// account takes a read at at of the unit's CPU time, cpuTime in the boot
// started at boot. Since the last read, if it was of the same boot and at
// most maxSpan before, the machine earned its rate and spent what its unit
// used, the balance clamped to [0, limit]. A read at or before asOf is
// ignored, so no time is accounted twice. A machine whose balance was last
// accounted more than creditKeep before gets launch credits again.
// dropped says why a span of the same boot went uncharged: spanLate for
// one past maxSpan, spanCounter for a counter gone back; "" for none.
func (l *ledger) account(at, boot time.Time, cpuTime, maxSpan time.Duration) (dropped string) {
	if !l.terms.burstable() || !at.After(l.asOf) {
		return ""
	}
	if at.Sub(l.asOf) > creditKeep {
		l.start(at, l.asOf)
	}
	switch {
	case !l.read || !boot.Equal(l.boot):
	case cpuTime < l.cpu:
		dropped = spanCounter
	case at.Sub(l.asOf) > maxSpan:
		dropped = spanLate
	default:
		earned := l.terms.rate() * at.Sub(l.asOf).Minutes()
		used := (cpuTime - l.cpu).Minutes()
		l.balance = min(max(l.balance+earned-used, 0), l.terms.limit())
	}
	l.asOf, l.boot, l.cpu, l.read = at, boot, cpuTime, true
	return dropped
}

// Why a span of a boot went uncharged (ledger.account), for the metric.
const (
	spanLate    = "late"
	spanCounter = "counter"
)

// held says the node holds the machine to its baseline: its credits are
// spent, short of what one more poll at all its vCPUs would take, so a
// poll never spends more than the balance; or they are launch credits not
// yet written.
func (l *ledger) held(poll time.Duration) bool {
	return l.terms.burstable() && (l.unwritten || l.balance < float64(l.terms.vcpus)*poll.Minutes())
}

// cpuLimit is what the machine's unit is held to, in millicores, zero for
// no limit.
func (l *ledger) cpuLimit(poll time.Duration) int64 {
	if l.held(poll) {
		return l.terms.milliCPU()
	}
	return 0
}

// due says the balance is to be written to the machine's status now:
// launch credits not written yet; or a balance accounted past the one
// written, every checkpoint while the machine runs and at once once it
// does not.
func (l *ledger) due(now time.Time, every time.Duration) bool {
	switch {
	case l.unwritten:
		return true
	case !l.asOf.After(l.written):
		return false
	case !l.running:
		return true
	}
	return now.Sub(l.writtenAt) >= every
}

// checkpoint is a ledger's balance as written to its machine's status.
type checkpoint struct {
	uid     string
	key     controller.Key
	balance float64
	asOf    time.Time
	epoch   int
}

func (c checkpoint) status() *computev1alpha1.MachineCPUCredits {
	return &computev1alpha1.MachineCPUCredits{Balance: c.balance, AsOf: timestamppb.New(c.asOf)}
}

// credits are the ledgers of the machines on the node that are, or were,
// burstable, by machine uid. A ledger outlives its machine's stops, so a
// stale status is read back only once in the agent's life: a ledger is
// made from it only when there is none.
type credits struct {
	mu      sync.Mutex
	ledgers map[string]*ledger
}

func newCredits() *credits {
	return &credits{ledgers: map[string]*ledger{}}
}

// track has the ledgers follow machine m, placed on the node, as of now: a
// burstable one gets a ledger if it has none, from its status; one deleted
// loses its own.
func (c *credits) track(m *computev1alpha1.Machine, now time.Time) {
	uid := m.GetMetadata().GetUid()
	c.mu.Lock()
	defer c.mu.Unlock()
	if m.GetMetadata().GetDeleteTime() != nil {
		delete(c.ledgers, uid)
		return
	}
	terms := creditTermsOf(m)
	l, ok := c.ledgers[uid]
	switch {
	case ok:
		l.retarget(terms)
		l.launch = m.GetSpec().GetCpuLaunch()
	case terms.burstable():
		key := controller.Key{Project: m.GetMetadata().GetProject(), Name: m.GetMetadata().GetName()}
		c.ledgers[uid] = newLedger(key, terms, m.GetSpec().GetCpuLaunch(), m.GetStatus().GetCpuCredits(), now)
	}
}

// forget drops the ledger of the machine of key, gone from the node.
func (c *credits) forget(key controller.Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for uid, l := range c.ledgers {
		if l.key == key {
			delete(c.ledgers, uid)
		}
	}
}

// startLimit is what the machine's VM is to start held to, in millicores.
func (c *credits) startLimit(uid string, poll time.Duration) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l, ok := c.ledgers[uid]; ok {
		return l.cpuLimit(poll)
	}
	return 0
}

// sampled records which machines' VMs run.
func (c *credits) sampled(running map[string]driver.VM) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for uid, l := range c.ledgers {
		_, l.running = running[uid]
	}
}

// account takes a read of a running machine's unit, as ledger.account
// does, and says what its unit is to be held to now, and whether the node
// is to set it: for a burstable machine, or one whose limit is to be
// lifted.
func (c *credits) account(uid string, at, boot time.Time, cpuTime, maxSpan, poll time.Duration) (milliCPU int64, set bool, dropped string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.ledgers[uid]
	if !ok {
		return 0, false, ""
	}
	dropped = l.account(at, boot, cpuTime, maxSpan)
	return l.cpuLimit(poll), l.terms.burstable() || l.lift, dropped
}

// followed are the machines whose units the ledgers set a limit on: the
// burstable ones, and those whose limit is to be lifted.
func (c *credits) followed() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]bool{}
	for uid, l := range c.ledgers {
		if l.terms.burstable() || l.lift {
			out[uid] = true
		}
	}
	return out
}

// lifted records that a machine's unit, no longer burstable, holds no
// limit.
func (c *credits) lifted(uid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l, ok := c.ledgers[uid]; ok && !l.terms.burstable() {
		l.lift = false
	}
}

// due are the checkpoints to write now.
func (c *credits) due(now time.Time, every time.Duration) []checkpoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []checkpoint
	for _, uid := range slices.Sorted(maps.Keys(c.ledgers)) {
		if l := c.ledgers[uid]; l.due(now, every) {
			out = append(out, checkpoint{uid: uid, key: l.key, balance: l.balance, asOf: l.asOf, epoch: l.epoch})
		}
	}
	return out
}

// wrote records a checkpoint written to its machine's status at now.
func (c *credits) wrote(cp checkpoint, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.ledgers[cp.uid]
	if !ok || cp.epoch != l.epoch {
		return
	}
	l.unwritten = false
	if cp.asOf.After(l.written) {
		l.written = cp.asOf
	}
	l.writtenAt = now
}

// creditView is what the node answers of a burstable machine's credits.
type creditView struct {
	baseline       int32
	balance, limit float64
	held           bool
}

// view is the machine's credits, and whether it is burstable with a
// ledger.
func (c *credits) view(uid string, poll time.Duration) (creditView, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.ledgers[uid]
	if !ok || !l.terms.burstable() {
		return creditView{}, false
	}
	return creditView{baseline: l.terms.baseline, balance: l.balance, limit: l.terms.limit(), held: l.held(poll)}, true
}

// accountCredits charges a running machine's credits for what its unit
// used, read at at, and holds its unit to its baseline, or lifts its
// limit, as its balance says.
func (a *Agent) accountCredits(ctx context.Context, vm driver.VM, at time.Time, cpuTime time.Duration) {
	poll := a.cfg.UsagePoll
	milliCPU, set, dropped := a.credits.account(vm.MachineUID, at, vm.Started, cpuTime, 2*poll, poll)
	if dropped != "" {
		a.metrics.creditSpansDropped.WithLabelValues(dropped).Inc()
		a.cfg.Log.Debug("a span of the machine's CPU went uncharged", "machine_uid", vm.MachineUID, "reason", dropped)
	}
	if !set {
		return
	}
	if err := a.cfg.Drivers.Hypervisor.SetCPULimit(ctx, vm.MachineUID, milliCPU); err != nil {
		a.cfg.Log.Warn("setting the machine's CPU limit", "machine_uid", vm.MachineUID, "milli_cpu", milliCPU, "error", err)
		return
	}
	if milliCPU == 0 {
		a.credits.lifted(vm.MachineUID)
	}
}

// liftCPULimit lifts the CPU limit of the running VM of a machine that is
// not burstable; the driver writes nothing where its unit has none.
func (a *Agent) liftCPULimit(ctx context.Context, uid string) {
	if err := a.cfg.Drivers.Hypervisor.SetCPULimit(ctx, uid, 0); err != nil {
		a.cfg.Log.Warn("lifting the machine's CPU limit", "machine_uid", uid, "error", err)
		return
	}
	a.credits.lifted(uid)
}

// creditLoop accounts the burstable machines' credits every UsagePoll, in
// a pass of its own that reads only their units' CPU time: the usage
// sampler waits on guests, and the checkpoints on the API, and a pass
// slower than two polls would charge nothing (ledger.account).
func (a *Agent) creditLoop(ctx context.Context) {
	ticker := a.cfg.Clock.NewTicker(a.cfg.UsagePoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.Chan():
		}
		a.accountAll(ctx)
	}
}

// accountAll accounts each running machine a ledger follows from its
// unit's CPU time, and records which run.
func (a *Agent) accountAll(ctx context.Context) {
	hv := a.cfg.Drivers.Hypervisor
	vms, err := hv.VMs(ctx)
	if err != nil {
		a.cfg.Log.Debug("listing VMs to account their CPU credits", "error", err)
		return
	}
	running := map[string]driver.VM{}
	for _, vm := range vms {
		if vm.State == driver.VMRunning && !vm.FileShareServer {
			running[vm.MachineUID] = vm
		}
	}
	a.credits.sampled(running)
	for uid := range a.credits.followed() {
		vm, ok := running[uid]
		if !ok || ctx.Err() != nil {
			continue
		}
		used, err := hv.Usage(ctx, uid)
		if err != nil {
			a.cfg.Log.Debug("reading the machine's CPU time for its credits", "machine_uid", uid, "error", err)
			continue
		}
		a.accountCredits(ctx, vm, a.cfg.Clock.Now(), used.CPU)
	}
}

// creditWriteLoop writes the checkpoints that are due every UsagePoll, and
// as the agent stops, every balance accounted past the one written.
func (a *Agent) creditWriteLoop(ctx context.Context) {
	ticker := a.cfg.Clock.NewTicker(a.cfg.UsagePoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// ctx is done: the last writes get a bound of their own.
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), creditFlushTimeout)
			defer cancel()
			a.writeCredits(final, 0)
			return
		case <-ticker.Chan():
		}
		a.writeCredits(ctx, a.cfg.CreditCheckpoint)
	}
}

// creditFlushTimeout bounds the checkpoints written as the agent stops.
const creditFlushTimeout = 10 * time.Second

// writeCredits writes each burstable machine's balance to its status where
// it is due (ledger.due), a running machine's every apart. A write that
// fails, as on a conflict, is tried at the next pass; until then the
// ledger in memory goes on.
func (a *Agent) writeCredits(ctx context.Context, every time.Duration) {
	for _, cp := range a.credits.due(a.cfg.Clock.Now(), every) {
		if ctx.Err() != nil {
			return
		}
		m, err := a.getMachine(ctx, cp.key)
		if err != nil || m == nil || m.GetMetadata().GetUid() != cp.uid {
			if err != nil {
				a.cfg.Log.Debug("reading the machine to write its CPU credits", "machine_uid", cp.uid, "error", err)
			}
			continue
		}
		status := &computev1alpha1.MachineStatus{}
		if m.GetStatus() != nil {
			status = proto.Clone(m.GetStatus()).(*computev1alpha1.MachineStatus)
		}
		status.CpuCredits = cp.status()
		rv := m.GetMetadata().GetResourceVersion()
		_, err = a.machines.UpdateMachineStatus(ctx, connect.NewRequest(&computev1alpha1.UpdateMachineStatusRequest{
			Project: cp.key.Project, Name: cp.key.Name, Status: status, ExpectedResourceVersion: &rv,
		}))
		if err := a.metrics.statusWritten(err); err != nil {
			a.cfg.Log.Debug("writing the machine's CPU credits", "machine_uid", cp.uid, "error", err)
			continue
		}
		a.credits.wrote(cp, a.cfg.Clock.Now())
		if !a.credits.heldNow(cp.uid, a.cfg.UsagePoll) {
			// Launch credits written: the VM need not wait a poll for them.
			if err := a.cfg.Drivers.Hypervisor.SetCPULimit(ctx, cp.uid, 0); err != nil {
				a.cfg.Log.Debug("lifting the machine's CPU limit", "machine_uid", cp.uid, "error", err)
			}
		}
	}
}

// heldNow says whether the machine is burstable and held to its baseline.
func (c *credits) heldNow(uid string, poll time.Duration) bool {
	v, ok := c.view(uid, poll)
	return !ok || v.held
}
