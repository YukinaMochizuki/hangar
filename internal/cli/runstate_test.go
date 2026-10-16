package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/cli"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// containerJSON is the part of a container the stop and start tests look at.
type containerJSON struct {
	Metadata struct {
		Name            string            `json:"name"`
		ResourceVersion string            `json:"resourceVersion"`
		Generation      string            `json:"generation"`
		Labels          map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Image     string            `json:"image"`
		Ports     []json.RawMessage `json:"ports"`
		Resources map[string]string `json:"resources"`
		RunState  string            `json:"runState"`
	} `json:"spec"`
}

func (w *world) container(name string) containerJSON {
	w.t.Helper()
	var c containerJSON
	if err := json.Unmarshal([]byte(w.must("", "get", "ct", name, "-o", "json")), &c); err != nil {
		w.t.Fatal(err)
	}
	return c
}

// runWith runs the CLI with an HTTP client of the test's own.
func (w *world) runWith(hc *http.Client, args ...string) result {
	w.t.Helper()
	var out, errb bytes.Buffer
	code := cli.Run(context.Background(), args, cli.IO{
		In: strings.NewReader(""), Out: &out, Err: &errb,
		Env: func(k string) string {
			if k == "HANGAR_CONFIG" {
				return w.config
			}
			return ""
		},
	}, cli.Options{HTTPClient: hc})
	return result{code, out.String(), errb.String()}
}

// beforeApply calls hook before each ApplyContainer request it passes on.
type beforeApply struct {
	hook  func()
	calls atomic.Int32
}

func (b *beforeApply) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/ApplyContainer") {
		b.calls.Add(1)
		b.hook()
	}
	return http.DefaultTransport.RoundTrip(req)
}

func TestStopStart(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	w.must(stack, "apply", "-f", "-")
	before := w.container("api")
	if before.Spec.RunState != "" {
		t.Fatalf("a fresh container has run state %q", before.Spec.RunState)
	}

	// Unset is RUNNING, so there is nothing to start.
	if out := w.must("", "start", "container", "api"); out != "container/api already running\n" {
		t.Errorf("start of a running container printed %q", out)
	}
	if got := w.container("api"); got.Metadata.Generation != before.Metadata.Generation {
		t.Errorf("start of a running container changed its spec: generation %s, was %s", got.Metadata.Generation, before.Metadata.Generation)
	}

	if out := w.must("", "stop", "container", "api"); out != "container/api stopped\n" {
		t.Errorf("stop printed %q", out)
	}
	stopped := w.container("api")
	if stopped.Spec.RunState != "CONTAINER_RUN_STATE_STOPPED" {
		t.Errorf("after stop, run state %q", stopped.Spec.RunState)
	}
	if stopped.Spec.Image != before.Spec.Image || len(stopped.Spec.Ports) != 1 || stopped.Metadata.Labels["tier"] != "front" ||
		stopped.Spec.Resources["cpu"] != "2" || stopped.Spec.Resources["memory"] != "512Mi" || stopped.Spec.Resources["disk"] != "20Gi" {
		t.Errorf("stop changed more than the run state: %+v", stopped)
	}
	if stopped.Metadata.Generation == before.Metadata.Generation {
		t.Errorf("stop left the generation at %s", stopped.Metadata.Generation)
	}

	if out := w.must("", "stop", "ct", "api"); out != "container/api already stopped\n" {
		t.Errorf("second stop printed %q", out)
	}
	if got := w.container("api"); got.Metadata.Generation != stopped.Metadata.Generation {
		t.Errorf("second stop changed the spec: generation %s, was %s", got.Metadata.Generation, stopped.Metadata.Generation)
	}

	if out := w.must("", "start", "Container", "api", "-o", "name"); out != "container/api\n" {
		t.Errorf("start -o name printed %q", out)
	}
	if got := w.container("api"); got.Spec.RunState != "CONTAINER_RUN_STATE_RUNNING" || got.Spec.Image != before.Spec.Image {
		t.Errorf("after start: %+v", got.Spec)
	}

	var js containerJSON
	if err := json.Unmarshal([]byte(w.must("", "stop", "containers", "api", "-o", "json")), &js); err != nil {
		t.Fatal(err)
	}
	if js.Metadata.Name != "api" || js.Spec.RunState != "CONTAINER_RUN_STATE_STOPPED" || js.Metadata.ResourceVersion == "" {
		t.Errorf("stop -o json printed %+v", js)
	}
	// A no-op prints the container as it is.
	if err := json.Unmarshal([]byte(w.must("", "stop", "ct", "api", "-o", "json")), &js); err != nil {
		t.Fatal(err)
	}
	if js.Spec.RunState != "CONTAINER_RUN_STATE_STOPPED" {
		t.Errorf("no-op stop -o json printed %+v", js)
	}
}

// TestStopStartRetriesConflicts: a write between the read and the apply makes
// the apply fail on the resource version; the command reads again and keeps
// that write.
func TestStopStartRetriesConflicts(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	w.must(stack, "apply", "-f", "-")
	ct := strings.Split(stack, "---\n")[2]

	n := 0
	once := &beforeApply{hook: func() {
		if n++; n == 1 {
			w.must(strings.Replace(ct, "tier: front", "tier: back", 1), "apply", "-f", "-")
		}
	}}
	r := w.runWith(&http.Client{Transport: once}, "stop", "ct", "api")
	if r.code != 0 || r.out != "container/api stopped\n" {
		t.Fatalf("stop with one conflict: exit %d: %q %s", r.code, r.out, r.err)
	}
	// The controllers' status writes can cost another try.
	if got := once.calls.Load(); got < 2 {
		t.Errorf("stop applied %d times, want at least 2", got)
	}
	got := w.container("api")
	if got.Spec.RunState != "CONTAINER_RUN_STATE_STOPPED" || got.Metadata.Labels["tier"] != "back" {
		t.Errorf("after a retried stop: run state %q, labels %v", got.Spec.RunState, got.Metadata.Labels)
	}

	// A writer that gets in before every apply: each of its writes runs the
	// container again, so every fresh read still has something to stop, and
	// stop gives up after a bounded number of tries.
	w.must("", "start", "ct", "api")
	i := 0
	always := &beforeApply{hook: func() {
		i++
		w.must(strings.Replace(ct, "tier: front", fmt.Sprintf("tier: t%d", i), 1), "apply", "-f", "-")
	}}
	r = w.runWith(&http.Client{Transport: always}, "stop", "ct", "api")
	if r.code != cli.ExitConflict || !strings.Contains(r.err, "5 tries") {
		t.Errorf("stop that always conflicts: exit %d: %s", r.code, r.err)
	}
	if got := always.calls.Load(); got != 5 {
		t.Errorf("stop applied %d times, want 5", got)
	}
}

func TestStopStartWait(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	w.must(stack, "apply", "-f", "-")
	node := w.node()
	node.ready()
	m := node.placed("ct-api")
	node.report(m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration())

	run := func(args ...string) chan result {
		ch := make(chan result, 1)
		go func() { ch <- w.run("", args...) }()
		return ch
	}
	finished := func(ch chan result) result {
		t.Helper()
		select {
		case r := <-ch:
			return r
		case <-time.After(10 * time.Second):
			t.Fatal("the command did not return")
		}
		return result{}
	}
	// nextGeneration waits for the machine to take a new spec.
	nextGeneration := func(after int64) *computev1alpha1.Machine {
		t.Helper()
		var m *computev1alpha1.Machine
		testutil.Eventually(t, "the machine taking the new spec", func() bool {
			m = node.placed("ct-api")
			return m.GetMetadata().GetGeneration() > after
		})
		return m
	}

	ch := run("stop", "ct", "api", "--wait", "--timeout=10s")
	m = nextGeneration(m.GetMetadata().GetGeneration())
	if m.GetSpec().GetRunState() != computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED {
		t.Fatalf("machine run state %v", m.GetSpec().GetRunState())
	}
	select {
	case r := <-ch:
		t.Fatalf("stop --wait returned before the node stopped the machine: exit %d: %s", r.code, r.err)
	case <-time.After(300 * time.Millisecond):
	}
	node.report(m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration())
	if r := finished(ch); r.code != 0 || r.out != "container/api stopped\n" {
		t.Fatalf("stop --wait: exit %d: %q %s", r.code, r.out, r.err)
	}
	// Already stopped, and the stop has taken effect: --wait returns at once.
	if r := finished(run("stop", "ct", "api", "--wait", "--timeout=10s")); r.code != 0 || r.out != "container/api already stopped\n" {
		t.Errorf("no-op stop --wait: exit %d: %q %s", r.code, r.out, r.err)
	}

	ch = run("start", "ct", "api", "--wait", "--timeout=10s")
	m = nextGeneration(m.GetMetadata().GetGeneration())
	select {
	case r := <-ch:
		t.Fatalf("start --wait returned before the machine ran: exit %d: %s", r.code, r.err)
	case <-time.After(300 * time.Millisecond):
	}
	node.report(m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration())
	if r := finished(ch); r.code != 0 || r.out != "container/api started\n" {
		t.Fatalf("start --wait: exit %d: %q %s", r.code, r.out, r.err)
	}

	// The change is made even when the wait gives up.
	r := w.run("", "stop", "ct", "api", "--wait", "--timeout=200ms")
	if r.code != cli.ExitTimeout || r.out != "container/api stopped\n" {
		t.Errorf("stop --wait that times out: exit %d: %q %s", r.code, r.out, r.err)
	}
	if got := w.container("api"); got.Spec.RunState != "CONTAINER_RUN_STATE_STOPPED" {
		t.Errorf("after a stop whose wait timed out, run state %q", got.Spec.RunState)
	}
}

func TestStopStartErrors(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	w.must(stack, "apply", "-f", "-")

	tests := []struct {
		name string
		args []string
		want int
	}{
		{"not found", []string{"stop", "container", "missing"}, cli.ExitNotFound},
		{"start not found", []string{"start", "ct", "missing"}, cli.ExitNotFound},
		{"not a container", []string{"stop", "tenant", "acme"}, cli.ExitUsage},
		{"unknown kind", []string{"start", "widget", "x"}, cli.ExitUsage},
		{"no name", []string{"stop", "ct"}, cli.ExitUsage},
		{"bad output", []string{"stop", "ct", "api", "-o", "yaml"}, cli.ExitUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := w.run("", tt.args...); r.code != tt.want {
				t.Errorf("exit %d (%s), want %d", r.code, strings.TrimSpace(r.err), tt.want)
			}
		})
	}
	if get, stop := w.run("", "get", "ct", "missing"), w.run("", "stop", "ct", "missing"); get.err != stop.err {
		t.Errorf("stop of a missing container says %q, get says %q", stop.err, get.err)
	}

	w.as("bob@example.com")
	for _, verb := range []string{"stop", "start"} {
		r := w.run("", verb, "ct", "api")
		if r.code != cli.ExitError || !strings.Contains(r.err, "permission_denied") {
			t.Errorf("bob %s alice's container: exit %d: %s", verb, r.code, r.err)
		}
	}
	w.as("alice@example.com")
	if got := w.container("api"); got.Spec.RunState != "" {
		t.Errorf("bob's attempts changed the run state to %q", got.Spec.RunState)
	}
}

const vmStack = `apiVersion: storage.hangarlab.cloud/v1alpha1
kind: Image
metadata:
  name: noble
spec:
  url: https://cloud-images.example/noble.img
  sha256: 1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a
  os: ubuntu
  version: "24.04"
`

const webVM = `apiVersion: compute.hangarlab.cloud/v1alpha1
kind: VirtualMachine
metadata:
  name: web
spec:
  rootDisk:
    fromImage:
      image:
        name: noble
  resources:
    memory: 2Gi
`

// vmJSON is the part of a VM the stop and start tests look at.
type vmJSON struct {
	Metadata struct {
		Generation string `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		RunState  string            `json:"runState"`
		Resources map[string]string `json:"resources"`
		RootDisk  json.RawMessage   `json:"rootDisk"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration string `json:"observedGeneration"`
	} `json:"status"`
}

func (w *world) vm(name string) vmJSON {
	w.t.Helper()
	var v vmJSON
	if err := json.Unmarshal([]byte(w.must("", "get", "vm", name, "-o", "json")), &v); err != nil {
		w.t.Fatal(err)
	}
	return v
}

// TestStopStartAVirtualMachine: stop and start set a VM's run state, as a
// container's, and leave the rest of its spec; start on a VM meant to run
// whose guest powered itself off fails, saying what boots it again.
func TestStopStartAVirtualMachine(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	w.must(stack, "apply", "-f", "-")
	w.as("root@example.com")
	w.must(vmStack, "apply", "-f", "-")
	w.as("alice@example.com")
	w.must(webVM, "apply", "-f", "-")
	before := w.vm("web")

	if out := w.must("", "start", "vm", "web"); out != "virtualmachine/web already running\n" {
		t.Errorf("start of a running VM printed %q", out)
	}
	if out := w.must("", "stop", "virtualmachine", "web"); out != "virtualmachine/web stopped\n" {
		t.Errorf("stop printed %q", out)
	}
	stopped := w.vm("web")
	if stopped.Spec.RunState != "VIRTUAL_MACHINE_RUN_STATE_STOPPED" || stopped.Spec.Resources["memory"] != "2Gi" ||
		string(stopped.Spec.RootDisk) != string(before.Spec.RootDisk) || stopped.Metadata.Generation == before.Metadata.Generation {
		t.Errorf("after stop: %+v, before %+v", stopped, before)
	}
	if out := w.must("", "stop", "vm", "web"); out != "virtualmachine/web already stopped\n" {
		t.Errorf("second stop printed %q", out)
	}
	if out := w.must("", "start", "vm", "web"); out != "virtualmachine/web started\n" {
		t.Errorf("start printed %q", out)
	}
	if got := w.vm("web"); got.Spec.RunState != "VIRTUAL_MACHINE_RUN_STATE_RUNNING" {
		t.Errorf("after start, run state %q", got.Spec.RunState)
	}

	// The node reports the VM ended with reason and message, and the VM
	// reads so.
	node := w.node()
	node.ready()
	// The node reports against the machine as it is once the controller has
	// carried the VM's latest spec to it; until then the machine may still be
	// at the stop, and a report for that generation reads as Updating.
	testutil.Eventually(t, "the VM controller observing the VM's generation", func() (bool, string) {
		v := w.vm("web")
		return v.Status.ObservedGeneration == v.Metadata.Generation, fmt.Sprintf("%+v", v)
	})
	ended := func(phase computev1alpha1.MachinePhase, reason, msg string) {
		t.Helper()
		m := node.placed("vm-web")
		if m.GetSpec().GetRunState() != computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING {
			t.Fatalf("machine run state %v, want running", m.GetSpec().GetRunState())
		}
		node.report(m, phase, m.GetMetadata().GetGeneration(), &metav1.Condition{
			Type: "Ready", Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reason, Message: msg,
			ObservedGeneration: m.GetMetadata().GetGeneration(),
		})
		testutil.Eventually(t, "the VM reading as "+reason, func() (bool, string) {
			out := w.must("", "get", "vm", "web", "-o", "json")
			return strings.Contains(out, `"`+reason+`"`), out
		})
	}

	// It crashed, and waits out its restart's backoff: it boots again on
	// its own, and start says nothing more.
	ended(computev1alpha1.MachinePhase_MACHINE_PHASE_FAILED, "Exited", "the VM ended without powering off, code 253")
	if r := w.run("", "start", "vm", "web"); r.code != 0 || r.out != "virtualmachine/web already running\n" {
		t.Errorf("start of a crashed VM: exit %d: %q %s", r.code, r.out, r.err)
	}

	// Its guest powers itself off: start has nothing to ask, and says how
	// to boot it again.
	ended(computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, "PoweredOff", "the guest powered off")
	r := w.run("", "start", "vm", "web", "--wait", "--timeout=10s")
	if r.code != cli.ExitError || r.out != "" || !strings.Contains(r.err, "virtualmachine/web is meant to run, but is off: the guest powered off") ||
		!strings.Contains(r.err, "hangar stop vm web --wait && hangar start vm web") {
		t.Errorf("start of a powered-off VM: exit %d: %q %s", r.code, r.out, r.err)
	}
}
