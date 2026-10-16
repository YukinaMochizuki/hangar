package edge_test

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1/networkv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/auth/login"
	"github.com/YukinaMochizuki/hangar/internal/auth/oidctest"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/edge"
)

// handoffWorld is a world whose apiserver signs people in through a fake
// provider, with the console and the edge each on TLS, as the front door
// serves them.
type handoffWorld struct {
	*world
	provider *oidctest.Provider
	console  *httptest.Server
	edgeTLS  *httptest.Server
	auth     networkv1alpha1connect.EndpointAuthServiceClient
}

func newHandoffWorld(t *testing.T) *handoffWorld {
	t.Helper()
	return newHandoffWorldLogging(t, io.Discard)
}

// newHandoffWorldLogging is newHandoffWorld with the apiserver and the edge
// logging everything, debug included, to log.
func newHandoffWorldLogging(t *testing.T, log io.Writer) *handoffWorld {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := oidctest.New(t)
	env := apiservertest.StartOIDC(t, func(c *apiserver.Config) {
		c.PublicEndpoints.Domain = domain
		c.Logger = logger
	}, apiserver.OIDCConfig{
		Issuer: p.URL, ClientID: p.ClientID, ClientSecret: token.Secret(p.ClientSecret), PublicURL: consoleURL,
	})
	w := newWorldLogging(t, env, logger)
	console := httptest.NewTLSServer(env.Public)
	t.Cleanup(console.Close)
	edgeTLS := httptest.NewTLSServer(w.edge)
	t.Cleanup(edgeTLS.Close)
	p.SignIn(apiservertest.Root, true)
	return &handoffWorld{
		world: w, provider: p, console: console, edgeTLS: edgeTLS,
		auth: networkv1alpha1connect.NewEndpointAuthServiceClient(client.UnixSocket(env.EdgeSocket), client.UnixSocketURL),
	}
}

// browser is a fresh browser: the console's name reaches the console, the
// endpoints' the edge, the provider its own address. It records the URL of
// every request it makes, and stops at a URL stop says to.
func (w *handoffWorld) browser(visited *[]*url.URL, stop func(*url.URL) bool) *http.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test servers' own certificates
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h, _, _ := net.SplitHostPort(addr)
			switch {
			case h == "console.cp.test":
				addr = w.console.Listener.Addr().String()
			case strings.HasSuffix(h, "."+domain):
				addr = w.edgeTLS.Listener.Addr().String()
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	jar, _ := cookiejar.New(nil)
	return &http.Client{Transport: tr, Jar: jar, CheckRedirect: func(r *http.Request, _ []*http.Request) error {
		if visited != nil {
			*visited = append(*visited, r.URL)
		}
		if stop != nil && stop(r.URL) {
			return http.ErrUseLastResponse
		}
		return nil
	}}
}

// page is what a browser ended at, its body read and closed: the last
// response's status and header, and its URL.
type page struct {
	StatusCode int
	Header     http.Header
	URL        *url.URL
}

// Location is where the page redirects, resolved against its URL.
func (p *page) Location() (*url.URL, error) {
	return (&http.Response{Header: p.Header, Request: &http.Request{URL: p.URL}}).Location()
}

// load has the browser get u, and returns where it ended and the body.
func load(t *testing.T, b *http.Client, u string) (*page, string) {
	t.Helper()
	resp, err := b.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return &page{StatusCode: resp.StatusCode, Header: resp.Header, URL: resp.Request.URL}, string(body)
}

// cookiesOf returns the names of the browser's cookies for host.
func cookiesOf(b *http.Client, h string) map[string]bool {
	out := map[string]bool{}
	for _, c := range b.Jar.Cookies(&url.URL{Scheme: "https", Host: h, Path: "/"}) {
		out[c.Name] = true
	}
	return out
}

// TestAHandoffEndToEnd: a browser opening a private endpoint signs in at
// the provider through the console, and comes back to the very page it
// asked for with a session of that host alone; on another endpoint it is
// handed off again without seeing the provider; signing out of the console
// ends its endpoint sessions.
func TestAHandoffEndToEnd(t *testing.T) {
	w := newHandoffWorld(t)
	w.endpoint(8080, private)
	w.applyNamed("other", private)
	w.served(w.rootCookie())

	var visited []*url.URL
	b := w.browser(&visited, nil)
	resp, body := load(t, b, "https://"+host+"/app/page?x=1")
	if resp.URL.String() != "https://"+host+"/app/page?x=1" || resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, `port 8080 path="/app/page"`) {
		t.Fatalf("after the handoff, at %s: %d %q; through %v", resp.URL, resp.StatusCode, body, visited)
	}
	if strings.Contains(body, "hge_") || strings.Contains(body, "hgs_") {
		t.Errorf("the endpoint saw a Hangar credential: %s", body)
	}
	if c := cookiesOf(b, host); !c[edge.EndpointCookie] || len(c) != 1 {
		t.Errorf("the browser's cookies on the endpoint after the handoff: %v; want the session alone", c)
	}
	if c := cookiesOf(b, otherHost); len(c) != 0 {
		t.Errorf("cookies on another endpoint: %v", c)
	}
	sawProvider := func() bool {
		for _, u := range visited {
			if strings.HasPrefix(u.String(), w.provider.URL) {
				return true
			}
		}
		return false
	}
	if !sawProvider() {
		t.Errorf("signed in without the provider: %v", visited)
	}

	// Signed in: the next request goes straight through.
	visited = nil
	if resp, body := load(t, b, "https://"+host+"/next"); resp.StatusCode != http.StatusOK || len(visited) != 0 || !strings.HasPrefix(body, `port 8080 path="/next"`) {
		t.Errorf("the next request: %d %q through %v", resp.StatusCode, body, visited)
	}
	// Another endpoint: a handoff of its own, silently, with the console's
	// session.
	if resp, body := load(t, b, "https://"+otherHost+"/"); resp.StatusCode != http.StatusOK || sawProvider() || resp.URL.Host != otherHost {
		t.Errorf("another endpoint: %d %q at %s through %v", resp.StatusCode, body, resp.URL, visited)
	}

	// Signing out of the console ends both endpoint sessions.
	req, _ := http.NewRequest(http.MethodPost, consoleURL+login.PathLogout, nil)
	req.Header.Set("Origin", consoleURL)
	out, err := b.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = out.Body.Close()
	if out.StatusCode != http.StatusNoContent {
		t.Fatalf("signing out: %d", out.StatusCode)
	}
	for _, h := range []string{host, otherHost} {
		req, _ := http.NewRequest(http.MethodGet, "https://"+h+"/", nil)
		req.Header.Set("Sec-Fetch-Mode", "cors")
		resp, err := b.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s after signing out of the console: %d", h, resp.StatusCode)
		}
	}
}

// TestAHandoffCodeIsTakenOnce: the code a browser brings back counts once,
// for its host, and only from the browser that started the handoff.
func TestAHandoffCodeIsTakenOnce(t *testing.T) {
	w := newHandoffWorld(t)
	w.endpoint(8080, private)
	w.applyNamed("other", private)
	w.served(w.rootCookie())
	atCallback := func(u *url.URL) bool { return u.Path == login.EndpointCallbackPath }

	// handoff runs a handoff in b up to the callback, and returns it.
	handoff := func(b *http.Client) *url.URL {
		t.Helper()
		resp, body := load(t, b, "https://"+host+"/")
		loc, err := resp.Location()
		if resp.StatusCode != http.StatusSeeOther || err != nil || loc.Path != login.EndpointCallbackPath || loc.Host != host {
			t.Fatalf("the handoff stopped at %d %q, Location %v", resp.StatusCode, body, loc)
		}
		return loc
	}
	exchange := func(cb *url.URL, h string) error {
		_, err := w.auth.ExchangeEndpointCode(t.Context(), connect.NewRequest(&networkv1alpha1.ExchangeEndpointCodeRequest{
			Code: cb.Query().Get("code"), Host: h, State: cb.Query().Get("state"),
		}))
		return err
	}
	finish := func(b *http.Client, cb *url.URL) (*page, string) {
		t.Helper()
		resp, body := load(t, b, cb.String())
		return resp, body
	}

	b := w.browser(nil, atCallback)
	cb := handoff(b)
	if err := exchange(cb, otherHost); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("the code for another host: %v", err)
	}
	if resp, body := finish(b, cb); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "used already or has expired") {
		t.Errorf("the code after a try for another host: %d %q", resp.StatusCode, body)
	}

	cb = handoff(b)
	forged := *cb
	q := forged.Query()
	q.Set("code", "never-issued")
	forged.RawQuery = q.Encode()
	if resp, body := finish(b, &forged); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "used already or has expired") {
		t.Errorf("a code never issued: %d %q", resp.StatusCode, body)
	}

	cb = handoff(b)
	// Another browser, which did not start this handoff, brings the code.
	thief := w.browser(nil, atCallback)
	if resp, body := finish(thief, cb); resp.StatusCode != http.StatusBadRequest || cookiesOf(thief, host)[edge.EndpointCookie] {
		t.Errorf("another browser bringing the code: %d %q", resp.StatusCode, body)
	}
	// The browser that started it gets in, once, sent on with no referrer,
	// the callback's URL holding the code.
	once := &http.Client{Transport: b.Transport, Jar: b.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, _ := load(t, once, cb.String()); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Referrer-Policy") != "no-referrer" || resp.Header.Get("Location") != "/" {
		t.Errorf("the callback: %d, Referrer-Policy %q, Location %q", resp.StatusCode, resp.Header.Get("Referrer-Policy"), resp.Header.Get("Location"))
	}
	if resp, body := load(t, b, "https://"+host+"/"); resp.StatusCode != http.StatusOK || resp.URL.String() != "https://"+host+"/" ||
		!strings.HasPrefix(body, "port 8080") || !cookiesOf(b, host)[edge.EndpointCookie] {
		t.Errorf("the code in its own browser: %d at %s: %q", resp.StatusCode, resp.URL, body)
	}
	if err := exchange(cb, host); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("the code used again: %v", err)
	}
}

// TestAHandoffReturnsToTheEndpointAlone: whatever the path a browser asked
// for, the handoff brings it back to a path of that endpoint, never to
// another site.
func TestAHandoffReturnsToTheEndpointAlone(t *testing.T) {
	w := newHandoffWorld(t)
	w.endpoint(8080, private)
	w.served(w.rootCookie())
	for _, p := range []string{"//evil.example/", "///evil.example/x", "/%2F%2Fevil.example%2F", "/%5Cevil.example", "/%09/evil.example", "/x?next=https://evil.example"} {
		var visited []*url.URL
		b := w.browser(&visited, nil)
		resp, body := load(t, b, "https://"+host+p)
		if resp.URL.Host != host || resp.StatusCode != http.StatusOK {
			t.Errorf("%s: ended at %s: %d %q", p, resp.URL, resp.StatusCode, body)
		}
		for _, u := range visited {
			if strings.Contains(u.Host, "evil") {
				t.Errorf("%s: went to %s", p, u)
			}
		}
	}
}

// syncBuffer is a log both servers write at once.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
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

// TestAHandoffLogsNoSecret: neither the apiserver nor the edge, logging
// everything, writes a handoff's code or state, or a session, through a
// handoff that works and one refused; and both log who signed in.
func TestAHandoffLogsNoSecret(t *testing.T) {
	logs := &syncBuffer{}
	w := newHandoffWorldLogging(t, logs)
	w.endpoint(8080, private)
	w.served(w.rootCookie())

	var visited []*url.URL
	b := w.browser(&visited, nil)
	if resp, body := load(t, b, "https://"+host+"/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("the handoff: %d %q", resp.StatusCode, body)
	}
	var secrets []string
	for _, u := range visited {
		for _, k := range []string{"code", "state"} {
			if v := u.Query().Get(k); len(v) > 8 {
				secrets = append(secrets, v)
			}
		}
	}
	for _, c := range b.Jar.Cookies(&url.URL{Scheme: "https", Host: host, Path: "/"}) {
		secrets = append(secrets, c.Value)
	}
	for _, c := range b.Jar.Cookies(&url.URL{Scheme: "https", Host: "console.cp.test", Path: "/"}) {
		secrets = append(secrets, c.Value)
	}
	if len(secrets) < 4 {
		t.Fatalf("found %d secrets to look for, through %v", len(secrets), visited)
	}
	// A refused callback, with a code of its own.
	cb := w.browser(nil, func(u *url.URL) bool { return u.Path == login.EndpointCallbackPath })
	resp, _ := load(t, cb, "https://"+host+"/")
	loc, _ := resp.Location()
	secrets = append(secrets, loc.Query().Get("code"))
	q := loc.Query()
	q.Set("code", "x"+q.Get("code"))
	loc.RawQuery = q.Encode()
	if resp, _ := load(t, cb, loc.String()); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a code changed: %d", resp.StatusCode)
	}

	got := logs.String()
	for _, s := range secrets {
		if strings.Contains(got, s) {
			t.Errorf("a secret %.6s... is in the logs", s)
		}
	}
	for _, p := range token.Prefixes {
		if strings.Contains(got, p) && !strings.Contains(got, p+"[redacted]") {
			t.Errorf("a %s token is in the logs", p)
		}
	}
	for _, want := range []string{"handed a browser off to a private endpoint", "signed in on a private endpoint", "refused a handoff code"} {
		if !strings.Contains(got, want) {
			t.Errorf("the logs say nothing of %q", want)
		}
	}
}

// TestAHandoffForSomeoneNoLongerAllowed: a person whose grant goes between
// endpoint-login and the callback gets no session, and a page saying they
// may no longer use the endpoint, not that the link was bad.
func TestAHandoffForSomeoneNoLongerAllowed(t *testing.T) {
	w := newHandoffWorld(t)
	w.endpoint(8080, private)
	w.served(w.rootCookie())
	w.env.AddMember("bob@example.com", "platform.member")
	if _, err := w.root.Apply(context.Background(), registry.RoleBinding,
		binding("bob-owner", "tenant.owner", &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: apiservertest.Tenant}})); err != nil {
		t.Fatal(err)
	}
	w.provider.SignIn("bob@example.com", true)
	b := w.browser(nil, func(u *url.URL) bool { return u.Path == login.EndpointCallbackPath })
	resp, body := load(t, b, "https://"+host+"/")
	cb, err := resp.Location()
	if resp.StatusCode != http.StatusSeeOther || err != nil || cb.Path != login.EndpointCallbackPath {
		t.Fatalf("bob's handoff stopped at %d %q", resp.StatusCode, body)
	}
	if _, err := w.root.Delete(context.Background(), registry.RoleBinding, client.Key{Name: "bob-owner"}); err != nil {
		t.Fatal(err)
	}
	resp, body = load(t, b, cb.String())
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "may no longer use this endpoint") || cookiesOf(b, host)[edge.EndpointCookie] {
		t.Errorf("the callback after bob's grant went: %d %q", resp.StatusCode, body)
	}
}
