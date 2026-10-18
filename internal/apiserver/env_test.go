package apiserver_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	"github.com/YukinaMochizuki/hangar/internal/controller/controllertest"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
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
	// dbDir holds the store's database files.
	dbDir string
	// onboarded is the tenant, and project, signing up opened for each
	// person, by email.
	onboarded map[string]string
}

type envOption func(*apiserver.Config)

func newEnv(t *testing.T, opts ...envOption) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	dbDir := t.TempDir()
	st, err := sqlite.Open(ctx, filepath.Join(dbDir, "hangar.db"))
	if err != nil {
		t.Fatal(err)
	}
	clock := clockwork.NewFakeClock()
	// The API tests use containers as examples of any kind; the controllers
	// would write to them behind the tests' back. The controllers' own tests
	// run them, and the CLI tests go through them end to end.
	cfg := apiserver.Config{Store: st, Clock: clock, BootstrapAdmin: root, Nodes: []string{testNode}, DisableControllers: true,
		// Tests size machines and open tenants freely; the quota tests set limits.
		DefaultPlanLimits: &tenancyv1alpha1.Limits{}, DefaultMaxOwnedTenants: 1000, Credentials: st}
	for _, o := range opts {
		o(&cfg)
	}
	srv := apiserver.New(cfg)
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	chain := auth.Chain{auth.Dev{}}
	if tokens, ok := srv.ServiceTokens(); ok {
		chain = append(chain, tokens)
	}
	public := httptest.NewServer(srv.Handler(chain))

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
	e := &env{t: t, srv: srv, store: st, clock: clock, url: public.URL, socket: sock, dbDir: dbDir, onboarded: map[string]string{}}
	e.waitBootstrapped()
	e.addMember(alice)
	e.addMember(bob)
	return e
}

// waitSettled waits until the server's background work that follows writes
// (the garbage collector, and controllers when an option enables them) has
// finished reacting to every write so far, and returns the keys still
// waiting on something outside it (see controllertest.WaitSettled).
func (e *env) waitSettled() []controllertest.Waiting {
	e.t.Helper()
	return controllertest.WaitSettled(e.t, e.store.Revision, e.srv.ControllerStatus)
}

// storedVersion reads the revision of the stored object at key, or "absent",
// for testutil.Unchanged.
func (e *env) storedVersion(key store.Key) func() (string, error) {
	return func() (string, error) {
		o, err := e.store.Get(context.Background(), key)
		if errors.Is(err, store.ErrNotFound) {
			return "absent", nil
		}
		return fmt.Sprint(o.Revision), err
	}
}

// bootstrapBinding is the binding a bootstrap admin is granted root by.
var bootstrapBinding = store.Key{Kind: "RoleBinding", Name: "bootstrap-root"}

// addMember makes a person a platform member, as an administrator inviting
// them and they signing up do.
func (e *env) addMember(email string) {
	e.t.Helper()
	e.invite(email, "platform.member")
	e.signUp(email)
}

// invite binds a person a platform role, as the bootstrap admin.
func (e *env) invite(email, role string) {
	e.t.Helper()
	must[iamv1alpha1.ApplyRoleBindingResponse](e.t)(e.as(root).bindings.ApplyRoleBinding(context.Background(),
		binding(role[strings.LastIndexByte(role, '.')+1:]+"-"+usernameOf(email), email, role, platformScope())))
}

// signUp signs an invited person up with the username usernameOf makes from
// their email.
func (e *env) signUp(email string) {
	e.t.Helper()
	res := must[iamv1alpha1.SignUpResponse](e.t)(e.as(email).accounts.SignUp(context.Background(),
		connect.NewRequest(&iamv1alpha1.SignUpRequest{Username: usernameOf(email)})))
	e.onboarded[email] = res.GetTenant()
}

// usernameOf is the username test helpers sign a person up with: their
// email, with '@' and '.' made '-'.
func usernameOf(email string) string {
	return strings.NewReplacer("@", "-", ".", "-").Replace(email)
}

// waitBootstrapped waits until the bootstrap admin's grant is written, which
// happens in the background at start, signs them up, and waits until they
// can act, so tests start from a known state.
func (e *env) waitBootstrapped() {
	e.t.Helper()
	testutil.Eventually(e.t, "bootstrap admin invited", func() (bool, string) {
		me, err := e.as(root).accounts.WhoAmI(context.Background(), connect.NewRequest(&iamv1alpha1.WhoAmIRequest{}))
		return err == nil && me.Msg.GetInvited(), fmt.Sprint(err)
	})
	e.signUp(root)
	rbs := must[iamv1alpha1.ListRoleBindingsResponse](e.t)(e.as(root).bindings.ListRoleBindings(context.Background(),
		connect.NewRequest(&iamv1alpha1.ListRoleBindingsRequest{})))
	if len(rbs.GetItems()) == 0 {
		e.t.Fatal("the bootstrap admin sees no bindings")
	}
}

// clients are typed API clients acting as one caller.
type clients struct {
	tenants    tenancyv1alpha1connect.TenantServiceClient
	projects   tenancyv1alpha1connect.ProjectServiceClient
	plans      tenancyv1alpha1connect.PlanServiceClient
	bindings   iamv1alpha1connect.RoleBindingServiceClient
	users      iamv1alpha1connect.UserServiceClient
	accounts   iamv1alpha1connect.AccountServiceClient
	members    iamv1alpha1connect.MemberServiceClient
	names      tenancyv1alpha1connect.NameServiceClient
	containers computev1alpha1connect.ContainerServiceClient
	machines   computev1alpha1connect.MachineServiceClient
	nodes      computev1alpha1connect.NodeServiceClient
	sizes      computev1alpha1connect.MachineSizeServiceClient
	tokens     iamv1alpha1connect.ServiceTokenServiceClient
	issuer     iamv1alpha1connect.ServiceTokenIssuerServiceClient
	capability computev1alpha1connect.MachineCapabilityServiceClient
}

func newClients(hc connect.HTTPClient, url string, opts ...connect.ClientOption) clients {
	return clients{
		tenants:    tenancyv1alpha1connect.NewTenantServiceClient(hc, url, opts...),
		projects:   tenancyv1alpha1connect.NewProjectServiceClient(hc, url, opts...),
		plans:      tenancyv1alpha1connect.NewPlanServiceClient(hc, url, opts...),
		bindings:   iamv1alpha1connect.NewRoleBindingServiceClient(hc, url, opts...),
		users:      iamv1alpha1connect.NewUserServiceClient(hc, url, opts...),
		accounts:   iamv1alpha1connect.NewAccountServiceClient(hc, url, opts...),
		members:    iamv1alpha1connect.NewMemberServiceClient(hc, url, opts...),
		names:      tenancyv1alpha1connect.NewNameServiceClient(hc, url, opts...),
		containers: computev1alpha1connect.NewContainerServiceClient(hc, url, opts...),
		machines:   computev1alpha1connect.NewMachineServiceClient(hc, url, opts...),
		nodes:      computev1alpha1connect.NewNodeServiceClient(hc, url, opts...),
		sizes:      computev1alpha1connect.NewMachineSizeServiceClient(hc, url, opts...),
		tokens:     iamv1alpha1connect.NewServiceTokenServiceClient(hc, url, opts...),
		issuer:     iamv1alpha1connect.NewServiceTokenIssuerServiceClient(hc, url, opts...),
		capability: computev1alpha1connect.NewMachineCapabilityServiceClient(hc, url, opts...),
	}
}

// withToken returns clients authenticated by a bearer token, such as a
// service token's secret.
func (e *env) withToken(secret string) clients {
	return newClients(http.DefaultClient, e.url, connect.WithInterceptors(headerInterceptor{"Authorization", "Bearer " + secret}))
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
