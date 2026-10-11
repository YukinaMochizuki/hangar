package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// bare is a Machine of no container, written as the system writes
// machines, for what a Container cannot ask for.
func bare(name, ipv4 string) *computev1alpha1.Machine {
	return &computev1alpha1.Machine{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec: &computev1alpha1.MachineSpec{
			Resources:     &computev1alpha1.Resources{Vcpus: 1, MemoryMib: 512},
			RunState:      computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING,
			Boot:          &computev1alpha1.MachineSpec_Container{Container: &computev1alpha1.ContainerBoot{Image: "nginx"}},
			Interfaces:    []*computev1alpha1.MachineInterface{{Name: "eth0", Network: "default", Ipv4: ipv4}},
			RestartPolicy: computev1alpha1.RestartPolicy_RESTART_POLICY_ALWAYS,
		},
	}
}

func (w *world) applyMachine(m *computev1alpha1.Machine) {
	w.t.Helper()
	if _, err := w.system.Apply(context.Background(), registry.Machine, m); err != nil {
		w.t.Fatal(err)
	}
}

// waitMachine waits for the machine to be in phase, with the node's Ready
// condition giving reason, at its current generation.
func (w *world) waitMachine(name string, phase computev1alpha1.MachinePhase, reason string) *computev1alpha1.Machine {
	w.t.Helper()
	var m *computev1alpha1.Machine
	w.eventually(fmt.Sprintf("machine %s %v %s", name, phase, reason), func() (bool, string) {
		var ok bool
		m, ok = get[*computev1alpha1.Machine](w, registry.Machine, name)
		r := conditions.Find(m.GetStatus().GetConditions(), computeapi.MachineReady)
		return ok && m.GetStatus().GetPhase() == phase && r.GetReason() == reason &&
			r.GetObservedGeneration() == m.GetMetadata().GetGeneration(), fmt.Sprintf("%v", m.GetStatus())
	})
	return m
}

// TestARegistryOutageLeavesARunningMachineAlone: the registry going down
// changes nothing for a machine already running, even when its VM restarts.
func TestARegistryOutageLeavesARunningMachineAlone(t *testing.T) {
	w := newWorld(t)
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	uid := w.machine("api").GetMetadata().GetUid()
	w.settle("api")

	w.host.FailPull("nginx", errors.New("registry unreachable"))
	before := w.machine("api").GetStatus().GetBootTime().AsTime()
	if err := w.host.Exit(uid, 0); err != nil {
		t.Fatal(err)
	}
	w.eventually("the restart reported", func() (bool, string) {
		boot := w.machine("api").GetStatus().GetBootTime().AsTime()
		return boot.After(before), fmt.Sprintf("boot time %v", boot)
	})
	if m := w.machine("api"); m.GetStatus().GetPhase() != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING {
		t.Errorf("phase %v while the registry is down", m.GetStatus().GetPhase())
	}
	w.waitReason("api", "Running")
}

// TestAStoppedMachineKeepsItsDisk: stopping takes the VM down and keeps its
// disk, needs no registry, and starting again boots the same disk.
func TestAStoppedMachineKeepsItsDisk(t *testing.T) {
	w := newWorld(t)
	m := bare("solo", "10.0.0.50/24")
	w.applyMachine(m)
	w.waitMachine("solo", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning)
	uid := w.machine2("solo").GetMetadata().GetUid()

	w.host.FailPull("nginx", errors.New("registry unreachable"))
	m = w.machine2("solo") // as placed
	m.Spec.RunState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	w.applyMachine(m)
	w.waitMachine("solo", computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, computeapi.ReasonStopped)
	s := w.host.Snapshot()
	if _, running := s.VMs[uid]; running {
		t.Error("a stopped machine's VM is still there")
	}
	if s.Roots[uid].Digest != "sha256:nginx-1" || len(s.Interfaces[uid]) != 1 {
		t.Errorf("a stopped machine lost its disk or interface: %+v", s)
	}

	m = w.machine2("solo")
	m.Spec.RunState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING
	w.applyMachine(m)
	w.waitMachine("solo", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning)
	if vm := w.host.Snapshot().VMs[uid]; vm.Spec.Root.Digest != "sha256:nginx-1" {
		t.Errorf("started again on %+v", vm.Spec.Root)
	}
}

func (w *world) machine2(name string) *computev1alpha1.Machine {
	w.t.Helper()
	m, ok := get[*computev1alpha1.Machine](w, registry.Machine, name)
	if !ok {
		w.t.Fatalf("no machine %s", name)
	}
	return m
}

// TestASuccessfulExitStops: under OnFailure, an exit with 0 leaves the
// container stopped, not failed.
func TestASuccessfulExitStops(t *testing.T) {
	w := newWorld(t)
	c := container("job", "nginx")
	c.Spec.RestartPolicy = computev1alpha1.RestartPolicy_RESTART_POLICY_ON_FAILURE
	w.apply(c)
	w.waitReason("job", "Running")
	uid := w.machine("job").GetMetadata().GetUid()
	w.settle("job")
	if err := w.host.Exit(uid, 0); err != nil {
		t.Fatal(err)
	}
	c = w.waitReason("job", computeapi.ReasonExited)
	if msg := ready(c).GetMessage(); msg != "the container exited with code 0" {
		t.Errorf("message %q", msg)
	}
	if m := w.machine("job"); m.GetStatus().GetPhase() != computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED {
		t.Errorf("phase %v after a successful exit", m.GetStatus().GetPhase())
	}
}

// TestAnInterfaceChangeReplacesIt: an interface taken away goes from the
// host, one changed is made anew, and the VM is started on what is left.
func TestAnInterfaceChangeReplacesIt(t *testing.T) {
	w := newWorld(t)
	m := bare("solo", "10.0.0.50/24")
	m.Spec.Interfaces = append(m.Spec.Interfaces, &computev1alpha1.MachineInterface{Name: "eth1", Network: "default", Ipv4: "10.0.0.60/24"})
	w.applyMachine(m)
	w.waitMachine("solo", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning)
	uid := w.machine2("solo").GetMetadata().GetUid()
	if n := len(w.host.Snapshot().Interfaces[uid]); n != 2 {
		t.Fatalf("%d interfaces on the host, want 2", n)
	}

	m = w.machine2("solo") // as placed
	m.Spec.Interfaces = m.Spec.Interfaces[:1]
	m.Spec.Interfaces[0].Ipv4 = "10.0.0.51/24"
	w.applyMachine(m)
	w.eventually("the interface replaced and the VM on it", func() (bool, string) {
		s := w.host.Snapshot()
		ifs, vm := s.Interfaces[uid], s.VMs[uid]
		return len(ifs) == 1 && ifs[0].Address.String() == "10.0.0.51/24" && len(vm.Spec.Interfaces) == 1 &&
			vm.Spec.Interfaces[0].Address.String() == "10.0.0.51/24" && vm.State == driver.VMRunning, fmt.Sprintf("%+v", s)
	})
	w.waitMachine("solo", computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning)
}

// TestAMachineOnAnotherNodeIsLeftAlone: the agent builds nothing for a
// machine placed elsewhere.
func TestAMachineOnAnotherNodeIsLeftAlone(t *testing.T) {
	w := newWorld(t)
	m := bare("elsewhere", "10.0.0.50/24")
	m.Spec.Node = "n2"
	w.applyMachine(m)
	time.Sleep(300 * time.Millisecond)
	if s := w.host.Snapshot(); !s.Empty() {
		t.Errorf("the host holds something for another node's machine: %+v", s)
	}
	if st := w.machine2("elsewhere").GetStatus(); conditions.Find(st.GetConditions(), computeapi.MachineReady) != nil {
		t.Errorf("the agent reported on another node's machine: %v", st)
	}
}

// TestAMachineWithoutItsNetworkSaysSo: a machine whose network does not
// exist waits for it and says so.
func TestAMachineWithoutItsNetworkSaysSo(t *testing.T) {
	w := newWorld(t)
	m := bare("lost", "10.9.0.5/24")
	m.Spec.Interfaces[0].Network = "nowhere"
	w.applyMachine(m)
	got := w.waitMachine("lost", computev1alpha1.MachinePhase_MACHINE_PHASE_CREATING, computeapi.ReasonNetworkNotReady)
	if msg := conditions.Find(got.GetStatus().GetConditions(), computeapi.MachineReady).GetMessage(); !strings.Contains(msg, "nowhere") {
		t.Errorf("message %q", msg)
	}
}

// TestReleasedOnlyOnceTheHostIsClean: a deleted machine is reported
// released only after its VM, disk and interfaces are gone.
func TestReleasedOnlyOnceTheHostIsClean(t *testing.T) {
	w := newWorld(t)
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	w.settle("api")

	w.host.FailRemoval(errors.New("device busy"))
	w.delete("api")
	time.Sleep(400 * time.Millisecond)
	m, ok := get[*computev1alpha1.Machine](w, registry.Machine, "ct-api")
	if !ok || conditions.IsTrue(m.GetStatus().GetConditions(), computeapi.MachineReleased) {
		t.Fatalf("released while the host still holds it: %v", m.GetStatus())
	}
	w.host.FailRemoval(nil)
	w.waitGone("api")
	w.waitHostEmpty()
}

// TestStatusWritesAreConditional: the agent writes a machine's status only
// on top of what it read, so a write made meanwhile by another (here a
// condition the test adds, as the scheduler adds its own) is never lost.
func TestStatusWritesAreConditional(t *testing.T) {
	w := newIdleWorld(t)
	probe := &metav1.Condition{Type: "Probe", Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Test"}
	var done atomic.Bool
	w.wrap = func(next http.RoundTripper) http.RoundTripper {
		return afterCall{next: next, procedure: "/GetMachine", then: func() {
			if !done.Load() && addCondition(t, w, "ct-api", probe) {
				done.Store(true)
			}
		}}
	}
	w.startAgent()
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	w.eventually("the condition written meanwhile still there", func() (bool, string) {
		st := w.machine("api").GetStatus()
		return done.Load() && conditions.IsTrue(st.GetConditions(), "Probe"), fmt.Sprintf("%v", st)
	})
}

// addCondition writes c into a placed machine's status in the store, behind
// the agent's back, unless it is there already; it reports whether it did.
func addCondition(t *testing.T, w *world, name string, c *metav1.Condition) bool {
	ctx := context.Background()
	key := store.Key{Kind: registry.Machine.Kind, Project: project, Name: name}
	obj, err := w.env.Store.Get(ctx, key)
	if err != nil {
		return false
	}
	var m computev1alpha1.Machine
	if err := proto.Unmarshal(obj.Data, &m); err != nil {
		t.Error(err)
		return false
	}
	if m.GetSpec().GetNode() == "" || conditions.Find(m.GetStatus().GetConditions(), c.GetType()) != nil {
		return false
	}
	if m.Status == nil {
		m.Status = &computev1alpha1.MachineStatus{}
	}
	m.Status.Conditions = conditions.Set(m.Status.Conditions, c, time.Now())
	data, err := proto.Marshal(&m)
	if err != nil {
		t.Error(err)
		return false
	}
	_, err = w.env.Store.Update(ctx, key, data, obj.Revision)
	return err == nil
}

// afterCall calls then after each successful call of the procedure, before
// the caller sees the answer.
type afterCall struct {
	next      http.RoundTripper
	procedure string
	then      func()
}

func (a afterCall) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := a.next.RoundTrip(r)
	if err == nil && res.StatusCode == http.StatusOK && strings.HasSuffix(r.URL.Path, a.procedure) {
		a.then()
	}
	return res, err
}

// failing fails every call of the procedure while on is set.
type failing struct {
	next      http.RoundTripper
	procedure string
	on        *atomic.Bool
}

func (f failing) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.on.Load() && strings.HasSuffix(r.URL.Path, f.procedure) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable",
			Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r,
		}, nil
	}
	return f.next.RoundTrip(r)
}

// TestTheSweepTrustsNoFailedList: neither the API nor the host failing to
// list what it holds reads as "nothing is wanted".
func TestTheSweepTrustsNoFailedList(t *testing.T) {
	w := newIdleWorld(t)
	var apiDown atomic.Bool
	w.wrap = func(next http.RoundTripper) http.RoundTripper {
		return failing{next: next, procedure: "/ListMachines", on: &apiDown}
	}
	w.startAgent()
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	uid := w.machine("api").GetMetadata().GetUid()
	w.settle("api")
	intact := func(what string) {
		t.Helper()
		s := w.host.Snapshot()
		vm := s.VMs[uid]
		if vm.State != driver.VMRunning || vm.DeadNICs || len(s.Networks) != 1 || len(s.Interfaces[uid]) != 1 ||
			s.Roots[uid].Digest == "" || len(s.Bases) != 1 {
			t.Errorf("%s: the host lost what a running machine needs: %+v", what, s)
		}
	}

	apiDown.Store(true)
	time.Sleep(400 * time.Millisecond) // several sweeps
	intact("the API failing to list machines")
	apiDown.Store(false)

	// Each list the sweep judges shared things by, failing alone.
	for _, list := range []string{fake.ListInterfaces, fake.ListRoots, ""} {
		lists := []string{list}
		if list == "" {
			lists = nil // all of them
		}
		w.host.FailListing(errors.New("netlink: no buffer space"), lists...)
		time.Sleep(400 * time.Millisecond)
		intact(fmt.Sprintf("the host failing to list %q", list))
		w.host.FailListing(nil)
	}
}

// TestAHostHiccupLeavesARunningMachineAlone: a step the agent cannot check
// again for a while, here networking, changes nothing for a machine running
// its current spec.
func TestAHostHiccupLeavesARunningMachineAlone(t *testing.T) {
	w := newWorld(t)
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	uid := w.machine("api").GetMetadata().GetUid()
	w.settle("api")

	w.host.FailNetworking(errors.New("netlink: no buffer space"))
	if err := w.host.Exit(uid, 0); err != nil { // the restart makes the agent look again
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	m := w.machine("api")
	if r := conditions.Find(m.GetStatus().GetConditions(), computeapi.MachineReady); m.GetStatus().GetPhase() != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING || r.GetReason() != computeapi.ReasonRunning {
		t.Errorf("a running machine reported %v, %v while networking could not be checked", m.GetStatus().GetPhase(), r)
	}
	w.host.FailNetworking(nil)
	w.waitReason("api", "Running")
}

// TestABaseBeingBuiltOnIsKept: the sweep spares the base of a machine whose
// root disk could not be made yet, so the image is not unpacked again.
func TestABaseBeingBuiltOnIsKept(t *testing.T) {
	w := newWorld(t)
	w.host.FailClone(driver.ErrNoSpace)
	w.apply(container("api", "nginx"))
	w.waitReason("api", computeapi.ReasonDiskFailed)
	time.Sleep(400 * time.Millisecond) // several sweeps
	w.host.FailClone(nil)
	w.waitReason("api", "Running")
	if n := w.host.Unpacked("sha256:nginx-1"); n != 1 {
		t.Errorf("the image was unpacked %d times", n)
	}
}

// TestLostNetworkingIsNotHidden: when networking changed behind the agent's
// back and cannot be checked, a running machine is not reported as fine.
func TestLostNetworkingIsNotHidden(t *testing.T) {
	w := newWorld(t)
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	w.settle("api")
	net := w.host.Snapshot().Interfaces[w.machine("api").GetMetadata().GetUid()][0].NetworkUID

	w.host.FailNetworking(errors.New("netlink: no buffer space"))
	w.host.LoseNetwork(net)
	c := w.waitReason("api", computeapi.ReasonNetworkFailed)
	if msg := ready(c).GetMessage(); !strings.Contains(msg, "netlink") {
		t.Errorf("message %q", msg)
	}
	w.host.FailNetworking(nil)
	w.waitReason("api", "Running")

	// Found whole again, a later hiccup with nothing changed is quiet.
	w.settle("api")
	w.host.FailNetworking(errors.New("netlink: no buffer space"))
	if err := w.host.Exit(w.machine("api").GetMetadata().GetUid(), 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if c, _ := get[*computev1alpha1.Container](w, registry.Container, "api"); ready(c).GetReason() != "Running" {
		t.Errorf("after a hiccup with nothing changed: %v", ready(c))
	}
	w.host.FailNetworking(nil)
}

// TestAnUpdateThatCannotBePulledSaysSo: a running container changed to an
// image that cannot be pulled says so, while the old one keeps running.
func TestAnUpdateThatCannotBePulledSaysSo(t *testing.T) {
	w := newWorld(t)
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	uid := w.machine("api").GetMetadata().GetUid()

	w.apply(container("api", "nginx:missing"))
	w.waitReason("api", computeapi.ReasonImagePullFailed)
	if vm := w.host.Snapshot().VMs[uid]; vm.State != driver.VMRunning || vm.Spec.Root.Digest != "sha256:nginx-1" {
		t.Errorf("the old container stopped for an update that cannot happen: %+v", vm)
	}
}

// TestASlowImageHoldsUpNoOtherMachine: while one image takes long to pull,
// the node runs, stops and deletes other machines, says the waiting ones
// are pulling, and pulls the image once for all that want it.
func TestASlowImageHoldsUpNoOtherMachine(t *testing.T) {
	w := newWorld(t)
	w.apply(container("old", "nginx"))
	w.waitReason("old", "Running")
	w.host.AddImage("big", "sha256:big-1", nginx)
	letGo := w.host.HoldUnpack("big")
	defer letGo()

	w.apply(container("big-a", "big"))
	w.apply(container("big-b", "big"))
	w.waitReason("big-a", computeapi.ReasonPulling)
	w.waitReason("big-b", computeapi.ReasonPulling)

	w.apply(container("small", "nginx"))
	w.waitReason("small", "Running")
	w.delete("old")
	w.waitGone("old")

	letGo()
	w.waitReason("big-a", "Running")
	w.waitReason("big-b", "Running")
	if n := w.host.Unpacked("sha256:big-1"); n != 1 {
		t.Errorf("the image was unpacked %d times for two machines", n)
	}
	if n := w.host.Resolved("big"); n != 1 {
		t.Errorf("the image was resolved %d times while it was pulled", n)
	}
}

// TestAFullDiskForANewImageSaysSo: a base that cannot be made for lack of
// room is the disk's failure, not the registry's.
func TestAFullDiskForANewImageSaysSo(t *testing.T) {
	w := newWorld(t)
	w.host.SetFull(true)
	w.apply(container("api", "nginx"))
	c := w.waitReason("api", computeapi.ReasonDiskFailed)
	if msg := ready(c).GetMessage(); !strings.Contains(msg, "no space") {
		t.Errorf("message %q", msg)
	}
	w.host.SetFull(false)
	w.waitReason("api", "Running")
}

// TestAVanishedBaseIsMadeAgain: a base gone after its image was prepared,
// before the machine's disk was cloned from it, is made again.
func TestAVanishedBaseIsMadeAgain(t *testing.T) {
	w := newWorld(t)
	w.host.FailClone(driver.ErrNoSpace)
	w.apply(container("api", "nginx"))
	w.waitReason("api", computeapi.ReasonDiskFailed)
	w.host.LoseBase("sha256:nginx-1")
	w.host.FailClone(nil)
	w.waitReason("api", "Running")
	if n := w.host.Unpacked("sha256:nginx-1"); n != 2 {
		t.Errorf("the image was unpacked %d times; want it made again once", n)
	}
}

// TestNetworkChangesWhilePullingAreNotHidden: a network change signalled
// while the agent is busy with an image is still checked, so a VM cut off
// meanwhile is not reported fine.
func TestNetworkChangesWhilePullingAreNotHidden(t *testing.T) {
	w := newWorld(t)
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	uid := w.machine("api").GetMetadata().GetUid()
	net := w.host.Snapshot().Interfaces[uid][0].NetworkUID
	w.settle("api")

	// A restarted agent prepares the image again, held up here.
	w.stopAgent()
	w.host.LoseBase("sha256:nginx-1")
	letGo := w.host.HoldUnpack("nginx")
	defer letGo()
	w.startAgent()
	time.Sleep(200 * time.Millisecond)
	w.host.FailNetworking(errors.New("netlink: no buffer space"))
	w.host.LoseNetwork(net)
	time.Sleep(200 * time.Millisecond)
	letGo()
	w.waitReason("api", computeapi.ReasonNetworkFailed)
	w.host.FailNetworking(nil)
	w.waitReason("api", "Running")
	if vm := w.host.Snapshot().VMs[uid]; vm.DeadNICs {
		t.Errorf("a VM with dead interfaces reported running: %+v", vm)
	}
}

// TestAStalledPullFailsAndIsTriedAgain: a pull that makes no progress fails
// in time, says so, and is tried again.
func TestAStalledPullFailsAndIsTriedAgain(t *testing.T) {
	w := newIdleWorld(t)
	w.pullTimeout = 300 * time.Millisecond
	w.startAgent()
	letGo := w.host.HoldUnpack("nginx")
	defer letGo()
	w.apply(container("api", "nginx"))
	c := w.waitReason("api", computeapi.ReasonImagePullFailed)
	if msg := ready(c).GetMessage(); !strings.Contains(msg, "deadline") {
		t.Errorf("message %q", msg)
	}
	letGo()
	w.waitReason("api", "Running")
}

// TestAHungGuestHoldsUpNoOtherMachine: a guest agent that takes a call and
// never answers holds up no other machine.
func TestAHungGuestHoldsUpNoOtherMachine(t *testing.T) {
	w := newWorld(t)
	letGo := w.host.HoldUnpack("nginx")
	defer letGo()
	w.apply(container("stuck", "nginx"))
	w.waitReason("stuck", computeapi.ReasonPulling)
	w.host.HangGuest(w.machine("stuck").GetMetadata().GetUid())
	letGo()
	w.waitReason("stuck", computeapi.ReasonBooting)

	w.apply(container("fine", "nginx"))
	w.waitReason("fine", "Running")
}

// TestAMachineDeletedWhilePulling: deleting a machine whose image is still
// being pulled leaves nothing behind once the pull ends.
func TestAMachineDeletedWhilePulling(t *testing.T) {
	w := newWorld(t)
	letGo := w.host.HoldUnpack("nginx")
	defer letGo()
	w.apply(container("api", "nginx"))
	w.waitReason("api", computeapi.ReasonPulling)
	w.delete("api")
	w.waitGone("api")
	time.Sleep(300 * time.Millisecond) // sweeps while the pull runs
	letGo()
	w.waitHostEmpty()
	w.eventually("the image forgotten", func() (bool, string) { return !w.host.Cached("sha256:nginx-1"), "" })
}

// TestUnpackFailuresAreBlamedRight: a layer that cannot be fetched is the
// registry's failure, and no room to write it is the disk's.
func TestUnpackFailuresAreBlamedRight(t *testing.T) {
	w := newWorld(t)
	w.host.FailUnpack("nginx", errors.New("layer sha256:abc: unexpected EOF"))
	w.apply(container("api", "nginx"))
	c := w.waitReason("api", computeapi.ReasonImagePullFailed)
	if msg := ready(c).GetMessage(); !strings.Contains(msg, "unexpected EOF") {
		t.Errorf("message %q", msg)
	}
	w.host.FailUnpack("nginx", fmt.Errorf("writing layer: %w", driver.ErrNoSpace))
	w.waitReason("api", computeapi.ReasonDiskFailed)
	w.host.FailUnpack("nginx", nil)
	w.waitReason("api", "Running")
}
