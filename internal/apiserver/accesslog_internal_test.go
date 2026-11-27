package apiserver

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1/tenancyv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// accessLines are the access lines written to logs so far, each as its
// attributes, for the procedure given.
func accessLines(t *testing.T, logs *syncBuffer, procedure string) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(logs.String()))
	for sc.Scan() {
		var line map[string]any
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("log line %q: %v", sc.Text(), err)
		}
		if line["msg"] == accessLogMessage && line["procedure"] == procedure {
			out = append(out, line)
		}
	}
	return out
}

// wantAccess checks that line holds want, and holds none of the keys absent
// names.
func wantAccess(t *testing.T, what string, line map[string]any, want map[string]any, absent ...string) {
	t.Helper()
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s: %s = %v, want %v (line %v)", what, k, line[k], v, line)
		}
	}
	for _, k := range absent {
		if v, ok := line[k]; ok {
			t.Errorf("%s: has %s = %v, want none (line %v)", what, k, v, line)
		}
	}
	if _, ok := line["duration_ms"].(float64); !ok {
		t.Errorf("%s: duration_ms = %v, want a number", what, line["duration_ms"])
	}
}

// onlyAccess is the one access line for procedure, failing unless there is
// exactly one.
func onlyAccess(t *testing.T, logs *syncBuffer, procedure string) map[string]any {
	t.Helper()
	lines := accessLines(t, logs, procedure)
	if len(lines) != 1 {
		t.Fatalf("%d access lines for %s, want 1: %v", len(lines), procedure, lines)
	}
	return lines[0]
}

// TestAccessLog: every Connect call writes one access line as it ends,
// naming who called, on which project and tenant, and how it ended, calls
// refused for their credentials included.
func TestAccessLog(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	s, _, _ := runningWith(t, func(c *Config) { c.Logger = slog.New(slog.NewJSONHandler(logs, nil)) })
	srv := httptest.NewServer(s.Handler(auth.Chain{auth.Dev{}}))
	t.Cleanup(srv.Close)
	const alice = "alice@example.com"
	as := func(req interface{ Header() http.Header }, email string) {
		if email != "" {
			req.Header().Set(auth.DevUserHeader, email)
		}
	}
	machines := computev1alpha1connect.NewMachineServiceClient(http.DefaultClient, srv.URL)

	t.Run("unary", func(t *testing.T) {
		get := connect.NewRequest(&computev1alpha1.GetMachineRequest{Project: "web", Name: "missing"})
		as(get, alice)
		_, err := machines.GetMachine(t.Context(), get)
		wantCodeOf(t, err, connect.CodeNotFound, "reading a missing machine")
		wantAccess(t, "a call that failed", onlyAccess(t, logs, computev1alpha1connect.MachineServiceGetMachineProcedure), map[string]any{
			"code": "not_found", "principal": "user:" + alice, "principal_type": "user",
			"project": "web", "tenant": "acme", "stream": false,
		})
	})

	t.Run("the project of the object applied", func(t *testing.T) {
		apply := connect.NewRequest(&computev1alpha1.ApplyMachineRequest{
			Machine: &computev1alpha1.Machine{Metadata: &metav1.ObjectMeta{Project: "web", Name: "m"}},
		})
		as(apply, alice)
		_, _ = machines.ApplyMachine(t.Context(), apply)
		wantAccess(t, "an apply", onlyAccess(t, logs, computev1alpha1connect.MachineServiceApplyMachineProcedure), map[string]any{
			"principal": "user:" + alice, "project": "web", "tenant": "acme",
		})
	})

	t.Run("refused for its credentials", func(t *testing.T) {
		list := connect.NewRequest(&computev1alpha1.ListMachinesRequest{Project: "web"})
		_, err := machines.ListMachines(t.Context(), list)
		wantCodeOf(t, err, connect.CodeUnauthenticated, "listing without credentials")
		// No tenant: a caller nobody authenticated never has a project
		// looked up.
		wantAccess(t, "a refused call", onlyAccess(t, logs, computev1alpha1connect.MachineServiceListMachinesProcedure), map[string]any{
			"code": "unauthenticated", "project": "web",
		}, "principal", "principal_type", "tenant")
	})

	t.Run("no project", func(t *testing.T) {
		tenants := tenancyv1alpha1connect.NewTenantServiceClient(http.DefaultClient, srv.URL)
		list := connect.NewRequest(&tenancyv1alpha1.ListTenantsRequest{})
		as(list, alice)
		if _, err := tenants.ListTenants(t.Context(), list); err != nil {
			t.Fatal(err)
		}
		wantAccess(t, "a call on no project", onlyAccess(t, logs, tenancyv1alpha1connect.TenantServiceListTenantsProcedure), map[string]any{
			"code": "ok", "principal": "user:" + alice,
		}, "project", "tenant")
	})

	t.Run("stream", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		watch := connect.NewRequest(&computev1alpha1.WatchMachinesRequest{Project: "web"})
		as(watch, alice)
		stream, err := machines.WatchMachines(ctx, watch)
		if err != nil {
			t.Fatal(err)
		}
		// The line is written as the stream ends, and only then.
		stream.Receive()
		if lines := accessLines(t, logs, computev1alpha1connect.MachineServiceWatchMachinesProcedure); len(lines) != 0 {
			t.Errorf("access lines of a stream still open: %v", lines)
		}
		cancel()
		_ = stream.Close()
		testutil.Eventually(t, "the stream's access line", func() bool {
			return len(accessLines(t, logs, computev1alpha1connect.MachineServiceWatchMachinesProcedure)) > 0
		})
		wantAccess(t, "a stream", onlyAccess(t, logs, computev1alpha1connect.MachineServiceWatchMachinesProcedure), map[string]any{
			"principal": "user:" + alice, "project": "web", "tenant": "acme", "stream": true,
		})
	})
}

// TestAccessLogNamesProjectsAndTenants: calls on projects and tenants
// themselves name them by their objects' names, a project's call its
// tenant too.
func TestAccessLogNamesProjectsAndTenants(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	s, _, _ := runningWith(t, func(c *Config) { c.Logger = slog.New(slog.NewJSONHandler(logs, nil)) })
	srv := httptest.NewServer(s.Handler(auth.Chain{auth.Dev{}}))
	t.Cleanup(srv.Close)
	const alice = "alice@example.com"

	projects := tenancyv1alpha1connect.NewProjectServiceClient(http.DefaultClient, srv.URL)
	get := connect.NewRequest(&tenancyv1alpha1.GetProjectRequest{Name: "web"})
	get.Header().Set(auth.DevUserHeader, alice)
	if _, err := projects.GetProject(t.Context(), get); err != nil {
		t.Fatal(err)
	}
	wantAccess(t, "reading a project", onlyAccess(t, logs, tenancyv1alpha1connect.ProjectServiceGetProjectProcedure), map[string]any{
		"project": "web", "tenant": "acme",
	})
	apply := connect.NewRequest(&tenancyv1alpha1.ApplyProjectRequest{Project: &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: "web"}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "acme"},
	}})
	apply.Header().Set(auth.DevUserHeader, alice)
	_, _ = projects.ApplyProject(t.Context(), apply)
	wantAccess(t, "applying a project", onlyAccess(t, logs, tenancyv1alpha1connect.ProjectServiceApplyProjectProcedure), map[string]any{
		"project": "web", "tenant": "acme",
	})

	tenants := tenancyv1alpha1connect.NewTenantServiceClient(http.DefaultClient, srv.URL)
	getT := connect.NewRequest(&tenancyv1alpha1.GetTenantRequest{Name: "acme"})
	getT.Header().Set(auth.DevUserHeader, alice)
	if _, err := tenants.GetTenant(t.Context(), getT); err != nil {
		t.Fatal(err)
	}
	wantAccess(t, "reading a tenant", onlyAccess(t, logs, tenancyv1alpha1connect.TenantServiceGetTenantProcedure), map[string]any{
		"tenant": "acme",
	}, "project")
}

func TestAccessLogBoundsTheProject(t *testing.T) {
	t.Parallel()
	c := &accessCall{}
	c.noteRequest(&computev1alpha1.GetMachineRequest{Project: strings.Repeat("p", 1000)})
	if got := *c.project.Load(); len(got) != maxLoggedName {
		t.Errorf("logged a project of %d bytes, want %d", len(got), maxLoggedName)
	}
}
