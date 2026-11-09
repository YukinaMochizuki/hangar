package node

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// DefaultUsagePoll is how often each running machine's usage is sampled by
// default.
const DefaultUsagePoll = 10 * time.Second

// usagePoints is how many minutes of a machine's usage are kept: an hour.
const usagePoints = 60

// usageQuiet is how long a guest that let a MemoryUsage run out its time
// is not asked its memory again, so one hung guest does not hold up every
// pass of the sampler.
const usageQuiet = time.Minute

// usageSample is a machine's usage at one time, or over one minute.
type usageSample struct {
	at time.Time
	// cpu is how busy its vCPUs were, from 0 to 1 of them all.
	cpu float64
	// memory is the memory it uses, in bytes, if hasMemory.
	memory    uint64
	hasMemory bool
}

// machineUsage is what the node keeps of one running machine's usage
// (docs/decisions/machine-usage.md).
type machineUsage struct {
	vcpus int32
	// hostMemory says its memory is what its unit holds on the host, not
	// what its guest says it uses: a firmware VM's, which has no guest
	// agent.
	hostMemory bool
	// guestCannot says its guest, in the boot started, cannot say its
	// memory, and is not asked again.
	guestCannot bool
	// guestQuietUntil is when its guest, which let a call run out its
	// time, may be asked its memory again.
	guestQuietUntil time.Time
	// started, at and cpuTime are its VM's boot, and when its counter was
	// last read and what it said, for the next sample's CPU.
	started time.Time
	at      time.Time
	cpuTime time.Duration
	read    bool
	// minute is the minute being gathered: the CPU time used and the time
	// passed in it, and its memory samples' sum and count.
	minute       time.Time
	busy, span   time.Duration
	memSum, memN uint64
	// points are the minutes gathered, oldest first; latest the last
	// sample with a CPU value.
	points []usageSample
	latest *usageSample
}

// record takes a sample read at: the unit's CPU time since the VM, of
// vcpus, booted at started, and the memory, if hasMemory. A sample's CPU is
// the CPU time used since the one before over the time passed, in the boot
// both read, at most maxSpan before; the first of a boot, or one after a
// gap, has none, so a gap shows as minutes with no point rather than as
// its whole CPU time in the minute after it. A sample in a later minute
// than the one gathered ends that one.
func (m *machineUsage) record(at, started time.Time, cpuTime time.Duration, memory uint64, hasMemory bool, maxSpan time.Duration) {
	minute := at.Truncate(time.Minute)
	if !m.minute.IsZero() && minute.After(m.minute) {
		m.endMinute()
	}
	if m.minute.IsZero() || minute.After(m.minute) {
		m.minute = minute
	}
	s := usageSample{at: at, memory: memory, hasMemory: hasMemory}
	if m.read && started.Equal(m.started) && cpuTime >= m.cpuTime && at.After(m.at) && at.Sub(m.at) <= maxSpan {
		busy, span := cpuTime-m.cpuTime, at.Sub(m.at)
		m.busy += busy
		m.span += span
		s.cpu = cpuShare(busy, span, m.vcpus)
		m.latest = &s
	}
	if hasMemory {
		m.memSum += memory
		m.memN++
	}
	m.started, m.at, m.cpuTime, m.read = started, at, cpuTime, true
}

// endMinute keeps the minute gathered as a point, if any CPU time was
// measured in it, the oldest dropped past usagePoints.
func (m *machineUsage) endMinute() {
	if m.span > 0 {
		p := usageSample{at: m.minute, cpu: cpuShare(m.busy, m.span, m.vcpus)}
		if m.memN > 0 {
			p.memory, p.hasMemory = m.memSum/m.memN, true
		}
		m.points = append(m.points, p)
		if n := len(m.points); n > usagePoints {
			m.points = slices.Delete(m.points, 0, n-usagePoints)
		}
	}
	m.busy, m.span, m.memSum, m.memN = 0, 0, 0, 0
}

// cpuShare is busy CPU time over span on vcpus vCPUs, from 0 to 1.
func cpuShare(busy, span time.Duration, vcpus int32) float64 {
	if span <= 0 {
		return 0
	}
	share := float64(busy) / (float64(span) * float64(max(vcpus, 1)))
	return min(max(share, 0), 1)
}

// usageView is what the node answers of a machine's usage.
type usageView struct {
	points []usageSample
	latest *usageSample
}

// usages is the usage of every running machine the sampler saw.
type usages struct {
	mu       sync.Mutex
	machines map[string]*machineUsage
	// guestTimeout bounds asking a guest its memory: guestTimeout but in
	// tests.
	guestTimeout time.Duration
}

func newUsages() *usages {
	return &usages{machines: map[string]*machineUsage{}, guestTimeout: guestTimeout}
}

// view is what is kept of the machine's usage, and whether anything is;
// its latest sample only if it was taken at staleBefore or after.
func (u *usages) view(uid string, staleBefore time.Time) (usageView, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	m, ok := u.machines[uid]
	if !ok {
		return usageView{}, false
	}
	v := usageView{points: slices.Clone(m.points)}
	if m.latest != nil && !m.latest.at.Before(staleBefore) {
		l := *m.latest
		v.latest = &l
	}
	return v, true
}

// usageLoop samples every running machine's usage every UsagePoll.
func (a *Agent) usageLoop(ctx context.Context) {
	ticker := a.cfg.Clock.NewTicker(a.cfg.UsagePoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.Chan():
		}
		a.sampleUsage(ctx)
	}
}

// sampleUsage samples each running machine's usage, off the host lock,
// and forgets the machines no longer running. A container's guest that
// let a call run out its time is not asked its memory until it answers
// again (guestReady), nor one that let the sampler's own call run out its
// time, for usageQuiet.
func (a *Agent) sampleUsage(ctx context.Context) {
	vms, err := a.cfg.Drivers.Hypervisor.VMs(ctx)
	if err != nil {
		a.cfg.Log.Debug("listing VMs to sample their usage", "error", err)
		return
	}
	running := map[string]driver.VM{}
	for _, vm := range vms {
		if vm.State == driver.VMRunning && !vm.FileShareServer {
			running[vm.MachineUID] = vm
		}
	}
	u := a.usages
	u.mu.Lock()
	for uid := range u.machines {
		if _, ok := running[uid]; !ok {
			delete(u.machines, uid)
		}
	}
	u.mu.Unlock()
	a.host.Lock()
	slow := map[string]bool{}
	for uid := range a.guests.slow {
		slow[uid] = true
	}
	a.host.Unlock()
	for _, uid := range slices.Sorted(maps.Keys(running)) {
		if ctx.Err() != nil {
			return
		}
		a.sampleMachine(ctx, running[uid], slow[uid])
	}
}

// sampleMachine samples one running machine's usage.
func (a *Agent) sampleMachine(ctx context.Context, vm driver.VM, slowGuest bool) {
	uid := vm.MachineUID
	used, err := a.cfg.Drivers.Hypervisor.Usage(ctx, uid)
	if err != nil {
		a.cfg.Log.Debug("reading the machine's usage", "machine_uid", uid, "error", err)
		return
	}
	at := a.cfg.Clock.Now()
	u := a.usages
	u.mu.Lock()
	m := u.machines[uid]
	if m == nil || !m.started.Equal(vm.Started) {
		// A boot of its own: its guest may say its memory now.
		if m == nil {
			m = &machineUsage{}
			u.machines[uid] = m
		}
		m.guestCannot = false
	}
	m.vcpus, m.hostMemory = vm.VCPUs, vm.Firmware
	askGuest := !m.hostMemory && !m.guestCannot && !slowGuest && !at.Before(m.guestQuietUntil)
	timeout := u.guestTimeout
	u.mu.Unlock()

	memory, hasMemory := used.Memory, vm.Firmware
	cannot, quiet := false, false
	if askGuest {
		gctx, cancel := context.WithTimeout(ctx, timeout)
		mem, err := a.cfg.Drivers.Guest.MemoryUsage(gctx, uid)
		cancel()
		switch {
		case errors.Is(err, driver.ErrGuestCannot):
			cannot = true
		case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
			quiet = true
			a.cfg.Log.Debug("the machine's guest did not say its memory in time", "machine_uid", uid)
		case err != nil:
			a.cfg.Log.Debug("asking the machine's guest its memory", "machine_uid", uid, "error", err)
		default:
			memory, hasMemory = mem.Total-min(mem.Available, mem.Total), true
		}
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	m.guestCannot = m.guestCannot || cannot
	if quiet {
		m.guestQuietUntil = at.Add(usageQuiet)
	}
	m.record(at, vm.Started, used.CPU, memory, hasMemory, 2*a.cfg.UsagePoll)
}

func (s nodeService) MachineUsage(ctx context.Context, req *connect.Request[nodev1.MachineUsageRequest]) (*connect.Response[nodev1.MachineUsageResponse], error) {
	if s.a.cfg.Drivers == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this node runs no machines"))
	}
	uid := req.Msg.GetMachineUid()
	if uid == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("machine_uid is required"))
	}
	vm, err := s.a.vmOf(ctx, uid)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	if vm == nil || vm.State != driver.VMRunning || vm.FileShareServer {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("machine %s has no running VM on this node", uid))
	}
	res := &nodev1.MachineUsageResponse{MemorySource: nodev1.MemorySource_MEMORY_SOURCE_GUEST_USED}
	if vm.Firmware {
		res.MemorySource = nodev1.MemorySource_MEMORY_SOURCE_HOST_HELD
	}
	// A machine the sampler has not seen yet has nothing to say, and a
	// latest sample more than three polls old is no longer the latest.
	stale := s.a.cfg.Clock.Now().Add(-3 * s.a.cfg.UsagePoll)
	if v, ok := s.a.usages.view(uid, stale); ok {
		for _, p := range v.points {
			res.Points = append(res.Points, usagePoint(p))
		}
		if v.latest != nil {
			res.Latest = usagePoint(*v.latest)
		}
	}
	return connect.NewResponse(res), nil
}

func usagePoint(s usageSample) *nodev1.UsagePoint {
	p := &nodev1.UsagePoint{Time: timestamppb.New(s.at), Cpu: s.cpu}
	if s.hasMemory {
		p.MemoryBytes = &s.memory
	}
	return p
}
