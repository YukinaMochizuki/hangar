package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	public, node, admin string
	log                 *syncBuffer
}

// start runs the apiserver as main does, with dev auth on the public
// listener, the node socket and the admin socket, where uid is root.
func start(t *testing.T, uid uint32) listeners {
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
	l := listeners{public: "http://" + addr, node: filepath.Join(dir, "node.sock"), admin: filepath.Join(dir, "admin.sock"), log: &syncBuffer{}}
	f := flags{
		db: filepath.Join(dir, "hangar.db"), listen: addr, socket: l.node, adminSocket: l.admin,
		nodeUID: uint(os.Getuid()), nodeName: "n1", authMode: "dev", bootstrapAdmin: "root@example.com",
		maxOwnedTenants: 3,
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

func TestSockets(t *testing.T) {
	dir := t.TempDir()
	inherit := func(t *testing.T, name, path string) *sockets {
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		return &sockets{inherited: map[string]net.Listener{name: ln}, activated: true}
	}
	t.Run("the one systemd passed", func(t *testing.T) {
		p := filepath.Join(dir, "a.sock")
		s := inherit(t, adminSocketName, p)
		ln, err := s.listen(adminSocketName, p, 0o600)
		if err != nil || ln.Addr().String() != p {
			t.Fatalf("got %v, %v", ln, err)
		}
		if err := s.unused(); err != nil {
			t.Error(err)
		}
	})
	t.Run("one at another path", func(t *testing.T) {
		s := inherit(t, adminSocketName, filepath.Join(dir, "b.sock"))
		if _, err := s.listen(adminSocketName, filepath.Join(dir, "other.sock"), 0o600); err == nil {
			t.Fatal("took a socket at another path")
		}
	})
	t.Run("none of that name under systemd", func(t *testing.T) {
		s := inherit(t, nodeSocketName, filepath.Join(dir, "c.sock"))
		p := filepath.Join(dir, "d.sock")
		if _, err := s.listen(adminSocketName, p, 0o600); err == nil {
			t.Fatal("made its own socket under systemd")
		}
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("left a socket at %s: %v", p, err)
		}
		if err := s.unused(); err == nil || !strings.Contains(err.Error(), nodeSocketName) {
			t.Errorf("unused: %v", err)
		}
	})
	t.Run("its own without systemd", func(t *testing.T) {
		s := &sockets{inherited: map[string]net.Listener{}}
		p := filepath.Join(dir, "e.sock")
		ln, err := s.listen(adminSocketName, p, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("made %v, %v", info.Mode(), err)
		}
	})
}
