package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1/storagev1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
)

// machineStore is the apiserver's machines and disks as the usage poll
// reaches them: one machine, its status writes conditional on its resource
// version, each write's status.disk_wanted and status.disks_wanted kept,
// and its Disks.
type machineStore struct {
	computev1alpha1connect.MachineServiceClient
	storagev1alpha1connect.DiskServiceClient
	mu         sync.Mutex
	m          *computev1alpha1.Machine
	disks      map[string]*storagev1alpha1.Disk
	writes     []string
	diskWrites []string
}

func (s *machineStore) GetMachine(context.Context, *connect.Request[computev1alpha1.GetMachineRequest]) (*connect.Response[computev1alpha1.GetMachineResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return connect.NewResponse(&computev1alpha1.GetMachineResponse{Machine: proto.Clone(s.m).(*computev1alpha1.Machine)}), nil
}

func (s *machineStore) GetDisk(_ context.Context, req *connect.Request[storagev1alpha1.GetDiskRequest]) (*connect.Response[storagev1alpha1.GetDiskResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.disks[req.Msg.GetName()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("disk %s", req.Msg.GetName()))
	}
	return connect.NewResponse(&storagev1alpha1.GetDiskResponse{Disk: proto.Clone(d).(*storagev1alpha1.Disk)}), nil
}

func (s *machineStore) UpdateMachineStatus(_ context.Context, req *connect.Request[computev1alpha1.UpdateMachineStatusRequest]) (*connect.Response[computev1alpha1.UpdateMachineStatusResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Msg.GetExpectedResourceVersion() != s.m.GetMetadata().GetResourceVersion() {
		return nil, connect.NewError(connect.CodeAborted, fmt.Errorf("resource version %d, not %d", s.m.GetMetadata().GetResourceVersion(), req.Msg.GetExpectedResourceVersion()))
	}
	st := req.Msg.GetStatus()
	if st.GetDiskWanted() != s.m.GetStatus().GetDiskWanted() {
		s.writes = append(s.writes, st.GetDiskWanted())
	}
	if !sameDisksWanted(st.GetDisksWanted(), s.m.GetStatus().GetDisksWanted()) {
		var asks []string
		for _, d := range st.GetDisksWanted() {
			asks = append(asks, d.GetDisk()+"="+d.GetSize())
		}
		s.diskWrites = append(s.diskWrites, strings.Join(asks, ","))
	}
	s.m.Status = st
	s.m.Metadata.ResourceVersion++
	return connect.NewResponse(&computev1alpha1.UpdateMachineStatusResponse{Machine: s.m}), nil
}

// written is each write's status.disk_wanted, where it changed.
func (s *machineStore) written() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.writes)
}

// disksWritten is each write's status.disks_wanted, as disk=size joined by
// commas, where it changed.
func (s *machineStore) disksWritten() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.diskWrites)
}

// rootHost is an agent on a fake host running a container machine, m1,
// p/ct-web, with a 4Gi root that grows by itself 2x up to 20Gi at 80%, and
// two Disks that grow by themselves up to 8Gi: data, of 2Gi, mounted
// read-write at /data, the VM's vdc, and logs, of 1Gi, mounted read-only
// at /logs, its vdd. Its VM started at those sizes and was reconciled once;
// what the agent queued again is kept.
type rootHost struct {
	a     *Agent
	host  *fake.Host
	store *machineStore
	spec  driver.VMSpec
	vm    driver.VM
	disks []*storagev1alpha1.Disk

	mu       sync.Mutex
	requeued []controller.Key
}

func newRootHost(t *testing.T) *rootHost {
	t.Helper()
	ctx := t.Context()
	host := fake.New()
	fill := func(dir string) error { return os.WriteFile(filepath.Join(dir, "image"), []byte("app"), 0o600) }
	if err := host.Storage().EnsureBase(ctx, "sha256:a", 0, fill); err != nil {
		t.Fatal(err)
	}
	root, err := host.Storage().EnsureRoot(ctx, "m1", "sha256:a", 4<<30)
	if err != nil {
		t.Fatal(err)
	}
	var vols []driver.Volume
	var disks []*storagev1alpha1.Disk
	for _, d := range []struct {
		name string
		size int64
	}{{"data", 2 << 30}, {"logs", 1 << 30}} {
		v, err := host.Storage().EnsureVolume(ctx, "d-"+d.name, d.size, "")
		if err != nil {
			t.Fatal(err)
		}
		vols = append(vols, v)
		size := quantity.FormatDisk(uint64(d.size))
		disks = append(disks, &storagev1alpha1.Disk{
			Metadata: &metav1.ObjectMeta{Project: "p", Name: d.name, Uid: "d-" + d.name},
			Spec:     &storagev1alpha1.DiskSpec{Size: size, Grow: &storagev1alpha1.DiskGrow{Max: "8Gi"}},
			Status:   &storagev1alpha1.DiskStatus{Node: "n1", Size: size},
		})
	}
	spec := driver.VMSpec{
		MachineUID: "m1", Machine: "p/ct-web", VCPUs: 1, MemoryMiB: 256, Root: root, Disks: vols,
		Mounts: []driver.Mount{{Disk: 0, Name: "data", Target: "/data"}, {Disk: 1, Name: "logs", Target: "/logs", ReadOnly: true}},
	}
	if err := host.Hypervisor().Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	m := &computev1alpha1.Machine{
		Metadata: &metav1.ObjectMeta{Project: "p", Name: "ct-web", Uid: "m1", ResourceVersion: 7},
		Spec: &computev1alpha1.MachineSpec{Node: "n1", Resources: &computev1alpha1.Resources{
			Disk: "4Gi", DiskGrow: &computev1alpha1.DiskGrow{Max: "20Gi"},
		}},
	}
	store := &machineStore{m: m, disks: map[string]*storagev1alpha1.Disk{}}
	for _, d := range disks {
		store.disks[d.GetMetadata().GetName()] = d
	}
	r := &rootHost{host: host, store: store, spec: spec, vm: host.Snapshot().VMs["m1"].VM, disks: disks}
	r.a = &Agent{
		cfg: Config{
			Name: "n1", RootPoll: DefaultRootPoll, Clock: clockwork.NewRealClock(), Log: slog.New(slog.DiscardHandler),
			Drivers: &Drivers{Storage: host.Storage(), Hypervisor: host.Hypervisor(), Guest: host.Guest()},
		},
		machines: store, disks: store, metrics: newAgentMetrics(), guests: newGuests(),
		toldDisks: map[grownKey]toldDisk{}, growings: map[string]growing{}, growWatches: map[string]*growWatch{},
		rootGrowths: map[string]diskGrowth{}, volumeGrowths: map[string]diskGrowth{},
		requeue: func(keys ...controller.Key) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.requeued = append(r.requeued, keys...)
		},
	}
	r.a.host.Lock()
	defer r.a.host.Unlock()
	if err := r.a.ensureDiskSizes(ctx, spec, &r.vm); err != nil {
		t.Fatal(err)
	}
	r.a.watchGrowth(m, spec, disks, &observation{phase: computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, started: timestamppb.New(r.vm.Started)})
	return r
}

func (r *rootHost) queued() []controller.Key {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requeued)
}

// rootPollAgent is a rootHost with the usage poll running until the test
// ends.
func rootPollAgent(t *testing.T) *rootHost {
	t.Helper()
	r := newRootHost(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.a.rootLoop(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return r
}

// count is how often calls holds call.
func count(calls []string, call string) int {
	n := 0
	for _, c := range calls {
		if c == call {
			n++
		}
	}
	return n
}

// TestTheRootIsAskedForEveryPoll: a running container whose root grows by
// itself is asked how full it is every 15 s; its machine asks for the next
// size once its root is full past the threshold, and asks for none once it
// is not, its status written only when what it asks changes.
func TestTheRootIsAskedForEveryPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := rootPollAgent(t)
		host, store := r.host, r.store
		usages := func() int { return count(host.RootCalls("m1"), fake.RootCallUsage) }
		host.FillRoot("m1", 1<<30)
		time.Sleep(DefaultRootPoll) //nolint:forbidigo // virtual time
		synctest.Wait()
		if n, w := usages(), store.written(); n != 1 || len(w) != 0 {
			t.Fatalf("after a poll: asked %d times, wrote %q; want once, nothing at 25%%", n, w)
		}

		host.FillRoot("m1", 7<<29)                // 3.5Gi of 4Gi, 87.5%
		time.Sleep(DefaultRootPoll - time.Second) //nolint:forbidigo // virtual time
		synctest.Wait()
		if n, w := usages(), store.written(); n != 1 || len(w) != 0 {
			t.Fatalf("before the next poll: asked %d times, wrote %q", n, w)
		}
		time.Sleep(time.Second) //nolint:forbidigo // virtual time
		synctest.Wait()
		if w := store.written(); !slices.Equal(w, []string{"8Gi"}) {
			t.Fatalf("a root 87.5%% full wrote %q; want the next size, 8Gi, once", w)
		}
		time.Sleep(3 * DefaultRootPoll) //nolint:forbidigo // virtual time
		synctest.Wait()
		if n, w := usages(), store.written(); n != 5 || len(w) != 1 {
			t.Errorf("three polls later: asked %d times in all, wrote %q; want 5, and no write again", n, w)
		}

		host.FillRoot("m1", 1<<29)
		time.Sleep(DefaultRootPoll) //nolint:forbidigo // virtual time
		synctest.Wait()
		if w := store.written(); !slices.Equal(w, []string{"8Gi", ""}) {
			t.Errorf("a root emptied wrote %q; want the ask cleared once", w)
		}
	})
}

// TestAGuestThatCannotSayIsNotAskedAgain: a guest from before RootUsage is
// asked once in its boot, and its machine asks for nothing.
func TestAGuestThatCannotSayIsNotAskedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := rootPollAgent(t)
		host, store := r.host, r.store
		host.OldRootGuest("m1", true)
		host.FillRoot("m1", 4<<30)
		time.Sleep(5 * DefaultRootPoll) //nolint:forbidigo // virtual time
		synctest.Wait()
		if n, w := count(host.RootCalls("m1"), fake.RootCallUsage), store.written(); n != 1 || len(w) != 0 {
			t.Errorf("asked %d times in five polls, wrote %q; want once, nothing", n, w)
		}
		if q := r.queued(); !slices.Equal(q, []controller.Key{{Project: "p", Name: "ct-web"}}) {
			t.Errorf("queued %v; want the machine once, to report it", q)
		}
		r.a.host.Lock()
		c := conditionOf(r.a.growthConditions(store.m, r.vm.Started), computeapi.RootGrowth)
		r.a.host.Unlock()
		if c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || c.GetReason() != computeapi.ReasonGuestCannotGrow {
			t.Errorf("RootGrowth %v; want False, %s", c, computeapi.ReasonGuestCannotGrow)
		}
	})
}

// TestALyingGuestIsNotBelieved: a guest whose claim makes no sense, more
// used than it has or more than its disk holds, asks for nothing.
func TestALyingGuestIsNotBelieved(t *testing.T) {
	for name, u := range map[string]driver.FSUsage{
		"more used than it has":   {Size: 4 << 30, Used: 5 << 30},
		"larger than its disk":    {Size: 40 << 30, Used: 40 << 30},
		"a filesystem of no size": {},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := rootPollAgent(t)
				host, store := r.host, r.store
				host.ClaimRoot("m1", &u)
				time.Sleep(2 * DefaultRootPoll) //nolint:forbidigo // virtual time
				synctest.Wait()
				if w := store.written(); len(w) != 0 {
					t.Errorf("wrote %q", w)
				}
			})
		})
	}
}

// TestWantedSize: the next size while full enough and below the max, none
// at the max or below the threshold, and nothing said while the disk is
// still taking its size, or of a claim that makes no sense.
func TestWantedSize(t *testing.T) {
	g := &storagev1alpha1.DiskGrow{Max: "10Gi", Threshold: "90%", Step: "3Gi"}
	full := driver.FSUsage{Size: 4 << 30, Used: 4 << 30}
	for name, tt := range map[string]struct {
		size    uint64
		have    int64
		settled bool
		u       driver.FSUsage
		want    string
		ok      bool
	}{
		"full":                  {4 << 30, 4 << 30, true, full, "7Gi", true},
		"below the threshold":   {4 << 30, 4 << 30, true, driver.FSUsage{Size: 4 << 30, Used: 3 << 30}, "", true},
		"near the max":          {8 << 30, 8 << 30, true, full, "10Gi", true},
		"at the max":            {10 << 30, 10 << 30, true, full, "", true},
		"still taking its size": {4 << 30, 4 << 30, false, full, "", false},
		"larger than its disk":  {4 << 30, 2 << 30, true, full, "", false},
		"more used than it has": {4 << 30, 4 << 30, true, driver.FSUsage{Size: 4 << 30, Used: 5 << 30}, "", false},
	} {
		got, ok := wantedSize(g, tt.size, tt.have, tt.settled, tt.u, "spec.grow")
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s: %q %t, want %q %t", name, got, ok, tt.want, tt.ok)
		}
	}
	// A container's root grows by the same rules.
	if got, ok := wantedSize(&computev1alpha1.DiskGrow{Max: "10Gi", Threshold: "90%", Step: "3Gi"}, 4<<30, 4<<30, true, full, "spec.resources.disk_grow"); got != "7Gi" || !ok {
		t.Errorf("a root: %q %t, want 7Gi", got, ok)
	}
}

// conditionOf is the condition of type typ among conds, nil if none.
func conditionOf(conds []*metav1.Condition, typ string) *metav1.Condition {
	if i := slices.IndexFunc(conds, func(c *metav1.Condition) bool { return c.GetType() == typ }); i >= 0 {
		return conds[i]
	}
	return nil
}

// grow grows r's root disk to size on the host, as EnsureRoot does in a
// reconcile, and returns the VM's spec with it.
func (r *rootHost) grow(t *testing.T, size int64) driver.VMSpec {
	t.Helper()
	root, err := r.host.Storage().EnsureRoot(t.Context(), "m1", "sha256:a", size)
	if err != nil {
		t.Fatal(err)
	}
	spec := r.spec
	spec.Root = root
	return spec
}

// ensureRoot runs ensureDiskSizes as a reconcile does, under the host lock.
func (r *rootHost) ensureRoot(t *testing.T, spec driver.VMSpec, vm driver.VM) error {
	t.Helper()
	r.a.host.Lock()
	defer r.a.host.Unlock()
	return r.a.ensureDiskSizes(t.Context(), spec, &vm)
}

// TestARootGrowsOffTheHostLock: the VM and guest are asked to take a grown
// root off the host lock, one ask at a time, the reconcile waiting
// meanwhile; once they have, the machine is queued again, and its next
// reconcile has nothing left to do.
func TestARootGrowsOffTheHostLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRootHost(t)
		spec := r.grow(t, 8<<30)
		letGo := r.host.HoldRootGrowth("m1")
		if err := r.ensureRoot(t, spec, r.vm); !errors.Is(err, controller.ErrWaiting) {
			t.Fatalf("growing: %v, want waiting", err)
		}
		synctest.Wait()
		// The guest holds GrowRoot: the lock is free, and a reconcile meanwhile
		// asks nothing more.
		if err := r.ensureRoot(t, spec, r.vm); !errors.Is(err, controller.ErrWaiting) {
			t.Errorf("a reconcile while the guest grows: %v, want waiting", err)
		}
		if q := r.queued(); len(q) != 0 {
			t.Errorf("queued %v before the guest answered", q)
		}
		letGo()
		synctest.Wait()
		if calls := r.host.RootCalls("m1"); !slices.Equal(calls, []string{fake.RootCallResize, fake.RootCallGrow}) {
			t.Errorf("asked %q; want the VM told, then the guest grown, once", calls)
		}
		if q := r.queued(); !slices.Equal(q, []controller.Key{{Project: "p", Name: "ct-web"}}) {
			t.Errorf("queued %v; want the machine, once", q)
		}
		if err := r.ensureRoot(t, spec, r.vm); err != nil {
			t.Errorf("reconciled again: %v", err)
		}
		synctest.Wait()
		if vm := r.host.Snapshot().VMs["m1"]; vm.GuestRootFS != 8<<30 || len(r.host.RootCalls("m1")) != 2 {
			t.Errorf("the guest's filesystem is %d bytes, asked %q", vm.GuestRootFS, r.host.RootCalls("m1"))
		}
	})
}

// TestARootThatCannotGrowSaysSo: a VM that cannot be told its root's size
// while it runs, or a guest that cannot grow its filesystem, is asked once,
// and its machine's RootGrowth condition is False with the reason; a later
// boot that grows the root makes it True. A machine that never failed has
// none.
func TestARootThatCannotGrowSaysSo(t *testing.T) {
	for _, tt := range []struct {
		reason string
		old    func(h *fake.Host, old bool)
	}{
		{computeapi.ReasonVMCannotResize, func(h *fake.Host, old bool) { h.NoLiveResize("m1", old) }},
		{computeapi.ReasonGuestCannotGrow, func(h *fake.Host, old bool) { h.OldRootGuest("m1", old) }},
	} {
		t.Run(tt.reason, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newRootHost(t)
				m := r.store.m
				condition := func(vm driver.VM) *metav1.Condition {
					r.a.host.Lock()
					defer r.a.host.Unlock()
					return conditionOf(r.a.growthConditions(m, vm.Started), computeapi.RootGrowth)
				}
				if c := condition(r.vm); c != nil {
					t.Errorf("before any growth: %v, want none", c)
				}
				spec := r.grow(t, 8<<30)
				tt.old(r.host, true)
				_ = r.ensureRoot(t, spec, r.vm)
				synctest.Wait()
				if err := r.ensureRoot(t, spec, r.vm); err != nil {
					t.Errorf("reconciled again: %v; want it left for its next boot", err)
				}
				synctest.Wait()
				c := condition(r.vm)
				if c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || c.GetReason() != tt.reason || c.GetMessage() == "" {
					t.Fatalf("RootGrowth %v; want False, %s, saying why", c, tt.reason)
				}
				if n := count(r.host.RootCalls("m1"), fake.RootCallResize); n != 1 {
					t.Errorf("the VM was told %d times; want once", n)
				}

				// Its next boot, an able one, grows the root.
				tt.old(r.host, false)
				time.Sleep(time.Second) //nolint:forbidigo // virtual time, so the next boot starts later
				if err := r.host.Exit("m1", 0); err != nil {
					t.Fatal(err)
				}
				vm := r.host.Snapshot().VMs["m1"].VM
				m.Status = &computev1alpha1.MachineStatus{Conditions: []*metav1.Condition{c}}
				_ = r.ensureRoot(t, spec, vm)
				synctest.Wait()
				if c := condition(vm); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE || c.GetReason() != computeapi.ReasonRootGrew {
					t.Errorf("RootGrowth after a boot that grew the root: %v; want True, %s", c, computeapi.ReasonRootGrew)
				}
				m.Status = nil
				if c := condition(vm); c != nil {
					t.Errorf("a machine that never failed to grow: %v, want none", c)
				}
			})
		})
	}
}

// TestAVolumeIsAskedOnTheSamePoll: a Disk that grows by itself, mounted
// read-write, is asked how full it is on the root's poll; its machine asks
// for its next size once it is full past the threshold, and for none once
// it is not, its status written only when what it asks changes. A Disk
// mounted read-only alone is never asked.
func TestAVolumeIsAskedOnTheSamePoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := rootPollAgent(t)
		host, store := r.host, r.store
		asked := func(call string) int { return count(host.RootCalls("m1"), call) }
		host.FillVolume("d-data", 1<<30) // 50%
		time.Sleep(DefaultRootPoll)      //nolint:forbidigo // virtual time
		synctest.Wait()
		if n, w := asked(fake.RootCallVolumeUsage+" 0"), store.disksWritten(); n != 1 || len(w) != 0 {
			t.Fatalf("after a poll: data asked %d times, wrote %q; want once, nothing at 50%%", n, w)
		}
		if n := asked(fake.RootCallUsage); n != 1 {
			t.Errorf("the root asked %d times on the first poll; want once, the same poll", n)
		}

		host.FillVolume("d-data", 15<<27) // 1.875Gi of 2Gi, 93.75%
		time.Sleep(DefaultRootPoll)       //nolint:forbidigo // virtual time
		synctest.Wait()
		if w := store.disksWritten(); !slices.Equal(w, []string{"data=4Gi"}) {
			t.Fatalf("data 93.75%% full wrote %q; want data's next size, 4Gi, once", w)
		}
		time.Sleep(3 * DefaultRootPoll) //nolint:forbidigo // virtual time
		synctest.Wait()
		if n, w := asked(fake.RootCallVolumeUsage+" 0"), store.disksWritten(); n != 5 || len(w) != 1 {
			t.Errorf("three polls later: data asked %d times in all, wrote %q; want 5, and no write again", n, w)
		}
		if w := store.written(); len(w) != 0 {
			t.Errorf("the root, empty, asked %q", w)
		}

		host.FillVolume("d-data", 0)
		time.Sleep(DefaultRootPoll) //nolint:forbidigo // virtual time
		synctest.Wait()
		if w := store.disksWritten(); !slices.Equal(w, []string{"data=4Gi", ""}) {
			t.Errorf("data emptied wrote %q; want the ask cleared once", w)
		}
		for _, c := range host.RootCalls("m1") {
			if c == fake.RootCallVolumeUsage+" -1" {
				t.Fatalf("a volume mounted read-only alone was asked: %q", host.RootCalls("m1"))
			}
		}
	})
}

// TestAGuestThatCannotSayOfItsVolumes: a guest from before VolumeUsage is
// asked of its volumes once in its boot, its machine says so on its
// VolumeGrowth condition and asks for nothing; its root is asked on.
func TestAGuestThatCannotSayOfItsVolumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := rootPollAgent(t)
		host, store := r.host, r.store
		host.OldVolumeGuest("m1", true)
		host.FillVolume("d-data", 2<<30)
		time.Sleep(5 * DefaultRootPoll) //nolint:forbidigo // virtual time
		synctest.Wait()
		if n, w := count(host.RootCalls("m1"), fake.RootCallVolumeUsage+" 0"), store.disksWritten(); n != 1 || len(w) != 0 {
			t.Errorf("data asked %d times in five polls, wrote %q; want once, nothing", n, w)
		}
		if n := count(host.RootCalls("m1"), fake.RootCallUsage); n != 5 {
			t.Errorf("the root asked %d times in five polls; want 5", n)
		}
		if q := r.queued(); !slices.Equal(q, []controller.Key{{Project: "p", Name: "ct-web"}}) {
			t.Errorf("queued %v; want the machine once, to report it", q)
		}
		r.a.host.Lock()
		conds := r.a.growthConditions(store.m, r.vm.Started)
		r.a.host.Unlock()
		if c := conditionOf(conds, computeapi.VolumeGrowth); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || c.GetReason() != computeapi.ReasonGuestCannotGrow {
			t.Errorf("VolumeGrowth %v; want False, %s", c, computeapi.ReasonGuestCannotGrow)
		}
		if c := conditionOf(conds, computeapi.RootGrowth); c != nil {
			t.Errorf("RootGrowth %v; want none", c)
		}
	})
}

// TestALyingGuestIsNotBelievedOfItsVolumes: a guest claiming a volume's
// filesystem larger than its disk asks for nothing.
func TestALyingGuestIsNotBelievedOfItsVolumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := rootPollAgent(t)
		r.host.ClaimVolume("d-data", &driver.FSUsage{Size: 40 << 30, Used: 40 << 30})
		time.Sleep(2 * DefaultRootPoll) //nolint:forbidigo // virtual time
		synctest.Wait()
		if w := r.store.disksWritten(); len(w) != 0 {
			t.Errorf("wrote %q", w)
		}
	})
}

// growVolume grows r's n-th volume to size on the host, as EnsureVolume
// does in a reconcile, and returns spec with it.
func (r *rootHost) growVolume(t *testing.T, spec driver.VMSpec, n int, size int64) driver.VMSpec {
	t.Helper()
	v, err := r.host.Storage().EnsureVolume(t.Context(), spec.Disks[n].DiskUID, size, "")
	if err != nil {
		t.Fatal(err)
	}
	spec.Disks = slices.Clone(spec.Disks)
	spec.Disks[n] = v
	return spec
}

// TestAVolumeGrowsOffTheHostLock: volumes grown are told the VM, then each
// one the guest mounts read-write grown in it, off the host lock, the
// reconcile waiting meanwhile; a volume mounted read-only alone is told the
// VM, its filesystem left as it is. Once done, the machine is queued again,
// and its next reconcile has nothing left to do.
func TestAVolumeGrowsOffTheHostLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRootHost(t)
		spec := r.growVolume(t, r.growVolume(t, r.spec, 0, 4<<30), 1, 2<<30)
		letGo := r.host.HoldRootGrowth("m1")
		if err := r.ensureRoot(t, spec, r.vm); !errors.Is(err, controller.ErrWaiting) {
			t.Fatalf("growing: %v, want waiting", err)
		}
		synctest.Wait()
		if err := r.ensureRoot(t, spec, r.vm); !errors.Is(err, controller.ErrWaiting) {
			t.Errorf("a reconcile while the guest grows: %v, want waiting", err)
		}
		letGo()
		synctest.Wait()
		want := []string{fake.RootCallResizeVolume + " 0", fake.RootCallGrowVolume + " 0", fake.RootCallResizeVolume + " 1"}
		if calls := r.host.RootCalls("m1"); !slices.Equal(calls, want) {
			t.Errorf("asked %q; want %q", calls, want)
		}
		if q := r.queued(); !slices.Equal(q, []controller.Key{{Project: "p", Name: "ct-web"}}) {
			t.Errorf("queued %v; want the machine, once", q)
		}
		if err := r.ensureRoot(t, spec, r.vm); err != nil {
			t.Errorf("reconciled again: %v", err)
		}
		synctest.Wait()
		vm := r.host.Snapshot().VMs["m1"]
		if !slices.Equal(vm.GuestVolumes, []int64{4 << 30, 2 << 30}) || !slices.Equal(vm.GuestVolumeFS, []int64{4 << 30, 1 << 30}) || len(r.host.RootCalls("m1")) != 3 {
			t.Errorf("the guest sees its volumes at %v, their filesystems at %v; asked %q", vm.GuestVolumes, vm.GuestVolumeFS, r.host.RootCalls("m1"))
		}
		r.a.host.Lock()
		w := r.a.growWatches["m1"]
		r.a.host.Unlock()
		if w == nil || len(w.vols) != 1 || w.vols[0].disk != "data" {
			t.Errorf("watching %+v; want data alone, mounted read-write", w)
		}
	})
}

// TestAVolumeThatCannotGrowSaysSo: a VM that cannot be told a volume's size
// while it runs, or a guest that cannot grow its filesystem, is asked once,
// and its machine's VolumeGrowth condition is False with the reason; a
// later boot that grows a volume makes it True.
func TestAVolumeThatCannotGrowSaysSo(t *testing.T) {
	for _, tt := range []struct {
		reason string
		old    func(h *fake.Host, old bool)
	}{
		{computeapi.ReasonVMCannotResize, func(h *fake.Host, old bool) { h.NoLiveResize("m1", old) }},
		{computeapi.ReasonGuestCannotGrow, func(h *fake.Host, old bool) { h.OldVolumeGuest("m1", old) }},
	} {
		t.Run(tt.reason, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newRootHost(t)
				m := r.store.m
				condition := func(vm driver.VM) *metav1.Condition {
					r.a.host.Lock()
					defer r.a.host.Unlock()
					return conditionOf(r.a.growthConditions(m, vm.Started), computeapi.VolumeGrowth)
				}
				spec := r.growVolume(t, r.spec, 0, 4<<30)
				tt.old(r.host, true)
				_ = r.ensureRoot(t, spec, r.vm)
				synctest.Wait()
				if err := r.ensureRoot(t, spec, r.vm); err != nil {
					t.Errorf("reconciled again: %v; want it left for its next boot", err)
				}
				synctest.Wait()
				c := condition(r.vm)
				if c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || c.GetReason() != tt.reason || c.GetMessage() == "" {
					t.Fatalf("VolumeGrowth %v; want False, %s, saying why", c, tt.reason)
				}
				if n := count(r.host.RootCalls("m1"), fake.RootCallResizeVolume+" 0"); n != 1 {
					t.Errorf("the VM was told %d times; want once", n)
				}

				tt.old(r.host, false)
				time.Sleep(time.Second) //nolint:forbidigo // virtual time, so the next boot starts later
				if err := r.host.Exit("m1", 0); err != nil {
					t.Fatal(err)
				}
				vm := r.host.Snapshot().VMs["m1"].VM
				m.Status = &computev1alpha1.MachineStatus{Conditions: []*metav1.Condition{c}}
				_ = r.ensureRoot(t, spec, vm)
				synctest.Wait()
				if c := condition(vm); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE || c.GetReason() != computeapi.ReasonVolumeGrew {
					t.Errorf("VolumeGrowth after a boot that grew a volume: %v; want True, %s", c, computeapi.ReasonVolumeGrew)
				}
			})
		})
	}
}
