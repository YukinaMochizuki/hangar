package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/oidctest"
	"github.com/YukinaMochizuki/hangar/internal/client"
)

// syncBuffer is a log the apiserver writes while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type listeners struct {
	public, node, admin, publicSocket string
	log                               *syncBuffer
}

// start runs the apiserver as main does, with dev auth on the public
// listener, the node socket and the admin socket, where uid is root. opts
// change the flags before it starts.
func start(t *testing.T, uid uint32, opts ...func(*flags)) listeners {
	t.Helper()
	old := adminUID
	adminUID = uid
	t.Cleanup(func() { adminUID = old })
	dir := t.TempDir()
	port, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := port.Addr().String()
	_ = port.Close()
	l := listeners{
		public: "http://" + addr, node: filepath.Join(dir, "node.sock"), admin: filepath.Join(dir, "admin.sock"),
		publicSocket: filepath.Join(dir, "public.sock"), log: &syncBuffer{},
	}
	f := flags{
		db: filepath.Join(dir, "hangar.db"), listen: addr, socket: l.node, adminSocket: l.admin,
		nodeUID: uint(os.Getuid()), nodeName: "n1", authMode: "dev", bootstrapAdmin: "root@example.com",
		maxOwnedTenants: 3,
	}
	for _, o := range opts {
		o(&f)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, f, slog.New(slog.NewTextHandler(l.log, nil))) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(l.admin); err == nil {
			if c, err := net.Dial("tcp", addr); err == nil {
				_ = c.Close()
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the apiserver did not start: %s", l.log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return l
}

func bindings(hc connect.HTTPClient, base string, header http.Header) iamv1alpha1connect.RoleBindingServiceClient {
	return iamv1alpha1connect.NewRoleBindingServiceClient(hc, base, connect.WithInterceptors(connect.UnaryInterceptorFunc(
		func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				for k, v := range header {
					req.Header()[k] = v
				}
				return next(ctx, req)
			}
		})))
}

// grant asks for platform.root for mallory, the first thing someone holding
// it would do with it.
func grant(c iamv1alpha1connect.RoleBindingServiceClient) error {
	_, err := c.ApplyRoleBinding(context.Background(), connect.NewRequest(&iamv1alpha1.ApplyRoleBindingRequest{
		RoleBinding: &iamv1alpha1.RoleBinding{
			Metadata: &metav1.ObjectMeta{Name: "mallory-root"},
			Spec: &iamv1alpha1.RoleBindingSpec{
				Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: "mallory@example.com"}},
				Role:    "platform.root",
				Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}},
			},
		},
	}))
	return err
}

func TestAdminSocketIsRootOnTheHost(t *testing.T) {
	l := start(t, uint32(os.Getuid()))
	if info, err := os.Stat(l.admin); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("admin socket %v, %v; want mode 0600", info.Mode(), err)
	}
	admin := client.UnixSocket(l.admin)
	if err := grant(bindings(admin, client.UnixSocketURL, nil)); err != nil {
		t.Fatalf("root on the host, with no binding of its own: %v", err)
	}
	// Root on the host may read logs, and finds nothing to read.
	logs := computev1alpha1connect.NewMachineCapabilityServiceClient(admin, client.UnixSocketURL)
	stream, err := logs.Logs(context.Background(), connect.NewRequest(&computev1alpha1.LogsRequest{Project: "web", Kind: "Container", Name: "api"}))
	if err == nil {
		for stream.Receive() {
		}
		err = stream.Err()
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("logs of a container that is not there: %v; want not found", err)
	}
	if got := l.log.String(); !strings.Contains(got, `msg="admin socket request" method=POST path=/hangar.iam.v1alpha1.RoleBindingService/ApplyRoleBinding uid=`) ||
		!strings.Contains(got, "principal=local:root@n1") {
		t.Errorf("the request is not in the log as root on the host:\n%s", got)
	}
}

func TestAdminSocketRefusesOtherUIDs(t *testing.T) {
	l := start(t, uint32(os.Getuid())+1)
	err := grant(bindings(client.UnixSocket(l.admin), client.UnixSocketURL, nil))
	if connect.CodeOf(err) != connect.CodeUnauthenticated || !strings.Contains(err.Error(), "is not root") {
		t.Fatalf("a uid that is not root: %v; want unauthenticated", err)
	}
	if got := l.log.String(); !strings.Contains(got, `msg="admin socket request refused"`) || !strings.Contains(got, fmt.Sprintf("uid=%d", os.Getuid())) {
		t.Errorf("the refusal is not in the log with its uid:\n%s", got)
	}
}

// TestNoOtherListenerIsRoot: whatever a caller sends, the public listener and
// the node socket never act as root on the host, even for the uid the admin
// socket accepts.
func TestNoOtherListenerIsRoot(t *testing.T) {
	l := start(t, uint32(os.Getuid()))
	for _, tt := range []struct {
		name   string
		hc     connect.HTTPClient
		base   string
		header http.Header
	}{
		{"public, no credentials", http.DefaultClient, l.public, nil},
		{"public, as the principal's name", http.DefaultClient, l.public, http.Header{auth.DevUserHeader: {"local:root@n1"}}},
		{"public, as root@n1", http.DefaultClient, l.public, http.Header{auth.DevUserHeader: {"root@n1"}}},
		{"node socket", client.UnixSocket(l.node), client.UnixSocketURL, nil},
		{"node socket, with a dev header", client.UnixSocket(l.node), client.UnixSocketURL, http.Header{auth.DevUserHeader: {"root@example.com"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := grant(bindings(tt.hc, tt.base, tt.header))
			if code := connect.CodeOf(err); code != connect.CodeUnauthenticated && code != connect.CodePermissionDenied {
				t.Fatalf("granted platform.root: %v", err)
			}
		})
	}
	if strings.Contains(l.log.String(), "local:") {
		t.Errorf("a principal of type local outside the admin socket:\n%s", l.log.String())
	}
}

// TestPublicSocketIsTheEdgesOnly: the socket hangar-edge forwards the
// public API to is the public listener for the edge's uid alone, sign-in
// included, which must say each client's address; it never runs in dev
// auth.
func TestPublicSocketIsTheEdgesOnly(t *testing.T) {
	p := oidctest.New(t)
	secret := filepath.Join(t.TempDir(), "oidc-client-secret")
	if err := os.WriteFile(secret, []byte(p.ClientSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	const console = "console.cp.test"
	oidc := func(edgeUID uint) func(*flags) {
		return func(f *flags) {
			f.authMode, f.oidcIssuer, f.oidcClientID, f.oidcSecretFile = "oidc", p.URL, p.ClientID, secret
			f.publicURL = "https://" + console
			f.publicSocket, f.edgeUID = filepath.Join(filepath.Dir(f.db), "public.sock"), edgeUID
		}
	}
	// call sends a request to the public socket, as the edge does, for
	// the console's host, and returns its status and Location.
	call := func(path, method, target string, h http.Header) (int, string) {
		var body io.Reader
		if method == http.MethodPost {
			body = strings.NewReader("{}")
		}
		req, err := http.NewRequest(method, client.UnixSocketURL+target, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = console
		if h != nil {
			req.Header = h.Clone()
		}
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
		}
		c := client.UnixSocket(path)
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, resp.Header.Get("Location")
	}
	const whoami = "/hangar.iam.v1alpha1.AccountService/WhoAmI"
	t.Run("the edge, with an address", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("the edge's uid is never root, and this test's peer is; internal/auth's tests cover the check")
		}
		l := start(t, uint32(os.Getuid()), oidc(uint(os.Getuid())))
		if info, err := os.Stat(l.publicSocket); err != nil || info.Mode().Perm() != 0o660 {
			t.Errorf("public socket %v, %v; want mode 0660", info.Mode(), err)
		}
		h := http.Header{auth.ClientIPHeader: {"2001:db8::7"}}
		// Sign-in is served here, as on the public listener: it sends the
		// browser on to the provider.
		if code, location := call(l.publicSocket, http.MethodGet, "/auth/login", h); code != http.StatusFound || !strings.HasPrefix(location, p.URL) {
			t.Errorf("sign-in through the edge: %d to %q; want 302 to the provider", code, location)
		}
		// The edge vouches for the address, never for the caller.
		if code, _ := call(l.publicSocket, http.MethodPost, whoami, h); code != http.StatusUnauthorized {
			t.Errorf("no credential through the edge: %d; want 401", code)
		}
		if code, _ := call(l.publicSocket, http.MethodPost, whoami, nil); code != http.StatusBadRequest {
			t.Errorf("no client address: %d; want 400", code)
		}
	})
	t.Run("another uid", func(t *testing.T) {
		l := start(t, uint32(os.Getuid()), oidc(uint(os.Getuid())+1))
		for _, target := range []string{whoami, "/auth/login"} {
			method := http.MethodPost
			if target == "/auth/login" {
				method = http.MethodGet
			}
			if code, _ := call(l.publicSocket, method, target, http.Header{auth.ClientIPHeader: {"192.0.2.1"}}); code != http.StatusForbidden {
				t.Errorf("%s from a uid that is not the edge's: %d; want 403", target, code)
			}
		}
	})
	t.Run("never in dev auth", func(t *testing.T) {
		dir := t.TempDir()
		f := flags{
			db: filepath.Join(dir, "hangar.db"), listen: "127.0.0.1:0", authMode: "dev",
			publicSocket: filepath.Join(dir, "public.sock"), edgeUID: 1000, maxOwnedTenants: 3,
		}
		err := run(context.Background(), f, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err == nil || !strings.Contains(err.Error(), "never serves --public-socket") {
			t.Errorf("dev auth with a public socket: %v; want refused", err)
		}
	})
}

func TestDevRefusesTunnelledRequests(t *testing.T) {
	l := start(t, uint32(os.Getuid()))
	// WhoAmI answers whoever authenticates, member or not.
	whoami := func(h http.Header) error {
		_, err := iamv1alpha1connect.NewAccountServiceClient(http.DefaultClient, l.public, connect.WithInterceptors(connect.UnaryInterceptorFunc(
			func(next connect.UnaryFunc) connect.UnaryFunc {
				return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
					for k, v := range h {
						req.Header()[k] = v
					}
					return next(ctx, req)
				}
			}))).WhoAmI(context.Background(), connect.NewRequest(&iamv1alpha1.WhoAmIRequest{}))
		return err
	}
	if err := whoami(http.Header{auth.DevUserHeader: {"root@example.com"}}); err != nil {
		t.Fatalf("the bootstrap admin on loopback: %v", err)
	}
	// Cf-Warp-Tag-Id is what cloudflared itself adds to every request it
	// proxies; the rest come from Cloudflare's edge.
	for _, k := range []string{"Cf-Warp-Tag-Id", "Cdn-Loop", "Cf-Connecting-Ip", "Cf-Ray"} {
		err := whoami(http.Header{auth.DevUserHeader: {"root@example.com"}, k: {"x"}})
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("the bootstrap admin with %s: %v; want unauthenticated", k, err)
		}
	}
}
