package node

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// overlaps records each driver call that touches a machine while its VM is
// being stopped (a second stop, a start, or removing its logs, disk or
// interfaces), and each that fails: with no fault injected, only a call
// out of order does, such as a start while another spec runs.
type overlaps struct {
	mu       sync.Mutex
	stopping map[string]int
	found    []string
}

func (o *overlaps) touch(op, uid string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stopping[uid] > 0 {
		o.found = append(o.found, fmt.Sprintf("%s of machine %s while it was being stopped", op, uid))
	}
}

func (o *overlaps) check(ctx context.Context, op, uid string, err error) error {
	if err != nil && ctx.Err() == nil {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.found = append(o.found, fmt.Sprintf("%s of machine %s failed: %v", op, uid, err))
	}
	return err
}

func (o *overlaps) report(t *testing.T) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, f := range o.found {
		t.Error(f)
	}
}

type checkedHypervisor struct {
	driver.Hypervisor
	o *overlaps
}

func (h checkedHypervisor) Start(ctx context.Context, spec driver.VMSpec) error {
	h.o.touch("Start", spec.MachineUID)
	return h.o.check(ctx, "Start", spec.MachineUID, h.Hypervisor.Start(ctx, spec))
}

func (h checkedHypervisor) Stop(ctx context.Context, uid string) error {
	h.o.touch("Stop", uid)
	h.o.mu.Lock()
	h.o.stopping[uid]++
	h.o.mu.Unlock()
	defer func() {
		h.o.mu.Lock()
		h.o.stopping[uid]--
		h.o.mu.Unlock()
	}()
	return h.o.check(ctx, "Stop", uid, h.Hypervisor.Stop(ctx, uid))
}

func (h checkedHypervisor) DeleteLogs(ctx context.Context, uid string) error {
	h.o.touch("DeleteLogs", uid)
	return h.o.check(ctx, "DeleteLogs", uid, h.Hypervisor.DeleteLogs(ctx, uid))
}

type checkedStorage struct {
	driver.Storage
	o *overlaps
}

func (s checkedStorage) DeleteRoot(ctx context.Context, uid string) error {
	s.o.touch("DeleteRoot", uid)
	return s.o.check(ctx, "DeleteRoot", uid, s.Storage.DeleteRoot(ctx, uid))
}

type checkedNetwork struct {
	driver.Network
	o *overlaps
}

func (n checkedNetwork) DeleteInterfaces(ctx context.Context, uid string) error {
	n.o.touch("DeleteInterfaces", uid)
	return n.o.check(ctx, "DeleteInterfaces", uid, n.Network.DeleteInterfaces(ctx, uid))
}

// TestAStopRacesNothing: while machines are stopped, started again, changed
// in ways that stop their VMs, deleted, and lost from the API in the middle
// of their release, the agent's own worker and sweep and more reconciles and
// sweeps besides all run at once. No machine's VM is ever started, stopped
// again, or stripped of its disk, interfaces or logs while its stop runs,
// and once every machine is gone the host holds nothing.
func TestAStopRacesNothing(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	host := fake.New()
	host.AddImage("nginx", "sha256:1", driver.ImageConfig{Entrypoint: []string{"nginx"}})
	host.AddImage("nginx:2", "sha256:2", driver.ImageConfig{Entrypoint: []string{"nginx"}})
	// Long enough for the others to come by while it runs, even under the
	// race detector.
	host.DelayStop("", 20*time.Millisecond)
	o := &overlaps{stopping: map[string]int{}}
	a, err := New(Config{
		Name: apiservertest.Node, HTTP: client.UnixSocket(env.Socket), BaseURL: client.UnixSocketURL,
		HeartbeatInterval: 50 * time.Millisecond,
		SweepInterval:     5 * time.Millisecond,
		Drivers: &Drivers{
			Images: host.Images(), Storage: checkedStorage{host.Storage(), o}, Network: checkedNetwork{host.Network(), o},
			Hypervisor: checkedHypervisor{host.Hypervisor(), o}, Guest: host.Guest(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = a.Run(ctx)
	}()
	// More reconciles of every machine, and more sweeps, than the agent
	// runs by itself.
	const machines = 4
	for i := range machines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_ = a.reconcileMachine(ctx, controller.Key{Project: apiservertest.Project, Name: fmt.Sprintf("m%d", i)})
				time.Sleep(5 * time.Millisecond) //nolint:forbidigo // paces the extra reconciles
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			_ = a.sweep(ctx)
			time.Sleep(5 * time.Millisecond) //nolint:forbidigo // paces the extra sweeps
		}
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	sys := env.AsSystem()
	var mwg sync.WaitGroup
	for i := range machines {
		mwg.Add(1)
		go func() {
			defer mwg.Done()
			churn(offTestGoroutine{t}, env, sys, a, host, fmt.Sprintf("m%d", i), fmt.Sprintf("10.0.0.%d/24", 10+i), i%2 == 1)
		}()
	}
	mwg.Wait()

	testutil.Eventually(t, "nothing left on the host", func() (bool, string) {
		s := host.Snapshot()
		return s.Empty(), fmt.Sprintf("%+v", s)
	})
	o.report(t)
}

// offTestGoroutine lets a helper that fails with Fatal run on a goroutine
// the test started: it fails the test and ends only that goroutine, as
// FailNow may not be called off the test's own.
type offTestGoroutine struct{ testing.TB }

func (g offTestGoroutine) Fatal(args ...any) {
	g.Helper()
	g.Error(args...)
	runtime.Goexit()
}

func (g offTestGoroutine) Fatalf(format string, args ...any) {
	g.Helper()
	g.Errorf(format, args...)
	runtime.Goexit()
}

// churn runs one machine through each way of stopping its VM, then deletes
// it; lost takes the machine out of the API while its release's stop runs,
// so the sweep finds what it left.
func churn(t testing.TB, env *apiservertest.Env, sys *client.Client, a *Agent, host *fake.Host, name, ipv4 string, lost bool) {
	m := &computev1alpha1.Machine{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: name, Finalizers: []string{"hangarlab.cloud/node-cleanup"}},
		Spec: &computev1alpha1.MachineSpec{
			Node:          apiservertest.Node,
			Resources:     &computev1alpha1.Resources{Cpu: "1", Memory: "512Mi", Disk: "10Gi"},
			RunState:      computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING,
			Boot:          &computev1alpha1.MachineSpec_Container{Container: &computev1alpha1.ContainerBoot{Image: "nginx"}},
			Interfaces:    []*computev1alpha1.MachineInterface{{Name: "eth0", Network: "default", Ipv4: ipv4}},
			RestartPolicy: computev1alpha1.RestartPolicy_RESTART_POLICY_ALWAYS,
		},
	}
	if _, err := sys.Apply(context.Background(), registry.Machine, m); err != nil {
		t.Error(err)
		return
	}
	running := func(m *computev1alpha1.Machine) bool {
		return m.GetStatus().GetPhase() == computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING &&
			conditions.IsTrue(m.GetStatus().GetConditions(), computeapi.MachineReady) &&
			m.GetStatus().GetObservedGeneration() == m.GetMetadata().GetGeneration()
	}
	steps := []func(*computev1alpha1.Machine){
		// Stopped, and started again before the stop can have ended.
		func(m *computev1alpha1.Machine) {
			m.Spec.RunState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
		},
		func(m *computev1alpha1.Machine) {
			m.Spec.RunState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING
		},
		// Another command stops the VM to start it again.
		func(m *computev1alpha1.Machine) {
			m.Spec.GetContainer().Args = []string{"-c", "/etc/other.conf"}
		},
		// Another image stops it to rebuild its disk.
		func(m *computev1alpha1.Machine) {
			m.Spec.GetContainer().Image = "nginx:2"
		},
		// Another address stops it to make its interface again.
		func(m *computev1alpha1.Machine) {
			m.Spec.Interfaces[0].Ipv4 = ipv4[:len(ipv4)-3] + "1" + ipv4[len(ipv4)-3:]
		},
	}
	waitMachine(t, sys, name, "running", running)
	for i, step := range steps {
		if !edit(t, sys, name, step) {
			return
		}
		// Every other step lands while the last is still being carried
		// out.
		if i%2 == 1 {
			waitMachine(t, sys, name, "running", running)
		}
	}
	waitMachine(t, sys, name, "running", running)
	var uid string
	if lost {
		obj, err := sys.Get(context.Background(), registry.Machine, client.Key{Project: apiservertest.Project, Name: name})
		if err != nil {
			t.Error(err)
			return
		}
		uid = obj.(*computev1alpha1.Machine).GetMetadata().GetUid()
		letGo := host.DelayStop(uid, time.Hour)
		defer letGo()
	}
	if _, err := sys.Delete(context.Background(), registry.Machine, client.Key{Project: apiservertest.Project, Name: name}); err != nil {
		t.Error(err)
		return
	}
	if lost {
		// Gone from the API while its release's stop runs, as if its
		// finalizer were taken off by hand; sweeps come by meanwhile.
		testutil.Eventually(t, "machine "+name+"'s release stopping its VM", func() bool {
			a.host.Lock()
			defer a.host.Unlock()
			return a.stops.running[uid]
		})
		key := store.Key{Kind: registry.Machine.Kind, Project: apiservertest.Project, Name: name}
		for {
			obj, err := env.Store.Get(context.Background(), key)
			if err != nil {
				t.Error(err)
				return
			}
			if _, _, err := env.Store.Delete(context.Background(), key, obj.Revision); err == nil {
				break
			}
		}
		time.Sleep(30 * time.Millisecond) //nolint:forbidigo // lets background work run
	}
	waitMachine(t, sys, name, "gone", nil)
}

// edit changes the machine as it is now, again if another write came in
// between.
func edit(t testing.TB, sys *client.Client, name string, change func(*computev1alpha1.Machine)) bool {
	for range 100 {
		obj, err := sys.Get(context.Background(), registry.Machine, client.Key{Project: apiservertest.Project, Name: name})
		if err != nil {
			t.Error(err)
			return false
		}
		m := proto.Clone(obj.(*computev1alpha1.Machine)).(*computev1alpha1.Machine)
		change(m)
		_, err = sys.Apply(context.Background(), registry.Machine, m)
		if connect.CodeOf(err) == connect.CodeAborted {
			continue
		}
		if err != nil {
			t.Error(err)
			return false
		}
		return true
	}
	t.Errorf("machine %s: every edit lost to another write", name)
	return false
}

// waitMachine waits until the machine satisfies ok, or is gone if ok is
// nil.
func waitMachine(t testing.TB, sys *client.Client, name, what string, ok func(*computev1alpha1.Machine) bool) {
	t.Helper()
	testutil.Eventually(t, "machine "+name+" "+what, func() (bool, string) {
		obj, err := sys.Get(context.Background(), registry.Machine, client.Key{Project: apiservertest.Project, Name: name})
		switch {
		case connect.CodeOf(err) == connect.CodeNotFound:
			return ok == nil, "gone"
		case err != nil:
			t.Fatal(err)
		}
		m := obj.(*computev1alpha1.Machine)
		return ok != nil && ok(m), fmt.Sprint(m.GetStatus())
	})
}
