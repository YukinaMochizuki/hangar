package node

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// A container's root disk grows while its VM runs: the zvol first
// (EnsureRoot), then the VM is told the device's size, then the guest grows
// its filesystem. The agent asks each running container whose root grows by
// itself how full its root is, and asks its owner, through the machine's
// status, for the next size (docs/decisions/disk-autogrow.md).

// DefaultRootPoll is how often a running container whose root disk grows by
// itself is asked how full its root is.
const DefaultRootPoll = 15 * time.Second

// resizeTimeout bounds telling a VM its root disk's size: Cloud Hypervisor,
// the host's, answers once the guest has been told.
const resizeTimeout = 10 * time.Second

// toldRoot is what a container's VM and guest took of its root disk in one
// boot: its size, and whether both took it, the VM seeing the disk at that
// size and the guest's filesystem filling it. busy says it is being told,
// off the host lock; err is how the last try failed, to be tried again.
type toldRoot struct {
	boot time.Time
	size int64
	took bool
	busy bool
	err  error
}

// rootGrowth is whether a container's root disk grew while its VM ran, in
// one boot: cannot is the RootGrowth reason it does not, grew says it did.
type rootGrowth struct {
	boot   time.Time
	cannot string
	grew   bool
}

// ensureRootSize has a running container's VM and guest take its root
// disk's size, grown since the VM started, without starting it again: the
// VM is told the size its device has, then its guest grows its filesystem
// to it. Each is done once per boot and size, and not at all for a VM that
// booted at the size, which its guest grew to as it booted. Both are asked
// off the host lock, one VM's at a time, and the machine is checked again
// once they answer; meanwhile, and after a failure, which is tried again,
// it returns an error wrapping controller.ErrWaiting. A VM or guest that
// cannot is left as it is, to take the size at its next boot, which its
// RootGrowth condition says. The caller holds the host lock.
func (a *Agent) ensureRootSize(ctx context.Context, spec driver.VMSpec, vm *driver.VM) error {
	uid := spec.MachineUID
	size := spec.Root.Size
	t, ok := a.toldRoot[uid]
	switch {
	case ok && t.busy:
		return fmt.Errorf("%w: machine %s's root disk being grown", controller.ErrWaiting, spec.Machine)
	case ok && t.boot.Equal(vm.Started) && t.size == size && t.err == nil:
		return nil
	case size == vm.RootSize:
		a.toldRoot[uid] = toldRoot{boot: vm.Started, size: size, took: true}
		return nil
	}
	var last error
	if ok && t.boot.Equal(vm.Started) && t.size == size {
		last = t.err
	}
	a.toldRoot[uid] = toldRoot{boot: vm.Started, size: size, busy: true}
	project, name, _ := strings.Cut(spec.Machine, "/")
	key := controller.Key{Project: project, Name: name}
	boot := vm.Started
	// The reconcile's context is the controller's, which outlives it.
	a.guests.wg.Go(func() { a.growRoot(ctx, spec, boot, key) })
	if last != nil {
		return fmt.Errorf("%w: machine %s's root disk, tried again: %w", controller.ErrWaiting, spec.Machine, last)
	}
	return fmt.Errorf("%w: machine %s's root disk being grown", controller.ErrWaiting, spec.Machine)
}

// growRoot tells the VM of spec, in its boot, its root disk's size, and
// has its guest grow its filesystem, then records how it went and, unless
// it failed, checks the machine of key again. It takes the host lock only
// to record.
func (a *Agent) growRoot(ctx context.Context, spec driver.VMSpec, boot time.Time, key controller.Key) {
	uid, size := spec.MachineUID, spec.Root.Size
	d := a.cfg.Drivers
	res := toldRoot{boot: boot, size: size}
	growth := rootGrowth{boot: boot}
	rctx, cancel := context.WithTimeout(ctx, resizeTimeout)
	seen, err := d.Hypervisor.ResizeRoot(rctx, uid)
	cancel()
	switch {
	case errors.Is(err, driver.ErrNoLiveResize):
		a.cfg.Log.Warn("the machine's VM cannot be told its grown root disk while it runs, as one started before it could; it sees the disk once it starts again",
			"machine", spec.Machine, "machine_uid", uid, "size", size)
		growth.cannot = computeapi.ReasonVMCannotResize
	case err != nil:
		res.err = fmt.Errorf("telling the VM its root disk's size: %w", err)
	default:
		gctx, cancel := context.WithTimeout(ctx, guestTimeout)
		err = d.Guest.GrowRoot(gctx, uid)
		cancel()
		switch {
		case errors.Is(err, driver.ErrGuestCannot):
			a.cfg.Log.Warn("the machine's guest cannot grow its root filesystem while it runs, as one started before it could; it grows it once it starts again",
				"machine", spec.Machine, "machine_uid", uid, "size", size)
			growth.cannot = computeapi.ReasonGuestCannotGrow
		case err != nil:
			res.err = fmt.Errorf("growing the guest's root filesystem: %w", err)
		default:
			a.cfg.Log.Info("the machine's root disk grew while it runs", "machine", spec.Machine, "machine_uid", uid, "size", seen)
			res.took, growth.grew = true, true
		}
	}
	a.host.Lock()
	// A machine gone meanwhile has nothing recorded.
	if cur, ok := a.toldRoot[uid]; ok && cur.busy {
		a.toldRoot[uid] = res
		if growth.cannot != "" || growth.grew {
			a.rootGrowths[uid] = growth
		}
	}
	a.host.Unlock()
	if res.err == nil && ctx.Err() == nil && a.requeue != nil {
		a.requeue(key)
	}
}

// rootGrowthCondition is the RootGrowth condition of a container machine m
// whose VM runs in the boot that started, as the agent found it: nil while
// it has nothing to say, or what m has already.
func (a *Agent) rootGrowthCondition(m *computev1alpha1.Machine, started time.Time) *metav1.Condition {
	g, ok := a.rootGrowths[m.GetMetadata().GetUid()]
	if !ok || !g.boot.Equal(started) {
		return nil
	}
	switch {
	case g.cannot == computeapi.ReasonVMCannotResize:
		return &metav1.Condition{Type: computeapi.RootGrowth, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: g.cannot,
			Message: "the VM, started before it could be told its root disk's size while it runs, sees the disk at its size once it starts again"}
	case g.cannot == computeapi.ReasonGuestCannotGrow:
		return &metav1.Condition{Type: computeapi.RootGrowth, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: g.cannot,
			Message: "the guest's init, from before it could, neither says how full its root disk is nor grows it while it runs: the disk does not grow by itself, and takes its size once the container starts again"}
	case g.grew && conditions.Find(m.GetStatus().GetConditions(), computeapi.RootGrowth) != nil:
		return &metav1.Condition{Type: computeapi.RootGrowth, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: computeapi.ReasonRootGrew,
			Message: "the root disk grew while the VM runs"}
	}
	return nil
}

// rootWatch is a running container whose root disk grows by itself, as the
// agent last reconciled it: what its usage is judged by.
type rootWatch struct {
	key  controller.Key
	boot time.Time
	// growth is the machine's resources.disk_grow, and disk its
	// resources.disk in bytes; root is the root disk's size on the host.
	growth *computev1alpha1.DiskGrow
	disk   uint64
	root   int64
	// settled says the VM and guest took root in this boot: until they
	// do, a filesystem full of what fits the size before says nothing of
	// what the size after needs.
	settled bool
	// wanted is the machine's status.disk_wanted as last read or written.
	wanted string
	// cannot says the guest cannot say how full its root is, in this boot.
	cannot bool
}

// watchRoot keeps, or drops, what the usage poll needs of a container
// machine m whose reconcile observed obs, its VM started with spec. The
// caller holds the host lock.
func (a *Agent) watchRoot(m *computev1alpha1.Machine, spec driver.VMSpec, obs *observation) {
	uid := m.GetMetadata().GetUid()
	res := m.GetSpec().GetResources()
	disk, err := quantity.Bytes(res.GetDisk())
	if obs == nil || obs.phase != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING || obs.started == nil ||
		res.GetDiskGrow() == nil || err != nil {
		delete(a.rootWatches, uid)
		return
	}
	boot := obs.started.AsTime()
	told := a.toldRoot[uid]
	w := &rootWatch{
		key:    controller.Key{Project: m.GetMetadata().GetProject(), Name: m.GetMetadata().GetName()},
		boot:   boot,
		growth: proto.Clone(res.GetDiskGrow()).(*computev1alpha1.DiskGrow),
		disk:   disk,
		root:   spec.Root.Size,
		// The poll's write, read back by this reconcile, comes first.
		wanted:  m.GetStatus().GetDiskWanted(),
		settled: told.boot.Equal(boot) && told.size == spec.Root.Size && told.took,
	}
	if old, ok := a.rootWatches[uid]; ok && old.boot.Equal(boot) {
		w.cannot = old.cannot
	}
	a.rootWatches[uid] = w
}

// rootLoop asks, every RootPoll, each running container whose root disk
// grows by itself how full its root is, until ctx ends.
func (a *Agent) rootLoop(ctx context.Context) {
	ticker := a.cfg.Clock.NewTicker(a.cfg.RootPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.Chan():
		}
		a.pollRoots(ctx)
	}
}

// pollRoots asks each watched container's guest how full its root is, off
// the host lock, and writes its machine's status.disk_wanted where that
// changes.
func (a *Agent) pollRoots(ctx context.Context) {
	a.host.Lock()
	watches := map[string]rootWatch{}
	for uid, w := range a.rootWatches {
		// One whose guest let a call run out its time is not asked again
		// until it answers (guestReady).
		if !w.cannot && a.guests.slow[uid] == nil {
			watches[uid] = *w
		}
	}
	a.host.Unlock()
	for _, uid := range slices.Sorted(maps.Keys(watches)) {
		if ctx.Err() != nil {
			return
		}
		w := watches[uid]
		gctx, cancel := context.WithTimeout(ctx, guestTimeout)
		u, err := a.cfg.Drivers.Guest.RootUsage(gctx, uid)
		cancel()
		switch {
		case errors.Is(err, driver.ErrGuestCannot):
			a.cfg.Log.Warn("the machine's guest cannot say how full its root disk is, as one started before it could; its disk does not grow by itself until it starts again",
				"machine_uid", uid, "project", w.key.Project, "machine", w.key.Name)
			a.host.Lock()
			if cur, ok := a.rootWatches[uid]; ok && cur.boot.Equal(w.boot) {
				cur.cannot = true
				a.rootGrowths[uid] = rootGrowth{boot: w.boot, cannot: computeapi.ReasonGuestCannotGrow}
			}
			a.host.Unlock()
			// Its reconcile reports it.
			if a.requeue != nil {
				a.requeue(w.key)
			}
			continue
		case err != nil:
			a.cfg.Log.Debug("asking the machine's guest how full its root disk is", "machine_uid", uid, "error", err)
			continue
		}
		want, ok := wantedDisk(w, u)
		if !ok || want == w.wanted {
			continue
		}
		if err := a.writeDiskWanted(ctx, w, uid, u); err != nil && ctx.Err() == nil {
			a.cfg.Log.Warn("asking for the machine's root disk to grow", "machine_uid", uid, "error", err)
		}
	}
}

// wantedDisk is what a container watched as w asks its root disk to grow
// to, its guest saying u: the next size while the root is full enough and
// below its max, else none. ok is false where u says nothing: the guest's
// claim makes no sense, or the root is still taking its last size. The
// guest is not trusted; a lie grows its own disk, which its tenant pays
// for, no further than its max.
func wantedDisk(w rootWatch, u driver.RootUsage) (string, bool) {
	if !w.settled || u.Size == 0 || u.Used > u.Size || w.root <= 0 || u.Size > uint64(w.root) {
		return "", false
	}
	dg, ok, err := computeapi.ParseDiskGrow(w.growth, w.disk, "spec.resources.disk_grow")
	if err != nil || !ok {
		return "", false
	}
	if !dg.Due(u.Used, u.Size) || w.disk >= dg.Max {
		return "", true
	}
	return quantity.FormatDisk(dg.Next(w.disk)), true
}

// writeDiskWanted writes the machine's status.disk_wanted as its guest's
// usage u asks, judged by its spec as read now, unless it says so already.
// The write is conditional on the machine as read.
func (a *Agent) writeDiskWanted(ctx context.Context, w rootWatch, uid string, u driver.RootUsage) error {
	m, err := a.getMachine(ctx, w.key)
	if err != nil || m == nil || m.GetMetadata().GetUid() != uid || m.GetSpec().GetNode() != a.cfg.Name {
		return err
	}
	disk, err := quantity.Bytes(m.GetSpec().GetResources().GetDisk())
	if err != nil {
		return fmt.Errorf("the machine's disk: %w", err)
	}
	if disk != w.disk {
		// Granted since: the reconcile grows the root, and watches it again.
		return nil
	}
	w.growth = m.GetSpec().GetResources().GetDiskGrow()
	want, ok := wantedDisk(w, u)
	if !ok {
		return nil
	}
	status := &computev1alpha1.MachineStatus{}
	if m.GetStatus() != nil {
		status = proto.Clone(m.GetStatus()).(*computev1alpha1.MachineStatus)
	}
	if status.GetDiskWanted() != want {
		status.DiskWanted = want
		rv := m.GetMetadata().GetResourceVersion()
		_, err = a.machines.UpdateMachineStatus(ctx, connect.NewRequest(&computev1alpha1.UpdateMachineStatusRequest{
			Project: w.key.Project, Name: w.key.Name, Status: status, ExpectedResourceVersion: &rv,
		}))
		if err = a.metrics.statusWritten(err); err != nil {
			return err
		}
	}
	a.host.Lock()
	if cur, ok := a.rootWatches[uid]; ok && cur.boot.Equal(w.boot) {
		cur.wanted = want
	}
	a.host.Unlock()
	return nil
}
