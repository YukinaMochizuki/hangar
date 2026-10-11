// Package apiservertest runs a real apiserver for tests outside the
// apiserver package: a SQLite store in a temporary directory, the public API
// in dev auth mode, and the node socket, accepting this process as node
// agent Node.
package apiservertest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
)

const (
	// Node is the Node the agent on the socket acts as.
	Node = "n1"
	// Root holds platform.root.
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
	Store  store.Store
}

// Start runs an apiserver until the test ends. configure may adjust its
// config; the store, the bootstrap admin and the node are already set.
func Start(t *testing.T, configure func(*apiserver.Config)) *Env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hangar.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := apiserver.Config{Store: st, BootstrapAdmin: Root, Nodes: []string{Node}}
	if configure != nil {
		configure(&cfg)
	}
	srv := apiserver.New(cfg)
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
	t.Cleanup(func() {
		public.Close()
		system.Close()
		_ = node.Close()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("apiserver: %v", err)
		}
		_ = st.Close()
		_ = os.RemoveAll(dir)
	})
	return &Env{t: t, URL: public.URL, SystemURL: system.URL, Socket: sock, Store: st}
}

// AsNode returns a client acting as Node.
func (e *Env) AsNode() *client.Client {
	return client.New(client.UnixSocket(e.Socket), client.UnixSocketURL)
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

// Setup creates Tenant and its Project as Root, waiting for the bootstrap
// admin's grant, which is written in the background at start.
func (e *Env) Setup() {
	e.t.Helper()
	root := e.As(Root)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := root.Apply(context.Background(), registry.Tenant, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: Tenant}})
		if err == nil {
			break
		}
		if connect.CodeOf(err) != connect.CodePermissionDenied || time.Now().After(deadline) {
			e.t.Fatalf("creating the tenant: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := root.Apply(context.Background(), registry.Project, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: Project}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: Tenant},
	}); err != nil {
		e.t.Fatalf("creating the project: %v", err)
	}
}
