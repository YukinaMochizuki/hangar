package edge

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"connectrpc.com/connect"

	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/login"
	"github.com/YukinaMochizuki/hangar/internal/auth/pages"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
)

// A PRIVATE endpoint takes a caller who brings one of:
//   - the endpoint session cookie, EndpointCookie, which the edge sets at
//     the end of a handoff from the console (login.PathEndpointLogin);
//   - `Authorization: Bearer` with a CLI access token or a service token.
// Which credential is whose is the apiserver's to say (EndpointAuthService,
// on the edge socket); whether that principal may use the endpoint is the
// edge's, from the authorization data it follows.
//
// Only the __Host- names below are read. A browser takes a __Host- cookie
// only from the host itself, Secure, for the whole host and with no Domain,
// so no other host under the endpoint domain, another tenant's endpoint
// among them, can plant or overwrite one; a lookalike without the prefix is
// ignored. The endpoint itself cannot read them (HttpOnly), and its answers
// cannot set or clear them (confineCookies). Its own scripts can still
// write one under such a name where none is set, on its own host alone. The
// edge takes the value only if it issued it for that host, so at most the
// endpoint signs its visitors in there with a session it already holds.

const (
	// EndpointCookie holds a browser's session on one endpoint's host.
	EndpointCookie = "__Host-hangar-endpoint"
	// StateCookiePrefix starts the cookie of a handoff in progress, which
	// the state's id ends, so a browser may run several at once (tabs
	// restored together).
	StateCookiePrefix = "__Host-hangar-endpoint-state-"
	// handoffTTL is how long a handoff may take, sign-in at the provider
	// included.
	handoffTTL = 10 * time.Minute
	// handoffPurpose binds a sealed handoff to its use.
	handoffPurpose = "hangar endpoint handoff v1"
	// stateIDLen is the length of a state's id, its cookie's name's end.
	stateIDLen = 12
	// maxStateCookies is how many handoffs in progress a browser keeps on
	// one host; starting one more clears them first.
	maxStateCookies = 8
	// checkTimeout bounds one call to the apiserver about a credential.
	checkTimeout = 10 * time.Second
)

// handoff is a handoff in progress, sealed into its state cookie: the
// return path never leaves the edge, so no one can make the callback send a
// browser elsewhere.
type handoff struct {
	State  string `json:"s"`
	Host   string `json:"h"`
	Return string `json:"r"`
	// Expires is in Unix milliseconds.
	Expires int64 `json:"e"`
}

// reservedPath reports whether a request's path is in /.hangar/, the
// edge's own, however it is spelled: in any case, with dot segments, a
// backslash for a slash or a ;parameter, as some servers read them. It
// returns the path cleaned.
func reservedPath(p string) (string, bool) {
	clean := path.Clean("/" + strings.ReplaceAll(p, "\\", "/"))
	seg, _, _ := strings.Cut(strings.TrimPrefix(clean, "/"), "/")
	seg, _, _ = strings.Cut(seg, ";")
	seg = strings.TrimRight(seg, ". ")
	return clean, strings.EqualFold(seg, ".hangar")
}

// credentialOf returns the credential a request brings to a PRIVATE
// endpoint: a Hangar bearer token, else the endpoint cookie, by its exact
// name. bearer says which.
func credentialOf(r *http.Request) (cred token.Secret, bearer bool) {
	for _, v := range r.Header.Values("Authorization") {
		if s, ours := hangarBearer(v); ours {
			return s, true
		}
	}
	if c, err := r.Cookie(EndpointCookie); err == nil && c.Value != "" {
		return token.Secret(c.Value), false
	}
	return "", false
}

// authenticate finds who calls a PRIVATE endpoint, and until when what
// their credential lets in may last. When it returns ok false it has
// answered: a browser without a valid session is handed off to sign in,
// anything else is 401.
func (e *Edge) authenticate(w http.ResponseWriter, r *http.Request, host string, ep endpoint) (auth.Principal, token.Secret, time.Time, bool) {
	cred, bearer := credentialOf(r)
	if cred == "" {
		e.unauthenticated(w, r, host)
		return auth.Principal{}, "", time.Time{}, false
	}
	p, until, err := e.check(r.Context(), cred, host)
	switch {
	case connect.CodeOf(err) == connect.CodeUnauthenticated:
		if bearer {
			e.cfg.Log.Info("a private endpoint refused a token", "endpoint", ep.project+"/"+ep.name)
			w.Header().Set("WWW-Authenticate", `Bearer realm="hangar"`)
			http.Error(w, "the token is not valid for this endpoint", http.StatusUnauthorized)
			return auth.Principal{}, "", time.Time{}, false
		}
		// An ended session is no session: sign in again.
		e.unauthenticated(w, r, host)
		return auth.Principal{}, "", time.Time{}, false
	case err != nil:
		e.cfg.Log.Warn("could not check a private endpoint's caller", "endpoint", ep.project+"/"+ep.name, "error", err)
		http.Error(w, "the sign-in cannot be checked now; try again shortly", http.StatusServiceUnavailable)
		return auth.Principal{}, "", time.Time{}, false
	}
	return p, cred, until, true
}

// check asks the apiserver who holds cred on host.
func (e *Edge) check(ctx context.Context, cred token.Secret, host string) (auth.Principal, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	resp, err := e.cfg.Auth.CheckEndpointCredential(ctx, connect.NewRequest(&networkv1alpha1.CheckEndpointCredentialRequest{
		Credential: cred.Reveal(), Host: host,
	}))
	if err != nil {
		return auth.Principal{}, time.Time{}, err
	}
	p, err := auth.ParsePrincipal(resp.Msg.GetPrincipal())
	if err != nil {
		return auth.Principal{}, time.Time{}, err
	}
	return p, resp.Msg.GetExpireTime().AsTime(), nil
}

// navigation reports whether a request is a browser opening a page, which
// a handoff can bring back to it; anything else would only be redirected
// away from what asked.
func navigation(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	mode := r.Header.Get("Sec-Fetch-Mode")
	return mode == "" || mode == "navigate"
}

// unauthenticated answers a request with no valid credential.
func (e *Edge) unauthenticated(w http.ResponseWriter, r *http.Request, host string) {
	if navigation(r) && e.cfg.ConsoleURL != "" {
		e.startHandoff(w, r, host)
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="hangar"`)
	http.Error(w, "sign in to use this endpoint", http.StatusUnauthorized)
}

// deny answers a caller who may not use the endpoint.
func (e *Edge) deny(w http.ResponseWriter, r *http.Request, host string, ep endpoint, p auth.Principal) {
	e.cfg.Log.Info("a private endpoint refused", "endpoint", ep.project+"/"+ep.name, "principal", p.String())
	if navigation(r) && p.Type == auth.User {
		pages.Render(w, http.StatusForbidden, pages.EndpointDenied, pages.Data{Email: p.ID, Host: host})
		return
	}
	http.Error(w, p.ID+" may not use this endpoint", http.StatusForbidden)
}

// startHandoff sends a browser to the console's endpoint-login, to come
// back signed in to the very page it asked for. What it asked for, and the
// state the callback must bring back, stay in this browser, sealed in a
// cookie of this host.
func (e *Edge) startHandoff(w http.ResponseWriter, r *http.Request, host string) {
	id := token.Random()[:stateIDLen]
	state := id + "." + token.Random()
	sealed, err := e.sealer.Seal(handoff{
		State: state, Host: host, Return: login.SafeReturn(r.URL.RequestURI()),
		Expires: e.cfg.Now().Add(handoffTTL).UnixMilli(),
	})
	if err != nil {
		e.cfg.Log.Error("could not start a handoff", "error", err)
		http.Error(w, "an internal error; try again", http.StatusInternalServerError)
		return
	}
	var held []string
	for _, c := range r.Cookies() {
		if strings.HasPrefix(c.Name, StateCookiePrefix) {
			held = append(held, c.Name)
		}
	}
	if len(held) >= maxStateCookies {
		for _, name := range held {
			http.SetCookie(w, platformCookie(name, "", -1))
		}
	}
	http.SetCookie(w, platformCookie(StateCookiePrefix+id, sealed, int(handoffTTL/time.Second)))
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	to := strings.TrimSuffix(e.cfg.ConsoleURL, "/") + login.PathEndpointLogin + "?" + url.Values{"host": {host}, "state": {state}}.Encode()
	http.Redirect(w, r, to, http.StatusFound)
}

// callback ends a handoff: the browser is back from the console with a
// one-time code. It counts only in the browser that started the handoff,
// for this host, within handoffTTL, and the apiserver takes it only once,
// for this host and state, within its own minute.
func (e *Edge) callback(w http.ResponseWriter, r *http.Request, host string, ep endpoint) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	fail := func(status int, reason string) {
		// The callback's URL holds the code: no referrer to the endpoint,
		// whose "/" the page links to.
		pages.Render(w, status, pages.EndpointFailed, pages.Data{Host: host, Reason: reason, NoReferrer: true})
	}
	q := r.URL.Query()
	code, state := q.Get("code"), q.Get("state")
	id, _, ok := strings.Cut(state, ".")
	if !ok || len(id) != stateIDLen || code == "" {
		fail(http.StatusBadRequest, "the sign-in link is not valid.")
		return
	}
	name := StateCookiePrefix + id
	var hf handoff
	c, err := r.Cookie(name)
	if err == nil {
		http.SetCookie(w, platformCookie(name, "", -1))
		err = e.sealer.Open(c.Value, &hf)
	}
	if err != nil || hf.Host != host || !e.cfg.Now().Before(time.UnixMilli(hf.Expires)) ||
		subtle.ConstantTimeCompare([]byte(hf.State), []byte(state)) != 1 {
		fail(http.StatusBadRequest, "this browser did not start the sign-in, or it took too long.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
	defer cancel()
	resp, err := e.cfg.Auth.ExchangeEndpointCode(ctx, connect.NewRequest(&networkv1alpha1.ExchangeEndpointCodeRequest{
		Code: code, Host: host, State: state,
	}))
	switch {
	case connect.CodeOf(err) == connect.CodeUnauthenticated:
		e.cfg.Log.Info("refused a handoff code", "endpoint", ep.project+"/"+ep.name)
		fail(http.StatusBadRequest, "the sign-in link was used already or has expired.")
		return
	case connect.CodeOf(err) == connect.CodePermissionDenied:
		e.cfg.Log.Info("refused a handoff code: its person may no longer use the endpoint", "endpoint", ep.project+"/"+ep.name)
		fail(http.StatusForbidden, "your account may no longer use this endpoint. Ask an owner of its project for access.")
		return
	case err != nil:
		e.cfg.Log.Warn("could not exchange a handoff code", "endpoint", ep.project+"/"+ep.name, "error", err)
		fail(http.StatusServiceUnavailable, "the sign-in cannot be checked now.")
		return
	}
	exp := resp.Msg.GetExpireTime().AsTime()
	ck := platformCookie(EndpointCookie, resp.Msg.GetSession(), int(time.Until(exp)/time.Second))
	ck.Expires = exp
	http.SetCookie(w, ck)
	e.cfg.Log.Info("signed in on a private endpoint", "endpoint", ep.project+"/"+ep.name, "principal", resp.Msg.GetPrincipal())
	http.Redirect(w, r, login.SafeReturn(hf.Return), http.StatusSeeOther)
}

// platformCookie is one of the edge's cookies: __Host-, so for this host
// alone, Secure, out of scripts' reach, and sent on a top-level navigation
// from another site, as the handoff's return from the console is. maxAge
// below zero deletes it.
func platformCookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	}
}

// recheckCredentials ends, every Config.Recheck, the requests to PRIVATE
// endpoints whose credentials are no longer valid: revoked by a sign-out, a
// revoke of the person's logins, or a service token rotated or deleted. So
// nothing a credential let in outlives its revoke by more than that.
func (e *Edge) recheckCredentials(ctx context.Context) {
	t := time.NewTicker(e.cfg.Recheck)
	defer t.Stop()
	type key struct {
		cred token.Secret
		host string
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		held := map[key][]*request{}
		e.mu.RLock()
		for r := range e.active {
			if r.cred != "" {
				k := key{r.cred, r.host}
				held[k] = append(held[k], r)
			}
		}
		e.mu.RUnlock()
		for k, reqs := range held {
			_, _, err := e.check(ctx, k.cred, k.host)
			if connect.CodeOf(err) != connect.CodeUnauthenticated {
				// Valid, or not known to be revoked: the next round asks again.
				continue
			}
			for _, r := range reqs {
				e.cfg.Log.Info("ended a request whose credential is no longer valid", "endpoint", r.ep.project+"/"+r.ep.name, "principal", r.caller.String())
				r.cancel()
			}
		}
	}
}
