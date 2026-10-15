// Package oidctest is a fake OpenID Connect provider for tests: discovery,
// keys, an authorization endpoint that signs whoever the test says in
// without asking, and a token endpoint that checks the client, the redirect
// and PKCE, then issues a signed ID token. The real relying-party code runs
// against it, so tests exercise the whole sign-in.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// Provider is the fake provider; its URL is the issuer.
type Provider struct {
	URL          string
	ClientID     string
	ClientSecret string

	key *rsa.PrivateKey

	mu       sync.Mutex
	email    string
	verified bool
	deny     bool
	tamper   func(claims map[string]any)
	codes    map[string]grant
}

type grant struct {
	nonce, challenge, redirect, email string
	verified                          bool
}

// New starts a provider that lives until the test ends. It signs in
// alice@example.com, verified, until told otherwise.
func New(t testing.TB) *Provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{
		ClientID: "hangar-test", ClientSecret: "test-client-secret", key: key,
		email: "alice@example.com", verified: true, codes: map[string]grant{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	mux.HandleFunc("GET /keys", p.keys)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p.URL = srv.URL
	return p
}

// SignIn sets who the next sign-ins are, and whether the provider says it
// verified their email.
func (p *Provider) SignIn(email string, verified bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.email, p.verified = email, verified
}

// Deny makes the next sign-ins end in error=access_denied, as when the
// person cancels at the provider.
func (p *Provider) Deny(deny bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deny = deny
}

// Tamper changes every ID token's claims before they are signed; nil stops.
func (p *Provider) Tamper(f func(claims map[string]any)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tamper = f
}

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                p.URL,
		"authorization_endpoint":                p.URL + "/authorize",
		"token_endpoint":                        p.URL + "/token",
		"jwks_uri":                              p.URL + "/keys",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (p *Provider) keys(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
}

// authorize signs the configured person in at once and sends the browser
// back with a code, as the real provider does after its own pages.
func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect := q.Get("redirect_uri")
	if q.Get("client_id") != p.ClientID || q.Get("response_type") != "code" || redirect == "" ||
		!strings.Contains(q.Get("scope"), "openid") || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	back := url.Values{"state": {q.Get("state")}}
	if p.deny {
		back.Set("error", "access_denied")
	} else {
		code := randomString()
		p.codes[code] = grant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: redirect, email: p.email, verified: p.verified}
		back.Set("code", code)
	}
	http.Redirect(w, r, redirect+"?"+back.Encode(), http.StatusFound)
}

// token exchanges a code once, for the client that holds the secret, with
// the redirect and PKCE verifier the code was issued for.
func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if u, err := url.QueryUnescape(id); err == nil {
		id = u
	}
	if s, err := url.QueryUnescape(secret); err == nil {
		secret = s
	}
	if id != p.ClientID || secret != p.ClientSecret {
		oauthError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	p.mu.Lock()
	g, ok := p.codes[r.PostForm.Get("code")]
	delete(p.codes, r.PostForm.Get("code"))
	tamper := p.tamper
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("redirect_uri") != g.redirect ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		oauthError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	now := time.Now()
	claims := map[string]any{
		"iss": p.URL, "aud": p.ClientID, "sub": "sub-" + g.email, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"nonce": g.nonce, "email": g.email, "email_verified": g.verified,
	}
	if tamper != nil {
		tamper(claims)
	}
	idToken, err := p.sign(claims)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"access_token": randomString(), "token_type": "Bearer", "expires_in": 3600, "id_token": idToken})
}

func (p *Provider) sign(claims map[string]any) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: p.key, KeyID: "k1"}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", err
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return sig.CompactSerialize()
}

func oauthError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func randomString() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
