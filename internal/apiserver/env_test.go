package apiserver_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1/tenancyv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
)

const (
	root = "root@example.com"
	// testNode is the Node the agent on the test socket acts as.
	testNode = "n1"
)

// env is a running apiserver over a real SQLite store, reached over HTTP
// exactly as clients reach it: a dev-auth listener for users and a unix
// socket for the node.
type env struct {
	t      *testing.T
	srv    *apiserver.Server
	store  store.Store
	clock  *clockwork.FakeClock
	url    string
	socket string
}

type envOption func(*apiserver.Config)

func newEnv(t *testing.T, opts ...envOption) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hangar.db"))
	if err != nil {
		t.Fatal(err)
	}
	clock := clockwork.NewFakeClock()
	// The API tests use containers as examples of any kind; the controllers
	// would write to them behind the tests' back. The controllers' own tests
	// run them, and the CLI tests go through them end to end.
	cfg := apiserver.Config{Store: st, Clock: clock, BootstrapAdmin: root, Nodes: []string{testNode}, DisableControllers: true}
	for _, o := range opts {
		o(&cfg)
	}
	srv := apiserver.New(cfg)
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
		if err := <-done; err != nil {
			t.Errorf("server Run: %v", err)
		}
		_ = st.Close()
	})
	e := &env{t: t, srv: srv, store: st, clock: clock, url: public.URL, socket: sock}
	e.waitBootstrapped()
	e.addMember(alice)
	e.addMember(bob)
	return e
}

// addMember makes a person a platform member, as an administrator does.
func (e *env) addMember(email string) {
	e.t.Helper()
	must[iamv1alpha1.ApplyRoleBindingResponse](e.t)(e.as(root).bindings.ApplyRoleBinding(context.Background(),
		binding("member-"+strings.NewReplacer("@", "-", ".", "-").Replace(email), email, "platform.member", platformScope())))
}

// waitBootstrapped waits until the bootstrap admin can act, so tests start
// from a known state.
func (e *env) waitBootstrapped() {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := e.as(root).tenants.ListTenants(context.Background(), connect.NewRequest(&tenancyv1alpha1.ListTenantsRequest{}))
		if err == nil {
			rbs, err := e.as(root).bindings.ListRoleBindings(context.Background(), connect.NewRequest(&iamv1alpha1.ListRoleBindingsRequest{}))
			if err == nil && len(rbs.Msg.GetItems()) > 0 {
				return
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatal("bootstrap admin never became usable")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// clients are typed API clients acting as one caller.
type clients struct {
	tenants    tenancyv1alpha1connect.TenantServiceClient
	projects   tenancyv1alpha1connect.ProjectServiceClient
	bindings   iamv1alpha1connect.RoleBindingServiceClient
	containers computev1alpha1connect.ContainerServiceClient
	machines   computev1alpha1connect.MachineServiceClient
	nodes      computev1alpha1connect.NodeServiceClient
}

func newClients(hc connect.HTTPClient, url string, opts ...connect.ClientOption) clients {
	return clients{
		tenants:    tenancyv1alpha1connect.NewTenantServiceClient(hc, url, opts...),
		projects:   tenancyv1alpha1connect.NewProjectServiceClient(hc, url, opts...),
		bindings:   iamv1alpha1connect.NewRoleBindingServiceClient(hc, url, opts...),
		containers: computev1alpha1connect.NewContainerServiceClient(hc, url, opts...),
		machines:   computev1alpha1connect.NewMachineServiceClient(hc, url, opts...),
		nodes:      computev1alpha1connect.NewNodeServiceClient(hc, url, opts...),
	}
}

// as returns clients authenticated as a user through dev auth; an empty email
// sends no credentials.
func (e *env) as(email string) clients {
	return newClients(http.DefaultClient, e.url, connect.WithInterceptors(headerInterceptor{auth.DevUserHeader, email}))
}

// asNode returns clients that reach the apiserver over the node socket.
func (e *env) asNode() clients {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", e.socket)
		},
	}}
	return newClients(hc, "http://node")
}

// headerInterceptor sets a request header on unary and streaming calls.
type headerInterceptor struct{ name, value string }

func (h headerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if h.value != "" {
			req.Header().Set(h.name, h.value)
		}
		return next(ctx, req)
	}
}

func (h headerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if h.value != "" {
			conn.RequestHeader().Set(h.name, h.value)
		}
		return conn
	}
}

func (h headerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func must[T any](t *testing.T) func(*connect.Response[T], error) *T {
	return func(r *connect.Response[T], err error) *T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg
	}
}

func wantCode(t *testing.T, err error, code connect.Code, what string) {
	t.Helper()
	if connect.CodeOf(err) != code {
		t.Errorf("%s: got %v, want %s", what, err, code)
	}
}

func tenant(name string) *tenancyv1alpha1.ApplyTenantRequest {
	return &tenancyv1alpha1.ApplyTenantRequest{Tenant: &tenancyv1alpha1.Tenant{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec:     &tenancyv1alpha1.TenantSpec{DisplayName: name},
	}}
}

func project(name, tenant string) *tenancyv1alpha1.ApplyProjectRequest {
	return &tenancyv1alpha1.ApplyProjectRequest{Project: &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec:     &tenancyv1alpha1.ProjectSpec{Tenant: tenant},
	}}
}

func container(project, name, image string) *computev1alpha1.Container {
	return &computev1alpha1.Container{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec:     &computev1alpha1.ContainerSpec{Image: image},
	}
}

func applyContainer(c *computev1alpha1.Container) *connect.Request[computev1alpha1.ApplyContainerRequest] {
	return connect.NewRequest(&computev1alpha1.ApplyContainerRequest{Container: c})
}

func getContainer(project, name string) *connect.Request[computev1alpha1.GetContainerRequest] {
	return connect.NewRequest(&computev1alpha1.GetContainerRequest{Project: project, Name: name})
}

// setupTenant has owner create a tenant with one project.
func (e *env) setupTenant(owner, tenantName, projectName string) {
	e.t.Helper()
	ctx := context.Background()
	c := e.as(owner)
	must[tenancyv1alpha1.ApplyTenantResponse](e.t)(c.tenants.ApplyTenant(ctx, connect.NewRequest(tenant(tenantName))))
	must[tenancyv1alpha1.ApplyProjectResponse](e.t)(c.projects.ApplyProject(ctx, connect.NewRequest(project(projectName, tenantName))))
}
