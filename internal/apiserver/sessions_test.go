package apiserver_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/oauth2"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1/tenancyv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/login"
	"github.com/YukinaMochizuki/hangar/internal/auth/oidctest"
	"github.com/YukinaMochizuki/hangar/internal/auth/session"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// oidcWorld is an apiserver in oidc auth, over TLS as in production, with a
// fake provider. alice@example.com is the bootstrap admin.
type oidcWorld struct {
	t        *testing.T
	url      string
	provider *oidctest.Provider
	tls      *http.Client
	srv      *apiserver.Server
	// dir holds the store's files.
	dir string
}

func newOIDCWorld(t *testing.T) *oidcWorld {
	t.Helper()
	return newOIDCWorldWith(t, 0)
}

// newOIDCWorldWith is newOIDCWorld with a cap on the sign-ins in progress;
// zero is the default.
func newOIDCWorldWith(t *testing.T, maxPending int) *oidcWorld {
	t.Helper()
	return newOIDCWorldConfig(t, maxPending, nil)
}

// newOIDCWorldConfig is newOIDCWorldWith with configure adjusting the
// apiserver's config.
func newOIDCWorldConfig(t *testing.T, maxPending int, configure func(*apiserver.Config)) *oidcWorld {
	t.Helper()
	return newOIDCWorldCreds(t, maxPending, configure, nil)
}

// newOIDCWorldCreds is newOIDCWorldConfig with sign-in keeping its
// credentials in what creds wraps the store in; nil is the store itself.
func newOIDCWorldCreds(t *testing.T, maxPending int, configure func(*apiserver.Config), creds func(store.Credentials) store.Credentials) *oidcWorld {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, filepath.Join(dir, "hangar.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := apiserver.Config{Store: st, BootstrapAdmin: "alice@example.com", DefaultPlanLimits: &tenancyv1alpha1.Limits{}}
	if configure != nil {
		configure(&cfg)
	}
	srv := apiserver.New(cfg)
	var handler http.Handler
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	ts.StartTLS()
	p := oidctest.New(t)
	var c store.Credentials = st
	if creds != nil {
		c = creds(st)
	}
	chain, err := srv.EnableOIDC(apiserver.OIDCConfig{
		Issuer: p.URL, ClientID: p.ClientID, ClientSecret: token.Secret(p.ClientSecret), PublicURL: ts.URL,
		DeviceInterval: time.Second, MaxPending: maxPending,
	}, c)
	if err != nil {
		t.Fatal(err)
	}
	// As on the TCP listener, the client's address is the connection's:
	// a test may say what it is with testPeer.
	handler = asPeer(auth.ClientIPFromConn(srv.PublicHandler(chain)))
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		ts.Close()
		cancel()
		<-done
		_ = st.Close()
	})
	return &oidcWorld{t: t, url: ts.URL, provider: p, tls: ts.Client(), srv: srv, dir: dir}
}

// testPeer names the address a request's connection is to come from, so
// tests can start device logins from several sources.
const testPeer = "Test-Peer"

// asPeer gives a request the connection address its testPeer header names.
func asPeer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.Header.Get(testPeer); p != "" {
			r.RemoteAddr = net.JoinHostPort(p, "40000")
		}
		next.ServeHTTP(w, r)
	})
}

// browser is a fresh browser: a cookie jar that follows redirects.
func (w *oidcWorld) browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Transport: w.tls.Transport, Jar: jar}
}

// get loads a page in the browser and returns the final response's status,
// URL and body.
func get(t *testing.T, c *http.Client, u string) (int, *url.URL, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Request.URL, string(b)
}

// signIn signs a browser in as email through the console's sign-in, and
// returns its session cookie, empty if it got none.
func (w *oidcWorld) signIn(c *http.Client, email string) string {
	w.t.Helper()
	w.provider.SignIn(email, true)
	_, final, _ := get(w.t, c, w.url+"/auth/login?return=/console/home")
	if final.Path != "/console/home" {
		w.t.Fatalf("signing in %s ended at %s", email, final)
	}
	return w.cookie(c)
}

// signInAdmin signs the bootstrap admin in, once the apiserver has granted
// her the role in the background: until then she is not invited.
func (w *oidcWorld) signInAdmin(c *http.Client, email string) string {
	w.t.Helper()
	var cookie string
	testutil.Eventually(w.t, "the bootstrap admin can sign in", func() bool {
		w.provider.SignIn(email, true)
		get(w.t, c, w.url+"/auth/login")
		cookie = w.cookie(c)
		return cookie != ""
	})
	return cookie
}

func (w *oidcWorld) cookie(c *http.Client) string {
	u, _ := url.Parse(w.url)
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == session.CookieName {
			return ck.Value
		}
	}
	return ""
}

// headers sets request headers on every call, unary or streaming.
type headers map[string]string

func (h headers) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		for k, v := range h {
			req.Header().Set(k, v)
		}
		return next(ctx, req)
	}
}

func (h headers) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		for k, v := range h {
			conn.RequestHeader().Set(k, v)
		}
		return conn
	}
}

func (h headers) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// asCookie are the headers the console's own page sends with a session.
func (w *oidcWorld) asCookie(cookie string) headers {
	return headers{"Cookie": session.CookieName + "=" + cookie, "Origin": w.url}
}

func asBearer(access string) headers { return headers{"Authorization": "Bearer " + access} }

func (w *oidcWorld) accounts(h headers) iamv1alpha1connect.AccountServiceClient {
	return iamv1alpha1connect.NewAccountServiceClient(w.tls, w.url, connect.WithInterceptors(h))
}

func (w *oidcWorld) sessions(h headers) iamv1alpha1connect.SessionServiceClient {
	return iamv1alpha1connect.NewSessionServiceClient(w.tls, w.url, connect.WithInterceptors(h))
}

func (w *oidcWorld) whoami(h headers) (*iamv1alpha1.WhoAmIResponse, error) {
	res, err := w.accounts(h).WhoAmI(w.t.Context(), connect.NewRequest(&iamv1alpha1.WhoAmIRequest{}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// adminInvites makes alice, the bootstrap admin, sign up and invite the
// emails as platform members, signed in with her browser session.
func (w *oidcWorld) adminInvites(emails ...string) headers {
	w.t.Helper()
	ctx := w.t.Context()
	alice := w.asCookie(w.signInAdmin(w.browser(), "alice@example.com"))
	must[iamv1alpha1.SignUpResponse](w.t)(w.accounts(alice).SignUp(ctx, connect.NewRequest(&iamv1alpha1.SignUpRequest{Username: "alice"})))
	bindings := iamv1alpha1connect.NewRoleBindingServiceClient(w.tls, w.url, connect.WithInterceptors(alice))
	for i, e := range emails {
		must[iamv1alpha1.ApplyRoleBindingResponse](w.t)(bindings.ApplyRoleBinding(ctx, connect.NewRequest(&iamv1alpha1.ApplyRoleBindingRequest{
			RoleBinding: &iamv1alpha1.RoleBinding{
				Metadata: &metav1.ObjectMeta{Name: "member-" + string(rune('a'+i))},
				Spec: &iamv1alpha1.RoleBindingSpec{
					Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: e}},
					Role:    "platform.member",
					Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}},
				},
			},
		})))
	}
	return alice
}

// TestBrowserSignIn: the console's sign-in leaves a session cookie that
// authenticates the console's own requests, as the verified email in lower
// case, and nothing else; signing out ends it.
func TestBrowserSignIn(t *testing.T) {
	w := newOIDCWorld(t)
	c := w.browser()
	w.signInAdmin(w.browser(), "alice@example.com")
	cookie := w.signIn(c, "Alice@Example.com")
	if !strings.HasPrefix(cookie, token.Session) {
		t.Fatalf("session cookie %.4s…", cookie)
	}
	u, _ := url.Parse(w.url)
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == "__Host-hangar_login" {
			t.Error("the sign-in's state cookie outlived the sign-in")
		}
	}
	me, err := w.whoami(w.asCookie(cookie))
	if err != nil {
		t.Fatal(err)
	}
	if me.GetType() != "user" || me.GetId() != "alice@example.com" {
		t.Errorf("whoami: %v", me)
	}
	ck := session.CookieName + "=" + cookie
	for name, h := range map[string]headers{
		"no origin":      {"Cookie": ck},
		"another origin": {"Cookie": ck, "Origin": "https://evil.example"},
		"cross site":     {"Cookie": ck, "Origin": w.url, "Sec-Fetch-Site": "cross-site"},
		"forged cookie":  {"Cookie": session.CookieName + "=hgs_forged", "Origin": w.url},
	} {
		_, err := w.whoami(h)
		wantCode(t, err, connect.CodeUnauthenticated, name)
	}
	// The dev header means nothing on this listener.
	_, err = w.whoami(headers{auth.DevUserHeader: "alice@example.com"})
	wantCode(t, err, connect.CodeUnauthenticated, "the dev header")

	logout := func(origin string) int {
		req, _ := http.NewRequest(http.MethodPost, w.url+"/auth/logout", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := logout("https://evil.example"); got != http.StatusForbidden {
		t.Errorf("a cross-site sign-out: %d", got)
	}
	if _, err := w.whoami(w.asCookie(cookie)); err != nil {
		t.Fatalf("a cross-site sign-out ended the session: %v", err)
	}
	if got := logout(w.url); got != http.StatusNoContent {
		t.Errorf("sign-out: %d", got)
	}
	if w.cookie(c) != "" {
		t.Error("sign-out left the cookie in the browser")
	}
	_, err = w.whoami(w.asCookie(cookie))
	wantCode(t, err, connect.CodeUnauthenticated, "the session after sign-out")
}

// TestSignInRefusals: what the callback refuses, and that a refusal leaves
// no session behind.
func TestSignInRefusals(t *testing.T) {
	w := newOIDCWorld(t)
	w.adminInvites("bob@example.com", "kate@example.com")

	t.Run("forged state", func(t *testing.T) {
		c := w.browser()
		status, _, body := get(t, c, w.url+"/auth/callback?state=forged&code=whatever")
		if status != http.StatusBadRequest || !strings.Contains(body, "did not come from this browser") {
			t.Errorf("%d %s", status, body)
		}
	})
	t.Run("someone else's state", func(t *testing.T) {
		// The attacker starts a sign-in and stops at the provider's
		// redirect back; the victim's browser is sent there.
		attacker := w.browser()
		attacker.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
			if req.URL.Path == "/auth/callback" {
				return http.ErrUseLastResponse
			}
			return nil
		}
		w.provider.SignIn("bob@example.com", true)
		resp, err := attacker.Get(w.url + "/auth/login")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		callback := resp.Header.Get("Location")
		if !strings.Contains(callback, "/auth/callback?") {
			t.Fatalf("no callback: %s", callback)
		}
		victim := w.browser()
		status, _, body := get(t, victim, callback)
		if status != http.StatusBadRequest || w.cookie(victim) != "" || !strings.Contains(body, "did not come from this browser") {
			t.Errorf("the victim's browser took the attacker's sign-in: %d %s", status, body)
		}
	})
	for _, tt := range []struct {
		name, email string
		verified    bool
		tamper      func(map[string]any)
		deny        bool
		status      int
		want        string
	}{
		{name: "unverified email", email: "bob@example.com", status: http.StatusBadRequest, want: "has not verified the email address bob@example.com"},
		{name: "not invited", email: "eve@example.com", verified: true, status: http.StatusForbidden, want: "eve@example.com has not been invited to Hangar"},
		// U+212A KELVIN SIGN lower-cases to k: kate's invitation.
		{name: "non-ASCII email", email: "\u212aate@example.com", verified: true, status: http.StatusBadRequest, want: "only ASCII email addresses"},
		{name: "another nonce", email: "bob@example.com", verified: true, tamper: func(c map[string]any) { c["nonce"] = "other" }, status: http.StatusBadRequest, want: "for another sign-in"},
		{name: "another audience", email: "bob@example.com", verified: true, tamper: func(c map[string]any) { c["aud"] = "other-client" }, status: http.StatusBadRequest, want: "did not verify"},
		{name: "expired ID token", email: "bob@example.com", verified: true, tamper: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }, status: http.StatusBadRequest, want: "did not verify"},
		{name: "cancelled at the provider", email: "bob@example.com", verified: true, deny: true, status: http.StatusBadRequest, want: "returned an error (access_denied)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w.provider.SignIn(tt.email, tt.verified)
			w.provider.Tamper(tt.tamper)
			w.provider.Deny(tt.deny)
			defer w.provider.Tamper(nil)
			defer w.provider.Deny(false)
			c := w.browser()
			status, _, body := get(t, c, w.url+"/auth/login")
			if status != tt.status || !strings.Contains(body, tt.want) {
				t.Errorf("got %d %q, want %d %q", status, body, tt.status, tt.want)
			}
			if w.cookie(c) != "" {
				t.Error("a refused sign-in left a session")
			}
		})
	}
	t.Run("return to another site", func(t *testing.T) {
		for _, ret := range []string{"//evil.example/x", "https://evil.example/", "/\\evil.example", "/\t/evil.example", "/\n/evil.example", "/%09/evil.example"} {
			w.provider.SignIn("bob@example.com", true)
			_, final, _ := get(t, w.browser(), w.url+"/auth/login?return="+url.QueryEscape(ret))
			if final.Host != strings.TrimPrefix(w.url, "https://") || final.Path != "/" {
				t.Errorf("return %q ended at %s", ret, final)
			}
		}
	})
}

// loopback stands in for the CLI's loopback listener: it records what the
// browser brings back.
type loopback struct {
	url string
	mu  sync.Mutex
	got url.Values
}

func newLoopback(t *testing.T) *loopback {
	l := &loopback{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.got = r.URL.Query()
		l.mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	l.url = srv.URL + "/callback"
	return l
}

func (l *loopback) query() url.Values {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.got
}

// cliSignIn runs a CLI's loopback sign-in in a browser and returns what came
// back to the loopback, with the PKCE verifier.
func (w *oidcWorld) cliSignIn(email string) (url.Values, string, *loopback) {
	w.t.Helper()
	l := newLoopback(w.t)
	verifier := oauth2.GenerateVerifier()
	w.provider.SignIn(email, true)
	get(w.t, w.browser(), w.url+"/auth/cli?"+url.Values{
		"redirect_uri": {l.url}, "state": {"cli-state"},
		"code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)}, "code_challenge_method": {"S256"},
	}.Encode())
	return l.query(), verifier, l
}

// TestCLISignIn: the loopback sign-in's one-time code, with its verifier,
// gets access and refresh tokens; refresh rotates them, and replaying a
// spent refresh token revokes the login.
func TestCLISignIn(t *testing.T) {
	w := newOIDCWorld(t)
	w.adminInvites("bob@example.com")
	ctx := t.Context()
	anon := w.sessions(headers{})

	got, verifier, l := w.cliSignIn("Bob@Example.com")
	if got.Get("state") != "cli-state" || got.Get("code") == "" {
		t.Fatalf("loopback got %v", got)
	}
	exchange := func(code, verifier, redirect string) (*iamv1alpha1.Tokens, error) {
		res, err := anon.ExchangeCode(ctx, connect.NewRequest(&iamv1alpha1.ExchangeCodeRequest{Code: code, CodeVerifier: verifier, RedirectUri: redirect}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetTokens(), nil
	}
	tok, err := exchange(got.Get("code"), verifier, l.url)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok.GetAccessToken(), token.Access) || !strings.HasPrefix(tok.GetRefreshToken(), token.Refresh) || tok.GetEmail() != "bob@example.com" {
		t.Fatalf("tokens %.4s… %.4s… %s", tok.GetAccessToken(), tok.GetRefreshToken(), tok.GetEmail())
	}
	if d := time.Until(tok.GetAccessExpireTime().AsTime()); d < 55*time.Minute || d > time.Hour {
		t.Errorf("access token lives %v", d)
	}
	_, err = exchange(got.Get("code"), verifier, l.url)
	wantCode(t, err, connect.CodeUnauthenticated, "a code exchanged twice")

	me, err := w.whoami(asBearer(tok.GetAccessToken()))
	if err != nil || me.GetId() != "bob@example.com" || !me.GetInvited() || me.GetMember() {
		t.Fatalf("whoami: %v, %v", me, err)
	}
	// Membership rules are unchanged: invited, not signed up, nothing else.
	tenants := tenancyv1alpha1connect.NewTenantServiceClient(w.tls, w.url, connect.WithInterceptors(asBearer(tok.GetAccessToken())))
	_, err = tenants.ListTenants(ctx, connect.NewRequest(&tenancyv1alpha1.ListTenantsRequest{}))
	wantCode(t, err, connect.CodePermissionDenied, "listing tenants before signing up")

	// Refresh rotates; the spent token, replayed, revokes the login.
	res, err := anon.Refresh(ctx, connect.NewRequest(&iamv1alpha1.RefreshRequest{RefreshToken: tok.GetRefreshToken()}))
	if err != nil {
		t.Fatal(err)
	}
	next := res.Msg.GetTokens()
	if next.GetRefreshToken() == tok.GetRefreshToken() || next.GetAccessToken() == tok.GetAccessToken() {
		t.Fatal("refresh did not rotate")
	}
	if _, err := w.whoami(asBearer(next.GetAccessToken())); err != nil {
		t.Fatal(err)
	}
	_, err = anon.Refresh(ctx, connect.NewRequest(&iamv1alpha1.RefreshRequest{RefreshToken: tok.GetRefreshToken()}))
	wantCode(t, err, connect.CodeUnauthenticated, "a spent refresh token")
	if err != nil && !strings.Contains(err.Error(), "already used") {
		t.Errorf("reuse error: %v", err)
	}
	_, err = w.whoami(asBearer(next.GetAccessToken()))
	wantCode(t, err, connect.CodeUnauthenticated, "the newest access token after a reuse")
	_, err = anon.Refresh(ctx, connect.NewRequest(&iamv1alpha1.RefreshRequest{RefreshToken: next.GetRefreshToken()}))
	wantCode(t, err, connect.CodeUnauthenticated, "the newest refresh token after a reuse")

	// Logout revokes the login it names.
	got, verifier, l = w.cliSignIn("bob@example.com")
	tok, err = exchange(got.Get("code"), verifier, l.url)
	if err != nil {
		t.Fatal(err)
	}
	must[iamv1alpha1.LogoutResponse](t)(anon.Logout(ctx, connect.NewRequest(&iamv1alpha1.LogoutRequest{RefreshToken: tok.GetRefreshToken()})))
	_, err = w.whoami(asBearer(tok.GetAccessToken()))
	wantCode(t, err, connect.CodeUnauthenticated, "an access token after logout")
}

// TestCLISignInRefusals: the one-time code needs its own verifier and
// redirect; the CLI's redirect must be a loopback one; and a refused
// sign-in tells the loopback why.
func TestCLISignInRefusals(t *testing.T) {
	w := newOIDCWorld(t)
	w.adminInvites("bob@example.com")
	ctx := t.Context()
	anon := w.sessions(headers{})

	got, verifier, l := w.cliSignIn("bob@example.com")
	_, err := anon.ExchangeCode(ctx, connect.NewRequest(&iamv1alpha1.ExchangeCodeRequest{Code: got.Get("code"), CodeVerifier: oauth2.GenerateVerifier(), RedirectUri: l.url}))
	wantCode(t, err, connect.CodeUnauthenticated, "another verifier")
	_, err = anon.ExchangeCode(ctx, connect.NewRequest(&iamv1alpha1.ExchangeCodeRequest{Code: got.Get("code"), CodeVerifier: verifier, RedirectUri: l.url}))
	wantCode(t, err, connect.CodeUnauthenticated, "the right verifier after a wrong one spent the code")

	got, verifier, _ = w.cliSignIn("bob@example.com")
	_, err = anon.ExchangeCode(ctx, connect.NewRequest(&iamv1alpha1.ExchangeCodeRequest{Code: got.Get("code"), CodeVerifier: verifier, RedirectUri: "http://127.0.0.1:1/callback"}))
	wantCode(t, err, connect.CodeUnauthenticated, "another redirect")

	for _, redirect := range []string{"https://evil.example/callback", "http://localhost:9/callback", "http://127.0.0.1/callback", "http://127.0.0.1:9/other", "http://10.0.0.1:9/callback"} {
		status, _, body := get(t, w.browser(), w.url+"/auth/cli?"+url.Values{
			"redirect_uri": {redirect}, "state": {"s"}, "code_challenge": {oauth2.S256ChallengeFromVerifier("v")}, "code_challenge_method": {"S256"},
		}.Encode())
		if status != http.StatusBadRequest || !strings.Contains(body, "Sign-in failed") {
			t.Errorf("redirect %s: %d", redirect, status)
		}
	}

	got, _, _ = w.cliSignIn("eve@example.com")
	if got.Get("error") != "access_denied" || got.Get("state") != "cli-state" || got.Get("code") != "" ||
		!strings.Contains(got.Get("error_description"), "not been invited") {
		t.Errorf("an uninvited CLI sign-in: %v", got)
	}
}

// TestDeviceSignIn: the device flow, approved and denied, from a browser
// signed in to the console.
func TestDeviceSignIn(t *testing.T) {
	w := newOIDCWorld(t)
	w.adminInvites("bob@example.com")
	ctx := t.Context()
	anon := w.sessions(headers{testPeer: "198.51.100.7"})
	start := func() *iamv1alpha1.StartDeviceLoginResponse {
		return must[iamv1alpha1.StartDeviceLoginResponse](t)(anon.StartDeviceLogin(ctx, connect.NewRequest(&iamv1alpha1.StartDeviceLoginRequest{})))
	}
	poll := func(code string) (*iamv1alpha1.Tokens, error) {
		res, err := anon.PollDeviceLogin(ctx, connect.NewRequest(&iamv1alpha1.PollDeviceLoginRequest{DeviceCode: code}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetTokens(), nil
	}
	before := time.Now().Truncate(time.Second)
	d := start()
	after := time.Now()
	if len(d.GetUserCode()) != 9 || d.GetVerificationUri() != w.url+"/auth/device" || !strings.Contains(d.GetVerificationUriComplete(), d.GetUserCode()) {
		t.Fatalf("start: %v", d)
	}
	_, err := poll(d.GetDeviceCode())
	wantCode(t, err, connect.CodeFailedPrecondition, "polling before approval")
	_, err = poll(d.GetDeviceCode())
	wantCode(t, err, connect.CodeResourceExhausted, "polling too fast")

	// Not signed in, the page sends the browser to sign in first, and back.
	c := w.browser()
	w.provider.SignIn("bob@example.com", true)
	status, final, body := get(t, c, d.GetVerificationUriComplete())
	if status != http.StatusOK || final.Path != "/auth/device" || !strings.Contains(body, "Sign in the hangar CLI as bob@example.com?") ||
		!strings.Contains(body, d.GetUserCode()) {
		t.Fatalf("device page: %d %s %s", status, final, body)
	}
	// It says where and when the login was started, and to deny one the
	// person did not start.
	m := regexp.MustCompile(`started from <strong>198\.51\.100\.7</strong> at <strong>(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) UTC</strong>`).FindStringSubmatch(body)
	if m == nil || !strings.Contains(body, "choose Deny") {
		t.Errorf("the device page does not say where and when the login was started: %s", body)
	} else if at, err := time.Parse(time.DateTime, m[1]); err != nil || at.Before(before) || at.After(after) {
		t.Errorf("the device page says it started at %s UTC, not between %s and %s", m[1], before.UTC(), after.UTC())
	}
	decide := func(c *http.Client, code, decision, origin string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, w.url+"/auth/device", strings.NewReader(url.Values{"user_code": {code}, "decision": {decision}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if status, _ := decide(c, d.GetUserCode(), "approve", "https://evil.example"); status != http.StatusForbidden {
		t.Errorf("a cross-site approval: %d", status)
	}
	if status, _ := decide(w.browser(), d.GetUserCode(), "approve", w.url); status != http.StatusUnauthorized {
		t.Errorf("an approval without a session: %d", status)
	}
	if status, body := decide(c, strings.ToLower(d.GetUserCode()), "approve", w.url); status != http.StatusOK || !strings.Contains(body, "Approved") {
		t.Fatalf("approve: %d %s", status, body)
	}
	if status, _ := decide(c, d.GetUserCode(), "approve", w.url); status != http.StatusNotFound {
		t.Errorf("a code approved twice: %d", status)
	}
	var tok *iamv1alpha1.Tokens
	testutil.Eventually(t, "the device login is approved", func() bool {
		tok, err = poll(d.GetDeviceCode())
		return err == nil
	})
	if me, err := w.whoami(asBearer(tok.GetAccessToken())); err != nil || me.GetId() != "bob@example.com" {
		t.Fatalf("whoami: %v %v", me, err)
	}
	_, err = poll(d.GetDeviceCode())
	wantCode(t, err, connect.CodeUnauthenticated, "polling after the tokens were handed out")

	denied := start()
	if status, body := decide(c, denied.GetUserCode(), "deny", w.url); status != http.StatusOK || !strings.Contains(body, "Denied") {
		t.Fatalf("deny: %d %s", status, body)
	}
	_, err = poll(denied.GetDeviceCode())
	wantCode(t, err, connect.CodePermissionDenied, "a denied device login")

	if status, _, body := get(t, c, w.url+"/auth/device?user_code=BBBB-BBBB"); status != http.StatusNotFound || !strings.Contains(body, "unknown or has expired") {
		t.Errorf("an unknown code: %d %s", status, body)
	}
}

// TestRevokeSessions: a person revokes their own logins; only an
// administrator revokes someone else's.
func TestRevokeSessions(t *testing.T) {
	w := newOIDCWorld(t)
	alice := w.adminInvites("bob@example.com", "carol@example.com")
	ctx := t.Context()
	cliTokens := func(email string) *iamv1alpha1.Tokens {
		got, verifier, l := w.cliSignIn(email)
		res, err := w.sessions(headers{}).ExchangeCode(ctx, connect.NewRequest(&iamv1alpha1.ExchangeCodeRequest{Code: got.Get("code"), CodeVerifier: verifier, RedirectUri: l.url}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetTokens()
	}
	bob, carol := cliTokens("bob@example.com"), cliTokens("carol@example.com")
	bobBrowser := w.asCookie(w.signIn(w.browser(), "bob@example.com"))

	_, err := w.sessions(asBearer(bob.GetAccessToken())).RevokeSessions(ctx, connect.NewRequest(&iamv1alpha1.RevokeSessionsRequest{Email: "carol@example.com"}))
	wantCode(t, err, connect.CodePermissionDenied, "bob revoking carol")
	_, err = w.sessions(headers{}).RevokeSessions(ctx, connect.NewRequest(&iamv1alpha1.RevokeSessionsRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated, "revoking without credentials")
	// Refused, not lower-cased into another address: the Kelvin sign is k.
	_, err = w.sessions(alice).RevokeSessions(ctx, connect.NewRequest(&iamv1alpha1.RevokeSessionsRequest{Email: "\u212aate@example.com"}))
	wantCode(t, err, connect.CodeInvalidArgument, "revoking a non-ASCII email")

	res := must[iamv1alpha1.RevokeSessionsResponse](t)(w.sessions(asBearer(bob.GetAccessToken())).RevokeSessions(ctx, connect.NewRequest(&iamv1alpha1.RevokeSessionsRequest{})))
	if res.GetRevoked() != 4 {
		t.Errorf("bob revoked %d credentials, want his CLI's two, the family its sign-in reserved, and his browser's one", res.GetRevoked())
	}
	for name, h := range map[string]headers{"cli": asBearer(bob.GetAccessToken()), "browser": bobBrowser} {
		_, err := w.whoami(h)
		wantCode(t, err, connect.CodeUnauthenticated, "bob's "+name+" after revoking his own")
	}
	if _, err := w.whoami(asBearer(carol.GetAccessToken())); err != nil {
		t.Fatalf("bob's revoke ended carol's login: %v", err)
	}
	must[iamv1alpha1.RevokeSessionsResponse](t)(w.sessions(alice).RevokeSessions(ctx, connect.NewRequest(&iamv1alpha1.RevokeSessionsRequest{Email: "Carol@Example.com"})))
	_, err = w.whoami(asBearer(carol.GetAccessToken()))
	wantCode(t, err, connect.CodeUnauthenticated, "carol's login after the admin revoked it")
	if _, err := w.whoami(alice); err != nil {
		t.Errorf("the admin's own session went too: %v", err)
	}
}

// uninvite deletes the platform binding adminInvites made for the i-th
// email.
func (w *oidcWorld) uninvite(admin headers, i int) {
	w.t.Helper()
	bindings := iamv1alpha1connect.NewRoleBindingServiceClient(w.tls, w.url, connect.WithInterceptors(admin))
	must[iamv1alpha1.DeleteRoleBindingResponse](w.t)(bindings.DeleteRoleBinding(w.t.Context(),
		connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: "member-" + string(rune('a'+i))})))
}

// TestAdmissionAtEveryIssue: someone uninvited after signing in in a
// browser gets no CLI tokens from a loopback code or a device login, and
// cannot approve a device login.
func TestAdmissionAtEveryIssue(t *testing.T) {
	w := newOIDCWorld(t)
	alice := w.adminInvites("bob@example.com")
	ctx := t.Context()
	anon := w.sessions(headers{})

	got, verifier, l := w.cliSignIn("bob@example.com")
	bobBrowser := w.browser()
	w.signIn(bobBrowser, "bob@example.com")
	d := must[iamv1alpha1.StartDeviceLoginResponse](t)(anon.StartDeviceLogin(ctx, connect.NewRequest(&iamv1alpha1.StartDeviceLoginRequest{})))
	w.uninvite(alice, 0)

	_, err := anon.ExchangeCode(ctx, connect.NewRequest(&iamv1alpha1.ExchangeCodeRequest{Code: got.Get("code"), CodeVerifier: verifier, RedirectUri: l.url}))
	wantCode(t, err, connect.CodePermissionDenied, "exchanging a code after the invitation went")

	req, _ := http.NewRequest(http.MethodPost, w.url+"/auth/device", strings.NewReader(url.Values{"user_code": {d.GetUserCode()}, "decision": {"approve"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", w.url)
	resp, err := bobBrowser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "has not been invited") {
		t.Errorf("approving after the invitation went: %d %s", resp.StatusCode, body)
	}
	_, err = anon.PollDeviceLogin(ctx, connect.NewRequest(&iamv1alpha1.PollDeviceLoginRequest{DeviceCode: d.GetDeviceCode()}))
	wantCode(t, err, connect.CodeFailedPrecondition, "the device login nobody could approve")
}

// TestSignInOnlyOnThePublicListener: the sign-in pages and SessionService
// are on the public listener alone; the admin socket has RevokeSessions and
// nothing else of them, and the node and edge sockets have none.
func TestSignInOnlyOnThePublicListener(t *testing.T) {
	w := newOIDCWorld(t)
	status := func(h http.Handler, method, path string) int {
		var body io.Reader
		if method == http.MethodPost {
			body = strings.NewReader("{}")
		}
		req := httptest.NewRequest(method, path, body)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	exchange := "/" + iamv1alpha1connect.SessionServiceName + "/ExchangeCode"
	revoke := "/" + iamv1alpha1connect.SessionServiceName + "/RevokeSessions"
	node := w.srv.Handler(auth.Chain{auth.PeerCred{NodeUIDs: []uint32{0}, NodeName: "n1"}})
	edge := w.srv.Handler(auth.Chain{auth.PeerEdge{UIDs: []uint32{1}, Host: "n1"}})
	admin := w.srv.AdminHandler(auth.Chain{auth.LocalRoot{Host: "n1", UID: 0}})
	for name, h := range map[string]http.Handler{"node": node, "edge": edge, "admin": admin} {
		for _, path := range []string{"/auth/login", "/auth/cli", "/auth/callback", "/auth/device"} {
			if got := status(h, http.MethodGet, path); got != http.StatusNotFound {
				t.Errorf("%s socket serves %s: %d", name, path, got)
			}
		}
		if got := status(h, http.MethodPost, exchange); got != http.StatusNotFound {
			t.Errorf("%s socket serves ExchangeCode: %d", name, got)
		}
	}
	for name, h := range map[string]http.Handler{"node": node, "edge": edge} {
		if got := status(h, http.MethodPost, revoke); got != http.StatusNotFound {
			t.Errorf("%s socket serves RevokeSessions: %d", name, got)
		}
	}
	// Here it is served, and refuses a caller the socket did not vouch for.
	if got := status(admin, http.MethodPost, revoke); got != http.StatusUnauthorized {
		t.Errorf("admin socket's RevokeSessions without a peer: %d, want 401", got)
	}
}

// TestAnonymousSignInsWriteNothing: starting a browser's or a CLI's sign-in
// keeps nothing on the server, so no flood of them can fill a cap; the rows
// that are kept, device logins and unexchanged CLI codes, are capped each on
// its own, so a flood of one blocks neither the other nor sign-in itself.
func TestAnonymousSignInsWriteNothing(t *testing.T) {
	w := newOIDCWorldWith(t, 3)
	alice := w.adminInvites("bob@example.com")
	ctx := t.Context()
	noFollow := w.browser()
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for i := range 20 {
		path := "/auth/login"
		if i%2 == 1 {
			path = "/auth/cli?" + url.Values{"redirect_uri": {"http://127.0.0.1:9/callback"}, "state": {"s"},
				"code_challenge": {oauth2.S256ChallengeFromVerifier("v")}, "code_challenge_method": {"S256"}}.Encode()
		}
		resp, err := noFollow.Get(w.url + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("start %d of %s: %d", i, path, resp.StatusCode)
		}
	}
	// Device logins fill their own cap...
	anon := w.sessions(headers{})
	for i := range 3 {
		if _, err := anon.StartDeviceLogin(ctx, connect.NewRequest(&iamv1alpha1.StartDeviceLoginRequest{})); err != nil {
			t.Fatalf("device login %d: %v", i, err)
		}
	}
	_, err := anon.StartDeviceLogin(ctx, connect.NewRequest(&iamv1alpha1.StartDeviceLoginRequest{}))
	wantCode(t, err, connect.CodeUnavailable, "a device login past its cap")
	// ...and sign-in still works, in a browser and for a CLI, up to the
	// CLI codes' own cap.
	if c := w.signIn(w.browser(), "bob@example.com"); c == "" {
		t.Fatal("a browser cannot sign in")
	}
	for i := range 3 {
		if got, _, _ := w.cliSignIn("bob@example.com"); got.Get("code") == "" {
			t.Fatalf("CLI sign-in %d: %v", i, got)
		}
	}
	for range 3 {
		got, _, _ := w.cliSignIn("bob@example.com")
		if got.Get("error") != "access_denied" || !strings.Contains(got.Get("error_description"), "too many") {
			t.Errorf("a CLI code past its cap: %v", got)
		}
	}
	// A refused CLI sign-in reserved no login: bob holds his browser
	// session and the three codes' reservations, nothing more.
	res := must[iamv1alpha1.RevokeSessionsResponse](t)(w.sessions(alice).RevokeSessions(ctx, connect.NewRequest(&iamv1alpha1.RevokeSessionsRequest{Email: "bob@example.com"})))
	if res.GetRevoked() != 4 {
		t.Errorf("bob held %d credentials; want 4: refused sign-ins kept reservations", res.GetRevoked())
	}
}

// TestDeviceLoginsCappedPerSource: one source, an IPv4 address or an IPv6
// /64, may have only so many device logins waiting, so it cannot fill the
// shared cap; other sources still start theirs, and a login that ends
// frees its source's place.
func TestDeviceLoginsCappedPerSource(t *testing.T) {
	w := newOIDCWorld(t)
	ctx := t.Context()
	start := func(peer string) (*iamv1alpha1.StartDeviceLoginResponse, error) {
		res, err := w.sessions(headers{testPeer: peer}).StartDeviceLogin(ctx, connect.NewRequest(&iamv1alpha1.StartDeviceLoginRequest{}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	fill := func(peers ...string) []*iamv1alpha1.StartDeviceLoginResponse {
		var ds []*iamv1alpha1.StartDeviceLoginResponse
		for i := range login.MaxDeviceLoginsPerSource {
			d, err := start(peers[i%len(peers)])
			if err != nil {
				t.Fatalf("device login %d from %v: %v", i, peers, err)
			}
			ds = append(ds, d)
		}
		return ds
	}
	first := fill("192.0.2.1")
	_, err := start("192.0.2.1")
	wantCode(t, err, connect.CodeResourceExhausted, "a device login past its source's cap")
	if _, err := start("192.0.2.2"); err != nil {
		t.Errorf("another IPv4 address: %v", err)
	}

	// An IPv6 source is its /64.
	fill("2001:db8:1:2::1", "2001:db8:1:2:aaaa::9")
	_, err = start("2001:db8:1:2:ffff:ffff:ffff:ffff")
	wantCode(t, err, connect.CodeResourceExhausted, "a device login from a /64 at its cap")
	if _, err := start("2001:db8:1:3::1"); err != nil {
		t.Errorf("another /64: %v", err)
	}

	// A login that ends, denied or approved, once its CLI has heard, no
	// longer counts: each frees one place, and only one.
	c := w.browser()
	w.signInAdmin(c, "alice@example.com")
	decide := func(code, decision string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, w.url+"/auth/device", strings.NewReader(url.Values{"user_code": {code}, "decision": {decision}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", w.url)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", decision, resp.StatusCode)
		}
	}
	poll := func(d *iamv1alpha1.StartDeviceLoginResponse) error {
		_, err := w.sessions(headers{}).PollDeviceLogin(ctx, connect.NewRequest(&iamv1alpha1.PollDeviceLoginRequest{DeviceCode: d.GetDeviceCode()}))
		return err
	}
	decide(first[0].GetUserCode(), "deny")
	wantCode(t, poll(first[0]), connect.CodePermissionDenied, "the denied device login")
	if _, err := start("192.0.2.1"); err != nil {
		t.Errorf("a device login after a denied one from the source ended: %v", err)
	}
	decide(first[1].GetUserCode(), "approve")
	if err := poll(first[1]); err != nil {
		t.Fatalf("the approved device login: %v", err)
	}
	if _, err := start("192.0.2.1"); err != nil {
		t.Errorf("a device login after an approved one from the source ended: %v", err)
	}
	_, err = start("192.0.2.1")
	wantCode(t, err, connect.CodeResourceExhausted, "a device login past its source's cap again")
}

// TestDeviceLoginsOnTheHostEachCount: an address in the host's own /64,
// as a node reports it, is a source by itself, as the front door counts
// it, so one machine cannot block device login for the others; any other
// /64 is still one source.
func TestDeviceLoginsOnTheHostEachCount(t *testing.T) {
	w := newOIDCWorldConfig(t, 0, func(c *apiserver.Config) { c.Nodes = []string{"n1"} })
	ctx := t.Context()
	w.nodeReportsPrefix("2001:db8:0:1::/64")
	start := func(peer string) error {
		_, err := w.sessions(headers{testPeer: peer}).StartDeviceLogin(ctx, connect.NewRequest(&iamv1alpha1.StartDeviceLoginRequest{}))
		return err
	}
	for i := range login.MaxDeviceLoginsPerSource {
		if err := start("2001:db8:0:1::a"); err != nil {
			t.Fatalf("device login %d from a machine: %v", i, err)
		}
	}
	wantCode(t, start("2001:db8:0:1::a"), connect.CodeResourceExhausted, "a machine past its own cap")
	if err := start("2001:db8:0:1::b"); err != nil {
		t.Errorf("another machine on the host: %v", err)
	}
	for i := range login.MaxDeviceLoginsPerSource {
		if err := start(fmt.Sprintf("2001:db8:0:2::%x", i+1)); err != nil {
			t.Fatalf("device login %d from another /64: %v", i, err)
		}
	}
	wantCode(t, start("2001:db8:0:2::ff"), connect.CodeResourceExhausted, "another /64, one source, past its cap")
}

// asNodeAuth authenticates every request as the node n1.
type asNodeAuth struct{}

func (asNodeAuth) Authenticate(context.Context, http.Header) (auth.Principal, bool, error) {
	return auth.Principal{Type: auth.Node, ID: "n1"}, true, nil
}

// nodeReportsPrefix reports n1's IPv6 prefix, as its agent does, once the
// apiserver has made its Node.
func (w *oidcWorld) nodeReportsPrefix(prefix string) {
	w.t.Helper()
	ts := httptest.NewServer(w.srv.Handler(auth.Chain{asNodeAuth{}}))
	defer ts.Close()
	nodes := computev1alpha1connect.NewNodeServiceClient(ts.Client(), ts.URL)
	testutil.Eventually(w.t, "the node reports its prefix", func() bool {
		_, err := nodes.UpdateNodeStatus(w.t.Context(), connect.NewRequest(&computev1alpha1.UpdateNodeStatusRequest{
			Name: "n1", Status: &computev1alpha1.NodeStatus{Ipv6Prefix: prefix},
		}))
		return err == nil
	})
}

// failingSwaps is a credentials store whose SwapAuthRequest fails while
// fail is set.
type failingSwaps struct {
	store.Credentials
	fail atomic.Bool
}

func (s *failingSwaps) SwapAuthRequest(ctx context.Context, key string, old, data []byte) error {
	if s.fail.Load() {
		return errors.New("the store failed")
	}
	return s.Credentials.SwapAuthRequest(ctx, key, old, data)
}

// TestFailedDeviceLoginsHoldNoPlace: a device login that fails to start,
// after its row was kept, frees its source's place and its place under the
// shared cap, here no larger than the source's.
func TestFailedDeviceLoginsHoldNoPlace(t *testing.T) {
	fs := &failingSwaps{}
	w := newOIDCWorldCreds(t, login.MaxDeviceLoginsPerSource, nil, func(c store.Credentials) store.Credentials {
		fs.Credentials = c
		return fs
	})
	anon := w.sessions(headers{testPeer: "192.0.2.1"})
	start := func() error {
		_, err := anon.StartDeviceLogin(t.Context(), connect.NewRequest(&iamv1alpha1.StartDeviceLoginRequest{}))
		return err
	}
	fs.fail.Store(true)
	for i := range login.MaxDeviceLoginsPerSource {
		if start() == nil {
			t.Fatalf("device login %d started on a failing store", i)
		}
	}
	fs.fail.Store(false)
	if err := start(); err != nil {
		t.Errorf("a device login after %d failed ones from the source: %v", login.MaxDeviceLoginsPerSource, err)
	}
}

// TestDeviceLoginsCappedPerSourceAtOnce: of many device logins one source
// starts at once, exactly its cap are kept.
func TestDeviceLoginsCappedPerSourceAtOnce(t *testing.T) {
	w := newOIDCWorld(t)
	anon := w.sessions(headers{testPeer: "192.0.2.1"})
	const n = 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[connect.Code]int{}
	for range n {
		wg.Go(func() {
			_, err := anon.StartDeviceLogin(t.Context(), connect.NewRequest(&iamv1alpha1.StartDeviceLoginRequest{}))
			mu.Lock()
			codes[connect.CodeOf(err)]++
			mu.Unlock()
		})
	}
	wg.Wait()
	// connect.CodeOf(nil) is Unknown.
	if codes[connect.CodeUnknown] != login.MaxDeviceLoginsPerSource || codes[connect.CodeResourceExhausted] != n-login.MaxDeviceLoginsPerSource {
		t.Errorf("%d device logins at once from one source: %v; want %d started and the rest resource_exhausted", n, codes, login.MaxDeviceLoginsPerSource)
	}
}

// TestDeviceCodeGuessesAreLimited: one browser session may try only so many
// device codes a minute.
func TestDeviceCodeGuessesAreLimited(t *testing.T) {
	w := newOIDCWorld(t)
	c := w.browser()
	w.signInAdmin(c, "alice@example.com")
	last := 0
	for i := range 11 {
		last, _, _ = get(t, c, fmt.Sprintf("%s/auth/device?user_code=BBBB-BB%02d", w.url, i))
		if i < 10 && last != http.StatusNotFound {
			t.Fatalf("guess %d: %d", i, last)
		}
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("the 11th guess in a minute: %d, want 429", last)
	}
}

// TestCookieWebSocketNeedsTheOrigin: an exec or ssh upgrade a browser's
// cookie authenticates is refused from a foreign origin, before anything
// about the machine is looked at.
func TestCookieWebSocketNeedsTheOrigin(t *testing.T) {
	w := newOIDCWorld(t)
	cookie := w.signInAdmin(w.browser(), "alice@example.com")
	for _, action := range []string{"exec", "ssh"} {
		for _, origin := range []string{"https://evil.example", w.url} {
			req, _ := http.NewRequest(http.MethodGet, w.url+"/v1/projects/p/containers/c:"+action, nil)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			req.Header.Set("Cookie", session.CookieName+"="+cookie)
			req.Header.Set("Origin", origin)
			resp, err := w.tls.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			foreign := origin != w.url
			if foreign && resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s from %s: %d, want 401", action, origin, resp.StatusCode)
			}
			if !foreign && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusSwitchingProtocols) {
				t.Errorf("%s from the console: %d, want past authentication and refused for the missing machine", action, resp.StatusCode)
			}
		}
	}
}

// TestRevokeEndsApprovedLogins: a login approved but not yet claimed, a
// device login waiting for its poll or a CLI code not yet exchanged, ends
// with its person's logins: what it would claim afterwards is refused.
func TestRevokeEndsApprovedLogins(t *testing.T) {
	w := newOIDCWorld(t)
	alice := w.adminInvites("bob@example.com")
	ctx := t.Context()
	anon := w.sessions(headers{})
	bobBrowser := w.browser()
	bob := w.asCookie(w.signIn(bobBrowser, "bob@example.com"))

	t.Run("a device login, revoked by an administrator", func(t *testing.T) {
		d := must[iamv1alpha1.StartDeviceLoginResponse](t)(anon.StartDeviceLogin(ctx, connect.NewRequest(&iamv1alpha1.StartDeviceLoginRequest{})))
		req, _ := http.NewRequest(http.MethodPost, w.url+"/auth/device", strings.NewReader(url.Values{"user_code": {d.GetUserCode()}, "decision": {"approve"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", w.url)
		resp, err := bobBrowser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("approve: %d", resp.StatusCode)
		}
		must[iamv1alpha1.RevokeSessionsResponse](t)(w.sessions(alice).RevokeSessions(ctx, connect.NewRequest(&iamv1alpha1.RevokeSessionsRequest{Email: "bob@example.com"})))
		_, err = anon.PollDeviceLogin(ctx, connect.NewRequest(&iamv1alpha1.PollDeviceLoginRequest{DeviceCode: d.GetDeviceCode()}))
		wantCode(t, err, connect.CodeUnauthenticated, "polling a device login approved before the revocation")
	})
	t.Run("a CLI code, revoked by its person", func(t *testing.T) {
		bob = w.asCookie(w.signIn(bobBrowser, "bob@example.com"))
		got, verifier, l := w.cliSignIn("bob@example.com")
		must[iamv1alpha1.RevokeSessionsResponse](t)(w.sessions(bob).RevokeSessions(ctx, connect.NewRequest(&iamv1alpha1.RevokeSessionsRequest{})))
		_, err := anon.ExchangeCode(ctx, connect.NewRequest(&iamv1alpha1.ExchangeCodeRequest{Code: got.Get("code"), CodeVerifier: verifier, RedirectUri: l.url}))
		wantCode(t, err, connect.CodeUnauthenticated, "exchanging a code issued before the revocation")
	})
	t.Run("an approved login claimed with nothing revoked still works", func(t *testing.T) {
		got, verifier, l := w.cliSignIn("bob@example.com")
		res, err := anon.ExchangeCode(ctx, connect.NewRequest(&iamv1alpha1.ExchangeCodeRequest{Code: got.Get("code"), CodeVerifier: verifier, RedirectUri: l.url}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.whoami(asBearer(res.Msg.GetTokens().GetAccessToken())); err != nil {
			t.Error(err)
		}
	})
}

// TestAdminSocketRevokes: root on the host, through the admin socket,
// revokes someone's logins.
func TestAdminSocketRevokes(t *testing.T) {
	w := newOIDCWorld(t)
	w.adminInvites("bob@example.com")
	ctx := t.Context()
	got, verifier, l := w.cliSignIn("bob@example.com")
	res, err := w.sessions(headers{}).ExchangeCode(ctx, connect.NewRequest(&iamv1alpha1.ExchangeCodeRequest{Code: got.Get("code"), CodeVerifier: verifier, RedirectUri: l.url}))
	if err != nil {
		t.Fatal(err)
	}
	bob := asBearer(res.Msg.GetTokens().GetAccessToken())

	sock := filepath.Join(t.TempDir(), "admin.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	root := auth.Chain{auth.LocalRoot{Host: "n1", UID: uint32(os.Getuid())}}
	admin := &http.Server{Handler: w.srv.AdminHandler(root), ConnContext: auth.AdminConnContext}
	go func() { _ = admin.Serve(ln) }()
	t.Cleanup(func() { _ = admin.Close() })

	sessions := iamv1alpha1connect.NewSessionServiceClient(client.UnixSocket(sock), client.UnixSocketURL)
	out := must[iamv1alpha1.RevokeSessionsResponse](t)(sessions.RevokeSessions(ctx, connect.NewRequest(&iamv1alpha1.RevokeSessionsRequest{Email: "bob@example.com"})))
	if out.GetRevoked() < 2 {
		t.Errorf("revoked %d credentials", out.GetRevoked())
	}
	_, err = w.whoami(bob)
	wantCode(t, err, connect.CodeUnauthenticated, "bob's login after root revoked it")
	_, err = sessions.RevokeSessions(ctx, connect.NewRequest(&iamv1alpha1.RevokeSessionsRequest{}))
	wantCode(t, err, connect.CodeInvalidArgument, "root revoking its own logins, which it has none of")
}
