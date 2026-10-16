package apiserver_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1/networkv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/login"
	"github.com/YukinaMochizuki/hangar/internal/client"
)

const (
	siteHost  = "site-web." + apiserver.DefaultEndpointDomain
	otherHost = "other-web." + apiserver.DefaultEndpointDomain
	openHost  = "open-web." + apiserver.DefaultEndpointDomain
	// handoffState is a state as hangar-edge makes one.
	handoffState = "abcdefghijkl.0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
)

// endpointWorld is an oidcWorld with a fake clock, alice signed in and
// signed up, a project web with the PRIVATE endpoints site and other and the
// PUBLIC endpoint open, and an edge socket's EndpointAuthService.
type endpointWorld struct {
	*oidcWorld
	clock   *clockwork.FakeClock
	alice   headers
	browser *http.Client
	edge    networkv1alpha1connect.EndpointAuthServiceClient
}

// asEdge is what the edge socket's chain makes of its peer.
type asEdge struct{}

func (asEdge) Authenticate(context.Context, http.Header) (auth.Principal, bool, error) {
	return auth.Principal{Type: auth.Edge, ID: "n1"}, true, nil
}

func newEndpointWorld(t *testing.T) *endpointWorld {
	t.Helper()
	return newEndpointWorldWith(t, 0)
}

// newEndpointWorldWith is newEndpointWorld with a cap on the sign-ins of
// each kind waiting at once; zero is the default.
func newEndpointWorldWith(t *testing.T, maxPending int) *endpointWorld {
	t.Helper()
	clock := clockwork.NewFakeClockAt(time.Now())
	w := &endpointWorld{oidcWorld: newOIDCWorldConfig(t, maxPending, func(c *apiserver.Config) { c.Clock = clock }), clock: clock}
	w.browser = w.oidcWorld.browser()
	w.browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	follow := w.oidcWorld.browser()
	follow.Jar = w.browser.Jar
	w.alice = w.asCookie(w.signInAdmin(follow, "alice@example.com"))
	ctx := t.Context()
	must[iamv1alpha1.SignUpResponse](t)(w.accounts(w.alice).SignUp(ctx, connect.NewRequest(&iamv1alpha1.SignUpRequest{Username: "alice"})))
	api := client.New(w.tls, w.url, connect.WithInterceptors(w.alice))
	if _, err := api.Apply(ctx, registry.Tenant, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "acme"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Apply(ctx, registry.Project, &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: "web"}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "acme"}}); err != nil {
		t.Fatal(err)
	}
	for name, access := range map[string]networkv1alpha1.PublicEndpointAccess{
		"site":  networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PRIVATE,
		"other": networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PRIVATE,
		"open":  networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PUBLIC,
	} {
		if _, err := api.Apply(ctx, registry.PublicEndpoint, &networkv1alpha1.PublicEndpoint{
			Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
			Spec: &networkv1alpha1.PublicEndpointSpec{
				Target: &networkv1alpha1.PublicEndpointTarget{Object: &networkv1alpha1.PublicEndpointTarget_Container{Container: "api"}},
				Port:   8080, Access: access,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	edge := httptest.NewServer(w.srv.EdgeHandler(auth.Chain{asEdge{}}))
	t.Cleanup(edge.Close)
	w.edge = networkv1alpha1connect.NewEndpointAuthServiceClient(http.DefaultClient, edge.URL)
	return w
}

// endpointLogin asks the console's endpoint-login, in the signed-in
// browser, for host with state, and returns the answer's status, Location and
// body.
func (w *endpointWorld) endpointLogin(host, state string) (int, string, string) {
	w.t.Helper()
	resp, err := w.browser.Get(w.url + login.PathEndpointLogin + "?" + url.Values{"host": {host}, "state": {state}}.Encode())
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

// code gets a handoff code for host and state.
func (w *endpointWorld) code(host, state string) string {
	w.t.Helper()
	status, loc, body := w.endpointLogin(host, state)
	if status != http.StatusSeeOther {
		w.t.Fatalf("endpoint-login for %s: %d %q", host, status, body)
	}
	u, err := url.Parse(loc)
	if err != nil {
		w.t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != host || u.Path != login.EndpointCallbackPath || u.Query().Get("state") != state {
		w.t.Fatalf("endpoint-login for %s sent the browser to %s", host, loc)
	}
	return u.Query().Get("code")
}

func (w *endpointWorld) exchange(code, host, state string) (*networkv1alpha1.ExchangeEndpointCodeResponse, error) {
	resp, err := w.edge.ExchangeEndpointCode(w.t.Context(), connect.NewRequest(&networkv1alpha1.ExchangeEndpointCodeRequest{Code: code, Host: host, State: state}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (w *endpointWorld) check(cred, host string) (*networkv1alpha1.CheckEndpointCredentialResponse, error) {
	resp, err := w.edge.CheckEndpointCredential(w.t.Context(), connect.NewRequest(&networkv1alpha1.CheckEndpointCredentialRequest{Credential: cred, Host: host}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// TestAHandoffCodeCountsOnceForItsHostAndState: a code endpoint-login
// issues goes only to its endpoint's callback, and is taken once, within a
// minute, for that host and that state; anything else spends it and gets
// nothing. The session it makes counts on that host alone.
func TestAHandoffCodeCountsOnceForItsHostAndState(t *testing.T) {
	w := newEndpointWorld(t)

	resp, err := w.browser.Get(w.url + login.PathEndpointLogin + "?" + url.Values{"host": {siteHost}, "state": {handoffState}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("the redirect to the endpoint has Referrer-Policy %q", got)
	}

	code := w.code(siteHost, handoffState)
	_, err = w.exchange(code, otherHost, handoffState)
	wantCode(t, err, connect.CodeUnauthenticated, "a code for another host")
	_, err = w.exchange(code, siteHost, handoffState)
	wantCode(t, err, connect.CodeUnauthenticated, "the code after a try for another host")

	code = w.code(siteHost, handoffState)
	_, err = w.exchange(code, siteHost, handoffState+"x")
	wantCode(t, err, connect.CodeUnauthenticated, "a code with another state")

	code = w.code(siteHost, handoffState)
	got, err := w.exchange(code, siteHost, handoffState)
	if err != nil {
		t.Fatalf("exchanging the code: %v", err)
	}
	if !strings.HasPrefix(got.GetSession(), "hge_") || got.GetPrincipal() != "user:alice@example.com" {
		t.Errorf("exchanged for %q as %q", got.GetSession()[:4], got.GetPrincipal())
	}
	if ttl := got.GetExpireTime().AsTime().Sub(w.clock.Now()); ttl <= 23*time.Hour || ttl > 24*time.Hour {
		t.Errorf("the endpoint session lasts %v; want a day", ttl)
	}
	_, err = w.exchange(code, siteHost, handoffState)
	wantCode(t, err, connect.CodeUnauthenticated, "a code used again")

	// The session: this host's alone, and no API credential.
	if c, err := w.check(got.GetSession(), siteHost); err != nil || c.GetPrincipal() != "user:alice@example.com" {
		t.Errorf("checking the session on its host: %v %v", c, err)
	}
	_, err = w.check(got.GetSession(), otherHost)
	wantCode(t, err, connect.CodeUnauthenticated, "the session on another host")
	_, err = w.whoami(asBearer(got.GetSession()))
	wantCode(t, err, connect.CodeUnauthenticated, "the endpoint session on the API")

	code = w.code(siteHost, handoffState)
	w.clock.Advance(login.EndpointCodeTTL + time.Second)
	_, err = w.exchange(code, siteHost, handoffState)
	wantCode(t, err, connect.CodeUnauthenticated, "a code past its minute")

	// Only hashes are stored: no code and no session in any file of the
	// store.
	code = w.code(siteHost, handoffState)
	files, _ := filepath.Glob(filepath.Join(w.dir, "*"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for what, secret := range map[string]string{"a code": code, "a session": got.GetSession()} {
			if bytes.Contains(b, []byte(secret)) {
				t.Errorf("%s is stored in %s", what, filepath.Base(f))
			}
		}
	}
}

// TestEndpointLoginSendsBrowsersToEndpointsAlone: endpoint-login hands off
// to the callback of an existing PRIVATE endpoint and to nothing else, so it
// redirects nowhere a link names; a person who may not use the endpoint is
// told so, and one not signed in signs in first and comes back.
func TestEndpointLoginSendsBrowsersToEndpointsAlone(t *testing.T) {
	w := newEndpointWorld(t)
	for _, c := range []struct {
		host, state string
		want        int
	}{
		{"evil.example", handoffState, http.StatusNotFound},
		{siteHost + ".evil.example", handoffState, http.StatusNotFound},
		{"evil-" + apiserver.DefaultEndpointDomain, handoffState, http.StatusNotFound},
		{"a.site-web." + apiserver.DefaultEndpointDomain, handoffState, http.StatusNotFound},
		{apiserver.DefaultEndpointDomain, handoffState, http.StatusNotFound},
		{openHost, handoffState, http.StatusNotFound},
		{strings.ToUpper(siteHost), handoffState, http.StatusBadRequest},
		{siteHost + ":8443", handoffState, http.StatusBadRequest},
		{siteHost + ".", handoffState, http.StatusBadRequest},
		{siteHost + "/x", handoffState, http.StatusBadRequest},
		{"evil.example@" + siteHost, handoffState, http.StatusBadRequest},
		{"", handoffState, http.StatusBadRequest},
		{siteHost, "", http.StatusBadRequest},
		{siteHost, "short", http.StatusBadRequest},
		{siteHost, handoffState + "&x=<y>", http.StatusBadRequest},
		{siteHost, strings.Repeat("a", 201), http.StatusBadRequest},
	} {
		status, loc, body := w.endpointLogin(c.host, c.state)
		if status != c.want || loc != "" {
			t.Errorf("host %q state %q: %d, Location %q; want %d and none: %q", c.host, c.state, status, loc, c.want, body)
		}
	}

	// bob is a member who may not use the endpoint.
	w.adminInvitesWith(w.alice, "bob@example.com")
	bob := w.oidcWorld.browser()
	bob.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	follow := w.oidcWorld.browser()
	follow.Jar = bob.Jar
	w.signIn(follow, "bob@example.com")
	resp, err := bob.Get(w.url + login.PathEndpointLogin + "?" + url.Values{"host": {siteHost}, "state": {handoffState}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Location") != "" || !strings.Contains(string(b), "may not use "+siteHost) {
		t.Errorf("bob, who may not use the endpoint: %d %q", resp.StatusCode, b)
	}

	// Not signed in: to the provider, then back to endpoint-login, which
	// hands off.
	anon := w.oidcWorld.browser()
	var seen []string
	anon.CheckRedirect = func(r *http.Request, _ []*http.Request) error {
		seen = append(seen, r.URL.String())
		if r.URL.Path == login.EndpointCallbackPath {
			return http.ErrUseLastResponse
		}
		return nil
	}
	w.provider.SignIn("alice@example.com", true)
	resp, err = anon.Get(w.url + login.PathEndpointLogin + "?" + url.Values{"host": {siteHost}, "state": {handoffState}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "https://"+siteHost+login.EndpointCallbackPath+"?") {
		t.Errorf("signing in from endpoint-login ended at %d %q, through %q", resp.StatusCode, resp.Header.Get("Location"), seen)
	}
}

// TestSigningOutEndsEndpointSessions: an endpoint session belongs to the
// console session it came from: signing out of the console ends it, and
// revoking the person's logins ends one of any.
func TestSigningOutEndsEndpointSessions(t *testing.T) {
	w := newEndpointWorld(t)
	session := func() string {
		t.Helper()
		got, err := w.exchange(w.code(siteHost, handoffState), siteHost, handoffState)
		if err != nil {
			t.Fatal(err)
		}
		return got.GetSession()
	}
	s := session()
	req, _ := http.NewRequest(http.MethodPost, w.url+login.PathLogout, nil)
	req.Header.Set("Origin", w.url)
	resp, err := w.browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("signing out: %d", resp.StatusCode)
	}
	_, err = w.check(s, siteHost)
	wantCode(t, err, connect.CodeUnauthenticated, "an endpoint session after its console session signed out")

	follow := w.oidcWorld.browser()
	follow.Jar = w.browser.Jar
	w.signIn(follow, "alice@example.com")
	s = session()
	if _, err := w.check(s, siteHost); err != nil {
		t.Fatal(err)
	}
	if _, err := w.sessions(w.asCookie(w.cookie(w.browser))).RevokeSessions(t.Context(), connect.NewRequest(&iamv1alpha1.RevokeSessionsRequest{Email: "alice@example.com"})); err != nil {
		t.Fatal(err)
	}
	_, err = w.check(s, siteHost)
	wantCode(t, err, connect.CodeUnauthenticated, "an endpoint session after its person's logins were revoked")
}

// TestEndpointAuthIsTheEdgesAlone: EndpointAuthService answers the Edge
// principal on the edge socket's handler and no one else, and no other
// listener serves it.
func TestEndpointAuthIsTheEdgesAlone(t *testing.T) {
	w := newEndpointWorld(t)
	notEdge := httptest.NewServer(w.srv.EdgeHandler(auth.Chain{asUser{}}))
	defer notEdge.Close()
	c := networkv1alpha1connect.NewEndpointAuthServiceClient(http.DefaultClient, notEdge.URL)
	_, err := c.CheckEndpointCredential(t.Context(), connect.NewRequest(&networkv1alpha1.CheckEndpointCredentialRequest{Credential: "hge_x", Host: siteHost}))
	wantCode(t, err, connect.CodeFailedPrecondition, "a user calling CheckEndpointCredential")
	_, err = c.ExchangeEndpointCode(t.Context(), connect.NewRequest(&networkv1alpha1.ExchangeEndpointCodeRequest{Code: "x", Host: siteHost, State: handoffState}))
	wantCode(t, err, connect.CodeFailedPrecondition, "a user calling ExchangeEndpointCode")

	path := "/" + networkv1alpha1connect.EndpointAuthServiceName + "/CheckEndpointCredential"
	for name, h := range map[string]http.Handler{
		"public": w.srv.PublicHandler(auth.Chain{asEdge{}}),
		"node":   w.srv.Handler(auth.Chain{asEdge{}}),
		"admin":  w.srv.AdminHandler(auth.Chain{asEdge{}}),
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("the %s listener serves EndpointAuthService: %d", name, rec.Code)
		}
	}
	// A session counts on its host, and on no host at all is refused.
	got, err := w.exchange(w.code(siteHost, handoffState), siteHost, handoffState)
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.check(got.GetSession(), "")
	wantCode(t, err, connect.CodeUnauthenticated, "a session checked for no host")
	// So is a token bound to no host, checked for none.
	issuer := iamv1alpha1connect.NewServiceTokenIssuerServiceClient(w.tls, w.url, connect.WithInterceptors(w.alice))
	tok := must[iamv1alpha1.CreateServiceTokenResponse](t)(issuer.CreateServiceToken(t.Context(), connect.NewRequest(&iamv1alpha1.CreateServiceTokenRequest{
		Project: "web", Name: "deployer", Role: "project.editor",
	})))
	if c, err := w.check(tok.GetSecret(), siteHost); err != nil || c.GetPrincipal() != "serviceToken:web/deployer" {
		t.Errorf("a service token on an endpoint's host: %v, %v", c, err)
	}
	_, err = w.check(tok.GetSecret(), "")
	wantCode(t, err, connect.CodeUnauthenticated, "a service token checked for no host")
	// Tokens it takes, and those it does not.
	_, err = w.check("hgs_"+strings.Repeat("a", 43), siteHost)
	wantCode(t, err, connect.CodeUnauthenticated, "a console session cookie")
	_, err = w.check("Bearer nonsense", siteHost)
	wantCode(t, err, connect.CodeUnauthenticated, "no Hangar token")
}

type asUser struct{}

func (asUser) Authenticate(context.Context, http.Header) (auth.Principal, bool, error) {
	return auth.Principal{Type: auth.User, ID: "alice@example.com"}, true, nil
}

// adminInvitesWith invites the emails as platform members, as alice with
// headers.
func (w *oidcWorld) adminInvitesWith(alice headers, emails ...string) {
	w.t.Helper()
	bindings := iamv1alpha1connect.NewRoleBindingServiceClient(w.tls, w.url, connect.WithInterceptors(alice))
	for _, e := range emails {
		must[iamv1alpha1.ApplyRoleBindingResponse](w.t)(bindings.ApplyRoleBinding(w.t.Context(), connect.NewRequest(&iamv1alpha1.ApplyRoleBindingRequest{
			RoleBinding: &iamv1alpha1.RoleBinding{
				Metadata: &metav1.ObjectMeta{Name: "member-" + strings.NewReplacer("@", "-", ".", "-").Replace(e)},
				Spec: &iamv1alpha1.RoleBindingSpec{
					Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: e}},
					Role:    "platform.member",
					Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}},
				},
			},
		})))
	}
}

// TestEndpointCodesAreCappedPerPerson: one person may have only so many
// codes waiting, so no one fills the cap all share; another person still
// gets one, and a code exchanged no longer counts.
func TestEndpointCodesAreCappedPerPerson(t *testing.T) {
	w := newEndpointWorld(t)
	var codes []string
	for range login.MaxEndpointCodesPerPerson {
		codes = append(codes, w.code(siteHost, handoffState))
	}
	if status, loc, body := w.endpointLogin(siteHost, handoffState); status != http.StatusTooManyRequests || loc != "" {
		t.Fatalf("one code past the cap: %d, Location %q: %q", status, loc, body)
	}

	// bob may use the endpoint, and is not held to alice's codes.
	bob := w.bobWhoMayUse()
	resp, err := bob.Get(w.url + login.PathEndpointLogin + "?" + url.Values{"host": {siteHost}, "state": {handoffState}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("another person while alice is at the cap: %d", resp.StatusCode)
	}

	// One of alice's codes spent, she may have one more.
	if _, err := w.exchange(codes[0], siteHost, handoffState); err != nil {
		t.Fatal(err)
	}
	w.code(siteHost, handoffState)
	if status, _, _ := w.endpointLogin(siteHost, handoffState); status != http.StatusTooManyRequests {
		t.Errorf("past the cap again: %d", status)
	}
	// A code past its minute no longer counts.
	w.clock.Advance(login.EndpointCodeTTL + time.Second)
	w.code(siteHost, handoffState)
}

// bobOwner is the binding that lets bob use acme's endpoints.
func bobOwner() *iamv1alpha1.RoleBinding {
	return &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: "bob-owner"},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: "bob@example.com"}},
			Role:    "tenant.owner",
			Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: "acme"}},
		},
	}
}

// bindings is alice's RoleBinding client.
func (w *endpointWorld) bindings() iamv1alpha1connect.RoleBindingServiceClient {
	return iamv1alpha1connect.NewRoleBindingServiceClient(w.tls, w.url, connect.WithInterceptors(w.alice))
}

// bobWhoMayUse invites bob, signs him up and makes him owner of the
// tenant, so he may use its endpoints, and returns his signed-in browser,
// which follows no redirect.
func (w *endpointWorld) bobWhoMayUse() *http.Client {
	w.t.Helper()
	w.adminInvitesWith(w.alice, "bob@example.com")
	must[iamv1alpha1.ApplyRoleBindingResponse](w.t)(w.bindings().ApplyRoleBinding(w.t.Context(), connect.NewRequest(&iamv1alpha1.ApplyRoleBindingRequest{RoleBinding: bobOwner()})))
	bob := w.oidcWorld.browser()
	bob.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	follow := w.oidcWorld.browser()
	follow.Jar = bob.Jar
	cookie := w.signIn(follow, "bob@example.com")
	must[iamv1alpha1.SignUpResponse](w.t)(w.accounts(w.asCookie(cookie)).SignUp(w.t.Context(), connect.NewRequest(&iamv1alpha1.SignUpRequest{Username: "bob"})))
	return bob
}

// codeIn gets a handoff code for host and state in the browser b.
func (w *endpointWorld) codeIn(b *http.Client, host, state string) string {
	w.t.Helper()
	resp, err := b.Get(w.url + login.PathEndpointLogin + "?" + url.Values{"host": {host}, "state": {state}}.Encode())
	if err != nil {
		w.t.Fatal(err)
	}
	_ = resp.Body.Close()
	loc, err := resp.Location()
	if resp.StatusCode != http.StatusSeeOther || err != nil {
		w.t.Fatalf("endpoint-login: %d, %v", resp.StatusCode, err)
	}
	return loc.Query().Get("code")
}

// TestAnExchangeAsksForTheGrantAgain: a code whose person lost the grant,
// or the invitation, since it was issued gets nothing, and the edge hears
// that the person is denied rather than that the code is bad. (An
// uninvited person's grants count for nothing, so the grant check refuses
// the second as well; the invitation is asked again on its own anyway, as
// at every other issue.)
func TestAnExchangeAsksForTheGrantAgain(t *testing.T) {
	w := newEndpointWorld(t)
	bob := w.bobWhoMayUse()
	del := func(name string) {
		t.Helper()
		must[iamv1alpha1.DeleteRoleBindingResponse](t)(w.bindings().DeleteRoleBinding(t.Context(), connect.NewRequest(&iamv1alpha1.DeleteRoleBindingRequest{Name: name})))
	}

	// The control: a code exchanged while he holds both works.
	if _, err := w.exchange(w.codeIn(bob, siteHost, handoffState), siteHost, handoffState); err != nil {
		t.Fatalf("bob's code: %v", err)
	}

	code := w.codeIn(bob, siteHost, handoffState)
	del("bob-owner")
	_, err := w.exchange(code, siteHost, handoffState)
	wantCode(t, err, connect.CodePermissionDenied, "a code whose person lost the grant since")

	must[iamv1alpha1.ApplyRoleBindingResponse](t)(w.bindings().ApplyRoleBinding(t.Context(), connect.NewRequest(&iamv1alpha1.ApplyRoleBindingRequest{RoleBinding: bobOwner()})))
	code = w.codeIn(bob, siteHost, handoffState)
	del("member-bob-example-com")
	_, err = w.exchange(code, siteHost, handoffState)
	wantCode(t, err, connect.CodePermissionDenied, "a code whose person was uninvited since")
}

// TestEndpointCodesAreCappedPerPersonAtOnce: endpoint-logins fired at once
// by one person get no more codes than the cap, the rest 429, as the count
// and the insert are one transaction.
func TestEndpointCodesAreCappedPerPersonAtOnce(t *testing.T) {
	w := newEndpointWorld(t)
	const n = 60
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for range n {
		wg.Go(func() {
			resp, err := w.browser.Get(w.url + login.PathEndpointLogin + "?" + url.Values{"host": {siteHost}, "state": {handoffState}}.Encode())
			if err != nil {
				t.Error(err)
				return
			}
			_ = resp.Body.Close()
			mu.Lock()
			codes[resp.StatusCode]++
			mu.Unlock()
		})
	}
	wg.Wait()
	if codes[http.StatusSeeOther] != login.MaxEndpointCodesPerPerson || codes[http.StatusTooManyRequests] != n-login.MaxEndpointCodesPerPerson {
		t.Errorf("%d endpoint-logins at once: %v; want %d codes and the rest 429", n, codes, login.MaxEndpointCodesPerPerson)
	}
}

// TestEndpointCodesHaveASharedCap: past the cap on endpoint codes waiting,
// everyone's together, endpoint-login answers 503 to anyone, not the 429 of
// one person's own cap; a code exchanged makes room again.
func TestEndpointCodesHaveASharedCap(t *testing.T) {
	w := newEndpointWorldWith(t, 3)
	bob := w.bobWhoMayUse()
	codes := []string{w.code(siteHost, handoffState), w.code(siteHost, handoffState), w.codeIn(bob, siteHost, handoffState)}
	if status, loc, body := w.endpointLogin(siteHost, handoffState); status != http.StatusServiceUnavailable || loc != "" {
		t.Errorf("alice past the shared cap: %d, Location %q: %q", status, loc, body)
	}
	resp, err := bob.Get(w.url + login.PathEndpointLogin + "?" + url.Values{"host": {siteHost}, "state": {handoffState}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("bob past the shared cap: %d", resp.StatusCode)
	}
	if _, err := w.exchange(codes[0], siteHost, handoffState); err != nil {
		t.Fatal(err)
	}
	w.code(siteHost, handoffState)
}
