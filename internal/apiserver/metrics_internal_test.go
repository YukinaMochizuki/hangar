package apiserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/metrics/metricstest"
)

// apiserverMetrics are the families the apiserver registers.
var apiserverMetrics = []string{
	"hangar_apiserver_rpc_requests_total", "hangar_apiserver_rpc_duration_seconds",
	"hangar_apiserver_node_ready", "hangar_apiserver_machines", "hangar_apiserver_machines_crash_looping",
}

func registryOf(s *Server) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(s.Collectors()...)
	return reg
}

// machineStatus is what a node reports on a machine it runs.
func machineStatus(phase computev1alpha1.MachinePhase, reason string, booted time.Time) *computev1alpha1.MachineStatus {
	ready := metav1.ConditionStatus_CONDITION_STATUS_FALSE
	if reason == computeapi.ReasonRunning {
		ready = metav1.ConditionStatus_CONDITION_STATUS_TRUE
	}
	st := &computev1alpha1.MachineStatus{
		Phase:      phase,
		Conditions: []*metav1.Condition{{Type: computeapi.MachineReady, Status: ready, Reason: reason}},
	}
	if !booted.IsZero() {
		st.BootTime = timestamppb.New(booted)
	}
	return st
}

// TestMetricsNameNoTenant runs every apiserver metric over a store whose
// tenant, project, machine and person names all carry a marker, and finds
// the marker nowhere in what the metrics listener serves.
func TestMetricsNameNoTenant(t *testing.T) {
	const marker = "qz7"
	const (
		tenantName  = "tenant-" + marker
		projectName = "proj-" + marker
		carol       = "carol-" + marker + "@example.com"
	)
	s, system, _ := runningWith(t, func(c *Config) { c.Nodes = []string{"n1", "n2"} })
	waitNode(t, s, "n1")
	waitNode(t, s, "n2")
	addMember(t, s, carol)
	owner := auth.WithPrincipal(system, auth.Principal{Type: auth.User, ID: carol})
	if _, err := s.backend(registry.Tenant).Apply(owner, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: tenantName}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Project).Apply(owner, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: projectName}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: tenantName},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Network).Apply(system, &networkv1alpha1.Network{
		Metadata: &metav1.ObjectMeta{Project: projectName, Name: DefaultNetwork},
		Spec:     &networkv1alpha1.NetworkSpec{Ipv4Cidr: DefaultNetworkCIDR},
	}); err != nil {
		t.Fatal(err)
	}
	report(t, s, system, "n1", s.clock.Now())

	machines := s.backend(registry.Machine)
	t0 := s.clock.Now()
	statuses := map[string]*computev1alpha1.MachineStatus{
		"api-" + marker:    machineStatus(computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning, t0),
		"worker-" + marker: machineStatus(computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning, t0),
		"job-" + marker:    machineStatus(computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, computeapi.ReasonExited, t0),
		// A reason the apiserver does not know is no label value.
		"odd-" + marker: machineStatus(computev1alpha1.MachinePhase_MACHINE_PHASE_FAILED, "Odd-"+marker, t0),
	}
	var uids []string
	for name, st := range statuses {
		m := machine(name, "n1")
		m.Metadata.Project = projectName
		applied, err := machines.Apply(system, m)
		if err != nil {
			t.Fatal(err)
		}
		uids = append(uids, applied.Object.(*computev1alpha1.Machine).GetMetadata().GetUid())
		if _, err := machines.UpdateStatus(asNode(system, "n1"), resource.Key{Project: projectName, Name: name}, st, nil); err != nil {
			t.Fatal(err)
		}
	}
	waitSettled(t, s)

	// Calls that succeed, fail, and are refused, each naming the tenant's
	// project and machine.
	srv := httptest.NewServer(s.Handler(auth.Chain{auth.Dev{}}))
	t.Cleanup(srv.Close)
	call := func(email, name string) error {
		c := computev1alpha1connect.NewMachineServiceClient(http.DefaultClient, srv.URL)
		req := connect.NewRequest(&computev1alpha1.GetMachineRequest{Project: projectName, Name: name})
		if email != "" {
			req.Header().Set(auth.DevUserHeader, email)
		}
		_, err := c.GetMachine(context.Background(), req)
		return err
	}
	if err := call(carol, "api-"+marker); err != nil {
		t.Fatal(err)
	}
	wantCodeOf(t, call(carol, "missing-"+marker), connect.CodeNotFound, "reading a missing machine")
	wantCodeOf(t, call("", "api-"+marker), connect.CodeUnauthenticated, "reading without credentials")

	reg := registryOf(s)
	metricstest.CheckLabels(t, reg, apiserverMetrics...)
	metricstest.CheckAbsent(t, reg, append(uids, marker)...)

	get := computev1alpha1connect.MachineServiceGetMachineProcedure
	for _, w := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"hangar_apiserver_rpc_requests_total", map[string]string{"procedure": get, "code": "ok"}, 1},
		{"hangar_apiserver_rpc_requests_total", map[string]string{"procedure": get, "code": "not_found"}, 1},
		{"hangar_apiserver_rpc_requests_total", map[string]string{"procedure": get, "code": "unauthenticated"}, 1},
		{"hangar_apiserver_rpc_duration_seconds", map[string]string{"procedure": get}, 3},
		{"hangar_apiserver_node_ready", map[string]string{"node": "n1", "status": "True"}, 1},
		{"hangar_apiserver_node_ready", map[string]string{"node": "n1", "status": "Unknown"}, 0},
		{"hangar_apiserver_node_ready", map[string]string{"node": "n2", "status": "True"}, 0},
		{"hangar_apiserver_node_ready", map[string]string{"node": "n2", "status": "Unknown"}, 1},
		{"hangar_apiserver_machines", map[string]string{"phase": "Running", "reason": "Running"}, 2},
		{"hangar_apiserver_machines", map[string]string{"phase": "Stopped", "reason": "Exited"}, 1},
		{"hangar_apiserver_machines", map[string]string{"phase": "Failed", "reason": "Other"}, 1},
		{"hangar_apiserver_machines_crash_looping", map[string]string{}, 0},
	} {
		got, ok := metricstest.Value(t, reg, w.name, w.labels)
		if !ok || got != w.want {
			t.Errorf("%s %v = %v (present %t), want %v", w.name, w.labels, got, ok, w.want)
		}
	}
}

func TestCrashLoopingMachinesAreCounted(t *testing.T) {
	clock := clockwork.NewFakeClock()
	s, system, _ := runningWith(t, func(c *Config) {
		c.Clock = clock
		c.Nodes = []string{"n1"}
	})
	waitNode(t, s, "n1")
	machines := s.backend(registry.Machine)
	for _, name := range []string{"loops", "calm"} {
		if _, err := machines.Apply(system, machine(name, "n1")); err != nil {
			t.Fatal(err)
		}
	}
	boot := func(name string) {
		t.Helper()
		st := machineStatus(computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, computeapi.ReasonRunning, clock.Now())
		if _, err := machines.UpdateStatus(asNode(system, "n1"), resource.Key{Project: "web", Name: name}, st, nil); err != nil {
			t.Fatal(err)
		}
		waitSettled(t, s)
	}
	reg := registryOf(s)
	looping := func() float64 {
		t.Helper()
		v, _ := metricstest.Value(t, reg, "hangar_apiserver_machines_crash_looping", map[string]string{})
		return v
	}

	// The first boot seen is no change; then a boot a minute, as a node
	// restarting a container that keeps exiting does. The calm machine
	// boots once.
	boot("loops")
	boot("calm")
	for range crashLoopBoots {
		clock.Advance(time.Minute)
		boot("loops")
	}
	// The same status written again, its boot time with it, is no boot.
	boot("loops")
	if n := looping(); n != 0 {
		t.Errorf("after %d boots in %v, %v machines loop; want 0", crashLoopBoots, crashLoopBoots*time.Minute, n)
	}
	clock.Advance(time.Minute)
	boot("loops")
	if n := looping(); n != 1 {
		t.Errorf("after %d boots in %v, %v machines loop; want 1", crashLoopBoots+1, (crashLoopBoots+1)*time.Minute, n)
	}

	// Its first boot leaves the window, and with it the loop.
	clock.Advance(crashLoopWindow - crashLoopBoots*time.Minute - time.Second)
	if n := looping(); n != 1 {
		t.Errorf("a second before its first boot left the window, %v machines loop; want 1", n)
	}
	clock.Advance(time.Second)
	if n := looping(); n != 0 {
		t.Errorf("once its first boot left the window, %v machines loop; want 0", n)
	}
}
