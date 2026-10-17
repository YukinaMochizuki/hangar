package edge_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/login"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
	"github.com/YukinaMochizuki/hangar/internal/edge"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

const (
	private = networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PRIVATE
	public  = networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PUBLIC
	// otherHost is endpoint other's host, which applyNamed makes.
	otherHost = "other-" + apiservertest.Project + "." + domain
)

// applyNamed applies endpoint name of project web on port 8080 with access.
func (w *world) applyNamed(name string, access networkv1alpha1.PublicEndpointAccess) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.PublicEndpoint, &networkv1alpha1.PublicEndpoint{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: name},
		Spec: &networkv1alpha1.PublicEndpointSpec{
			Target: &networkv1alpha1.PublicEndpointTarget{Object: &networkv1alpha1.PublicEndpointTarget_Container{Container: "api"}},
			Port:   8080, Access: access,
		},
	}); err != nil {
		w.t.Fatal(err)
	}
}

// serviceToken makes a service token of project web, bound to role unless
// it is empty, and returns its secret.
func (w *world) serviceToken(name, role string) token.Secret {
	w.t.Helper()
	c := iamv1alpha1connect.NewServiceTokenIssuerServiceClient(apiservertest.DevUser(apiservertest.Root), w.env.URL)
	resp, err := c.CreateServiceToken(context.Background(), connect.NewRequest(&iamv1alpha1.CreateServiceTokenRequest{
		Project: apiservertest.Project, Name: name, Role: role,
	}))
	if err != nil {
		w.t.Fatal(err)
	}
	return token.Secret(resp.Msg.GetSecret())
}

// served waits until the root's session on host gets through to the
// endpoint, as it does once the edge has read the endpoint.
func (w *world) served(h http.Header) {
	w.t.Helper()
	testutil.Eventually(w.t, "the endpoint served", func() (bool, string) {
		status, body := w.get(h)
		return status == http.StatusOK, fmt.Sprint(status, body)
	})
}

// TestWhoMayUseAPrivateEndpoint: a private endpoint takes the endpoint
// session of its own host and a Hangar CLI access token or service token,
// and then the grant decides. A browser with none is handed off to sign in;
// any other request with none, or with another credential, is refused.
func TestWhoMayUseAPrivateEndpoint(t *testing.T) {
	w := newWorld(t)
	w.endpoint(8080, private)
	w.applyNamed("other", private)
	root := w.rootCookie()
	w.served(root)

	ctx := context.Background()
	rootP := auth.Principal{Type: auth.User, ID: apiservertest.Root}
	cli, err := w.sessions.StartCLI(ctx, rootP)
	if err != nil {
		t.Fatal(err)
	}
	consoleSession, _ := w.consoleSession(apiservertest.Root, time.Time{})
	editor := w.serviceToken("deployer", "project.editor")
	nobody := w.serviceToken("idle", "")
	otherSession := w.endpointSession(apiservertest.Root, otherHost, time.Time{})
	stranger := w.endpointSession("mallory@example.com", host, time.Time{})

	for what, c := range map[string]struct {
		h    http.Header
		want int
	}{
		"the root's session":                   {root, http.StatusOK},
		"a CLI access token":                   {bearer(cli.Access), http.StatusOK},
		"a service token bound as an editor":   {bearer(editor), http.StatusOK},
		"a service token bound to nothing":     {bearer(nobody), http.StatusForbidden},
		"a stranger's session":                 {cookie(stranger), http.StatusForbidden},
		"a console session as a bearer":        {bearer(consoleSession), http.StatusUnauthorized},
		"a refresh token as a bearer":          {bearer(cli.Refresh), http.StatusUnauthorized},
		"an endpoint session as a bearer":      {bearer(otherSession), http.StatusUnauthorized},
		"an unknown service token":             {bearer(token.New(token.Service)), http.StatusUnauthorized},
		"no credential, from a script":         {http.Header{"Sec-Fetch-Mode": {"cors"}}, http.StatusUnauthorized},
		"a console session as the cookie":      {cookie(consoleSession), http.StatusFound},
		"another host's session":               {cookie(otherSession), http.StatusFound},
		"an unknown session":                   {cookie(token.New(token.Endpoint)), http.StatusFound},
		"no credential, from a browser":        {nil, http.StatusFound},
		"an Authorization of the endpoint's":   {http.Header{"Authorization": {"Basic YTpi"}}, http.StatusFound},
		"a bearer token of the endpoint's":     {http.Header{"Authorization": {"Bearer app-token"}}, http.StatusFound},
		"the session under a lookalike name":   {http.Header{"Cookie": {"__host-hangar-endpoint=" + root.Get("Cookie")[len(edge.EndpointCookie)+1:]}}, http.StatusFound},
		"the session under a name sans prefix": {http.Header{"Cookie": {"hangar-endpoint=" + root.Get("Cookie")[len(edge.EndpointCookie)+1:]}}, http.StatusFound},
	} {
		if status, body := w.get(c.h); status != c.want {
			t.Errorf("%s: %d %q; want %d", what, status, body, c.want)
		}
	}
	// A POST without a credential is no navigation: refused, not redirected.
	if resp, body := w.do(http.MethodPost, host, "/", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a POST with no credential: %d %q", resp.StatusCode, body)
	}
	// The session of host replayed on another host is no session there.
	if resp, _ := w.do(http.MethodGet, otherHost, "/", http.Header{"Cookie": root["Cookie"], "Sec-Fetch-Mode": {"cors"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("host's session on another host: %d", resp.StatusCode)
	}
}

// TestABrowserWithoutASessionIsHandedOff: the edge sends a browser to the
// console's endpoint-login with its host and a state, and keeps the state,
// with where to return, in a __Host- cookie of its own host: the return
// path never leaves the edge.
func TestABrowserWithoutASessionIsHandedOff(t *testing.T) {
	w := newWorld(t)
	w.endpoint(8080, private)
	w.served(w.rootCookie())
	resp, _ := w.do(http.MethodGet, host, "/app/page?x=1", nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("no session: %d", resp.StatusCode)
	}
	to, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := to.Query()
	if to.Scheme+"://"+to.Host != consoleURL || to.Path != login.PathEndpointLogin || q.Get("host") != host || len(q) != 2 {
		t.Errorf("handed off to %s", to)
	}
	if strings.Contains(to.String(), "app") || strings.Contains(to.String(), "page") {
		t.Errorf("the return path left the edge: %s", to)
	}
	state := q.Get("state")
	id, _, _ := strings.Cut(state, ".")
	var ck *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == edge.StateCookiePrefix+id {
			ck = c
		}
	}
	if ck == nil || !ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/" || ck.Domain != "" || ck.MaxAge <= 0 {
		t.Fatalf("the state cookie: %+v of %q", ck, resp.Header.Values("Set-Cookie"))
	}
	if strings.Contains(ck.Value, state) || strings.Contains(ck.Value, "page") {
		t.Errorf("the state cookie is not sealed: %q", ck.Value)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy %q", got)
	}
}

// TestTheCallbackTakesOnlyItsOwnHandoff: the callback refuses a code
// without the state cookie of the browser that started the handoff, or with
// a state cookie of another handoff or host, before it asks the apiserver,
// and sets no session for any of them. (A code the apiserver refuses is
// TestAHandoffEndToEnd's.)
func TestTheCallbackTakesOnlyItsOwnHandoff(t *testing.T) {
	w := newWorld(t)
	w.endpoint(8080, private)
	w.applyNamed("other", private)
	w.served(w.rootCookie())
	start := func(h string) (state string, ck *http.Cookie) {
		t.Helper()
		resp, _ := w.do(http.MethodGet, h, "/", nil)
		to, _ := url.Parse(resp.Header.Get("Location"))
		state = to.Query().Get("state")
		for _, c := range resp.Cookies() {
			if strings.HasPrefix(c.Name, edge.StateCookiePrefix) {
				ck = c
			}
		}
		return state, ck
	}
	callback := func(h, code, state string, ck *http.Cookie) (*answer, string) {
		t.Helper()
		hd := http.Header{}
		if ck != nil {
			hd.Set("Cookie", ck.Name+"="+ck.Value)
		}
		return w.do(http.MethodGet, h, login.EndpointCallbackPath+"?"+url.Values{"code": {code}, "state": {state}}.Encode(), hd)
	}
	state, ck := start(host)
	otherState, otherCk := start(otherHost)
	for what, c := range map[string]func() (*answer, string){
		"no state cookie":                 func() (*answer, string) { return callback(host, "code", state, nil) },
		"another handoff's state":         func() (*answer, string) { return callback(host, "code", otherState, ck) },
		"another host's state and cookie": func() (*answer, string) { return callback(host, "code", otherState, otherCk) },
		"no state":                        func() (*answer, string) { return callback(host, "code", "", ck) },
	} {
		resp, body := c()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "Sign-in failed") {
			t.Errorf("%s: %d %q", what, resp.StatusCode, body)
		}
		// The page's URL holds the code, and it links to the endpoint.
		if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" || !strings.Contains(body, `content="no-referrer"`) {
			t.Errorf("%s: Referrer-Policy %q", what, got)
		}
		for _, c := range resp.Cookies() {
			if c.Name == edge.EndpointCookie {
				t.Errorf("%s set a session", what)
			}
		}
	}
	// A public endpoint has no callback, and nothing under /.hangar/ reaches
	// it.
	w.applyNamed("open", public)
	openHost := "open-" + apiservertest.Project + "." + domain
	testutil.Eventually(w.t, "the public endpoint", func() (bool, string) {
		resp, body := w.do(http.MethodGet, openHost, "/", nil)
		return resp.StatusCode == http.StatusOK, body
	})
	if resp, body := callback(openHost, "code", state, ck); resp.StatusCode != http.StatusNotFound || strings.HasPrefix(body, "port ") {
		t.Errorf("a public endpoint's callback: %d %q", resp.StatusCode, body)
	}
}

// TestNothingOfHangarsReachesAnEndpoint: through the edge, an endpoint,
// public or private, sees none of Hangar's cookies, bearer tokens or Hangar-
// headers, nor what Cloudflare Access used, and sees the rest; nothing under
// /.hangar/ reaches it, however spelled; and it cannot set Hangar's cookies.
func TestNothingOfHangarsReachesAnEndpoint(t *testing.T) {
	w := newWorld(t)
	w.endpoint(8080, private)
	w.applyNamed("open", public)
	openHost := "open-" + apiservertest.Project + "." + domain
	root := w.rootCookie()
	w.served(root)
	editor := w.serviceToken("deployer", "project.editor")
	session := root.Get("Cookie")

	extra := func(h http.Header) http.Header {
		h.Add("Cookie", "theme=dark; __Host-hangar-endpoint-state-abcdefghijkl=s; __Host-hangar_session=hgs_x")
		h.Add("Cookie", "CF_Authorization=a; __HOST-HANGAR_login=l;lang=en")
		h.Set("Hangar-Principal", "user:root@example.com")
		h.Set("Hangar-Client-Ip", "192.0.2.1")
		h.Set("hangar-email", "root@example.com")
		h.Set("Cf-Access-Jwt-Assertion", "jwt")
		h.Set("Cf-Access-Client-Secret", "secret")
		return h
	}
	want := `port 8080 path="/" headers=[] authorization=%s cookie="theme=dark; lang=en"`
	for what, c := range map[string]struct {
		h        http.Header
		hostname string
		auth     string
	}{
		"private, with the session": {extra(http.Header{"Cookie": {session}}), host, "[]"},
		"private, with a token":     {extra(http.Header{"Authorization": {"Bearer " + editor.Reveal()}}), host, "[]"},
		"public, with both":         {extra(http.Header{"Cookie": {session}, "Authorization": {"Bearer " + editor.Reveal()}}), openHost, "[]"},
		"public, the app's own":     {extra(http.Header{"Authorization": {"Basic YTpi"}}), openHost, `["Basic YTpi"]`},
		"private, the app's own":    {extra(http.Header{"Cookie": {session}, "Authorization": {"Bearer app-token"}}), host, `["Bearer app-token"]`},
	} {
		var body string
		testutil.Eventually(w.t, what, func() (bool, string) {
			var resp *answer
			resp, body = w.do(http.MethodGet, c.hostname, "/", c.h)
			return resp.StatusCode == http.StatusOK, body
		})
		if want := fmt.Sprintf(want, c.auth); body != want {
			t.Errorf("%s: the endpoint saw\n%s\nwant\n%s", what, body, want)
		}
		if strings.Contains(body, "hge_") || strings.Contains(body, "hgr_") {
			t.Errorf("%s: a Hangar credential reached the endpoint: %s", what, body)
		}
	}

	// The /.hangar/ path space, on a public endpoint and on a private one
	// with a session: never forwarded.
	for _, p := range []string{
		"/.hangar/", "/.hangar/x", "/.HANGAR/callback", "/%2ehangar/x", "/%2Ehangar%2Fx", "/a/../.hangar/x",
		"/./.hangar/x", "/.hangar;x/y", "/.hangar./x", "//.hangar/x", "/.hangar",
	} {
		for _, hh := range []struct {
			name string
			h    http.Header
		}{{openHost, nil}, {host, http.Header{"Cookie": {session}}}} {
			status, body := rawGet(t, w.url, hh.name, p, hh.h)
			if status != http.StatusNotFound || strings.HasPrefix(body, "port ") {
				t.Errorf("%s on %s: %d %q", p, hh.name, status, body)
			}
		}
	}
	status, body := rawGet(t, w.url, openHost, "/\\.hangar/x", nil)
	if status != http.StatusNotFound || strings.HasPrefix(body, "port ") {
		t.Errorf(`/\.hangar/x: %d %q`, status, body)
	}
	// Lookalikes are the endpoint's own.
	for _, p := range []string{"/.hangarx/y", "/x/.hangar/y", "/hangar/y"} {
		if status, body := rawGet(t, w.url, openHost, p, nil); status != http.StatusOK || !strings.HasPrefix(body, "port 8080") {
			t.Errorf("%s: %d %q; want the endpoint", p, status, body)
		}
	}

	resp, _ := w.do(http.MethodGet, openHost, "/set-cookie", nil)
	if got := resp.Header.Values("Set-Cookie"); !slices.Equal(got, []string{"theme=dark", "lang=en; Path=/"}) {
		t.Errorf("an endpoint set %q; want only its own cookies, host-only", got)
	}
}

// rawGet sends GET path to the edge at base for host exactly as written,
// with header.
func rawGet(t *testing.T, base, host, path string, header http.Header) (int, string) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	req := "GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\nConnection: close\r\n"
	for k, vs := range header {
		for _, v := range vs {
			req += k + ": " + v + "\r\n"
		}
	}
	if _, err := io.WriteString(c, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestAnUpgradeEndsWithItsCredential: what a credential let in ends within
// the recheck once it is revoked: an endpoint session when its console
// session signs out, a service token when it is rotated. Another caller's
// connection stays.
func TestAnUpgradeEndsWithItsCredential(t *testing.T) {
	w := newWorld(t)
	w.endpoint(8080, private)
	console, f := w.consoleSession(apiservertest.Root, time.Time{})
	b, err := w.sessions.StartEndpoint(context.Background(), f, host)
	if err != nil {
		t.Fatal(err)
	}
	w.served(cookie(b.Secret))
	editor := w.serviceToken("deployer", "project.editor")
	other := w.rootCookie()

	signedIn := w.upgrade(cookie(b.Secret))
	tokens := w.upgrade(bearer(editor))
	stays := w.upgrade(other)

	// Signing out of the console revokes the login the endpoint session is in.
	if err := w.sessions.Revoke(context.Background(), console); err != nil {
		t.Fatal(err)
	}
	if err := closedWithin(signedIn, 5*time.Second); err != nil {
		t.Errorf("an upgrade outlived its session's sign-out: %v", err)
	}
	if status, _ := w.get(http.Header{"Cookie": {edge.EndpointCookie + "=" + b.Secret.Reveal()}, "Sec-Fetch-Mode": {"cors"}}); status != http.StatusUnauthorized {
		t.Errorf("the session after its sign-out: %d", status)
	}

	c := iamv1alpha1connect.NewServiceTokenIssuerServiceClient(apiservertest.DevUser(apiservertest.Root), w.env.URL)
	if _, err := c.RotateServiceToken(context.Background(), connect.NewRequest(&iamv1alpha1.RotateServiceTokenRequest{
		Project: apiservertest.Project, Name: "deployer",
	})); err != nil {
		t.Fatal(err)
	}
	if err := closedWithin(tokens, 5*time.Second); err != nil {
		t.Errorf("an upgrade outlived its rotated token: %v", err)
	}
	if err := closedWithin(stays, 300*time.Millisecond); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("another caller's connection: %v", err)
	}
}

// TestAHandoffEndsAfterTenMinutes: the state a browser holds for a handoff
// counts for ten minutes; after that the callback refuses it before it
// asks the apiserver anything.
func TestAHandoffEndsAfterTenMinutes(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	w := newWorldConfig(t, apiservertest.Start(t, nil), func(c *edge.Config) { c.Now = clock })
	w.endpoint(8080, private)
	w.served(w.rootCookie())
	callback := func() (*answer, string) {
		t.Helper()
		resp, _ := w.do(http.MethodGet, host, "/", nil)
		to, _ := url.Parse(resp.Header.Get("Location"))
		state := to.Query().Get("state")
		var ck *http.Cookie
		for _, c := range resp.Cookies() {
			if strings.HasPrefix(c.Name, edge.StateCookiePrefix) {
				ck = c
			}
		}
		if ck == nil {
			t.Fatal("no state cookie")
		}
		mu.Lock()
		now = now.Add(9 * time.Minute)
		mu.Unlock()
		return w.do(http.MethodGet, host, login.EndpointCallbackPath+"?"+url.Values{"code": {"code"}, "state": {state}}.Encode(),
			http.Header{"Cookie": {ck.Name + "=" + ck.Value}})
	}
	// Nine minutes on, the state still counts: the edge asks the apiserver,
	// which signs no one in here (dev auth), so the answer is 503.
	if resp, body := callback(); resp.StatusCode != http.StatusServiceUnavailable || strings.Contains(body, "took too long") {
		t.Errorf("nine minutes on: %d %q", resp.StatusCode, body)
	}
	// Eleven minutes on, it does not.
	resp, _ := w.do(http.MethodGet, host, "/", nil)
	to, _ := url.Parse(resp.Header.Get("Location"))
	var ck *http.Cookie
	for _, c := range resp.Cookies() {
		if strings.HasPrefix(c.Name, edge.StateCookiePrefix) {
			ck = c
		}
	}
	mu.Lock()
	now = now.Add(11 * time.Minute)
	mu.Unlock()
	late, body := w.do(http.MethodGet, host, login.EndpointCallbackPath+"?"+url.Values{"code": {"code"}, "state": {to.Query().Get("state")}}.Encode(),
		http.Header{"Cookie": {ck.Name + "=" + ck.Value}})
	if late.StatusCode != http.StatusBadRequest || !strings.Contains(body, "took too long") {
		t.Errorf("eleven minutes on: %d %q", late.StatusCode, body)
	}
}

// TestStripCredentialsTakesAnySpacing: a Hangar bearer token goes however
// the header spaces it, tabs included; the endpoint's own Authorization
// stays.
func TestStripCredentialsTakesAnySpacing(t *testing.T) {
	for _, v := range []string{
		"Bearer hgr_x", "bearer hge_x", "Bearer\thga_x", "Bearer \t hgs_x", " BEARER  hgf_x ", "Bearer\thgr_x extra",
	} {
		h := http.Header{"Authorization": {v}}
		edge.StripCredentials(h)
		if got := h.Values("Authorization"); len(got) != 0 {
			t.Errorf("%q kept as %q", v, got)
		}
	}
	for _, v := range []string{"Basic YTpi", "Bearer app-token", "Bearer\tapp-token", "hgr_x"} {
		h := http.Header{"Authorization": {v}}
		edge.StripCredentials(h)
		if got := h.Values("Authorization"); !slices.Equal(got, []string{v}) {
			t.Errorf("%q became %q", v, got)
		}
	}
	h := http.Header{"Authorization": {"Basic YTpi", "Bearer\thgr_x"}}
	edge.StripCredentials(h)
	if got := h.Values("Authorization"); !slices.Equal(got, []string{"Basic YTpi"}) {
		t.Errorf("two values: %q", got)
	}
}

// TestEndpointCookiesAreHostOnly: every Set-Cookie of an endpoint's answer
// loses its Domain attribute, however written and wherever it stands, and
// keeps the rest byte for byte; one of Hangar's names still goes.
func TestEndpointCookiesAreHostOnly(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a=1; Domain=" + domain, "a=1"},
		{"a=1; Domain=." + domain + "; Path=/", "a=1; Path=/"},
		{"a=1;domain=" + domain, "a=1"},
		{"a=1; DOMAIN = " + domain + "; Secure", "a=1; Secure"},
		{"a=1; dOmAiN", "a=1"},
		{"a=1; Domain=e-abc." + domain, "a=1"},
		{"a=1; Path=/x;  Domain=" + domain + " ;HttpOnly; SameSite=Lax", "a=1; Path=/x;HttpOnly; SameSite=Lax"},
		{"a=1; Domain=x; Secure; Domain=y", "a=1; Secure"},
		{"a=b=c;  Path=/ ; Max-Age=60;Secure", "a=b=c;  Path=/ ; Max-Age=60;Secure"},
		{"domain=1; Path=/", "domain=1; Path=/"},
		{"a=1; Domains=x; X-Domain=y", "a=1; Domains=x; X-Domain=y"},
		{"a=1", "a=1"},
	} {
		h := http.Header{"Set-Cookie": {tc.in}}
		edge.ConfineCookies(h)
		if got := h.Values("Set-Cookie"); !slices.Equal(got, []string{tc.want}) {
			t.Errorf("%q became %q; want %q", tc.in, got, tc.want)
		}
	}

	h := http.Header{"Set-Cookie": {
		"a=1; Domain=" + domain + "; Path=/",
		edge.EndpointCookie + "=planted; Secure; Path=/",
		"b=2; Secure",
		"c=3; HttpOnly; domain=." + domain,
	}}
	edge.ConfineCookies(h)
	want := []string{"a=1; Path=/", "b=2; Secure", "c=3; HttpOnly"}
	if got := h.Values("Set-Cookie"); !slices.Equal(got, want) {
		t.Errorf("several: %q; want %q", got, want)
	}
}

// TestEveryHeaderBlockKeepsCookiesHostOnly: on the bytes a client gets, an
// endpoint's 103 and 101 carry its cookies host-only and none of Hangar's
// names, and its trailers carry no Set-Cookie, declared or not, while the
// rest of them still arrives.
func TestEveryHeaderBlockKeepsCookiesHostOnly(t *testing.T) {
	w := newWorld(t)
	w.endpoint(8080, public)
	w.served(nil)
	get := func(path string) string {
		return rawExchange(t, w.url, "GET "+path+" HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n", "")
	}

	hints := get("/early-hints")
	head, _, _ := strings.Cut(hints, "HTTP/1.1 200")
	if !strings.HasPrefix(head, "HTTP/1.1 103") || !strings.Contains(head, "\r\nLink: </style.css>; rel=preload\r\n") ||
		!strings.Contains(head, "\r\nSet-Cookie: hint=1; Path=/\r\n") || strings.Contains(head, "planted") {
		t.Errorf("the 103: %q", hints)
	}
	if !strings.Contains(hints, "port 8080 after hints") {
		t.Errorf("after the 103: %q", hints)
	}

	for _, tc := range []struct{ path, cookie, sum string }{
		{"/trailers", "declared=1", "declared"},
		{"/undeclared-trailers", "undeclared=1", "undeclared"},
	} {
		got := get(tc.path)
		if strings.Contains(got, tc.cookie) || !strings.Contains(got, "\r\nX-Sum: "+tc.sum+"\r\n") {
			t.Errorf("%s: %q", tc.path, got)
		}
	}

	up := rawExchange(t, w.url, "GET /upgrade HTTP/1.1\r\nHost: "+host+"\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n", "\r\n\r\n")
	if !strings.HasPrefix(up, "HTTP/1.1 101") || !strings.Contains(up, "\r\nSet-Cookie: ws=1; Path=/\r\n") ||
		strings.Contains(up, "planted") {
		t.Errorf("the 101: %q", up)
	}
}

// rawExchange sends req to the edge at base exactly as written and returns
// the bytes it answers with, up to and including the first end, or all of
// them until it closes when end is "".
func rawExchange(t *testing.T, base, req, end string) string {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	var got []byte
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		got = append(got, buf[:n]...)
		if end != "" {
			if i := strings.Index(string(got), end); i >= 0 {
				return string(got[:i+len(end)])
			}
		}
		if errors.Is(err, io.EOF) {
			return string(got)
		}
		if err != nil {
			t.Fatalf("after %q: %v", got, err)
		}
	}
}

// TestStateCookiesAreBounded: a browser holding as many handoffs in
// progress as the edge keeps on one host has them all cleared when it
// starts another, so they cannot pile up; one with fewer keeps them.
func TestStateCookiesAreBounded(t *testing.T) {
	w := newWorld(t)
	w.endpoint(8080, private)
	w.served(w.rootCookie())
	start := func(held int) (cleared, set int) {
		t.Helper()
		var cookies []string
		for i := range held {
			cookies = append(cookies, fmt.Sprintf("%s%012d=x", edge.StateCookiePrefix, i))
		}
		h := http.Header{}
		if held > 0 {
			h.Set("Cookie", strings.Join(cookies, "; "))
		}
		resp, _ := w.do(http.MethodGet, host, "/", h)
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("with %d held: %d", held, resp.StatusCode)
		}
		for _, c := range resp.Cookies() {
			switch {
			case !strings.HasPrefix(c.Name, edge.StateCookiePrefix):
			case c.MaxAge < 0:
				cleared++
			default:
				set++
			}
		}
		return cleared, set
	}
	if cleared, set := start(7); cleared != 0 || set != 1 {
		t.Errorf("seven held: %d cleared, %d set; want 0 and 1", cleared, set)
	}
	if cleared, set := start(8); cleared != 8 || set != 1 {
		t.Errorf("eight held: %d cleared, %d set; want 8 and 1", cleared, set)
	}
}

// refusingAuth is an apiserver that refuses the edge itself, as a
// misconfigured edge socket would.
type refusingAuth struct{}

func (refusingAuth) ExchangeEndpointCode(context.Context, *connect.Request[networkv1alpha1.ExchangeEndpointCodeRequest]) (*connect.Response[networkv1alpha1.ExchangeEndpointCodeResponse], error) {
	return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("only hangar-edge may ask"))
}

func (refusingAuth) CheckEndpointCredential(context.Context, *connect.Request[networkv1alpha1.CheckEndpointCredentialRequest]) (*connect.Response[networkv1alpha1.CheckEndpointCredentialResponse], error) {
	return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("only hangar-edge may ask"))
}

// TestTheEdgeRefusedIsNotThePersonDenied: when the apiserver refuses the
// edge itself, a browser's callback says the sign-in cannot be checked
// (503), not that its person may no longer use the endpoint, and a request
// with a credential is answered 503, not as if the credential were bad.
func TestTheEdgeRefusedIsNotThePersonDenied(t *testing.T) {
	w := newWorldConfig(t, apiservertest.Start(t, nil), func(c *edge.Config) { c.Auth = refusingAuth{} })
	w.endpoint(8080, private)
	var to *url.URL
	var ck *http.Cookie
	testutil.Eventually(w.t, "a handoff", func() (bool, string) {
		resp, body := w.do(http.MethodGet, host, "/", nil)
		to, _ = url.Parse(resp.Header.Get("Location"))
		for _, c := range resp.Cookies() {
			if strings.HasPrefix(c.Name, edge.StateCookiePrefix) {
				ck = c
			}
		}
		return resp.StatusCode == http.StatusFound && ck != nil, fmt.Sprint(resp.StatusCode, body)
	})
	resp, body := w.do(http.MethodGet, host, login.EndpointCallbackPath+"?"+url.Values{"code": {"code"}, "state": {to.Query().Get("state")}}.Encode(),
		http.Header{"Cookie": {ck.Name + "=" + ck.Value}})
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "cannot be checked now") || strings.Contains(body, "may no longer") {
		t.Errorf("the callback with the edge refused: %d %q", resp.StatusCode, body)
	}
	if status, body := w.get(cookie(token.New(token.Endpoint))); status != http.StatusServiceUnavailable {
		t.Errorf("a session with the edge refused: %d %q", status, body)
	}
}
