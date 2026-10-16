package login

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/pages"
	"github.com/YukinaMochizuki/hangar/internal/auth/session"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// A browser reaching a PRIVATE public endpoint without a session there is
// sent by hangar-edge to the console's endpoint-login, with the endpoint's
// host and a state the edge keeps in that browser. Endpoint-login signs the
// person in if they are not (the console's own sign-in), checks they may use
// the endpoint, and sends the browser back to the endpoint's callback with a
// one-time code bound to that host, that person and that state. The edge
// trades the code (ExchangeEndpoint) for a session cookie of that host alone.
// See docs/decisions/public-endpoints.md.

const (
	// PathEndpointLogin is where hangar-edge sends a browser to be handed
	// off to a private endpoint.
	PathEndpointLogin = "/auth/endpoint-login"
	// EndpointCallbackPath is where on the endpoint's host the browser comes
	// back with its code; hangar-edge serves it, and never forwards it.
	EndpointCallbackPath = "/.hangar/callback"
	// EndpointCodeTTL is how long the edge has to exchange a code.
	EndpointCodeTTL = time.Minute
	// endpointKey keys the codes not yet exchanged.
	endpointKey = "endpoint:"
	// endpointByKey keys a marker for each code not yet exchanged, under the
	// hash of the person it is for, so a person's waiting codes are counted
	// by prefix; the code's own key holds no person.
	endpointByKey = "endpoint-by:"
	// MaxEndpointCodesPerPerson is how many codes one person may have
	// waiting at once: any signed-in person can make them, and without it
	// one could fill DefaultMaxPending for everyone else.
	MaxEndpointCodesPerPerson = 20
	// maxState bounds the state hangar-edge passes through.
	maxState = 200
	// maxHost is the longest DNS name.
	maxHost = 253
)

// EndpointVerdict is whether a person may use the private endpoint at a host.
type EndpointVerdict int

const (
	// EndpointUnknown: no PRIVATE endpoint has the host.
	EndpointUnknown EndpointVerdict = iota
	// EndpointDenied: there is one, and the person may not use it.
	EndpointDenied
	// EndpointAllowed: there is one, and the person may use it.
	EndpointAllowed
)

// ErrEndpointDenied means the person may no longer use the endpoint the
// code was issued for.
var ErrEndpointDenied = errors.New("the person may no longer use the endpoint")

// endpointGrant is a one-time code hangar-edge exchanges for an endpoint
// session.
type endpointGrant struct {
	Host      string `json:"host"`
	Principal string `json:"principal"`
	// State is the SHA-256 of the state the edge started the handoff with.
	State string `json:"state"`
	// Family and LoginEnd are the console session's the code came from:
	// the endpoint session joins its login and ends no later.
	Family   string `json:"family"`
	LoginEnd int64  `json:"loginEnd"`
}

// endpointLogin hands a signed-in browser off to a private endpoint. It is a
// GET from another site, the endpoint's, by design: the session cookie is
// read without the console's origin check, and what it produces, a code,
// goes only to the endpoint's own callback, bound to the state of the
// browser that started the handoff, so nothing reaches the site that sent
// the browser here.
func (f *Flow) endpointLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	host, state := q.Get("host"), q.Get("state")
	if !validHost(host) || !validState(state) {
		f.fail(w, http.StatusBadRequest, "the link to the endpoint is not valid")
		return
	}
	var found session.Found
	s, ok := session.SessionFromHeader(r.Header)
	var err error
	if ok {
		found, err = f.cfg.Sessions.Check(ctx, s, session.TypeSession, "")
	}
	if !ok || errors.Is(err, session.ErrInvalid) {
		// Signed in first, then back here.
		back := PathEndpointLogin + "?" + url.Values{"host": {host}, "state": {state}}.Encode()
		f.begin(w, r, pending{Return: SafeReturn(back)})
		return
	}
	if err != nil {
		f.internal(w, err)
		return
	}
	p := found.Principal
	if admitted, err := f.cfg.Admit(ctx, p); err != nil {
		f.internal(w, err)
		return
	} else if !admitted {
		pages.Render(w, http.StatusForbidden, pages.NotInvited, pages.Data{Email: p.ID})
		return
	}
	verdict, err := f.cfg.Endpoint(ctx, p, host)
	if err != nil {
		f.internal(w, err)
		return
	}
	switch verdict {
	case EndpointUnknown:
		pages.Render(w, http.StatusNotFound, pages.EndpointUnknown, pages.Data{Host: host})
		return
	case EndpointDenied:
		f.cfg.Log.Info("refused an endpoint handoff: may not use the endpoint", "principal", p.String(), "host", host, "client_ip", clientIP(ctx))
		pages.Render(w, http.StatusForbidden, pages.EndpointDenied, pages.Data{Email: p.ID, Host: host})
		return
	}
	code := token.Random()
	g := endpointGrant{Host: host, Principal: p.String(), State: token.Hash(state), Family: found.Family, LoginEnd: found.LoginEnd.UnixMilli()}
	// The person's marker and the code, kept together only while both the
	// shared cap and the person's have room, in one transaction.
	person := endpointPersonKey(p)
	err = f.putWithin(ctx, []store.AuthLimit{f.shared(endpointKey), {Prefix: person, Max: MaxEndpointCodesPerPerson}},
		row{person + token.Hash(code), struct{}{}, EndpointCodeTTL},
		row{endpointKey + token.Hash(code), g, EndpointCodeTTL})
	var le *store.LimitError
	switch {
	case errors.As(err, &le) && le.Prefix == person:
		f.cfg.Log.Warn("refused an endpoint handoff: too many waiting for one person", "principal", p.String())
		f.fail(w, http.StatusTooManyRequests, "too many sign-ins to endpoints are waiting; try again in a minute")
		return
	case errors.Is(err, store.ErrLimit):
		f.fail(w, http.StatusServiceUnavailable, ErrBusy.Error())
		return
	case err != nil:
		f.internal(w, err)
		return
	}
	f.cfg.Log.Info("handed a browser off to a private endpoint", "principal", p.String(), "host", host, "client_ip", clientIP(ctx))
	// The endpoint learns nothing of the console from the referrer.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "https://"+host+EndpointCallbackPath+"?"+url.Values{"code": {code}, "state": {state}}.Encode(), http.StatusSeeOther)
}

// ExchangeEndpoint trades a code endpoint-login issued for a session on the
// host alone, in the family of the console session it came from. The code is
// spent whatever the outcome, and counts only for the host and the state it
// was issued with, within EndpointCodeTTL.
func (f *Flow) ExchangeEndpoint(ctx context.Context, code, host, state string) (session.Browser, auth.Principal, error) {
	if f.cfg.Endpoint == nil {
		return session.Browser{}, auth.Principal{}, ErrInvalidCode
	}
	var g endpointGrant
	if err := f.take(ctx, endpointKey+token.Hash(code), &g); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return session.Browser{}, auth.Principal{}, ErrInvalidCode
		}
		return session.Browser{}, auth.Principal{}, err
	}
	p, err := auth.ParsePrincipal(g.Principal)
	if err != nil {
		return session.Browser{}, auth.Principal{}, err
	}
	// The code is spent: it no longer counts as waiting for its person.
	if _, err := f.cfg.Store.TakeAuthRequest(ctx, endpointPersonKey(p)+token.Hash(code)); err != nil && !errors.Is(err, store.ErrNotFound) {
		return session.Browser{}, auth.Principal{}, err
	}
	if g.Host != host || subtle.ConstantTimeCompare([]byte(g.State), []byte(token.Hash(state))) != 1 {
		return session.Browser{}, auth.Principal{}, ErrInvalidCode
	}
	// Asked again: an invitation or a grant may have gone since the code.
	if ok, err := f.cfg.Admit(ctx, p); err != nil {
		return session.Browser{}, auth.Principal{}, err
	} else if !ok {
		return session.Browser{}, auth.Principal{}, ErrNotAdmitted
	}
	if v, err := f.cfg.Endpoint(ctx, p, host); err != nil {
		return session.Browser{}, auth.Principal{}, err
	} else if v != EndpointAllowed {
		return session.Browser{}, auth.Principal{}, ErrEndpointDenied
	}
	b, err := f.cfg.Sessions.StartEndpoint(ctx, session.Found{Principal: p, Family: g.Family, LoginEnd: time.UnixMilli(g.LoginEnd)}, host)
	if errors.Is(err, session.ErrInvalid) {
		// The console session's login was revoked since the code.
		return session.Browser{}, auth.Principal{}, ErrInvalidCode
	}
	if err != nil {
		return session.Browser{}, auth.Principal{}, err
	}
	f.cfg.Log.Info("signed in on a private endpoint", "principal", p.String(), "host", host)
	return b, p, nil
}

// endpointPersonKey prefixes the markers of p's waiting codes.
func endpointPersonKey(p auth.Principal) string {
	return endpointByKey + token.Hash(p.String()) + ":"
}

// validHost reports whether s may be a hostname as hangar-edge writes one:
// lower case, no port, no trailing dot. Which hostnames are endpoints is
// Config.Endpoint's to say.
func validHost(s string) bool {
	if s == "" || len(s) > maxHost || s[len(s)-1] == '.' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// validState reports whether s may be a state hangar-edge made: base64url
// and dots, of a bounded length.
func validState(s string) bool {
	if len(s) < 16 || len(s) > maxState {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}
