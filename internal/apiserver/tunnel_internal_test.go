package apiserver

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/tunnel"
)

// fixedAuth authenticates every caller as one principal, or none.
type fixedAuth struct {
	p  auth.Principal
	ok bool
}

func (f fixedAuth) Authenticate(context.Context, http.Header) (auth.Principal, bool, error) {
	return f.p, f.ok, nil
}

// listen serves s's handler with chain on a unix socket, as the node
// socket is served, and returns the socket.
func listen(t *testing.T, s *Server, chain auth.Chain) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "apiserver")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "node.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.Handler(chain), ConnContext: auth.ConnContext}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = os.RemoveAll(dir) })
	return sock
}

// open opens a tunnel on sock and serves a handler on it that names
// itself, as a node agent does.
func open(t *testing.T, sock string) error {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	c, err = tunnel.Open(t.Context(), c)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		//nolint:staticcheck // SA1019: as the agent serves a tunnel, internal/tunnel
		(&http2.Server{}).ServeConn(c, &http2.ServeConnOpts{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "the agent")
		})})
	}()
	t.Cleanup(func() { _ = c.Close(); <-done })
	return nil
}

// TestOnlyANodeOpensATunnel: a tunnel is the node the listener
// authenticates; a person, a caller without credentials, or a node this
// apiserver does not run cannot open one.
func TestOnlyANodeOpensATunnel(t *testing.T) {
	s, _, _ := runningWith(t, func(c *Config) { c.Nodes = []string{"n1"} })
	for _, tt := range []struct {
		name  string
		chain auth.Chain
		want  string
	}{
		{"no credentials", auth.Chain{}, "401"},
		{"a person", auth.Chain{fixedAuth{auth.Principal{Type: auth.User, ID: "alice@example.com"}, true}}, "403"},
		{"the system", auth.Chain{fixedAuth{auth.SystemPrincipal, true}}, "403"},
		{"a node not run here", auth.Chain{fixedAuth{auth.Principal{Type: auth.Node, ID: "n2"}, true}}, "403"},
		{"another uid on the node socket", auth.Chain{auth.PeerCred{NodeUIDs: []uint32{uint32(os.Getuid()) + 1}, NodeName: "n1"}}, "401"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := open(t, listen(t, s, tt.chain))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("opening a tunnel: %v, want %s", err, tt.want)
			}
			if _, err := s.tunnels.Node("n1"); err == nil {
				t.Error("a tunnel is open")
			}
			if _, err := s.tunnels.Node("n2"); err == nil {
				t.Error("a tunnel is open")
			}
		})
	}

	// The node socket's own: PeerCred names the node, whatever the
	// request says.
	sock := listen(t, s, auth.Chain{auth.PeerCred{NodeUIDs: []uint32{uint32(os.Getuid())}, NodeName: "n1"}})
	if err := open(t, sock); err != nil {
		t.Fatal(err)
	}
	// The hub takes the tunnel once it has answered the upgrade.
	var tun *tunnel.Tunnel
	for deadline := time.Now().Add(5 * time.Second); tun == nil; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the tunnel was never taken as n1's")
		}
		tun, _ = s.tunnels.Node("n1")
	}
	resp, err := tun.HTTPClient().Get("http://n1/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "the agent" {
		t.Errorf("the tunnel answered %q", b)
	}
}
