//go:build !windows

// Local auth is root's on the host, through its admin socket: Linux only.

package cli_test

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/cli"
)

// serveUnix serves h on a unix socket and returns its path.
func serveUnix(t *testing.T, h http.Handler) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "admin.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// runLocal runs hangar with a context whose endpoint is the socket at sock.
func runLocal(t *testing.T, sock string, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	config := filepath.Join(t.TempDir(), "config.yaml")
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := cli.Run(context.Background(), args, cli.IO{In: strings.NewReader(""), Out: &out, Err: &errb, Env: func(k string) string {
			if k == "HANGAR_CONFIG" {
				return config
			}
			return env[k]
		}}, cli.Options{})
		return code, out.String(), errb.String()
	}
	if code, _, e := run("config", "set-context", "local", "--endpoint", "unix://"+sock, "--project", "web", "--use"); code != 0 {
		t.Fatal(e)
	}
	return run(args...)
}

// TestAdminSocket: root on the host gets and applies through the admin
// socket with no binding of its own.
func TestAdminSocket(t *testing.T) {
	w := newWorld(t)
	w.must("", "config", "set-context", "local", "--endpoint", "unix://"+w.admin, "--auth", "local", "--use")
	if out := w.must("", "get", "rolebindings", "-o", "name"); !strings.Contains(out, "rolebinding/bootstrap-root") {
		t.Errorf("get rolebindings as root on the host printed %q", out)
	}
	// What a locked-out administrator does: grant platform.root again.
	w.must(`apiVersion: iam.hangarlab.cloud/v1alpha1
kind: RoleBinding
metadata: {name: root-again}
spec: {subject: {user: carol@example.com}, role: platform.root, scope: {platform: {}}}
`, "apply", "-f", "-")
	// carol had no User yet, so she signs up before the grant counts.
	w.signUp("carol@example.com")
	if out := w.must("", "get", "rolebindings", "-o", "name"); !strings.Contains(out, "rolebinding/root-again") {
		t.Errorf("the binding root made does not grant: %q", out)
	}
}

func TestLocalSendsNoCredentials(t *testing.T) {
	headers := make(chan http.Header, 1)
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		http.Error(w, "no", http.StatusForbidden)
	}))
	runLocal(t, sock, nil, "get", "tenants")
	h := <-headers
	for _, k := range []string{"Authorization", "Cookie", "X-Hangar-Dev-User"} {
		if v := h.Get(k); v != "" {
			t.Errorf("sent %s: %q to the admin socket", k, v)
		}
	}
}

func TestLocalLogs(t *testing.T) {
	f := &fakeLogs{recs: twoBootLogs()}
	mux := http.NewServeMux()
	mux.Handle(computev1alpha1connect.NewMachineCapabilityServiceHandler(fakeCapabilities{c: f, vm: &fakeVMLogs{}}))
	code, out, errOut := runLocal(t, serveUnix(t, mux), nil, "logs", "ct", "api")
	if code != 0 || out != "one\ntwo\nthree\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.req.GetProject() != "web" || f.req.GetKind() != "Container" || f.req.GetName() != "api" || f.user != "" {
		t.Errorf("asked for %v as %q", f.req, f.user)
	}
}

func TestLocalExec(t *testing.T) {
	f := &execServer{}
	code, out, errOut := runLocal(t, serveUnix(t, f), nil, "exec", "ct", "api", "--", "out")
	if code != 7 || out != "to stdout\n" || errOut != "to stderr\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.path != "/v1/projects/web/containers/api:exec" || f.user != "" {
		t.Errorf("at %q as %q", f.path, f.user)
	}
}

func TestLocalContextChecks(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"relative socket", []string{"--endpoint", "unix://run/admin.sock"}, "absolute path"},
		{"unix with oidc", []string{"--endpoint", "unix:///run/hangar/admin.sock", "--auth", "oidc"}, "takes local auth"},
		{"unix with dev", []string{"--endpoint", "unix:///run/hangar/admin.sock", "--auth", "dev", "--user", "a@b.c"}, "takes local auth"},
		{"local over https", []string{"--endpoint", "https://api.example.com", "--auth", "local"}, "needs a unix:// endpoint"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := &world{t: t, config: filepath.Join(t.TempDir(), "config.yaml")}
			r := w.run("", append([]string{"config", "set-context", "x"}, tt.args...)...)
			if r.code != cli.ExitUsage || !strings.Contains(r.err, tt.want) {
				t.Errorf("exit %d: %s; want a usage error saying %q", r.code, r.err, tt.want)
			}
		})
	}
	w := &world{t: t, config: filepath.Join(t.TempDir(), "config.yaml")}
	w.must("", "config", "set-context", "local", "--endpoint", "unix:///run/hangar/admin.sock")
	if r := w.run("", "login"); r.code != cli.ExitUsage || !strings.Contains(r.err, "needs no login") {
		t.Errorf("login on a local context: exit %d: %s", r.code, r.err)
	}
}
