package edge_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/edge"
	"github.com/YukinaMochizuki/hangar/internal/edge/accesstest"
)

func TestAccessVerifier(t *testing.T) {
	iss := accesstest.NewIssuer(t)
	cf, err := edge.NewAccessVerifier(t.Context(), iss.Config())
	if err != nil {
		t.Fatal(err)
	}
	header := func(tok string) http.Header {
		h := http.Header{}
		if tok != "" {
			h.Set(edge.AccessJWTHeader, tok)
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
