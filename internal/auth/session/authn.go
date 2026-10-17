package session

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
)

// CookieName is the browser session's cookie. The __Host- prefix makes the
// browser refuse it unless it is Secure, for the whole host, and set by the
// host itself, so no sibling domain can plant or read one.
const CookieName = "__Host-hangar_session"

// Bearer authenticates `Authorization: Bearer <token>` with an agent's
// service token or, unless AccessOff, a CLI access token. Any other token,
// and one unknown, revoked or expired, is refused alike.
type Bearer struct {
	M *Manager
	// AccessOff refuses CLI access tokens, for a listener where no one
	// signs in and only service tokens count.
	AccessOff bool
}

func (b Bearer) Authenticate(ctx context.Context, header http.Header) (auth.Principal, bool, error) {
	f, _, _, ok, err := b.authenticate(ctx, header)
	return f.Principal, ok, err
}

// AuthenticateStream rechecks a service token as it is, and an access
// token by the login it was issued in (Manager.LoginStands).
func (b Bearer) AuthenticateStream(ctx context.Context, header http.Header) (auth.Principal, func(context.Context) error, bool, error) {
	f, s, typ, ok, err := b.authenticate(ctx, header)
	if !ok || err != nil {
		return auth.Principal{}, nil, ok, err
	}
	if typ == TypeAccess {
		return f.Principal, func(ctx context.Context) error { return b.M.LoginStands(ctx, f.Family) }, true, nil
	}
	return f.Principal, func(ctx context.Context) error { return b.M.Stands(ctx, s, typ, "") }, true, nil
}

func (b Bearer) authenticate(ctx context.Context, header http.Header) (Found, token.Secret, string, bool, error) {
	v := header.Get("Authorization")
	if v == "" {
		return Found{}, "", "", false, nil
	}
	scheme, raw, _ := strings.Cut(v, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return Found{}, "", "", false, errors.New("the Authorization header takes a Bearer token")
	}
	s := token.Secret(strings.TrimSpace(raw))
	var typ string
	switch {
	case s.Prefix() == token.Access && !b.AccessOff:
		typ = TypeAccess
	case s.Prefix() == token.Service:
		typ = TypeService
	default:
		return Found{}, "", "", false, ErrInvalid
	}
	f, err := b.M.Check(ctx, s, typ, "")
	if err != nil {
		return Found{}, "", "", false, err
	}
	return f, s, typ, true, nil
}

// Cookie authenticates a browser by its session cookie. A cookie goes with
// every request the browser makes to the host, whichever page asked for it,
// so it counts only on a request the console's own origin made: one whose
// Origin is the console's, and, where the browser says, from the same
// origin. Browsers send Origin on every POST and every WebSocket handshake,
// which is all the API takes from a browser, so a cross-site form, fetch or
// WebSocket gets nothing from the cookie (cross-site request forgery and
// WebSocket hijacking).
type Cookie struct {
	M *Manager
	// Origin is the console's origin, such as https://console.example.com.
	Origin string
}

func (c Cookie) Authenticate(ctx context.Context, header http.Header) (auth.Principal, bool, error) {
	s, ok := SessionFromHeader(header)
	if !ok {
		return auth.Principal{}, false, nil
	}
	if !SameOrigin(header, c.Origin) {
		return auth.Principal{}, false, errors.New("a session cookie counts only on a request from the console's own origin")
	}
	p, err := c.M.Lookup(ctx, s, TypeSession)
	if err != nil {
		return auth.Principal{}, false, err
	}
	return p, true, nil
}

// AuthenticateStream rechecks the session without counting that as a use,
// so a tab left open still ends at the session's idle limit.
func (c Cookie) AuthenticateStream(ctx context.Context, header http.Header) (auth.Principal, func(context.Context) error, bool, error) {
	p, ok, err := c.Authenticate(ctx, header)
	if !ok || err != nil {
		return auth.Principal{}, nil, ok, err
	}
	s, _ := SessionFromHeader(header)
	return p, func(ctx context.Context) error { return c.M.Stands(ctx, s, TypeSession, "") }, true, nil
}

// SessionFromHeader returns the session cookie the request carries.
func SessionFromHeader(header http.Header) (token.Secret, bool) {
	r := http.Request{Header: header}
	ck, err := r.Cookie(CookieName)
	if err != nil || ck.Value == "" {
		return "", false
	}
	return token.Secret(ck.Value), true
}

// SameOrigin reports whether a browser request came from the origin: its
// Origin header is exactly the origin, and its Sec-Fetch-Site, when the
// browser sends one, says same-origin.
func SameOrigin(header http.Header, origin string) bool {
	if header.Get("Origin") != origin {
		return false
	}
	site := header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin"
}
