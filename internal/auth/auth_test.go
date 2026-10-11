package auth_test

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/authtest"
)

func TestCloudflare(t *testing.T) {
	iss := authtest.NewIssuer(t)
	cf, err := auth.NewCloudflare(t.Context(), iss.Config())
	if err != nil {
		t.Fatal(err)
	}
	header := func(tok string) http.Header {
		h := http.Header{}
		if tok != "" {
			h.Set(auth.CloudflareJWTHeader, tok)
		}
		return h
	}

	tests := []struct {
		name    string
		header  http.Header
		want    auth.Principal
		ok      bool
		wantErr bool
	}{
		{name: "user", header: header(iss.UserToken(t, "Alice@Example.com")), want: auth.Principal{Type: auth.User, ID: "alice@example.com"}, ok: true},
		{name: "service token", header: header(iss.ServiceToken(t, "abc.access")), want: auth.Principal{Type: auth.ServiceToken, ID: "abc.access"}, ok: true},
		{name: "no token", header: header("")},
		{
			name: "identity header without a token",
			header: http.Header{
				"Cf-Access-Authenticated-User-Email": {"root@example.com"},
			},
		},
		{name: "wrong audience", header: header(iss.Token(t, map[string]any{"email": "a@b.c", "aud": []string{"other"}})), wantErr: true},
		{name: "wrong issuer", header: header(iss.Token(t, map[string]any{"email": "a@b.c", "iss": "https://evil.example"})), wantErr: true},
		{name: "expired", header: header(iss.Token(t, map[string]any{"email": "a@b.c", "exp": time.Now().Add(-time.Hour).Unix()})), wantErr: true},
		{name: "unknown signing key", header: header(iss.ForeignToken(t, map[string]any{"email": "a@b.c"})), wantErr: true},
		{name: "no identity claim", header: header(iss.Token(t, nil)), wantErr: true},
		{name: "garbage", header: header("not-a-jwt"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok, err := cf.Authenticate(t.Context(), tt.header)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tt.wantErr)
			}
			if ok != tt.ok || p != tt.want {
				t.Errorf("got %v, %t; want %v, %t", p, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestChain(t *testing.T) {
	chain := auth.Chain{auth.Dev{}}
	h := http.Header{}
	if _, err := chain.Authenticate(t.Context(), h); err == nil {
		t.Error("a request with no credentials was authenticated")
	}
	h.Set(auth.DevUserHeader, "Bob@Example.com")
	ctx, err := chain.Authenticate(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := auth.FromContext(ctx); p != (auth.Principal{Type: auth.User, ID: "bob@example.com"}) {
		t.Errorf("principal %v", p)
	}
}

func TestPeerCred(t *testing.T) {
	me := uint32(os.Getuid())
	for _, tt := range []struct {
		name    string
		uids    []uint32
		ok      bool
		wantErr bool
	}{
		{name: "allowed uid", uids: []uint32{me}, ok: true},
		{name: "other uid", uids: []uint32{me + 1}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sock := filepath.Join(t.TempDir(), "s.sock")
			ln, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			type result struct {
				p   auth.Principal
				ok  bool
				err error
			}
			got := make(chan result, 1)
			srv := &http.Server{
				ConnContext: auth.ConnContext,
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					p, ok, err := auth.PeerCred{NodeUIDs: tt.uids}.Authenticate(r.Context(), r.Header)
					got <- result{p, ok, err}
				}),
			}
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })

			client := &http.Client{Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", sock)
				},
			}}
			resp, err := client.Get("http://node/")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			r := <-got
			if (r.err != nil) != tt.wantErr || r.ok != tt.ok {
				t.Fatalf("got %v, %t, %v", r.p, r.ok, r.err)
			}
			if tt.ok && r.p != (auth.Principal{Type: auth.Node, ID: "local"}) {
				t.Errorf("principal %v", r.p)
			}
		})
	}
}

func TestPeerCredIgnoresTCP(t *testing.T) {
	_, ok, err := auth.PeerCred{NodeUIDs: []uint32{0}}.Authenticate(t.Context(), http.Header{})
	if ok || err != nil {
		t.Errorf("a request without peer credentials was recognized: %t, %v", ok, err)
	}
}
