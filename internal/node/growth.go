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
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// A container's root disk and its Disk volumes grow while its VM runs: the
// zvol first (EnsureRoot, EnsureVolume), then the VM is told the device's
// size, then the guest grows the filesystem. The agent asks each running
// container whose root, or a Disk it mounts read-write, grows by itself how
// full each is, and asks the owners, through the machine's status, for the
// next size (docs/decisions/disk-autogrow.md).

// DefaultRootPoll is how often a running container whose root disk, or a
// volume, grows by itself is asked how full it is.
const DefaultRootPoll = 15 * time.Second

// resizeTimeout bounds telling a VM a disk's size: Cloud Hypervisor, the
// host's, answers once the guest has been told.
const resizeTimeout = 10 * time.Second

// grownKey names a disk of a machine's that grows while its VM runs: its
// root disk, with disk empty, or a Disk's volume, by the Disk's uid.
type grownKey struct {
	machine, disk string
}

// toldDisk is what a container's VM and guest took of one of its disks in
// one boot: its size, and whether both took it, the VM seeing the disk at
// that size and the guest's filesystem filling it.
type toldDisk struct {
	boot time.Time
	size int64
	took bool
}

// growing is a machine's disks being told their sizes off the host lock,
// or how the last try failed, to be tried again.
type growing struct {
	busy bool
	err  error
}

// diskGrowth is whether a container's root disk, or its volumes, grew while
// its VM ran, in one boot: cannot is the reason one does not, grew says one
// did.
type diskGrowth struct {
	boot   time.Time
	cannot string
	grew   bool
}

// growStep is one of a machine's disks to tell its size: the root disk if n
// is negative, else its n-th volume, whose guest grows its filesystem
// mounted read-write from device, or nothing if it has none.
type growStep struct {
	key    grownKey
	n      int
	size   int64
	device string
}

// rwDevice is the device the guest of spec mounts its n-th volume from
// read-write, as its boot disk names it; empty if it mounts it read-only
// alone.
func rwDevice(spec driver.VMSpec, n int) string {
	if slices.ContainsFunc(spec.Mounts, func(m driver.Mount) bool { return !m.Share.IsValid() && m.Disk == n && !m.ReadOnly }) {
		return "/dev/" + computeapi.VirtioDevice(computeapi.FirstContainerDisk+n)
	}
	return ""
}

// ensureDiskSizes has a running container's VM and guest take its root
// disk's size, and its volumes', grown since the VM started, without
// starting it again: the VM is told the size each device has, then its guest
// grows each filesystem it mounts read-write to it. Each is done once per
// boot and size, and not at all for a disk the VM booted at the size of,
// which its guest grew to as it booted. They are asked off the host lock,
// one VM's at a time, and the machine is checked again once they answer;
// meanwhile, and after a failure, which is tried again, it returns an error
// wrapping controller.ErrWaiting. A VM or guest that cannot is left as it
// is, to take the sizes at its next boot, which its RootGrowth or
// VolumeGrowth condition says. The caller holds the host lock.
func (a *Agent) ensureDiskSizes(ctx context.Context, spec driver.VMSpec, vm *driver.VM) error {
	uid := spec.MachineUID
	g := a.growings[uid]
	if g.busy {
		return fmt.Errorf("%w: machine %s's disks being grown", controller.ErrWaiting, spec.Machine)
	}
	need := func(k grownKey, size, started int64) bool {
		if t, ok := a.toldDisks[k]; ok && t.boot.Equal(vm.Started) && t.size == size {
			return false
		}
		if size == started {
			a.toldDisks[k] = toldDisk{boot: vm.Started, size: size, took: true}
			return false
		}
		return true
	}
	var steps []growStep
	if k := (grownKey{uid, ""}); need(k, spec.Root.Size, vm.RootSize) {
		steps = append(steps, growStep{key: k, n: -1, size: spec.Root.Size})
	}
	for n, v := range spec.Disks {
		started := int64(-1)
		if len(vm.DiskSizes) == len(spec.Disks) {
			started = vm.DiskSizes[n]
		}
		if k := (grownKey{uid, v.DiskUID}); need(k, v.Size, started) {
			steps = append(steps, growStep{key: k, n: n, size: v.Size, device: rwDevice(spec, n)})
		}
	}
	if len(steps) == 0 {
		delete(a.growings, uid)
		return nil
	}
	a.growings[uid] = growing{busy: true}
	project, name, _ := strings.Cut(spec.Machine, "/")
	key := controller.Key{Project: project, Name: name}
	boot := vm.Started
	// The reconcile's context is the controller's, which outlives it.
	a.guests.wg.Go(func() { a.growDisks(ctx, spec, boot, key, steps) })
	if g.err != nil {
		return fmt.Errorf("%w: machine %s's disks, tried again: %w", controller.ErrWaiting, spec.Machine, g.err)
	}
	return fmt.Errorf("%w: machine %s's disks being grown", controller.ErrWaiting, spec.Machine)
}

// growDisks tells the VM of spec, in its boot, each step's disk's size, and
// has its guest grow the filesystem, then records how it went and, unless
// it failed, checks the machine of key again. It takes the host lock only
// to record.
func (a *Agent) growDisks(ctx context.Context, spec driver.VMSpec, boot time.Time, key controller.Key, steps []growStep) {
	uid := spec.MachineUID
	d := a.cfg.Drivers
	told := map[grownKey]toldDisk{}
	root, vols := diskGrowth{boot: boot}, diskGrowth{boot: boot}
	var failed error
	for _, s := range steps {
		what, growth := "root disk", &root
		if s.n >= 0 {
			what, growth = "volume "+spec.Disks[s.n].DiskUID, &vols
		}
		res := toldDisk{boot: boot, size: s.size}
		rctx, cancel := context.WithTimeout(ctx, resizeTimeout)
		var seen int64
		var err error
		if s.n < 0 {
			seen, err = d.Hypervisor.ResizeRoot(rctx, uid)
		} else {
			seen, err = d.Hypervisor.ResizeVolume(rctx, uid, s.n)
		}
		cancel()
		if errors.Is(err, driver.ErrNoLiveResize) {
			a.cfg.Log.Warn("the machine's VM cannot be told a grown disk's size while it runs, as one started before it could; it sees the disk once it starts again",
				"machine", spec.Machine, "machine_uid", uid, "disk", what, "size", s.size)
			growth.cannot = computeapi.ReasonVMCannotResize
			told[s.key] = res
			continue
		}
		if err != nil {
			failed = fmt.Errorf("telling the VM its %s's size: %w", what, err)
			break
		}
		gctx, cancel := context.WithTimeout(ctx, guestTimeout)
		switch {
		case s.n < 0:
			err = d.Guest.GrowRoot(gctx, uid)
		case s.device != "":
			err = d.Guest.GrowVolume(gctx, uid, s.device)
		}
		cancel()
		if errors.Is(err, driver.ErrGuestCannot) {
			a.cfg.Log.Warn("the machine's guest cannot grow a filesystem while it runs, as one started before it could; it grows it once it starts again",
				"machine", spec.Machine, "machine_uid", uid, "disk", what, "size", s.size)
			growth.cannot = computeapi.ReasonGuestCannotGrow
			told[s.key] = res
			continue
		}
		if err != nil {
			failed = fmt.Errorf("growing the guest's filesystem on its %s: %w", what, err)
			break
		}
		a.cfg.Log.Info("the machine's disk grew while it runs", "machine", spec.Machine, "machine_uid", uid, "disk", what, "size", seen)
		res.took, growth.grew = true, true
		told[s.key] = res
	}
	a.host.Lock()
	// A machine gone meanwhile has nothing recorded.
	if a.growings[uid].busy {
		maps.Copy(a.toldDisks, told)
		if root.cannot != "" || root.grew {
			a.rootGrowths[uid] = root
		}
		if vols.cannot != "" || vols.grew {
			a.volumeGrowths[uid] = vols
		}
		a.growings[uid] = growing{err: failed}
	}
	a.host.Unlock()
	if failed == nil && ctx.Err() == nil && a.requeue != nil {
		a.requeue(key)
	}
}

// forgetGrowth drops what the agent holds of a machine's disks' growth, as
// when it is gone. The caller holds the host lock.
func (a *Agent) forgetGrowth(uid string) {
	maps.DeleteFunc(a.toldDisks, func(k grownKey, _ toldDisk) bool { return k.machine == uid })
	delete(a.growings, uid)
	delete(a.rootGrowths, uid)
	delete(a.volumeGrowths, uid)
	delete(a.growWatches, uid)
}

// growthConditions are the RootGrowth and VolumeGrowth conditions of a
// container machine m whose VM runs in the boot that started, as the agent
// found it, each where it has something to say other than what m has.
func (a *Agent) growthConditions(m *computev1alpha1.Machine, started time.Time) []*metav1.Condition {
	uid := m.GetMetadata().GetUid()
	var out []*metav1.Condition
	for _, c := range []*metav1.Condition{
		growthCondition(m, started, a.rootGrowths[uid], computeapi.RootGrowth, computeapi.ReasonRootGrew, map[string]string{
			computeapi.ReasonVMCannotResize:  "the VM, started before it could be told its root disk's size while it runs, sees the disk at its size once it starts again",
			computeapi.ReasonGuestCannotGrow: "the guest's init, from before it could, neither says how full its root disk is nor grows it while it runs: the disk does not grow by itself, and takes its size once the container starts again",
			computeapi.ReasonRootGrew:        "the root disk grew while the VM runs",
		}),
		growthCondition(m, started, a.volumeGrowths[uid], computeapi.VolumeGrowth, computeapi.ReasonVolumeGrew, map[string]string{
			computeapi.ReasonVMCannotResize:  "the VM, started before it could be told its volumes' sizes while it runs, sees its volumes at their sizes once it starts again",
			computeapi.ReasonGuestCannotGrow: "the guest's init, from before it could, neither says how full its volumes are nor grows them while it runs: they do not grow by themselves, and take their sizes once the container starts again",
			computeapi.ReasonVolumeGrew:      "a volume grew while the VM runs",
		}),
	} {
		if c != nil {
			out = append(out, c)
		}
	}
	return out
}

// growthCondition is a growth condition of type typ of m, whose VM runs in
// the boot that started, as g says: False with g's reason, or True, with
// grew, once a later boot grew a disk and m has the condition; nil while it
// has nothing to say. says gives each reason's message.
func growthCondition(m *computev1alpha1.Machine, started time.Time, g diskGrowth, typ, grew string, says map[string]string) *metav1.Condition {
	if !g.boot.Equal(started) {
		return nil
	}
	switch {
	case g.cannot != "":
		return &metav1.Condition{Type: typ, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: g.cannot, Message: says[g.cannot]}
	case g.grew && conditions.Find(m.GetStatus().GetConditions(), typ) != nil:
		return &metav1.Condition{Type: typ, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: grew, Message: says[grew]}
	}
	return nil
}

// growWatch is a running container whose root disk, or a volume it mounts
// read-write, grows by itself, as the agent last reconciled it: what its
// usage is judged by.
type growWatch struct {
	key  controller.Key
	boot time.Time
	// root is its root disk's growth, nil if it grows not; vols are the
	// volumes that do, in the order of its Disks.
	root *rootGrow
	vols []volumeGrow
	// wanted and disksWanted are the machine's status.disk_wanted and
	// status.disks_wanted as last read or written.
	wanted      string
	disksWanted []*computev1alpha1.MachineDiskWanted
	// rootCannot and volumesCannot say the guest cannot say how full its
	// root, or its volumes, are, in this boot.
	rootCannot, volumesCannot bool
}

// rootGrow is how a container's root disk grows: its growth, the
// machine's resources.disk in bytes, the root disk's size on the host, and
// whether the VM and guest took that size in this boot. Until they do, a
// filesystem full of what fits the size before says nothing of what the
// size after needs.
type rootGrow struct {
	growth  *computev1alpha1.DiskGrow
	disk    uint64
	have    int64
	settled bool
}

// volumeGrow is how a Disk a container mounts grows, as rootGrow is of its
// root: the Disk's name and growth, its status.size in bytes, its volume's
// size on the host, and the device its guest mounts it from read-write.
type volumeGrow struct {
	disk    string
	device  string
	growth  *storagev1alpha1.DiskGrow
	size    uint64
	have    int64
	settled bool
}

// watchGrowth keeps, or drops, what the usage poll needs of a container
// machine m whose reconcile observed obs, its VM started with spec, the
// Disks of whose volumes are disks, in its order. The caller holds the host
// lock.
func (a *Agent) watchGrowth(m *computev1alpha1.Machine, spec driver.VMSpec, disks []*storagev1alpha1.Disk, obs *observation) {
	uid := m.GetMetadata().GetUid()
	if obs == nil || obs.phase != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING || obs.started == nil {
		delete(a.growWatches, uid)
		return
	}
	boot := obs.started.AsTime()
	took := func(disk string, size int64) bool {
		t := a.toldDisks[grownKey{uid, disk}]
		return t.boot.Equal(boot) && t.size == size && t.took
	}
	w := &growWatch{
		key:  controller.Key{Project: m.GetMetadata().GetProject(), Name: m.GetMetadata().GetName()},
		boot: boot,
		// The poll's write, read back by this reconcile, comes first.
		wanted:      m.GetStatus().GetDiskWanted(),
		disksWanted: m.GetStatus().GetDisksWanted(),
	}
	res := m.GetSpec().GetResources()
	if disk, err := quantity.Bytes(res.GetDisk()); err == nil && res.GetDiskGrow() != nil {
		w.root = &rootGrow{
			growth: proto.Clone(res.GetDiskGrow()).(*computev1alpha1.DiskGrow),
			disk:   disk, have: spec.Root.Size, settled: took("", spec.Root.Size),
		}
	}
	for n, d := range disks {
		if n >= len(spec.Disks) || d.GetSpec().GetGrow() == nil {
			continue
		}
		dev := rwDevice(spec, n)
		size, err := statusSize(d)
		if dev == "" || err != nil {
			continue
		}
		v := spec.Disks[n]
		w.vols = append(w.vols, volumeGrow{
			disk: d.GetMetadata().GetName(), device: dev,
			growth: proto.Clone(d.GetSpec().GetGrow()).(*storagev1alpha1.DiskGrow),
			size:   uint64(size), have: v.Size, settled: took(v.DiskUID, v.Size),
		})
	}
	if w.root == nil && len(w.vols) == 0 {
		delete(a.growWatches, uid)
		return
	}
	if old, ok := a.growWatches[uid]; ok && old.boot.Equal(boot) {
		w.rootCannot, w.volumesCannot = old.rootCannot, old.volumesCannot
	}
	a.growWatches[uid] = w
}

// rootLoop asks, every RootPoll, each running container whose root disk, or
// a volume, grows by itself how full each is, until ctx ends.
func (a *Agent) rootLoop(ctx context.Context) {
	ticker := a.cfg.Clock.NewTicker(a.cfg.RootPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.Chan():
		}
		a.pollGrowth(ctx)
	}
}

// pollGrowth asks each watched container's guest how full its root, and
// each volume of it that grows, are, off the host lock, and writes its
// machine's status.disk_wanted and status.disks_wanted where they change.
func (a *Agent) pollGrowth(ctx context.Context) {
	a.host.Lock()
	watches := map[string]growWatch{}
	for uid, w := range a.growWatches {
		// One whose guest let a call run out its time is not asked again
		// until it answers (guestReady).
		if a.guests.slow[uid] == nil {
			watches[uid] = *w
		}
	}
	a.host.Unlock()
	for _, uid := range slices.Sorted(maps.Keys(watches)) {
		if ctx.Err() != nil {
			return
		}
		a.pollMachine(ctx, uid, watches[uid])
	}
}

// pollMachine asks the guest of the machine watched as w how full its disks
// that grow are, and writes what it asks of them where that changes.
func (a *Agent) pollMachine(ctx context.Context, uid string, w growWatch) {
	cannot := func(volumes bool) {
		a.cfg.Log.Warn("the machine's guest cannot say how full its disks are, as one started before it could; they do not grow by themselves until it starts again",
			"machine_uid", uid, "project", w.key.Project, "machine", w.key.Name, "volumes", volumes)
		a.host.Lock()
		if cur, ok := a.growWatches[uid]; ok && cur.boot.Equal(w.boot) {
			g := diskGrowth{boot: w.boot, cannot: computeapi.ReasonGuestCannotGrow}
			if volumes {
				cur.volumesCannot = true
				a.volumeGrowths[uid] = g
			} else {
				cur.rootCannot = true
				a.rootGrowths[uid] = g
			}
		}
		a.host.Unlock()
		// Its reconcile reports it.
		if a.requeue != nil {
			a.requeue(w.key)
		}
	}
	var root *driver.FSUsage
	if w.root != nil && !w.rootCannot {
		gctx, cancel := context.WithTimeout(ctx, guestTimeout)
		u, err := a.cfg.Drivers.Guest.RootUsage(gctx, uid)
		cancel()
		switch {
		case errors.Is(err, driver.ErrGuestCannot):
			cannot(false)
		case err != nil:
			a.cfg.Log.Debug("asking the machine's guest how full its root disk is", "machine_uid", uid, "error", err)
		default:
			root = &u
		}
	}
	vols := map[string]driver.FSUsage{}
	if !w.volumesCannot {
		for _, v := range w.vols {
			gctx, cancel := context.WithTimeout(ctx, guestTimeout)
			u, err := a.cfg.Drivers.Guest.VolumeUsage(gctx, uid, v.device)
			cancel()
			if errors.Is(err, driver.ErrGuestCannot) {
				cannot(true)
				break
			}
			if err != nil {
				a.cfg.Log.Debug("asking the machine's guest how full a volume is", "machine_uid", uid, "disk", v.disk, "error", err)
				continue
			}
			vols[v.disk] = u
		}
	}
	wanted, disksWanted := w.asks(root, vols, w.wanted, w.disksWanted)
	if wanted == w.wanted && sameDisksWanted(disksWanted, w.disksWanted) {
		return
	}
	if err := a.writeWanted(ctx, w, uid, root, vols); err != nil && ctx.Err() == nil {
		a.cfg.Log.Warn("asking for the machine's disks to grow", "machine_uid", uid, "error", err)
	}
}

// asks is what a container watched as w asks of its root disk and its
// volumes, its guest saying root of its root and vols of its volumes, by
// Disk name: what it asked, wanted and disksWanted, where nothing is said,
// and asks of no Disk it does not watch.
func (w growWatch) asks(root *driver.FSUsage, vols map[string]driver.FSUsage, wanted string, disksWanted []*computev1alpha1.MachineDiskWanted) (string, []*computev1alpha1.MachineDiskWanted) {
	if w.root != nil && root != nil {
		if want, ok := wantedSize(w.root.growth, w.root.disk, w.root.have, w.root.settled, *root, "spec.resources.disk_grow"); ok {
			wanted = want
		}
	}
	var out []*computev1alpha1.MachineDiskWanted
	for _, v := range w.vols {
		want := ""
		if i := slices.IndexFunc(disksWanted, func(d *computev1alpha1.MachineDiskWanted) bool { return d.GetDisk() == v.disk }); i >= 0 {
			want = disksWanted[i].GetSize()
		}
		if u, ok := vols[v.disk]; ok {
			if judged, ok := wantedSize(v.growth, v.size, v.have, v.settled, u, "spec.grow"); ok {
				want = judged
			}
		}
		if want != "" {
			out = append(out, &computev1alpha1.MachineDiskWanted{Disk: v.disk, Size: want})
		}
	}
	return wanted, out
}

func sameDisksWanted(a, b []*computev1alpha1.MachineDiskWanted) bool {
	return slices.EqualFunc(a, b, func(x, y *computev1alpha1.MachineDiskWanted) bool { return proto.Equal(x, y) })
}

// wantedSize is what a disk growing by g, of size bytes as its owner has
// it and have on the host, asks to grow to, its guest saying u of its
// filesystem: the next size while it is full enough and below its max,
// else none. ok is false where u says nothing: the guest's claim makes no
// sense, or the disk is still taking its last size, settled false. The
// guest is not trusted; a lie grows its own disk, which its tenant pays
// for, no further than its max. field is g's path, for errors.
func wantedSize[G computeapi.DiskGrowSpec](g G, size uint64, have int64, settled bool, u driver.FSUsage, field string) (string, bool) {
	if !settled || u.Size == 0 || u.Used > u.Size || have <= 0 || u.Size > uint64(have) {
		return "", false
	}
	dg, ok, err := computeapi.ParseDiskGrow(g, size, field)
	if err != nil || !ok {
		return "", false
	}
	if !dg.Due(u.Used, u.Size) || size >= dg.Max {
		return "", true
	}
	return quantity.FormatDisk(dg.Next(size)), true
}

// writeWanted writes the machine's status.disk_wanted and
// status.disks_wanted as its guest's usage, root of its root and vols of
// its volumes, asks, each judged by its owner's spec as read now, unless
// they say so already. A disk grown since it was watched keeps what it
// asked: the reconcile grows it, and watches it again. The write is
// conditional on the machine as read.
func (a *Agent) writeWanted(ctx context.Context, w growWatch, uid string, root *driver.FSUsage, vols map[string]driver.FSUsage) error {
	m, err := a.getMachine(ctx, w.key)
	if err != nil || m == nil || m.GetMetadata().GetUid() != uid || m.GetSpec().GetNode() != a.cfg.Name {
		return err
	}
	if w.root != nil {
		disk, err := quantity.Bytes(m.GetSpec().GetResources().GetDisk())
		if err != nil {
			return fmt.Errorf("the machine's disk: %w", err)
		}
		if g := m.GetSpec().GetResources().GetDiskGrow(); disk == w.root.disk && g != nil {
			w.root.growth = g
		} else {
			root = nil
		}
	}
	fresh := map[string]driver.FSUsage{}
	for i, v := range w.vols {
		u, ok := vols[v.disk]
		if !ok {
			continue
		}
		d, err := a.getDisk(ctx, w.key.Project, v.disk)
		if err != nil {
			return err
		}
		if d == nil || d.GetSpec().GetGrow() == nil {
			continue
		}
		if size, err := statusSize(d); err != nil || uint64(size) != v.size {
			continue
		}
		w.vols[i].growth = d.GetSpec().GetGrow()
		fresh[v.disk] = u
	}
	status := &computev1alpha1.MachineStatus{}
	if m.GetStatus() != nil {
		status = proto.Clone(m.GetStatus()).(*computev1alpha1.MachineStatus)
	}
	wanted, disksWanted := w.asks(root, fresh, status.GetDiskWanted(), status.GetDisksWanted())
	if wanted != status.GetDiskWanted() || !sameDisksWanted(disksWanted, status.GetDisksWanted()) {
		status.DiskWanted, status.DisksWanted = wanted, disksWanted
		rv := m.GetMetadata().GetResourceVersion()
		_, err = a.machines.UpdateMachineStatus(ctx, connect.NewRequest(&computev1alpha1.UpdateMachineStatusRequest{
			Project: w.key.Project, Name: w.key.Name, Status: status, ExpectedResourceVersion: &rv,
		}))
		if err = a.metrics.statusWritten(err); err != nil {
			return err
		}
	}
	a.host.Lock()
	if cur, ok := a.growWatches[uid]; ok && cur.boot.Equal(w.boot) {
		cur.wanted, cur.disksWanted = wanted, disksWanted
	}
	a.host.Unlock()
	return nil
}
