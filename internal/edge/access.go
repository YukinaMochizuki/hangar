package edge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/YukinaMochizuki/hangar/internal/auth"
)

// AccessJWTHeader carries the token Cloudflare Access signs for every
// request it lets through.
const AccessJWTHeader = "Cf-Access-Jwt-Assertion"

// AccessConfig identifies the Access application whose tokens are accepted.
type AccessConfig struct {
	// TeamDomain is the token issuer, such as https://team.cloudflareaccess.com.
	TeamDomain string
	// Audience is the Access application's AUD tag.
	Audience string
	// CertsURL serves the signing keys; empty means the team's standard
	// endpoint.
	CertsURL string
}

// AccessVerifier verifies the Access tokens of the endpoint domain's
// application, which every PRIVATE endpoint is behind. It never trusts the
// plain identity headers Access also sets, because anything that can reach
// the listener could forge them; only the signature counts. It lives in
// internal/edge because nothing else trusts Access.
type AccessVerifier struct {
	verifier *oidc.IDTokenVerifier
}

// NewAccessVerifier returns a verifier for one Access application. Keys are
// fetched lazily and refreshed when an unknown key ID appears.
func NewAccessVerifier(ctx context.Context, cfg AccessConfig) (*AccessVerifier, error) {
	if cfg.TeamDomain == "" || cfg.Audience == "" {
		return nil, errors.New("cloudflare auth needs a team domain and an audience")
	}
	issuer := strings.TrimSuffix(cfg.TeamDomain, "/")
	certs := cfg.CertsURL
	if certs == "" {
		certs = issuer + "/cdn-cgi/access/certs"
	}
	keys := oidc.NewRemoteKeySet(ctx, certs)
	return &AccessVerifier{verifier: oidc.NewVerifier(issuer, keys, &oidc.Config{ClientID: cfg.Audience})}, nil
}

// accessClaims are the identity claims in an Access token: users have an
// email, service tokens a common name (their client ID).
type accessClaims struct {
	Email      string `json:"email"`
	CommonName string `json:"common_name"`
}

func (c *AccessVerifier) Authenticate(ctx context.Context, header http.Header) (auth.Principal, bool, error) {
	p, _, ok, err := c.AuthenticateUntil(ctx, header)
	return p, ok, err
}

// AuthenticateUntil is Authenticate, and says until when the token holds:
// what it let in must end then.
func (c *AccessVerifier) AuthenticateUntil(ctx context.Context, header http.Header) (auth.Principal, time.Time, bool, error) {
	raw := header.Get(AccessJWTHeader)
	if raw == "" {
		return auth.Principal{}, time.Time{}, false, nil
	}
	tok, err := c.verifier.Verify(ctx, raw)
	if err != nil {
		return auth.Principal{}, time.Time{}, false, fmt.Errorf("access token: %w", err)
	}
	var claims accessClaims
	if err := tok.Claims(&claims); err != nil {
		return auth.Principal{}, time.Time{}, false, fmt.Errorf("access token claims: %w", err)
	}
	switch {
	case claims.Email != "":
		return auth.Principal{Type: auth.User, ID: strings.ToLower(claims.Email)}, tok.Expiry, true, nil
	case claims.CommonName != "":
		return auth.Principal{Type: auth.ServiceToken, ID: claims.CommonName}, tok.Expiry, true, nil
	}
	return auth.Principal{}, time.Time{}, false, errors.New("access token has neither an email nor a service token")
}
