package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
)

// machineStore is the apiserver's machines as the usage poll reaches them:
// one machine, its status writes conditional on its resource version, each
// write's status.disk_wanted kept.
type machineStore struct {
	computev1alpha1connect.MachineServiceClient
	mu     sync.Mutex
	m      *computev1alpha1.Machine
	writes []string
}

func (s *machineStore) GetMachine(context.Context, *connect.Request[computev1alpha1.GetMachineRequest]) (*connect.Response[computev1alpha1.GetMachineResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return connect.NewResponse(&computev1alpha1.GetMachineResponse{Machine: proto.Clone(s.m).(*computev1alpha1.Machine)}), nil
}

func (s *machineStore) UpdateMachineStatus(_ context.Context, req *connect.Request[computev1alpha1.UpdateMachineStatusRequest]) (*connect.Response[computev1alpha1.UpdateMachineStatusResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Msg.GetExpectedResourceVersion() != s.m.GetMetadata().GetResourceVersion() {
		return nil, connect.NewError(connect.CodeAborted, fmt.Errorf("resource version %d, not %d", s.m.GetMetadata().GetResourceVersion(), req.Msg.GetExpectedResourceVersion()))
	}
	s.m.Status = req.Msg.GetStatus()
	s.m.Metadata.ResourceVersion++
	s.writes = append(s.writes, req.Msg.GetStatus().GetDiskWanted())
	return connect.NewResponse(&computev1alpha1.UpdateMachineStatusResponse{Machine: s.m}), nil
}

func (s *machineStore) written() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.writes)
}

// rootHost is an agent on a fake host running a container machine, m1,
// p/ct-web, with a 4Gi root that grows by itself 2x up to 20Gi at 80%, its
// VM started at that size and reconciled once; what the agent queued
// again is kept.
type rootHost struct {
	a     *Agent
	host  *fake.Host
	store *machineStore
	spec  driver.VMSpec
	vm    driver.VM

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
	spec := driver.VMSpec{MachineUID: "m1", Machine: "p/ct-web", VCPUs: 1, MemoryMiB: 256, Root: root}
	if err := host.Hypervisor().Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	m := &computev1alpha1.Machine{
		Metadata: &metav1.ObjectMeta{Project: "p", Name: "ct-web", Uid: "m1", ResourceVersion: 7},
		Spec: &computev1alpha1.MachineSpec{Node: "n1", Resources: &computev1alpha1.Resources{
			Disk: "4Gi", DiskGrow: &computev1alpha1.DiskGrow{Max: "20Gi"},
		}},
	}
	r := &rootHost{host: host, store: &machineStore{m: m}, spec: spec, vm: host.Snapshot().VMs["m1"].VM}
	r.a = &Agent{
		cfg: Config{
			Name: "n1", RootPoll: DefaultRootPoll, Clock: clockwork.NewRealClock(), Log: slog.New(slog.DiscardHandler),
			Drivers: &Drivers{Storage: host.Storage(), Hypervisor: host.Hypervisor(), Guest: host.Guest()},
		},
		machines: r.store, metrics: newAgentMetrics(), guests: newGuests(),
		toldRoot: map[string]toldRoot{}, rootWatches: map[string]*rootWatch{}, rootGrowths: map[string]rootGrowth{},
		requeue: func(keys ...controller.Key) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.requeued = append(r.requeued, keys...)
		},
	}
	r.a.host.Lock()
	defer r.a.host.Unlock()
	if err := r.a.ensureRootSize(ctx, spec, &r.vm); err != nil {
		t.Fatal(err)
	}
	r.a.watchRoot(m, spec, &observation{phase: computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, started: timestamppb.New(r.vm.Started)})
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
		c := r.a.rootGrowthCondition(store.m, r.vm.Started)
		r.a.host.Unlock()
		if c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || c.GetReason() != computeapi.ReasonGuestCannotGrow {
			t.Errorf("RootGrowth %v; want False, %s", c, computeapi.ReasonGuestCannotGrow)
		}
	})
}

// TestALyingGuestIsNotBelieved: a guest whose claim makes no sense, more
// used than it has or more than its disk holds, asks for nothing.
func TestALyingGuestIsNotBelieved(t *testing.T) {
	for name, u := range map[string]driver.RootUsage{
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

// TestWantedDisk: the next size while full enough and below the max, none
// at the max or below the threshold, and nothing said while the root is
// still taking its size.
func TestWantedDisk(t *testing.T) {
	w := rootWatch{growth: &computev1alpha1.DiskGrow{Max: "10Gi", Threshold: "90%", Step: "3Gi"}, disk: 4 << 30, root: 4 << 30, settled: true}
	full := driver.RootUsage{Size: 4 << 30, Used: 4 << 30}
	for name, tt := range map[string]struct {
		w    func(rootWatch) rootWatch
		u    driver.RootUsage
		want string
		ok   bool
	}{
		"full":                  {func(w rootWatch) rootWatch { return w }, full, "7Gi", true},
		"below the threshold":   {func(w rootWatch) rootWatch { return w }, driver.RootUsage{Size: 4 << 30, Used: 3 << 30}, "", true},
		"near the max":          {func(w rootWatch) rootWatch { w.disk, w.root = 8<<30, 8<<30; return w }, full, "10Gi", true},
		"at the max":            {func(w rootWatch) rootWatch { w.disk, w.root = 10<<30, 10<<30; return w }, full, "", true},
		"still taking its size": {func(w rootWatch) rootWatch { w.settled = false; return w }, full, "", false},
	} {
		got, ok := wantedDisk(tt.w(w), tt.u)
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s: %q %t, want %q %t", name, got, ok, tt.want, tt.ok)
		}
	}
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

// ensureRoot runs ensureRootSize as a reconcile does, under the host lock.
func (r *rootHost) ensureRoot(t *testing.T, spec driver.VMSpec, vm driver.VM) error {
	t.Helper()
	r.a.host.Lock()
	defer r.a.host.Unlock()
	return r.a.ensureRootSize(t.Context(), spec, &vm)
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
					return r.a.rootGrowthCondition(m, vm.Started)
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
