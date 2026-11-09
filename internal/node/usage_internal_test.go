package node

import (
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
)

// usageHost is an agent on a fake host, its clock a fake one at 12:00:05,
// running a container, ct, of 2 vCPUs and 1Gi, and a firmware VM, vm, of
// 1 vCPU and 512Mi that its restart policy keeps down once it exits.
type usageHost struct {
	a     *Agent
	host  *fake.Host
	clock *clockwork.FakeClock
}

func newUsageHost(t *testing.T) *usageHost {
	t.Helper()
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
	vol, err := host.Storage().EnsureVolume(ctx, "d-vm", 1<<30, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []driver.VMSpec{
		{MachineUID: "ct", Machine: "p/ct", VCPUs: 2, MemoryMiB: 1024, Root: root},
		{MachineUID: "vm", Machine: "p/vm", VCPUs: 1, MemoryMiB: 512, Disks: []driver.Volume{vol}, Firmware: true, Restart: driver.RestartNever},
	} {
		if err := host.Hypervisor().Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
	}
	clock := clockwork.NewFakeClockAt(time.Date(2026, 11, 9, 12, 0, 5, 0, time.UTC))
	a := &Agent{
		cfg: Config{
			Name: "n1", UsagePoll: DefaultUsagePoll, Clock: clock, Log: slog.New(slog.DiscardHandler),
			Drivers: &Drivers{Storage: host.Storage(), Hypervisor: host.Hypervisor(), Guest: host.Guest()},
		},
		guests: newGuests(), usages: newUsages(),
	}
	return &usageHost{a: a, host: host, clock: clock}
}

// tick has the machines use cpu each of CPU time over the next poll, then
// samples them.
func (u *usageHost) tick(t *testing.T, cpu map[string]time.Duration) {
	t.Helper()
	for uid, d := range cpu {
		if err := u.host.RunCPU(uid, d); err != nil {
			t.Fatal(err)
		}
	}
	u.clock.Advance(DefaultUsagePoll)
	u.a.sampleUsage(t.Context())
}

func (u *usageHost) usage(t *testing.T, uid string) (*nodev1.MachineUsageResponse, error) {
	t.Helper()
	res, err := nodeService{a: u.a, agent: t.Context()}.MachineUsage(t.Context(), connect.NewRequest(&nodev1.MachineUsageRequest{MachineUid: uid}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestUsageIsAMinutesMean: a sample's CPU is the CPU time its unit used
// since the last over the time passed and its vCPUs, at most all of them;
// a minute's point is its mean, by time, and its memory samples' mean, kept
// once a sample of a later minute comes. A container's memory is what its
// guest uses, a firmware VM's what its unit holds.
func TestUsageIsAMinutesMean(t *testing.T) {
	u := newUsageHost(t)
	u.host.UseGuestMemory("ct", 300<<20)
	u.host.HoldMemory("vm", 400<<20)
	// 12:00:15, the first sample of each: no CPU yet.
	u.tick(t, nil)
	res, err := u.usage(t, "ct")
	if err != nil {
		t.Fatal(err)
	}
	if res.GetLatest() != nil || len(res.GetPoints()) != 0 || res.GetMemorySource() != nodev1.MemorySource_MEMORY_SOURCE_GUEST_USED {
		t.Errorf("after one sample, the container's usage is %v", res)
	}
	// 12:00:25: ct 5s over 10s of 2 vCPUs is a quarter of them.
	u.tick(t, map[string]time.Duration{"ct": 5 * time.Second, "vm": 2 * time.Second})
	res, _ = u.usage(t, "ct")
	if l := res.GetLatest(); l == nil || !near(l.GetCpu(), 0.25) || l.GetMemoryBytes() != 300<<20 ||
		!l.GetTime().AsTime().Equal(time.Date(2026, 11, 9, 12, 0, 25, 0, time.UTC)) {
		t.Errorf("the container's latest is %v, want a quarter of its CPUs at 12:00:25 and 300Mi", l)
	}
	// 12:00:35, 12:00:45, 12:00:55: 40s over 10s of 2 vCPUs is past all of
	// them, so all; then 15s, 0s.
	u.host.UseGuestMemory("ct", 500<<20)
	u.tick(t, map[string]time.Duration{"ct": 40 * time.Second, "vm": 2 * time.Second})
	if l := mustUsage(t, u, "ct").GetLatest(); !near(l.GetCpu(), 1) {
		t.Errorf("a sample past all the vCPUs is %v of them, want 1", l.GetCpu())
	}
	u.tick(t, map[string]time.Duration{"ct": 15 * time.Second, "vm": 2 * time.Second})
	u.tick(t, map[string]time.Duration{"vm": 2 * time.Second})
	if p := mustUsage(t, u, "ct").GetPoints(); len(p) != 0 {
		t.Errorf("points %v before the minute ended", p)
	}
	// 12:01:05 ends the minute 12:00: ct's samples of it used 5+40+15+0s
	// of 40s on 2 vCPUs (the mean by time, not of the samples' clamped
	// shares); its memory 300Mi twice and 500Mi thrice.
	u.tick(t, map[string]time.Duration{"ct": 10 * time.Second, "vm": 2 * time.Second})
	ct := mustUsage(t, u, "ct")
	if len(ct.GetPoints()) != 1 {
		t.Fatalf("the container's points are %v, want one", ct.GetPoints())
	}
	p := ct.GetPoints()[0]
	if !p.GetTime().AsTime().Equal(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)) || !near(p.GetCpu(), 60.0/80) ||
		p.GetMemoryBytes() != (2*300<<20+3*500<<20)/5 {
		t.Errorf("the container's minute 12:00 is %v, want 0.75 of its CPUs and 420Mi", p)
	}
	vm := mustUsage(t, u, "vm")
	if vm.GetMemorySource() != nodev1.MemorySource_MEMORY_SOURCE_HOST_HELD || len(vm.GetPoints()) != 1 ||
		!near(vm.GetPoints()[0].GetCpu(), 0.2) || vm.GetPoints()[0].GetMemoryBytes() != 400<<20 {
		t.Errorf("the VM's usage is %v, want 0.2 of its CPU and the 400Mi its unit holds", vm)
	}
}

func mustUsage(t *testing.T, u *usageHost, uid string) *nodev1.MachineUsageResponse {
	t.Helper()
	res, err := u.usage(t, uid)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestUsageKeepsAnHour: at most 60 points are kept, the oldest dropped.
func TestUsageKeepsAnHour(t *testing.T) {
	u := newUsageHost(t)
	for range 90 * 6 {
		u.tick(t, map[string]time.Duration{"ct": time.Second})
	}
	pts := mustUsage(t, u, "ct").GetPoints()
	if len(pts) != usagePoints {
		t.Fatalf("kept %d points, want %d", len(pts), usagePoints)
	}
	// Samples from 12:00:15 to 13:30:05; the minute 13:30 is being
	// gathered.
	if first, last := pts[0].GetTime().AsTime(), pts[len(pts)-1].GetTime().AsTime(); !first.Equal(time.Date(2026, 11, 9, 12, 30, 0, 0, time.UTC)) ||
		!last.Equal(time.Date(2026, 11, 9, 13, 29, 0, 0, time.UTC)) {
		t.Errorf("points from %v to %v, want the hour from 12:30 to 13:29", first, last)
	}
	for _, p := range pts {
		if !near(p.GetCpu(), 0.05) {
			t.Errorf("point %v, want 0.05 of the CPUs", p)
		}
	}
}

// TestUsageForgetsAGoneMachine: a machine whose VM restarts keeps its
// points, its counter read again from its new boot; one whose VM stops or
// exits for good is forgotten, and asked of is refused.
func TestUsageForgetsAGoneMachine(t *testing.T) {
	u := newUsageHost(t)
	for range 7 {
		u.tick(t, map[string]time.Duration{"ct": time.Second, "vm": time.Second})
	}
	if len(mustUsage(t, u, "ct").GetPoints()) == 0 || len(mustUsage(t, u, "vm").GetPoints()) == 0 {
		t.Fatal("no points after a minute")
	}
	before := mustUsage(t, u, "ct").GetLatest()
	if err := u.host.Exit("ct", 1); err != nil {
		t.Fatal(err)
	}
	// The new boot's counter is past the old one's: only the boot tells
	// they cannot be taken one from the other.
	u.tick(t, map[string]time.Duration{"ct": 100 * time.Second})
	after := mustUsage(t, u, "ct")
	if len(after.GetPoints()) == 0 || !after.GetLatest().GetTime().AsTime().Equal(before.GetTime().AsTime()) {
		t.Errorf("after a restart, the container's usage is %v; its latest was %v", after, before)
	}
	u.tick(t, map[string]time.Duration{"ct": 4 * time.Second})
	if l := mustUsage(t, u, "ct").GetLatest(); !near(l.GetCpu(), 0.2) {
		t.Errorf("the restarted container's latest is %v, want 0.2 of its CPUs", l)
	}
	if err := u.host.Hypervisor().Stop(t.Context(), "ct"); err != nil {
		t.Fatal(err)
	}
	if err := u.host.Exit("vm", 0); err != nil {
		t.Fatal(err)
	}
	u.tick(t, nil)
	if _, ok := u.a.usages.view("ct", time.Time{}); ok {
		t.Error("a stopped machine's usage is kept")
	}
	if _, ok := u.a.usages.view("vm", time.Time{}); ok {
		t.Error("an exited machine's usage is kept")
	}
	for _, uid := range []string{"ct", "vm", "never"} {
		if _, err := u.usage(t, uid); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("the usage of %s, not running: %v, want FailedPrecondition", uid, err)
		}
	}
}

// TestUsageWithoutTheGuestsMemory: a container whose guest cannot say its
// memory has points of CPU alone, and its guest is not asked again in the
// same boot.
func TestUsageWithoutTheGuestsMemory(t *testing.T) {
	u := newUsageHost(t)
	u.host.OldMemoryGuest("ct", true)
	for range 7 {
		u.tick(t, map[string]time.Duration{"ct": time.Second})
	}
	res := mustUsage(t, u, "ct")
	if len(res.GetPoints()) != 1 || res.GetPoints()[0].MemoryBytes != nil || res.GetLatest().MemoryBytes != nil {
		t.Errorf("a guest that cannot say its memory has %v", res)
	}
	u.host.OldMemoryGuest("ct", false)
	u.tick(t, map[string]time.Duration{"ct": time.Second})
	if l := mustUsage(t, u, "ct").GetLatest(); l.MemoryBytes != nil {
		t.Errorf("a guest that could not say its memory was asked again in its boot: %v", l)
	}
	// Its next boot is asked again.
	if err := u.host.Exit("ct", 1); err != nil {
		t.Fatal(err)
	}
	u.tick(t, nil)
	u.tick(t, map[string]time.Duration{"ct": time.Second})
	if l := mustUsage(t, u, "ct").GetLatest(); l.GetMemoryBytes() != 256<<20 {
		t.Errorf("a guest booted again, which can say its memory, has %v; want 256Mi", l)
	}
}

// TestUsageSkipsASlowGuest: a guest the agent found slow is not asked its
// memory until it answers again.
func TestUsageSkipsASlowGuest(t *testing.T) {
	u := newUsageHost(t)
	u.a.host.Lock()
	u.a.guests.slow["ct"] = &slowGuest{}
	u.a.host.Unlock()
	u.tick(t, nil)
	u.tick(t, map[string]time.Duration{"ct": time.Second})
	if l := mustUsage(t, u, "ct").GetLatest(); l == nil || l.MemoryBytes != nil {
		t.Errorf("a slow guest's latest is %v, want CPU and no memory", l)
	}
	u.a.host.Lock()
	delete(u.a.guests.slow, "ct")
	u.a.host.Unlock()
	u.tick(t, map[string]time.Duration{"ct": time.Second})
	if l := mustUsage(t, u, "ct").GetLatest(); l.GetMemoryBytes() != 256<<20 {
		t.Errorf("a guest that answers again has %v; want 256Mi", l)
	}
}

// TestUsageQuietsAGuestThatTimesOut: a guest that let the sampler's call
// run out its time is not asked its memory for a minute, then is again.
func TestUsageQuietsAGuestThatTimesOut(t *testing.T) {
	u := newUsageHost(t)
	u.a.usages.guestTimeout = 10 * time.Millisecond
	letGo := u.host.HangGuest("ct")
	u.tick(t, nil) // 12:00:15, timed out
	letGo()
	for range 5 { // to 12:01:05
		u.tick(t, map[string]time.Duration{"ct": time.Second})
		if l := mustUsage(t, u, "ct").GetLatest(); l == nil || l.MemoryBytes != nil {
			t.Fatalf("a guest asked within a minute of timing out: latest %v", l)
		}
	}
	u.tick(t, map[string]time.Duration{"ct": time.Second}) // 12:01:15
	if l := mustUsage(t, u, "ct").GetLatest(); l.GetMemoryBytes() != 256<<20 {
		t.Errorf("a guest a minute after it timed out has %v; want 256Mi", l)
	}
}

// TestUsageAfterAGap: a sample more than two polls after the one before
// gives no CPU, so a gap's CPU time lands in no minute; the next sample
// goes on from it.
func TestUsageAfterAGap(t *testing.T) {
	u := newUsageHost(t)
	u.tick(t, nil)
	u.tick(t, map[string]time.Duration{"ct": 5 * time.Second}) // 12:00:25
	before := mustUsage(t, u, "ct").GetLatest()
	if err := u.host.RunCPU("ct", 60*time.Second); err != nil {
		t.Fatal(err)
	}
	u.clock.Advance(20 * time.Second)
	u.tick(t, nil) // 12:00:55, 30s after
	if l := mustUsage(t, u, "ct").GetLatest(); !l.GetTime().AsTime().Equal(before.GetTime().AsTime()) {
		t.Errorf("a sample after a gap gave CPU: latest %v, was %v", l, before)
	}
	u.tick(t, map[string]time.Duration{"ct": 10 * time.Second}) // 12:01:05
	res := mustUsage(t, u, "ct")
	if !near(res.GetLatest().GetCpu(), 0.5) {
		t.Errorf("the sample after the gap's is %v, want 0.5 of the CPUs", res.GetLatest())
	}
	// The minute 12:00 measured 12:00:15 to 12:00:25 alone.
	if p := res.GetPoints(); len(p) != 1 || !near(p[0].GetCpu(), 0.25) {
		t.Errorf("the minute with a gap is %v, want 0.25 of the CPUs", p)
	}
}

// TestUsageLatestGoesStale: the latest sample is not answered once three
// polls old, as when the sampler stopped reaching the machine.
func TestUsageLatestGoesStale(t *testing.T) {
	u := newUsageHost(t)
	u.tick(t, nil)
	u.tick(t, map[string]time.Duration{"ct": time.Second})
	u.clock.Advance(3 * DefaultUsagePoll)
	if l := mustUsage(t, u, "ct").GetLatest(); l == nil {
		t.Fatal("a latest sample three polls old is not answered")
	}
	u.clock.Advance(time.Second)
	if l := mustUsage(t, u, "ct").GetLatest(); l != nil {
		t.Errorf("a latest sample more than three polls old is answered: %v", l)
	}
}
