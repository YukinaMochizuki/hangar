package node

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
)

// creditPoll is the usage poll the ledger tests take reads at.
const creditPoll = 10 * time.Second

var (
	creditT0   = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	creditBoot = creditT0.Add(-time.Hour)
	// t220 is a burstable machine of 2 vCPUs at 20%: it earns 0.4
	// CPU-minutes a minute, holds at most 576, and gets 12 to start.
	t220 = creditTerms{vcpus: 2, baseline: 20}
	// granted is a launch decision granting them, a year before creditT0.
	granted = &computev1alpha1.CPULaunchDecision{Granted: true, Decided: timestamppb.New(creditT0.AddDate(-1, 0, 0))}
)

// stored is a status's credits of balance as of at.
func stored(balance float64, at time.Time) *computev1alpha1.MachineCPUCredits {
	return &computev1alpha1.MachineCPUCredits{Balance: balance, AsOf: timestamppb.New(at)}
}

// reads has the ledger read its unit every creditPoll for d, from at and
// cpu, the unit busy cores CPUs of the time, in creditBoot; it returns the
// time and the CPU time read last.
func reads(l *ledger, at time.Time, cpu time.Duration, d time.Duration, cores float64) (time.Time, time.Duration) {
	for end := at.Add(d); at.Before(end); {
		at = at.Add(creditPoll)
		cpu += time.Duration(cores * float64(creditPoll))
		l.account(at, creditBoot, cpu, 2*creditPoll)
	}
	return at, cpu
}

// TestALedgerEarnsAndSpendsItsCredits: each minute a burstable machine
// earns its vCPUs times its baseline and spends what its unit used, the
// balance held between zero and a day of what it earns.
func TestALedgerEarnsAndSpendsItsCredits(t *testing.T) {
	l := newLedger(controller.Key{}, t220, granted, stored(10, creditT0), creditT0)
	if l.unwritten || !near(l.balance, 10) {
		t.Fatalf("read back, the ledger is %+v, want 10 written", l)
	}
	l.account(creditT0.Add(creditPoll), creditBoot, time.Minute, 2*creditPoll)
	if !near(l.balance, 10) {
		t.Errorf("the first read after a restart charged or earned: %v, want 10", l.balance)
	}
	at, cpu := reads(l, creditT0.Add(creditPoll), time.Minute, 10*time.Minute, 0)
	if !near(l.balance, 14) {
		t.Errorf("idle for 10 minutes, the balance is %v, want 10 + 10 × 0.4 = 14", l.balance)
	}
	at, cpu = reads(l, at, cpu, 5*time.Minute, 2)
	if !near(l.balance, 14-5*(2-0.4)) {
		t.Errorf("both vCPUs busy for 5 minutes, the balance is %v, want 14 − 5 × 1.6 = 6", l.balance)
	}
	at, cpu = reads(l, at, cpu, 10*time.Minute, 2)
	if l.balance != 0 {
		t.Errorf("busy past its credits, the balance is %v, want 0", l.balance)
	}
	_, _ = reads(l, at, cpu, 3*24*time.Hour, 0)
	if !near(l.balance, 576) {
		t.Errorf("idle for 3 days, the balance is %v, want its limit, 576", l.balance)
	}
}

// TestALedgerHoldsItsMachineOnceItsCreditsAreSpent: the machine is held to
// its baseline, in millicores, while its balance would not last one more
// poll at all its vCPUs, and runs free once it would.
func TestALedgerHoldsItsMachineOnceItsCreditsAreSpent(t *testing.T) {
	l := newLedger(controller.Key{}, t220, granted, stored(1, creditT0), creditT0)
	if l.held(creditPoll) || l.cpuLimit(creditPoll) != 0 {
		t.Errorf("with 1 CPU-minute, it is held to %d millicores", l.cpuLimit(creditPoll))
	}
	// A poll at both vCPUs takes a third of a CPU-minute.
	l.balance = 0.3
	if got := l.cpuLimit(creditPoll); got != 400 {
		t.Errorf("with 0.3 CPU-minutes, it is held to %d millicores, want 400", got)
	}
	l.balance = 0.34
	if l.held(creditPoll) {
		t.Error("with credits for a poll at both vCPUs, it is held")
	}
	// Held at its baseline, a busy machine earns what it spends: it stays
	// held rather than running free every other poll.
	l.balance = 0
	l.account(creditT0.Add(creditPoll), creditBoot, time.Minute, 2*creditPoll)
	at, _ := reads(l, creditT0.Add(creditPoll), time.Minute, time.Hour, 0.4)
	if !l.held(creditPoll) {
		t.Errorf("busy at its baseline for an hour, it was let go: %v at %v", l.balance, at)
	}
}

// TestALedgerGivesLaunchCreditsOnce: a machine with no balance gets 30
// minutes of what it earns, held to its baseline until they are written to
// its status, so an agent that restarts before writing them never gives
// them twice; a write of a balance from before them does not count.
func TestALedgerGivesLaunchCreditsOnce(t *testing.T) {
	c := newCredits()
	m := burstableMachine("u1", 20, nil)
	c.track(m, creditT0)
	l := c.ledgers["u1"]
	if !near(l.balance, 12) || !l.unwritten || !l.held(creditPoll) {
		t.Fatalf("started, the ledger is %+v, want 12 unwritten and held", l)
	}
	if got := c.startLimit("u1", creditPoll); got != 400 {
		t.Errorf("its VM starts held to %d millicores, want 400", got)
	}
	due := c.due(creditT0, time.Hour)
	if len(due) != 1 || !near(due[0].balance, 12) {
		t.Fatalf("due: %+v, want its launch credits", due)
	}
	stale := due[0]
	stale.epoch--
	c.wrote(stale, creditT0)
	if !l.unwritten {
		t.Error("a balance from before the launch credits counted as writing them")
	}
	c.wrote(due[0], creditT0)
	if l.unwritten || l.held(creditPoll) {
		t.Errorf("written, the ledger is %+v, want it free", l)
	}
	if got := c.startLimit("u1", creditPoll); got != 0 {
		t.Errorf("its VM starts held to %d millicores, want none", got)
	}
	// What was written comes back as its status once the agent restarts:
	// no launch credits again.
	again := newCredits()
	again.track(burstableMachine("u1", 20, due[0].status()), creditT0.Add(time.Minute))
	if l := again.ledgers["u1"]; l.unwritten || !near(l.balance, 12) {
		t.Errorf("read back, the ledger is %+v, want the 12 written", l)
	}
}

// TestAMachineNotGrantedStartsFromNone: a machine the apiserver gave no
// launch credits starts from a balance of none, held to its baseline until
// it earns some; an agent that restarts before writing it, finding no
// checkpoint, gives it none either, nor after a week stopped.
func TestAMachineNotGrantedStartsFromNone(t *testing.T) {
	m := burstableMachine("u1", 20, nil)
	m.Spec.CpuLaunch = &computev1alpha1.CPULaunchDecision{Decided: timestamppb.New(creditT0)}
	for range 2 { // as the agent restarts
		c := newCredits()
		c.track(m, creditT0)
		l := c.ledgers["u1"]
		if l.balance != 0 || l.unwritten || !l.held(creditPoll) {
			t.Fatalf("not granted, the ledger is %+v, want 0 and held", l)
		}
		if got := c.startLimit("u1", creditPoll); got != 400 {
			t.Errorf("not granted, its VM starts held to %d millicores, want 400", got)
		}
	}
	l := newLedger(controller.Key{}, t220, nil, stored(500, creditT0.Add(-8*24*time.Hour)), creditT0)
	if l.balance != 0 || l.unwritten {
		t.Errorf("not granted, read back after 8 days, the ledger is %+v, want 0", l)
	}
}

// TestALedgerKeepsItsBalanceThroughAResize: a tier change or a resize keeps
// the balance, clipped to the new limit; a machine no longer burstable is
// lifted and accounted no more, and its first read once burstable again is
// charged nothing.
func TestALedgerKeepsItsBalanceThroughAResize(t *testing.T) {
	l := newLedger(controller.Key{}, creditTerms{vcpus: 4, baseline: 40}, granted, stored(2000, creditT0), creditT0)
	if !near(l.balance, 2000) {
		t.Fatalf("read back, %v, want 2000 of a limit of 2304", l.balance)
	}
	l.retarget(creditTerms{vcpus: 4, baseline: 20})
	if !near(l.balance, 1152) {
		t.Errorf("down to 20%%, the balance is %v, want its new limit, 1152", l.balance)
	}
	l.retarget(creditTerms{vcpus: 8, baseline: 20})
	if !near(l.balance, 1152) {
		t.Errorf("grown to 8 vCPUs, the balance is %v, want it kept, 1152", l.balance)
	}
	l.account(creditT0.Add(creditPoll), creditBoot, time.Minute, 2*creditPoll)
	l.retarget(creditTerms{vcpus: 8, baseline: 100})
	if !l.lift || l.held(creditPoll) {
		t.Errorf("at 100%%, the ledger is %+v, want it lifted and never held", l)
	}
	l.account(creditT0.Add(2*creditPoll), creditBoot, time.Hour, 2*creditPoll)
	if !near(l.balance, 1152) {
		t.Errorf("at 100%%, a busy hour charged it: %v", l.balance)
	}
	l.retarget(creditTerms{vcpus: 8, baseline: 20})
	l.account(creditT0.Add(3*creditPoll), creditBoot, 2*time.Hour, 2*creditPoll)
	if !near(l.balance, 1152) {
		t.Errorf("burstable again, its first read charged it: %v", l.balance)
	}
}

// TestALedgerChargesNoGap: a span the node did not see whole, a read more
// than two polls after the one before, one of another boot, or the first
// after the ledger was read back, is charged nothing and earns nothing.
func TestALedgerChargesNoGap(t *testing.T) {
	l := newLedger(controller.Key{}, t220, granted, stored(100, creditT0), creditT0)
	// The agent was down for an hour after writing it.
	l.account(creditT0.Add(time.Hour), creditBoot, 10*time.Hour, 2*creditPoll)
	if !near(l.balance, 100) {
		t.Errorf("after an hour unaccounted, the balance is %v, want 100", l.balance)
	}
	at := creditT0.Add(time.Hour)
	l.account(at.Add(time.Minute), creditBoot, 11*time.Hour, 2*creditPoll)
	if !near(l.balance, 100) {
		t.Errorf("after a minute between reads, the balance is %v, want 100", l.balance)
	}
	// The new boot's counter has passed the old one's: a read of it is no
	// read of the same unit all the same.
	l.account(at.Add(time.Minute+creditPoll), creditBoot.Add(time.Hour), 12*time.Hour, 2*creditPoll)
	if !near(l.balance, 100) {
		t.Errorf("a read of another boot was charged: %v, want 100", l.balance)
	}
	l.account(at.Add(time.Minute+2*creditPoll), creditBoot.Add(time.Hour), 12*time.Hour+creditPoll, 2*creditPoll)
	if want := 100 + 0.4/6 - 1.0/6; !near(l.balance, want) {
		t.Errorf("a poll at one busy vCPU, the balance is %v, want %v", l.balance, want)
	}
}

// TestALedgerNeverCountsTwice: the agent reads a balance back from the
// machine's status only when it has none in memory, however often the
// machine is reconciled with its older status; a read at or before the
// time the balance is accounted to is ignored, as is a status's balance
// from the future.
func TestALedgerNeverCountsTwice(t *testing.T) {
	c := newCredits()
	m := burstableMachine("u1", 20, stored(100, creditT0))
	c.track(m, creditT0)
	l := c.ledgers["u1"]
	l.account(creditT0.Add(creditPoll), creditBoot, time.Minute, 2*creditPoll)
	reads(l, creditT0.Add(creditPoll), time.Minute, 30*time.Minute, 2)
	spent := l.balance
	if !near(spent, 100-30*1.6) {
		t.Fatalf("busy for 30 minutes, the balance is %v, want 52", spent)
	}
	// Reconciled, stopped and started again, its status still the
	// checkpoint of 100: the ledger in memory stands.
	c.track(m, creditT0.Add(31*time.Minute))
	stopped := proto.Clone(m).(*computev1alpha1.Machine)
	stopped.Spec.RunState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	c.track(stopped, creditT0.Add(32*time.Minute))
	c.track(m, creditT0.Add(33*time.Minute))
	if c.ledgers["u1"] != l || !near(l.balance, spent) {
		t.Errorf("reconciled with its older status, the balance is %v, want %v", c.ledgers["u1"].balance, spent)
	}
	// A read no later than the last accounted.
	asOf := l.asOf
	l.account(asOf, creditBoot, 0, 2*creditPoll)
	l.account(asOf.Add(-creditPoll), creditBoot, 0, 2*creditPoll)
	if !near(l.balance, spent) || !l.asOf.Equal(asOf) {
		t.Errorf("a read at or before %v moved the ledger to %v as of %v", asOf, l.balance, l.asOf)
	}
	// A status from ahead of the clock accounts nothing until the clock
	// passes it.
	ahead := newLedger(controller.Key{}, t220, granted, stored(50, creditT0.Add(time.Hour)), creditT0)
	ahead.account(creditT0.Add(creditPoll), creditBoot, 0, 2*creditPoll)
	ahead.account(creditT0.Add(2*creditPoll), creditBoot, time.Hour, 2*creditPoll)
	if !near(ahead.balance, 50) {
		t.Errorf("before its as_of, the balance moved to %v", ahead.balance)
	}
}

// TestALedgerExpiresAfterAWeek: a balance last accounted more than 7 days
// before, whether read back or kept in memory while the machine was
// stopped, gives way to launch credits only for a grant decided after it:
// the grant that started it gives it none again, nor does a decision
// against.
func TestALedgerExpiresAfterAWeek(t *testing.T) {
	lastAccounted := creditT0.Add(-8 * 24 * time.Hour)
	regranted := &computev1alpha1.CPULaunchDecision{Granted: true, Decided: timestamppb.New(creditT0.Add(-time.Minute))}
	denied := &computev1alpha1.CPULaunchDecision{Decided: timestamppb.New(creditT0.Add(-time.Minute))}
	for _, tt := range []struct {
		name   string
		launch *computev1alpha1.CPULaunchDecision
		want   float64
	}{
		{"granted again", regranted, 12},
		{"by the grant before it", granted, 0},
		{"denied", denied, 0},
	} {
		l := newLedger(controller.Key{}, t220, tt.launch, stored(500, lastAccounted), creditT0)
		if !near(l.balance, tt.want) || l.unwritten != (tt.want > 0) {
			t.Errorf("read back after 8 days, %s, the ledger is %+v, want %v", tt.name, l, tt.want)
		}
		l = newLedger(controller.Key{}, t220, tt.launch, stored(500, lastAccounted), lastAccounted)
		l.account(lastAccounted.Add(creditPoll), creditBoot, 0, 2*creditPoll)
		l.account(creditT0, creditBoot.Add(8*24*time.Hour), 0, 2*creditPoll)
		if !near(l.balance, tt.want) || l.unwritten != (tt.want > 0) {
			t.Errorf("started again after 8 days kept in memory, %s, the ledger is %+v, want %v", tt.name, l, tt.want)
		}
	}
	kept := newLedger(controller.Key{}, t220, granted, stored(500, creditT0.Add(-6*24*time.Hour)), creditT0)
	if kept.unwritten || !near(kept.balance, 500) {
		t.Errorf("read back after 6 days, the ledger is %+v, want 500", kept)
	}
}

// TestALedgerIsWrittenWhenDue: a running machine's balance is written once
// each checkpoint, and a stopped one's at once, each only when accounted
// past what was written.
func TestALedgerIsWrittenWhenDue(t *testing.T) {
	c := newCredits()
	c.track(burstableMachine("u1", 20, stored(100, creditT0)), creditT0)
	l := c.ledgers["u1"]
	l.running = true
	if due := c.due(creditT0.Add(time.Hour), 5*time.Minute); len(due) != 0 {
		t.Errorf("accounted no further, due %+v", due)
	}
	l.account(creditT0.Add(creditPoll), creditBoot, 0, 2*creditPoll)
	if due := c.due(creditT0.Add(time.Minute), 5*time.Minute); len(due) != 0 {
		t.Errorf("a minute after it was read back, due %+v", due)
	}
	due := c.due(creditT0.Add(5*time.Minute), 5*time.Minute)
	if len(due) != 1 {
		t.Fatalf("a checkpoint later, due %+v, want one", due)
	}
	c.wrote(due[0], creditT0.Add(5*time.Minute))
	l.account(creditT0.Add(2*creditPoll), creditBoot, 0, 2*creditPoll)
	l.running = false
	if due := c.due(creditT0.Add(5*time.Minute+time.Second), 5*time.Minute); len(due) != 1 || !due[0].asOf.Equal(creditT0.Add(2*creditPoll)) {
		t.Errorf("stopped, due %+v, want its last balance at once", due)
	}
}

// TestALedgerSaysWhatSpanItDropped: a span of one boot past two polls, or
// whose counter went back, is dropped and says why; a new boot's first
// read starts a span, dropping none.
func TestALedgerSaysWhatSpanItDropped(t *testing.T) {
	l := newLedger(controller.Key{}, t220, granted, stored(100, creditT0), creditT0)
	if got := l.account(creditT0.Add(creditPoll), creditBoot, time.Hour, 2*creditPoll); got != "" {
		t.Errorf("the first read after a read back dropped %q", got)
	}
	if got := l.account(creditT0.Add(time.Minute), creditBoot, time.Hour, 2*creditPoll); got != spanLate {
		t.Errorf("a read 50 s after the last dropped %q, want %q", got, spanLate)
	}
	if got := l.account(creditT0.Add(time.Minute+creditPoll), creditBoot, time.Minute, 2*creditPoll); got != spanCounter {
		t.Errorf("a counter gone back dropped %q, want %q", got, spanCounter)
	}
	if got := l.account(creditT0.Add(time.Minute+2*creditPoll), creditBoot.Add(time.Hour), 0, 2*creditPoll); got != "" {
		t.Errorf("a new boot's first read dropped %q", got)
	}
	if !near(l.balance, 100) {
		t.Errorf("the dropped spans moved the balance to %v", l.balance)
	}
}

// TestAFileSharesServerIsNeverBurstable: a server's machine has no ledger,
// whatever its baseline says.
func TestAFileSharesServerIsNeverBurstable(t *testing.T) {
	m := burstableMachine("s1", 20, nil)
	if !creditTermsOf(m).burstable() {
		t.Fatal("a container's machine at 20% is not burstable")
	}
	m.Spec.Boot = &computev1alpha1.MachineSpec_FileShareServer{FileShareServer: &computev1alpha1.FileShareServerBoot{FileShare: "data", Size: "10Gi"}}
	if creditTermsOf(m).burstable() {
		t.Error("a file share's server at 20% is burstable")
	}
	c := newCredits()
	c.track(m, creditT0)
	if _, ok := c.ledgers["s1"]; ok {
		t.Error("a file share's server got a ledger")
	}
}

// TestACheckpointLandsOnlyOnItsOwnMachine: a balance due for a machine
// that was deleted is not written to a new machine of its name, whose uid
// differs; written to its own, it lands.
func TestACheckpointLandsOnlyOnItsOwnMachine(t *testing.T) {
	host := fake.New()
	agent := func(store *machineStore) *Agent {
		return &Agent{
			cfg: Config{
				UsagePoll: creditPoll, CreditCheckpoint: time.Hour, Clock: clockwork.NewFakeClockAt(creditT0), Log: slog.New(slog.DiscardHandler),
				Drivers: &Drivers{Hypervisor: host.Hypervisor()},
			},
			machines: store, metrics: newAgentMetrics(), credits: newCredits(),
		}
	}
	old := burstableMachine("old", 20, nil)
	successor := burstableMachine("new", 20, nil)
	store := &machineStore{m: successor}
	a := agent(store)
	a.credits.track(old, creditT0)
	a.writeCredits(t.Context(), time.Hour)
	if st := store.m.GetStatus().GetCpuCredits(); st != nil {
		t.Errorf("the deleted machine's balance landed on its successor: %v", st)
	}
	own := &machineStore{m: burstableMachine("old", 20, nil)}
	a = agent(own)
	a.credits.track(old, creditT0)
	a.writeCredits(t.Context(), time.Hour)
	if st := own.m.GetStatus().GetCpuCredits(); !near(st.GetBalance(), 12) {
		t.Errorf("written to its own machine, the balance is %v, want its launch credits", st)
	}
}

// TestTheCreditPassReadsOnlyTheUnits: the credit pass charges a running
// burstable machine from its unit's CPU time and holds it once its credits
// are spent, with no guest driver to ask, so a slow guest cannot make its
// spans late.
func TestTheCreditPassReadsOnlyTheUnits(t *testing.T) {
	ctx := t.Context()
	host := fake.New()
	fill := func(dir string) error { return os.WriteFile(filepath.Join(dir, "image"), []byte("app"), 0o600) }
	if err := host.Storage().EnsureBase(ctx, "sha256:a", 0, fill); err != nil {
		t.Fatal(err)
	}
	root, err := host.Storage().EnsureRoot(ctx, "ct", "sha256:a", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Hypervisor().Start(ctx, driver.VMSpec{MachineUID: "ct", Machine: "p/ct", VCPUs: 2, MemoryMiB: 1024, Root: root}); err != nil {
		t.Fatal(err)
	}
	clock := clockwork.NewFakeClockAt(creditT0)
	a := &Agent{
		cfg: Config{
			UsagePoll: creditPoll, CreditCheckpoint: time.Hour, Clock: clock, Log: slog.New(slog.DiscardHandler),
			Drivers: &Drivers{Hypervisor: host.Hypervisor()},
		},
		metrics: newAgentMetrics(), credits: newCredits(),
	}
	a.credits.track(burstableMachine("ct", 20, stored(5, creditT0)), creditT0)
	clock.Advance(creditPoll)
	a.accountAll(ctx)
	if err := host.RunCPU("ct", 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	clock.Advance(creditPoll)
	a.accountAll(ctx)
	if v, _ := a.credits.view("ct", creditPoll); v.balance != 0 || !v.held {
		t.Errorf("10 CPU-minutes spent of 5, the ledger is %+v, want 0 and held", v)
	}
	if got := host.CPULimit("ct"); got != 400 {
		t.Errorf("held, the VM's unit is held to %d millicores, want 400", got)
	}
}

// burstableMachine is machine uid of 2 vCPUs at baseline, granted launch
// credits, its status's credits st.
func burstableMachine(uid string, baseline int32, st *computev1alpha1.MachineCPUCredits) *computev1alpha1.Machine {
	return &computev1alpha1.Machine{
		Metadata: &metav1.ObjectMeta{Project: "p", Name: "m-" + uid, Uid: uid},
		Spec: &computev1alpha1.MachineSpec{
			Resources: &computev1alpha1.Resources{Cpu: "2", Memory: "1Gi", CpuBaseline: baseline},
			RunState:  computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING,
			// Granted, as the apiserver grants a tenant's first.
			CpuLaunch: granted,
		},
		Status: &computev1alpha1.MachineStatus{CpuCredits: st},
	}
}
