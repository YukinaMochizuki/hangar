// Package login signs people in through an OpenID Connect provider (Google)
// and hands them Hangar's own credentials (internal/auth/session): a session
// cookie for the console, or access and refresh tokens for the CLI, which
// gets them through a loopback redirect or a device code. The apiserver is
// the provider's relying party; the provider only ever proves an email. See
// docs/decisions/authentication.md.
package login

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/pages"
	"github.com/YukinaMochizuki/hangar/internal/auth/session"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// Paths the flow serves.
const (
	PathLogin    = "/auth/login"
	PathCLI      = "/auth/cli"
	PathCallback = "/auth/callback"
	PathDevice   = "/auth/device"
	PathLogout   = "/auth/logout"
)

// LoopbackPath is the only path a CLI's loopback redirect may have.
const LoopbackPath = "/callback"

// What a sign-in may carry into the login cookie: a browser's return path
// longer than maxReturn becomes "/", and a CLI's state longer than
// maxCLIState is refused.
const (
	maxReturn   = 1024
	maxCLIState = 256
)

// loginCookie holds a sign-in in progress, sealed (see seal), in the
// browser that started it: the callback is accepted only from a browser
// holding the state it was sent with, so no one can finish their own
// sign-in in someone else's browser, and starting a sign-in writes nothing
// on the server for anyone to fill up.
const loginCookie = "__Host-hangar_login"

// Lifetimes of what a sign-in leaves behind while it runs.
const (
	// pendingTTL is how long a person has at the provider.
	pendingTTL = 10 * time.Minute
	// codeTTL is how long the CLI has to exchange its one-time code.
	codeTTL = time.Minute
	// DeviceTTL is how long a device login waits for its approval.
	DeviceTTL = 10 * time.Minute
	// DefaultDeviceInterval is how often a CLI may poll a device login.
	DefaultDeviceInterval = 5 * time.Second
)

// Errors of the CLI's calls. None says more than which step failed.
var (
	ErrInvalidCode = errors.New("the sign-in code is unknown, used or expired, or does not match; run hangar login again")
	ErrPending     = errors.New("the device login has not been approved yet")
	ErrSlowDown    = errors.New("the device login was polled too often; wait longer between polls")
	ErrDenied      = errors.New("the device login was denied")
	ErrExpired     = errors.New("the device login is unknown or has expired; run hangar login --device again")
	// ErrNotAdmitted means the person may no longer sign in: their
	// invitation went between the sign-in and the tokens.
	ErrNotAdmitted = errors.New("the account may no longer sign in; ask an administrator to invite it again")
	// ErrBusy means too many logins of one kind are waiting to start
	// another.
	ErrBusy = errors.New("too many sign-ins are in progress; try again in a few minutes")
	// ErrTooManyFromSource means the address a device login comes from has
	// MaxDeviceLoginsPerSource waiting already.
	ErrTooManyFromSource = errors.New("too many device logins are waiting from this address; finish one, or wait up to 10 minutes for them to expire")
)

// Limits on what the apiserver keeps for logins not yet finished. A
// browser's sign-in keeps nothing (it is in loginCookie); a device login
// started, and a CLI's code not yet exchanged, each keep a row, and each kind
// has its own cap, so a flood of one cannot block the other. Device logins,
// which anyone may start, are also capped per source
// (MaxDeviceLoginsPerSource), so one address cannot fill their shared cap.
// Per-address rate limits are the front door's (hangar-edge).
const (
	// DefaultMaxPending is how many logins of each kind may wait at once,
	// everyone's together.
	DefaultMaxPending = 5000
	// Keys of the rows each kind keeps, by which they are counted.
	deviceKey = "device:"
	codeKey   = "code:"
	// codeAttempts is how many device codes one browser session may look
	// up or decide a minute, so a signed-in person cannot guess others'.
	codeAttempts = 10
)

// Config configures the flow.
type Config struct {
	// Issuer is the provider's issuer URL, such as https://accounts.google.com.
	Issuer       string
	ClientID     string
	ClientSecret token.Secret
	// PublicURL is the origin people reach the console and the API at; the
	// provider redirects back to its /auth/callback.
	PublicURL string
	Sessions  *session.Manager
	Store     store.Credentials
	// Admit says whether a verified person may sign in at all: whether
	// they were invited.
	Admit func(context.Context, auth.Principal) (bool, error)
	// Endpoint says whether a person may use the PRIVATE endpoint at a
	// host, for the handoff to it (PathEndpointLogin); nil serves no
	// handoff.
	Endpoint func(ctx context.Context, p auth.Principal, host string) (EndpointVerdict, error)
	// HTTPClient calls the provider; nil is a client with a timeout.
	HTTPClient *http.Client
	// DeviceInterval is how often a CLI may poll a device login; zero is
	// DefaultDeviceInterval.
	DeviceInterval time.Duration
	// MaxPending caps the logins of each kind waiting at once; zero is
	// DefaultMaxPending.
	MaxPending int
	Log        *slog.Logger
}

// Flow serves sign-in.
type Flow struct {
	cfg    Config
	origin *url.URL
	// attempts limits device code lookups per browser session.
	attempts attempts
	// sealer seals loginCookie.
	sealer *Sealer

	mu       sync.Mutex
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// New checks the config and returns the flow. It reaches the provider only
// on the first sign-in, so the apiserver starts while the provider is away.
func New(cfg Config) (*Flow, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("OIDC sign-in needs an issuer, a client ID and a client secret")
	}
	if cfg.Sessions == nil || cfg.Store == nil || cfg.Admit == nil {
		return nil, errors.New("OIDC sign-in needs sessions, a store and an admission check")
	}
	origin, err := ParseOrigin(cfg.PublicURL)
	if err != nil {
		return nil, err
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.DeviceInterval == 0 {
		cfg.DeviceInterval = DefaultDeviceInterval
	}
	if cfg.MaxPending == 0 {
		cfg.MaxPending = DefaultMaxPending
	}
	sealer, err := NewSealer(signInPurpose)
	if err != nil {
		return nil, err
	}
	return &Flow{cfg: cfg, origin: origin, sealer: sealer}, nil
}

// ParseOrigin reads a public URL: an https origin, or http on a loopback
// host for development, with no path, query or fragment.
func ParseOrigin(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("public URL %q is not a URL such as https://console.example.com", s)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("public URL %q must be an origin only, with no path, query or credentials", s)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return nil, fmt.Errorf("public URL %q must be https: session cookies are Secure", s)
		}
	default:
		return nil, fmt.Errorf("public URL %q must be https", s)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

// Origin is the public origin, such as https://console.example.com.
func (f *Flow) Origin() string { return f.origin.String() }

// Mount adds the flow's pages to mux.
func (f *Flow) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET "+PathLogin, f.onPublicHost(f.startBrowser))
	mux.HandleFunc("GET "+PathCLI, f.onPublicHost(f.startCLI))
	mux.HandleFunc("GET "+PathCallback, f.callback)
	mux.HandleFunc("GET "+PathDevice, f.onPublicHost(f.devicePage))
	mux.HandleFunc("POST "+PathDevice, f.deviceDecision)
	mux.HandleFunc("POST "+PathLogout, f.logout)
	if f.cfg.Endpoint != nil {
		mux.HandleFunc("GET "+PathEndpointLogin, f.onPublicHost(f.endpointLogin))
	}
}

// onPublicHost sends a browser that came in on another host, such as the
// API's, to the same page on the public origin first: the sign-in's cookies
// are the public origin's, and the provider returns there.
func (f *Flow) onPublicHost(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Host != f.origin.Host {
			u := *f.origin
			u.Path, u.RawQuery = r.URL.Path, r.URL.RawQuery
			http.Redirect(w, r, u.String(), http.StatusFound)
			return
		}
		next(w, r)
	}
}

// pending is a sign-in waiting at the provider, under its state.
type pending struct {
	// CLI is set for a CLI's loopback sign-in, unset for a browser's.
	CLI      bool   `json:"cli,omitempty"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	// Return is where a browser goes once signed in.
	Return string `json:"return,omitempty"`
	// The CLI's own redirect, state and PKCE challenge.
	Redirect  string `json:"redirect,omitempty"`
	State     string `json:"state,omitempty"`
	Challenge string `json:"challenge,omitempty"`
}

// grant is a one-time code a CLI exchanges for its tokens.
type grant struct {
	Principal string `json:"principal"`
	Redirect  string `json:"redirect"`
	Challenge string `json:"challenge"`
	// Family is the login reserved for it (session.Manager.Reserve), so
	// revoking the person's logins before the exchange ends it too.
	Family string `json:"family"`
}

func (f *Flow) startBrowser(w http.ResponseWriter, r *http.Request) {
	f.begin(w, r, pending{Return: SafeReturn(r.URL.Query().Get("return"))})
}

func (f *Flow) startCLI(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, state, challenge := q.Get("redirect_uri"), q.Get("state"), q.Get("code_challenge")
	if err := checkLoopback(redirect); err != nil {
		f.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if state == "" || len(state) > maxCLIState || len(challenge) != 43 || q.Get("code_challenge_method") != "S256" {
		f.fail(w, http.StatusBadRequest, "the CLI's request has no state or no S256 code challenge")
		return
	}
	f.begin(w, r, pending{CLI: true, Redirect: redirect, State: state, Challenge: challenge})
}

// begin seals the sign-in, under a fresh state, into the browser's login
// cookie and sends the browser to the provider, with its own nonce and PKCE
// verifier. It writes nothing on the server.
func (f *Flow) begin(w http.ResponseWriter, r *http.Request, p pending) {
	oauth, _, err := f.provider(r.Context())
	if err != nil {
		f.cfg.Log.Error("cannot reach the OIDC provider", "error", err)
		f.fail(w, http.StatusBadGateway, "the identity provider cannot be reached; try again later")
		return
	}
	state := token.Random()
	p.Nonce, p.Verifier = token.Random(), oauth2.GenerateVerifier()
	sealed, err := f.seal(inFlight{Pending: p, State: state, Expires: f.cfg.Sessions.Now().Add(pendingTTL).UnixMilli()})
	if err != nil {
		f.internal(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: loginCookie, Value: sealed, Path: "/", MaxAge: int(pendingTTL / time.Second),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, oauth.AuthCodeURL(state, oidc.Nonce(p.Nonce), oauth2.S256ChallengeOption(p.Verifier)), http.StatusFound)
}

// callback is where the provider sends the browser back.
func (f *Flow) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	state := q.Get("state")
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	ck, err := r.Cookie(loginCookie)
	var in inFlight
	if err == nil {
		in, err = f.open(ck.Value)
	}
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(in.State), []byte(state)) != 1 ||
		!f.cfg.Sessions.Now().Before(time.UnixMilli(in.Expires)) {
		f.fail(w, http.StatusBadRequest, "the sign-in request did not come from this browser, or has expired")
		return
	}
	p := in.Pending
	email, reason := f.verify(ctx, q, p)
	if reason != "" {
		f.refuse(w, r, p, http.StatusBadRequest, reason, "")
		return
	}
	pr := auth.Principal{Type: auth.User, ID: email}
	ok, err := f.cfg.Admit(ctx, pr)
	if err != nil {
		f.internal(w, err)
		return
	}
	if !ok {
		f.cfg.Log.Info("refused a sign-in: not invited", "email", email, "client_ip", clientIP(ctx))
		f.refuse(w, r, p, http.StatusForbidden, email+" has not been invited to Hangar; ask an administrator to invite this address", email)
		return
	}
	if !p.CLI {
		s, err := f.cfg.Sessions.StartBrowser(ctx, pr)
		if err != nil {
			f.internal(w, err)
			return
		}
		f.cfg.Log.Info("signed in on a browser", "principal", pr.String(), "client_ip", clientIP(ctx))
		http.SetCookie(w, &http.Cookie{
			Name: session.CookieName, Value: s.Secret.Reveal(), Path: "/", Expires: s.ExpiresAt,
			Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, p.Return, http.StatusSeeOther)
		return
	}
	// The family is reserved before the code is kept, so that a revoke of
	// the person's logins in between is never missed. So the cap is looked
	// at first, and a code that still finds it full, having lost a race to
	// another, releases its reservation: a refused sign-in keeps nothing.
	if n, err := f.cfg.Store.CountAuthRequests(ctx, codeKey, f.cfg.Sessions.Now()); err != nil {
		f.internal(w, err)
		return
	} else if n >= f.cfg.MaxPending {
		f.cfg.Log.Warn("refused a login: too many waiting", "kind", strings.TrimSuffix(codeKey, ":"))
		f.refuse(w, r, p, http.StatusServiceUnavailable, ErrBusy.Error(), "")
		return
	}
	family, err := f.cfg.Sessions.Reserve(ctx, pr, codeTTL+time.Minute)
	if err != nil {
		f.internal(w, err)
		return
	}
	code := token.Random()
	if err := f.putWithin(ctx, []store.AuthLimit{f.shared(codeKey)},
		row{codeKey + token.Hash(code), grant{Principal: pr.String(), Redirect: p.Redirect, Challenge: p.Challenge, Family: family}, codeTTL}); err != nil {
		if rerr := f.cfg.Sessions.Release(ctx, family); rerr != nil {
			f.cfg.Log.Warn("could not release a reserved login", "error", rerr)
		}
		if errors.Is(err, store.ErrLimit) {
			f.refuse(w, r, p, http.StatusServiceUnavailable, ErrBusy.Error(), "")
			return
		}
		f.internal(w, err)
		return
	}
	f.cfg.Log.Info("signed in for a CLI", "principal", pr.String(), "client_ip", clientIP(ctx))
	http.Redirect(w, r, withQuery(p.Redirect, url.Values{"code": {code}, "state": {p.State}}), http.StatusSeeOther)
}

// verify exchanges the provider's code and checks its ID token: signed by
// the provider for this client, with this sign-in's nonce, and a verified
// email. It returns the email in lower case, or why it is refused.
func (f *Flow) verify(ctx context.Context, q url.Values, p pending) (email, reason string) {
	if e := q.Get("error"); e != "" {
		return "", "the identity provider returned an error (" + e + ")"
	}
	oauth, verifier, err := f.provider(ctx)
	if err != nil {
		f.cfg.Log.Error("cannot reach the OIDC provider", "error", err)
		return "", "the identity provider cannot be reached; try again later"
	}
	tok, err := oauth.Exchange(context.WithValue(ctx, oauth2.HTTPClient, f.cfg.HTTPClient), q.Get("code"), oauth2.VerifierOption(p.Verifier))
	if err != nil {
		f.cfg.Log.Warn("the OIDC code exchange failed", "error", err)
		return "", "the identity provider did not accept the sign-in"
	}
	raw, _ := tok.Extra("id_token").(string)
	id, err := verifier.Verify(oidc.ClientContext(ctx, f.cfg.HTTPClient), raw)
	if err != nil {
		f.cfg.Log.Warn("the ID token did not verify", "error", err)
		return "", "the identity provider's answer did not verify"
	}
	if subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(p.Nonce)) != 1 {
		return "", "the identity provider's answer was for another sign-in"
	}
	var claims struct {
		Email    string       `json:"email"`
		Verified flexibleBool `json:"email_verified"`
	}
	if err := id.Claims(&claims); err != nil || claims.Email == "" {
		return "", "the identity provider gave no email address"
	}
	if !claims.Verified {
		return "", "the identity provider has not verified the email address " + claims.Email
	}
	return strings.ToLower(claims.Email), ""
}

// refuse ends a sign-in that reached the callback: a browser sees why, and
// a CLI's loopback is told why, so the terminal does not wait for nothing.
func (f *Flow) refuse(w http.ResponseWriter, r *http.Request, p pending, status int, reason, notInvited string) {
	switch {
	case p.CLI:
		http.Redirect(w, r, withQuery(p.Redirect, url.Values{
			"error": {"access_denied"}, "error_description": {reason}, "state": {p.State},
		}), http.StatusSeeOther)
	case notInvited != "":
		pages.Render(w, status, pages.NotInvited, pages.Data{Email: notInvited})
	default:
		f.fail(w, status, reason)
	}
}

// ExchangeCode trades the one-time code the CLI's loopback got for the CLI's
// tokens. The code is spent whatever the outcome, and counts only with the
// redirect it was sent to and the PKCE verifier of the challenge the CLI
// started with.
func (f *Flow) ExchangeCode(ctx context.Context, code, verifier, redirect string) (session.Tokens, error) {
	var g grant
	if err := f.take(ctx, codeKey+token.Hash(code), &g); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return session.Tokens{}, ErrInvalidCode
		}
		return session.Tokens{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if g.Redirect != redirect || subtle.ConstantTimeCompare([]byte(challenge), []byte(g.Challenge)) != 1 {
		return session.Tokens{}, ErrInvalidCode
	}
	p, err := auth.ParsePrincipal(g.Principal)
	if err != nil {
		return session.Tokens{}, err
	}
	return f.startCLILogin(ctx, p, g.Family)
}

// startCLILogin admits the person again, as the invitation may have gone since
// they signed in, then starts their CLI login in the family reserved for it
// when the sign-in was approved: if their logins were revoked since, it
// issues nothing.
func (f *Flow) startCLILogin(ctx context.Context, p auth.Principal, family string) (session.Tokens, error) {
	ok, err := f.cfg.Admit(ctx, p)
	if err != nil {
		return session.Tokens{}, err
	}
	if !ok {
		return session.Tokens{}, ErrNotAdmitted
	}
	if family == "" {
		return session.Tokens{}, ErrInvalidCode
	}
	return f.cfg.Sessions.StartCLIIn(ctx, p, family)
}

// shared is the cap on the logins of the kind keyed by prefix waiting at
// once, everyone's together.
func (f *Flow) shared(prefix string) store.AuthLimit {
	return store.AuthLimit{Prefix: prefix, Max: f.cfg.MaxPending}
}

// row is a record to keep under a key for a time.
type row struct {
	key string
	v   any
	ttl time.Duration
}

// putWithin stores the records, all or none, if every limit has room, in
// one store transaction, so that of logins started at once no more are
// kept than the limits allow. A limit at its cap is store.ErrLimit, as a
// *store.LimitError naming it.
func (f *Flow) putWithin(ctx context.Context, limits []store.AuthLimit, rows ...row) error {
	now := f.cfg.Sessions.Now()
	rs := make([]store.AuthRequest, 0, len(rows))
	for _, r := range rows {
		b, err := json.Marshal(r.v)
		if err != nil {
			return err
		}
		rs = append(rs, store.AuthRequest{Key: r.key, Data: b, ExpiresAt: now.Add(r.ttl)})
	}
	err := f.cfg.Store.CreateAuthRequestsWithin(ctx, now, limits, rs...)
	// A shared cap reached is worth an operator's look; a person's own, its
	// caller says.
	if le := (*store.LimitError)(nil); errors.As(err, &le) && slices.Contains([]string{deviceKey, codeKey, endpointKey}, le.Prefix) {
		f.cfg.Log.Warn("refused a login: too many waiting", "kind", strings.TrimSuffix(le.Prefix, ":"))
	}
	return err
}

// attempts counts recent device code attempts by key, in this process.
type attempts struct {
	mu     sync.Mutex
	recent map[string][]time.Time
}

// allow records an attempt by key at now, and reports whether it is within
// limit for the minute before.
func (a *attempts) allow(key string, now time.Time, limit int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.recent == nil {
		a.recent = map[string][]time.Time{}
	}
	cutoff := now.Add(-time.Minute)
	if len(a.recent) > 10000 {
		for k, ts := range a.recent {
			if ts[len(ts)-1].Before(cutoff) {
				delete(a.recent, k)
			}
		}
	}
	var kept []time.Time
	for _, t := range a.recent[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= limit {
		a.recent[key] = kept
		return false
	}
	a.recent[key] = append(kept, now)
	return true
}

// logout ends the browser's session, from the console's own origin only.
func (f *Flow) logout(w http.ResponseWriter, r *http.Request) {
	if !session.SameOrigin(r.Header, f.Origin()) {
		http.Error(w, "sign out from the console", http.StatusForbidden)
		return
	}
	if s, ok := session.SessionFromHeader(r.Header); ok {
		if err := f.cfg.Sessions.Revoke(r.Context(), s); err != nil {
			f.internal(w, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: session.CookieName, Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}

// provider returns the provider's OAuth config and ID token verifier,
// discovering them on first use.
func (f *Flow) provider(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.oauth != nil {
		return f.oauth, f.verifier, nil
	}
	// The provider keeps this context to fetch its keys later, so it must
	// outlive the request.
	pctx := oidc.ClientContext(context.WithoutCancel(ctx), f.cfg.HTTPClient)
	p, err := oidc.NewProvider(pctx, f.cfg.Issuer)
	if err != nil {
		return nil, nil, err
	}
	f.oauth = &oauth2.Config{
		ClientID: f.cfg.ClientID, ClientSecret: f.cfg.ClientSecret.Reveal(), Endpoint: p.Endpoint(),
		RedirectURL: f.Origin() + PathCallback, Scopes: []string{oidc.ScopeOpenID, "email"},
	}
	f.verifier = p.Verifier(&oidc.Config{ClientID: f.cfg.ClientID})
	return f.oauth, f.verifier, nil
}

// put stores a record under a key for ttl.
func (f *Flow) put(ctx context.Context, key string, v any, ttl time.Duration) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return f.cfg.Store.CreateAuthRequest(ctx, store.AuthRequest{Key: key, Data: b, ExpiresAt: f.cfg.Sessions.Now().Add(ttl)})
}

// take removes and returns a record, ErrNotFound if it is gone or expired.
func (f *Flow) take(ctx context.Context, key string, v any) error {
	r, err := f.cfg.Store.TakeAuthRequest(ctx, key)
	if err != nil {
		return err
	}
	if !f.cfg.Sessions.Now().Before(r.ExpiresAt) {
		return store.ErrNotFound
	}
	return json.Unmarshal(r.Data, v)
}

// get returns a record, ErrNotFound if it is gone or expired.
func (f *Flow) get(ctx context.Context, key string, v any) (store.AuthRequest, error) {
	r, err := f.cfg.Store.GetAuthRequest(ctx, key)
	if err != nil {
		return r, err
	}
	if !f.cfg.Sessions.Now().Before(r.ExpiresAt) {
		return r, store.ErrNotFound
	}
	return r, json.Unmarshal(r.Data, v)
}

func (f *Flow) fail(w http.ResponseWriter, status int, reason string) {
	pages.Render(w, status, pages.SignInFailed, pages.Data{Reason: reason})
}

func (f *Flow) internal(w http.ResponseWriter, err error) {
	f.cfg.Log.Error("sign-in failed", "error", err)
	f.fail(w, http.StatusInternalServerError, "an internal error; try again")
}

// SafeReturn keeps a return path on the origin it is used on, or makes it
// "/", for the sign-in and for hangar-edge's endpoint handoff. Browsers
// read //host and /\host as another host, and drop tabs and newlines
// before they look, so /\t/host is one too: only a clean absolute path
// passes, with no backslash and no control byte, raw or escaped, and no
// scheme or host however url.Parse reads it.
func SafeReturn(s string) string {
	if !cleanPath(s) {
		return "/"
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || !cleanPath(u.Path) {
		return "/"
	}
	return s
}

// cleanPath reports whether s starts with exactly one slash, holds no
// backslash and no control byte, and fits in the login cookie.
func cleanPath(s string) bool {
	if !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") || len(s) > maxReturn {
		return false
	}
	for i := 0; i < len(s); i++ {
		if b := s[i]; b < 0x20 || b == 0x7f || b == '\\' {
			return false
		}
	}
	return true
}

// checkLoopback accepts only a CLI's loopback redirect: http on 127.0.0.1
// or [::1], any port, at LoopbackPath (RFC 8252, section 7.3). Not
// localhost, which a hosts file can point anywhere.
func checkLoopback(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "http" || u.Port() == "" || u.Path != LoopbackPath || u.RawQuery != "" || u.User != nil {
		return fmt.Errorf("the CLI's redirect %q is not http://127.0.0.1:PORT%s", s, LoopbackPath)
	}
	if h := u.Hostname(); h != "127.0.0.1" && h != "::1" {
		return fmt.Errorf("the CLI's redirect %q is not on 127.0.0.1 or [::1]", s)
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func withQuery(base string, q url.Values) string {
	return base + "?" + q.Encode()
}

// flexibleBool reads a JSON bool, or the string "true" some providers send.
type flexibleBool bool

func (b *flexibleBool) UnmarshalJSON(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	switch v := v.(type) {
	case bool:
		*b = flexibleBool(v)
	case string:
		*b = flexibleBool(v == "true")
	}
	return nil
}

// clientIP is the address the request came from, as its listener
// established it (auth.ClientIP), for the audit lines; empty when the
// listener has none.
func clientIP(ctx context.Context) string {
	if ip, ok := auth.ClientIP(ctx); ok {
		return ip.String()
	}
	return ""
}
