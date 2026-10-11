// Package authtest stands in for Cloudflare Access in tests: it serves a
// signing key set and issues tokens shaped like the ones Access signs, so the
// real verification path runs against them.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/YukinaMochizuki/hangar/internal/auth"
)

// Issuer is a fake Access team: its URL is the issuer and team domain.
type Issuer struct {
	URL      string
	Audience string
	key      *rsa.PrivateKey
}

// NewIssuer starts an issuer that lives until the test ends.
func NewIssuer(t testing.TB) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iss := &Issuer{Audience: "test-audience", key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/cdn-cgi/access/certs", func(w http.ResponseWriter, _ *http.Request) {
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig"}}}
		_ = json.NewEncoder(w).Encode(set)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	iss.URL = srv.URL
	return iss
}

// Config is the authenticator configuration that trusts this issuer.
func (i *Issuer) Config() auth.CloudflareConfig {
	return auth.CloudflareConfig{TeamDomain: i.URL, Audience: i.Audience}
}

// UserToken returns a valid token for a person.
func (i *Issuer) UserToken(t testing.TB, email string) string {
	return i.Token(t, map[string]any{"email": email})
}

// ServiceToken returns a valid token for an Access service token.
func (i *Issuer) ServiceToken(t testing.TB, clientID string) string {
	return i.Token(t, map[string]any{"common_name": clientID})
}

// Token signs claims on top of valid defaults (issuer, audience, expiry);
// claims override the defaults.
func (i *Issuer) Token(t testing.TB, claims map[string]any) string {
	t.Helper()
	return i.sign(t, i.key, claims)
}

// ForeignToken is otherwise valid but signed by a key the issuer never
// published.
func (i *Issuer) ForeignToken(t testing.TB, claims map[string]any) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return i.sign(t, key, claims)
}

func (i *Issuer) sign(t testing.TB, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	now := time.Now()
	body := map[string]any{
		"iss":  i.URL,
		"aud":  []string{i.Audience},
		"iat":  now.Unix(),
		"exp":  now.Add(time.Hour).Unix(),
		"type": "app",
	}
	for k, v := range claims {
		body[k] = v
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "test"}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := sig.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
