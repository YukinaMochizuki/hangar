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
	srv := apiserver.New(apiserver.Config{Store: st, BootstrapAdmin: "root@example.com"})
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	public := httptest.NewServer(srv.Handler(auth.Chain{auth.Dev{}}))
	sock := filepath.Join(t.TempDir(), "node.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	node := &http.Server{
		Handler:     srv.Handler(auth.Chain{auth.PeerCred{NodeUIDs: []uint32{uint32(os.Getuid())}}}),
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

// setStatus writes a container's status as the node does.
func (w *world) setStatus(name string, status *computev1alpha1.ContainerStatus) {
	w.t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", w.socket)
	}}}
	c := computev1alpha1connect.NewContainerServiceClient(hc, "http://node")
	if _, err := c.UpdateContainerStatus(context.Background(), connect.NewRequest(&computev1alpha1.UpdateContainerStatusRequest{
		Project: "web", Name: name, Status: status,
	})); err != nil {
		w.t.Fatal(err)
	}
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
			Image string `json:"image"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(w.must("", "get", "ct", "api", "-o", "json")), &one); err != nil {
		t.Fatal(err)
	}
	if one.APIVersion != "compute.hangar.dev/v1alpha1" || one.Metadata.Name != "api" || one.Metadata.Project != "web" ||
		one.Spec.Image != "nginx:1" || one.Metadata.Labels["tier"] != "front" || one.Metadata.ResourceVersion == "" {
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
	if r := w.run("", "get", "container", "api"); r.code != cli.ExitNotFound {
		t.Errorf("get after delete: exit %d (%s), want %d", r.code, r.err, cli.ExitNotFound)
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
	ready := func(gen int64) *computev1alpha1.ContainerStatus {
		return &computev1alpha1.ContainerStatus{
			ObservedGeneration: gen,
			Conditions: []*metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, ObservedGeneration: gen,
			}},
		}
	}

	ch := waitFor("wait", "container", "api", "--for=condition=Ready", "--timeout=10s")
	w.setStatus("api", ready(1))
	if r := finished(ch); r.code != 0 {
		t.Fatalf("wait for Ready: exit %d: %s", r.code, r.err)
	}

	// After a spec change, Ready for the old generation is not enough.
	w.must(strings.Replace(strings.Split(stack, "---\n")[2], "nginx:1", "nginx:2", 1), "apply", "-f", "-")
	ch = waitFor("wait", "container", "api", "--for=condition=Ready", "--timeout=10s")
	w.setStatus("api", ready(1))
	select {
	case r := <-ch:
		t.Fatalf("wait returned on a stale condition: exit %d", r.code)
	case <-time.After(300 * time.Millisecond):
	}
	w.setStatus("api", ready(2))
	if r := finished(ch); r.code != 0 {
		t.Fatalf("wait for Ready at generation 2: exit %d: %s", r.code, r.err)
	}

	ch = waitFor("wait", "container", "api", "--for=delete", "--timeout=10s")
	w.must("", "delete", "container", "api")
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
