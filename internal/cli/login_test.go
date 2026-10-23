package cli_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"sigs.k8s.io/yaml"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/auth/oidctest"
	"github.com/YukinaMochizuki/hangar/internal/auth/session"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
	"github.com/YukinaMochizuki/hangar/internal/cli"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// oidcCLI is an apiserver in oidc auth over TLS, with a fake provider, and
// a CLI config for it. The CLI's browser is a cookie jar that follows
// redirects, and its clock can be moved ahead.
type oidcCLI struct {
	t        *testing.T
	url      string
	provider *oidctest.Provider
	tls      *http.Client
	config   string
	browser  *http.Client

	mu  sync.Mutex
	now time.Time
}

func newOIDCCLI(t *testing.T) *oidcCLI {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hangar.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := apiserver.New(apiserver.Config{Store: st, BootstrapAdmin: "alice@example.com", DefaultPlanLimits: &tenancyv1alpha1.Limits{}})
	var handler http.Handler
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	ts.StartTLS()
	p := oidctest.New(t)
	chain, err := srv.EnableOIDC(apiserver.OIDCConfig{
		Issuer: p.URL, ClientID: p.ClientID, ClientSecret: token.Secret(p.ClientSecret), PublicURL: ts.URL, DeviceInterval: time.Second,
	}, st)
	if err != nil {
		t.Fatal(err)
	}
	handler = srv.PublicHandler(chain)
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		ts.Close()
		cancel()
		<-done
		_ = st.Close()
	})
	w := &oidcCLI{t: t, url: ts.URL, provider: p, tls: ts.Client(), config: filepath.Join(t.TempDir(), "hangar", "config.yaml"), now: time.Now()}
	w.newBrowser()
	if r := w.run("config", "set-context", "prod", "--endpoint", ts.URL, "--use"); r.code != 0 {
		t.Fatal(r.err)
	}
	return w
}

func (w *oidcCLI) newBrowser() {
	jar, _ := cookiejar.New(nil)
	w.browser = &http.Client{Transport: w.tls.Transport, Jar: jar}
}

func (w *oidcCLI) advance(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = w.now.Add(d)
}

func (w *oidcCLI) clock() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now
}

// open is the CLI's browser: it loads the URL, following every redirect,
// back to the CLI's loopback listener.
func (w *oidcCLI) open(u string) error {
	go func() {
		resp, err := w.browser.Get(u)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	return nil
}

// syncBuffer is a buffer the CLI writes while the test reads it.
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

func (w *oidcCLI) runWith(errb *syncBuffer, args ...string) result {
	var out syncBuffer
	if errb == nil {
		errb = &syncBuffer{}
	}
	code := cli.Run(context.Background(), args, cli.IO{
		In: strings.NewReader(""), Out: &out, Err: errb,
		Env: func(k string) string {
			if k == "HANGAR_CONFIG" {
				return w.config
			}
			return ""
		},
	}, cli.Options{HTTPClient: w.tls, OpenBrowser: w.open, Now: w.clock})
	return result{code, out.String(), errb.String()}
}

func (w *oidcCLI) run(args ...string) result { return w.runWith(nil, args...) }

func (w *oidcCLI) credentialsPath() string {
	return filepath.Join(filepath.Dir(w.config), "credentials.yaml")
}

type storedLogin struct {
	Email         string    `json:"email"`
	AccessToken   string    `json:"accessToken"`
	AccessExpires time.Time `json:"accessExpires"`
	RefreshToken  string    `json:"refreshToken"`
}

func (w *oidcCLI) stored() map[string]storedLogin {
	w.t.Helper()
	b, err := os.ReadFile(w.credentialsPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		w.t.Fatal(err)
	}
	var f struct {
		Logins map[string]storedLogin `json:"logins"`
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		w.t.Fatal(err)
	}
	return f.Logins
}

// tokenLike matches anything shaped like a Hangar token.
var tokenLike = regexp.MustCompile(`hg[a-z]_[A-Za-z0-9_-]{20,}`)

// noSecrets fails if the CLI printed a token, or any of the given secrets.
func (w *oidcCLI) noSecrets(r result, secrets ...string) {
	w.t.Helper()
	all := r.out + r.err
	if m := tokenLike.FindString(all); m != "" {
		w.t.Errorf("printed a token %.8s…:\n%s", m, all)
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(all, s) {
			w.t.Errorf("printed a secret %.8s…:\n%s", s, all)
		}
	}
}

// login runs hangar login until the bootstrap admin's grant has landed,
// for alice, or once, for anyone else.
func (w *oidcCLI) login(email string, args ...string) result {
	w.t.Helper()
	w.provider.SignIn(email, true)
	var r result
	testutil.Eventually(w.t, email+" logged in", func() (bool, string) {
		r = w.run(append([]string{"login"}, args...)...)
		return r.code == 0 || email != "alice@example.com", r.err
	})
	return r
}

// TestLogin: hangar login signs in through the browser and keeps the login
// in a file only its owner reads, apart from the config; commands then use
// it; nothing ever prints a token.
func TestLogin(t *testing.T) {
	w := newOIDCCLI(t)
	r := w.login("alice@example.com")
	if r.code != 0 || r.out != "logged in to "+w.url+" as alice@example.com\n" {
		t.Fatalf("login: exit %d\n%s%s", r.code, r.out, r.err)
	}
	if !strings.Contains(r.err, w.url+"/auth/cli?") {
		t.Errorf("login did not show the URL to open:\n%s", r.err)
	}
	w.noSecrets(r)
	if !isPrivate(t, w.credentialsPath()) {
		t.Fatalf("credentials file not private: %s", accessOf(t, w.credentialsPath()))
	}
	if dir := filepath.Dir(w.credentialsPath()); !isPrivate(t, dir) {
		t.Errorf("credentials directory not private: %s", accessOf(t, dir))
	}
	l := w.stored()[w.url]
	if !strings.HasPrefix(l.AccessToken, token.Access) || !strings.HasPrefix(l.RefreshToken, token.Refresh) || l.Email != "alice@example.com" {
		t.Fatalf("stored login %q %.4s… %.4s…", l.Email, l.AccessToken, l.RefreshToken)
	}
	cfg, _ := os.ReadFile(w.config)
	if strings.Contains(string(cfg), "hg") {
		t.Errorf("config.yaml holds a token:\n%s", cfg)
	}
	r = w.run("config", "view", "-o", "json")
	w.noSecrets(r, l.AccessToken, l.RefreshToken)

	r = w.run("whoami")
	if r.code != 0 || !strings.Contains(r.out, "alice@example.com") {
		t.Fatalf("whoami: exit %d\n%s%s", r.code, r.out, r.err)
	}
	w.noSecrets(r, l.AccessToken, l.RefreshToken)
}

// TestLoginRefreshes: an access token about to expire is renewed with the
// refresh token, which rotates; a stolen copy of a spent refresh token, used
// later, ends the login for everyone, and says so without printing it.
func TestLoginRefreshes(t *testing.T) {
	w := newOIDCCLI(t)
	if r := w.login("alice@example.com"); r.code != 0 {
		t.Fatal(r.err)
	}
	first := w.stored()[w.url]
	stolen, err := os.ReadFile(w.credentialsPath())
	if err != nil {
		t.Fatal(err)
	}

	w.advance(time.Hour)
	if r := w.run("whoami"); r.code != 0 {
		t.Fatalf("whoami after an hour: %s", r.err)
	}
	second := w.stored()[w.url]
	if second.RefreshToken == first.RefreshToken || second.AccessToken == first.AccessToken {
		t.Fatal("the CLI did not renew an expiring access token")
	}
	// Back at the server's time, the new access token is fresh and kept.
	w.advance(-time.Hour)
	if r := w.run("whoami"); r.code != 0 || w.stored()[w.url].RefreshToken != second.RefreshToken {
		t.Errorf("a fresh access token was renewed again: %s", r.err)
	}
	w.advance(time.Hour)

	// The thief's copy holds the spent refresh token.
	if err := os.WriteFile(w.credentialsPath(), stolen, 0o600); err != nil {
		t.Fatal(err)
	}
	w.advance(time.Hour)
	r := w.run("whoami")
	if r.code == 0 || !strings.Contains(r.err, "your login to "+w.url+" has ended") || !strings.Contains(r.err, "already used") {
		t.Fatalf("replaying a spent refresh token: exit %d\n%s", r.code, r.err)
	}
	w.noSecrets(r, first.RefreshToken, second.RefreshToken)
	if _, ok := w.stored()[w.url]; ok {
		t.Error("the ended login is still stored")
	}
	// The owner's newest tokens died with it.
	accounts := iamv1alpha1connect.NewAccountServiceClient(w.tls, w.url, connect.WithInterceptors(bearer(second.AccessToken)))
	_, err = accounts.WhoAmI(t.Context(), connect.NewRequest(&iamv1alpha1.WhoAmIRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("the newest access token after the replay: %v", err)
	}
	if r := w.run("whoami"); r.code == 0 || !strings.Contains(r.err, "not logged in to "+w.url+"; run hangar login") {
		t.Errorf("after the login ended: exit %d %s", r.code, r.err)
	}
}

// TestLoginRefusals: a sign-in the apiserver refuses fails the command with
// the reason; a credentials file others may read is refused.
func TestLoginRefusals(t *testing.T) {
	w := newOIDCCLI(t)
	if r := w.login("alice@example.com"); r.code != 0 {
		t.Fatal(r.err)
	}
	w.newBrowser()
	r := w.login("eve@example.com")
	if r.code == 0 || !strings.Contains(r.err, "eve@example.com has not been invited to Hangar") {
		t.Errorf("an uninvited login: exit %d\n%s", r.code, r.err)
	}
	w.noSecrets(r)

	openUp(t, w.credentialsPath())
	r = w.run("whoami")
	if r.code == 0 || !strings.Contains(r.err, fixHint) {
		t.Errorf("a credentials file others can read: exit %d %s", r.code, r.err)
	}
	w.noSecrets(r)
}

// TestLogout: hangar logout ends the login on the server and here; with
// --all, every login of the person ends, browsers' included.
func TestLogout(t *testing.T) {
	w := newOIDCCLI(t)
	if r := w.login("alice@example.com"); r.code != 0 {
		t.Fatal(r.err)
	}
	l := w.stored()[w.url]
	r := w.run("logout")
	if r.code != 0 || r.out != "logged out of "+w.url+"\n" {
		t.Fatalf("logout: exit %d %s%s", r.code, r.out, r.err)
	}
	w.noSecrets(r, l.AccessToken, l.RefreshToken)
	accounts := iamv1alpha1connect.NewAccountServiceClient(w.tls, w.url, connect.WithInterceptors(bearer(l.AccessToken)))
	if _, err := accounts.WhoAmI(t.Context(), connect.NewRequest(&iamv1alpha1.WhoAmIRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("the access token after logout: %v", err)
	}
	if r := w.run("logout"); r.code != 0 || !strings.Contains(r.out, "not logged in") {
		t.Errorf("logout twice: exit %d %s%s", r.code, r.out, r.err)
	}

	// A browser session of alice's, then logout --all from the CLI.
	if r := w.login("alice@example.com"); r.code != 0 {
		t.Fatal(r.err)
	}
	u, _ := url.Parse(w.url)
	var cookie string
	for _, ck := range w.browser.Jar.Cookies(u) {
		if ck.Name == session.CookieName {
			cookie = ck.Value
		}
	}
	if cookie == "" {
		resp, err := w.browser.Get(w.url + "/auth/login")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		for _, ck := range w.browser.Jar.Cookies(u) {
			if ck.Name == session.CookieName {
				cookie = ck.Value
			}
		}
	}
	r = w.run("logout", "--all")
	if r.code != 0 || !strings.Contains(r.out, "every login of alice@example.com there has ended") {
		t.Fatalf("logout --all: exit %d %s%s", r.code, r.out, r.err)
	}
	browser := iamv1alpha1connect.NewAccountServiceClient(w.tls, w.url, connect.WithInterceptors(
		headerSetter{"Cookie": session.CookieName + "=" + cookie, "Origin": w.url}))
	if _, err := browser.WhoAmI(t.Context(), connect.NewRequest(&iamv1alpha1.WhoAmIRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("the browser session after logout --all: %v", err)
	}
}

// TestLoginDevice: hangar login --device prints a code, which a person
// approves in a browser signed in elsewhere.
func TestLoginDevice(t *testing.T) {
	w := newOIDCCLI(t)
	if r := w.login("alice@example.com"); r.code != 0 {
		t.Fatal(r.err)
	}
	if r := w.run("logout"); r.code != 0 {
		t.Fatal(r.err)
	}
	var errb syncBuffer
	done := make(chan result, 1)
	go func() { done <- w.runWith(&errb, "login", "--device") }()
	complete := regexp.MustCompile(`\(or open (\S+)\)`)
	var page string
	testutil.Eventually(t, "a device URL printed", func() (bool, string) {
		out := errb.String()
		if m := complete.FindStringSubmatch(out); m != nil {
			page = m[1]
		}
		return page != "", "\n" + out
	})
	code, _ := url.Parse(page)
	// The browser is still signed in from the first login.
	resp, err := w.browser.Get(page)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), code.Query().Get("user_code")) {
		t.Fatalf("device page: %s", body)
	}
	req, _ := http.NewRequest(http.MethodPost, w.url+"/auth/device", strings.NewReader(url.Values{
		"user_code": {code.Query().Get("user_code")}, "decision": {"approve"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", w.url)
	resp, err = w.browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	r := <-done
	if r.code != 0 || r.out != "logged in to "+w.url+" as alice@example.com\n" {
		t.Fatalf("login --device: exit %d\n%s%s", r.code, r.out, r.err)
	}
	w.noSecrets(r)
	if r := w.run("whoami"); r.code != 0 {
		t.Errorf("whoami after a device login: %s", r.err)
	}
}

type headerSetter map[string]string

func (h headerSetter) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		for k, v := range h {
			req.Header().Set(k, v)
		}
		return next(ctx, req)
	}
}

func (h headerSetter) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (h headerSetter) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func bearer(access string) headerSetter { return headerSetter{"Authorization": "Bearer " + access} }

// TestConcurrentRefreshes: several hangar processes renewing one login at
// once take turns under the credentials file's lock, so only one spends the
// refresh token and none trips the reuse detection.
func TestConcurrentRefreshes(t *testing.T) {
	w := newOIDCCLI(t)
	if r := w.login("alice@example.com"); r.code != 0 {
		t.Fatal(r.err)
	}
	first := w.stored()[w.url]
	w.advance(time.Hour)
	var wg sync.WaitGroup
	results := make([]result, 6)
	for i := range results {
		wg.Go(func() { results[i] = w.run("whoami") })
	}
	wg.Wait()
	for i, r := range results {
		if r.code != 0 {
			t.Errorf("whoami %d: exit %d %s", i, r.code, r.err)
		}
	}
	after := w.stored()[w.url]
	if after.RefreshToken == "" || after.RefreshToken == first.RefreshToken {
		t.Fatalf("the login was not renewed, or was lost")
	}
	if r := w.run("whoami"); r.code != 0 {
		t.Errorf("the login after the concurrent renewals: %s", r.err)
	}
}

// TestConfigDirIsPrivate: the default config directory, the CLI's own, is
// made private even when it already existed open; one HANGAR_CONFIG names
// is left as it is, with a warning, and the credentials file in it is 0600
// either way.
func TestConfigDirIsPrivate(t *testing.T) {
	run := func(env map[string]string, args ...string) result {
		var out, errb bytes.Buffer
		code := cli.Run(context.Background(), args, cli.IO{
			In: strings.NewReader(""), Out: &out, Err: &errb, Env: func(k string) string { return env[k] },
		}, cli.Options{})
		return result{code, out.String(), errb.String()}
	}
	t.Run("the default directory", func(t *testing.T) {
		home := t.TempDir()
		dir := filepath.Join(home, "hangar")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		openUp(t, dir)
		r := run(map[string]string{configHome: home}, "config", "set-context", "prod", "--endpoint", "https://api.example.com", "--use")
		if r.code != 0 {
			t.Fatal(r.err)
		}
		if !isPrivate(t, dir) {
			t.Errorf("default config directory not private: %s", accessOf(t, dir))
		}
		if r.err != "" {
			t.Errorf("warned about the directory it made private: %s", r.err)
		}
	})
	t.Run("a directory HANGAR_CONFIG names", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "checkout")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		openUp(t, dir)
		before := accessOf(t, dir)
		r := run(map[string]string{"HANGAR_CONFIG": filepath.Join(dir, "config.yaml")}, "config", "set-context", "prod", "--endpoint", "https://api.example.com", "--use")
		if r.code != 0 {
			t.Fatal(r.err)
		}
		if after := accessOf(t, dir); after != before {
			t.Errorf("HANGAR_CONFIG's directory was changed: %s, was %s", after, before)
		}
		if !strings.Contains(r.err, "warning: "+dir) {
			t.Errorf("no warning about %s: %q", dir, r.err)
		}
		if cfg := filepath.Join(dir, "config.yaml"); !isPrivate(t, cfg) {
			t.Errorf("config.yaml not private: %s", accessOf(t, cfg))
		}
	})
}

// TestCloudflareAuthIsGone: a context still on cloudflare auth says how to
// move on.
func TestCloudflareAuthIsGone(t *testing.T) {
	w := &oidcCLI{t: t, config: filepath.Join(t.TempDir(), "config.yaml"), now: time.Now()}
	if err := os.WriteFile(w.config, []byte("currentContext: prod\ncontexts:\n  prod: {endpoint: https://api.example.com, auth: cloudflare}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := w.run("whoami")
	if r.code != cli.ExitUsage || !strings.Contains(r.err, "--auth oidc") {
		t.Errorf("exit %d: %s", r.code, r.err)
	}
}
