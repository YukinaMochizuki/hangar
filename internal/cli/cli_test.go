package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/cli"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
)

// world is a running apiserver plus a config directory, for running the CLI
// against it as different users.
type world struct {
	t      *testing.T
	url    string
	socket string
	config string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hangar.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := apiserver.New(apiserver.Config{Store: st, BootstrapAdmin: "root@example.com", Nodes: []string{testNode}})
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	public := httptest.NewServer(srv.Handler(auth.Chain{auth.Dev{}}))
	sock := filepath.Join(t.TempDir(), "node.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	node := &http.Server{
		Handler:     srv.Handler(auth.Chain{auth.PeerCred{NodeUIDs: []uint32{uint32(os.Getuid())}, NodeName: testNode}}),
		ConnContext: auth.ConnContext,
	}
	go func() { _ = node.Serve(ln) }()
	t.Cleanup(func() {
		public.Close()
		_ = node.Close()
		cancel()
		<-done
		_ = st.Close()
	})
	w := &world{t: t, url: public.URL, socket: sock, config: filepath.Join(t.TempDir(), "config.yaml")}
	w.addMembers("alice@example.com", "bob@example.com")
	return w
}

// addMembers makes people platform members, as the bootstrap admin, the way
// an administrator would with the CLI.
func (w *world) addMembers(emails ...string) {
	w.t.Helper()
	w.as("root@example.com")
	var docs []string
	for i, e := range emails {
		docs = append(docs, fmt.Sprintf(`apiVersion: iam.hangar.dev/v1alpha1
kind: RoleBinding
metadata: {name: member-%d}
spec: {subject: {user: %q}, role: platform.member, scope: {platform: {}}}
`, i, e))
	}
	// The bootstrap admin's grant is written in the background at start.
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := w.run(strings.Join(docs, "---\n"), "apply", "-f", "-")
		if r.code == 0 {
			return
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("adding members: exit %d: %s", r.code, r.err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type result struct {
	code     int
	out, err string
}

// run runs the CLI with the world's config file.
func (w *world) run(stdin string, args ...string) result {
	return w.runEnv(stdin, nil, args...)
}

func (w *world) runEnv(stdin string, env map[string]string, args ...string) result {
	w.t.Helper()
	var out, errb bytes.Buffer
	code := cli.Run(context.Background(), args, cli.IO{
		In: strings.NewReader(stdin), Out: &out, Err: &errb,
		Env: func(k string) string {
			if k == "HANGAR_CONFIG" {
				return w.config
			}
			return env[k]
		},
	}, cli.Options{})
	return result{code, out.String(), errb.String()}
}

func (w *world) must(stdin string, args ...string) string {
	w.t.Helper()
	r := w.run(stdin, args...)
	if r.code != 0 {
		w.t.Fatalf("hangar %s: exit %d: %s", strings.Join(args, " "), r.code, r.err)
	}
	return r.out
}

// as makes a dev-auth context for the user current.
func (w *world) as(user string) {
	w.t.Helper()
	w.must("", "config", "set-context", user, "--endpoint", w.url, "--auth", "dev", "--user", user, "--project", "web", "--use")
}

// testNode is the Node the agent on the test socket acts as.
const testNode = "n1"

// fakeNode plays the node agent over the socket, as hangar-node will.
type fakeNode struct {
	t        *testing.T
	nodes    computev1alpha1connect.NodeServiceClient
	machines computev1alpha1connect.MachineServiceClient
}

func (w *world) node() *fakeNode {
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", w.socket)
	}}}
	return &fakeNode{
		t:        w.t,
		nodes:    computev1alpha1connect.NewNodeServiceClient(hc, "http://node"),
		machines: computev1alpha1connect.NewMachineServiceClient(hc, "http://node"),
	}
}

// ready reports the node ready, with room for a few machines.
func (n *fakeNode) ready() {
	n.t.Helper()
	if _, err := n.nodes.UpdateNodeStatus(context.Background(), connect.NewRequest(&computev1alpha1.UpdateNodeStatusRequest{
		Name: testNode,
		Status: &computev1alpha1.NodeStatus{
			Capacity: &computev1alpha1.NodeResources{Cpu: "4", Memory: "4Gi"},
			Conditions: []*metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "AgentReporting",
			}},
		},
	})); err != nil {
		n.t.Fatal(err)
	}
}

// placed waits until the scheduler puts the machine on this node and returns
// it.
func (n *fakeNode) placed(name string) *computev1alpha1.Machine {
	n.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := n.machines.GetMachine(context.Background(), connect.NewRequest(&computev1alpha1.GetMachineRequest{Project: "web", Name: name}))
		if err == nil && res.Msg.GetMachine().GetSpec().GetNode() == testNode {
			return res.Msg.GetMachine()
		}
		if time.Now().After(deadline) {
			n.t.Fatalf("machine %s never placed: %v", name, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// report writes a machine's status as the node does: phase, the generation it
// has applied, and conditions, keeping the scheduler's own.
func (n *fakeNode) report(m *computev1alpha1.Machine, phase computev1alpha1.MachinePhase, observed int64, conds ...*metav1.Condition) {
	n.t.Helper()
	status := &computev1alpha1.MachineStatus{Phase: phase, ObservedGeneration: observed}
	for _, c := range m.GetStatus().GetConditions() {
		if c.GetType() == "Scheduled" {
			status.Conditions = append(status.Conditions, c)
		}
	}
	status.Conditions = append(status.Conditions, conds...)
	if _, err := n.machines.UpdateMachineStatus(context.Background(), connect.NewRequest(&computev1alpha1.UpdateMachineStatusRequest{
		Project: "web", Name: m.GetMetadata().GetName(), Status: status,
	})); err != nil {
		n.t.Fatal(err)
	}
}

// released reports that the machine's VM and disks are gone from the host.
func (n *fakeNode) released(name string) {
	n.t.Helper()
	res, err := n.machines.GetMachine(context.Background(), connect.NewRequest(&computev1alpha1.GetMachineRequest{Project: "web", Name: name}))
	if err != nil {
		n.t.Fatal(err)
	}
	n.report(res.Msg.GetMachine(), computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, res.Msg.GetMachine().GetMetadata().GetGeneration(),
		&metav1.Condition{Type: "Released", Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned"})
}

const stack = `apiVersion: tenancy.hangar.dev/v1alpha1
kind: Tenant
metadata:
  name: acme
spec:
  displayName: Acme
---
apiVersion: tenancy.hangar.dev/v1alpha1
kind: Project
metadata:
  name: web
spec:
  tenant: acme
---
apiVersion: compute.hangar.dev/v1alpha1
kind: Container
metadata:
  name: api
  labels:
    tier: front
spec:
  image: nginx:1
  ports:
  - name: http
    port: 80
  resources:
    cpu: 2
    memory: 512Mi
    disk: 20Gi
`

func TestApplyGetDelete(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	file := filepath.Join(t.TempDir(), "stack.yaml")
	if err := os.WriteFile(file, []byte(stack), 0o600); err != nil {
		t.Fatal(err)
	}

	out := w.must("", "apply", "-f", file)
	want := "tenant/acme created\nproject/web created\ncontainer/api created\n"
	if out != want {
		t.Errorf("apply printed %q, want %q", out, want)
	}
	if out := w.must("", "apply", "-f", file, "-o", "name"); out != "tenant/acme\nproject/web\ncontainer/api\n" {
		t.Errorf("second apply -o name printed %q", out)
	}

	var one struct {
		APIVersion string `json:"apiVersion"`
		Metadata   struct {
			Name            string            `json:"name"`
			Project         string            `json:"project"`
			ResourceVersion string            `json:"resourceVersion"`
			Labels          map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Image     string            `json:"image"`
			Resources map[string]string `json:"resources"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(w.must("", "get", "ct", "api", "-o", "json")), &one); err != nil {
		t.Fatal(err)
	}
	if one.APIVersion != "compute.hangar.dev/v1alpha1" || one.Metadata.Name != "api" || one.Metadata.Project != "web" ||
		one.Spec.Image != "nginx:1" || one.Metadata.Labels["tier"] != "front" || one.Metadata.ResourceVersion == "" ||
		one.Spec.Resources["cpu"] != "2" || one.Spec.Resources["memory"] != "512Mi" || one.Spec.Resources["disk"] != "20Gi" {
		t.Errorf("get -o json returned %+v", one)
	}

	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(w.must("", "get", "containers", "-o", "json")), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Errorf("get containers -o json listed %d items", len(list.Items))
	}
	if out := w.must("", "get", "containers", "-l", "tier=back", "-o", "name"); out != "" {
		t.Errorf("a selector matching nothing printed %q", out)
	}
	table := w.must("", "get", "tenants")
	if !strings.HasPrefix(table, "NAME") || !strings.Contains(table, "acme") {
		t.Errorf("table output:\n%s", table)
	}
	if out := w.must("", "get", "Container", "-A", "-o", "table"); !strings.Contains(out, "PROJECT") {
		t.Errorf("-A table has no PROJECT column:\n%s", out)
	}
	y := w.must("", "get", "container", "api", "-o", "yaml")
	if !strings.Contains(y, "kind: Container") || !strings.Contains(y, "image: nginx:1") {
		t.Errorf("get -o yaml:\n%s", y)
	}

	if out := w.must("", "delete", "container", "api"); out != "container/api deleted\n" {
		t.Errorf("delete printed %q", out)
	}
	w.must("", "wait", "container", "api", "--for=delete", "--timeout=10s")
	if r := w.run("", "get", "container", "api"); r.code != cli.ExitNotFound {
		t.Errorf("get after delete: exit %d (%s), want %d", r.code, r.err, cli.ExitNotFound)
	}
}

// TestApplyTakesNumbersAsText: a YAML number where the API has a string is
// sent as the text written, in any string field; numeric fields keep numbers.
func TestApplyTakesNumbersAsText(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	docs := strings.Split(stack, "---\n")
	w.must(docs[0]+"---\n"+docs[1], "apply", "-f", "-")
	w.must(`apiVersion: compute.hangar.dev/v1alpha1
kind: Container
metadata:
  name: api
  labels:
    version: 2
spec:
  image: nginx:1
  args: [1, 1.10, 1e3, "x"]
  env:
  - name: PORT
    value: 8080
  - name: RATIO
    value: 0.50
  ports:
  - name: http
    port: 80
  resources:
    cpu: 2000m
    memory: 1e9
    disk: 20Gi
`, "apply", "-f", "-")

	var got struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Args []string `json:"args"`
			Env  []struct {
				Value string `json:"value"`
			} `json:"env"`
			Ports []struct {
				Port int `json:"port"`
			} `json:"ports"`
			Resources map[string]string `json:"resources"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(w.must("", "get", "ct", "api", "-o", "json")), &got); err != nil {
		t.Fatal(err)
	}
	if got.Metadata.Labels["version"] != "2" {
		t.Errorf("labels %v", got.Metadata.Labels)
	}
	if strings.Join(got.Spec.Args, " ") != "1 1.10 1e3 x" {
		t.Errorf("args %q", got.Spec.Args)
	}
	if len(got.Spec.Env) != 2 || got.Spec.Env[0].Value != "8080" || got.Spec.Env[1].Value != "0.50" {
		t.Errorf("env %+v", got.Spec.Env)
	}
	if len(got.Spec.Ports) != 1 || got.Spec.Ports[0].Port != 80 {
		t.Errorf("ports %+v", got.Spec.Ports)
	}
	if r := got.Spec.Resources; r["cpu"] != "2000m" || r["memory"] != "1e9" || r["disk"] != "20Gi" {
		t.Errorf("resources %v", r)
	}

	// Proto field names work as well as JSON ones.
	w.must("apiVersion: tenancy.hangar.dev/v1alpha1\nkind: Tenant\nmetadata: {name: numbers}\nspec: {display_name: 2024}\n", "apply", "-f", "-")
	if out := w.must("", "get", "tenant", "numbers", "-o", "json"); !strings.Contains(out, `"displayName": "2024"`) {
		t.Errorf("tenant:\n%s", out)
	}

	// A number is not text where the API wants a number.
	bad := "apiVersion: compute.hangar.dev/v1alpha1\nkind: Container\nmetadata: {name: api}\nspec: {image: x, ports: [{name: http, port: eighty}]}\n"
	if r := w.run(bad, "apply", "-f", "-"); r.code != cli.ExitUsage {
		t.Errorf("a word for a port: exit %d (%s)", r.code, r.err)
	}
	// A number anchored outside a string field stays a number through an
	// alias or a merge key, as the help says; quoting the anchor works.
	for _, doc := range []string{
		"apiVersion: compute.hangar.dev/v1alpha1\nkind: Container\nmetadata: {name: api}\nspec:\n  image: x\n  ports: [{name: http, port: &p 80}]\n  args: [*p]\n",
		"apiVersion: compute.hangar.dev/v1alpha1\nkind: Container\nmetadata: {name: api}\nspec:\n  image: x\n  resources:\n    <<: &r {cpu: 2}\n",
	} {
		if r := w.run(doc, "apply", "-f", "-"); r.code != cli.ExitUsage {
			t.Errorf("a number through an alias: exit %d (%s)", r.code, r.err)
		}
	}
	w.must("apiVersion: compute.hangar.dev/v1alpha1\nkind: Container\nmetadata: {name: api}\nspec:\n  image: x\n  resources:\n    <<: &r {cpu: \"2\"}\n", "apply", "-f", "-")
	// The server, not the CLI, says a fraction of a vCPU is refused.
	frac := "apiVersion: compute.hangar.dev/v1alpha1\nkind: Container\nmetadata: {name: api}\nspec: {image: x, resources: {cpu: 1.5}}\n"
	if r := w.run(frac, "apply", "-f", "-"); r.code != cli.ExitError || !strings.Contains(r.err, "vCPUs are whole") {
		t.Errorf("cpu 1.5: exit %d (%s)", r.code, r.err)
	}
}

func TestApplyFromStdinAndDirectory(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	docs := strings.Split(stack, "---\n")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "1-tenant.yaml"), []byte(docs[0]+"---\n"+docs[1]), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a manifest"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := w.must("", "apply", "-f", dir, "-o", "name"); out != "tenant/acme\nproject/web\n" {
		t.Errorf("apply of a directory printed %q", out)
	}
	if out := w.must(docs[2], "apply", "-f", "-", "-o", "name"); out != "container/api\n" {
		t.Errorf("apply from stdin printed %q", out)
	}
}

func TestDeleteFromFiles(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	file := filepath.Join(t.TempDir(), "stack.yaml")
	if err := os.WriteFile(file, []byte(stack), 0o600); err != nil {
		t.Fatal(err)
	}
	w.must("", "apply", "-f", file)
	// Naming a missing object is an error.
	if r := w.run("", "delete", "container", "missing"); r.code != cli.ExitNotFound {
		t.Errorf("deleting a missing container by name: exit %d", r.code)
	}
	// A file is deleted contents first, so nothing it names has already
	// gone with its owner.
	out := w.must("", "delete", "-f", file)
	want := "container/api deleted\nproject/web deleted\ntenant/acme deleted\n"
	if out != want {
		t.Errorf("delete -f printed %q, want %q", out, want)
	}
}

func TestExitCodes(t *testing.T) {
	w := newWorld(t)
	fresh := &world{t: t, config: filepath.Join(t.TempDir(), "config.yaml")}
	if r := fresh.run("", "get", "tenants"); r.code != cli.ExitUsage {
		t.Errorf("no context: exit %d, want usage", r.code)
	}
	w.as("alice@example.com")
	w.must(stack, "apply", "-f", "-")

	tests := []struct {
		name  string
		stdin string
		args  []string
		want  int
	}{
		{"unknown kind", "", []string{"get", "widgets"}, cli.ExitUsage},
		{"unknown flag", "", []string{"get", "tenants", "--bogus"}, cli.ExitUsage},
		{"missing arguments", "", []string{"wait", "container"}, cli.ExitUsage},
		{"bad output", "", []string{"get", "tenants", "-o", "xml"}, cli.ExitUsage},
		{"unknown field in a manifest", "apiVersion: tenancy.hangar.dev/v1alpha1\nkind: Tenant\nmetadata: {name: x}\nspec: {colour: red}\n", []string{"apply", "-f", "-"}, cli.ExitUsage},
		{"unknown kind in a manifest", "apiVersion: v1\nkind: Pod\n", []string{"apply", "-f", "-"}, cli.ExitUsage},
		{"not found", "", []string{"get", "container", "missing"}, cli.ExitNotFound},
		{"stale resource version", "apiVersion: compute.hangar.dev/v1alpha1\nkind: Container\nmetadata: {name: api, resourceVersion: 1}\nspec: {image: x}\n", []string{"apply", "-f", "-"}, cli.ExitConflict},
		{"server rejects", "apiVersion: compute.hangar.dev/v1alpha1\nkind: Container\nmetadata: {name: noimage}\nspec: {}\n", []string{"apply", "-f", "-"}, cli.ExitError},
		{"wait times out", "", []string{"wait", "container", "api", "--for=condition=Ready", "--timeout=200ms"}, cli.ExitTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := w.run(tt.stdin, tt.args...); r.code != tt.want {
				t.Errorf("exit %d (%s), want %d", r.code, strings.TrimSpace(r.err), tt.want)
			}
		})
	}
}

func TestWait(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	w.must(stack, "apply", "-f", "-")

	waitFor := func(args ...string) chan result {
		ch := make(chan result, 1)
		go func() { ch <- w.run("", args...) }()
		time.Sleep(200 * time.Millisecond)
		return ch
	}
	finished := func(ch chan result) result {
		t.Helper()
		select {
		case r := <-ch:
			return r
		case <-time.After(10 * time.Second):
			t.Fatal("wait did not return")
		}
		return result{}
	}
	running := computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING
	node := w.node()

	// The container becomes ready once a node runs its machine.
	ch := waitFor("wait", "container", "api", "--for=condition=Ready", "--timeout=10s")
	node.ready()
	m := node.placed("ct-api")
	node.report(m, running, m.GetMetadata().GetGeneration())
	if r := finished(ch); r.code != 0 {
		t.Fatalf("wait for Ready: exit %d: %s", r.code, r.err)
	}
	if out := w.must("", "get", "machines", "-o", "name"); out != "machine/ct-api\n" {
		t.Errorf("get machines printed %q", out)
	}

	// After a spec change, a machine still running the old one is not enough.
	w.must(strings.Replace(strings.Split(stack, "---\n")[2], "nginx:1", "nginx:2", 1), "apply", "-f", "-")
	ch = waitFor("wait", "container", "api", "--for=condition=Ready", "--timeout=10s")
	before := m.GetMetadata().GetGeneration()
	deadline := time.Now().Add(10 * time.Second)
	for m = node.placed("ct-api"); m.GetMetadata().GetGeneration() == before; m = node.placed("ct-api") {
		if time.Now().After(deadline) {
			t.Fatal("the machine never took the new image")
		}
		time.Sleep(5 * time.Millisecond)
	}
	node.report(m, running, before)
	select {
	case r := <-ch:
		t.Fatalf("wait returned while the machine ran the old spec: exit %d", r.code)
	case <-time.After(300 * time.Millisecond):
	}
	node.report(m, running, m.GetMetadata().GetGeneration())
	if r := finished(ch); r.code != 0 {
		t.Fatalf("wait for Ready after the change: exit %d: %s", r.code, r.err)
	}

	// Deleting waits for the node to release the machine.
	ch = waitFor("wait", "container", "api", "--for=delete", "--timeout=10s")
	w.must("", "delete", "container", "api")
	select {
	case r := <-ch:
		t.Fatalf("the container was gone before its node released the machine: exit %d", r.code)
	case <-time.After(300 * time.Millisecond):
	}
	node.released("ct-api")
	if r := finished(ch); r.code != 0 {
		t.Fatalf("wait for delete: exit %d: %s", r.code, r.err)
	}
}

func TestPermissionDenied(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	w.must(stack, "apply", "-f", "-")
	w.as("bob@example.com")
	r := w.run("", "get", "container", "api")
	if r.code != cli.ExitError || !strings.Contains(r.err, "permission_denied") {
		t.Errorf("bob reading alice's container: exit %d: %s", r.code, r.err)
	}
}

// capture is an endpoint that records request headers and rejects every call.
func capture(t *testing.T) (string, func() []http.Header) {
	var mu sync.Mutex
	var got []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"test"}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []http.Header {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

func TestServiceTokenCredentials(t *testing.T) {
	w := &world{t: t, config: filepath.Join(t.TempDir(), "config.yaml")}
	url, headers := capture(t)
	w.must("", "config", "set-context", "prod", "--endpoint", url)
	env := map[string]string{"HANGAR_CF_CLIENT_ID": "id.access", "HANGAR_CF_CLIENT_SECRET": "s3cret"}
	w.runEnv("", env, "get", "tenants")
	h := headers()
	if len(h) != 1 || h[0].Get("CF-Access-Client-Id") != "id.access" || h[0].Get("CF-Access-Client-Secret") != "s3cret" {
		t.Errorf("service token headers not sent: %v", h)
	}
	if r := w.runEnv("", map[string]string{"HANGAR_CF_CLIENT_ID": "only-id"}, "get", "tenants"); r.code != cli.ExitUsage {
		t.Errorf("half a service token: exit %d, want usage", r.code)
	}
}

func TestCloudflaredCredentials(t *testing.T) {
	url, headers := capture(t)
	config := filepath.Join(t.TempDir(), "config.yaml")
	var calls [][]string
	token := "e30.eyJleHAiOjQxMDI0NDQ4MDB9.sig" // exp in 2100
	run := func(args ...string) result {
		var out, errb bytes.Buffer
		code := cli.Run(context.Background(), args, cli.IO{
			In: strings.NewReader(""), Out: &out, Err: &errb,
			Env: func(k string) string {
				if k == "HANGAR_CONFIG" {
					return config
				}
				return ""
			},
		}, cli.Options{RunCloudflared: func(_ context.Context, a ...string) (string, error) {
			calls = append(calls, a)
			return token + "\n", nil
		}})
		return result{code, out.String(), errb.String()}
	}
	if r := run("config", "set-context", "prod", "--endpoint", url); r.code != 0 {
		t.Fatal(r.err)
	}
	run("login")
	if len(calls) != 1 || strings.Join(calls[0], " ") != "access login "+url {
		t.Errorf("login ran cloudflared %v", calls)
	}
	calls = nil
	run("get", "tenants", "tenant-a", "tenant-b")
	h := headers()
	if len(h) < 1 || h[0].Get("cf-access-token") != token {
		t.Errorf("Access token not sent: %v", h)
	}
	if len(calls) != 1 || strings.Join(calls[0], " ") != "access token -app="+url {
		t.Errorf("cloudflared calls %v, want one token fetch", calls)
	}
}
