// Package apiservertest runs a real apiserver for tests outside the
// apiserver package: a SQLite store in a temporary directory, the public API
// in dev auth mode, the node socket, accepting this process as node agent
// Node, and the edge socket, accepting it as hangar-edge. StartOIDC also
// signs people in through an OpenID Connect provider, such as oidctest's.
package apiservertest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/controller/controllertest"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

const (
	// Node is the Node the agent on the socket acts as.
	Node = "n1"
	// Root holds platform.root, and has signed up once Start returns.
	Root = "root@example.com"
	// Tenant and Project are what Setup creates.
	Tenant  = "acme"
	Project = "web"
)

// Env is a running apiserver.
type Env struct {
	t *testing.T
	// URL is the public API, which trusts auth.DevUserHeader.
	URL string
	// SystemURL serves every caller as the System principal, as the
	// apiserver's own controllers act; tests use it to write what only the
	// system may, such as a Machine.
	SystemURL string
	// Socket is the node socket.
	Socket string
	// EdgeSocket is the edge socket, where this process is hangar-edge.
	EdgeSocket string
	Store      store.Store
	// Credentials is the store's credentials, which the apiserver keeps
	// sessions and tokens in.
	Credentials store.Credentials
	// Public is the public listener's handler with sign-in, for StartOIDC;
	// nil for Start.
	Public http.Handler
	srv    *apiserver.Server
}

// WaitSettled waits until the apiserver's controllers, and those others
// reports (a node agent's), have finished reacting to every write so far,
// and returns the keys still waiting on something outside them (see
// controllertest.WaitSettled). Controllers following the API see how far
// they have come by its bookmarks, which Start sends often unless
// configure says otherwise.
func (e *Env) WaitSettled(others ...controllertest.Statuses) []controllertest.Waiting {
	e.t.Helper()
	return controllertest.WaitSettled(e.t, e.Store.Revision, append([]controllertest.Statuses{e.srv.ControllerStatus}, others...)...)
}

// Start runs an apiserver until the test ends, with Root signed up.
// configure may adjust its config; the store, the bootstrap admin and the
// node are already set.
func Start(t *testing.T, configure func(*apiserver.Config)) *Env {
	t.Helper()
	return start(t, configure, nil)
}

// StartOIDC is Start with sign-in through the provider oidc names: Env.Public
// serves it, and the console's endpoint-login, for a test to put behind the
// console's origin, oidc.PublicURL.
func StartOIDC(t *testing.T, configure func(*apiserver.Config), oidc apiserver.OIDCConfig) *Env {
	t.Helper()
	return start(t, configure, &oidc)
}

func start(t *testing.T, configure func(*apiserver.Config), oidc *apiserver.OIDCConfig) *Env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hangar.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Tests size machines and open tenants freely, unless configure sets
	// limits.
	cfg := apiserver.Config{Store: st, BootstrapAdmin: Root, Nodes: []string{Node},
		DefaultPlanLimits: &tenancyv1alpha1.Limits{}, DefaultMaxOwnedTenants: 1000, Credentials: st,
		BookmarkInterval: 10 * time.Millisecond}
	if configure != nil {
		configure(&cfg)
	}
	srv := apiserver.New(cfg)
	var publicHandler http.Handler
	if oidc != nil {
		chain, err := srv.EnableOIDC(*oidc, st)
		if err != nil {
			t.Fatal(err)
		}
		publicHandler = srv.PublicHandler(chain)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	public := httptest.NewServer(srv.Handler(auth.Chain{auth.Dev{}}))
	system := httptest.NewServer(srv.Handler(auth.Chain{asSystem{}}))
	// Unix socket paths are short; t.TempDir can be too long for one.
	dir, err := os.MkdirTemp("", "hangar")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "node.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	node := &http.Server{
		Handler:     srv.Handler(auth.Chain{auth.PeerCred{NodeUIDs: []uint32{uint32(os.Getuid())}, NodeName: Node}}),
		ConnContext: auth.ConnContext,
	}
	go func() { _ = node.Serve(ln) }()
	edgeSock := filepath.Join(dir, "edge.sock")
	eln, err := net.Listen("unix", edgeSock)
	if err != nil {
		t.Fatal(err)
	}
	edge := &http.Server{
		Handler:     srv.EdgeHandler(auth.Chain{auth.PeerEdge{UIDs: []uint32{uint32(os.Getuid())}, Host: Node}}),
		ConnContext: auth.EdgeConnContext,
	}
	go func() { _ = edge.Serve(eln) }()
	t.Cleanup(func() {
		public.Close()
		system.Close()
		_ = node.Close()
		_ = edge.Close()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("apiserver: %v", err)
		}
		_ = st.Close()
		_ = os.RemoveAll(dir)
	})
	e := &Env{t: t, URL: public.URL, SystemURL: system.URL, Socket: sock, EdgeSocket: edgeSock, Store: st, Credentials: st, Public: publicHandler, srv: srv}
	e.waitInvited(Root)
	e.SignUp(Root)
	return e
}

// waitInvited waits until the person holds a platform role, as the bootstrap
// admin does once its grant, written in the background at start, is in.
func (e *Env) waitInvited(email string) {
	e.t.Helper()
	accounts := iamv1alpha1connect.NewAccountServiceClient(DevUser(email), e.URL)
	testutil.Eventually(e.t, email+" invited", func() (bool, string) {
		me, err := accounts.WhoAmI(context.Background(), connect.NewRequest(&iamv1alpha1.WhoAmIRequest{}))
		return err == nil && me.Msg.GetInvited(), fmt.Sprint(err)
	})
}

// AddMember makes a person a platform member with the platform role, as
// Root inviting them and they signing up do.
func (e *Env) AddMember(email, role string) {
	e.t.Helper()
	e.Invite(email, role)
	e.SignUp(email)
}

// Invite binds a person the platform role, as Root.
func (e *Env) Invite(email, role string) {
	e.t.Helper()
	if _, err := e.As(Root).Apply(context.Background(), registry.RoleBinding, &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: role[strings.LastIndexByte(role, '.')+1:] + "-" + Username(email)},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: email}},
			Role:    role,
			Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{Platform: &emptypb.Empty{}}},
		},
	}); err != nil {
		e.t.Fatalf("inviting %s: %v", email, err)
	}
}

// SignUp signs an invited person up, as Username(email).
func (e *Env) SignUp(email string) {
	e.t.Helper()
	accounts := iamv1alpha1connect.NewAccountServiceClient(DevUser(email), e.URL)
	if _, err := accounts.SignUp(context.Background(), connect.NewRequest(&iamv1alpha1.SignUpRequest{Username: Username(email)})); err != nil {
		e.t.Fatalf("signing %s up: %v", email, err)
	}
}

// Username is the username the helpers sign a person up with: their email,
// with '@' and '.' made '-'.
func Username(email string) string {
	return strings.NewReplacer("@", "-", ".", "-").Replace(email)
}

// AsNode returns a client acting as Node.
func (e *Env) AsNode() *client.Client {
	return client.New(client.UnixSocket(e.Socket), client.UnixSocketURL)
}

// AsEdge returns a client acting as hangar-edge.
func (e *Env) AsEdge() *client.Client {
	return client.New(client.UnixSocket(e.EdgeSocket), client.UnixSocketURL)
}

// AsSystem returns a client acting as the System principal.
func (e *Env) AsSystem() *client.Client {
	return client.New(http.DefaultClient, e.SystemURL)
}

type asSystem struct{}

func (asSystem) Authenticate(context.Context, http.Header) (auth.Principal, bool, error) {
	return auth.SystemPrincipal, true, nil
}

// As returns a client acting as the person with the email.
func (e *Env) As(email string, opts ...connect.ClientOption) *client.Client {
	return client.New(DevUser(email), e.URL, opts...)
}

// DevUser is an HTTP client that calls the public API as the person with
// the email.
func DevUser(email string) *http.Client {
	return &http.Client{Transport: devUser{email}}
}

type devUser struct{ email string }

func (d devUser) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(auth.DevUserHeader, d.email)
	return http.DefaultTransport.RoundTrip(r)
}

// Setup creates Tenant and its Project as Root.
func (e *Env) Setup() {
	e.t.Helper()
	root := e.As(Root)
	if _, err := root.Apply(context.Background(), registry.Tenant, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: Tenant}}); err != nil {
		e.t.Fatalf("creating the tenant: %v", err)
	}
	if _, err := root.Apply(context.Background(), registry.Project, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: Project}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: Tenant},
	}); err != nil {
		e.t.Fatalf("creating the project: %v", err)
	}
}
